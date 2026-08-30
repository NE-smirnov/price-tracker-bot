package scraper

import (
	"encoding/json"
	"fmt"
	"strings"

	"golang.org/x/net/html"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

// Observation is what a scrape of one product page yields.
type Observation struct {
	Price domain.Money
	// InStock is availability as stated by the page. When the page says nothing,
	// a listed price is taken as available: shops that hide unavailable items
	// usually remove the price too.
	InStock bool
	// Title as printed on the page, used to fill in an item the user added
	// without naming it.
	Title string
	// Source records which strategy produced the price, so a systematic misread
	// can be traced to one extractor instead of guessed at.
	Source string
}

// Extract reads a price out of an arbitrary product page.
//
// Strategies are ordered by how much the shop committed to them. Structured data
// is published for search engines and therefore kept correct; visible markup is
// not used at all here, because class names change with every redesign and a
// wrong number is worse than no number.
func Extract(body []byte, hint domain.Currency) (Observation, error) {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return Observation{}, fmt.Errorf("parse html: %w", err)
	}

	page := scanPage(doc)
	if page.blocked {
		return Observation{}, ErrBlocked
	}

	for _, strategy := range []func(pageData, domain.Currency) (Observation, bool){
		fromJSONLD,
		fromMicrodata,
		fromOpenGraph,
	} {
		if obs, ok := strategy(page, hint); ok {
			if obs.Title == "" {
				obs.Title = page.title
			}
			obs.Title = strings.TrimSpace(obs.Title)
			return obs, nil
		}
	}
	return Observation{}, ErrNoPrice
}

// ---------------------------------------------------------------- page scan

// pageData is everything the extractors need, collected in a single DOM walk.
type pageData struct {
	title    string
	jsonLD   []string
	meta     map[string]string
	itemprop map[string]string
	blocked  bool
}

// challengeMarkers are phrases that appear on anti-bot pages. They matter because
// such a page is a valid HTTP 200 with no price on it, which would otherwise be
// misreported as "the shop changed its markup".
var challengeMarkers = []string{
	"enter the characters you see below",
	"to discuss automated access",
	"подтвердите, что запросы отправляли вы",
	"доступ ограничен",
	"проверка браузера",
	"checking your browser",
	"smartcaptcha",
	"showcaptcha",
	"cf-browser-verification",
	"are you a robot",
}

func scanPage(root *html.Node) pageData {
	page := pageData{
		meta:     map[string]string{},
		itemprop: map[string]string{},
	}

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "title":
				if page.title == "" {
					page.title = textOf(n)
				}
			case "script":
				if strings.Contains(strings.ToLower(attr(n, "type")), "ld+json") {
					page.jsonLD = append(page.jsonLD, textOf(n))
				}
			case "meta":
				key := attr(n, "property")
				if key == "" {
					key = attr(n, "name")
				}
				if key != "" {
					if _, seen := page.meta[strings.ToLower(key)]; !seen {
						page.meta[strings.ToLower(key)] = attr(n, "content")
					}
				}
			}

			// Microdata can sit on any element, including the meta handled above.
			if prop := strings.ToLower(attr(n, "itemprop")); prop != "" {
				value := attr(n, "content")
				if value == "" {
					value = attr(n, "href")
				}
				if value == "" {
					value = textOf(n)
				}
				if _, seen := page.itemprop[prop]; !seen && strings.TrimSpace(value) != "" {
					page.itemprop[prop] = strings.TrimSpace(value)
				}
			}
		}

		if n.Type == html.TextNode && !page.blocked {
			lower := strings.ToLower(n.Data)
			for _, marker := range challengeMarkers {
				if strings.Contains(lower, marker) {
					page.blocked = true
					break
				}
			}
		}

		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)

	return page
}

func attr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, name) {
			return a.Val
		}
	}
	return ""
}

func textOf(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			b.WriteString(node.Data)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(n)
	return strings.TrimSpace(b.String())
}

// ---------------------------------------------------------------- strategies

// fromJSONLD reads schema.org Product data. This is the most reliable source:
// shops maintain it for Google Shopping, so it breaks last.
func fromJSONLD(page pageData, hint domain.Currency) (Observation, bool) {
	for _, blob := range page.jsonLD {
		var parsed any
		if err := json.Unmarshal([]byte(blob), &parsed); err != nil {
			// Malformed JSON-LD is common; it is skipped rather than fatal.
			continue
		}
		for _, node := range flattenJSONLD(parsed) {
			if obs, ok := productFromNode(node, hint); ok {
				obs.Source = "json-ld"
				return obs, true
			}
		}
	}
	return Observation{}, false
}

// flattenJSONLD expands the shapes shops use: a single object, an array, and
// "@graph" containers, at any nesting depth.
func flattenJSONLD(node any) []map[string]any {
	switch typed := node.(type) {
	case map[string]any:
		out := []map[string]any{typed}
		for _, key := range []string{"@graph", "mainEntity", "itemListElement"} {
			if nested, ok := typed[key]; ok {
				out = append(out, flattenJSONLD(nested)...)
			}
		}
		return out
	case []any:
		var out []map[string]any
		for _, item := range typed {
			out = append(out, flattenJSONLD(item)...)
		}
		return out
	default:
		return nil
	}
}

func productFromNode(node map[string]any, hint domain.Currency) (Observation, bool) {
	offers := firstOffer(node["offers"])
	if offers == nil {
		return Observation{}, false
	}

	priceRaw, numeric, ok := priceString(offers["price"])
	if !ok {
		// Some shops only publish a range; the low end is the price a user cares
		// about when watching for a drop.
		if priceRaw, numeric, ok = priceString(offers["lowPrice"]); !ok {
			return Observation{}, false
		}
	}

	currency := hint
	if code, _, ok := priceString(offers["priceCurrency"]); ok {
		if detected, found := DetectCurrency(code); found {
			currency = detected
		}
	}

	money, err := structuredMoney(priceRaw, currency, numeric)
	if err != nil {
		return Observation{}, false
	}

	availability, _, _ := priceString(offers["availability"])
	name, _, _ := priceString(node["name"])

	return Observation{
		Price:   money,
		InStock: availabilityInStock(availability),
		Title:   name,
	}, true
}

// firstOffer normalises the offers field, which shops write as an object, an
// array, or an AggregateOffer wrapping more offers.
func firstOffer(value any) map[string]any {
	switch typed := value.(type) {
	case map[string]any:
		if nested, ok := typed["offers"]; ok {
			if inner := firstOffer(nested); inner != nil {
				return inner
			}
		}
		return typed
	case []any:
		for _, item := range typed {
			if offer := firstOffer(item); offer != nil {
				return offer
			}
		}
	}
	return nil
}

// fromMicrodata reads itemprop attributes, the older structured-data format.
func fromMicrodata(page pageData, hint domain.Currency) (Observation, bool) {
	priceRaw, ok := page.itemprop["price"]
	if !ok {
		if priceRaw, ok = page.itemprop["lowprice"]; !ok {
			return Observation{}, false
		}
	}

	currency := hint
	if code, ok := page.itemprop["pricecurrency"]; ok {
		if detected, found := DetectCurrency(code); found {
			currency = detected
		}
	}

	money, err := structuredMoney(priceRaw, currency, false)
	if err != nil {
		return Observation{}, false
	}

	return Observation{
		Price:   money,
		InStock: availabilityInStock(page.itemprop["availability"]),
		Title:   page.itemprop["name"],
		Source:  "microdata",
	}, true
}

// fromOpenGraph reads the og:/product: meta tags. Least detailed of the three,
// but present on many shops that publish nothing else.
func fromOpenGraph(page pageData, hint domain.Currency) (Observation, bool) {
	priceRaw := firstNonEmpty(page.meta,
		"product:price:amount", "og:price:amount", "twitter:data1")
	if priceRaw == "" {
		return Observation{}, false
	}

	currency := hint
	if code := firstNonEmpty(page.meta,
		"product:price:currency", "og:price:currency", "twitter:label1"); code != "" {
		if detected, found := DetectCurrency(code); found {
			currency = detected
		}
	}

	money, err := structuredMoney(priceRaw, currency, false)
	if err != nil {
		return Observation{}, false
	}

	availability := firstNonEmpty(page.meta, "product:availability", "og:availability")

	return Observation{
		Price:   money,
		InStock: availabilityInStock(availability),
		Title:   firstNonEmpty(page.meta, "og:title", "twitter:title"),
		Source:  "opengraph",
	}, true
}

func firstNonEmpty(m map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(m[key]); value != "" {
			return value
		}
	}
	return ""
}

// availabilityInStock interprets a schema.org availability value.
//
// The default for an unknown or absent value is true: the page did show a price,
// and treating that as unavailable would suppress the alerts the user asked for.
// Only explicit negatives turn it off.
func availabilityInStock(raw string) bool {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" {
		return true
	}
	// The value is often a full URL, e.g. "https://schema.org/InStock".
	if idx := strings.LastIndexAny(value, "/#"); idx >= 0 {
		value = value[idx+1:]
	}
	value = strings.ReplaceAll(value, " ", "")

	switch value {
	case "outofstock", "soldout", "discontinued", "outofservice", "нетвналичии":
		return false
	default:
		return true
	}
}

// structuredMoney parses a price taken from structured data.
//
// A value that arrived as a JSON number, or a string with no comma in it, is
// unambiguous machine-readable decimal and is parsed as such. Only a string that
// mixes separators falls back to the locale heuristic, because some shops do
// paste their display format into JSON-LD.
func structuredMoney(raw string, currency domain.Currency, numeric bool) (domain.Money, error) {
	if currency == "" {
		return domain.Money{}, fmt.Errorf("%w: no currency for %q", ErrNoPrice, raw)
	}
	if numeric || !strings.Contains(raw, ",") {
		amount, err := ParseDecimalAmount(raw, currency)
		if err == nil {
			return domain.Money{Amount: amount, Currency: currency}, nil
		}
		if numeric {
			return domain.Money{}, err
		}
	}
	return ParseMoney(raw, currency)
}

// priceString accepts the JSON types shops use for a price and reports whether
// the value was a number, which makes its format unambiguous.
func priceString(value any) (raw string, numeric bool, ok bool) {
	switch typed := value.(type) {
	case string:
		trimmed := strings.TrimSpace(typed)
		return trimmed, false, trimmed != ""
	case float64:
		// %g would render large prices in exponent form, which the parser would
		// then read as a much smaller number.
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.4f", typed), "0"), "."), true, true
	case json.Number:
		return typed.String(), true, true
	default:
		return "", false, false
	}
}
