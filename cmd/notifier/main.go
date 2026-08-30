// Command notifier drains the alert queue into Telegram. It is separate from the
// bot so that delivery is not blocked by update handling, and so it can be
// restarted or scaled without dropping the conversation state the bot holds.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/NE-smirnov/price-tracker-bot/internal/notify"
	"github.com/NE-smirnov/price-tracker-bot/internal/platform/config"
	"github.com/NE-smirnov/price-tracker-bot/internal/platform/logger"
	"github.com/NE-smirnov/price-tracker-bot/internal/platform/queue"
	"github.com/NE-smirnov/price-tracker-bot/internal/platform/redisclient"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "notifier: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if err := config.LoadDotEnv(); err != nil {
		return err
	}
	cfg, err := config.LoadNotifier()
	if err != nil {
		return err
	}
	log := logger.New("notifier", cfg.LogLevel, cfg.LogFormat)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	redisClient, err := redisclient.New(ctx, redisclient.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPass,
		DB:       cfg.RedisDB,
	})
	if err != nil {
		// Unlike everywhere else, Redis is required here: the queue it holds is the
		// only thing this service consumes.
		return fmt.Errorf("notifier needs redis: %w", err)
	}
	defer func() { _ = redisClient.Close() }()

	sender, err := notify.NewTelegram(cfg.Token)
	if err != nil {
		return err
	}

	worker := notify.NewWorker(notify.WorkerOptions{
		Source:       queue.New(redisClient, "alerts"),
		Sender:       sender,
		Claimer:      notify.NewRedisClaimer(redisClient),
		Log:          log,
		Workers:      cfg.Workers,
		RequeueDelay: cfg.RequeueDelay,
		MaxAttempts:  cfg.MaxAttempts,
	})

	return worker.Run(ctx)
}
