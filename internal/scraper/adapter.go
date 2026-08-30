package scraper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

// Adapter reads a price from one shop using knowledge specific to it.
//
// Adapters exist because the generic extractor only works on shops that publish
// structured data to a plain HTTP client, and several large ones do not: they
// render the price in the browser, or answer a server address with a challenge.
// An adapter can use the shop's own JSON API instead, which is both more robust
// and far cheaper than the HTML page.
type Adapter interface {
	// Name identifies the adapter in logs and in the recorded source.
	Name() string
	// Handles reports whether this adapter understands the host.
	Handles(host string) bool
	// Observe reads the price. It receives the shared client so per-host rate
	// limiting and retries apply to adapter traffic too.
	Observe(ctx context.Context, client *Client, rawURL string, hint domain.Currency) (Observation, error)
}

// Scraper turns a URL into an observation, choosing between the shop-specific
// adapters and the generic extractor.
type Scraper struct {
	client   *Client
	adapters []Adapter
	log      *slog.Logger
}

// NewScraper builds a scraper. Adapters are tried in order, so a more specific
// one should be registered before a broader one.
func NewScraper(client *Client, log *slog.Logger, adapters ...Adapter) *Scraper {
	if log == nil {
		log = slog.Default()
	}
	return &Scraper{client: client, adapters: adapters, log: log}
}

// Observe reads price and availability for a URL.
//
// hint is the currency to assume when the page states a number without a
// currency; it is the item's own currency, so a shop that omits the code is read
// the way the user expects rather than guessed at.
func (s *Scraper) Observe(ctx context.Context, rawURL string, hint domain.Currency) (Observation, error) {
	host := strings.ToLower(domain.HostOf(rawURL))
	if host == "" {
		return Observation{}, fmt.Errorf("%w: %q has no host", ErrNoPrice, rawURL)
	}

	for _, adapter := range s.adapters {
		if !adapter.Handles(host) {
			continue
		}
		obs, err := adapter.Observe(ctx, s.client, rawURL, hint)
		if err == nil {
			if obs.Source == "" {
				obs.Source = adapter.Name()
			}
			return obs, nil
		}
		// A failing adapter falls through to the generic path: the shop may have
		// changed its API, and the HTML page is a worse but real alternative.
		// A block is not worth retrying through another route on the same host.
		if errors.Is(err, ErrBlocked) {
			return Observation{}, err
		}
		s.log.WarnContext(ctx, "adapter failed, falling back to the generic extractor",
			"adapter", adapter.Name(), "host", host, "error", err)
	}

	page, err := s.client.Fetch(ctx, rawURL)
	if err != nil {
		return Observation{}, err
	}
	obs, err := Extract(page.Body, hint)
	if err != nil {
		return Observation{}, err
	}
	return obs, nil
}
