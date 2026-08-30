package scraper

import (
	"errors"
	"testing"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

// A misread price is the worst failure this project can have: it does not crash,
// it just notifies the user about a number that was never on the page. So the
// parser is tested against the formats shops actually print, including the ones
// that are ambiguous.
func TestParseAmount(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		currency domain.Currency
		want     int64
	}{
		{"plain integer", "1299", "TRY", 129900},
		{"comma decimal", "1299,90", "TRY", 129990},
		{"dot decimal", "1299.90", "USD", 129990},
		{"space grouping with comma decimal", "1 299,90", "RUB", 129990},
		{"nbsp grouping", "1\u00a0299,90", "RUB", 129990},
		{"narrow nbsp grouping", "1\u202f299,90", "RUB", 129990},
		{"comma grouping with dot decimal", "1,299.90", "USD", 129990},
		{"dot grouping with comma decimal", "1.299,90", "TRY", 129990},
		{"dot grouping only", "1.299", "TRY", 129900},
		{"comma grouping only", "1,299", "USD", 129900},
		{"millions with dot grouping", "1.234.567", "RUB", 123456700},
		{"currency symbol before", "$1,234.56", "USD", 123456},
		{"currency symbol after", "1 234,56 ₽", "RUB", 123456},
		{"turkish lira suffix", "1.299,00 TL", "TRY", 129900},
		{"zero decimal currency", "12500", "JPY", 12500},
		{"zero decimal currency with grouping", "12,500", "JPY", 12500},
		{"three decimal currency", "12,345", "KWD", 12345},
		{"single decimal digit", "99,9", "USD", 9990},
		{"two decimals for a three-decimal currency pad", "12,34", "KWD", 12340},
		// Three digits after the separator are ambiguous; for a two-decimal
		// currency they are read as grouping, which is what shops mean.
		{"three digits are grouping for a two-decimal currency", "10.123", "USD", 1012300},
		// A zero-decimal currency has no fraction, so extra digits are rounded
		// away rather than shifting the amount by a factor of ten.
		{"zero decimal currency rounds a printed fraction", "1250,75", "JPY", 1251},
		{"zero decimal currency rounds a fraction down", "1250,25", "JPY", 1250},
		{"leading decimal", ",99", "USD", 99},
		{"noise around the number", "цена: 1 299 ₽ за штуку", "RUB", 129900},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseAmount(tc.raw, tc.currency)
			if err != nil {
				t.Fatalf("ParseAmount(%q, %s) returned error: %v", tc.raw, tc.currency, err)
			}
			if got != tc.want {
				t.Fatalf("ParseAmount(%q, %s) = %d, want %d", tc.raw, tc.currency, got, tc.want)
			}
		})
	}
}

func TestParseAmountRejects(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"letters only", "нет в наличии"},
		{"zero", "0"},
		{"zero with decimals", "0,00"},
		{"currency symbol only", "₽"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := ParseAmount(tc.raw, "RUB"); !errors.Is(err, ErrNoPrice) {
				t.Fatalf("ParseAmount(%q) = %d, %v; want ErrNoPrice", tc.raw, got, err)
			}
		})
	}
}

func TestDetectCurrency(t *testing.T) {
	tests := []struct {
		raw  string
		want domain.Currency
		ok   bool
	}{
		{"USD", "USD", true},
		{"usd", "USD", true},
		{"RUB", "RUB", true},
		{"1 299 ₽", "RUB", true},
		{"1 299 руб.", "RUB", true},
		{"1.299,00 TL", "TRY", true},
		{"1.299,00 ₺", "TRY", true},
		{"$19.99", "USD", true},
		{"€19,99", "EUR", true},
		{"£19.99", "GBP", true},
		{"12500 ₸", "KZT", true},
		{"1299", "", false},
		{"", "", false},
		{"нет цены", "", false},
	}

	for _, tc := range tests {
		got, ok := DetectCurrency(tc.raw)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("DetectCurrency(%q) = %q, %v; want %q, %v", tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseMoney(t *testing.T) {
	// A hint is used only when the string itself is silent about the currency;
	// an explicit symbol on the page always wins, because the hint comes from a
	// guess made elsewhere.
	got, err := ParseMoney("1 299,90 ₽", "USD")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Currency != "RUB" || got.Amount != 129990 {
		t.Fatalf("got %+v, want 129990 RUB", got)
	}

	got, err = ParseMoney("1299,90", "TRY")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Currency != "TRY" || got.Amount != 129990 {
		t.Fatalf("got %+v, want 129990 TRY", got)
	}

	if _, err = ParseMoney("1299,90", ""); !errors.Is(err, ErrNoPrice) {
		t.Fatalf("err = %v, want ErrNoPrice when neither the string nor the hint has a currency", err)
	}
}

func TestMinorUnits(t *testing.T) {
	for currency, want := range map[domain.Currency]int{
		"USD": 2, "RUB": 2, "TRY": 2, "EUR": 2,
		"JPY": 0, "KRW": 0,
		"KWD": 3, "TND": 3,
		"XYZ": 2, // unknown codes fall back to the common case
	} {
		if got := MinorUnits(currency); got != want {
			t.Fatalf("MinorUnits(%s) = %d, want %d", currency, got, want)
		}
	}
}
