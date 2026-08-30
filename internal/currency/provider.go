package currency

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

// DefaultProviderURL is the open.er-api.com endpoint.
//
// It is chosen over Frankfurter, the other keyless option, for one concrete
// reason: Frankfurter serves ECB reference rates, which do not include RUB, and
// the shop that actually works from a server address prices in roubles. Both
// speak nearly the same JSON, so switching is a matter of the base URL.
const DefaultProviderURL = "https://open.er-api.com/v6/latest"

// maxResponseBytes bounds the provider response. A rate table is a few kilobytes;
// anything far larger is a misrouted request or a captive portal, and reading it
// into memory would be the failure mode rather than the error.
const maxResponseBytes = 1 << 20

// HTTPProvider reads rates over HTTP.
type HTTPProvider struct {
	baseURL string
	client  *http.Client
}

// NewHTTPProvider builds a provider. An empty baseURL uses the default endpoint.
func NewHTTPProvider(baseURL string, timeout time.Duration) *HTTPProvider {
	if baseURL == "" {
		baseURL = DefaultProviderURL
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &HTTPProvider{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: timeout},
	}
}

// Name identifies the provider, and is part of the cache key so a table cached
// from one source is never served after the source is switched.
func (p *HTTPProvider) Name() string { return "open.er-api" }

// providerResponse covers the shape of both open.er-api and Frankfurter: the
// former reports "result" and a unix timestamp, the latter a date string.
type providerResponse struct {
	Result             string             `json:"result"`
	ErrorType          string             `json:"error-type"`
	Base               string             `json:"base_code"`
	FrankfurterBase    string             `json:"base"`
	TimeLastUpdateUnix int64              `json:"time_last_update_unix"`
	Date               string             `json:"date"`
	Rates              map[string]float64 `json:"rates"`
}

// Rates fetches the table for base.
func (p *HTTPProvider) Rates(ctx context.Context, base domain.Currency) (Table, error) {
	if !domain.ValidCurrency(base) {
		return Table{}, fmt.Errorf("%w: bad base %q", ErrUnsupportedCurrency, base)
	}

	url := p.baseURL + "/" + string(base)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Table{}, fmt.Errorf("build rates request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return Table{}, fmt.Errorf("fetch rates: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		// The provider answers 404 for a base it does not publish, which is a
		// permanent condition for that currency rather than a transient error.
		return Table{}, fmt.Errorf("%w: provider has no table for %s", ErrUnsupportedCurrency, base)
	}
	if resp.StatusCode != http.StatusOK {
		return Table{}, fmt.Errorf("fetch rates: provider returned %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return Table{}, fmt.Errorf("read rates: %w", err)
	}

	var parsed providerResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Table{}, fmt.Errorf("decode rates: %w", err)
	}
	if parsed.Result != "" && parsed.Result != "success" {
		return Table{}, fmt.Errorf("%w: provider said %q (%s)", ErrUnsupportedCurrency, parsed.Result, parsed.ErrorType)
	}
	if len(parsed.Rates) == 0 {
		return Table{}, fmt.Errorf("decode rates: provider returned an empty table for %s", base)
	}

	table := Table{
		Base:  base,
		Rates: make(map[domain.Currency]int64, len(parsed.Rates)),
		AsOf:  parsed.asOf(),
	}
	for code, rate := range parsed.Rates {
		currency := domain.NormalizeCurrency(code)
		if !domain.ValidCurrency(currency) || rate <= 0 {
			continue
		}
		scaled, ok := toRateE8(rate)
		if !ok {
			// A rate that cannot be represented is skipped rather than clamped:
			// a wrong rate is worse than a missing one.
			continue
		}
		table.Rates[currency] = scaled
	}
	if len(table.Rates) == 0 {
		return Table{}, fmt.Errorf("decode rates: no usable rates for %s", base)
	}
	return table, nil
}

func (r providerResponse) asOf() time.Time {
	if r.TimeLastUpdateUnix > 0 {
		return time.Unix(r.TimeLastUpdateUnix, 0).UTC()
	}
	if r.Date != "" {
		if parsed, err := time.Parse("2006-01-02", r.Date); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

// toRateE8 converts the provider's float into fixed point.
//
// The float is only ever a transport detail: it is turned into an integer at the
// edge, and every calculation downstream is exact. big.Float carries enough
// precision that the rounding happens once, here.
func toRateE8(rate float64) (int64, bool) {
	// Bounds first: an int64 scaled by 1e8 tops out around 9.2e10, and no real
	// currency pair comes anywhere near that. Rejecting here means the conversion
	// below cannot overflow.
	if rate <= 0 || rate > 1e10 {
		return 0, false
	}

	scaled := new(big.Float).SetFloat64(rate)
	scaled.Mul(scaled, new(big.Float).SetInt64(RateScale))
	scaled.Add(scaled, big.NewFloat(0.5)) // round half up

	value, _ := scaled.Int64()
	if value <= 0 {
		return 0, false
	}
	return value, true
}
