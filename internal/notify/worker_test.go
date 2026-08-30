package notify

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
	"github.com/NE-smirnov/price-tracker-bot/internal/platform/queue"
)

// memQueue is an in-memory stand-in for the Redis list.
type memQueue struct {
	mu    sync.Mutex
	items []Alert
}

func (q *memQueue) Push(_ context.Context, payload any) error {
	alert, ok := payload.(Alert)
	if !ok {
		return errors.New("unexpected payload type")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append(q.items, alert)
	return nil
}

func (q *memQueue) Pop(_ context.Context, _ time.Duration, dst any) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return queue.ErrEmpty
	}
	target, ok := dst.(*Alert)
	if !ok {
		return errors.New("unexpected destination type")
	}
	*target = q.items[0]
	q.items = q.items[1:]
	return nil
}

func (q *memQueue) depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// memClaimer is an in-memory deduplication store.
type memClaimer struct {
	mu   sync.Mutex
	seen map[string]bool
	fail error
}

func newMemClaimer() *memClaimer { return &memClaimer{seen: map[string]bool{}} }

func (c *memClaimer) Claim(_ context.Context, key string, _ time.Duration) (bool, error) {
	if c.fail != nil {
		return false, c.fail
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen[key] {
		return false, nil
	}
	c.seen[key] = true
	return true, nil
}

func (c *memClaimer) Release(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.seen, key)
	return nil
}

// recordingSender captures what would have been sent and can be told to fail.
type recordingSender struct {
	mu   sync.Mutex
	sent []string
	// errs is consumed one per call; a shorter slice means later calls succeed.
	errs []error
	call int
}

func (s *recordingSender) Send(_ context.Context, chatID int64, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.call++
	if s.call <= len(s.errs) {
		if err := s.errs[s.call-1]; err != nil {
			return err
		}
	}
	s.sent = append(s.sent, text)
	_ = chatID
	return nil
}

func (s *recordingSender) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}

func testAlert() Alert {
	price := domain.Money{Amount: 149900, Currency: "RUB"}
	previous := domain.Money{Amount: 199900, Currency: "RUB"}
	target := domain.Money{Amount: 150000, Currency: "RUB"}
	return Alert{
		Kind:          KindPriceDrop,
		ItemID:        "item-1",
		UserID:        "user-1",
		TelegramID:    42,
		Title:         "Sony WH-1000XM5",
		URL:           "https://www.wildberries.ru/catalog/1/detail.aspx",
		Price:         &price,
		PreviousPrice: &previous,
		TargetPrice:   &target,
		DedupKey:      "item-1:price_drop:149900",
		RaisedAt:      time.Now(),
	}
}

// newTestWorker builds a worker with no waiting, so retries are instant.
func newTestWorker(source Source, sender Sender, claimer Claimer, attempts int) *Worker {
	return NewWorker(WorkerOptions{
		Source:       source,
		Sender:       sender,
		Claimer:      claimer,
		Log:          slog.New(slog.DiscardHandler),
		Workers:      1,
		PopTimeout:   time.Millisecond,
		RequeueDelay: time.Nanosecond,
		MaxAttempts:  attempts,
	})
}

func TestWorkerDeliversAnAlert(t *testing.T) {
	q := &memQueue{}
	alert := testAlert()
	if err := q.Push(context.Background(), alert); err != nil {
		t.Fatalf("push: %v", err)
	}
	sender := &recordingSender{}
	worker := newTestWorker(q, sender, newMemClaimer(), 3)

	worker.deliver(context.Background(), alert)

	messages := sender.messages()
	if len(messages) != 1 {
		t.Fatalf("sent %d messages, want 1", len(messages))
	}
	// The message has to carry the numbers the user reacts to.
	for _, want := range []string{"1499.00 RUB", "1999.00 RUB", "Sony WH-1000XM5", "−25.0%"} {
		if !strings.Contains(messages[0], want) {
			t.Fatalf("message %q does not contain %q", messages[0], want)
		}
	}
}

func TestWorkerSendsAnAlertOnlyOnce(t *testing.T) {
	// The queue is at-least-once, so the same alert can be read twice after a
	// restart. Telling the user twice about one price drop reads as a bug.
	alert := testAlert()
	sender := &recordingSender{}
	worker := newTestWorker(&memQueue{}, sender, newMemClaimer(), 3)

	worker.deliver(context.Background(), alert)
	worker.deliver(context.Background(), alert)

	if got := len(sender.messages()); got != 1 {
		t.Fatalf("sent %d messages, want exactly 1", got)
	}
}

func TestWorkerRequeuesARetryableFailure(t *testing.T) {
	q := &memQueue{}
	sender := &recordingSender{errs: []error{ErrRetryable}}
	claimer := newMemClaimer()
	worker := newTestWorker(q, sender, claimer, 3)

	alert := testAlert()
	worker.deliver(context.Background(), alert)

	if q.depth() != 1 {
		t.Fatalf("queue depth = %d, want the alert back on the queue", q.depth())
	}
	// The claim must have been released, or the retry would silently drop.
	var requeued Alert
	if err := q.Pop(context.Background(), 0, &requeued); err != nil {
		t.Fatalf("pop: %v", err)
	}
	worker.deliver(context.Background(), requeued)
	if got := len(sender.messages()); got != 1 {
		t.Fatalf("sent %d messages, want the retry to succeed", got)
	}
}

func TestWorkerGivesUpAfterMaxAttempts(t *testing.T) {
	q := &memQueue{}
	sender := &recordingSender{errs: []error{ErrRetryable, ErrRetryable, ErrRetryable, ErrRetryable}}
	worker := newTestWorker(q, sender, newMemClaimer(), 2)

	alert := testAlert()
	worker.deliver(context.Background(), alert)
	var second Alert
	if err := q.Pop(context.Background(), 0, &second); err != nil {
		t.Fatalf("pop: %v", err)
	}
	worker.deliver(context.Background(), second)

	// Second failure hits the cap, so the alert stops circulating.
	if q.depth() != 0 {
		t.Fatalf("queue depth = %d, want the alert dropped after the cap", q.depth())
	}
	if got := len(sender.messages()); got != 0 {
		t.Fatalf("sent %d messages, want none", got)
	}
}

func TestWorkerDropsAPermanentFailure(t *testing.T) {
	q := &memQueue{}
	sender := &recordingSender{errs: []error{errors.New("forbidden: bot was blocked by the user")}}
	worker := newTestWorker(q, sender, newMemClaimer(), 3)

	worker.deliver(context.Background(), testAlert())

	if q.depth() != 0 {
		t.Fatalf("queue depth = %d, want a permanent failure dropped", q.depth())
	}
}

func TestWorkerSendsWhenDeduplicationIsBroken(t *testing.T) {
	// A Redis outage must not silence alerts; duplicates are the lesser evil.
	claimer := newMemClaimer()
	claimer.fail = errors.New("redis is down")
	sender := &recordingSender{}
	worker := newTestWorker(&memQueue{}, sender, claimer, 3)

	worker.deliver(context.Background(), testAlert())

	if got := len(sender.messages()); got != 1 {
		t.Fatalf("sent %d messages, want the alert to go out anyway", got)
	}
}

func TestWorkerDropsAnAlertWithNoRecipient(t *testing.T) {
	sender := &recordingSender{}
	worker := newTestWorker(&memQueue{}, sender, newMemClaimer(), 3)

	alert := testAlert()
	alert.TelegramID = 0
	worker.deliver(context.Background(), alert)

	if got := len(sender.messages()); got != 0 {
		t.Fatalf("sent %d messages, want none without a chat id", got)
	}
}

func TestWorkerRunStopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	worker := newTestWorker(&memQueue{}, &recordingSender{}, newMemClaimer(), 3)

	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestRenderCoversEveryKind(t *testing.T) {
	// A missing case would send an empty or misleading message, and the user sees
	// these strings directly.
	for _, kind := range []Kind{
		KindPriceDrop, KindAllTimeLow, KindBackInStock, KindOutOfStock, KindScrapeDegraded,
	} {
		alert := testAlert()
		alert.Kind = kind
		text := Render(alert)
		if len(text) < 20 || !strings.Contains(text, "Sony WH-1000XM5") {
			t.Fatalf("Render(%s) = %q, want a message naming the item", kind, text)
		}
	}
}

func TestRenderEscapesShopTitles(t *testing.T) {
	// Shop titles contain angle brackets and ampersands; an unescaped one breaks
	// Telegram's HTML parser and the whole message fails to send.
	alert := testAlert()
	alert.Title = `Кабель <USB-C> & "быстрый"`
	text := Render(alert)
	if strings.Contains(text, "<USB-C>") {
		t.Fatalf("Render did not escape the title: %q", text)
	}
	if !strings.Contains(text, "&lt;USB-C&gt;") || !strings.Contains(text, "&amp;") {
		t.Fatalf("Render escaped the title incorrectly: %q", text)
	}
}

func TestRenderTruncatesLongTitles(t *testing.T) {
	alert := testAlert()
	alert.Title = strings.Repeat("Наушники ", 40)
	text := Render(alert)
	if !strings.Contains(text, "…") {
		t.Fatalf("Render did not truncate a long title: %q", text)
	}
}

func TestRenderSkipsThePercentageAcrossCurrencies(t *testing.T) {
	// Comparing 1499 RUB against 19.99 USD would produce a meaningless number.
	alert := testAlert()
	previous := domain.Money{Amount: 1999, Currency: "USD"}
	alert.PreviousPrice = &previous
	text := Render(alert)
	if strings.Contains(text, "%") {
		t.Fatalf("Render compared prices in different currencies: %q", text)
	}
}

func TestRenderShowsTheShopPriceAlongsideTheConvertedOne(t *testing.T) {
	// The alert is judged in the user's currency, but the number they will pay at
	// checkout is the shop's. Hiding either one makes the message misleading.
	alert := testAlert()
	price := domain.Money{Amount: 2068408, Currency: "TRY"}
	alert.Price = &price
	target := domain.Money{Amount: 9000000, Currency: "TRY"}
	alert.TargetPrice = &target
	original := domain.Money{Amount: 3682900, Currency: "RUB"}
	alert.OriginalPrice = &original
	alert.PreviousPrice = nil

	text := Render(alert)
	if !strings.Contains(text, "20684.08 TRY") {
		t.Fatalf("Render dropped the converted price: %q", text)
	}
	if !strings.Contains(text, "36829.00 RUB") {
		t.Fatalf("Render dropped the shop price: %q", text)
	}
}

func TestRenderOmitsTheShopPriceWhenItIsTheSame(t *testing.T) {
	alert := testAlert()
	alert.OriginalPrice = nil
	if strings.Contains(Render(alert), "В магазине") {
		t.Fatalf("Render invented a shop price line: %q", Render(alert))
	}
}
