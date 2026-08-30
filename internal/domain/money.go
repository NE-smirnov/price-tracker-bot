package domain

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrInvalidMoney is returned when a money literal cannot be parsed.
var ErrInvalidMoney = errors.New("invalid money value")

// ErrAmbiguousSeparator is returned for input where a single separator followed
// by exactly three digits could mean either decimals or digit grouping:
// "1.234" is 1234 for a Turkish user and 1.23 for an American one. Guessing
// would silently set a 1000x wrong alert threshold, so the caller must ask.
var ErrAmbiguousSeparator = errors.New("ambiguous decimal separator")

// Currency is an ISO-4217 alphabetic code, always upper case.
type Currency string

// Money is an exact monetary amount stored in minor units (cents, kuruş, ...).
// Prices are never represented as float64: rounding drift in a price tracker
// would silently corrupt alert thresholds and trend statistics.
type Money struct {
	// Amount is the value in minor units, e.g. 1999 for 19.99 USD.
	//
	// The JSON names are given explicitly because this type crosses the queue
	// between the scraper and the notifier: a field rename in Go must not silently
	// change the wire format of alerts already sitting in Redis.
	Amount int64 `json:"amount"`
	// Currency is the ISO-4217 code, e.g. "USD".
	Currency Currency `json:"currency"`
}

// MinorUnits is how many decimal digits a currency prints, per ISO 4217.
//
// It lives in domain because every layer needs the same answer: the bot when it
// renders a price, the scraper when it parses one off a page, and the converter
// when it moves an amount between two currencies with different precision.
// Getting it wrong shifts a price by a factor of ten or a hundred.
func MinorUnits(c Currency) int {
	switch c {
	// Currencies with no subunit in circulation.
	case "JPY", "KRW", "VND", "CLP", "ISK", "HUF", "TWD", "XAF", "XOF", "UGX", "RWF":
		return 0
	// Currencies with a thousandth subunit.
	case "KWD", "BHD", "OMR", "JOD", "TND", "LYD", "IQD":
		return 3
	default:
		return 2
	}
}

// scale returns 10^MinorUnits as an integer multiplier.
func scale(c Currency) int64 {
	value := int64(1)
	for i := 0; i < MinorUnits(c); i++ {
		value *= 10
	}
	return value
}

// NewMoney builds a Money value from minor units.
func NewMoney(minorUnits int64, currency Currency) Money {
	return Money{Amount: minorUnits, Currency: NormalizeCurrency(string(currency))}
}

// NormalizeCurrency upper-cases and trims a currency code.
func NormalizeCurrency(code string) Currency {
	return Currency(strings.ToUpper(strings.TrimSpace(code)))
}

// ValidCurrency reports whether code looks like an ISO-4217 alphabetic code.
func ValidCurrency(code Currency) bool {
	if len(code) != 3 {
		return false
	}
	for _, r := range code {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// ParseMoney parses human input such as "19.99 USD", "1 234,50 TRY" or "3990".
// A currency is required unless fallback is non-empty.
func ParseMoney(input string, fallback Currency) (Money, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return Money{}, fmt.Errorf("%w: empty input", ErrInvalidMoney)
	}

	currency := NormalizeCurrency(string(fallback))

	// A trailing 3-letter code wins over the fallback: "19.99 usd".
	if fields := strings.Fields(s); len(fields) > 1 {
		last := NormalizeCurrency(fields[len(fields)-1])
		if ValidCurrency(last) {
			currency = last
			s = strings.Join(fields[:len(fields)-1], "")
		}
	}
	if !ValidCurrency(currency) {
		return Money{}, fmt.Errorf("%w: missing or bad currency code", ErrInvalidMoney)
	}

	// Drop digit-grouping whitespace and underscores: "1 234,50" -> "1234,50".
	s = strings.NewReplacer(" ", "", "\u00a0", "", "\u202f", "", "_", "").Replace(s)

	intPart, fracPart, err := splitDecimal(s)
	if err != nil {
		return Money{}, err
	}

	if intPart == "" {
		intPart = "0"
	}
	units, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return Money{}, fmt.Errorf("%w: %q", ErrInvalidMoney, input)
	}
	if units < 0 {
		return Money{}, fmt.Errorf("%w: negative price", ErrInvalidMoney)
	}

	exp := MinorUnits(currency)
	var minor int64
	roundUp := false
	switch {
	case len(fracPart) <= exp:
		// Pad the missing digits: "19.9" in a two-decimal currency is 19.90.
		padded := fracPart + strings.Repeat("0", exp-len(fracPart))
		if padded != "" {
			d, convErr := strconv.ParseInt(padded, 10, 64)
			if convErr != nil {
				return Money{}, fmt.Errorf("%w: %q", ErrInvalidMoney, input)
			}
			minor = d
		}
	default:
		// More digits than the currency has: keep the significant ones and round
		// half up on the first dropped digit.
		if exp > 0 {
			d, convErr := strconv.ParseInt(fracPart[:exp], 10, 64)
			if convErr != nil {
				return Money{}, fmt.Errorf("%w: %q", ErrInvalidMoney, input)
			}
			minor = d
		}
		roundUp = fracPart[exp] >= '5'
	}

	amount := units*scale(currency) + minor
	if roundUp {
		amount++
	}
	return Money{Amount: amount, Currency: currency}, nil
}

// splitDecimal separates the integer and fractional parts of a numeric literal,
// resolving '.' and ',' as either decimal separator or digit grouping.
func splitDecimal(s string) (intPart, fracPart string, err error) {
	dots := strings.Count(s, ".")
	commas := strings.Count(s, ",")

	switch {
	case dots == 0 && commas == 0:
		return s, "", nil

	case dots > 0 && commas > 0:
		// Mixed notation: whichever separator comes last is the decimal one,
		// e.g. "1.234,50" (tr) and "1,234.50" (en) both mean 1234.50.
		decSep := "."
		if strings.LastIndex(s, ",") > strings.LastIndex(s, ".") {
			decSep = ","
		}
		idx := strings.LastIndex(s, decSep)
		head, tail := s[:idx], s[idx+1:]
		return stripSeparators(head), tail, nil

	default:
		sep := "."
		count := dots
		if commas > 0 {
			sep, count = ",", commas
		}
		if count > 1 {
			// "1.234.567" can only be digit grouping.
			return stripSeparators(s), "", nil
		}
		idx := strings.Index(s, sep)
		head, tail := s[:idx], s[idx+1:]
		// Grouping is only plausible when the head is a single 1..3 digit group
		// without a leading zero: "19.995" may be 19995, but "1234.567" and
		// "0.500" can only be decimals.
		if len(tail) == 3 && len(head) >= 1 && len(head) <= 3 && head[0] != '0' {
			return "", "", fmt.Errorf("%w: %w: %q could mean %s%s or %s.%s",
				ErrInvalidMoney, ErrAmbiguousSeparator, s, head, tail, head, tail)
		}
		return head, tail, nil
	}
}

func stripSeparators(s string) string {
	return strings.NewReplacer(".", "", ",", "").Replace(s)
}

// String renders the amount with the precision its currency actually uses:
// "19.99 USD", "1250 JPY", "12.345 KWD".
func (m Money) String() string {
	sign := ""
	amount := m.Amount
	if amount < 0 {
		sign = "-"
		amount = -amount
	}

	exp := MinorUnits(m.Currency)
	if exp == 0 {
		// Printing "1250.00 JPY" would invent a subunit the currency does not
		// have; a yen price is a whole number.
		return fmt.Sprintf("%s%d %s", sign, amount, m.Currency)
	}
	factor := scale(m.Currency)
	return fmt.Sprintf("%s%d.%0*d %s", sign, amount/factor, exp, amount%factor, m.Currency)
}

// IsZero reports whether the amount is unset.
func (m Money) IsZero() bool { return m.Amount == 0 && m.Currency == "" }

// LessThan compares two amounts of the same currency.
// It returns an error instead of a silent wrong answer on a currency mismatch.
func (m Money) LessThan(other Money) (bool, error) {
	if m.Currency != other.Currency {
		return false, fmt.Errorf("%w: cannot compare %s with %s", ErrInvalidMoney, m.Currency, other.Currency)
	}
	return m.Amount < other.Amount, nil
}
