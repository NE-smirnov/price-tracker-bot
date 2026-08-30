// Package postgres wires up the shared pgx connection pool.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Options tune the pool. Zero values fall back to the defaults below.
type Options struct {
	DSN             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	ConnectTimeout  time.Duration
}

// New opens the pool and verifies it can actually reach the database, so a bad
// DSN fails at startup instead of on the first user request.
func New(ctx context.Context, opts Options) (*pgxpool.Pool, error) {
	if opts.DSN == "" {
		return nil, fmt.Errorf("postgres: empty DSN")
	}

	cfg, err := pgxpool.ParseConfig(opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse DSN: %w", err)
	}

	cfg.MaxConns = orDefaultInt32(opts.MaxConns, 10)
	cfg.MinConns = orDefaultInt32(opts.MinConns, 1)
	cfg.MaxConnLifetime = orDefaultDuration(opts.MaxConnLifetime, time.Hour)
	cfg.MaxConnIdleTime = orDefaultDuration(opts.MaxConnIdleTime, 5*time.Minute)
	// Recycling slightly early spreads reconnects instead of having the whole
	// pool expire at the same instant.
	cfg.MaxConnLifetimeJitter = cfg.MaxConnLifetime / 10

	connectTimeout := orDefaultDuration(opts.ConnectTimeout, 5*time.Second)
	cfg.ConnConfig.ConnectTimeout = connectTimeout

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return pool, nil
}

func orDefaultInt32(v, def int32) int32 {
	if v <= 0 {
		return def
	}
	return v
}

func orDefaultDuration(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}
