package core

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

// FailureStreakThreshold is the number of consecutive failed scrapes after which
// the user is told the item can no longer be read.
const FailureStreakThreshold = 5

// maxFailureBackoff caps the exponential backoff so a permanently dead page is
// still retried a few times a day instead of drifting into never.
const maxFailureBackoff = 6 * time.Hour

// RecordSnapshotInput is one observation submitted by a scraper.
type RecordSnapshotInput struct {
	TrackedItemID string
	Price         domain.Money
	Converted     *domain.Money
	InStock       bool
	ObservedAt    time.Time
	// ObservedTitle fills in the item title when it was added without one.
	ObservedTitle string
}

// SnapshotResult is everything a scraper needs back after reporting a price: the
// stored observation plus fully populated alerts, so the notifier downstream
// never has to query anything.
type SnapshotResult struct {
	Snapshot   domain.PriceSnapshot
	Item       domain.TrackedItem
	TelegramID int64
	Alerts     []domain.Alert
}

// FailureResult mirrors SnapshotResult for a failed scrape attempt.
type FailureResult struct {
	Streak     int
	Item       domain.TrackedItem
	TelegramID int64
	Alerts     []domain.Alert
}

// RecordSnapshot stores an observation and returns the alerts it triggered.
//
// Reading the previous snapshot, the all-time minimum, inserting the new row and
// persisting alerts all happen in one transaction with the item row locked.
// That is what makes "is this an all-time low?" and "did we already send this?"
// answerable without races between concurrent scraper workers.
func (r *Repo) RecordSnapshot(ctx context.Context, in RecordSnapshotInput) (SnapshotResult, error) {
	if !domain.ValidCurrency(in.Price.Currency) {
		return SnapshotResult{}, fmt.Errorf("%w: bad currency %q", domain.ErrValidation, in.Price.Currency)
	}
	if err := mustPositive("price", in.Price.Amount); err != nil {
		return SnapshotResult{}, err
	}
	if in.Converted != nil && !domain.ValidCurrency(in.Converted.Currency) {
		return SnapshotResult{}, fmt.Errorf("%w: bad converted currency", domain.ErrValidation)
	}
	observedAt := in.ObservedAt
	if observedAt.IsZero() {
		observedAt = r.now()
	}

	var result SnapshotResult

	err := r.withTx(ctx, func(tx pgx.Tx) error {
		item, err := lockItem(ctx, tx, in.TrackedItemID)
		if err != nil {
			return err
		}
		telegramID, err := telegramIDOf(ctx, tx, item.UserID)
		if err != nil {
			return err
		}

		previous, err := latestSnapshot(ctx, tx, item.ID)
		if err != nil {
			return err
		}
		allTimeMin, err := allTimeMinimum(ctx, tx, item.ID, in.Price.Currency)
		if err != nil {
			return err
		}

		convAmount, convCurrency := splitMoney(in.Converted)
		row := tx.QueryRow(ctx, `
INSERT INTO price_snapshots (id, tracked_item_id, amount, currency,
                             converted_amount, converted_currency, in_stock, observed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, tracked_item_id, amount, currency, converted_amount, converted_currency, in_stock, observed_at`,
			uuid.NewString(), item.ID, in.Price.Amount, string(in.Price.Currency),
			convAmount, convCurrency, in.InStock, observedAt)

		snapshot, err := scanSnapshot(row)
		if err != nil {
			return classify(err, "insert snapshot")
		}

		decided := DecideAlerts(AlertInput{
			Item:       item,
			Previous:   previous,
			AllTimeMin: allTimeMin,
			New:        snapshot,
		})
		stored, err := persistAlerts(ctx, tx, decided)
		if err != nil {
			return err
		}
		result = SnapshotResult{Snapshot: snapshot, Item: item, TelegramID: telegramID, Alerts: stored}

		// A successful scrape clears the failure state, releases the lease and
		// schedules the next check one interval out.
		if _, updateErr := tx.Exec(ctx, `
UPDATE tracked_items
   SET failure_streak = 0,
       last_error     = '',
       claimed_until  = NULL,
       title          = CASE WHEN title = '' AND $2 <> '' THEN $2 ELSE title END,
       next_check_at  = now() + make_interval(secs => check_interval_seconds),
       updated_at     = now()
 WHERE id = $1`, item.ID, truncate(in.ObservedTitle, domain.MaxItemTitleLength)); updateErr != nil {
			return fmt.Errorf("reschedule item: %w", updateErr)
		}
		return nil
	})
	if err != nil {
		return SnapshotResult{}, err
	}
	return result, nil
}

// RecordFailure marks a scrape attempt that produced no price and backs the item
// off exponentially. It returns an alert only when the streak first reaches the
// threshold, so one dead page cannot turn into a message every few minutes.
func (r *Repo) RecordFailure(ctx context.Context, itemID, reason string) (FailureResult, error) {
	var result FailureResult

	err := r.withTx(ctx, func(tx pgx.Tx) error {
		item, err := lockItem(ctx, tx, itemID)
		if err != nil {
			return err
		}
		telegramID, err := telegramIDOf(ctx, tx, item.UserID)
		if err != nil {
			return err
		}
		streak := item.FailureStreak + 1

		backoff := time.Duration(math.Min(
			float64(item.CheckInterval)*math.Pow(2, float64(streak-1)),
			float64(maxFailureBackoff),
		))

		if _, updateErr := tx.Exec(ctx, `
UPDATE tracked_items
   SET failure_streak = $2,
       last_error     = $3,
       claimed_until  = NULL,
       next_check_at  = now() + make_interval(secs => $4),
       updated_at     = now()
 WHERE id = $1`, itemID, streak, truncate(reason, 500), int32(backoff.Seconds())); updateErr != nil {
			return fmt.Errorf("record failure: %w", updateErr)
		}

		alerts, err := persistAlerts(ctx, tx, DecideFailureAlert(item, streak, FailureStreakThreshold))
		if err != nil {
			return err
		}
		result = FailureResult{Streak: streak, Item: item, TelegramID: telegramID, Alerts: alerts}
		return nil
	})
	if err != nil {
		return FailureResult{}, err
	}
	return result, nil
}

// History streams the snapshots of an item oldest-first, invoking yield for each
// row. Streaming rather than collecting keeps memory flat for long windows.
func (r *Repo) History(ctx context.Context, userID, itemID string, since time.Time, limit int,
	yield func(domain.PriceSnapshot) error,
) error {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	// Ownership is checked once, up front: the stream itself must not leak
	// another user's history.
	if _, err := r.GetItem(ctx, userID, itemID); err != nil {
		return err
	}

	rows, err := r.pool.Query(ctx, `
SELECT id, tracked_item_id, amount, currency, converted_amount, converted_currency, in_stock, observed_at
  FROM price_snapshots
 WHERE tracked_item_id = $1
   AND ($2::TIMESTAMPTZ IS NULL OR observed_at >= $2)
 ORDER BY observed_at
 LIMIT $3`, itemID, nullTime(since), limit)
	if err != nil {
		return fmt.Errorf("history: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		snap, err := scanSnapshot(rows)
		if err != nil {
			return err
		}
		if err := yield(snap); err != nil {
			return err
		}
	}
	return rows.Err()
}

const statsSQL = `
WITH latest AS (
    SELECT amount, currency, in_stock, observed_at
      FROM price_snapshots
     WHERE tracked_item_id = $1
     ORDER BY observed_at DESC
     LIMIT 1
),
-- Restrict the window to the currency of the newest sample: if a shop switched
-- currency, averaging the two would produce a meaningless number.
window_rows AS (
    SELECT s.amount, s.observed_at
      FROM price_snapshots s, latest
     WHERE s.tracked_item_id = $1
       AND s.observed_at >= $2
       AND s.currency = latest.currency
)
SELECT (SELECT currency   FROM latest),
       (SELECT amount     FROM latest),
       (SELECT in_stock   FROM latest),
       count(*),
       min(amount),
       max(amount),
       (avg(amount))::BIGINT,
       min(observed_at),
       max(observed_at),
       (SELECT amount FROM window_rows ORDER BY observed_at LIMIT 1)
  FROM window_rows`

// Stats aggregates the price history over a window ending now.
func (r *Repo) Stats(ctx context.Context, userID, itemID string, window time.Duration) (domain.Stats, error) {
	if _, err := r.GetItem(ctx, userID, itemID); err != nil {
		return domain.Stats{}, err
	}
	if window <= 0 {
		window = 14 * 24 * time.Hour
	}
	from := r.now().Add(-window)

	var (
		currency               *string
		current, minA, maxA    *int64
		avgA, firstA           *int64
		inStock                *bool
		samples                int
		firstObserved, lastObs *time.Time
	)
	err := r.pool.QueryRow(ctx, statsSQL, itemID, from).Scan(
		&currency, &current, &inStock, &samples,
		&minA, &maxA, &avgA, &firstObserved, &lastObs, &firstA,
	)
	if err != nil {
		return domain.Stats{}, fmt.Errorf("stats: %w", err)
	}
	if currency == nil || samples == 0 {
		return domain.Stats{}, fmt.Errorf("no price history for item %s: %w", itemID, domain.ErrNotFound)
	}

	cur := domain.Currency(*currency)
	stats := domain.Stats{
		TrackedItemID: itemID,
		Currency:      cur,
		Current:       domain.Money{Amount: deref(current), Currency: cur},
		Min:           domain.Money{Amount: deref(minA), Currency: cur},
		Max:           domain.Money{Amount: deref(maxA), Currency: cur},
		Avg:           domain.Money{Amount: deref(avgA), Currency: cur},
		First:         domain.Money{Amount: deref(firstA), Currency: cur},
		Samples:       samples,
		InStock:       inStock != nil && *inStock,
		WindowFrom:    from,
	}
	if lastObs != nil {
		stats.WindowTo = *lastObs
	}
	if firstObserved != nil {
		stats.WindowFrom = *firstObserved
	}
	return stats, nil
}

// ---------------------------------------------------------------- internals

const lockItemSQL = `SELECT ` + itemColumnsNoSnapshot + ` FROM tracked_items WHERE id = $1 FOR UPDATE`

// lockItem takes a row lock so that concurrent snapshots for the same item are
// serialised and each sees the other's result.
func lockItem(ctx context.Context, tx pgx.Tx, itemID string) (domain.TrackedItem, error) {
	item, err := scanItemNoSnapshot(tx.QueryRow(ctx, lockItemSQL, itemID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.TrackedItem{}, fmt.Errorf("item %s: %w", itemID, domain.ErrNotFound)
	}
	if err != nil {
		return domain.TrackedItem{}, fmt.Errorf("lock item: %w", err)
	}
	return item, nil
}

// telegramIDOf resolves the chat to notify. It is read inside the same
// transaction as the alert decision so a deleted user cannot produce alerts.
func telegramIDOf(ctx context.Context, tx pgx.Tx, userID string) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, `SELECT telegram_id FROM users WHERE id = $1`, userID).Scan(&id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, fmt.Errorf("user %s: %w", userID, domain.ErrNotFound)
	case err != nil:
		return 0, fmt.Errorf("telegram id: %w", err)
	}
	return id, nil
}

func latestSnapshot(ctx context.Context, tx pgx.Tx, itemID string) (*domain.PriceSnapshot, error) {
	snap, err := scanSnapshot(tx.QueryRow(ctx, `
SELECT id, tracked_item_id, amount, currency, converted_amount, converted_currency, in_stock, observed_at
  FROM price_snapshots WHERE tracked_item_id = $1 ORDER BY observed_at DESC LIMIT 1`, itemID))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("latest snapshot: %w", err)
	}
	return &snap, nil
}

// allTimeMinimum returns the lowest price ever seen in the given currency.
// Comparing only within one currency avoids ranking amounts that are not
// comparable.
func allTimeMinimum(ctx context.Context, tx pgx.Tx, itemID string, currency domain.Currency) (*domain.Money, error) {
	var amount *int64
	err := tx.QueryRow(ctx, `
SELECT min(amount) FROM price_snapshots WHERE tracked_item_id = $1 AND currency = $2`,
		itemID, string(currency)).Scan(&amount)
	if err != nil {
		return nil, fmt.Errorf("all-time minimum: %w", err)
	}
	if amount == nil {
		return nil, nil
	}
	return &domain.Money{Amount: *amount, Currency: currency}, nil
}

// persistAlerts writes the decided alerts and returns only the ones that were
// actually new. The unique dedup_key is what makes a retried scrape produce no
// duplicate message.
func persistAlerts(ctx context.Context, tx pgx.Tx, alerts []domain.Alert) ([]domain.Alert, error) {
	if len(alerts) == 0 {
		return nil, nil
	}
	stored := make([]domain.Alert, 0, len(alerts))

	for _, a := range alerts {
		prevAmount, prevCurrency := splitMoney(a.PreviousPrice)
		// A degraded-scrape alert has no price at all; storing NULL keeps that
		// distinguishable from "the price was zero".
		var price *domain.Money
		if a.Price.Currency != "" {
			p := a.Price
			price = &p
		}
		amount, currency := splitMoney(price)
		var id string
		err := tx.QueryRow(ctx, `
INSERT INTO alerts (id, tracked_item_id, user_id, kind, amount, currency,
                    previous_amount, previous_currency, dedup_key)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (dedup_key) DO NOTHING
RETURNING id`,
			uuid.NewString(), a.TrackedItemID, a.UserID, string(a.Kind),
			amount, currency,
			prevAmount, prevCurrency, a.DedupKey).Scan(&id)

		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// Already reported; nothing to deliver.
			continue
		case err != nil:
			return nil, classify(err, "insert alert")
		}
		a.ID = id
		stored = append(stored, a)
	}
	return stored, nil
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func deref(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}
