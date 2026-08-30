package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

// These tests run against a real PostgreSQL instance because most of the logic
// under test *is* SQL: leases with SKIP LOCKED, ON CONFLICT dedup and the
// transaction that makes "all-time low" race-free cannot be verified by a mock.
//
// They are skipped unless TEST_DATABASE_URL points at a scratch database. The
// schema is (re)applied from migrations/ so the tests also verify the migration.

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping database integration tests")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	applySchema(t, pool)
	return pool
}

// applySchema drops and recreates the schema from the migration files, so every
// test starts from a known state and a broken migration fails the suite.
func applySchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	root := repoRoot(t)
	down := readFile(t, filepath.Join(root, "migrations", "000001_init.down.sql"))
	up := readFile(t, filepath.Join(root, "migrations", "000001_init.up.sql"))

	// The down migration may fail on a fresh database; that is expected.
	_, _ = pool.Exec(ctx, down)
	if _, err := pool.Exec(ctx, up); err != nil {
		t.Fatalf("apply up migration: %v", err)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	// The test binary runs in internal/core.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return root
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // fixed path inside the repo
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func newTestRepo(t *testing.T) (*Repo, context.Context) {
	t.Helper()
	repo := NewRepo(testPool(t))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return repo, ctx
}

func mustUser(t *testing.T, repo *Repo, ctx context.Context, telegramID int64) domain.User {
	t.Helper()
	user, _, err := repo.EnsureUser(ctx, telegramID, "tester", "ru")
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	return user
}

func mustItem(t *testing.T, repo *Repo, ctx context.Context, in CreateItemInput) domain.TrackedItem {
	t.Helper()
	item, err := repo.CreateItem(ctx, in)
	if err != nil {
		t.Fatalf("create item: %v", err)
	}
	return item
}

// ---------------------------------------------------------------- users

func TestEnsureUserIsIdempotent(t *testing.T) {
	repo, ctx := newTestRepo(t)

	first, created, err := repo.EnsureUser(ctx, 111, "nikita", "ru")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if !created {
		t.Fatal("first EnsureUser must report created=true")
	}

	second, created, err := repo.EnsureUser(ctx, 111, "nikita_renamed", "en")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if created {
		t.Fatal("second EnsureUser must report created=false")
	}
	if second.ID != first.ID {
		t.Fatalf("user id changed: %s -> %s", first.ID, second.ID)
	}
	// Telegram profile fields are refreshed; the currency the user chose is not.
	if second.Username != "nikita_renamed" || second.Language != "en" {
		t.Fatalf("profile not refreshed: %+v", second)
	}
	if second.DefaultCurrency != domain.DefaultCurrency {
		t.Fatalf("currency = %q, want the default", second.DefaultCurrency)
	}
}

func TestUpdateUserSettings(t *testing.T) {
	repo, ctx := newTestRepo(t)
	user := mustUser(t, repo, ctx, 222)

	eur := domain.Currency("EUR")
	updated, err := repo.UpdateUserSettings(ctx, user.ID, &eur)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.DefaultCurrency != eur {
		t.Fatalf("currency = %q, want EUR", updated.DefaultCurrency)
	}

	// A nil field must leave the stored value alone.
	unchanged, err := repo.UpdateUserSettings(ctx, user.ID, nil)
	if err != nil {
		t.Fatalf("no-op update: %v", err)
	}
	if unchanged.DefaultCurrency != eur {
		t.Fatalf("no-op update changed the currency to %q", unchanged.DefaultCurrency)
	}

	bad := domain.Currency("euro")
	if _, err := repo.UpdateUserSettings(ctx, user.ID, &bad); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected validation error, got %v", err)
	}
	if _, err := repo.UpdateUserSettings(ctx, "00000000-0000-0000-0000-000000000000", &eur); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}

// ---------------------------------------------------------------- items

func TestCreateItemNormalizesAndRejectsDuplicates(t *testing.T) {
	repo, ctx := newTestRepo(t)
	user := mustUser(t, repo, ctx, 333)

	target := domain.Money{Amount: 9900, Currency: "USD"}
	item := mustItem(t, repo, ctx, CreateItemInput{
		UserID:      user.ID,
		URL:         "https://shop.example.com/p/1?utm_source=telegram",
		Title:       "Widget",
		TargetPrice: &target,
		Interval:    30 * time.Minute,
	})
	if item.URL == "" || item.URL != mustNormalize(t, "https://shop.example.com/p/1?utm_source=telegram") {
		t.Fatalf("url was not normalised: %q", item.URL)
	}
	if item.TargetPrice == nil || *item.TargetPrice != target {
		t.Fatalf("target price = %v", item.TargetPrice)
	}
	if item.CheckInterval != 30*time.Minute {
		t.Fatalf("interval = %v", item.CheckInterval)
	}
	if !item.Active {
		t.Fatal("a new item must be active")
	}

	// The same product with different tracking parameters is still the same URL.
	_, err := repo.CreateItem(ctx, CreateItemInput{
		UserID: user.ID,
		URL:    "https://shop.example.com/p/1?utm_source=other",
	})
	if !errors.Is(err, domain.ErrAlreadyExist) {
		t.Fatalf("expected already exists, got %v", err)
	}
}

func mustNormalize(t *testing.T, raw string) string {
	t.Helper()
	got, err := domain.NormalizeURL(raw)
	if err != nil {
		t.Fatalf("normalize %q: %v", raw, err)
	}
	return got
}

func TestCreateItemValidation(t *testing.T) {
	repo, ctx := newTestRepo(t)
	user := mustUser(t, repo, ctx, 334)

	cases := map[string]CreateItemInput{
		"not a url": {UserID: user.ID, URL: "definitely not a url"},
		"interval too short": {
			UserID:   user.ID,
			URL:      "https://shop.example.com/p/2",
			Interval: time.Second,
		},
		"negative target": {
			UserID:      user.ID,
			URL:         "https://shop.example.com/p/3",
			TargetPrice: &domain.Money{Amount: -1, Currency: "USD"},
		},
		"bad target currency": {
			UserID:      user.ID,
			URL:         "https://shop.example.com/p/4",
			TargetPrice: &domain.Money{Amount: 100, Currency: "dollars"},
		},
	}

	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := repo.CreateItem(ctx, in); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("expected validation error, got %v", err)
			}
		})
	}
}

func TestCreateItemEnforcesPerUserLimit(t *testing.T) {
	repo, ctx := newTestRepo(t)
	user := mustUser(t, repo, ctx, 335)

	for i := 0; i < domain.MaxItemsPerUser; i++ {
		mustItem(t, repo, ctx, CreateItemInput{
			UserID: user.ID,
			URL:    "https://shop.example.com/p/" + itoa(i),
		})
	}
	_, err := repo.CreateItem(ctx, CreateItemInput{
		UserID: user.ID,
		URL:    "https://shop.example.com/p/over-the-limit",
	})
	if !errors.Is(err, domain.ErrLimitReached) {
		t.Fatalf("expected limit reached, got %v", err)
	}
}

func TestGetItemIsScopedToOwner(t *testing.T) {
	repo, ctx := newTestRepo(t)
	owner := mustUser(t, repo, ctx, 336)
	stranger := mustUser(t, repo, ctx, 337)

	item := mustItem(t, repo, ctx, CreateItemInput{UserID: owner.ID, URL: "https://shop.example.com/p/secret"})

	if _, err := repo.GetItem(ctx, owner.ID, item.ID); err != nil {
		t.Fatalf("owner cannot read own item: %v", err)
	}
	// Another user's item must be indistinguishable from a missing one.
	if _, err := repo.GetItem(ctx, stranger.ID, item.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected not found for a stranger, got %v", err)
	}
	if err := repo.DeleteItem(ctx, stranger.ID, item.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a stranger must not be able to delete: %v", err)
	}
}

func TestUpdateItemPartialAndClearTarget(t *testing.T) {
	repo, ctx := newTestRepo(t)
	user := mustUser(t, repo, ctx, 338)

	target := domain.Money{Amount: 5000, Currency: "USD"}
	item := mustItem(t, repo, ctx, CreateItemInput{
		UserID:      user.ID,
		URL:         "https://shop.example.com/p/upd",
		TargetPrice: &target,
		Interval:    time.Hour,
	})

	newTarget := domain.Money{Amount: 4000, Currency: "EUR"}
	updated, err := repo.UpdateItem(ctx, user.ID, item.ID, UpdateItemPatch{TargetPrice: &newTarget})
	if err != nil {
		t.Fatalf("update target: %v", err)
	}
	if updated.TargetPrice == nil || *updated.TargetPrice != newTarget {
		t.Fatalf("target = %v, want %v", updated.TargetPrice, newTarget)
	}
	if updated.CheckInterval != time.Hour {
		t.Fatalf("interval must be untouched, got %v", updated.CheckInterval)
	}

	inactive := false
	shorter := 10 * time.Minute
	updated, err = repo.UpdateItem(ctx, user.ID, item.ID, UpdateItemPatch{
		ClearTargetPrice: true,
		Interval:         &shorter,
		Active:           &inactive,
	})
	if err != nil {
		t.Fatalf("clear target: %v", err)
	}
	if updated.TargetPrice != nil {
		t.Fatalf("target should be cleared, got %v", updated.TargetPrice)
	}
	if updated.CheckInterval != shorter || updated.Active {
		t.Fatalf("patch not applied: %+v", updated)
	}

	if _, err := repo.UpdateItem(ctx, user.ID, item.ID, UpdateItemPatch{Interval: ptr(time.Second)}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected validation error for a 1s interval, got %v", err)
	}
}

func TestListItemsRespectsActiveFilter(t *testing.T) {
	repo, ctx := newTestRepo(t)
	user := mustUser(t, repo, ctx, 339)

	active := mustItem(t, repo, ctx, CreateItemInput{UserID: user.ID, URL: "https://shop.example.com/p/a"})
	paused := mustItem(t, repo, ctx, CreateItemInput{UserID: user.ID, URL: "https://shop.example.com/p/b"})

	off := false
	if _, err := repo.UpdateItem(ctx, user.ID, paused.ID, UpdateItemPatch{Active: &off}); err != nil {
		t.Fatalf("pause: %v", err)
	}

	visible, err := repo.ListItems(ctx, user.ID, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(visible) != 1 || visible[0].ID != active.ID {
		t.Fatalf("expected only the active item, got %d", len(visible))
	}

	all, err := repo.ListItems(ctx, user.ID, true)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected both items, got %d", len(all))
	}
}

// ---------------------------------------------------------------- scheduling

func TestClaimDueItemsLeasesEachItemOnce(t *testing.T) {
	repo, ctx := newTestRepo(t)
	user := mustUser(t, repo, ctx, 340)

	for i := 0; i < 3; i++ {
		mustItem(t, repo, ctx, CreateItemInput{UserID: user.ID, URL: "https://shop.example.com/p/due" + itoa(i)})
	}

	first, err := repo.ClaimDueItems(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(first) != 3 {
		t.Fatalf("expected 3 due items, got %d", len(first))
	}

	// A second worker must find nothing: the lease is what prevents two
	// replicas from hitting the same shop page at the same time.
	second, err := repo.ClaimDueItems(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("expected no items while leased, got %d", len(second))
	}
}

func TestClaimDueItemsSkipsInactiveAndFutureItems(t *testing.T) {
	repo, ctx := newTestRepo(t)
	user := mustUser(t, repo, ctx, 341)

	due := mustItem(t, repo, ctx, CreateItemInput{UserID: user.ID, URL: "https://shop.example.com/p/now"})
	paused := mustItem(t, repo, ctx, CreateItemInput{UserID: user.ID, URL: "https://shop.example.com/p/paused"})

	off := false
	if _, err := repo.UpdateItem(ctx, user.ID, paused.ID, UpdateItemPatch{Active: &off}); err != nil {
		t.Fatalf("pause: %v", err)
	}

	claimed, err := repo.ClaimDueItems(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 || claimed[0].Item.ID != due.ID {
		t.Fatalf("expected only the active due item, got %d", len(claimed))
	}
}

func TestClaimDueItemsCarriesTheOwnerCurrency(t *testing.T) {
	// The scraper converts every price it observes, and it learns which currency to
	// convert into from the lease itself rather than a second round trip per item.
	repo, ctx := newTestRepo(t)
	user := mustUser(t, repo, ctx, 9101)
	try := domain.Currency("TRY")
	if _, err := repo.UpdateUserSettings(ctx, user.ID, &try); err != nil {
		t.Fatalf("set currency: %v", err)
	}
	mustItem(t, repo, ctx, CreateItemInput{UserID: user.ID, URL: "https://shop.example.com/p/one"})

	claimed, err := repo.ClaimDueItems(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d items, want 1", len(claimed))
	}
	if claimed[0].PreferredCurrency != try {
		t.Fatalf("preferred currency = %q, want TRY", claimed[0].PreferredCurrency)
	}
}

// ---------------------------------------------------------------- pricing

func TestRecordSnapshotStoresHistoryAndReschedules(t *testing.T) {
	repo, ctx := newTestRepo(t)
	user := mustUser(t, repo, ctx, 350)
	item := mustItem(t, repo, ctx, CreateItemInput{
		UserID:   user.ID,
		URL:      "https://shop.example.com/p/hist",
		Interval: 30 * time.Minute,
	})

	before := time.Now()
	result, err := repo.RecordSnapshot(ctx, RecordSnapshotInput{
		TrackedItemID: item.ID,
		Price:         &domain.Money{Amount: 12345, Currency: "USD"},
		InStock:       true,
		ObservedTitle: "Widget Pro",
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if result.Snapshot.ID == "" || result.Snapshot.Price.Amount != 12345 {
		t.Fatalf("snapshot not stored correctly: %+v", result.Snapshot)
	}
	if result.TelegramID != 350 {
		t.Fatalf("telegram id = %d, want 350", result.TelegramID)
	}

	stored, err := repo.GetItem(ctx, user.ID, item.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// The title observed on the page fills in an item that was added without one.
	if stored.Title != "Widget Pro" {
		t.Fatalf("title = %q, want the observed one", stored.Title)
	}
	if stored.LastSnapshot == nil || stored.LastSnapshot.Price.Amount != 12345 {
		t.Fatalf("last snapshot not denormalised: %+v", stored.LastSnapshot)
	}
	// The next check is one interval away, so the scraper does not spin.
	if !stored.NextCheckAt.After(before.Add(25 * time.Minute)) {
		t.Fatalf("next check at %v is too early", stored.NextCheckAt)
	}
	if stored.FailureStreak != 0 {
		t.Fatalf("failure streak = %d after success", stored.FailureStreak)
	}
}

func TestRecordSnapshotAlertsAreDeduplicated(t *testing.T) {
	repo, ctx := newTestRepo(t)
	user := mustUser(t, repo, ctx, 351)

	target := domain.Money{Amount: 10000, Currency: "USD"}
	item := mustItem(t, repo, ctx, CreateItemInput{
		UserID:      user.ID,
		URL:         "https://shop.example.com/p/dedup",
		TargetPrice: &target,
	})

	// First observation crosses the threshold and must alert.
	first, err := repo.RecordSnapshot(ctx, RecordSnapshotInput{
		TrackedItemID: item.ID,
		Price:         &domain.Money{Amount: 9000, Currency: "USD"},
		InStock:       true,
	})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if len(first.Alerts) != 1 || first.Alerts[0].Kind != domain.AlertPriceDrop {
		t.Fatalf("expected one price_drop, got %v", kinds(first.Alerts))
	}

	// The same price observed again is not news, and the stored dedup key makes
	// it a no-op even though a new snapshot row is written.
	second, err := repo.RecordSnapshot(ctx, RecordSnapshotInput{
		TrackedItemID: item.ID,
		Price:         &domain.Money{Amount: 9000, Currency: "USD"},
		InStock:       true,
	})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if len(second.Alerts) != 0 {
		t.Fatalf("expected no repeat alerts, got %v", kinds(second.Alerts))
	}

	// A further drop is a new all-time low and a new dedup key.
	third, err := repo.RecordSnapshot(ctx, RecordSnapshotInput{
		TrackedItemID: item.ID,
		Price:         &domain.Money{Amount: 8000, Currency: "USD"},
		InStock:       true,
	})
	if err != nil {
		t.Fatalf("third: %v", err)
	}
	if !equalKinds(kinds(third.Alerts), domain.AlertAllTimeLow) {
		t.Fatalf("expected all_time_low only, got %v", kinds(third.Alerts))
	}
}

func TestRecordSnapshotRejectsUnknownItem(t *testing.T) {
	repo, ctx := newTestRepo(t)

	_, err := repo.RecordSnapshot(ctx, RecordSnapshotInput{
		TrackedItemID: "00000000-0000-0000-0000-000000000000",
		Price:         &domain.Money{Amount: 100, Currency: "USD"},
		InStock:       true,
	})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestRecordFailureBacksOffAndAlertsOnce(t *testing.T) {
	repo, ctx := newTestRepo(t)
	user := mustUser(t, repo, ctx, 352)
	item := mustItem(t, repo, ctx, CreateItemInput{
		UserID:   user.ID,
		URL:      "https://shop.example.com/p/broken",
		Interval: 10 * time.Minute,
	})

	var alertCount int
	for i := 1; i <= FailureStreakThreshold+2; i++ {
		result, err := repo.RecordFailure(ctx, item.ID, "selector not found")
		if err != nil {
			t.Fatalf("failure %d: %v", i, err)
		}
		if result.Streak != i {
			t.Fatalf("streak = %d, want %d", result.Streak, i)
		}
		alertCount += len(result.Alerts)
	}
	if alertCount != 1 {
		t.Fatalf("expected exactly one degraded alert, got %d", alertCount)
	}

	stored, err := repo.GetItem(ctx, user.ID, item.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.LastError == "" {
		t.Fatal("last error must be recorded for diagnosis")
	}
	// Backoff grows, but is capped, so a dead page is still retried.
	if !stored.NextCheckAt.After(time.Now().Add(time.Hour)) {
		t.Fatalf("expected a backed-off next check, got %v", stored.NextCheckAt)
	}
	if stored.NextCheckAt.After(time.Now().Add(maxFailureBackoff + time.Minute)) {
		t.Fatalf("backoff exceeded the cap: %v", stored.NextCheckAt)
	}

	// A successful scrape must clear the failure state.
	if _, err := repo.RecordSnapshot(ctx, RecordSnapshotInput{
		TrackedItemID: item.ID,
		Price:         &domain.Money{Amount: 100, Currency: "USD"},
		InStock:       true,
	}); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	recovered, err := repo.GetItem(ctx, user.ID, item.ID)
	if err != nil {
		t.Fatalf("get after recovery: %v", err)
	}
	if recovered.FailureStreak != 0 || recovered.LastError != "" {
		t.Fatalf("failure state not cleared: %+v", recovered)
	}
}

func TestHistoryAndStats(t *testing.T) {
	repo, ctx := newTestRepo(t)
	user := mustUser(t, repo, ctx, 353)
	item := mustItem(t, repo, ctx, CreateItemInput{UserID: user.ID, URL: "https://shop.example.com/p/stats"})

	base := time.Now().Add(-3 * time.Hour)
	prices := []int64{1000, 1400, 800, 1200}
	for i, amount := range prices {
		if _, err := repo.RecordSnapshot(ctx, RecordSnapshotInput{
			TrackedItemID: item.ID,
			Price:         &domain.Money{Amount: amount, Currency: "USD"},
			InStock:       true,
			ObservedAt:    base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("snapshot %d: %v", i, err)
		}
	}

	var seen []int64
	if err := repo.History(ctx, user.ID, item.ID, time.Time{}, 0, func(s domain.PriceSnapshot) error {
		seen = append(seen, s.Price.Amount)
		return nil
	}); err != nil {
		t.Fatalf("history: %v", err)
	}
	// Oldest first, so a chart can be drawn straight from the stream.
	if len(seen) != len(prices) || seen[0] != 1000 || seen[len(seen)-1] != 1200 {
		t.Fatalf("history order/content wrong: %v", seen)
	}

	stats, err := repo.Stats(ctx, user.ID, item.ID, 24*time.Hour)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Samples != len(prices) {
		t.Fatalf("samples = %d, want %d", stats.Samples, len(prices))
	}
	if stats.Min.Amount != 800 || stats.Max.Amount != 1400 {
		t.Fatalf("min/max = %d/%d", stats.Min.Amount, stats.Max.Amount)
	}
	if stats.Avg.Amount != 1100 {
		t.Fatalf("avg = %d, want 1100", stats.Avg.Amount)
	}
	if stats.First.Amount != 1000 || stats.Current.Amount != 1200 {
		t.Fatalf("first/current = %d/%d", stats.First.Amount, stats.Current.Amount)
	}
	if stats.Trend() != domain.TrendUp {
		t.Fatalf("trend = %q, want up", stats.Trend())
	}
	if stats.Currency != "USD" {
		t.Fatalf("currency = %q", stats.Currency)
	}

	// A window that contains no samples is reported as missing history rather
	// than as a zero-filled result.
	if _, err := repo.Stats(ctx, user.ID, item.ID, time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected not found for an empty window, got %v", err)
	}
}

func TestHistoryIsScopedToOwner(t *testing.T) {
	repo, ctx := newTestRepo(t)
	owner := mustUser(t, repo, ctx, 354)
	stranger := mustUser(t, repo, ctx, 355)
	item := mustItem(t, repo, ctx, CreateItemInput{UserID: owner.ID, URL: "https://shop.example.com/p/private"})

	err := repo.History(ctx, stranger.ID, item.ID, time.Time{}, 0, func(domain.PriceSnapshot) error {
		t.Fatal("a stranger must receive no snapshots")
		return nil
	})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestDeleteItemRemovesHistory(t *testing.T) {
	repo, ctx := newTestRepo(t)
	user := mustUser(t, repo, ctx, 356)
	item := mustItem(t, repo, ctx, CreateItemInput{UserID: user.ID, URL: "https://shop.example.com/p/gone"})

	if _, err := repo.RecordSnapshot(ctx, RecordSnapshotInput{
		TrackedItemID: item.ID,
		Price:         &domain.Money{Amount: 500, Currency: "USD"},
		InStock:       true,
	}); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if err := repo.DeleteItem(ctx, user.ID, item.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	var remaining int
	if err := repo.pool.QueryRow(ctx,
		`SELECT count(*) FROM price_snapshots WHERE tracked_item_id = $1`, item.ID).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("history was not cascaded away: %d rows left", remaining)
	}
	if err := repo.DeleteItem(ctx, user.ID, item.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("second delete should report not found, got %v", err)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

// TestRecordSnapshotCarriesPriceForwardWhenOutOfStock covers the shops that
// remove the price of an unavailable product. Wildberries does exactly this, and
// without carrying the previous price forward the stock transition would be lost
// and the user would never be told the item came back.
func TestRecordSnapshotCarriesPriceForwardWhenOutOfStock(t *testing.T) {
	repo, ctx := newTestRepo(t)
	user := mustUser(t, repo, ctx, 361)
	item := mustItem(t, repo, ctx, CreateItemInput{
		UserID: user.ID,
		URL:    "https://shop.example.com/p/vanishing-price",
	})

	if _, err := repo.RecordSnapshot(ctx, RecordSnapshotInput{
		TrackedItemID: item.ID,
		Price:         &domain.Money{Amount: 5000, Currency: "RUB"},
		InStock:       true,
	}); err != nil {
		t.Fatalf("first: %v", err)
	}

	gone, err := repo.RecordSnapshot(ctx, RecordSnapshotInput{
		TrackedItemID: item.ID,
		InStock:       false,
	})
	if err != nil {
		t.Fatalf("out of stock: %v", err)
	}
	if gone.Snapshot.Price.Amount != 5000 || gone.Snapshot.Price.Currency != "RUB" {
		t.Fatalf("price = %+v, want the previous 5000 RUB carried forward", gone.Snapshot.Price)
	}
	if !equalKinds(kinds(gone.Alerts), domain.AlertOutOfStock) {
		t.Fatalf("expected out_of_stock, got %v", kinds(gone.Alerts))
	}

	back, err := repo.RecordSnapshot(ctx, RecordSnapshotInput{
		TrackedItemID: item.ID,
		Price:         &domain.Money{Amount: 5000, Currency: "RUB"},
		InStock:       true,
	})
	if err != nil {
		t.Fatalf("back in stock: %v", err)
	}
	if !equalKinds(kinds(back.Alerts), domain.AlertBackInStock) {
		t.Fatalf("expected back_in_stock, got %v", kinds(back.Alerts))
	}
}

// TestRecordSnapshotRejectsMissingPriceWhenInStock keeps the relaxation narrow: a
// price that could not be read from an available product is a scrape failure and
// must not be stored as a snapshot.
func TestRecordSnapshotRejectsMissingPriceWhenInStock(t *testing.T) {
	repo, ctx := newTestRepo(t)
	user := mustUser(t, repo, ctx, 362)
	item := mustItem(t, repo, ctx, CreateItemInput{
		UserID: user.ID,
		URL:    "https://shop.example.com/p/no-price",
	})

	if _, err := repo.RecordSnapshot(ctx, RecordSnapshotInput{
		TrackedItemID: item.ID,
		InStock:       true,
	}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v, want a validation error", err)
	}

	// Nothing to carry forward either: the very first observation must have a
	// price, otherwise the row would be meaningless.
	if _, err := repo.RecordSnapshot(ctx, RecordSnapshotInput{
		TrackedItemID: item.ID,
		InStock:       false,
	}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v, want a validation error with no history", err)
	}
}
