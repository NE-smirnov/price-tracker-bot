package core

import (
	"fmt"
	"strings"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

// AlertInput is everything needed to decide which alerts a new observation
// triggers. It is deliberately a plain value: the decision is pure, so it can
// be tested exhaustively without a database.
type AlertInput struct {
	Item domain.TrackedItem
	// Previous is the snapshot immediately before New, if the item has one.
	Previous *domain.PriceSnapshot
	// AllTimeMin is the lowest price ever recorded before New, if any.
	AllTimeMin *domain.Money
	New        domain.PriceSnapshot
}

// DecideAlerts returns the alerts a snapshot triggers, in the order they should
// be delivered.
//
// The rules are edge-triggered on purpose. A price that merely stays below the
// threshold, or an item that stays out of stock, produces nothing: the user is
// told when something changes, not on every poll.
func DecideAlerts(in AlertInput) []domain.Alert {
	var alerts []domain.Alert

	// Stock transitions first: "back in stock" is the most actionable message
	// and should not be buried under a price line.
	if in.Previous != nil && in.New.InStock != in.Previous.InStock {
		kind := domain.AlertOutOfStock
		if in.New.InStock {
			kind = domain.AlertBackInStock
		}
		alerts = append(alerts, domain.Alert{
			Kind:          kind,
			TrackedItemID: in.Item.ID,
			UserID:        in.Item.UserID,
			Price:         in.New.Price,
			PreviousPrice: &in.Previous.Price,
			DedupKey:      dedupKey(kind, in.Item.ID, in.New.ID),
		})
	}

	// A price alert about an unavailable product is noise.
	if !in.New.InStock {
		return alerts
	}

	if in.Item.TargetPrice != nil {
		target := *in.Item.TargetPrice
		if current, ok := priceIn(in.New, target.Currency); ok {
			crossedNow := current.Amount <= target.Amount
			// Edge trigger: fire only when the previous observation was above
			// the threshold, otherwise every poll would repeat the message.
			wasAbove := true
			if in.Previous != nil {
				if prev, ok := priceIn(*in.Previous, target.Currency); ok {
					wasAbove = prev.Amount > target.Amount
				}
			}
			if crossedNow && wasAbove {
				alerts = append(alerts, domain.Alert{
					Kind:          domain.AlertPriceDrop,
					TrackedItemID: in.Item.ID,
					UserID:        in.Item.UserID,
					// Stated in the currency the comparison was made in, not the
					// shop's: a message reading "36829 RUB, желаемая 90000 TRY" gives
					// the user two numbers they cannot compare.
					Price:         current,
					PreviousPrice: previousPriceIn(in.Previous, target.Currency),
					OriginalPrice: originalPrice(in.New.Price, current),
					TargetPrice:   &target,
					// Keyed by price, so a drop to the same value after a
					// bounce back up is reported again, but a duplicate
					// delivery of the same event is not.
					DedupKey: dedupKey(domain.AlertPriceDrop, in.Item.ID,
						fmt.Sprintf("%d%s", current.Amount, current.Currency)),
				})
			}
		}
	}

	// An all-time low is worth reporting even without a threshold, and even if a
	// price-drop alert already fired: it is different information.
	if in.AllTimeMin != nil && in.AllTimeMin.Currency == in.New.Price.Currency &&
		in.New.Price.Amount < in.AllTimeMin.Amount {
		alerts = append(alerts, domain.Alert{
			Kind:          domain.AlertAllTimeLow,
			TrackedItemID: in.Item.ID,
			UserID:        in.Item.UserID,
			Price:         in.New.Price,
			PreviousPrice: &domain.Money{Amount: in.AllTimeMin.Amount, Currency: in.AllTimeMin.Currency},
			DedupKey: dedupKey(domain.AlertAllTimeLow, in.Item.ID,
				fmt.Sprintf("%d%s", in.New.Price.Amount, in.New.Price.Currency)),
		})
	}

	return alerts
}

// DecideFailureAlert reports a broken scrape only when the streak first crosses
// the threshold. Alerting on every failure would turn one dead shop page into a
// message every few minutes.
func DecideFailureAlert(item domain.TrackedItem, streak, threshold int) []domain.Alert {
	if threshold <= 0 || streak != threshold {
		return nil
	}
	return []domain.Alert{{
		Kind:          domain.AlertScrapeDegraded,
		TrackedItemID: item.ID,
		UserID:        item.UserID,
		DedupKey:      dedupKey(domain.AlertScrapeDegraded, item.ID, fmt.Sprintf("streak%d", streak)),
	}}
}

// priceIn returns the snapshot's price expressed in want, using the converted
// amount when the shop's own currency differs. It reports false rather than
// guessing a rate, because a wrong conversion would move the alert threshold.
func priceIn(s domain.PriceSnapshot, want domain.Currency) (domain.Money, bool) {
	if s.Price.Currency == want {
		return s.Price, true
	}
	if s.Converted != nil && s.Converted.Currency == want {
		return *s.Converted, true
	}
	return domain.Money{}, false
}

// originalPrice returns the shop's own price when it differs from the amount the
// alert is stated in, and nil when they are the same number in the same
// currency — repeating it would only add noise to the message.
func originalPrice(shop, stated domain.Money) *domain.Money {
	if shop.Currency == stated.Currency {
		return nil
	}
	price := shop
	return &price
}

// previousPriceIn states the earlier price in the same currency as the current
// one, so the two can be shown side by side. It returns nil rather than an
// amount in another currency, because a percentage across currencies is
// meaningless and a bare unconverted number is worse than none.
func previousPriceIn(s *domain.PriceSnapshot, want domain.Currency) *domain.Money {
	if s == nil {
		return nil
	}
	if price, ok := priceIn(*s, want); ok {
		return &price
	}
	return nil
}

func previousPrice(s *domain.PriceSnapshot) *domain.Money {
	if s == nil {
		return nil
	}
	p := s.Price
	return &p
}

func dedupKey(kind domain.AlertKind, itemID, discriminator string) string {
	return strings.Join([]string{string(kind), itemID, discriminator}, ":")
}
