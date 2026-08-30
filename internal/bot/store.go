package bot

import (
	"context"
	"time"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

// AddItemInput is the payload of Store.AddItem.
type AddItemInput struct {
	UserID   string
	URL      string
	Title    string
	Target   *domain.Money
	Interval time.Duration
}

// Store is everything the bot needs from the outside world.
//
// Two implementations exist:
//   - memStore  — in-process, used for the standalone bot prototype and tests;
//   - coreStore — gRPC client of the core service (wired in step 3).
//
// Keeping the boundary here means the Telegram layer never changes when the
// backend moves from a map to Postgres behind gRPC.
type Store interface {
	// EnsureUser registers the Telegram user on first contact and returns it.
	EnsureUser(ctx context.Context, telegramID int64, username, language string) (domain.User, error)
	// SetDefaultCurrency changes the currency used to render prices.
	SetDefaultCurrency(ctx context.Context, userID string, currency domain.Currency) error

	// AddItem starts tracking a product for a user.
	AddItem(ctx context.Context, in AddItemInput) (domain.TrackedItem, error)
	// ListItems returns every item of a user, newest first.
	ListItems(ctx context.Context, userID string) ([]domain.TrackedItem, error)
	// GetItem returns one item owned by the user.
	GetItem(ctx context.Context, userID, itemID string) (domain.TrackedItem, error)
	// RemoveItem stops tracking an item.
	RemoveItem(ctx context.Context, userID, itemID string) error
	// SetInterval changes the polling interval of an item.
	SetInterval(ctx context.Context, userID, itemID string, interval time.Duration) error

	// Stats returns aggregated price history for an item.
	Stats(ctx context.Context, userID, itemID string, window time.Duration) (domain.Stats, error)
	// History returns raw snapshots for an item, oldest first.
	History(ctx context.Context, userID, itemID string, window time.Duration) ([]domain.PriceSnapshot, error)
}
