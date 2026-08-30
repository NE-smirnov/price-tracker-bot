package currency

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

// stubProvider counts calls, which is how the caching claims are checked.
type stubProvider struct {
	mu    sync.Mutex
	calls int
	table Table
	err   error
}

func (p *stubProvider) Name() string { return "stub" }

func (p *stubProvider) Rates(context.Context, domain.Currency) (Table, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.err != nil {
		return Table{}, p.err
	}
	return p.table, nil
}

func (p *stubProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func usdTable() Table {
	return Table{
		Base: "USD",
		Rates: map[domain.Currency]int64{
			"RUB": 85_90946900, // 85.909469
			"TRY": 48_24889500,
			"EUR": 86_154200,
			"JPY": 147_00000000,
			"KWD": 30_600000,
		},
		AsOf: time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC),
	}
}

func newTestService(t *testing.T, provider Provider) *Service {
	t.Helper()
	return NewService(Options{Provider: provider, TTL: time.Hour})
}

func TestConvertSameCurrencyNeedsNoProvider(t *testing.T) {
	// An identity conversion must not depend on an external service being up:
	// most items are already priced in the currency the user asked for.
	provider := &stubProvider{err: errors.New("provider must not be called")}
	svc := newTestService(t, provider)

	got, rate, err := svc.Convert(context.Background(),
		domain.Money{Amount: 129900, Currency: "TRY"}, "TRY")
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if got.Amount != 129900 || got.Currency != "TRY" {
		t.Fatalf("converted = %+v, want the input unchanged", got)
	}
	if rate.RateE8 != RateScale {
		t.Fatalf("rate = %d, want the identity %d", rate.RateE8, RateScale)
	}
	if provider.callCount() != 0 {
		t.Fatal("provider was called for an identity conversion")
	}
}

func TestConvertUsesCrossRates(t *testing.T) {
	provider := &stubProvider{table: usdTable()}
	svc := newTestService(t, provider)
	ctx := context.Background()

	tests := []struct {
		name   string
		amount domain.Money
		to     domain.Currency
		want   int64
	}{
		// 100.00 USD at 85.909469 RUB/USD = 8590.95 RUB.
		{"usd to rub", domain.Money{Amount: 10000, Currency: "USD"}, "RUB", 859095},
		// 8590.95 RUB back to USD returns the original, give or take rounding.
		{"rub to usd", domain.Money{Amount: 859095, Currency: "RUB"}, "USD", 10000},
		// A cross rate through the pivot: 1000.00 RUB is 561.62 lira
		// (48.248895 / 85.909469 = 0.5616235 lira per rouble).
		{"rub to try", domain.Money{Amount: 100000, Currency: "RUB"}, "TRY", 56162},
		// Into a zero-decimal currency: 10.00 USD is 1470 yen, not 147000.
		{"usd to jpy", domain.Money{Amount: 1000, Currency: "USD"}, "JPY", 1470},
		// Out of a zero-decimal currency: 1470 yen back to dollars.
		{"jpy to usd", domain.Money{Amount: 1470, Currency: "JPY"}, "USD", 1000},
		// Into a three-decimal currency: 100.00 USD is 30.600 dinars.
		{"usd to kwd", domain.Money{Amount: 10000, Currency: "USD"}, "KWD", 30600},
		// And back out of it.
		{"kwd to usd", domain.Money{Amount: 30600, Currency: "KWD"}, "USD", 10000},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := svc.Convert(ctx, tc.amount, tc.to)
			if err != nil {
				t.Fatalf("Convert: %v", err)
			}
			if got.Amount != tc.want {
				t.Fatalf("amount = %d, want %d", got.Amount, tc.want)
			}
			if got.Currency != tc.to {
				t.Fatalf("currency = %s, want %s", got.Currency, tc.to)
			}
		})
	}
}

func TestConvertFetchesTheTableOnce(t *testing.T) {
	provider := &stubProvider{table: usdTable()}
	svc := newTestService(t, provider)
	ctx := context.Background()

	var wg sync.WaitGroup
	var failures atomic.Int64
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := svc.Convert(ctx, domain.Money{Amount: 1000, Currency: "USD"}, "RUB"); err != nil {
				failures.Add(1)
			}
		}()
	}
	wg.Wait()

	if failures.Load() != 0 {
		t.Fatalf("%d concurrent conversions failed", failures.Load())
	}
	// Twenty concurrent conversions must not become twenty provider requests:
	// the scrape pool converts every observation and would exhaust any free quota.
	if calls := provider.callCount(); calls != 1 {
		t.Fatalf("provider calls = %d, want 1", calls)
	}
}

func TestConvertRefreshesAfterTTL(t *testing.T) {
	provider := &stubProvider{table: usdTable()}
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	svc := NewService(Options{Provider: provider, TTL: time.Hour, Now: func() time.Time { return now }})
	ctx := context.Background()

	if _, _, err := svc.Convert(ctx, domain.Money{Amount: 1000, Currency: "USD"}, "RUB"); err != nil {
		t.Fatalf("first: %v", err)
	}
	now = now.Add(90 * time.Minute)
	if _, _, err := svc.Convert(ctx, domain.Money{Amount: 1000, Currency: "USD"}, "RUB"); err != nil {
		t.Fatalf("after ttl: %v", err)
	}
	if calls := provider.callCount(); calls != 2 {
		t.Fatalf("provider calls = %d, want 2 after the TTL expired", calls)
	}
}

func TestConvertServesStaleRatesWhenTheProviderIsDown(t *testing.T) {
	// A provider outage must not stop conversions: without a converted price the
	// core refuses to compare a foreign-currency price against the user's target,
	// which silently disables the alert the user is waiting for.
	provider := &stubProvider{table: usdTable()}
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	svc := NewService(Options{Provider: provider, TTL: time.Hour, Now: func() time.Time { return now }})
	ctx := context.Background()

	if _, _, err := svc.Convert(ctx, domain.Money{Amount: 1000, Currency: "USD"}, "RUB"); err != nil {
		t.Fatalf("warm up: %v", err)
	}

	provider.mu.Lock()
	provider.err = errors.New("provider is down")
	provider.mu.Unlock()

	now = now.Add(2 * time.Hour)
	got, rate, err := svc.Convert(ctx, domain.Money{Amount: 1000, Currency: "USD"}, "RUB")
	if err != nil {
		t.Fatalf("Convert with a dead provider: %v", err)
	}
	// 10.00 USD at the stale 85.909469 rate is 859.09 RUB.
	if got.Amount != 85909 {
		t.Fatalf("amount = %d, want the stale-rate result 85909", got.Amount)
	}
	if !rate.Cached {
		t.Fatal("rate.Cached = false, want true so a caller can see the rate is not fresh")
	}

	// Past the grace window the answer becomes an error rather than a rate from
	// another era.
	now = now.Add(staleGrace)
	if _, _, err := svc.Convert(ctx, domain.Money{Amount: 1000, Currency: "USD"}, "RUB"); err == nil {
		t.Fatal("expected an error once the stale grace period elapsed")
	}
}

func TestConvertRejectsUnknownCurrencies(t *testing.T) {
	svc := newTestService(t, &stubProvider{table: usdTable()})
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		amount domain.Money
		to     domain.Currency
	}{
		{"unknown target", domain.Money{Amount: 1000, Currency: "USD"}, "XYZ"},
		{"unknown source", domain.Money{Amount: 1000, Currency: "XYZ"}, "USD"},
		{"not a code", domain.Money{Amount: 1000, Currency: "USD"}, "dollars"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := svc.Convert(ctx, tc.amount, tc.to); !errors.Is(err, ErrUnsupportedCurrency) {
				t.Fatalf("err = %v, want ErrUnsupportedCurrency", err)
			}
		})
	}
}

func TestConvertRejectsAmountsThatRoundToNothing(t *testing.T) {
	// One kopeck is worth less than the smallest representable dinar fraction.
	// Returning zero would be a price of nothing, so this must be an error.
	svc := newTestService(t, &stubProvider{table: Table{
		Base:  "USD",
		Rates: map[domain.Currency]int64{"RUB": 85_90946900, "KWD": 30_600000},
	}})

	if _, _, err := svc.Convert(context.Background(),
		domain.Money{Amount: 1, Currency: "RUB"}, "KWD"); err == nil {
		t.Fatal("expected an error for an amount that rounds to zero")
	}
}

func TestHTTPProviderParsesTheProviderShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/USD" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"success","base_code":"USD",
			"time_last_update_unix":1787000000,
			"rates":{"USD":1,"RUB":85.909469,"TRY":48.248895,"BAD":-1,"toolong":2}}`))
	}))
	defer server.Close()

	provider := NewHTTPProvider(server.URL, 5*time.Second)
	table, err := provider.Rates(context.Background(), "USD")
	if err != nil {
		t.Fatalf("Rates: %v", err)
	}
	if table.Rates["RUB"] != 85_90946900 {
		t.Fatalf("RUB = %d, want 8590946900", table.Rates["RUB"])
	}
	if _, ok := table.Rates["BAD"]; ok {
		t.Fatal("a negative rate was accepted")
	}
	if _, ok := table.Rates["TOOLONG"]; ok {
		t.Fatal("a non-ISO code was accepted")
	}
	if table.AsOf.IsZero() {
		t.Fatal("AsOf is zero, want the provider's timestamp")
	}
}

func TestHTTPProviderReportsAnUnknownBaseAsPermanent(t *testing.T) {
	// The provider answers 404 for a base it does not publish. Treating that as a
	// transient failure would retry forever.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"result":"error","error-type":"unsupported-code"}`, http.StatusNotFound)
	}))
	defer server.Close()

	if _, err := NewHTTPProvider(server.URL, 5*time.Second).Rates(context.Background(), "XYZ"); !errors.Is(err, ErrUnsupportedCurrency) {
		t.Fatalf("err = %v, want ErrUnsupportedCurrency", err)
	}
}
