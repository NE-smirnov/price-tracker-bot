package bot_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/NE-smirnov/price-tracker-bot/internal/bot"
	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

func newStoreWithUser(t *testing.T) (bot.Store, domain.User) {
	t.Helper()

	store := bot.NewMemStore(false)
	user, err := store.EnsureUser(context.Background(), 111, "swortchy", "ru")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	return store, user
}

func TestEnsureUserIsIdempotent(t *testing.T) {
	t.Parallel()

	store, first := newStoreWithUser(t)
	second, err := store.EnsureUser(context.Background(), 111, "swortchy", "ru")
	if err != nil {
		t.Fatalf("EnsureUser: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("second EnsureUser created a new user: %s != %s", first.ID, second.ID)
	}
	if second.DefaultCurrency != domain.DefaultCurrency {
		t.Fatalf("default currency = %q, want %q", second.DefaultCurrency, domain.DefaultCurrency)
	}
}

func TestAddItemRejectsDuplicateURL(t *testing.T) {
	t.Parallel()

	store, user := newStoreWithUser(t)
	ctx := context.Background()
	in := bot.AddItemInput{UserID: user.ID, URL: "https://example.com/p/1", Interval: time.Hour}

	if _, err := store.AddItem(ctx, in); err != nil {
		t.Fatalf("first AddItem: %v", err)
	}
	_, err := store.AddItem(ctx, in)
	if !errors.Is(err, domain.ErrAlreadyExist) {
		t.Fatalf("second AddItem error = %v, want ErrAlreadyExist", err)
	}
}

func TestAddItemEnforcesPerUserLimit(t *testing.T) {
	t.Parallel()

	store, user := newStoreWithUser(t)
	ctx := context.Background()

	for i := 0; i < domain.MaxItemsPerUser; i++ {
		_, err := store.AddItem(ctx, bot.AddItemInput{
			UserID:   user.ID,
			URL:      "https://example.com/p/" + string(rune('a'+i)),
			Interval: time.Hour,
		})
		if err != nil {
			t.Fatalf("AddItem #%d: %v", i, err)
		}
	}
	_, err := store.AddItem(ctx, bot.AddItemInput{
		UserID:   user.ID,
		URL:      "https://example.com/p/over-the-limit",
		Interval: time.Hour,
	})
	if !errors.Is(err, domain.ErrLimitReached) {
		t.Fatalf("AddItem past the limit = %v, want ErrLimitReached", err)
	}
}

func TestAddItemRejectsTooShortInterval(t *testing.T) {
	t.Parallel()

	store, user := newStoreWithUser(t)
	_, err := store.AddItem(context.Background(), bot.AddItemInput{
		UserID:   user.ID,
		URL:      "https://example.com/p/1",
		Interval: time.Second,
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("AddItem with 1s interval = %v, want ErrValidation", err)
	}
}

// A user must never be able to read or delete another user's item by id.
func TestItemsAreIsolatedBetweenUsers(t *testing.T) {
	t.Parallel()

	store, alice := newStoreWithUser(t)
	ctx := context.Background()

	bob, err := store.EnsureUser(ctx, 222, "bob", "en")
	if err != nil {
		t.Fatalf("EnsureUser(bob): %v", err)
	}

	item, err := store.AddItem(ctx, bot.AddItemInput{
		UserID: alice.ID, URL: "https://example.com/secret", Interval: time.Hour,
	})
	if err != nil {
		t.Fatalf("AddItem: %v", err)
	}

	if _, err := store.GetItem(ctx, bob.ID, item.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("bob GetItem(alice item) = %v, want ErrNotFound", err)
	}
	if err := store.RemoveItem(ctx, bob.ID, item.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("bob RemoveItem(alice item) = %v, want ErrNotFound", err)
	}
	if _, err := store.GetItem(ctx, alice.ID, item.ID); err != nil {
		t.Fatalf("alice lost her own item: %v", err)
	}

	items, err := store.ListItems(ctx, bob.ID)
	if err != nil {
		t.Fatalf("ListItems(bob): %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("bob sees %d items, want 0", len(items))
	}
}

func TestSetIntervalAndRemove(t *testing.T) {
	t.Parallel()

	store, user := newStoreWithUser(t)
	ctx := context.Background()

	item, err := store.AddItem(ctx, bot.AddItemInput{
		UserID: user.ID, URL: "https://example.com/p/1", Interval: time.Hour,
	})
	if err != nil {
		t.Fatalf("AddItem: %v", err)
	}

	if err := store.SetInterval(ctx, user.ID, item.ID, 6*time.Hour); err != nil {
		t.Fatalf("SetInterval: %v", err)
	}
	updated, err := store.GetItem(ctx, user.ID, item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if updated.CheckInterval != 6*time.Hour {
		t.Fatalf("interval = %s, want 6h", updated.CheckInterval)
	}

	if err := store.RemoveItem(ctx, user.ID, item.ID); err != nil {
		t.Fatalf("RemoveItem: %v", err)
	}
	if _, err := store.GetItem(ctx, user.ID, item.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetItem after remove = %v, want ErrNotFound", err)
	}
}

// Telegram delivers updates concurrently, so the store must be race-free.
func TestMemStoreIsConcurrencySafe(t *testing.T) {
	t.Parallel()

	store, user := newStoreWithUser(t)
	ctx := context.Background()

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func(i int) {
			defer wg.Done()
			//nolint:errcheck // duplicates and limits are expected here; we only assert no race/panic
			_, _ = store.AddItem(ctx, bot.AddItemInput{
				UserID:   user.ID,
				URL:      "https://example.com/p/" + string(rune('A'+i%10)),
				Interval: time.Hour,
			})
			_, _ = store.ListItems(ctx, user.ID)
		}(i)
	}
	wg.Wait()

	items, err := store.ListItems(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(items) == 0 || len(items) > 10 {
		t.Fatalf("got %d items, want between 1 and 10 (10 distinct URLs)", len(items))
	}
}

func TestStatsAggregation(t *testing.T) {
	t.Parallel()

	now := time.Now()
	history := []domain.PriceSnapshot{
		{Price: domain.Money{Amount: 1000, Currency: "USD"}, ObservedAt: now.Add(-3 * time.Hour), InStock: true},
		{Price: domain.Money{Amount: 1400, Currency: "USD"}, ObservedAt: now.Add(-2 * time.Hour), InStock: true},
		{Price: domain.Money{Amount: 600, Currency: "USD"}, ObservedAt: now.Add(-time.Hour), InStock: false},
		{Price: domain.Money{Amount: 800, Currency: "USD"}, ObservedAt: now, InStock: true},
	}

	st := bot.AggregateStats("item-1", history)

	if st.Min.Amount != 600 {
		t.Errorf("Min = %d, want 600", st.Min.Amount)
	}
	if st.Max.Amount != 1400 {
		t.Errorf("Max = %d, want 1400", st.Max.Amount)
	}
	if st.Avg.Amount != 950 {
		t.Errorf("Avg = %d, want 950", st.Avg.Amount)
	}
	if st.Current.Amount != 800 {
		t.Errorf("Current = %d, want 800", st.Current.Amount)
	}
	if st.Samples != 4 {
		t.Errorf("Samples = %d, want 4", st.Samples)
	}
	if !st.InStock {
		t.Error("InStock must follow the latest snapshot")
	}
	if st.Trend() != domain.TrendDown {
		t.Errorf("Trend = %q, want down", st.Trend())
	}
	if got := st.ChangePercent(); got != -20 {
		t.Errorf("ChangePercent = %.2f, want -20", got)
	}
}

func TestStatsWithoutHistory(t *testing.T) {
	t.Parallel()

	store, user := newStoreWithUser(t)
	ctx := context.Background()

	item, err := store.AddItem(ctx, bot.AddItemInput{
		UserID: user.ID, URL: "https://example.com/p/1", Interval: time.Hour,
	})
	if err != nil {
		t.Fatalf("AddItem: %v", err)
	}
	if _, err := store.Stats(ctx, user.ID, item.ID, 24*time.Hour); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Stats without history = %v, want ErrNotFound", err)
	}
}
