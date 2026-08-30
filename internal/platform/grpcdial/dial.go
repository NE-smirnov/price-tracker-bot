// Package grpcdial builds the gRPC client connections services use to reach each
// other. It exists so every caller gets the same keepalive and timeout
// behaviour, instead of each binary rediscovering it.
package grpcdial

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

// Dial creates a client connection.
//
// The connection is lazy: gRPC returns immediately and connects in the
// background, so a service starts even when its dependency is still booting and
// recovers by itself once it is back.
//
// callTimeout, when set, bounds every unary call made on this connection. It is
// applied as an interceptor rather than left to each call site, because the
// contexts these services work with are long-lived — a scrape loop or a Telegram
// update — and one stuck backend would otherwise block a worker indefinitely.
// Streaming calls are left alone: a price-history stream is legitimately long.
func Dial(addr string, callTimeout time.Duration) (*grpc.ClientConn, error) {
	if addr == "" {
		return nil, errors.New("grpcdial: address is empty")
	}

	options := []grpc.DialOption{
		// Both services sit inside one compose network or VPC, so plaintext is
		// intentional; TLS belongs at the deployment edge.
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		// WaitForReady(false) makes a call fail fast while the target is down,
		// which surfaces an outage in the logs instead of hiding it as latency.
		grpc.WithDefaultCallOptions(grpc.WaitForReady(false)),
	}
	if callTimeout > 0 {
		options = append(options, grpc.WithUnaryInterceptor(timeoutInterceptor(callTimeout)))
	}

	conn, err := grpc.NewClient(addr, options...)
	if err != nil {
		return nil, fmt.Errorf("grpcdial: dial %s: %w", addr, err)
	}
	return conn, nil
}

func timeoutInterceptor(timeout time.Duration) grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req, reply any,
		conn *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		// An explicit deadline from the caller is respected when it is tighter.
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= timeout {
			return invoker(ctx, method, req, reply, conn, opts...)
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return invoker(ctx, method, req, reply, conn, opts...)
	}
}
