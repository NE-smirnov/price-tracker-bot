package scraper

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	pb "github.com/NE-smirnov/price-tracker-bot/internal/gen/pricetracker/v1"
	"github.com/NE-smirnov/price-tracker-bot/internal/notify"
)

// The stubs embed the generated client interfaces, so only the calls the pool
// actually makes have to be implemented; anything else would panic loudly rather
// than silently pass.
type stubItems struct {
	pb.ItemServiceClient
	mu      sync.Mutex
	batches [][]*pb.TrackedItem
	calls   int
}

func (s *stubItems) ClaimDueItems(context.Context, *pb.ClaimDueItemsRequest, ...grpc.CallOption) (*pb.ClaimDueItemsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if len(s.batches) == 0 {
		return &pb.ClaimDueItemsResponse{}, nil
	}
	batch := s.batches[0]
	s.batches = s.batches[1:]
	return &pb.ClaimDueItemsResponse{Items: batch}, nil
}

type stubPricing struct {
	pb.PricingServiceClient
	mu        sync.Mutex
	snapshots []*pb.RecordSnapshotRequest
	failures  []*pb.RecordFailureRequest
	alerts    []*pb.PendingAlert
	snapErr   error
}

func (s *stubPricing) RecordSnapshot(_ context.Context, in *pb.RecordSnapshotRequest, _ ...grpc.CallOption) (*pb.RecordSnapshotResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapErr != nil {
		return nil, s.snapErr
	}
	s.snapshots = append(s.snapshots, in)
	return &pb.RecordSnapshotResponse{Alerts: s.alerts}, nil
}

func (s *stubPricing) RecordFailure(_ context.Context, in *pb.RecordFailureRequest, _ ...grpc.CallOption) (*pb.RecordFailureResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = append(s.failures, in)
	return &pb.RecordFailureResponse{FailureStreak: int32(len(s.failures))}, nil
}

func (s *stubPricing) recorded() ([]*pb.RecordSnapshotRequest, []*pb.RecordFailureRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshots, s.failures
}

type stubCurrency struct {
	pb.CurrencyServiceClient
	mu    sync.Mutex
	calls []*pb.ConvertRequest
	// rateE8 converts an amount by multiplication; 2e8 doubles it.
	rateE8 int64
	err    error
}

func (s *stubCurrency) Convert(_ context.Context, in *pb.ConvertRequest, _ ...grpc.CallOption) (*pb.ConvertResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, in)
	if s.err != nil {
		return nil, s.err
	}
	return &pb.ConvertResponse{
		Converted: &pb.Money{
			Amount:   in.GetAmount().GetAmount() * s.rateE8 / 100_000_000,
			Currency: in.GetToCurrency(),
		},
	}, nil
}

type stubAlerts struct {
	mu      sync.Mutex
	pushed  []notify.Alert
	pushErr error
}

func (s *stubAlerts) Push(_ context.Context, payload any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pushErr != nil {
		return s.pushErr
	}
	alert, ok := payload.(notify.Alert)
	if !ok {
		return errors.New("unexpected payload type")
	}
	s.pushed = append(s.pushed, alert)
	return nil
}

func (s *stubAlerts) all() []notify.Alert {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]notify.Alert(nil), s.pushed...)
}

// ldJSONServer serves a page whose structured data states a price.
func ldJSONServer(t *testing.T, price, currency string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `<html><head><script type="application/ld+json">{"@type":"Product",
			"name":"Наушники","offers":{"price":"`+price+`","priceCurrency":"`+currency+`",
			"availability":"https://schema.org/InStock"}}</script></head></html>`)
	}))
	t.Cleanup(server.Close)
	return server
}

func newTestPool(items *stubItems, pricing *stubPricing, currency *stubCurrency, alerts *stubAlerts) *Pool {
	scraper := NewScraper(newTestClient(newFakeClock(), time.Millisecond, 0), slog.New(slog.DiscardHandler))
	opts := PoolOptions{
		Items:   items,
		Pricing: pricing,
		Scraper: scraper,
		Alerts:  alerts,
		Log:     slog.New(slog.DiscardHandler),
		Workers: 2,
	}
	if currency != nil {
		opts.Currency = currency
	}
	return NewPool(opts)
}

func TestPoolRecordsAScrapedPrice(t *testing.T) {
	server := ldJSONServer(t, "1499.00", "RUB")
	items := &stubItems{}
	pricing := &stubPricing{}
	pool := newTestPool(items, pricing, nil, &stubAlerts{})

	pool.process(context.Background(), &pb.TrackedItem{Id: "item-1", Url: server.URL})

	snapshots, failures := pricing.recorded()
	if len(snapshots) != 1 || len(failures) != 0 {
		t.Fatalf("recorded %d snapshots and %d failures, want 1 and 0", len(snapshots), len(failures))
	}
	got := snapshots[0]
	if got.GetPrice().GetAmount() != 149900 || got.GetPrice().GetCurrency() != "RUB" {
		t.Fatalf("price = %+v, want 149900 RUB", got.GetPrice())
	}
	if !got.GetInStock() {
		t.Fatal("in_stock = false, want true")
	}
	if got.GetObservedTitle() != "Наушники" {
		t.Fatalf("observed_title = %q, want the page title", got.GetObservedTitle())
	}
	if got.ConvertedPrice != nil {
		t.Fatalf("converted_price = %+v, want none without a target currency", got.GetConvertedPrice())
	}
}

func TestPoolConvertsIntoTheTargetCurrency(t *testing.T) {
	server := ldJSONServer(t, "20.00", "USD")
	pricing := &stubPricing{}
	currency := &stubCurrency{rateE8: 8_500_000_000} // 1 USD = 85 RUB
	pool := newTestPool(&stubItems{}, pricing, currency, &stubAlerts{})

	pool.process(context.Background(), &pb.TrackedItem{
		Id:          "item-1",
		Url:         server.URL,
		TargetPrice: &pb.Money{Amount: 200000, Currency: "RUB"},
	})

	snapshots, _ := pricing.recorded()
	if len(snapshots) != 1 {
		t.Fatalf("recorded %d snapshots, want 1", len(snapshots))
	}
	// The shop price is stored as shown, and the comparable amount alongside it.
	if got := snapshots[0].GetPrice(); got.GetAmount() != 2000 || got.GetCurrency() != "USD" {
		t.Fatalf("price = %+v, want the shop's 2000 USD", got)
	}
	if got := snapshots[0].GetConvertedPrice(); got.GetAmount() != 170000 || got.GetCurrency() != "RUB" {
		t.Fatalf("converted_price = %+v, want 170000 RUB", got)
	}
}

func TestPoolSkipsConversionWithinOneCurrency(t *testing.T) {
	server := ldJSONServer(t, "1499.00", "RUB")
	currency := &stubCurrency{rateE8: 100_000_000}
	pool := newTestPool(&stubItems{}, &stubPricing{}, currency, &stubAlerts{})

	pool.process(context.Background(), &pb.TrackedItem{
		Id:          "item-1",
		Url:         server.URL,
		TargetPrice: &pb.Money{Amount: 150000, Currency: "RUB"},
	})

	currency.mu.Lock()
	defer currency.mu.Unlock()
	if len(currency.calls) != 0 {
		t.Fatalf("made %d conversion calls, want none for a same-currency price", len(currency.calls))
	}
}

func TestPoolStoresThePriceWhenConversionFails(t *testing.T) {
	// A rate outage must not lose the observation: the shop price is real, and
	// core simply postpones the target comparison.
	server := ldJSONServer(t, "20.00", "USD")
	pricing := &stubPricing{}
	currency := &stubCurrency{err: errors.New("rate provider is down")}
	pool := newTestPool(&stubItems{}, pricing, currency, &stubAlerts{})

	pool.process(context.Background(), &pb.TrackedItem{
		Id:          "item-1",
		Url:         server.URL,
		TargetPrice: &pb.Money{Amount: 200000, Currency: "RUB"},
	})

	snapshots, failures := pricing.recorded()
	if len(snapshots) != 1 || len(failures) != 0 {
		t.Fatalf("recorded %d snapshots and %d failures, want the snapshot kept", len(snapshots), len(failures))
	}
	if snapshots[0].ConvertedPrice != nil {
		t.Fatalf("converted_price = %+v, want none", snapshots[0].GetConvertedPrice())
	}
}

func TestPoolRecordsAFailureWithAStableReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	pricing := &stubPricing{}
	pool := newTestPool(&stubItems{}, pricing, nil, &stubAlerts{})

	pool.process(context.Background(), &pb.TrackedItem{Id: "item-1", Url: server.URL})

	snapshots, failures := pricing.recorded()
	if len(snapshots) != 0 || len(failures) != 1 {
		t.Fatalf("recorded %d snapshots and %d failures, want 0 and 1", len(snapshots), len(failures))
	}
	// The reason must be stable, so a streak of one cause is recognisable in the
	// database rather than differing on every attempt.
	if failures[0].GetReason() != "blocked by the shop" {
		t.Fatalf("reason = %q, want a stable classification", failures[0].GetReason())
	}
}

func TestPoolForwardsAlerts(t *testing.T) {
	server := ldJSONServer(t, "1499.00", "RUB")
	alerts := &stubAlerts{}
	pricing := &stubPricing{alerts: []*pb.PendingAlert{{
		Kind:          pb.AlertKind_ALERT_KIND_PRICE_DROP,
		TrackedItemId: "item-1",
		TelegramId:    42,
		ItemTitle:     "Наушники",
		Price:         &pb.Money{Amount: 149900, Currency: "RUB"},
		DedupKey:      "item-1:drop:149900",
	}}}
	pool := newTestPool(&stubItems{}, pricing, nil, alerts)

	pool.process(context.Background(), &pb.TrackedItem{Id: "item-1", Url: server.URL})

	pushed := alerts.all()
	if len(pushed) != 1 {
		t.Fatalf("pushed %d alerts, want 1", len(pushed))
	}
	if pushed[0].Kind != notify.KindPriceDrop || pushed[0].DedupKey != "item-1:drop:149900" {
		t.Fatalf("pushed = %+v, want the alert core raised", pushed[0])
	}
}

func TestPoolRunDrainsABatchAndStops(t *testing.T) {
	server := ldJSONServer(t, "1499.00", "RUB")
	items := &stubItems{batches: [][]*pb.TrackedItem{{
		{Id: "item-1", Url: server.URL},
		{Id: "item-2", Url: server.URL},
		{Id: "item-3", Url: server.URL},
	}}}
	pricing := &stubPricing{}
	pool := newTestPool(items, pricing, nil, &stubAlerts{})
	pool.opts.IdlePause = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := pool.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	snapshots, _ := pricing.recorded()
	if len(snapshots) != 3 {
		t.Fatalf("recorded %d snapshots, want every item in the batch", len(snapshots))
	}
}

func TestPoolKeepsRunningWhenClaimFails(t *testing.T) {
	// Core being unavailable is temporary: the items are still due, and the loop
	// must not exit.
	items := &brokenItems{}
	pool := newTestPool(&stubItems{}, &stubPricing{}, nil, &stubAlerts{})
	pool.opts.Items = items
	pool.opts.IdlePause = time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := pool.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if items.count() < 2 {
		t.Fatalf("claimed %d times, want the loop to keep retrying", items.count())
	}
}

type brokenItems struct {
	pb.ItemServiceClient
	mu    sync.Mutex
	calls int
}

func (b *brokenItems) ClaimDueItems(context.Context, *pb.ClaimDueItemsRequest, ...grpc.CallOption) (*pb.ClaimDueItemsResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	return nil, errors.New("core is unavailable")
}

func (b *brokenItems) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}
