package core

import (
	"fmt"
	"time"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

// scanner is satisfied by both pgx.Row and pgx.Rows, so the same mapping code
// serves single-row and multi-row queries.
type scanner interface {
	Scan(dest ...any) error
}

// scanItem maps the itemColumns projection, which includes the latest snapshot.
func scanItem(s scanner) (domain.TrackedItem, error) {
	var (
		item           domain.TrackedItem
		intervalSecs   int32
		targetAmount   *int64
		targetCurrency *string

		snapID         *string
		snapAmount     *int64
		snapCurrency   *string
		convAmount     *int64
		convCurrency   *string
		snapInStock    *bool
		snapObservedAt *time.Time
	)

	if err := s.Scan(
		&item.ID, &item.UserID, &item.URL, &item.Title,
		&targetAmount, &targetCurrency,
		&intervalSecs, &item.Active,
		&item.NextCheckAt, &item.FailureStreak, &item.LastError,
		&item.CreatedAt, &item.UpdatedAt,
		&snapID, &snapAmount, &snapCurrency, &convAmount, &convCurrency,
		&snapInStock, &snapObservedAt,
	); err != nil {
		return domain.TrackedItem{}, err
	}

	item.CheckInterval = time.Duration(intervalSecs) * time.Second
	item.TargetPrice = money(targetAmount, targetCurrency)

	if snapID != nil && snapAmount != nil && snapCurrency != nil &&
		snapInStock != nil && snapObservedAt != nil {
		item.LastSnapshot = &domain.PriceSnapshot{
			ID:            *snapID,
			TrackedItemID: item.ID,
			Price:         domain.Money{Amount: *snapAmount, Currency: domain.Currency(*snapCurrency)},
			Converted:     money(convAmount, convCurrency),
			InStock:       *snapInStock,
			ObservedAt:    *snapObservedAt,
		}
		item.LastCheckedAt = snapObservedAt
	}
	return item, nil
}

// scanItemNoSnapshot maps the itemColumnsNoSnapshot projection.
func scanItemNoSnapshot(s scanner) (domain.TrackedItem, error) {
	var (
		item           domain.TrackedItem
		intervalSecs   int32
		targetAmount   *int64
		targetCurrency *string
	)
	if err := s.Scan(
		&item.ID, &item.UserID, &item.URL, &item.Title,
		&targetAmount, &targetCurrency,
		&intervalSecs, &item.Active,
		&item.NextCheckAt, &item.FailureStreak, &item.LastError,
		&item.CreatedAt, &item.UpdatedAt,
	); err != nil {
		return domain.TrackedItem{}, err
	}
	item.CheckInterval = time.Duration(intervalSecs) * time.Second
	item.TargetPrice = money(targetAmount, targetCurrency)
	return item, nil
}

func scanSnapshot(s scanner) (domain.PriceSnapshot, error) {
	var (
		snap         domain.PriceSnapshot
		currency     string
		convAmount   *int64
		convCurrency *string
	)
	if err := s.Scan(
		&snap.ID, &snap.TrackedItemID, &snap.Price.Amount, &currency,
		&convAmount, &convCurrency, &snap.InStock, &snap.ObservedAt,
	); err != nil {
		return domain.PriceSnapshot{}, err
	}
	snap.Price.Currency = domain.Currency(currency)
	snap.Converted = money(convAmount, convCurrency)
	return snap, nil
}

// money rebuilds an optional amount/currency column pair. The schema guarantees
// both columns are set or both are NULL, so a half-filled pair means the
// invariant was broken and is reported rather than silently defaulted.
func money(amount *int64, currency *string) *domain.Money {
	if amount == nil || currency == nil {
		return nil
	}
	return &domain.Money{Amount: *amount, Currency: domain.Currency(*currency)}
}

// splitMoney is the inverse of money, for building query arguments.
func splitMoney(m *domain.Money) (*int64, *string) {
	if m == nil {
		return nil, nil
	}
	a := m.Amount
	c := string(m.Currency)
	return &a, &c
}

func mustPositive(name string, v int64) error {
	if v < 0 {
		return fmt.Errorf("%w: %s must not be negative", domain.ErrValidation, name)
	}
	return nil
}
