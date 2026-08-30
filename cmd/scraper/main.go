// Command scraper runs the price-reading loop: it claims due items from core,
// reads their pages, converts the price when the user tracks in another currency
// and hands raised alerts to the notifier.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	pb "github.com/NE-smirnov/price-tracker-bot/internal/gen/pricetracker/v1"
	"github.com/NE-smirnov/price-tracker-bot/internal/platform/config"
	"github.com/NE-smirnov/price-tracker-bot/internal/platform/grpcdial"
	"github.com/NE-smirnov/price-tracker-bot/internal/platform/logger"
	"github.com/NE-smirnov/price-tracker-bot/internal/platform/queue"
	"github.com/NE-smirnov/price-tracker-bot/internal/platform/redisclient"
	"github.com/NE-smirnov/price-tracker-bot/internal/scraper"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "scraper: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if err := config.LoadDotEnv(); err != nil {
		return err
	}
	cfg, err := config.LoadScraper()
	if err != nil {
		return err
	}
	log := logger.New("scraper", cfg.LogLevel, cfg.LogFormat)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// A claim or a snapshot must not outlive the scrape it belongs to; the lease
	// is the natural bound.
	coreConn, err := grpcdial.Dial(cfg.CoreGRPCAddr, cfg.Lease)
	if err != nil {
		return err
	}
	defer func() { _ = coreConn.Close() }()

	currencyConn, err := grpcdial.Dial(cfg.CurrencyGRPCAddr, cfg.RequestTimeout)
	if err != nil {
		return err
	}
	defer func() { _ = currencyConn.Close() }()

	redisClient, err := redisclient.New(ctx, redisclient.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPass,
		DB:       cfg.RedisDB,
	})
	if err != nil {
		// Without Redis the loop still scrapes and stores prices; only the alerts
		// have nowhere to go, and that is logged per alert rather than silently.
		log.Warn("redis unavailable, alerts cannot be queued", "error", err)
	}
	if redisClient != nil {
		defer func() { _ = redisClient.Close() }()
	}

	client := scraper.NewClient(scraper.ClientOptions{
		Timeout:    cfg.RequestTimeout,
		UserAgent:  cfg.UserAgent,
		PerHostGap: cfg.PerHostGap,
		MaxRetries: cfg.MaxRetries,
	})
	// Shop-specific adapters come first; anything they do not claim falls through
	// to reading structured data out of the page.
	engine := scraper.NewScraper(client, log, scraper.Wildberries{})

	pool := scraper.NewPool(scraper.PoolOptions{
		Items:     pb.NewItemServiceClient(coreConn),
		Pricing:   pb.NewPricingServiceClient(coreConn),
		Currency:  pb.NewCurrencyServiceClient(currencyConn),
		Scraper:   engine,
		Alerts:    queue.New(redisClient, "alerts"),
		Log:       log,
		Workers:   cfg.Workers,
		BatchSize: cfg.BatchSize,
		Lease:     cfg.Lease,
		IdlePause: cfg.IdlePause,
	})

	log.Info("scraper configured",
		"core", cfg.CoreGRPCAddr, "currency", cfg.CurrencyGRPCAddr,
		"per_host_gap", cfg.PerHostGap)
	return pool.Run(ctx)
}
