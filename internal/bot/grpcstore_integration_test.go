package bot

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	pb "github.com/NE-smirnov/price-tracker-bot/internal/gen/pricetracker/v1"

	"github.com/NE-smirnov/price-tracker-bot/internal/core"
	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

// These tests run the real chain: CoreStore -> gRPC -> core servers -> Postgres.
// The point is not to re-test core's logic but to prove the two halves agree on
// the wire — a field the client forgets to map or a status code it translates
// wrongly cannot be caught by testing either side alone.
//
// They are skipped without TEST_DATABASE_URL, exactly like core's own DB tests.
func newLiveStore(t *testing.T) (*CoreStore, domain.User) {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set: skipping bot<->core wire test")
	}

	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	resetSchema(t, pool)

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	repo := core.NewRepo(pool)

	server := grpc.NewServer()
	pb.RegisterItemServiceServer(server, core.NewItemServer(repo, log))
	pb.RegisterPricingServiceServer(server, core.NewPricingServer(repo, nil, log))

	listener, err := net.Listen("tcp", "127.0.0.1:0") //nolint:noctx // test listener, torn down by Cleanup
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	store, err := NewCoreStore(CoreStoreOptions{
		Addr:        listener.Addr().String(),
		CallTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	user, err := store.EnsureUser(ctx, 1001, "wire", "ru")
	if err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	return store, user
}

// resetSchema reapplies the migration so each test starts from a known state.
func resetSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	ctx := context.Background()
	root := filepath.Join("..", "..")

	for _, name := range []string{
		filepath.Join(root, "migrations", "000001_init.down.sql"),
		filepath.Join(root, "migrations", "000001_init.up.sql"),
	} {
		sql, err := os.ReadFile(name) //nolint:gosec // fixed path inside the repo
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(name), err)
		}
	}
}

func TestCoreStoreImplementsStore(t *testing.T) {
	// Compile-time proof that the gRPC client can replace the in-memory one.
	var _ Store = (*CoreStore)(nil)
}

func TestCoreStoreRoundTrip(t *testing.T) {
	store, user := newLiveStore(t)
	ctx := context.Background()

	if user.ID == "" {
		t.Fatal("user id was not mapped back from the response")
	}
	if user.DefaultCurrency != domain.DefaultCurrency {
		t.Fatalf("default currency = %q, want %q", user.DefaultCurrency, domain.DefaultCurrency)
	}

	target := domain.Money{Amount: 250000, Currency: "TRY"}
	item, err := store.AddItem(ctx, AddItemInput{
		UserID:   user.ID,
		URL:      "https://shop.example.com/p/1?utm_source=tg",
		Title:    "Наушники",
		Target:   &target,
		Interval: 30 * time.Minute,
	})
	if err != nil {
		t.Fatalf("add item: %v", err)
	}

	// Tracking-parameter stripping happens server-side; seeing it here proves the
	// client returns the stored item rather than echoing its own input.
	if item.URL != "https://shop.example.com/p/1" {
		t.Fatalf("url = %q, want the normalised one", item.URL)
	}
	if item.TargetPrice == nil || item.TargetPrice.Amount != 250000 {
		t.Fatalf("target price = %+v, want 250000 TRY", item.TargetPrice)
	}
	if item.CheckInterval != 30*time.Minute {
		t.Fatalf("interval = %s, want 30m", item.CheckInterval)
	}
	if !item.Active {
		t.Fatal("new item must be active")
	}

	got, err := store.GetItem(ctx, user.ID, item.ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if got.ID != item.ID || got.Title != "Наушники" {
		t.Fatalf("get item returned %+v", got)
	}

	list, err := store.ListItems(ctx, user.ID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list returned %d items, want 1", len(list))
	}

	if err := store.SetInterval(ctx, user.ID, item.ID, 2*time.Hour); err != nil {
		t.Fatalf("set interval: %v", err)
	}
	if got, err = store.GetItem(ctx, user.ID, item.ID); err != nil || got.CheckInterval != 2*time.Hour {
		t.Fatalf("interval after update = %s (err %v)", got.CheckInterval, err)
	}

	if err := store.SetDefaultCurrency(ctx, user.ID, "TRY"); err != nil {
		t.Fatalf("set currency: %v", err)
	}

	if err := store.RemoveItem(ctx, user.ID, item.ID); err != nil {
		t.Fatalf("remove item: %v", err)
	}
	if _, err := store.GetItem(ctx, user.ID, item.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("get after remove: err = %v, want ErrNotFound", err)
	}
}

func TestCoreStoreHistoryAndStats(t *testing.T) {
	store, user := newLiveStore(t)
	ctx := context.Background()

	item, err := store.AddItem(ctx, AddItemInput{
		UserID:   user.ID,
		URL:      "https://shop.example.com/p/2",
		Title:    "Монитор",
		Interval: time.Hour,
	})
	if err != nil {
		t.Fatalf("add item: %v", err)
	}

	// Snapshots are written through the pricing client directly: the bot never
	// records prices itself, that is the scraper's job.
	for _, amount := range []int64{300000, 280000, 240000} {
		if _, err := store.pricing.RecordSnapshot(ctx, &pb.RecordSnapshotRequest{
			TrackedItemId: item.ID,
			Price:         &pb.Money{Amount: amount, Currency: "TRY"},
			InStock:       true,
		}); err != nil {
			t.Fatalf("record snapshot %d: %v", amount, err)
		}
	}

	history, err := store.History(ctx, user.ID, item.ID, 24*time.Hour)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("history has %d snapshots, want 3", len(history))
	}
	if history[0].Price.Amount != 300000 || history[2].Price.Amount != 240000 {
		t.Fatalf("history is not oldest-first: %d then %d",
			history[0].Price.Amount, history[2].Price.Amount)
	}
	if history[0].Price.Currency != "TRY" {
		t.Fatalf("currency = %q, want TRY", history[0].Price.Currency)
	}

	stats, err := store.Stats(ctx, user.ID, item.ID, 24*time.Hour)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Samples != 3 {
		t.Fatalf("samples = %d, want 3", stats.Samples)
	}
	if stats.Min.Amount != 240000 || stats.Max.Amount != 300000 {
		t.Fatalf("min/max = %d/%d, want 240000/300000", stats.Min.Amount, stats.Max.Amount)
	}
	if stats.Current.Amount != 240000 || stats.First.Amount != 300000 {
		t.Fatalf("current/first = %d/%d", stats.Current.Amount, stats.First.Amount)
	}
	// First is what makes the client-side trend match the server's view; a missing
	// mapping would silently produce TrendFlat here.
	if stats.Trend() != domain.TrendDown {
		t.Fatalf("trend = %s, want down", stats.Trend())
	}
	if pct := stats.ChangePercent(); pct > -19 || pct < -21 {
		t.Fatalf("change percent = %.2f, want about -20", pct)
	}
}

func TestCoreStoreErrorTranslation(t *testing.T) {
	store, user := newLiveStore(t)
	ctx := context.Background()

	const url = "https://shop.example.com/p/3"
	if _, err := store.AddItem(ctx, AddItemInput{
		UserID:   user.ID,
		URL:      url,
		Interval: time.Hour,
	}); err != nil {
		t.Fatalf("add item: %v", err)
	}

	tests := []struct {
		name string
		run  func() error
		want error
	}{
		{
			name: "duplicate url",
			run: func() error {
				_, err := store.AddItem(ctx, AddItemInput{UserID: user.ID, URL: url, Interval: time.Hour})
				return err
			},
			want: domain.ErrAlreadyExist,
		},
		{
			name: "unparsable url",
			run: func() error {
				_, err := store.AddItem(ctx, AddItemInput{UserID: user.ID, URL: "not a url", Interval: time.Hour})
				return err
			},
			want: domain.ErrValidation,
		},
		{
			name: "interval below the minimum",
			run: func() error {
				_, err := store.AddItem(ctx, AddItemInput{
					UserID: user.ID, URL: "https://shop.example.com/p/4", Interval: time.Second,
				})
				return err
			},
			want: domain.ErrValidation,
		},
		{
			name: "unknown item",
			run: func() error {
				_, err := store.GetItem(ctx, user.ID, "00000000-0000-0000-0000-000000000009")
				return err
			},
			want: domain.ErrNotFound,
		},
		{
			name: "stats without history",
			run: func() error {
				item, err := store.AddItem(ctx, AddItemInput{
					UserID: user.ID, URL: "https://shop.example.com/p/5", Interval: time.Hour,
				})
				if err != nil {
					return err
				}
				_, err = store.Stats(ctx, user.ID, item.ID, time.Hour)
				return err
			},
			want: domain.ErrNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestCoreStoreRejectsForeignItem(t *testing.T) {
	store, owner := newLiveStore(t)
	ctx := context.Background()

	item, err := store.AddItem(ctx, AddItemInput{
		UserID:   owner.ID,
		URL:      "https://shop.example.com/p/6",
		Interval: time.Hour,
	})
	if err != nil {
		t.Fatalf("add item: %v", err)
	}

	intruder, err := store.EnsureUser(ctx, 2002, "intruder", "ru")
	if err != nil {
		t.Fatalf("ensure intruder: %v", err)
	}

	// Ownership is enforced by core, but the check is repeated here because the
	// bot passes the user id on every call and a client-side mix-up would look
	// exactly like a successful read.
	if _, err := store.GetItem(ctx, intruder.ID, item.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("foreign get: err = %v, want ErrNotFound", err)
	}
	if err := store.RemoveItem(ctx, intruder.ID, item.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("foreign remove: err = %v, want ErrNotFound", err)
	}
	if err := store.SetInterval(ctx, intruder.ID, item.ID, 2*time.Hour); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("foreign update: err = %v, want ErrNotFound", err)
	}
	if _, err := store.History(ctx, intruder.ID, item.ID, time.Hour); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("foreign history: err = %v, want ErrNotFound", err)
	}
}

func TestCoreStoreUnavailableCoreIsReported(t *testing.T) {
	// A closed port stands in for "core is down": the bot must surface a plain
	// error instead of blocking, because WaitForReady is deliberately off.
	store, err := NewCoreStore(CoreStoreOptions{Addr: "127.0.0.1:1", CallTimeout: time.Second})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer func() { _ = store.Close() }()

	start := time.Now()
	if _, err := store.EnsureUser(context.Background(), 1, "x", "ru"); err == nil {
		t.Fatal("expected an error when core is unreachable")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("call took %s: it should fail fast, not hang", elapsed)
	}
}
