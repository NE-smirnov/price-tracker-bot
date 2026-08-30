package core

import (
	"testing"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

func usd(amount int64) domain.Money  { return domain.Money{Amount: amount, Currency: "USD"} }
func tryl(amount int64) domain.Money { return domain.Money{Amount: amount, Currency: "TRY"} }

func snap(id string, price domain.Money, inStock bool) domain.PriceSnapshot {
	return domain.PriceSnapshot{ID: id, TrackedItemID: "item", Price: price, InStock: inStock}
}

func itemWithTarget(target *domain.Money) domain.TrackedItem {
	return domain.TrackedItem{ID: "item", UserID: "user", TargetPrice: target}
}

// kinds extracts the alert kinds in order, which is what the assertions care
// about; the payload is checked separately in the cases where it matters.
func kinds(alerts []domain.Alert) []domain.AlertKind {
	out := make([]domain.AlertKind, 0, len(alerts))
	for _, a := range alerts {
		out = append(out, a.Kind)
	}
	return out
}

func equalKinds(got []domain.AlertKind, want ...domain.AlertKind) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestDecideAlertsPriceThreshold(t *testing.T) {
	t.Parallel()

	target := usd(1000)

	tests := []struct {
		name     string
		previous *domain.PriceSnapshot
		newPrice domain.Money
		want     []domain.AlertKind
	}{
		{
			name:     "first observation already below target fires",
			previous: nil,
			newPrice: usd(900),
			want:     []domain.AlertKind{domain.AlertPriceDrop},
		},
		{
			name:     "crossing the threshold fires once",
			previous: ptr(snap("s1", usd(1100), true)),
			newPrice: usd(950),
			want:     []domain.AlertKind{domain.AlertPriceDrop},
		},
		{
			name:     "staying below the threshold is silent",
			previous: ptr(snap("s1", usd(950), true)),
			newPrice: usd(940),
			want:     nil,
		},
		{
			name:     "exactly at the threshold counts as reached",
			previous: ptr(snap("s1", usd(1100), true)),
			newPrice: usd(1000),
			want:     []domain.AlertKind{domain.AlertPriceDrop},
		},
		{
			name:     "above the threshold is silent",
			previous: ptr(snap("s1", usd(1100), true)),
			newPrice: usd(1050),
			want:     nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := DecideAlerts(AlertInput{
				Item:     itemWithTarget(&target),
				Previous: tc.previous,
				New:      snap("s2", tc.newPrice, true),
			})
			if !equalKinds(kinds(got), tc.want...) {
				t.Fatalf("kinds = %v, want %v", kinds(got), tc.want)
			}
		})
	}
}

func TestDecideAlertsRefusesToCompareForeignCurrency(t *testing.T) {
	t.Parallel()

	target := usd(1000)
	// The shop quotes TRY and no conversion is attached: guessing a rate here
	// would silently move the user's threshold, so nothing must fire.
	got := DecideAlerts(AlertInput{
		Item: itemWithTarget(&target),
		New:  snap("s1", tryl(500), true),
	})
	if len(got) != 0 {
		t.Fatalf("expected no alerts without a usable rate, got %v", kinds(got))
	}
}

func TestDecideAlertsUsesConvertedPrice(t *testing.T) {
	t.Parallel()

	target := usd(1000)
	newSnap := snap("s1", tryl(30000), true)
	converted := usd(800)
	newSnap.Converted = &converted

	got := DecideAlerts(AlertInput{Item: itemWithTarget(&target), New: newSnap})
	if !equalKinds(kinds(got), domain.AlertPriceDrop) {
		t.Fatalf("kinds = %v, want price_drop", kinds(got))
	}
	// The message must quote the shop's own price, not the converted one.
	if got[0].Price != tryl(30000) {
		t.Fatalf("alert price = %v, want the shop price", got[0].Price)
	}
	if got[0].TargetPrice == nil || *got[0].TargetPrice != target {
		t.Fatalf("alert must carry the threshold it was compared against")
	}
}

func TestDecideAlertsStockTransitions(t *testing.T) {
	t.Parallel()

	item := itemWithTarget(nil)

	t.Run("back in stock", func(t *testing.T) {
		t.Parallel()
		got := DecideAlerts(AlertInput{
			Item:     item,
			Previous: ptr(snap("s1", usd(1000), false)),
			New:      snap("s2", usd(1000), true),
		})
		if !equalKinds(kinds(got), domain.AlertBackInStock) {
			t.Fatalf("kinds = %v, want back_in_stock", kinds(got))
		}
	})

	t.Run("out of stock suppresses price alerts", func(t *testing.T) {
		t.Parallel()
		target := usd(1000)
		got := DecideAlerts(AlertInput{
			Item:       itemWithTarget(&target),
			Previous:   ptr(snap("s1", usd(1100), true)),
			AllTimeMin: ptr(usd(1100)),
			New:        snap("s2", usd(500), false),
		})
		if !equalKinds(kinds(got), domain.AlertOutOfStock) {
			t.Fatalf("kinds = %v, want out_of_stock only", kinds(got))
		}
	})

	t.Run("unchanged availability is silent", func(t *testing.T) {
		t.Parallel()
		got := DecideAlerts(AlertInput{
			Item:     item,
			Previous: ptr(snap("s1", usd(1000), true)),
			New:      snap("s2", usd(1000), true),
		})
		if len(got) != 0 {
			t.Fatalf("expected silence, got %v", kinds(got))
		}
	})
}

func TestDecideAlertsAllTimeLow(t *testing.T) {
	t.Parallel()

	t.Run("new minimum fires without a threshold", func(t *testing.T) {
		t.Parallel()
		got := DecideAlerts(AlertInput{
			Item:       itemWithTarget(nil),
			Previous:   ptr(snap("s1", usd(1000), true)),
			AllTimeMin: ptr(usd(900)),
			New:        snap("s2", usd(850), true),
		})
		if !equalKinds(kinds(got), domain.AlertAllTimeLow) {
			t.Fatalf("kinds = %v, want all_time_low", kinds(got))
		}
	})

	t.Run("matching the old minimum is not a new low", func(t *testing.T) {
		t.Parallel()
		got := DecideAlerts(AlertInput{
			Item:       itemWithTarget(nil),
			AllTimeMin: ptr(usd(900)),
			New:        snap("s2", usd(900), true),
		})
		if len(got) != 0 {
			t.Fatalf("expected silence, got %v", kinds(got))
		}
	})

	t.Run("minimum in another currency is not comparable", func(t *testing.T) {
		t.Parallel()
		got := DecideAlerts(AlertInput{
			Item:       itemWithTarget(nil),
			AllTimeMin: ptr(tryl(10)),
			New:        snap("s2", usd(900), true),
		})
		if len(got) != 0 {
			t.Fatalf("expected silence across currencies, got %v", kinds(got))
		}
	})

	t.Run("threshold and all-time low are both reported", func(t *testing.T) {
		t.Parallel()
		target := usd(1000)
		got := DecideAlerts(AlertInput{
			Item:       itemWithTarget(&target),
			Previous:   ptr(snap("s1", usd(1100), true)),
			AllTimeMin: ptr(usd(1050)),
			New:        snap("s2", usd(800), true),
		})
		if !equalKinds(kinds(got), domain.AlertPriceDrop, domain.AlertAllTimeLow) {
			t.Fatalf("kinds = %v, want price_drop then all_time_low", kinds(got))
		}
	})
}

func TestDecideAlertsDedupKeysAreStableAndDistinct(t *testing.T) {
	t.Parallel()

	target := usd(1000)
	in := AlertInput{
		Item:       itemWithTarget(&target),
		Previous:   ptr(snap("s1", usd(1100), true)),
		AllTimeMin: ptr(usd(1050)),
		New:        snap("s2", usd(800), true),
	}

	first := DecideAlerts(in)
	second := DecideAlerts(in)
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("expected two alerts, got %d and %d", len(first), len(second))
	}
	// Stable across calls: this is what lets the unique index in Postgres turn a
	// retried scrape into a no-op.
	for i := range first {
		if first[i].DedupKey != second[i].DedupKey {
			t.Fatalf("dedup key %d is not stable: %q vs %q", i, first[i].DedupKey, second[i].DedupKey)
		}
	}
	// Distinct between kinds: otherwise one alert would swallow the other.
	if first[0].DedupKey == first[1].DedupKey {
		t.Fatalf("different alert kinds share a dedup key: %q", first[0].DedupKey)
	}
}

func TestDecideFailureAlertFiresOnceAtThreshold(t *testing.T) {
	t.Parallel()

	item := itemWithTarget(nil)

	for _, streak := range []int{1, 4, 6, 50} {
		if got := DecideFailureAlert(item, streak, 5); len(got) != 0 {
			t.Fatalf("streak %d should be silent, got %v", streak, kinds(got))
		}
	}
	got := DecideFailureAlert(item, 5, 5)
	if !equalKinds(kinds(got), domain.AlertScrapeDegraded) {
		t.Fatalf("kinds = %v, want scrape_degraded", kinds(got))
	}
	if got[0].DedupKey == "" {
		t.Fatal("failure alert must carry a dedup key")
	}
	if disabled := DecideFailureAlert(item, 5, 0); len(disabled) != 0 {
		t.Fatalf("threshold 0 disables the alert, got %v", kinds(disabled))
	}
}

func ptr[T any](v T) *T { return &v }
