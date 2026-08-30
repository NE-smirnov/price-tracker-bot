package bot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/NE-smirnov/price-tracker-bot/internal/gen/pricetracker/v1"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

// CoreStore is a Store backed by the core service over gRPC.
//
// It deliberately contains no business rules: limits, validation, ownership and
// alert decisions all live in core, because the bot is not the only future
// client of that data. What this type does own is the translation between
// transport errors and the domain sentinels the Telegram layer already knows
// how to render, so a handler cannot tell a memory store from a remote one.
type CoreStore struct {
	conn    *grpc.ClientConn
	items   pb.ItemServiceClient
	pricing pb.PricingServiceClient
	timeout time.Duration
}

// CoreStoreOptions configures the gRPC connection to core.
type CoreStoreOptions struct {
	// Addr is a host:port of the core service.
	Addr string
	// CallTimeout bounds every single RPC. A hung backend must not freeze a
	// Telegram conversation, so this is applied even when the caller passes a
	// context without a deadline.
	CallTimeout time.Duration
}

const defaultCallTimeout = 10 * time.Second

// NewCoreStore dials core. The connection is lazy: gRPC returns immediately and
// reconnects in the background, so the bot starts even if core is still booting
// and recovers by itself once core is back.
func NewCoreStore(opts CoreStoreOptions) (*CoreStore, error) {
	if opts.Addr == "" {
		return nil, errors.New("core address is empty")
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = defaultCallTimeout
	}

	conn, err := grpc.NewClient(opts.Addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		// Both services sit inside one compose network / VPC, so a plaintext
		// connection is intentional; TLS belongs at the deployment edge.
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.WithDefaultCallOptions(grpc.WaitForReady(false)),
	)
	if err != nil {
		return nil, fmt.Errorf("dial core at %s: %w", opts.Addr, err)
	}

	return &CoreStore{
		conn:    conn,
		items:   pb.NewItemServiceClient(conn),
		pricing: pb.NewPricingServiceClient(conn),
		timeout: opts.CallTimeout,
	}, nil
}

// call bounds one unary RPC. Telegram updates arrive with a long-lived context,
// so without this a single stuck backend call would keep a conversation waiting
// indefinitely.
func (s *CoreStore) call(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.timeout)
}

// Close releases the connection.
func (s *CoreStore) Close() error {
	if s == nil || s.conn == nil {
		return nil
	}
	return s.conn.Close()
}

// EnsureUser implements Store.
func (s *CoreStore) EnsureUser(ctx context.Context, telegramID int64, username, language string) (domain.User, error) {
	ctx, cancel := s.call(ctx)
	defer cancel()

	resp, err := s.items.EnsureUser(ctx, &pb.EnsureUserRequest{
		TelegramId:   telegramID,
		Username:     username,
		LanguageCode: language,
	})
	if err != nil {
		return domain.User{}, fromStatus(err, "ensure user")
	}
	return userFromProto(resp.GetUser()), nil
}

// SetDefaultCurrency implements Store.
func (s *CoreStore) SetDefaultCurrency(ctx context.Context, userID string, currency domain.Currency) error {
	ctx, cancel := s.call(ctx)
	defer cancel()

	code := string(currency)
	_, err := s.items.UpdateUserSettings(ctx, &pb.UpdateUserSettingsRequest{
		UserId:          userID,
		DefaultCurrency: &code,
	})
	if err != nil {
		return fromStatus(err, "set currency")
	}
	return nil
}

// AddItem implements Store.
func (s *CoreStore) AddItem(ctx context.Context, in AddItemInput) (domain.TrackedItem, error) {
	ctx, cancel := s.call(ctx)
	defer cancel()

	resp, err := s.items.CreateTrackedItem(ctx, &pb.CreateTrackedItemRequest{
		UserId:               in.UserID,
		Url:                  in.URL,
		Title:                in.Title,
		TargetPrice:          moneyPtrToProto(in.Target),
		CheckIntervalSeconds: int32(in.Interval.Seconds()),
	})
	if err != nil {
		return domain.TrackedItem{}, fromStatus(err, "add item")
	}
	return itemFromProto(resp.GetItem()), nil
}

// ListItems implements Store.
func (s *CoreStore) ListItems(ctx context.Context, userID string) ([]domain.TrackedItem, error) {
	ctx, cancel := s.call(ctx)
	defer cancel()

	resp, err := s.items.ListTrackedItems(ctx, &pb.ListTrackedItemsRequest{
		UserId:          userID,
		IncludeInactive: false,
	})
	if err != nil {
		return nil, fromStatus(err, "list items")
	}
	out := make([]domain.TrackedItem, 0, len(resp.GetItems()))
	for _, it := range resp.GetItems() {
		out = append(out, itemFromProto(it))
	}
	return out, nil
}

// GetItem implements Store.
func (s *CoreStore) GetItem(ctx context.Context, userID, itemID string) (domain.TrackedItem, error) {
	ctx, cancel := s.call(ctx)
	defer cancel()

	resp, err := s.items.GetTrackedItem(ctx, &pb.GetTrackedItemRequest{
		UserId: userID,
		ItemId: itemID,
	})
	if err != nil {
		return domain.TrackedItem{}, fromStatus(err, "get item")
	}
	return itemFromProto(resp.GetItem()), nil
}

// RemoveItem implements Store.
func (s *CoreStore) RemoveItem(ctx context.Context, userID, itemID string) error {
	ctx, cancel := s.call(ctx)
	defer cancel()

	if _, err := s.items.DeleteTrackedItem(ctx, &pb.DeleteTrackedItemRequest{
		UserId: userID,
		ItemId: itemID,
	}); err != nil {
		return fromStatus(err, "remove item")
	}
	return nil
}

// SetInterval implements Store.
func (s *CoreStore) SetInterval(ctx context.Context, userID, itemID string, interval time.Duration) error {
	ctx, cancel := s.call(ctx)
	defer cancel()

	seconds := int32(interval.Seconds())
	if _, err := s.items.UpdateTrackedItem(ctx, &pb.UpdateTrackedItemRequest{
		UserId:               userID,
		ItemId:               itemID,
		CheckIntervalSeconds: &seconds,
	}); err != nil {
		return fromStatus(err, "set interval")
	}
	return nil
}

// SetTarget changes or clears the alert threshold of an item.
//
// It is not part of Store yet: the /settings dialog currently only edits the
// interval. The method lives here so wiring the dialog needs no client changes.
func (s *CoreStore) SetTarget(ctx context.Context, userID, itemID string, target *domain.Money) error {
	ctx, cancel := s.call(ctx)
	defer cancel()

	req := &pb.UpdateTrackedItemRequest{
		UserId: userID,
		ItemId: itemID,
	}
	if target == nil {
		req.ClearTargetPrice = true
	} else {
		req.TargetPrice = moneyPtrToProto(target)
	}
	if _, err := s.items.UpdateTrackedItem(ctx, req); err != nil {
		return fromStatus(err, "set target price")
	}
	return nil
}

// Stats implements Store.
func (s *CoreStore) Stats(ctx context.Context, userID, itemID string, window time.Duration) (domain.Stats, error) {
	ctx, cancel := s.call(ctx)
	defer cancel()

	resp, err := s.pricing.GetStats(ctx, &pb.GetStatsRequest{
		UserId:        userID,
		ItemId:        itemID,
		WindowSeconds: int32(window.Seconds()),
	})
	if err != nil {
		return domain.Stats{}, fromStatus(err, "stats")
	}
	return statsFromProto(resp.GetStats()), nil
}

// History implements Store.
//
// The RPC is a server stream, so a long history arrives incrementally instead of
// as one large response. The bot still collects it into a slice because it
// renders a single message, but the transport no longer caps how much history
// core can hold.
func (s *CoreStore) History(ctx context.Context, userID, itemID string, window time.Duration) ([]domain.PriceSnapshot, error) {
	// A stream may legitimately outlive a single unary budget, so it gets a
	// wider one rather than the per-call timeout.
	ctx, cancel := context.WithTimeout(ctx, 3*s.timeout)
	defer cancel()

	req := &pb.GetPriceHistoryRequest{
		UserId: userID,
		ItemId: itemID,
		Limit:  0,
	}
	if window > 0 {
		req.Since = timestamppb.New(time.Now().Add(-window))
	}

	stream, err := s.pricing.GetPriceHistory(ctx, req)
	if err != nil {
		return nil, fromStatus(err, "history")
	}

	var out []domain.PriceSnapshot
	for {
		msg, recvErr := stream.Recv()
		if recvErr != nil {
			// A completed stream reports io.EOF, not a gRPC status, so it must be
			// checked before error translation.
			if errors.Is(recvErr, io.EOF) {
				break
			}
			return nil, fromStatus(recvErr, "history stream")
		}
		out = append(out, snapshotFromProto(msg))
	}
	return out, nil
}

// fromStatus turns a gRPC status back into the domain sentinel the Telegram
// handlers already branch on. Without this the bot would have to know gRPC codes
// in every handler, and a transport change would ripple through the UI layer.
func fromStatus(err error, what string) error {
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("%s: %w", what, err)
	}
	switch st.Code() {
	case codes.NotFound:
		return fmt.Errorf("%s: %w", what, domain.ErrNotFound)
	case codes.AlreadyExists:
		return fmt.Errorf("%s: %w", what, domain.ErrAlreadyExist)
	case codes.InvalidArgument:
		return fmt.Errorf("%s: %w: %s", what, domain.ErrValidation, st.Message())
	case codes.ResourceExhausted:
		return fmt.Errorf("%s: %w", what, domain.ErrLimitReached)
	case codes.Canceled:
		return fmt.Errorf("%s: %w", what, context.Canceled)
	case codes.DeadlineExceeded:
		return fmt.Errorf("%s: %w", what, context.DeadlineExceeded)
	default:
		return fmt.Errorf("%s: core unavailable: %s", what, st.Message())
	}
}

// ---------------------------------------------------------------- mappers

func moneyPtrToProto(m *domain.Money) *pb.Money {
	if m == nil {
		return nil
	}
	return &pb.Money{Amount: m.Amount, Currency: string(m.Currency)}
}

func moneyFromProto(m *pb.Money) domain.Money {
	if m == nil {
		return domain.Money{}
	}
	return domain.Money{Amount: m.GetAmount(), Currency: domain.Currency(m.GetCurrency())}
}

func moneyPtrFromProto(m *pb.Money) *domain.Money {
	if m == nil {
		return nil
	}
	out := moneyFromProto(m)
	return &out
}

func userFromProto(u *pb.User) domain.User {
	if u == nil {
		return domain.User{}
	}
	currency := domain.Currency(u.GetDefaultCurrency())
	if currency == "" {
		currency = domain.DefaultCurrency
	}
	return domain.User{
		ID:              u.GetId(),
		TelegramID:      u.GetTelegramId(),
		Username:        u.GetUsername(),
		Language:        u.GetLanguageCode(),
		DefaultCurrency: currency,
		CreatedAt:       u.GetCreatedAt().AsTime(),
	}
}

func itemFromProto(i *pb.TrackedItem) domain.TrackedItem {
	if i == nil {
		return domain.TrackedItem{}
	}
	item := domain.TrackedItem{
		ID:            i.GetId(),
		UserID:        i.GetUserId(),
		URL:           i.GetUrl(),
		Title:         i.GetTitle(),
		TargetPrice:   moneyPtrFromProto(i.GetTargetPrice()),
		CheckInterval: time.Duration(i.GetCheckIntervalSeconds()) * time.Second,
		Active:        i.GetActive(),
		CreatedAt:     i.GetCreatedAt().AsTime(),
		NextCheckAt:   i.GetNextCheckAt().AsTime(),
		FailureStreak: int(i.GetFailureStreak()),
	}
	if snap := i.GetLastSnapshot(); snap != nil {
		converted := snapshotFromProto(snap)
		item.LastSnapshot = &converted
		observed := converted.ObservedAt
		item.LastCheckedAt = &observed
	}
	return item
}

func snapshotFromProto(s *pb.PriceSnapshot) domain.PriceSnapshot {
	if s == nil {
		return domain.PriceSnapshot{}
	}
	return domain.PriceSnapshot{
		ID:            s.GetId(),
		TrackedItemID: s.GetTrackedItemId(),
		Price:         moneyFromProto(s.GetPrice()),
		Converted:     moneyPtrFromProto(s.GetConvertedPrice()),
		InStock:       s.GetInStock(),
		ObservedAt:    s.GetObservedAt().AsTime(),
	}
}

// statsFromProto rebuilds domain.Stats. Trend and ChangePercent are recomputed
// from First and Current by the domain methods rather than copied from the
// response, so there is exactly one implementation of those rules.
func statsFromProto(s *pb.Stats) domain.Stats {
	if s == nil {
		return domain.Stats{}
	}
	current := moneyFromProto(s.GetCurrent())
	return domain.Stats{
		TrackedItemID: s.GetTrackedItemId(),
		Currency:      current.Currency,
		Min:           moneyFromProto(s.GetMin()),
		Max:           moneyFromProto(s.GetMax()),
		Avg:           moneyFromProto(s.GetAvg()),
		Current:       current,
		First:         moneyFromProto(s.GetFirst()),
		Samples:       int(s.GetSamples()),
		InStock:       s.GetInStock(),
		WindowFrom:    s.GetFirstObservedAt().AsTime(),
		WindowTo:      s.GetLastObservedAt().AsTime(),
	}
}
