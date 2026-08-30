// Package queue is a small Redis-backed work queue used to hand alerts from the
// scraper to the notifier.
//
// A queue rather than a direct gRPC call, because delivery must survive the
// notifier being restarted or briefly down: the scraper's job is to observe
// prices on a schedule, and it must not be blocked or lose an alert because a
// Telegram sender is unavailable. It is deliberately a Redis list rather than a
// real broker — a list gives ordering, blocking reads and at-least-once delivery
// with one dependency this project already has, and alerts are not money: the
// worst case of a lost one is a price drop the user hears about an hour later.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrEmpty means nothing arrived before the read timed out.
var ErrEmpty = errors.New("queue: empty")

// ErrDisabled means the queue has no Redis behind it.
var ErrDisabled = errors.New("queue: redis is not configured")

// Queue is one named list.
type Queue struct {
	client *redis.Client
	key    string
}

// New builds a queue. A nil client yields a queue whose operations report
// ErrDisabled, so a caller can start and complain rather than panic.
func New(client *redis.Client, name string) *Queue {
	return &Queue{client: client, key: "queue:" + name}
}

// Enabled reports whether the queue can be used.
func (q *Queue) Enabled() bool { return q != nil && q.client != nil }

// Push appends a payload to the tail of the queue.
func (q *Queue) Push(ctx context.Context, payload any) error {
	if !q.Enabled() {
		return ErrDisabled
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("queue: encode payload: %w", err)
	}
	if err := q.client.RPush(ctx, q.key, encoded).Err(); err != nil {
		return fmt.Errorf("queue: push: %w", err)
	}
	return nil
}

// Pop blocks until an item is available or the timeout elapses, then decodes it
// into dst.
//
// This is BRPOP, not a reliable-queue BRPOPLPUSH: an item is removed when it is
// read, so a consumer that crashes mid-send loses that one alert. The
// alternative costs a second list and a recovery pass, which is not worth it for
// a payload this cheap to regenerate — the next scrape re-raises a price drop
// that still holds.
func (q *Queue) Pop(ctx context.Context, timeout time.Duration, dst any) error {
	if !q.Enabled() {
		return ErrDisabled
	}
	result, err := q.client.BLPop(ctx, timeout, q.key).Result()
	switch {
	case errors.Is(err, redis.Nil):
		return ErrEmpty
	case err != nil:
		return fmt.Errorf("queue: pop: %w", err)
	case len(result) != 2:
		return fmt.Errorf("queue: pop returned %d fields, want 2", len(result))
	}
	if err := json.Unmarshal([]byte(result[1]), dst); err != nil {
		// A payload that cannot be decoded is already off the list; dropping it is
		// the only option that does not block the queue forever.
		return fmt.Errorf("queue: decode payload: %w", err)
	}
	return nil
}

// Len reports the queue depth, for logging and health checks.
func (q *Queue) Len(ctx context.Context) (int64, error) {
	if !q.Enabled() {
		return 0, ErrDisabled
	}
	return q.client.LLen(ctx, q.key).Result()
}

// Claim marks a deduplication key as handled and reports whether this caller is
// the first to do so. It is how at-least-once delivery is turned into
// at-most-once user-visible behaviour.
func Claim(ctx context.Context, client *redis.Client, key string, ttl time.Duration) (bool, error) {
	if client == nil {
		// Without Redis every alert is treated as new. Repeats are possible; that
		// is strictly better than sending nothing.
		return true, nil
	}
	ok, err := client.SetNX(ctx, "dedup:"+key, "1", ttl).Result()
	if err != nil {
		return false, fmt.Errorf("queue: claim %q: %w", key, err)
	}
	return ok, nil
}
