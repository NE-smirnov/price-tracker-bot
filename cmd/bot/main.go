// Command bot runs the Telegram front-end of the price tracker.
//
// With BOT_STORAGE=memory it is fully self-contained: no Postgres, no Redis, no
// core service. That is the mode used to iterate on the conversation UX.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/NE-smirnov/price-tracker-bot/internal/bot"
	"github.com/NE-smirnov/price-tracker-bot/internal/platform/config"
	"github.com/NE-smirnov/price-tracker-bot/internal/platform/logger"
)

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet, so fail loudly on stderr.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if err := config.LoadDotEnv(); err != nil {
		return err
	}
	cfg, err := config.LoadBot()
	if err != nil {
		return err
	}

	log := logger.New("bot", cfg.LogLevel, cfg.LogFormat)

	// SIGINT/SIGTERM cancel the root context; every goroutine unwinds from there.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, closeStore, err := buildStore(cfg, log)
	if err != nil {
		return err
	}
	defer closeStore()

	b, err := bot.New(store, bot.Options{
		Token:        cfg.Token,
		AllowedUsers: cfg.AllowedUsers,
		SessionTTL:   15 * time.Minute,
		Logger:       log,
	})
	if err != nil {
		return err
	}

	log.Info("starting",
		slog.String("env", cfg.AppEnv),
		slog.String("storage", cfg.Storage))

	if err := b.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// buildStore selects the Store implementation and returns a cleanup function.
//
// The bot does not verify that core is reachable here on purpose: the gRPC
// client connects lazily and retries, so a bot started before core comes up
// heals by itself instead of crash-looping.
func buildStore(cfg config.Bot, log *slog.Logger) (bot.Store, func(), error) {
	switch cfg.Storage {
	case "memory":
		log.Warn("using in-memory storage: data is lost on restart and price history is synthetic")
		return bot.NewMemStore(true), func() {}, nil
	case "core":
		store, err := bot.NewCoreStore(bot.CoreStoreOptions{
			Addr:        cfg.CoreGRPCAddr,
			CallTimeout: 10 * time.Second,
		})
		if err != nil {
			return nil, nil, err
		}
		log.Info("using core storage", slog.String("addr", cfg.CoreGRPCAddr))
		return store, func() {
			if closeErr := store.Close(); closeErr != nil {
				log.Warn("closing core connection", slog.String("error", closeErr.Error()))
			}
		}, nil
	default:
		return nil, nil, fmt.Errorf("unknown BOT_STORAGE %q", cfg.Storage)
	}
}
