package bot

import (
	"strings"
	"testing"
	"time"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

// A product title with <b> or & must not be able to break the HTML message,
// which would make Telegram reject the whole send.
func TestRenderItemListEscapesUserContent(t *testing.T) {
	t.Parallel()

	items := []domain.TrackedItem{{
		ID:            "1",
		Title:         `<b>hack</b> & "quotes"`,
		URL:           "https://example.com/p?a=1&b=2",
		CheckInterval: time.Hour,
		Active:        true,
		CreatedAt:     time.Now(),
	}}

	out := renderItemList(items, "USD")

	if strings.Contains(out, "<b>hack</b>") {
		t.Fatal("raw user HTML leaked into the message")
	}
	if !strings.Contains(out, "&lt;b&gt;hack&lt;/b&gt;") {
		t.Fatalf("title not escaped, got:\n%s", out)
	}
	if !strings.Contains(out, "a=1&amp;b=2") {
		t.Fatalf("ampersand in URL not escaped, got:\n%s", out)
	}
}

func TestSparkline(t *testing.T) {
	t.Parallel()

	history := make([]domain.PriceSnapshot, 0, 48)
	for i := range 48 {
		history = append(history, domain.PriceSnapshot{
			Price: domain.Money{Amount: int64(1000 + i*10), Currency: "USD"},
		})
	}

	got := sparkline(history, 24)
	if n := len([]rune(got)); n != 24 {
		t.Fatalf("sparkline width = %d runes, want 24", n)
	}
	// A monotonically rising series must start at the lowest bar and end at the highest.
	runes := []rune(got)
	if runes[0] != sparkRunes[0] || runes[len(runes)-1] != sparkRunes[len(sparkRunes)-1] {
		t.Fatalf("rising series rendered as %q", got)
	}

	if got := sparkline(history[:1], 24); got != "" {
		t.Fatalf("sparkline of a single point = %q, want empty", got)
	}
}

func TestSparklineFlatSeries(t *testing.T) {
	t.Parallel()

	history := make([]domain.PriceSnapshot, 10)
	for i := range history {
		history[i].Price = domain.Money{Amount: 500, Currency: "USD"}
	}
	// A flat series must not divide by zero.
	got := sparkline(history, 8)
	if len([]rune(got)) != 8 {
		t.Fatalf("flat sparkline = %q", got)
	}
}

func TestHumanDuration(t *testing.T) {
	t.Parallel()

	tests := map[time.Duration]string{
		0:                  "—",
		15 * time.Minute:   "15 мин",
		time.Hour:          "1 ч",
		90 * time.Minute:   "1 ч 30 мин",
		24 * time.Hour:     "1 д",
		36 * time.Hour:     "1 д 12 ч",
		7 * 24 * time.Hour: "7 д",
	}
	for d, want := range tests {
		if got := humanDuration(d); got != want {
			t.Errorf("humanDuration(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestCallbackDataRoundTrip(t *testing.T) {
	t.Parallel()

	data := cbData(actRemove, "0b7e4a4c-4d5f-4a1e-9f1e-1a2b3c4d5e6f")
	action, arg, ok := parseCallback(data)
	if !ok {
		t.Fatalf("parseCallback(%q) failed", data)
	}
	if action != actRemove || arg != "0b7e4a4c-4d5f-4a1e-9f1e-1a2b3c4d5e6f" {
		t.Fatalf("round trip gave action=%q arg=%q", action, arg)
	}

	// Telegram rejects callback_data longer than 64 bytes: degrade to noop.
	long := cbData(actRemove, strings.Repeat("x", 100))
	if long != cbPrefix+actNoop {
		t.Fatalf("oversized callback data = %q, want noop", long)
	}

	if _, _, ok := parseCallback("garbage"); ok {
		t.Fatal("parseCallback accepted data without the version prefix")
	}
}

func TestRenderStatsContainsKeyNumbers(t *testing.T) {
	t.Parallel()

	item := domain.TrackedItem{Title: "Laptop", URL: "https://example.com/p/1"}
	history := []domain.PriceSnapshot{
		{Price: domain.Money{Amount: 100000, Currency: "USD"}, ObservedAt: time.Now().Add(-48 * time.Hour)},
		{Price: domain.Money{Amount: 90000, Currency: "USD"}, ObservedAt: time.Now(), InStock: true},
	}
	st := AggregateStats(item.ID, history)

	out := renderStats(item, st, history)
	for _, want := range []string{"900.00 USD", "1000.00 USD", "↓ вниз", "-10.0%"} {
		if !strings.Contains(out, want) {
			t.Errorf("stats message missing %q, got:\n%s", want, out)
		}
	}
}
