// Package notify carries alerts from the place that raises them to Telegram.
//
// The payload lives here rather than in the scraper because both the producer
// and the consumer must agree on it, and neither should import the other.
package notify

import (
	"time"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
	pb "github.com/NE-smirnov/price-tracker-bot/internal/gen/pricetracker/v1"
)

// Kind is what happened. It mirrors the proto enum as a short string, so a
// queued payload stays readable in redis-cli and survives a proto renumbering.
type Kind string

// The kinds of alert the product sends.
const (
	KindPriceDrop      Kind = "price_drop"
	KindBackInStock    Kind = "back_in_stock"
	KindOutOfStock     Kind = "out_of_stock"
	KindAllTimeLow     Kind = "all_time_low"
	KindScrapeDegraded Kind = "scrape_degraded"
)

// Alert is a ready-to-send notification.
//
// It is self-contained on purpose: the notifier renders and sends it without
// calling core back, so a queue drained after an outage still produces correct
// messages, and the notifier needs no read access to the database.
type Alert struct {
	Kind          Kind          `json:"kind"`
	ItemID        string        `json:"item_id"`
	UserID        string        `json:"user_id"`
	TelegramID    int64         `json:"telegram_id"`
	Title         string        `json:"title"`
	URL           string        `json:"url"`
	Price         *domain.Money `json:"price,omitempty"`
	PreviousPrice *domain.Money `json:"previous_price,omitempty"`
	TargetPrice   *domain.Money `json:"target_price,omitempty"`
	// OriginalPrice is the shop's own price when the alert was judged on a
	// converted amount.
	OriginalPrice *domain.Money `json:"original_price,omitempty"`
	// DedupKey identifies the state change this alert reports. The queue is
	// at-least-once, so the consumer claims this key before sending.
	DedupKey string `json:"dedup_key"`
	// RaisedAt is when the change was observed, not when it is delivered: a
	// message that arrives after a retry should still say when the price moved.
	RaisedAt time.Time `json:"raised_at"`
}

// FromProto converts an alert produced by core into the queued payload.
func FromProto(alert *pb.PendingAlert) Alert {
	return Alert{
		Kind:          kindFromProto(alert.GetKind()),
		ItemID:        alert.GetTrackedItemId(),
		UserID:        alert.GetUserId(),
		TelegramID:    alert.GetTelegramId(),
		Title:         alert.GetItemTitle(),
		URL:           alert.GetItemUrl(),
		Price:         moneyFromProto(alert.GetPrice()),
		PreviousPrice: moneyFromProto(alert.GetPreviousPrice()),
		TargetPrice:   moneyFromProto(alert.GetTargetPrice()),
		OriginalPrice: moneyFromProto(alert.GetOriginalPrice()),
		DedupKey:      alert.GetDedupKey(),
		RaisedAt:      time.Now().UTC(),
	}
}

func kindFromProto(kind pb.AlertKind) Kind {
	switch kind {
	case pb.AlertKind_ALERT_KIND_PRICE_DROP:
		return KindPriceDrop
	case pb.AlertKind_ALERT_KIND_BACK_IN_STOCK:
		return KindBackInStock
	case pb.AlertKind_ALERT_KIND_OUT_OF_STOCK:
		return KindOutOfStock
	case pb.AlertKind_ALERT_KIND_ALL_TIME_LOW:
		return KindAllTimeLow
	case pb.AlertKind_ALERT_KIND_SCRAPE_DEGRADED:
		return KindScrapeDegraded
	case pb.AlertKind_ALERT_KIND_UNSPECIFIED:
		return ""
	default:
		return ""
	}
}

func moneyFromProto(money *pb.Money) *domain.Money {
	if money == nil || money.GetCurrency() == "" {
		return nil
	}
	return &domain.Money{
		Amount:   money.GetAmount(),
		Currency: domain.Currency(money.GetCurrency()),
	}
}
