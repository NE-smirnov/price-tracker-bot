package domain_test

import (
	"errors"
	"testing"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

func TestParseMoney(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		fallback domain.Currency
		want     domain.Money
		wantErr  bool
	}{
		{name: "dot decimals with code", input: "19.99 USD", want: domain.Money{Amount: 1999, Currency: "USD"}},
		{name: "comma decimals", input: "19,99 EUR", want: domain.Money{Amount: 1999, Currency: "EUR"}},
		{name: "lowercase code", input: "1500 try", want: domain.Money{Amount: 150000, Currency: "TRY"}},
		{name: "integer with fallback", input: "4990", fallback: "RUB", want: domain.Money{Amount: 499000, Currency: "RUB"}},
		{name: "code overrides fallback", input: "10 EUR", fallback: "USD", want: domain.Money{Amount: 1000, Currency: "EUR"}},
		{name: "space grouping", input: "1 234.50 USD", want: domain.Money{Amount: 123450, Currency: "USD"}},
		{name: "turkish notation", input: "1.234,50 TRY", want: domain.Money{Amount: 123450, Currency: "TRY"}},
		{name: "english notation", input: "1,234.50 USD", want: domain.Money{Amount: 123450, Currency: "USD"}},
		{name: "repeated grouping separator", input: "1.234.567 TRY", want: domain.Money{Amount: 123456700, Currency: "TRY"}},
		{name: "single decimal digit", input: "19.9 USD", want: domain.Money{Amount: 1990, Currency: "USD"}},
		{name: "rounds half up", input: "19.9951 USD", want: domain.Money{Amount: 2000, Currency: "USD"}},
		{name: "truncates below half", input: "19.9949 USD", want: domain.Money{Amount: 1999, Currency: "USD"}},
		{name: "leading zero means decimals", input: "0.500 USD", want: domain.Money{Amount: 50, Currency: "USD"}},
		{name: "long head means decimals", input: "1234.567 USD", want: domain.Money{Amount: 123457, Currency: "USD"}},

		{name: "empty", input: "   ", wantErr: true},
		{name: "no currency at all", input: "19.99", wantErr: true},
		{name: "bad currency", input: "19.99 DOLLARS", wantErr: true},
		{name: "negative", input: "-5 USD", wantErr: true},
		{name: "not a number", input: "cheap USD", wantErr: true},
	}

	// "1.234" is 1234 in tr-TR and 1.23 in en-US; the parser must refuse to guess.
	t.Run("ambiguous separator is rejected", func(t *testing.T) {
		t.Parallel()

		_, err := domain.ParseMoney("1.234 TRY", "")
		if !errors.Is(err, domain.ErrAmbiguousSeparator) {
			t.Fatalf("error = %v, want ErrAmbiguousSeparator", err)
		}
		if !errors.Is(err, domain.ErrInvalidMoney) {
			t.Fatalf("error %v must also wrap ErrInvalidMoney", err)
		}
	})

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := domain.ParseMoney(tc.input, tc.fallback)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseMoney(%q) = %v, want error", tc.input, got)
				}
				if !errors.Is(err, domain.ErrInvalidMoney) {
					t.Fatalf("error %v does not wrap ErrInvalidMoney", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseMoney(%q) unexpected error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("ParseMoney(%q) = %+v, want %+v", tc.input, got, tc.want)
			}
		})
	}
}

func TestMoneyString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		money domain.Money
		want  string
	}{
		{domain.Money{Amount: 1999, Currency: "USD"}, "19.99 USD"},
		{domain.Money{Amount: 100, Currency: "EUR"}, "1.00 EUR"},
		{domain.Money{Amount: 5, Currency: "TRY"}, "0.05 TRY"},
		{domain.Money{Amount: -250, Currency: "RUB"}, "-2.50 RUB"},
	}
	for _, tc := range tests {
		if got := tc.money.String(); got != tc.want {
			t.Errorf("Money%+v.String() = %q, want %q", tc.money, got, tc.want)
		}
	}
}

func TestMoneyLessThanRejectsCurrencyMismatch(t *testing.T) {
	t.Parallel()

	usd := domain.Money{Amount: 100, Currency: "USD"}
	eur := domain.Money{Amount: 200, Currency: "EUR"}

	if _, err := usd.LessThan(eur); err == nil {
		t.Fatal("comparing USD with EUR must fail instead of returning a wrong answer")
	}

	less, err := usd.LessThan(domain.Money{Amount: 200, Currency: "USD"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !less {
		t.Fatal("1.00 USD must be less than 2.00 USD")
	}
}

func TestValidCurrency(t *testing.T) {
	t.Parallel()

	valid := []domain.Currency{"USD", "TRY", "RUB"}
	invalid := []domain.Currency{"", "US", "USDT", "usd", "U5D", "U D"}

	for _, c := range valid {
		if !domain.ValidCurrency(c) {
			t.Errorf("ValidCurrency(%q) = false, want true", c)
		}
	}
	for _, c := range invalid {
		if domain.ValidCurrency(c) {
			t.Errorf("ValidCurrency(%q) = true, want false", c)
		}
	}
}
