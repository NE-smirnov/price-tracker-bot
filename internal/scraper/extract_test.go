package scraper

import (
	"errors"
	"testing"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

func TestExtractJSONLD(t *testing.T) {
	page := []byte(`<!doctype html><html><head>
<title>Наушники XM5 — Магазин</title>
<script type="application/ld+json">
{"@context":"https://schema.org","@type":"Product","name":"Sony WH-1000XM5",
 "offers":{"@type":"Offer","price":"24990.00","priceCurrency":"RUB",
 "availability":"https://schema.org/InStock"}}
</script>
</head><body></body></html>`)

	obs, err := Extract(page, "")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if obs.Price.Amount != 2499000 || obs.Price.Currency != "RUB" {
		t.Fatalf("price = %+v, want 2499000 RUB", obs.Price)
	}
	if !obs.InStock {
		t.Fatal("InStock = false, want true")
	}
	if obs.Title != "Sony WH-1000XM5" {
		t.Fatalf("title = %q, want the product name from JSON-LD", obs.Title)
	}
	if obs.Source != "json-ld" {
		t.Fatalf("source = %q, want json-ld", obs.Source)
	}
}

func TestExtractJSONLDShapes(t *testing.T) {
	// The same data reaches the page as an object, an array, a @graph, an
	// AggregateOffer and a numeric price. All of these are real shop output.
	tests := []struct {
		name string
		body string
		want int64
	}{
		{
			name: "array at the root",
			body: `<script type="application/ld+json">
[{"@type":"BreadcrumbList"},{"@type":"Product","offers":{"price":"19.99","priceCurrency":"USD"}}]
</script>`,
			want: 1999,
		},
		{
			name: "graph container",
			body: `<script type="application/ld+json">
{"@graph":[{"@type":"WebPage"},{"@type":"Product","offers":{"price":"19.99","priceCurrency":"USD"}}]}
</script>`,
			want: 1999,
		},
		{
			name: "aggregate offer",
			body: `<script type="application/ld+json">
{"@type":"Product","offers":{"@type":"AggregateOffer","priceCurrency":"USD",
 "offers":[{"@type":"Offer","price":"24.50","priceCurrency":"USD"}]}}
</script>`,
			want: 2450,
		},
		{
			name: "low price only",
			body: `<script type="application/ld+json">
{"@type":"Product","offers":{"@type":"AggregateOffer","lowPrice":"15.00","highPrice":"30.00","priceCurrency":"USD"}}
</script>`,
			want: 1500,
		},
		{
			name: "numeric price",
			body: `<script type="application/ld+json">
{"@type":"Product","offers":{"price":19.99,"priceCurrency":"USD"}}
</script>`,
			want: 1999,
		},
		{
			name: "numeric integer price",
			body: `<script type="application/ld+json">
{"@type":"Product","offers":{"price":1299,"priceCurrency":"TRY"}}
</script>`,
			want: 129900,
		},
		{
			// A number with three decimals is unambiguous in JSON: it must not be
			// read as thousands grouping.
			name: "numeric price with extra precision",
			body: `<script type="application/ld+json">
{"@type":"Product","offers":{"price":10.126,"priceCurrency":"USD"}}
</script>`,
			want: 1013,
		},
		{
			// Non-compliant, but shops do it: display format inside JSON-LD.
			name: "display format inside json-ld",
			body: `<script type="application/ld+json">
{"@type":"Product","offers":{"price":"1.299,90","priceCurrency":"TRY"}}
</script>`,
			want: 129990,
		},
		{
			name: "broken json-ld is skipped for the next block",
			body: `<script type="application/ld+json">{ this is not json </script>
<script type="application/ld+json">{"@type":"Product","offers":{"price":"5.00","priceCurrency":"USD"}}</script>`,
			want: 500,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			obs, err := Extract([]byte("<html><head>"+tc.body+"</head></html>"), "")
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if obs.Price.Amount != tc.want {
				t.Fatalf("amount = %d, want %d", obs.Price.Amount, tc.want)
			}
		})
	}
}

func TestExtractMicrodata(t *testing.T) {
	page := []byte(`<html><head><title>Товар</title></head><body>
<div itemscope itemtype="https://schema.org/Product">
  <span itemprop="name">Монитор 27"</span>
  <div itemprop="offers" itemscope itemtype="https://schema.org/Offer">
    <meta itemprop="price" content="18990.50">
    <meta itemprop="priceCurrency" content="RUB">
    <link itemprop="availability" href="https://schema.org/OutOfStock">
  </div>
</div></body></html>`)

	obs, err := Extract(page, "")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if obs.Price.Amount != 1899050 || obs.Price.Currency != "RUB" {
		t.Fatalf("price = %+v, want 1899050 RUB", obs.Price)
	}
	if obs.InStock {
		t.Fatal("InStock = true, want false for OutOfStock")
	}
	if obs.Source != "microdata" {
		t.Fatalf("source = %q, want microdata", obs.Source)
	}
}

func TestExtractOpenGraph(t *testing.T) {
	page := []byte(`<html><head>
<title>Ignored when og:title exists</title>
<meta property="og:title" content="Клавиатура K2">
<meta property="product:price:amount" content="1299.00">
<meta property="product:price:currency" content="TRY">
<meta property="product:availability" content="in stock">
</head></html>`)

	obs, err := Extract(page, "")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if obs.Price.Amount != 129900 || obs.Price.Currency != "TRY" {
		t.Fatalf("price = %+v, want 129900 TRY", obs.Price)
	}
	if obs.Title != "Клавиатура K2" {
		t.Fatalf("title = %q", obs.Title)
	}
	if obs.Source != "opengraph" {
		t.Fatalf("source = %q, want opengraph", obs.Source)
	}
}

func TestExtractPrefersStructuredDataOrder(t *testing.T) {
	// All three sources present with different numbers: JSON-LD must win, because
	// it is the one shops keep correct for search engines.
	page := []byte(`<html><head>
<script type="application/ld+json">{"@type":"Product","offers":{"price":"100.00","priceCurrency":"USD"}}</script>
<meta property="product:price:amount" content="300.00">
<meta property="product:price:currency" content="USD">
</head><body><meta itemprop="price" content="200.00"><meta itemprop="priceCurrency" content="USD"></body></html>`)

	obs, err := Extract(page, "")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if obs.Price.Amount != 10000 {
		t.Fatalf("amount = %d, want the JSON-LD value 10000", obs.Price.Amount)
	}
}

func TestExtractCurrencyHint(t *testing.T) {
	// A page that states a price but no currency is only usable with a hint from
	// the caller; without one the observation must be refused rather than
	// guessed, because a wrong currency moves the alert threshold.
	page := []byte(`<html><head>
<script type="application/ld+json">{"@type":"Product","offers":{"price":"1299.00"}}</script>
</head></html>`)

	if _, err := Extract(page, ""); !errors.Is(err, ErrNoPrice) {
		t.Fatalf("err = %v, want ErrNoPrice without a currency", err)
	}

	obs, err := Extract(page, domain.Currency("TRY"))
	if err != nil {
		t.Fatalf("Extract with hint: %v", err)
	}
	if obs.Price.Currency != "TRY" || obs.Price.Amount != 129900 {
		t.Fatalf("price = %+v, want 129900 TRY", obs.Price)
	}
}

func TestExtractDetectsAntiBotPages(t *testing.T) {
	// These pages return HTTP 200 with no price. Reporting them as ErrNoPrice
	// would blame the shop's markup for what is actually a block, and the two
	// need different handling.
	pages := map[string]string{
		"amazon": `<html><head><title>Amazon.com</title></head><body>
			<h4>Enter the characters you see below</h4>
			<p>Sorry, we just need to make sure you're not a robot.</p></body></html>`,
		"yandex": `<html><head><title>Ой!</title></head><body>
			<p>Подтвердите, что запросы отправляли вы, а не робот</p></body></html>`,
		"ozon": `<html><body><h1>Доступ ограничен</h1></body></html>`,
		"cloudflare": `<html><body><div id="cf-browser-verification">
			Checking your browser before accessing the site</div></body></html>`,
	}

	for name, body := range pages {
		t.Run(name, func(t *testing.T) {
			if _, err := Extract([]byte(body), "USD"); !errors.Is(err, ErrBlocked) {
				t.Fatalf("err = %v, want ErrBlocked", err)
			}
		})
	}
}

func TestExtractNoPrice(t *testing.T) {
	pages := map[string]string{
		"empty page":            `<html><body></body></html>`,
		"article without offer": `<script type="application/ld+json">{"@type":"Article","name":"Обзор"}</script>`,
		"offer without price":   `<script type="application/ld+json">{"@type":"Product","offers":{"priceCurrency":"USD"}}</script>`,
		"zero price":            `<script type="application/ld+json">{"@type":"Product","offers":{"price":"0.00","priceCurrency":"USD"}}</script>`,
	}

	for name, body := range pages {
		t.Run(name, func(t *testing.T) {
			if _, err := Extract([]byte("<html><head>"+body+"</head></html>"), "USD"); !errors.Is(err, ErrNoPrice) {
				t.Fatalf("err = %v, want ErrNoPrice", err)
			}
		})
	}
}

func TestAvailabilityInStock(t *testing.T) {
	for raw, want := range map[string]bool{
		"":                              true, // a page with a price and no marker is treated as available
		"https://schema.org/InStock":    true,
		"http://schema.org/InStock":     true,
		"InStock":                       true,
		"in stock":                      true,
		"PreOrder":                      true,
		"BackOrder":                     true,
		"https://schema.org/OutOfStock": false,
		"OutOfStock":                    false,
		"out of stock":                  false,
		"SoldOut":                       false,
		"Discontinued":                  false,
	} {
		if got := availabilityInStock(raw); got != want {
			t.Fatalf("availabilityInStock(%q) = %v, want %v", raw, got, want)
		}
	}
}
