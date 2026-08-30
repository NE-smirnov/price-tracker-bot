package bot

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

// memStore is an in-memory Store used while the core service does not exist yet.
// It lets the bot be demoed end to end (add -> list -> stats -> remove) and is
// the reference implementation the gRPC-backed store is tested against.
type memStore struct {
	mu        sync.RWMutex
	users     map[int64]*domain.User // telegram id -> user
	items     map[string]*domain.TrackedItem
	snapshots map[string][]domain.PriceSnapshot // item id -> history (oldest first)

	now  func() time.Time
	rand *rand.Rand
	// seedHistory fabricates a plausible price history for new items so that
	// /stats and trend rendering can be exercised without a running scraper.
	seedHistory bool
}

// NewMemStore returns an in-memory Store.
func NewMemStore(seedHistory bool) Store {
	return &memStore{
		users:       make(map[int64]*domain.User),
		items:       make(map[string]*domain.TrackedItem),
		snapshots:   make(map[string][]domain.PriceSnapshot),
		now:         time.Now,
		rand:        rand.New(rand.NewPCG(42, 1024)),
		seedHistory: seedHistory,
	}
}

func (s *memStore) EnsureUser(_ context.Context, telegramID int64, username, language string) (domain.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if u, ok := s.users[telegramID]; ok {
		if username != "" && u.Username != username {
			u.Username = username
		}
		return *u, nil
	}
	u := &domain.User{
		ID:              uuid.NewString(),
		TelegramID:      telegramID,
		Username:        username,
		Language:        language,
		DefaultCurrency: domain.DefaultCurrency,
		CreatedAt:       s.now(),
	}
	s.users[telegramID] = u
	return *u, nil
}

func (s *memStore) SetDefaultCurrency(_ context.Context, userID string, currency domain.Currency) error {
	if !domain.ValidCurrency(currency) {
		return fmt.Errorf("%w: %q is not an ISO-4217 code", domain.ErrValidation, currency)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, u := range s.users {
		if u.ID == userID {
			u.DefaultCurrency = currency
			return nil
		}
	}
	return fmt.Errorf("user %s: %w", userID, domain.ErrNotFound)
}

func (s *memStore) AddItem(_ context.Context, in AddItemInput) (domain.TrackedItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	owned := 0
	for _, it := range s.items {
		if it.UserID != in.UserID {
			continue
		}
		owned++
		if it.URL == in.URL {
			return domain.TrackedItem{}, fmt.Errorf("this URL is already tracked: %w", domain.ErrAlreadyExist)
		}
	}
	if owned >= domain.MaxItemsPerUser {
		return domain.TrackedItem{}, fmt.Errorf("%w: at most %d items per user", domain.ErrLimitReached, domain.MaxItemsPerUser)
	}

	interval, err := domain.ValidateInterval(in.Interval)
	if err != nil {
		return domain.TrackedItem{}, err
	}

	now := s.now()
	item := &domain.TrackedItem{
		ID:            uuid.NewString(),
		UserID:        in.UserID,
		URL:           in.URL,
		Title:         titleOrHost(in.Title, in.URL),
		TargetPrice:   in.Target,
		CheckInterval: interval,
		Active:        true,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	s.items[item.ID] = item

	if s.seedHistory {
		s.snapshots[item.ID] = s.fakeHistory(item, now)
		if h := s.snapshots[item.ID]; len(h) > 0 {
			last := h[len(h)-1]
			item.LastSnapshot = &last
			item.LastCheckedAt = &last.ObservedAt
		}
	}
	return *item, nil
}

func (s *memStore) ListItems(_ context.Context, userID string) ([]domain.TrackedItem, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]domain.TrackedItem, 0, len(s.items))
	for _, it := range s.items {
		if it.UserID == userID {
			out = append(out, *it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (s *memStore) GetItem(_ context.Context, userID, itemID string) (domain.TrackedItem, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.getItemLocked(userID, itemID)
}

// getItemLocked requires at least a read lock to be held.
func (s *memStore) getItemLocked(userID, itemID string) (domain.TrackedItem, error) {
	it, ok := s.items[itemID]
	// The ownership check is not an optimisation: without it any user could
	// read another user's item by guessing an id.
	if !ok || it.UserID != userID {
		return domain.TrackedItem{}, fmt.Errorf("item %s: %w", itemID, domain.ErrNotFound)
	}
	return *it, nil
}

func (s *memStore) RemoveItem(_ context.Context, userID, itemID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.getItemLocked(userID, itemID); err != nil {
		return err
	}
	delete(s.items, itemID)
	delete(s.snapshots, itemID)
	return nil
}

func (s *memStore) SetInterval(_ context.Context, userID, itemID string, interval time.Duration) error {
	valid, err := domain.ValidateInterval(interval)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	it, ok := s.items[itemID]
	if !ok || it.UserID != userID {
		return fmt.Errorf("item %s: %w", itemID, domain.ErrNotFound)
	}
	it.CheckInterval = valid
	it.UpdatedAt = s.now()
	return nil
}

func (s *memStore) History(_ context.Context, userID, itemID string, window time.Duration) ([]domain.PriceSnapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if _, err := s.getItemLocked(userID, itemID); err != nil {
		return nil, err
	}
	history := s.snapshots[itemID]
	if window <= 0 {
		return append([]domain.PriceSnapshot(nil), history...), nil
	}
	cutoff := s.now().Add(-window)
	out := make([]domain.PriceSnapshot, 0, len(history))
	for _, snap := range history {
		if !snap.ObservedAt.Before(cutoff) {
			out = append(out, snap)
		}
	}
	return out, nil
}

func (s *memStore) Stats(ctx context.Context, userID, itemID string, window time.Duration) (domain.Stats, error) {
	history, err := s.History(ctx, userID, itemID, window)
	if err != nil {
		return domain.Stats{}, err
	}
	if len(history) == 0 {
		return domain.Stats{}, fmt.Errorf("no price history yet: %w", domain.ErrNotFound)
	}
	return AggregateStats(itemID, history), nil
}

// AggregateStats folds a price history into summary statistics.
// It is exported because the core service reuses the same maths.
func AggregateStats(itemID string, history []domain.PriceSnapshot) domain.Stats {
	first := history[0]
	last := history[len(history)-1]

	st := domain.Stats{
		TrackedItemID: itemID,
		Currency:      first.Price.Currency,
		Min:           first.Price,
		Max:           first.Price,
		First:         first.Price,
		Current:       last.Price,
		Samples:       len(history),
		InStock:       last.InStock,
		WindowFrom:    first.ObservedAt,
		WindowTo:      last.ObservedAt,
	}

	var sum int64
	for _, snap := range history {
		if snap.Price.Amount < st.Min.Amount {
			st.Min = snap.Price
		}
		if snap.Price.Amount > st.Max.Amount {
			st.Max = snap.Price
		}
		sum += snap.Price.Amount
	}
	st.Avg = domain.Money{Amount: sum / int64(len(history)), Currency: st.Currency}
	return st
}

// fakeHistory builds a random walk of hourly observations for the last 14 days.
// Requires the write lock to be held (it touches s.rand).
func (s *memStore) fakeHistory(item *domain.TrackedItem, now time.Time) []domain.PriceSnapshot {
	currency := domain.DefaultCurrency
	base := int64(5000 + s.rand.IntN(20000))
	if item.TargetPrice != nil {
		currency = item.TargetPrice.Currency
		base = item.TargetPrice.Amount + int64(s.rand.IntN(3000)) + 500
	}

	const points = 14 * 24
	out := make([]domain.PriceSnapshot, 0, points)
	price := base
	for i := points - 1; i >= 0; i-- {
		// ±1.5% random walk, clamped so the price never goes non-positive.
		delta := int64(float64(price) * (s.rand.Float64() - 0.5) * 0.03)
		price += delta
		if price < 100 {
			price = 100
		}
		out = append(out, domain.PriceSnapshot{
			ID:            uuid.NewString(),
			TrackedItemID: item.ID,
			Price:         domain.Money{Amount: price, Currency: currency},
			InStock:       s.rand.IntN(100) > 5,
			ObservedAt:    now.Add(-time.Duration(i) * time.Hour),
		})
	}
	return out
}

func titleOrHost(title, url string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		if host := domain.HostOf(url); host != "" {
			title = host
		} else {
			title = "item"
		}
	}
	if len(title) > domain.MaxItemTitleLength {
		title = title[:domain.MaxItemTitleLength]
	}
	return title
}
