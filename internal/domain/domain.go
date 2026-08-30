// Package domain holds the shared business entities of the price tracker.
// It has no dependency on transports (Telegram, gRPC, HTTP) or storage engines.
package domain

import (
	"errors"
	"time"
)

// Sentinel errors shared across layers.
var (
	ErrNotFound     = errors.New("not found")
	ErrAlreadyExist = errors.New("already exists")
	ErrValidation   = errors.New("validation failed")
	ErrLimitReached = errors.New("limit reached")
)

// Limits that keep a hobby deployment (and the target shops) safe.
const (
	MaxItemsPerUser    = 25
	MinCheckInterval   = 5 * time.Minute
	MaxCheckInterval   = 24 * time.Hour
	DefaultInterval    = 1 * time.Hour
	DefaultCurrency    = Currency("USD")
	MaxItemTitleLength = 120
)

// User is a Telegram user registered in the bot.
type User struct {
	ID              string    // internal UUID
	TelegramID      int64     // Telegram user id
	Username        string    // Telegram @username, may be empty
	Language        string    // IETF tag, e.g. "ru"
	DefaultCurrency Currency  // currency used to render prices
	CreatedAt       time.Time //
}

// TrackedItem is a product a user watches.
type TrackedItem struct {
	ID            string
	UserID        string
	URL           string
	Title         string
	TargetPrice   *Money        // nil -> notify on any price drop
	CheckInterval time.Duration // how often the scraper polls the page
	Active        bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
	LastCheckedAt *time.Time
	LastSnapshot  *PriceSnapshot // denormalised for fast /list rendering

	// NextCheckAt drives the scraper schedule; FailureStreak counts consecutive
	// failed scrapes so a broken parser can be reported instead of going quiet.
	NextCheckAt   time.Time
	FailureStreak int
	LastError     string
}

// PriceSnapshot is one observation of a product page.
type PriceSnapshot struct {
	ID            string
	TrackedItemID string
	Price         Money  // price in the shop's own currency
	Converted     *Money // price converted to the user's currency, if known
	InStock       bool
	ObservedAt    time.Time
}

// AlertKind enumerates the reasons the bot notifies a user.
type AlertKind string

const (
	AlertPriceDrop      AlertKind = "price_drop"      // price fell below the target
	AlertBackInStock    AlertKind = "back_in_stock"   // availability flipped to true
	AlertOutOfStock     AlertKind = "out_of_stock"    // availability flipped to false
	AlertAllTimeLow     AlertKind = "all_time_low"    // new minimum over the whole history
	AlertScrapeDegraded AlertKind = "scrape_degraded" // repeated scrape failures
)

// Alert is a notification produced by the alert engine.
type Alert struct {
	ID            string
	TrackedItemID string
	UserID        string
	Kind          AlertKind
	// Price is stated in the currency the alert was judged in, so it can be shown
	// next to the target the user set.
	Price         Money
	PreviousPrice *Money
	TargetPrice   *Money
	// OriginalPrice is what the shop actually charges, set only when it differs
	// from Price because a conversion was involved. Both numbers matter: the
	// converted one is what triggered the alert, the shop's one is what the user
	// will pay at checkout.
	OriginalPrice *Money

	// DedupKey identifies "this alert about this item at this value". Delivery is
	// at-least-once, so the key is what turns a retry into a no-op instead of a
	// second message to the user.
	DedupKey string

	CreatedAt time.Time
	SentAt    *time.Time
}

// Stats aggregates the price history of one item.
type Stats struct {
	TrackedItemID string
	Currency      Currency
	Min           Money
	Max           Money
	Avg           Money
	Current       Money
	First         Money
	Samples       int
	InStock       bool
	WindowFrom    time.Time
	WindowTo      time.Time
}

// TrendDirection describes where the price is heading.
type TrendDirection string

const (
	TrendDown TrendDirection = "down"
	TrendUp   TrendDirection = "up"
	TrendFlat TrendDirection = "flat"
)

// Trend returns the direction of the price between the first and current sample.
func (s Stats) Trend() TrendDirection {
	switch {
	case s.Samples < 2 || s.First.Amount == s.Current.Amount:
		return TrendFlat
	case s.Current.Amount < s.First.Amount:
		return TrendDown
	default:
		return TrendUp
	}
}

// ChangePercent returns the relative change between the first and current sample.
func (s Stats) ChangePercent() float64 {
	if s.Samples < 2 || s.First.Amount == 0 {
		return 0
	}
	return (float64(s.Current.Amount) - float64(s.First.Amount)) / float64(s.First.Amount) * 100
}

// ValidateInterval clamps a user-provided polling interval to the allowed range.
func ValidateInterval(d time.Duration) (time.Duration, error) {
	switch {
	case d == 0:
		return DefaultInterval, nil
	case d < MinCheckInterval:
		return 0, errors.Join(ErrValidation, errors.New("interval is shorter than "+MinCheckInterval.String()))
	case d > MaxCheckInterval:
		return 0, errors.Join(ErrValidation, errors.New("interval is longer than "+MaxCheckInterval.String()))
	default:
		return d, nil
	}
}
