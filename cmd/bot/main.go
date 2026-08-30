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

	store, err := buildStore(cfg, log)
	if err != nil {
		return err
	}

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

// buildStore selects the Store implementation. The gRPC-backed one is added
// together with the core service; until then the bot runs on an in-memory store.
func buildStore(cfg config.Bot, log *slog.Logger) (bot.Store, error) {
	switch cfg.Storage {
	case "memory":
		log.Warn("using in-memory storage: data is lost on restart and price history is synthetic")
		return bot.NewMemStore(true), nil
	case "core":
		return nil, errors.New("BOT_STORAGE=core is not wired yet: the core service arrives in the next step")
	default:
		return nil, fmt.Errorf("unknown BOT_STORAGE %q", cfg.Storage)
	}
}
