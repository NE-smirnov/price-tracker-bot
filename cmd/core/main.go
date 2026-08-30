// Command core runs the data-owning service: it exposes ItemService and
// PricingService over gRPC and is the only process that talks to Postgres.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"

	"github.com/NE-smirnov/price-tracker-bot/internal/core"
	pb "github.com/NE-smirnov/price-tracker-bot/internal/gen/pricetracker/v1"
	"github.com/NE-smirnov/price-tracker-bot/internal/platform/config"
	"github.com/NE-smirnov/price-tracker-bot/internal/platform/logger"
	"github.com/NE-smirnov/price-tracker-bot/internal/platform/postgres"
	"github.com/NE-smirnov/price-tracker-bot/internal/platform/redisclient"
)

// shutdownGrace bounds how long in-flight RPCs may finish before the server is
// stopped forcefully.
const shutdownGrace = 15 * time.Second

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

	server := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			// Long-lived clients (bot, scraper) idle between bursts; pinging them
			// keeps NAT and load balancers from silently dropping the connection.
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	pb.RegisterItemServiceServer(server, core.NewItemServer(repo, log))
	pb.RegisterPricingServiceServer(server, core.NewPricingServer(repo, statsCache, log))

	healthSrv := health.NewServer()
	healthpb.RegisterHealthServer(server, healthSrv)
	// Reflection lets grpcurl explore the API during development without a
	// checked-in descriptor set.
	reflection.Register(server)

	// ListenConfig rather than net.Listen: the socket is bound under the same
	// context that cancels on SIGTERM.
	var listenCfg net.ListenConfig
	listener, err := listenCfg.Listen(ctx, "tcp", cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.GRPCAddr, err)
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("core listening", "addr", listener.Addr().String())
		if err := server.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			serveErr <- fmt.Errorf("serve: %w", err)
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		log.Info("shutdown requested")
	}

	// Report NOT_SERVING first so a load balancer stops sending new work while
	// the current requests drain.
	healthSrv.Shutdown()

	stopped := make(chan struct{})
	go func() {
		server.GracefulStop()
		close(stopped)
	}()

	select {
	case <-stopped:
		log.Info("core stopped gracefully")
	case <-time.After(shutdownGrace):
		log.Warn("graceful stop timed out, forcing", "grace", shutdownGrace)
		server.Stop()
	}
	return awaitServe(serveErr, log)
}

// awaitServe drains the result of Serve after the shutdown path so a real error
// is not swallowed by the graceful stop.
func awaitServe(serveErr <-chan error, log *slog.Logger) error {
	select {
	case err := <-serveErr:
		return err
	case <-time.After(2 * time.Second):
		log.Warn("serve goroutine did not report completion")
		return nil
	}
}
