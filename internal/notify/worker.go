package notify

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/NE-smirnov/price-tracker-bot/internal/platform/queue"
)

// Sender delivers one rendered message. It is an interface so the worker can be
// tested without Telegram, and so a future channel (email, web push) is a new
// implementation rather than a change here.
type Sender interface {
	// Send returns a retryable error to have the alert re-queued, and any other
	// error to have it dropped.
	Send(ctx context.Context, chatID int64, text string) error
}

// Source is the queue side the worker needs.
type Source interface {
	Pop(ctx context.Context, timeout time.Duration, dst any) error
	Push(ctx context.Context, payload any) error
}

// Claimer marks an alert as delivered, exactly once across replicas.
type Claimer interface {
	Claim(ctx context.Context, key string, ttl time.Duration) (bool, error)
	// Release undoes a claim. It is needed because the claim is taken before the
	// send: an alert that is going back on the queue must be deliverable again,
	// or a Telegram hiccup would silence it permanently.
	Release(ctx context.Context, key string) error
}

// ErrRetryable marks a send that should be attempted again later.
var ErrRetryable = errors.New("notify: retryable send failure")

// dedupTTL is how long a delivered alert is remembered. It only has to outlive
// the window in which a duplicate could plausibly arrive — a re-queued alert
// after a restart — and not the item's whole history.
const dedupTTL = 24 * time.Hour

// WorkerOptions configures the delivery loop.
type WorkerOptions struct {
	Source  Source
	Sender  Sender
	Claimer Claimer
	Log     *slog.Logger

	// Workers is how many alerts are delivered concurrently. Telegram allows
	// roughly 30 messages per second overall, so a handful of workers is plenty
	// and more would only earn 429s.
	Workers int
	// PopTimeout is how long a worker blocks on an empty queue before looping, so
	// shutdown is never delayed by more than this.
	PopTimeout time.Duration
	// RequeueDelay spaces out a retry of a failed send. It is a plain sleep before
	// pushing the alert back: the queue has no scheduled delivery, and an alert
	// that waits a few seconds is still timely.
	RequeueDelay time.Duration
	// MaxAttempts caps re-queues, so an undeliverable alert cannot circulate
	// forever.
	MaxAttempts int
}

// Worker drains the alert queue into Telegram.
type Worker struct {
	opts     WorkerOptions
	attempts attemptCounter
}

// NewWorker builds the delivery loop.
func NewWorker(opts WorkerOptions) *Worker {
	if opts.Workers <= 0 {
		opts.Workers = 2
	}
	if opts.PopTimeout <= 0 {
		opts.PopTimeout = 5 * time.Second
	}
	if opts.RequeueDelay <= 0 {
		opts.RequeueDelay = 10 * time.Second
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 5
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &Worker{opts: opts, attempts: newAttemptCounter()}
}

// Run delivers alerts until the context is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	w.opts.Log.InfoContext(ctx, "notifier starting", "workers", w.opts.Workers)

	done := make(chan struct{})
	for i := 0; i < w.opts.Workers; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			w.loop(ctx)
		}()
	}
	for i := 0; i < w.opts.Workers; i++ {
		<-done
	}
	w.opts.Log.InfoContext(ctx, "notifier stopped")
	return nil
}

func (w *Worker) loop(ctx context.Context) {
	for ctx.Err() == nil {
		var alert Alert
		err := w.opts.Source.Pop(ctx, w.opts.PopTimeout, &alert)
		switch {
		case errors.Is(err, queue.ErrEmpty), errors.Is(err, context.Canceled),
			errors.Is(err, context.DeadlineExceeded):
			continue
		case err != nil:
			// Redis being unreachable, or a payload this build cannot decode. Both
			// are worth a pause: retrying instantly would fill the log.
			w.opts.Log.ErrorContext(ctx, "reading the alert queue failed", "error", err)
			if sleepCtx(ctx, time.Second) != nil {
				return
			}
			continue
		}
		w.deliver(ctx, alert)
	}
}

func (w *Worker) deliver(ctx context.Context, alert Alert) {
	log := w.opts.Log.With("kind", string(alert.Kind), "item_id", alert.ItemID,
		"dedup_key", alert.DedupKey)

	if alert.TelegramID == 0 {
		log.ErrorContext(ctx, "dropping an alert with no recipient")
		return
	}

	// Claim before sending, not after: the queue is at-least-once, and a duplicate
	// "цена снизилась" for a drop the user already saw reads as a bug. The cost is
	// that a crash between the claim and the send loses that alert, which is the
	// cheaper mistake.
	if w.opts.Claimer != nil && alert.DedupKey != "" {
		first, err := w.opts.Claimer.Claim(ctx, alert.DedupKey, dedupTTL)
		if err != nil {
			log.WarnContext(ctx, "deduplication unavailable, sending anyway", "error", err)
		} else if !first {
			log.DebugContext(ctx, "skipping an already delivered alert")
			return
		}
	}

	err := w.opts.Sender.Send(ctx, alert.TelegramID, Render(alert))
	switch {
	case err == nil:
		w.attempts.forget(alert.DedupKey)
		log.InfoContext(ctx, "alert delivered")

	case errors.Is(err, ErrRetryable) && ctx.Err() == nil:
		w.requeue(ctx, log, alert)

	case ctx.Err() != nil:
		// Shutting down mid-send: the alert is already claimed and will not be
		// retried. Logged so the loss is visible.
		log.WarnContext(ctx, "alert dropped during shutdown", "error", err)

	default:
		// A blocked bot, a deleted chat, malformed HTML: nothing improves by
		// retrying.
		log.ErrorContext(ctx, "alert dropped", "error", err)
	}
}

// requeue puts a failed alert back, giving Telegram time to recover.
func (w *Worker) requeue(ctx context.Context, log *slog.Logger, alert Alert) {
	attempt := w.attempts.next(alert.DedupKey)
	if attempt >= w.opts.MaxAttempts {
		w.attempts.forget(alert.DedupKey)
		log.ErrorContext(ctx, "alert dropped after repeated failures", "attempts", attempt)
		return
	}
	if w.opts.Claimer != nil && alert.DedupKey != "" {
		if err := w.opts.Claimer.Release(ctx, alert.DedupKey); err != nil {
			// The retry would now be swallowed by its own claim, so it is better to
			// stop here and say so than to re-queue an alert that cannot be sent.
			log.ErrorContext(ctx, "alert dropped: releasing the claim failed", "error", err)
			return
		}
	}
	if sleepCtx(ctx, w.opts.RequeueDelay) != nil {
		log.WarnContext(ctx, "alert dropped during shutdown", "attempts", attempt)
		return
	}
	if err := w.opts.Source.Push(ctx, alert); err != nil {
		log.ErrorContext(ctx, "re-queueing the alert failed", "error", err)
		return
	}
	log.WarnContext(ctx, "alert re-queued", "attempt", attempt)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
