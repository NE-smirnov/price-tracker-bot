// Package grpcserve runs a gRPC server with the lifecycle every service in this
// repo needs: keepalive tuned for long-lived idle clients, a health service, and
// a graceful stop that drains in-flight calls before the process exits.
//
// It exists so that adding a service is writing its handlers, not copying a
// hundred lines of shutdown logic that then drifts between binaries.
package grpcserve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
)

// shutdownGrace bounds how long in-flight RPCs may finish before the server is
// stopped forcefully.
const shutdownGrace = 15 * time.Second

// Options configures a run.
type Options struct {
	// Name appears in log lines.
	Name string
	// Addr is the listen address, e.g. ":9090".
	Addr string
	Log  *slog.Logger
	// Register attaches the service implementations to the server.
	Register func(*grpc.Server)
	// OnServing runs once the socket is bound and serving has started. It is used
	// for background loops that should not start before the port is up, and its
	// cleanup function is called during shutdown, before in-flight calls drain.
	OnServing func(context.Context) (stop func(), err error)
}

// Run serves until ctx is cancelled, then stops gracefully.
func Run(ctx context.Context, opts Options) error {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}

	server := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			// Long-lived clients idle between bursts; pinging them keeps NAT and
			// load balancers from silently dropping the connection.
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if opts.Register != nil {
		opts.Register(server)
	}

	healthSrv := health.NewServer()
	healthpb.RegisterHealthServer(server, healthSrv)
	// Reflection lets grpcurl explore the API during development without a
	// checked-in descriptor set.
	reflection.Register(server)

	// ListenConfig rather than net.Listen: the socket is bound under the same
	// context that cancels on SIGTERM.
	var listenCfg net.ListenConfig
	listener, err := listenCfg.Listen(ctx, "tcp", opts.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", opts.Addr, err)
	}

	serveErr := make(chan error, 1)
	go func() {
		opts.Log.Info(opts.Name+" listening", "addr", listener.Addr().String())
		if serveFailure := server.Serve(listener); serveFailure != nil &&
			!errors.Is(serveFailure, grpc.ErrServerStopped) {
			serveErr <- fmt.Errorf("serve: %w", serveFailure)
			return
		}
		serveErr <- nil
	}()

	var stopBackground func()
	if opts.OnServing != nil {
		stopBackground, err = opts.OnServing(ctx)
		if err != nil {
			server.Stop()
			return err
		}
	}

	select {
	case err := <-serveErr:
		if stopBackground != nil {
			stopBackground()
		}
		return err
	case <-ctx.Done():
		opts.Log.Info("shutdown requested")
	}

	// Report NOT_SERVING first so a load balancer stops sending new work while
	// the current requests drain.
	healthSrv.Shutdown()
	if stopBackground != nil {
		stopBackground()
	}

	stopped := make(chan struct{})
	go func() {
		server.GracefulStop()
		close(stopped)
	}()

	select {
	case <-stopped:
		opts.Log.Info(opts.Name + " stopped gracefully")
	case <-time.After(shutdownGrace):
		opts.Log.Warn("graceful stop timed out, forcing", "grace", shutdownGrace)
		server.Stop()
	}

	// Drain the result of Serve after the shutdown path so a real error is not
	// swallowed by the graceful stop.
	select {
	case err := <-serveErr:
		return err
	case <-time.After(2 * time.Second):
		opts.Log.Warn("serve goroutine did not report completion")
		return nil
	}
}
