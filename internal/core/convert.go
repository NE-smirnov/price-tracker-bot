package core

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
	pb "github.com/NE-smirnov/price-tracker-bot/internal/gen/pricetracker/v1"
)

// This file is the only place where wire types meet domain types. Keeping the
// mapping in one file means the transport can change without touching business
// logic, and the domain never imports generated code.

func moneyToProto(m domain.Money) *pb.Money {
	return &pb.Money{Amount: m.Amount, Currency: string(m.Currency)}
}

func moneyPtrToProto(m *domain.Money) *pb.Money {
	if m == nil {
		return nil
	}
	return moneyToProto(*m)
}

func moneyFromProto(m *pb.Money) *domain.Money {
	if m == nil {
		return nil
	}
	return &domain.Money{Amount: m.GetAmount(), Currency: domain.Currency(m.GetCurrency())}
}

func userToProto(u domain.User) *pb.User {
	return &pb.User{
		Id:              u.ID,
		TelegramId:      u.TelegramID,
		Username:        u.Username,
		LanguageCode:    u.Language,
		DefaultCurrency: string(u.DefaultCurrency),
		CreatedAt:       timestamppb.New(u.CreatedAt),
	}
}

func itemToProto(i domain.TrackedItem) *pb.TrackedItem {
	out := &pb.TrackedItem{
		Id:                   i.ID,
		UserId:               i.UserID,
		Url:                  i.URL,
		Title:                i.Title,
		TargetPrice:          moneyPtrToProto(i.TargetPrice),
		CheckIntervalSeconds: int32(i.CheckInterval.Seconds()),
		Active:               i.Active,
		CreatedAt:            timestamppb.New(i.CreatedAt),
		NextCheckAt:          timestamppb.New(i.NextCheckAt),
		FailureStreak:        int32(i.FailureStreak),
	}
	if i.LastSnapshot != nil {
		out.LastSnapshot = snapshotToProto(*i.LastSnapshot)
	}
	return out
}

func snapshotToProto(s domain.PriceSnapshot) *pb.PriceSnapshot {
	return &pb.PriceSnapshot{
		Id:             s.ID,
		TrackedItemId:  s.TrackedItemID,
		Price:          moneyToProto(s.Price),
		ConvertedPrice: moneyPtrToProto(s.Converted),
		InStock:        s.InStock,
		ObservedAt:     timestamppb.New(s.ObservedAt),
	}
}

func statsToProto(s domain.Stats) *pb.Stats {
	return &pb.Stats{
		TrackedItemId:   s.TrackedItemID,
		Current:         moneyToProto(s.Current),
		Min:             moneyToProto(s.Min),
		Max:             moneyToProto(s.Max),
		Avg:             moneyToProto(s.Avg),
		First:           moneyToProto(s.First),
		Trend:           trendToProto(s.Trend()),
		ChangePercent:   s.ChangePercent(),
		Samples:         int32(s.Samples),
		InStock:         s.InStock,
		FirstObservedAt: timestamppb.New(s.WindowFrom),
		LastObservedAt:  timestamppb.New(s.WindowTo),
	}
}

func trendToProto(t domain.TrendDirection) pb.Trend {
	switch t {
	case domain.TrendUp:
		return pb.Trend_TREND_UP
	case domain.TrendDown:
		return pb.Trend_TREND_DOWN
	case domain.TrendFlat:
		return pb.Trend_TREND_FLAT
	default:
		return pb.Trend_TREND_UNSPECIFIED
	}
}

func alertKindToProto(k domain.AlertKind) pb.AlertKind {
	switch k {
	case domain.AlertPriceDrop:
		return pb.AlertKind_ALERT_KIND_PRICE_DROP
	case domain.AlertBackInStock:
		return pb.AlertKind_ALERT_KIND_BACK_IN_STOCK
	case domain.AlertOutOfStock:
		return pb.AlertKind_ALERT_KIND_OUT_OF_STOCK
	case domain.AlertAllTimeLow:
		return pb.AlertKind_ALERT_KIND_ALL_TIME_LOW
	case domain.AlertScrapeDegraded:
		return pb.AlertKind_ALERT_KIND_SCRAPE_DEGRADED
	default:
		return pb.AlertKind_ALERT_KIND_UNSPECIFIED
	}
}

// pendingAlerts denormalises the alerts with the item and chat they belong to,
// so the notifier can send a message without a single extra round trip.
func pendingAlerts(item domain.TrackedItem, telegramID int64, alerts []domain.Alert) []*pb.PendingAlert {
	if len(alerts) == 0 {
		return nil
	}
	out := make([]*pb.PendingAlert, 0, len(alerts))
	for _, a := range alerts {
		out = append(out, &pb.PendingAlert{
			Kind:          alertKindToProto(a.Kind),
			TrackedItemId: a.TrackedItemID,
			UserId:        a.UserID,
			TelegramId:    telegramID,
			ItemTitle:     item.Title,
			ItemUrl:       item.URL,
			Price:         moneyToProto(a.Price),
			PreviousPrice: moneyPtrToProto(a.PreviousPrice),
			TargetPrice:   moneyPtrToProto(a.TargetPrice),
			OriginalPrice: moneyPtrToProto(a.OriginalPrice),
			DedupKey:      a.DedupKey,
		})
	}
	return out
}

func secondsToDuration(seconds int32) time.Duration {
	return time.Duration(seconds) * time.Second
}
