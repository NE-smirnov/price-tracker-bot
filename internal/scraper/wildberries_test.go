package scraper

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

func TestWildberriesHandles(t *testing.T) {
	for host, want := range map[string]bool{
		"www.wildberries.ru":      true,
		"wildberries.ru":          true,
		"global.wildberries.ru":   true,
		"am.wildberries.ru":       true,
		"wildberries.ru.evil.com": false,
		"ozon.ru":                 false,
	} {
		if got := (Wildberries{}).Handles(host); got != want {
			t.Fatalf("Handles(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestWildberriesProductID(t *testing.T) {
	if _, err := wbProductID("https://www.wildberries.ru/brands/apple"); !errors.Is(err, ErrNoPrice) {
		t.Fatalf("err = %v, want ErrNoPrice for a URL without an item id", err)
	}
	id, err := wbProductID("https://www.wildberries.ru/catalog/1110050605/detail.aspx?targetUrl=EX")
	if err != nil {
		t.Fatalf("wbProductID: %v", err)
	}
	if id != "1110050605" {
		t.Fatalf("id = %q, want 1110050605", id)
	}
}

// wbServer serves a canned API response and records the query it was asked.
func wbServer(t *testing.T, body string) (*httptest.Server, *string) {
	t.Helper()
	var lastQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server, &lastQuery
}

func TestWildberriesReadsTheCheapestAvailableSize(t *testing.T) {
	// Two sizes: the cheaper one is out of stock, so the price the user could
	// actually pay is the more expensive available one.
	server, query := wbServer(t, `{"products":[{"id":1,"name":"Наушники","brand":"Sony",
		"totalQuantity":5,"sizes":[
			{"stocks":[],"price":{"basic":900000,"product":700000}},
			{"stocks":[{"qty":5}],"price":{"basic":1000000,"product":850000}}]}]}`)

	obs, err := Wildberries{BaseURL: server.URL}.Observe(context.Background(),
		newTestClient(newFakeClock(), time.Millisecond, 0),
		"https://www.wildberries.ru/catalog/1/detail.aspx", "RUB")
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if obs.Price.Amount != 850000 || obs.Price.Currency != "RUB" {
		t.Fatalf("price = %+v, want 850000 RUB", obs.Price)
	}
	if !obs.InStock {
		t.Fatal("InStock = false, want true")
	}
	if obs.Title != "Sony Наушники" {
		t.Fatalf("title = %q, want the brand and the name", obs.Title)
	}
	// The delivery region is pinned: prices differ by region, and a target price
	// can only be compared against a consistently quoted one.
	if !strings.Contains(*query, "dest=-1257786") {
		t.Fatalf("query = %q, want a pinned dest", *query)
	}
}

func TestWildberriesReportsOutOfStockWithoutAPrice(t *testing.T) {
	// This is the live shape for an unavailable item: no price at all. The
	// observation must still carry the availability, because that is what the
	// "back in stock" alert is triggered on.
	server, _ := wbServer(t, `{"products":[{"id":1,"name":"Наушники","brand":"Sony",
		"totalQuantity":0,"sizes":[{"stocks":[],"price":null}]}]}`)

	obs, err := Wildberries{BaseURL: server.URL}.Observe(context.Background(),
		newTestClient(newFakeClock(), time.Millisecond, 0),
		"https://www.wildberries.ru/catalog/1/detail.aspx", "RUB")
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if obs.InStock {
		t.Fatal("InStock = true, want false")
	}
	if obs.Price.Amount != 0 {
		t.Fatalf("price = %+v, want no price", obs.Price)
	}
}

func TestWildberriesKeepsAPublishedPriceWhileOutOfStock(t *testing.T) {
	// Some listings keep a price with no stock behind it. Recording it keeps the
	// history advancing, and the return to stock stays a clean transition.
	server, _ := wbServer(t, `{"products":[{"id":1,"name":"Наушники","brand":"Sony",
		"totalQuantity":0,"sizes":[{"stocks":[],"price":{"basic":900000,"product":700000}}]}]}`)

	obs, err := Wildberries{BaseURL: server.URL}.Observe(context.Background(),
		newTestClient(newFakeClock(), time.Millisecond, 0),
		"https://www.wildberries.ru/catalog/1/detail.aspx", "RUB")
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if obs.InStock {
		t.Fatal("InStock = true, want false")
	}
	if obs.Price.Amount != 700000 {
		t.Fatalf("price = %+v, want the published 700000", obs.Price)
	}
}

func TestWildberriesReportsADelistedItem(t *testing.T) {
	// The API answers 200 with an empty list rather than 404.
	server, _ := wbServer(t, `{"products":[]}`)

	_, err := Wildberries{BaseURL: server.URL}.Observe(context.Background(),
		newTestClient(newFakeClock(), time.Millisecond, 0),
		"https://www.wildberries.ru/catalog/1/detail.aspx", "RUB")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// failingAdapter stands in for a shop API that changed shape.
type failingAdapter struct {
	err    error
	called bool
}

func (a *failingAdapter) Name() string        { return "failing" }
func (a *failingAdapter) Handles(string) bool { return true }
func (a *failingAdapter) Observe(context.Context, *Client, string, domain.Currency) (Observation, error) {
	a.called = true
	return Observation{}, a.err
}

func TestScraperFallsBackToTheGenericExtractor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `<html><head><script type="application/ld+json">
			{"@type":"Product","offers":{"price":"1234.00","priceCurrency":"RUB"}}
			</script></head></html>`)
	}))
	defer server.Close()

	adapter := &failingAdapter{err: errors.New("the shop's API changed")}
	scraper := NewScraper(newTestClient(newFakeClock(), time.Millisecond, 0),
		slog.New(slog.DiscardHandler), adapter)

	obs, err := scraper.Observe(context.Background(), server.URL, "RUB")
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !adapter.called {
		t.Fatal("the adapter was not tried")
	}
	if obs.Price.Amount != 123400 || obs.Price.Currency != "RUB" {
		t.Fatalf("price = %+v, want the generic extractor's 123400", obs.Price)
	}
}

func TestScraperDoesNotFallBackAfterABlock(t *testing.T) {
	// A block applies to the host, not to the route: retrying the HTML page after
	// the API refused only spends the shop's patience.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the generic extractor was used after a block")
	}))
	defer server.Close()

	adapter := &failingAdapter{err: ErrBlocked}
	scraper := NewScraper(newTestClient(newFakeClock(), time.Millisecond, 0),
		slog.New(slog.DiscardHandler), adapter)

	if _, err := scraper.Observe(context.Background(), server.URL, "RUB"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
}
