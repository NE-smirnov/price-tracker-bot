package scraper

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
	pb "github.com/NE-smirnov/price-tracker-bot/internal/gen/pricetracker/v1"
	"github.com/NE-smirnov/price-tracker-bot/internal/notify"
)

// Alerts is the sink the pool hands raised alerts to. It is an interface so the
// pool does not depend on Redis, and so tests can assert on what was raised.
type Alerts interface {
	Push(ctx context.Context, payload any) error
}

// PoolOptions configures the scrape loop.
type PoolOptions struct {
	Items    pb.ItemServiceClient
	Pricing  pb.PricingServiceClient
	Currency pb.CurrencyServiceClient
	Scraper  *Scraper
	Alerts   Alerts
	Log      *slog.Logger

	// Workers is how many items are scraped at once. Concurrency is bounded here
	// and spacing per shop is enforced by the client, so this number can exceed
	// the number of shops without hammering any of them.
	Workers int
	// BatchSize caps one claim. A batch is a unit of work, not a unit of time:
	// the loop claims again as soon as it finishes.
	BatchSize int
	// Lease is how long a claimed item stays invisible to other workers. It must
	// comfortably exceed the time one scrape takes, or two workers will duplicate
	// work; it must not be so long that a crashed worker's items stall for hours.
	Lease time.Duration
	// IdlePause is how long to wait after an empty claim, so an idle deployment
	// does not poll the database in a tight loop.
	IdlePause time.Duration
}

// Pool repeatedly claims due items and scrapes them.
type Pool struct {
	opts PoolOptions
}

// NewPool builds the scrape loop, filling in defaults.
func NewPool(opts PoolOptions) *Pool {
	if opts.Workers <= 0 {
		opts.Workers = 4
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = opts.Workers * 4
	}
	if opts.Lease <= 0 {
		opts.Lease = 2 * time.Minute
	}
	if opts.IdlePause <= 0 {
		opts.IdlePause = 15 * time.Second
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &Pool{opts: opts}
}

// Run claims and scrapes until the context is cancelled.
func (p *Pool) Run(ctx context.Context) error {
	p.opts.Log.InfoContext(ctx, "scrape loop starting",
		"workers", p.opts.Workers, "batch_size", p.opts.BatchSize, "lease", p.opts.Lease)

	// Cancellation is read from the context rather than from the returned error:
	// every path below is either shutdown or a failure worth logging, and mixing
	// the two makes the loop hard to read.
	for ctx.Err() == nil {
		scraped, err := p.runOnce(ctx)
		if err != nil && ctx.Err() == nil {
			// A failed claim is core or the database being unavailable. The loop
			// keeps running: the items are still due and will be claimed when core
			// comes back.
			p.opts.Log.ErrorContext(ctx, "claim failed", "error", err)
		}

		// Only pause when there was nothing to do. A full batch means the backlog
		// is still draining and the next claim should go out immediately.
		if scraped < p.opts.BatchSize {
			_ = sleepCtx(ctx, p.opts.IdlePause)
		}
	}

	p.opts.Log.InfoContext(ctx, "scrape loop stopped")
	return nil
}

// runOnce claims one batch and scrapes it, returning how many items were taken.
func (p *Pool) runOnce(ctx context.Context) (int, error) {
	claimed, err := p.opts.Items.ClaimDueItems(ctx, &pb.ClaimDueItemsRequest{
		Limit:        int32(p.opts.BatchSize),
		LeaseSeconds: int32(p.opts.Lease.Seconds()),
	})
	if err != nil {
		return 0, err
	}
	items := claimed.GetItems()
	if len(items) == 0 {
		return 0, nil
	}
	p.opts.Log.DebugContext(ctx, "claimed a batch", "items", len(items))

	work := make(chan *pb.TrackedItem)
	var wg sync.WaitGroup
	for i := 0; i < p.opts.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range work {
				p.process(ctx, item)
			}
		}()
	}

	for _, item := range items {
		select {
		case work <- item:
		case <-ctx.Done():
			// Stop feeding on shutdown; the in-flight items finish, and the rest
			// keep their lease and are re-claimed by the next process.
			close(work)
			wg.Wait()
			return len(items), nil
		}
	}
	close(work)
	wg.Wait()
	return len(items), nil
}

// process scrapes one item and records the outcome.
func (p *Pool) process(ctx context.Context, item *pb.TrackedItem) {
	log := p.opts.Log.With("item_id", item.GetId(), "url", item.GetUrl())

	hint := targetCurrency(item)
	observed, err := p.opts.Scraper.Observe(ctx, item.GetUrl(), hint)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		p.recordFailure(ctx, log, item, err)
		return
	}

	request := &pb.RecordSnapshotRequest{
		TrackedItemId: item.GetId(),
		InStock:       observed.InStock,
		ObservedAt:    timestamppb.Now(),
	}
	if observed.Title != "" {
		title := observed.Title
		request.ObservedTitle = &title
	}
	if observed.Price.Amount > 0 {
		request.Price = moneyToProto(observed.Price)
		if converted, ok := p.convert(ctx, log, observed.Price, hint); ok {
			request.ConvertedPrice = moneyToProto(converted)
		}
	}

	response, err := p.opts.Pricing.RecordSnapshot(ctx, request)
	if err != nil {
		if ctx.Err() == nil {
			// The observation is lost, but nothing is corrupted: the item's lease
			// expires and it is scraped again.
			log.ErrorContext(ctx, "recording the snapshot failed", "error", err)
		}
		return
	}

	log.DebugContext(ctx, "snapshot recorded",
		"price", observed.Price.String(), "in_stock", observed.InStock,
		"alerts", len(response.GetAlerts()))
	p.raise(ctx, log, response.GetAlerts())
}

// convert expresses a shop price in the currency the user set their target in.
//
// It is skipped when the item has no target price, because there is then no
// second currency to convert to: the alert is a new all-time low, which is
// judged against the item's own history in its own currency.
func (p *Pool) convert(ctx context.Context, log *slog.Logger, price domain.Money, target domain.Currency) (domain.Money, bool) {
	if target == "" || target == price.Currency || p.opts.Currency == nil {
		return domain.Money{}, false
	}
	response, err := p.opts.Currency.Convert(ctx, &pb.ConvertRequest{
		Amount:     moneyToProto(price),
		ToCurrency: string(target),
	})
	if err != nil {
		// A missing conversion is not a failed scrape: the shop price is real and
		// worth storing. Core compares against the target only when it has a
		// comparable amount, so the alert is postponed rather than wrong.
		log.WarnContext(ctx, "converting the price failed",
			"from", price.Currency, "to", target, "error", err)
		return domain.Money{}, false
	}
	converted := response.GetConverted()
	return domain.Money{
		Amount:   converted.GetAmount(),
		Currency: domain.Currency(converted.GetCurrency()),
	}, true
}

func (p *Pool) recordFailure(ctx context.Context, log *slog.Logger, item *pb.TrackedItem, cause error) {
	level := slog.LevelWarn
	if errors.Is(cause, ErrBlocked) || errors.Is(cause, ErrNotFound) {
		// These say something about the shop or the listing rather than about a
		// bad minute, and they are what a user eventually has to be told about.
		level = slog.LevelError
	}
	log.Log(ctx, level, "scrape failed", "error", cause)

	response, err := p.opts.Pricing.RecordFailure(ctx, &pb.RecordFailureRequest{
		TrackedItemId: item.GetId(),
		Reason:        failureReason(cause),
		ObservedAt:    timestamppb.Now(),
	})
	if err != nil {
		if ctx.Err() == nil {
			log.ErrorContext(ctx, "recording the failure failed", "error", err)
		}
		return
	}
	p.raise(ctx, log, response.GetAlerts())
}

// raise hands alerts to the notifier.
func (p *Pool) raise(ctx context.Context, log *slog.Logger, alerts []*pb.PendingAlert) {
	if len(alerts) == 0 || p.opts.Alerts == nil {
		return
	}
	for _, alert := range alerts {
		if err := p.opts.Alerts.Push(ctx, notify.FromProto(alert)); err != nil {
			if ctx.Err() == nil {
				// Loud, because this is a user-visible alert that will not arrive.
				// Core has already recorded the state change, so the next scrape
				// will not raise it again.
				log.ErrorContext(ctx, "enqueueing the alert failed",
					"kind", alert.GetKind().String(), "dedup_key", alert.GetDedupKey(), "error", err)
			}
			return
		}
	}
}

// failureReason keeps the stored reason short and stable, so a streak of the
// same cause is recognisable in the database instead of differing by timestamp.
func failureReason(cause error) string {
	switch {
	case errors.Is(cause, ErrBlocked):
		return "blocked by the shop"
	case errors.Is(cause, ErrNotFound):
		return "page not found"
	case errors.Is(cause, ErrTemporary):
		return "shop unavailable"
	case errors.Is(cause, ErrNoPrice):
		return "no price found on the page"
	default:
		return cause.Error()
	}
}

func targetCurrency(item *pb.TrackedItem) domain.Currency {
	if target := item.GetTargetPrice(); target != nil {
		return domain.Currency(target.GetCurrency())
	}
	return ""
}

func moneyToProto(money domain.Money) *pb.Money {
	return &pb.Money{Amount: money.Amount, Currency: string(money.Currency)}
}
