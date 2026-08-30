// Command core runs the data-owning service: it exposes ItemService and
// PricingService over gRPC and is the only process that talks to Postgres.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"

	"github.com/NE-smirnov/price-tracker-bot/internal/core"
	pb "github.com/NE-smirnov/price-tracker-bot/internal/gen/pricetracker/v1"
	"github.com/NE-smirnov/price-tracker-bot/internal/platform/config"
	"github.com/NE-smirnov/price-tracker-bot/internal/platform/grpcserve"
	"github.com/NE-smirnov/price-tracker-bot/internal/platform/logger"
	"github.com/NE-smirnov/price-tracker-bot/internal/platform/postgres"
	"github.com/NE-smirnov/price-tracker-bot/internal/platform/redisclient"
)

func main() {
	if err := run(); err != nil {
		// Errors are printed rather than logged: this path includes failures that
		// happen before the logger exists.
		fmt.Fprintf(os.Stderr, "core: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if err := config.LoadDotEnv(); err != nil {
		return err
	}
	cfg, err := config.LoadCore()
	if err != nil {
		return err
	}
	log := logger.New("core", cfg.LogLevel, cfg.LogFormat)

	// SIGINT/SIGTERM cancel this context, which unwinds every component below.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.New(ctx, postgres.Options{DSN: cfg.PostgresDSN})
	if err != nil {
		return err
	}
	defer pool.Close()
	log.Info("connected to postgres")

	redisClient, err := redisclient.New(ctx, redisclient.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPass,
		DB:       cfg.RedisDB,
	})
	if err != nil {
		// Redis is a cache, not a source of truth: a bad address should not stop
		// the service from serving correct, slower answers.
		log.Warn("redis unavailable, running without cache", "error", err)
	}
	if redisClient != nil {
		defer func() { _ = redisClient.Close() }()
		log.Info("connected to redis", "addr", cfg.RedisAddr)
	}

	repo := core.NewRepo(pool)
	statsCache := redisclient.NewCache(redisClient, "core", cfg.StatsTTL, log)

	return grpcserve.Run(ctx, grpcserve.Options{
		Name: "core",
		Addr: cfg.GRPCAddr,
		Log:  log,
		Register: func(server *grpc.Server) {
			pb.RegisterItemServiceServer(server, core.NewItemServer(repo, log))
			pb.RegisterPricingServiceServer(server, core.NewPricingServer(repo, statsCache, log))
		},
	})
}
