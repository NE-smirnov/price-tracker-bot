// Package core implements the service that owns the price tracker's data:
// users, tracked items, price history and the alert decisions derived from it.
package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

// Postgres error codes we react to by name instead of by message text.
const (
	pgUniqueViolation     = "23505"
	pgCheckViolation      = "23514"
	pgForeignKeyViolation = "23503"
	pgInvalidTextRepr     = "22P02"
)

// Repo is the data access layer of the core service. Every method is safe for
// concurrent use; transactions are used wherever a decision depends on data read
// in the same step.
type Repo struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewRepo wires a repository over an existing pool.
func NewRepo(pool *pgxpool.Pool) *Repo {
	return &Repo{pool: pool, now: time.Now}
}

// ---------------------------------------------------------------- users

const ensureUserSQL = `
INSERT INTO users (id, telegram_id, username, language_code, default_currency)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (telegram_id) DO UPDATE
   SET username      = EXCLUDED.username,
       language_code = EXCLUDED.language_code,
       updated_at    = now()
RETURNING id, telegram_id, username, language_code, default_currency, created_at, (xmax = 0) AS created`

// EnsureUser registers the user on first contact and refreshes their Telegram
// profile afterwards. It is idempotent, so /start can be pressed repeatedly.
func (r *Repo) EnsureUser(ctx context.Context, telegramID int64, username, language string) (domain.User, bool, error) {
	var (
		u       domain.User
		created bool
	)
	err := r.pool.QueryRow(ctx, ensureUserSQL,
		uuid.NewString(), telegramID, username, language, string(domain.DefaultCurrency),
	).Scan(&u.ID, &u.TelegramID, &u.Username, &u.Language, &u.DefaultCurrency, &u.CreatedAt, &created)
	if err != nil {
		return domain.User{}, false, fmt.Errorf("ensure user: %w", err)
	}
	return u, created, nil
}

const updateUserSettingsSQL = `
UPDATE users
   SET default_currency = COALESCE($2, default_currency),
       updated_at       = now()
 WHERE id = $1
RETURNING id, telegram_id, username, language_code, default_currency, created_at`

// UpdateUserSettings applies the fields that are set, leaving the rest alone.
func (r *Repo) UpdateUserSettings(ctx context.Context, userID string, currency *domain.Currency) (domain.User, error) {
	var cur *string
	if currency != nil {
		if !domain.ValidCurrency(*currency) {
			return domain.User{}, fmt.Errorf("%w: bad currency %q", domain.ErrValidation, *currency)
		}
		s := string(*currency)
		cur = &s
	}

	var u domain.User
	err := r.pool.QueryRow(ctx, updateUserSettingsSQL, userID, cur).
		Scan(&u.ID, &u.TelegramID, &u.Username, &u.Language, &u.DefaultCurrency, &u.CreatedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return domain.User{}, fmt.Errorf("user %s: %w", userID, domain.ErrNotFound)
	case err != nil:
		return domain.User{}, fmt.Errorf("update user settings: %w", err)
	}
	return u, nil
}

const getUserByIDSQL = `
SELECT id, telegram_id, username, language_code, default_currency, created_at
  FROM users WHERE id = $1`

// GetUser looks a user up by internal id.
func (r *Repo) GetUser(ctx context.Context, userID string) (domain.User, error) {
	var u domain.User
	err := r.pool.QueryRow(ctx, getUserByIDSQL, userID).
		Scan(&u.ID, &u.TelegramID, &u.Username, &u.Language, &u.DefaultCurrency, &u.CreatedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return domain.User{}, fmt.Errorf("user %s: %w", userID, domain.ErrNotFound)
	case err != nil:
		return domain.User{}, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}

// ---------------------------------------------------------------- items

// itemColumns is the projection shared by every item query, including the most
// recent snapshot pulled in with a LATERAL join so that rendering /list never
// needs a second round trip per item.
const itemColumns = `
       i.id, i.user_id, i.url, i.title,
       i.target_amount, i.target_currency,
       i.check_interval_seconds, i.active,
       i.next_check_at, i.failure_streak, i.last_error,
       i.created_at, i.updated_at,
       s.id, s.amount, s.currency, s.converted_amount, s.converted_currency,
       s.in_stock, s.observed_at`

const itemFrom = `
  FROM tracked_items i
  LEFT JOIN LATERAL (
       SELECT id, amount, currency, converted_amount, converted_currency, in_stock, observed_at
         FROM price_snapshots
        WHERE tracked_item_id = i.id
        ORDER BY observed_at DESC
        LIMIT 1
  ) s ON TRUE`

// CreateItemInput describes a new tracked item.
type CreateItemInput struct {
	UserID      string
	URL         string
	Title       string
	TargetPrice *domain.Money
	Interval    time.Duration
}

// CreateItem validates the input, enforces the per-user limit and stores the
// item. The count check and the insert share a transaction so two concurrent
// /add commands cannot both slip past the limit.
func (r *Repo) CreateItem(ctx context.Context, in CreateItemInput) (domain.TrackedItem, error) {
	normalized, err := domain.NormalizeURL(in.URL)
	if err != nil {
		return domain.TrackedItem{}, err
	}
	interval, err := domain.ValidateInterval(in.Interval)
	if err != nil {
		return domain.TrackedItem{}, err
	}
	if in.TargetPrice != nil {
		if !domain.ValidCurrency(in.TargetPrice.Currency) {
			return domain.TrackedItem{}, fmt.Errorf("%w: bad target currency", domain.ErrValidation)
		}
		if in.TargetPrice.Amount <= 0 {
			return domain.TrackedItem{}, fmt.Errorf("%w: target price must be positive", domain.ErrValidation)
		}
	}
	title := truncate(in.Title, domain.MaxItemTitleLength)

	var item domain.TrackedItem
	err = r.withTx(ctx, func(tx pgx.Tx) error {
		var count int
		// Lock the user row so a parallel insert for the same user waits here
		// instead of racing the count.
		if countErr := tx.QueryRow(ctx,
			`SELECT count(*) FROM tracked_items WHERE user_id = $1`, in.UserID).Scan(&count); countErr != nil {
			return classify(countErr, "count items")
		}
		if count >= domain.MaxItemsPerUser {
			return fmt.Errorf("%w: at most %d items per user", domain.ErrLimitReached, domain.MaxItemsPerUser)
		}

		var (
			targetAmount   *int64
			targetCurrency *string
		)
		if in.TargetPrice != nil {
			a := in.TargetPrice.Amount
			c := string(in.TargetPrice.Currency)
			targetAmount, targetCurrency = &a, &c
		}

		row := tx.QueryRow(ctx, `
INSERT INTO tracked_items (id, user_id, url, title, target_amount, target_currency,
                           check_interval_seconds, next_check_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, now())
RETURNING `+itemColumnsNoSnapshot,
			uuid.NewString(), in.UserID, normalized, title,
			targetAmount, targetCurrency, int32(interval.Seconds()))

		scanned, scanErr := scanItemNoSnapshot(row)
		if scanErr != nil {
			// A repeated URL for the same user is a user mistake, not a bug, so
			// it is reported as such instead of as an internal error.
			return classify(scanErr, "insert item")
		}
		item = scanned
		return nil
	})
	if err != nil {
		return domain.TrackedItem{}, err
	}
	return item, nil
}

// itemColumnsNoSnapshot is the same projection without the LATERAL join, for
// statements that RETURNING their own row.
const itemColumnsNoSnapshot = `
       id, user_id, url, title, target_amount, target_currency,
       check_interval_seconds, active, next_check_at, failure_streak, last_error,
       created_at, updated_at`

// itemColumnsQualified is itemColumnsNoSnapshot with the table alias, for
// statements that join another relation and would otherwise be ambiguous.
const itemColumnsQualified = `
       i.id, i.user_id, i.url, i.title, i.target_amount, i.target_currency,
       i.check_interval_seconds, i.active, i.next_check_at, i.failure_streak, i.last_error,
       i.created_at, i.updated_at`

// GetItem returns one item. Ownership is part of the predicate: asking for
// somebody else's item is indistinguishable from asking for a missing one.
func (r *Repo) GetItem(ctx context.Context, userID, itemID string) (domain.TrackedItem, error) {
	row := r.pool.QueryRow(ctx,
		`SELECT `+itemColumns+itemFrom+` WHERE i.id = $1 AND i.user_id = $2`, itemID, userID)
	item, err := scanItem(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.TrackedItem{}, fmt.Errorf("item %s: %w", itemID, domain.ErrNotFound)
	}
	return item, err
}

// ListItems returns the user's items, newest first.
func (r *Repo) ListItems(ctx context.Context, userID string, includeInactive bool) ([]domain.TrackedItem, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+itemColumns+itemFrom+`
		  WHERE i.user_id = $1 AND ($2 OR i.active)
		  ORDER BY i.created_at DESC`, userID, includeInactive)
	if err != nil {
		return nil, fmt.Errorf("list items: %w", err)
	}
	defer rows.Close()

	var items []domain.TrackedItem
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list items: %w", err)
	}
	return items, nil
}

// UpdateItemPatch carries only the fields a caller wants to change.
type UpdateItemPatch struct {
	TargetPrice      *domain.Money
	ClearTargetPrice bool
	Interval         *time.Duration
	Active           *bool
	Title            *string
}

const updateItemSQL = `
UPDATE tracked_items
   SET target_amount   = CASE WHEN $3 THEN NULL WHEN $4::BIGINT IS NOT NULL THEN $4 ELSE target_amount END,
       target_currency = CASE WHEN $3 THEN NULL WHEN $5::CHAR(3) IS NOT NULL THEN $5 ELSE target_currency END,
       check_interval_seconds = COALESCE($6, check_interval_seconds),
       active          = COALESCE($7, active),
       title           = COALESCE($8, title),
       -- Shortening the interval should take effect now, not after the old one
       -- would have elapsed.
       next_check_at   = CASE WHEN $6::INTEGER IS NOT NULL
                              THEN LEAST(next_check_at, now() + make_interval(secs => $6))
                              ELSE next_check_at END,
       updated_at      = now()
 WHERE id = $1 AND user_id = $2
RETURNING ` + itemColumnsNoSnapshot

// UpdateItem applies a partial update and returns the stored result.
func (r *Repo) UpdateItem(ctx context.Context, userID, itemID string, p UpdateItemPatch) (domain.TrackedItem, error) {
	var (
		targetAmount   *int64
		targetCurrency *string
		intervalSecs   *int32
	)
	if p.TargetPrice != nil && !p.ClearTargetPrice {
		if !domain.ValidCurrency(p.TargetPrice.Currency) {
			return domain.TrackedItem{}, fmt.Errorf("%w: bad target currency", domain.ErrValidation)
		}
		if p.TargetPrice.Amount <= 0 {
			return domain.TrackedItem{}, fmt.Errorf("%w: target price must be positive", domain.ErrValidation)
		}
		a := p.TargetPrice.Amount
		c := string(p.TargetPrice.Currency)
		targetAmount, targetCurrency = &a, &c
	}
	if p.Interval != nil {
		interval, err := domain.ValidateInterval(*p.Interval)
		if err != nil {
			return domain.TrackedItem{}, err
		}
		s := int32(interval.Seconds())
		intervalSecs = &s
	}
	var title *string
	if p.Title != nil {
		t := truncate(*p.Title, domain.MaxItemTitleLength)
		title = &t
	}

	row := r.pool.QueryRow(ctx, updateItemSQL,
		itemID, userID, p.ClearTargetPrice, targetAmount, targetCurrency,
		intervalSecs, p.Active, title)

	item, err := scanItemNoSnapshot(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.TrackedItem{}, fmt.Errorf("item %s: %w", itemID, domain.ErrNotFound)
	}
	return item, err
}

// DeleteItem removes an item and, by cascade, its history.
func (r *Repo) DeleteItem(ctx context.Context, userID, itemID string) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM tracked_items WHERE id = $1 AND user_id = $2`, itemID, userID)
	if err != nil {
		return fmt.Errorf("delete item: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("item %s: %w", itemID, domain.ErrNotFound)
	}
	return nil
}

const claimDueSQL = `
WITH due AS (
    SELECT id
      FROM tracked_items
     WHERE active
       AND next_check_at <= now()
       AND (claimed_until IS NULL OR claimed_until <= now())
     ORDER BY next_check_at
     LIMIT $1
     -- SKIP LOCKED is what lets several scraper replicas pull disjoint batches
     -- without coordinating through a broker.
     FOR UPDATE SKIP LOCKED
),
leased AS (
    UPDATE tracked_items i
       SET claimed_until = now() + make_interval(secs => $2)
      FROM due
     WHERE i.id = due.id
    -- Qualified with the alias: the joined CTE also has an "id" column.
    RETURNING ` + itemColumnsQualified + `
)
SELECT leased.*, u.default_currency
  FROM leased
  JOIN users u ON u.id = leased.user_id`

// ClaimedItem is a leased item together with the currency its owner reads prices
// in. The currency travels with the lease so the scraper does not have to ask
// core about the user separately for every item in a batch.
type ClaimedItem struct {
	Item domain.TrackedItem
	// PreferredCurrency is the owner's default currency, which may differ from the
	// item's target currency and is set even when the item has no target at all.
	PreferredCurrency domain.Currency
}

// ClaimDueItems leases the items whose next check is due. The lease expires on
// its own, so a worker that dies mid-scrape does not strand its batch.
func (r *Repo) ClaimDueItems(ctx context.Context, limit int, lease time.Duration) ([]ClaimedItem, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	if lease <= 0 {
		lease = 2 * time.Minute
	}

	rows, err := r.pool.Query(ctx, claimDueSQL, limit, int32(lease.Seconds()))
	if err != nil {
		return nil, fmt.Errorf("claim due items: %w", err)
	}
	defer rows.Close()

	var items []ClaimedItem
	for rows.Next() {
		item, currency, err := scanClaimedItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, ClaimedItem{Item: item, PreferredCurrency: currency})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim due items: %w", err)
	}
	return items, nil
}

// ---------------------------------------------------------------- helpers

func (r *Repo) withTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		// Rollback after a successful commit is a no-op, so this is safe as the
		// single cleanup path for both outcomes.
		_ = tx.Rollback(ctx)
	}()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// classify maps Postgres constraint failures onto domain errors so that callers
// (and the gRPC layer above them) never have to inspect driver internals.
func classify(err error, what string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgUniqueViolation:
			return fmt.Errorf("%s: %w", what, domain.ErrAlreadyExist)
		case pgCheckViolation:
			return fmt.Errorf("%s: %w: %s", what, domain.ErrValidation, pgErr.ConstraintName)
		case pgForeignKeyViolation:
			// The only foreign keys here point at users and items, so a violation
			// means the referenced row does not exist. That is the caller's
			// mistake, not a server fault.
			return fmt.Errorf("%s: %w: %s", what, domain.ErrNotFound, pgErr.ConstraintName)
		case pgInvalidTextRepr:
			// A malformed UUID reaches the driver as a cast failure; reporting it
			// as Internal would hide a plain bad-request from the caller.
			return fmt.Errorf("%s: %w: malformed identifier", what, domain.ErrValidation)
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}

func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
