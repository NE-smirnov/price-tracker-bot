package scraper

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

// wbCardAPI is Wildberries' own product endpoint.
//
// The HTML page is unusable from a server: it answers with HTTP 498, their
// bot-protection status. This JSON endpoint is what their web client calls and
// it answers plainly. Only v4 works; v1 through v3 are retired.
const wbCardAPI = "https://card.wb.ru/cards/v4/detail"

// wbCurrency is the only currency this adapter requests, so a price and the
// target it is compared against are always quoted the same way.
const wbCurrency = currencyRUB

// wbDest is the delivery region the prices are quoted for. Wildberries prices
// differ by region, so it is pinned rather than left to the API's default: an
// item whose price is compared against a target must be quoted consistently.
// -1257786 is Moscow.
const wbDest = "-1257786"

// The API's integer prices are kopecks, the same minor units this project
// stores, so no rescaling is applied. Verified against a live in-stock listing:
// an iPhone 13 Pro priced at 36 829 ₽ comes back as {"product": 3682900}.

// wbItemID matches the numeric product id in a Wildberries URL, e.g.
// https://www.wildberries.ru/catalog/169871469/detail.aspx
var wbItemID = regexp.MustCompile(`/catalog/(\d+)`)

// Wildberries reads prices through the shop's public JSON API.
type Wildberries struct {
	// BaseURL overrides the endpoint. It exists so the adapter can be tested
	// against a local server instead of the live shop.
	BaseURL string
}

func (w Wildberries) endpoint() string {
	if w.BaseURL != "" {
		return w.BaseURL
	}
	return wbCardAPI
}

// Name identifies the adapter.
func (Wildberries) Name() string { return "wildberries" }

// Handles claims the shop's domains, including the regional ones.
func (Wildberries) Handles(host string) bool {
	host = strings.TrimPrefix(strings.ToLower(host), "www.")
	return host == "wildberries.ru" ||
		strings.HasSuffix(host, ".wildberries.ru")
}

// wbResponse is the part of the API response this adapter depends on.
type wbResponse struct {
	Products []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		// Brand is prepended to the title, because "Наушники" alone is not enough
		// for a user to recognise which item an alert is about.
		Brand string `json:"brand"`
		// TotalQuantity is the stock across all warehouses. Zero means the item
		// is unavailable, and the API then omits the price entirely.
		TotalQuantity int64    `json:"totalQuantity"`
		Sizes         []wbSize `json:"sizes"`
	} `json:"products"`
}

// wbSize is one purchasable variant. A size can carry a price while having no
// stock anywhere.
type wbSize struct {
	Stocks []struct {
		Qty int64 `json:"qty"`
	} `json:"stocks"`
	Price *wbPrice `json:"price"`
}

// wbPrice is the shop's price block, in kopecks.
type wbPrice struct {
	// Basic is the price before the shop's discount, Product after it. Product is
	// the number a customer actually pays, so it is preferred.
	Basic   int64 `json:"basic"`
	Product int64 `json:"product"`
	Total   int64 `json:"total"`
}

// Observe reads price and availability for one Wildberries item.
func (w Wildberries) Observe(ctx context.Context, client *Client, rawURL string, _ domain.Currency) (Observation, error) {
	id, err := wbProductID(rawURL)
	if err != nil {
		return Observation{}, err
	}

	query := url.Values{
		"appType": {"1"},
		"curr":    {strings.ToLower(string(wbCurrency))},
		"dest":    {wbDest},
		"nm":      {id},
	}
	page, err := client.Fetch(ctx, w.endpoint()+"?"+query.Encode())
	if err != nil {
		return Observation{}, err
	}

	var parsed wbResponse
	if err := json.Unmarshal(page.Body, &parsed); err != nil {
		return Observation{}, fmt.Errorf("%w: decode wildberries response: %w", ErrNoPrice, err)
	}
	if len(parsed.Products) == 0 {
		// The API answers 200 with an empty list for a delisted item.
		return Observation{}, fmt.Errorf("%w: wildberries knows no item %s", ErrNotFound, id)
	}

	product := parsed.Products[0]
	title := strings.TrimSpace(strings.TrimSpace(product.Brand) + " " + strings.TrimSpace(product.Name))

	best, inStock := wbBestPrice(product.Sizes)
	if best == 0 {
		if inStock {
			// In stock but priced nowhere: an unexpected shape, and reporting a
			// price would mean inventing one.
			return Observation{}, fmt.Errorf("%w: no price in the wildberries response for %s", ErrNoPrice, id)
		}
		// Out of stock with no price is the normal shape for an unavailable item.
		// The observation still carries the availability, which is what the
		// "back in stock" alert is triggered on; core carries the last known
		// price forward.
		return Observation{InStock: false, Title: title, Source: "wildberries-api"}, nil
	}

	return Observation{
		Price:   domain.Money{Amount: best, Currency: wbCurrency},
		InStock: inStock,
		Title:   title,
		Source:  "wildberries-api",
	}, nil
}

// wbBestPrice picks the cheapest available size.
//
// A single product can have several sizes at different prices, and a size can be
// listed with a price while having no stock. The user is tracking the item, so
// the cheapest one they could actually buy is the honest answer.
func wbBestPrice(sizes []wbSize) (best int64, inStock bool) {
	var cheapestAnywhere int64
	for _, size := range sizes {
		available := false
		for _, stock := range size.Stocks {
			if stock.Qty > 0 {
				available = true
				break
			}
		}
		if size.Price == nil {
			continue
		}
		price := size.Price.Product
		if price <= 0 {
			price = size.Price.Total
		}
		if price <= 0 {
			price = size.Price.Basic
		}
		if price <= 0 {
			continue
		}

		if available {
			inStock = true
			if best == 0 || price < best {
				best = price
			}
		}
		if cheapestAnywhere == 0 || price < cheapestAnywhere {
			cheapestAnywhere = price
		}
	}

	// Nothing in stock but a price is still published: record it, so the history
	// keeps advancing and the item's return to stock is a clean transition.
	if best == 0 {
		best = cheapestAnywhere
	}
	return best, inStock
}

func wbProductID(rawURL string) (string, error) {
	match := wbItemID.FindStringSubmatch(rawURL)
	if len(match) != 2 {
		return "", fmt.Errorf("%w: %q has no wildberries item id", ErrNoPrice, rawURL)
	}
	return match[1], nil
}
