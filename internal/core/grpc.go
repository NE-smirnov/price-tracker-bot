package core

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
	pb "github.com/NE-smirnov/price-tracker-bot/internal/gen/pricetracker/v1"
	"github.com/NE-smirnov/price-tracker-bot/internal/platform/redisclient"
)

// ItemServer exposes the item and user side of the repository over gRPC.
type ItemServer struct {
	pb.UnimplementedItemServiceServer
	repo *Repo
	log  *slog.Logger
}

// NewItemServer wires the item service.
func NewItemServer(repo *Repo, log *slog.Logger) *ItemServer {
	return &ItemServer{repo: repo, log: log}
}

// EnsureUser registers or refreshes a Telegram user.
func (s *ItemServer) EnsureUser(ctx context.Context, req *pb.EnsureUserRequest) (*pb.EnsureUserResponse, error) {
	if req.GetTelegramId() == 0 {
		return nil, status.Error(codes.InvalidArgument, "telegram_id is required")
	}
	user, created, err := s.repo.EnsureUser(ctx, req.GetTelegramId(), req.GetUsername(), req.GetLanguageCode())
	if err != nil {
		return nil, s.fail("ensure user", err)
	}
	return &pb.EnsureUserResponse{User: userToProto(user), Created: created}, nil
}

// UpdateUserSettings changes the fields the request carries.
func (s *ItemServer) UpdateUserSettings(ctx context.Context, req *pb.UpdateUserSettingsRequest) (*pb.UpdateUserSettingsResponse, error) {
	if req.GetUserId() == "" {
		return nil, status.Error(codes.InvalidArgument, "user_id is required")
	}
	var currency *domain.Currency
	if req.DefaultCurrency != nil {
		c := domain.Currency(req.GetDefaultCurrency())
		currency = &c
	}
	user, err := s.repo.UpdateUserSettings(ctx, req.GetUserId(), currency)
	if err != nil {
		return nil, s.fail("update user settings", err)
	}
	return &pb.UpdateUserSettingsResponse{User: userToProto(user)}, nil
}

// CreateTrackedItem adds an item to the user's watch list.
func (s *ItemServer) CreateTrackedItem(ctx context.Context, req *pb.CreateTrackedItemRequest) (*pb.CreateTrackedItemResponse, error) {
	if req.GetUserId() == "" || req.GetUrl() == "" {
		return nil, status.Error(codes.InvalidArgument, "user_id and url are required")
	}
	item, err := s.repo.CreateItem(ctx, CreateItemInput{
		UserID:      req.GetUserId(),
		URL:         req.GetUrl(),
		Title:       req.GetTitle(),
		TargetPrice: moneyFromProto(req.GetTargetPrice()),
		Interval:    secondsToDuration(req.GetCheckIntervalSeconds()),
	})
	if err != nil {
		return nil, s.fail("create item", err)
	}
	return &pb.CreateTrackedItemResponse{Item: itemToProto(item)}, nil
}

// GetTrackedItem returns one item owned by the caller's user.
func (s *ItemServer) GetTrackedItem(ctx context.Context, req *pb.GetTrackedItemRequest) (*pb.GetTrackedItemResponse, error) {
	if req.GetUserId() == "" || req.GetItemId() == "" {
		return nil, status.Error(codes.InvalidArgument, "user_id and item_id are required")
	}
	item, err := s.repo.GetItem(ctx, req.GetUserId(), req.GetItemId())
	if err != nil {
		return nil, s.fail("get item", err)
	}
	return &pb.GetTrackedItemResponse{Item: itemToProto(item)}, nil
}

// ListTrackedItems returns the user's watch list.
func (s *ItemServer) ListTrackedItems(ctx context.Context, req *pb.ListTrackedItemsRequest) (*pb.ListTrackedItemsResponse, error) {
	if req.GetUserId() == "" {
		return nil, status.Error(codes.InvalidArgument, "user_id is required")
	}
	items, err := s.repo.ListItems(ctx, req.GetUserId(), req.GetIncludeInactive())
	if err != nil {
		return nil, s.fail("list items", err)
	}
	out := make([]*pb.TrackedItem, 0, len(items))
	for _, item := range items {
		out = append(out, itemToProto(item))
	}
	return &pb.ListTrackedItemsResponse{Items: out}, nil
}

// UpdateTrackedItem applies a partial update.
func (s *ItemServer) UpdateTrackedItem(ctx context.Context, req *pb.UpdateTrackedItemRequest) (*pb.UpdateTrackedItemResponse, error) {
	if req.GetUserId() == "" || req.GetItemId() == "" {
		return nil, status.Error(codes.InvalidArgument, "user_id and item_id are required")
	}
	patch := UpdateItemPatch{
		TargetPrice:      moneyFromProto(req.GetTargetPrice()),
		ClearTargetPrice: req.GetClearTargetPrice(),
	}
	if req.CheckIntervalSeconds != nil {
		d := secondsToDuration(req.GetCheckIntervalSeconds())
		patch.Interval = &d
	}
	if req.Active != nil {
		active := req.GetActive()
		patch.Active = &active
	}
	if req.Title != nil {
		title := req.GetTitle()
		patch.Title = &title
	}

	item, err := s.repo.UpdateItem(ctx, req.GetUserId(), req.GetItemId(), patch)
	if err != nil {
		return nil, s.fail("update item", err)
	}
	return &pb.UpdateTrackedItemResponse{Item: itemToProto(item)}, nil
}

// DeleteTrackedItem removes an item and its history.
func (s *ItemServer) DeleteTrackedItem(ctx context.Context, req *pb.DeleteTrackedItemRequest) (*pb.DeleteTrackedItemResponse, error) {
	if req.GetUserId() == "" || req.GetItemId() == "" {
		return nil, status.Error(codes.InvalidArgument, "user_id and item_id are required")
	}
	if err := s.repo.DeleteItem(ctx, req.GetUserId(), req.GetItemId()); err != nil {
		return nil, s.fail("delete item", err)
	}
	return &pb.DeleteTrackedItemResponse{}, nil
}

// ClaimDueItems leases the batch of items a scraper should fetch next.
func (s *ItemServer) ClaimDueItems(ctx context.Context, req *pb.ClaimDueItemsRequest) (*pb.ClaimDueItemsResponse, error) {
	items, err := s.repo.ClaimDueItems(ctx, int(req.GetLimit()), secondsToDuration(req.GetLeaseSeconds()))
	if err != nil {
		return nil, s.fail("claim due items", err)
	}
	out := make([]*pb.TrackedItem, 0, len(items))
	for _, item := range items {
		out = append(out, itemToProto(item))
	}
	return &pb.ClaimDueItemsResponse{Items: out}, nil
}

func (s *ItemServer) fail(op string, err error) error { return toStatus(s.log, op, err) }

// PricingServer exposes price history and statistics over gRPC.
type PricingServer struct {
	pb.UnimplementedPricingServiceServer
	repo  *Repo
	cache *redisclient.Cache
	log   *slog.Logger
}

// NewPricingServer wires the pricing service. The cache may be nil, in which
// case every request is computed from Postgres.
func NewPricingServer(repo *Repo, cache *redisclient.Cache, log *slog.Logger) *PricingServer {
	return &PricingServer{repo: repo, cache: cache, log: log}
}

// RecordSnapshot stores an observation and returns the alerts to deliver.
func (s *PricingServer) RecordSnapshot(ctx context.Context, req *pb.RecordSnapshotRequest) (*pb.RecordSnapshotResponse, error) {
	if req.GetTrackedItemId() == "" {
		return nil, status.Error(codes.InvalidArgument, "tracked_item_id is required")
	}
	// A missing price is only meaningful for an unavailable product; for an
	// in-stock one it means the scraper failed to read the page and should say so
	// through RecordFailure instead of writing a snapshot with no number in it.
	if req.GetPrice() == nil && req.GetInStock() {
		return nil, status.Error(codes.InvalidArgument, "price is required for an in-stock observation")
	}

	var observedAt time.Time
	if req.GetObservedAt() != nil {
		observedAt = req.GetObservedAt().AsTime()
	}

	result, err := s.repo.RecordSnapshot(ctx, RecordSnapshotInput{
		TrackedItemID: req.GetTrackedItemId(),
		Price:         moneyFromProto(req.GetPrice()),
		Converted:     moneyFromProto(req.GetConvertedPrice()),
		InStock:       req.GetInStock(),
		ObservedAt:    observedAt,
		ObservedTitle: req.GetObservedTitle(),
	})
	if err != nil {
		return nil, toStatus(s.log, "record snapshot", err)
	}
	// A new observation makes every cached window for this item wrong, so the
	// entry is dropped rather than left to expire.
	s.cache.Delete(ctx, statsCacheKey(req.GetTrackedItemId()))

	return &pb.RecordSnapshotResponse{
		Snapshot: snapshotToProto(result.Snapshot),
		Alerts:   pendingAlerts(result.Item, result.TelegramID, result.Alerts),
	}, nil
}

// RecordFailure reports a scrape attempt that produced no price.
func (s *PricingServer) RecordFailure(ctx context.Context, req *pb.RecordFailureRequest) (*pb.RecordFailureResponse, error) {
	if req.GetTrackedItemId() == "" {
		return nil, status.Error(codes.InvalidArgument, "tracked_item_id is required")
	}
	result, err := s.repo.RecordFailure(ctx, req.GetTrackedItemId(), req.GetReason())
	if err != nil {
		return nil, toStatus(s.log, "record failure", err)
	}
	return &pb.RecordFailureResponse{
		FailureStreak: int32(result.Streak),
		Alerts:        pendingAlerts(result.Item, result.TelegramID, result.Alerts),
	}, nil
}

// GetPriceHistory streams the snapshots of an item oldest-first.
func (s *PricingServer) GetPriceHistory(req *pb.GetPriceHistoryRequest, stream pb.PricingService_GetPriceHistoryServer) error {
	if req.GetUserId() == "" || req.GetItemId() == "" {
		return status.Error(codes.InvalidArgument, "user_id and item_id are required")
	}
	var since time.Time
	if req.GetSince() != nil {
		since = req.GetSince().AsTime()
	}

	err := s.repo.History(stream.Context(), req.GetUserId(), req.GetItemId(), since, int(req.GetLimit()),
		func(snap domain.PriceSnapshot) error {
			return stream.Send(snapshotToProto(snap))
		})
	if err != nil {
		return toStatus(s.log, "price history", err)
	}
	return nil
}

// GetStats returns aggregates over the requested window.
func (s *PricingServer) GetStats(ctx context.Context, req *pb.GetStatsRequest) (*pb.GetStatsResponse, error) {
	if req.GetUserId() == "" || req.GetItemId() == "" {
		return nil, status.Error(codes.InvalidArgument, "user_id and item_id are required")
	}
	window := secondsToDuration(req.GetWindowSeconds())
	key := statsCacheKey(req.GetItemId()) + ":" + strconv.FormatInt(int64(window.Seconds()), 10)

	var cached domain.Stats
	if s.cache.Get(ctx, key, &cached) && cached.TrackedItemID == req.GetItemId() {
		// Ownership still has to be proven on a cache hit; the cached value is
		// keyed by item, not by caller.
		if _, err := s.repo.GetItem(ctx, req.GetUserId(), req.GetItemId()); err != nil {
			return nil, toStatus(s.log, "stats", err)
		}
		return &pb.GetStatsResponse{Stats: statsToProto(cached)}, nil
	}

	stats, err := s.repo.Stats(ctx, req.GetUserId(), req.GetItemId(), window)
	if err != nil {
		return nil, toStatus(s.log, "stats", err)
	}
	s.cache.Set(ctx, key, stats)

	return &pb.GetStatsResponse{Stats: statsToProto(stats)}, nil
}

// statsCacheKey is the per-item prefix; the concrete window is appended by the
// reader so that invalidation can drop the item with a single prefix delete.
func statsCacheKey(itemID string) string { return "stats:" + itemID }

// toStatus maps domain sentinels onto gRPC codes. Anything unrecognised becomes
// Internal with a generic message: the details go to the log, not to the client,
// because a client cannot act on them and they may leak query internals.
func toStatus(log *slog.Logger, op string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, domain.ErrAlreadyExist):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, domain.ErrValidation):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, domain.ErrLimitReached):
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "deadline exceeded")
	default:
		if log != nil {
			log.Error("core request failed", "op", op, "error", err)
		}
		return status.Errorf(codes.Internal, "%s failed", op)
	}
}
