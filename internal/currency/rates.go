// Package currency converts money between currencies.
//
// It is the only component that talks to an external rate provider, so its
// rate limits, outages and caching stay out of the scrape loop. Rates are kept
// as integers scaled by 1e8: a rate is not money, and a float would make two
// services disagree on the last digit of a converted price.
package currency

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

// RateScale is the fixed-point scale of every rate in this package.
const RateScale = 100_000_000 // 1e8

// ErrUnsupportedCurrency means the provider publishes no rate for the pair. It
// is deliberately distinct from a transport failure: a caller should stop asking
// for an unsupported pair, but should retry a failed request.
var ErrUnsupportedCurrency = errors.New("currency: unsupported pair")

// Rate is one directed conversion factor.
type Rate struct {
	From   domain.Currency `json:"from"`
	To     domain.Currency `json:"to"`
	RateE8 int64           `json:"rate_e8"`
	AsOf   time.Time       `json:"as_of"`
	// Cached reports that the rate was served from the local cache. It is
	// returned to callers so a surprising conversion can be traced to a stale
	// table rather than to the arithmetic.
	Cached bool `json:"cached"`
}

// Table is a provider snapshot: every rate relative to one base currency.
type Table struct {
	Base  domain.Currency           `json:"base"`
	Rates map[domain.Currency]int64 `json:"rates"`
	AsOf  time.Time                 `json:"as_of"`
}

// Provider fetches a rate table from an external source.
type Provider interface {
	// Rates returns every rate the provider knows relative to base.
	Rates(ctx context.Context, base domain.Currency) (Table, error)
	// Name identifies the provider in logs and in the cache key, so switching
	// providers cannot serve rates cached from the previous one.
	Name() string
}

// cross derives the from→to rate from a table expressed in some other base.
//
// Deriving is what keeps this to a single provider request per hour: one USD
// table answers every pair the shops and users need, instead of one request per
// ordered pair.
func (t Table) cross(from, to domain.Currency) (int64, error) {
	if from == to {
		return RateScale, nil
	}

	fromRate, err := t.rateFromBase(from)
	if err != nil {
		return 0, err
	}
	toRate, err := t.rateFromBase(to)
	if err != nil {
		return 0, err
	}

	// rate(from→to) = rate(base→to) / rate(base→from), in 1e8 fixed point.
	numerator := new(big.Int).Mul(big.NewInt(toRate), big.NewInt(RateScale))
	result := new(big.Int)
	remainder := new(big.Int)
	result.QuoRem(numerator, big.NewInt(fromRate), remainder)

	// Round half up, so a derived rate is not systematically low.
	if new(big.Int).Lsh(remainder, 1).CmpAbs(big.NewInt(fromRate)) >= 0 {
		result.Add(result, big.NewInt(1))
	}
	if !result.IsInt64() || result.Sign() <= 0 {
		return 0, fmt.Errorf("%w: %s→%s is out of range", ErrUnsupportedCurrency, from, to)
	}
	return result.Int64(), nil
}

func (t Table) rateFromBase(c domain.Currency) (int64, error) {
	if c == t.Base {
		return RateScale, nil
	}
	rate, ok := t.Rates[c]
	if !ok || rate <= 0 {
		return 0, fmt.Errorf("%w: no rate for %s", ErrUnsupportedCurrency, c)
	}
	return rate, nil
}

// Apply converts an amount using this rate.
//
// The two currencies may have different numbers of decimals, so the amount is
// rescaled between minor units as part of the conversion; skipping that step
// would silently multiply a JPY price by a hundred. The result is rounded half
// up and must stay positive: a price that rounds to zero is not a price.
func (r Rate) Apply(amount int64) (int64, error) {
	if amount <= 0 {
		return 0, fmt.Errorf("currency: cannot convert a non-positive amount %d", amount)
	}
	if r.RateE8 <= 0 {
		return 0, fmt.Errorf("currency: invalid rate %d for %s→%s", r.RateE8, r.From, r.To)
	}

	fromExp := minorUnitExponent(r.From)
	toExp := minorUnitExponent(r.To)

	value := new(big.Int).Mul(big.NewInt(amount), big.NewInt(r.RateE8))
	divisor := new(big.Int).Mul(big.NewInt(RateScale), pow10(fromExp))
	value.Mul(value, pow10(toExp))

	quotient, remainder := new(big.Int).QuoRem(value, divisor, new(big.Int))
	if new(big.Int).Lsh(remainder, 1).Cmp(divisor) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() {
		return 0, fmt.Errorf("currency: converted amount overflows int64")
	}
	if quotient.Sign() <= 0 {
		return 0, fmt.Errorf("currency: %d %s rounds to zero in %s", amount, r.From, r.To)
	}
	return quotient.Int64(), nil
}

func pow10(exp int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(exp)), nil)
}

// minorUnitExponent is how many decimals a currency prints. The table lives in
// domain so that a converted amount means the same thing to every service.
func minorUnitExponent(c domain.Currency) int { return domain.MinorUnits(c) }
