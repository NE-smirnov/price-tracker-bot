// Package scraper fetches product pages and extracts price and availability.
package scraper

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

// ErrNoPrice means the page was fetched but no price could be read from it.
// It is a normal outcome, not a bug: shops redesign, and an item can be
// delisted. The caller reports it to core as a scrape failure.
var ErrNoPrice = errors.New("no price found on the page")

// ErrBlocked means the shop answered, but with an anti-bot challenge instead of
// the product page. It is distinguished from ErrNoPrice because the fix is
// different: a challenge means "back off", a missing price means "the page
// changed".
var ErrBlocked = errors.New("blocked by the shop's anti-bot protection")

// minorUnitExponent holds the ISO-4217 exponent for currencies that are not
// two-decimal. Money is stored in minor units, so getting this wrong would
// silently multiply or divide a price by 100.
var minorUnitExponent = map[domain.Currency]int{
	"JPY": 0, "KRW": 0, "VND": 0, "CLP": 0, "ISK": 0, "PYG": 0,
	"RWF": 0, "UGX": 0, "VUV": 0, "XAF": 0, "XOF": 0, "XPF": 0, "DJF": 0, "GNF": 0, "KMF": 0,
	"BHD": 3, "IQD": 3, "JOD": 3, "KWD": 3, "LYD": 3, "OMR": 3, "TND": 3,
}

// MinorUnits returns how many decimal places a currency has.
func MinorUnits(c domain.Currency) int {
	if exp, ok := minorUnitExponent[c]; ok {
		return exp
	}
	return 2
}

// currencyBySymbol maps the symbols and words shops actually print next to a
// price onto ISO codes. Longer keys are matched first so "TL" does not shadow
// nothing and "руб" is not cut down to something else.
var currencyBySymbol = []struct {
	token    string
	currency domain.Currency
}{
	{"руб", "RUB"}, {"₽", "RUB"}, {"rub", "RUB"},
	{"₺", "TRY"}, {"try", "TRY"}, {"tl", "TRY"},
	{"€", "EUR"}, {"eur", "EUR"},
	{"£", "GBP"}, {"gbp", "GBP"},
	{"¥", "JPY"}, {"jpy", "JPY"},
	{"₸", "KZT"}, {"kzt", "KZT"},
	{"₴", "UAH"}, {"uah", "UAH"},
	{"us$", "USD"}, {"usd", "USD"}, {"$", "USD"},
}

// DetectCurrency guesses the currency from a price string or a currency label.
// It returns false when nothing recognisable is present; the caller must then
// treat the price as unusable rather than assume a default, because assuming
// the wrong currency is how a threshold fires at the wrong number.
func DetectCurrency(s string) (domain.Currency, bool) {
	trimmed := strings.TrimSpace(s)

	// An explicit ISO code is the common case in structured data.
	if len(trimmed) == 3 && isAllLetters(trimmed) {
		return domain.Currency(strings.ToUpper(trimmed)), true
	}

	lower := strings.ToLower(trimmed)
	for _, entry := range currencyBySymbol {
		if strings.Contains(lower, entry.token) {
			return entry.currency, true
		}
	}
	return "", false
}

func isAllLetters(s string) bool {
	for _, r := range s {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return s != ""
}

// ParseAmount converts a human-formatted price into minor units of the given
// currency.
//
// Shops write the same number as "1 299,00", "1,299.00", "1.299" and "1299";
// the separator meaning is decided by position, not by locale guessing, because
// the page rarely says which locale it used. A separator counts as decimal only
// when it is the last one and is followed by one or two digits.
func ParseAmount(raw string, currency domain.Currency) (int64, error) {
	cleaned := keepNumeric(raw)
	if cleaned == "" {
		return 0, fmt.Errorf("%w: %q has no digits", ErrNoPrice, raw)
	}

	exp := MinorUnits(currency)

	intPart, fracPart, err := splitDecimal(cleaned, exp)
	if err != nil {
		return 0, err
	}

	// Pad or round the fraction to the currency's own precision. Rounding half
	// up matches what a shop shows when it prints more digits than it charges.
	switch {
	case len(fracPart) < exp:
		fracPart += strings.Repeat("0", exp-len(fracPart))
	case len(fracPart) > exp:
		roundUp := fracPart[exp] >= '5'
		fracPart = fracPart[:exp]
		if roundUp {
			return roundedUp(intPart, fracPart)
		}
	}

	value, err := strconv.ParseInt(intPart+fracPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q is not a number", ErrNoPrice, raw)
	}
	if value <= 0 {
		return 0, fmt.Errorf("%w: %q is not a positive price", ErrNoPrice, raw)
	}
	return value, nil
}

// roundedUp adds one minor unit; the carry across the fraction boundary is
// handled by the addition itself because the value is already in minor units.
func roundedUp(intPart, fracPart string) (int64, error) {
	value, err := strconv.ParseInt(intPart+fracPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q%q is not a number", ErrNoPrice, intPart, fracPart)
	}
	return value + 1, nil
}

// keepNumeric drops currency symbols, letters and thousands spaces, keeping only
// digits and the two possible separators.
func keepNumeric(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ',' || r == '.':
			b.WriteRune(r)
		default:
			// Spaces, NBSP, narrow NBSP, apostrophes and letters are separators
			// or noise; none of them carry numeric meaning.
		}
	}
	return b.String()
}

// splitDecimal decides which separator is the decimal point.
//
// The page rarely states its locale, so the decision uses position plus the
// currency's own precision. Three digits after the last separator are genuinely
// ambiguous — "1.299" is 1299 lira in Turkey and 1.299 dinars in Kuwait — and
// are resolved as grouping unless the currency really has three decimals. For a
// zero-decimal currency any separator is grouping by definition.
func splitDecimal(cleaned string, exp int) (intPart, fracPart string, err error) {
	lastComma := strings.LastIndex(cleaned, ",")
	lastDot := strings.LastIndex(cleaned, ".")

	sep := -1
	switch {
	case lastComma == -1 && lastDot == -1:
		return cleaned, "", nil
	case lastComma > lastDot:
		sep = lastComma
	default:
		sep = lastDot
	}

	digitsAfter := len(cleaned) - sep - 1
	isDecimal := true
	switch {
	case digitsAfter == 0:
		isDecimal = false
	case exp == 0:
		// A zero-decimal currency has no fraction to print, but shops sometimes
		// print one anyway. One or two trailing digits are that fraction and get
		// rounded away; three are grouping, as in "12,500 ¥".
		isDecimal = digitsAfter <= 2
	case digitsAfter == 3:
		isDecimal = exp == 3
	case digitsAfter > 3:
		isDecimal = false
	}

	if !isDecimal {
		stripped := strings.NewReplacer(",", "", ".", "").Replace(cleaned)
		if stripped == "" {
			return "", "", fmt.Errorf("%w: %q", ErrNoPrice, cleaned)
		}
		return stripped, "", nil
	}

	intPart = strings.NewReplacer(",", "", ".", "").Replace(cleaned[:sep])
	fracPart = cleaned[sep+1:]
	if intPart == "" {
		intPart = "0"
	}
	if !isAllDigits(fracPart) {
		return "", "", fmt.Errorf("%w: %q", ErrNoPrice, cleaned)
	}
	return intPart, fracPart, nil
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ParseMoney reads a price and its currency out of a single string, e.g.
// "1 299,00 ₽". The currency hint is used when the string itself has no symbol.
func ParseMoney(raw string, hint domain.Currency) (domain.Money, error) {
	currency := hint
	if detected, ok := DetectCurrency(raw); ok {
		currency = detected
	}
	if currency == "" {
		return domain.Money{}, fmt.Errorf("%w: no currency in %q and no hint", ErrNoPrice, raw)
	}

	amount, err := ParseAmount(raw, currency)
	if err != nil {
		return domain.Money{}, err
	}
	return domain.Money{Amount: amount, Currency: currency}, nil
}

// ParseDecimalAmount parses a machine-readable price, where "." is the decimal
// separator and grouping is not allowed.
//
// schema.org requires this form, so structured data must not go through the
// locale heuristic: there "10.123" means ten and a bit, while the heuristic
// would read it as ten thousand.
func ParseDecimalAmount(raw string, currency domain.Currency) (int64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, fmt.Errorf("%w: empty value", ErrNoPrice)
	}

	intPart, fracPart, _ := strings.Cut(trimmed, ".")
	if !isAllDigits(intPart) || (fracPart != "" && !isAllDigits(fracPart)) {
		return 0, fmt.Errorf("%w: %q is not a plain decimal", ErrNoPrice, raw)
	}

	exp := MinorUnits(currency)
	switch {
	case len(fracPart) < exp:
		fracPart += strings.Repeat("0", exp-len(fracPart))
	case len(fracPart) > exp:
		roundUp := fracPart[exp] >= '5'
		fracPart = fracPart[:exp]
		if roundUp {
			return roundedUp(intPart, fracPart)
		}
	}

	value, err := strconv.ParseInt(intPart+fracPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q is not a number", ErrNoPrice, raw)
	}
	if value <= 0 {
		return 0, fmt.Errorf("%w: %q is not a positive price", ErrNoPrice, raw)
	}
	return value, nil
}
