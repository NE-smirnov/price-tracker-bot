// Package redisclient provides the shared Redis connection and a small typed
// cache helper. Redis is treated as strictly optional: every helper degrades to
// "no cache" rather than failing the request, because a cold or missing cache
// must never make the product unavailable.
package redisclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// Options configures the client.
type Options struct {
	Addr     string
	Password string
	DB       int
}

// New dials Redis and verifies the connection. An empty Addr returns (nil, nil):
// callers must handle a nil client, which is the "caching disabled" mode.
func New(ctx context.Context, opts Options) (*redis.Client, error) {
	if opts.Addr == "" {
		return nil, nil
	}
	client := redis.NewClient(&redis.Options{
		Addr:            opts.Addr,
		Password:        opts.Password,
		DB:              opts.DB,
		DialTimeout:     3 * time.Second,
		ReadTimeout:     2 * time.Second,
		WriteTimeout:    2 * time.Second,
		MaxRetries:      2,
		MinIdleConns:    1,
		ConnMaxIdleTime: 5 * time.Minute,
	})

	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping redis at %s: %w", opts.Addr, err)
	}
	return client, nil
}

// Cache is a JSON key/value cache over Redis with a fixed namespace and TTL.
// The zero value and a nil client are both usable and simply never hit.
type Cache struct {
	client *redis.Client
	prefix string
	ttl    time.Duration
	log    *slog.Logger
}

// NewCache builds a namespaced cache. A nil client disables it.
func NewCache(client *redis.Client, prefix string, ttl time.Duration, log *slog.Logger) *Cache {
	return &Cache{client: client, prefix: prefix, ttl: ttl, log: log}
}

// Enabled reports whether the cache will actually store anything.
func (c *Cache) Enabled() bool { return c != nil && c.client != nil && c.ttl > 0 }

func (c *Cache) key(k string) string { return c.prefix + ":" + k }

// Get decodes a cached value into dst. It returns false on a miss and on any
// error, so a broken cache reads as an empty one.
func (c *Cache) Get(ctx context.Context, k string, dst any) bool {
	if !c.Enabled() {
		return false
	}
	raw, err := c.client.Get(ctx, c.key(k)).Bytes()
	switch {
	case errors.Is(err, redis.Nil):
		return false
	case err != nil:
		c.warn("cache get failed", k, err)
		return false
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		// A stale encoding is a miss, not an error: the value will be recomputed
		// and overwritten.
		c.warn("cache decode failed", k, err)
		return false
	}
	return true
}

// Set stores a value, ignoring failures for the same reason Get does.
func (c *Cache) Set(ctx context.Context, k string, v any) {
	if !c.Enabled() {
		return
	}
	raw, err := json.Marshal(v)
	if err != nil {
		c.warn("cache encode failed", k, err)
		return
	}
	if err := c.client.Set(ctx, c.key(k), raw, c.ttl).Err(); err != nil {
		c.warn("cache set failed", k, err)
	}
}

// Delete drops a key, used to invalidate after a write.
func (c *Cache) Delete(ctx context.Context, keys ...string) {
	if !c.Enabled() || len(keys) == 0 {
		return
	}
	full := make([]string, 0, len(keys))
	for _, k := range keys {
		full = append(full, c.key(k))
	}
	if err := c.client.Del(ctx, full...).Err(); err != nil {
		c.warn("cache delete failed", keys[0], err)
	}
}

func (c *Cache) warn(msg, key string, err error) {
	if c.log != nil {
		c.log.Warn(msg, "key", key, "error", err)
	}
}
