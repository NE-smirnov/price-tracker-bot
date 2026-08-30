package currency

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
	"github.com/NE-smirnov/price-tracker-bot/internal/platform/redisclient"
)

// PivotCurrency is the base every table is fetched in.
//
// One table per hour answers every pair by cross-rate, which keeps the service
// well inside any free provider's quota no matter how many currencies users pick.
const PivotCurrency = domain.Currency("USD")

// staleGrace is how long a table may be served past its cache TTL when the
// provider is unreachable. A slightly old rate converts a price a fraction of a
// percent wrong; no rate at all means the price-drop alert cannot fire, which is
// the worse failure.
const staleGrace = 24 * time.Hour

// Service converts amounts, caching provider tables in Redis.
type Service struct {
	provider Provider
	cache    *redisclient.Cache
	log      *slog.Logger
	now      func() time.Time

	// mu guards the in-process copy of the table. It also serialises refreshes,
	// so a burst of conversions after a cache expiry makes one provider request
	// rather than one per worker.
	mu     sync.Mutex
	table  Table
	loaded time.Time
	ttl    time.Duration
}

// Options configures the service.
type Options struct {
	Provider Provider
	Cache    *redisclient.Cache
	Log      *slog.Logger
	// TTL is how long a fetched table is considered fresh in this process. The
	// Redis TTL is configured on the cache itself.
	TTL time.Duration
	Now func() time.Time
}

// NewService builds a converter.
func NewService(opts Options) *Service {
	if opts.TTL <= 0 {
		opts.TTL = time.Hour
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &Service{
		provider: opts.Provider,
		cache:    opts.Cache,
		log:      opts.Log,
		now:      opts.Now,
		ttl:      opts.TTL,
	}
}

// Rate returns the conversion factor from one currency to another.
func (s *Service) Rate(ctx context.Context, from, to domain.Currency) (Rate, error) {
	from = domain.NormalizeCurrency(string(from))
	to = domain.NormalizeCurrency(string(to))
	if !domain.ValidCurrency(from) || !domain.ValidCurrency(to) {
		return Rate{}, fmt.Errorf("%w: %q→%q is not a pair of currency codes", ErrUnsupportedCurrency, from, to)
	}
	if from == to {
		// An identity conversion must not depend on the provider being up.
		return Rate{From: from, To: to, RateE8: RateScale, AsOf: s.now().UTC()}, nil
	}

	table, cached, err := s.currentTable(ctx)
	if err != nil {
		return Rate{}, err
	}
	rateE8, err := table.cross(from, to)
	if err != nil {
		return Rate{}, err
	}
	return Rate{From: from, To: to, RateE8: rateE8, AsOf: table.AsOf, Cached: cached}, nil
}

// Convert expresses an amount in another currency.
func (s *Service) Convert(ctx context.Context, amount domain.Money, to domain.Currency) (domain.Money, Rate, error) {
	rate, err := s.Rate(ctx, amount.Currency, to)
	if err != nil {
		return domain.Money{}, Rate{}, err
	}
	converted, err := rate.Apply(amount.Amount)
	if err != nil {
		return domain.Money{}, Rate{}, err
	}
	return domain.Money{Amount: converted, Currency: rate.To}, rate, nil
}

// currentTable returns a usable table, refreshing it when stale.
//
// The order is deliberate: the in-process copy, then Redis, then the provider.
// Redis exists so that a restart, or a second replica, does not spend a provider
// request; the in-process copy exists so the hot path does no network I/O at all.
func (s *Service) currentTable(ctx context.Context) (Table, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if len(s.table.Rates) > 0 && now.Sub(s.loaded) < s.ttl {
		return s.table, true, nil
	}

	var cached Table
	if s.cache.Get(ctx, s.cacheKey(), &cached) && len(cached.Rates) > 0 {
		s.table, s.loaded = cached, now
		return cached, true, nil
	}

	fetched, err := s.provider.Rates(ctx, PivotCurrency)
	if err != nil {
		// Fall back to whatever is still in memory, even though it is past its
		// TTL, so a provider outage degrades accuracy instead of availability.
		if len(s.table.Rates) > 0 && now.Sub(s.loaded) < staleGrace {
			s.log.WarnContext(ctx, "serving stale exchange rates",
				"error", err, "age", now.Sub(s.loaded).Round(time.Second))
			return s.table, true, nil
		}
		if errors.Is(err, ErrUnsupportedCurrency) {
			return Table{}, false, err
		}
		return Table{}, false, fmt.Errorf("refresh rates: %w", err)
	}

	s.table, s.loaded = fetched, now
	s.cache.Set(ctx, s.cacheKey(), fetched)
	return fetched, false, nil
}

// cacheKey includes the provider name, so switching providers cannot serve a
// table cached from the previous one.
func (s *Service) cacheKey() string {
	return fmt.Sprintf("%s:%s", s.provider.Name(), PivotCurrency)
}
