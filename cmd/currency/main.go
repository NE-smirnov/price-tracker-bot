// Command currency runs the conversion service: it exposes CurrencyService over
// gRPC and is the only process that talks to an external exchange-rate provider.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"

	"github.com/NE-smirnov/price-tracker-bot/internal/currency"
	pb "github.com/NE-smirnov/price-tracker-bot/internal/gen/pricetracker/v1"
	"github.com/NE-smirnov/price-tracker-bot/internal/platform/config"
	"github.com/NE-smirnov/price-tracker-bot/internal/platform/grpcserve"
	"github.com/NE-smirnov/price-tracker-bot/internal/platform/logger"
	"github.com/NE-smirnov/price-tracker-bot/internal/platform/redisclient"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "currency: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if err := config.LoadDotEnv(); err != nil {
		return err
	}
	cfg, err := config.LoadCurrency()
	if err != nil {
		return err
	}
	log := logger.New("currency", cfg.LogLevel, cfg.LogFormat)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	redisClient, err := redisclient.New(ctx, redisclient.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPass,
		DB:       cfg.RedisDB,
	})
	if err != nil {
		// Without Redis every replica keeps its own in-process table, which costs
		// a few more provider requests but still serves correct rates.
		log.Warn("redis unavailable, rates will be cached in memory only", "error", err)
	}
	if redisClient != nil {
		defer func() { _ = redisClient.Close() }()
	}

	service := currency.NewService(currency.Options{
		Provider: currency.NewHTTPProvider(cfg.ProviderURL, cfg.ProviderTimeout),
		Cache:    redisclient.NewCache(redisClient, "fx", cfg.RateTTL, log),
		Log:      log,
		TTL:      cfg.RateTTL,
	})

	return grpcserve.Run(ctx, grpcserve.Options{
		Name: "currency",
		Addr: cfg.GRPCAddr,
		Log:  log,
		Register: func(server *grpc.Server) {
			pb.RegisterCurrencyServiceServer(server, currency.NewServer(service, log))
		},
	})
}
