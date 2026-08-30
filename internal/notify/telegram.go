package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tg "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/redis/go-redis/v9"
)

// Telegram sends alerts through the Bot API.
//
// It shares the bot's token deliberately: notifications must arrive from the
// same account the user talks to, and sending is independent of the long polling
// the bot does, so two processes on one token do not conflict.
type Telegram struct {
	api *tg.Bot
}

// NewTelegram builds the sender. The client is created with no handlers, because
// this process never reads updates.
func NewTelegram(token string) (*Telegram, error) {
	api, err := tg.New(token)
	if err != nil {
		return nil, fmt.Errorf("notify: init telegram client: %w", err)
	}
	return &Telegram{api: api}, nil
}

// Send delivers one message, classifying the failure so the worker knows whether
// a retry can help.
func (t *Telegram) Send(ctx context.Context, chatID int64, text string) error {
	_, err := t.api.SendMessage(ctx, &tg.SendMessageParams{
		ChatID:    chatID,
		Text:      text,
		ParseMode: models.ParseModeHTML,
		// The preview would repeat the shop page under every alert and push the
		// price out of the notification snippet.
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: tg.True()},
	})
	if err == nil {
		return nil
	}
	return classify(err)
}

// classify decides whether a Bot API failure is worth another attempt.
func classify(err error) error {
	var tooMany *tg.TooManyRequestsError
	if errors.As(err, &tooMany) {
		// Telegram states how long to wait. The worker's fixed delay is used
		// instead of sleeping here, so one throttled chat does not block a worker.
		return fmt.Errorf("%w: rate limited, retry after %ds: %w",
			ErrRetryable, tooMany.RetryAfter, err)
	}

	// Malformed HTML, a chat that no longer exists, a user who blocked the bot: the
	// same request will fail the same way forever, so these are not retried.
	for _, permanent := range []error{
		tg.ErrorBadRequest, tg.ErrorForbidden, tg.ErrorNotFound, tg.ErrorUnauthorized,
	} {
		if errors.Is(err, permanent) {
			return fmt.Errorf("telegram refused the message permanently: %w", err)
		}
	}

	var migrated *tg.MigrateError
	if errors.As(err, &migrated) {
		// A group turned into a supergroup and changed its id. Alerts go to private
		// chats, so this is unexpected rather than something to follow.
		return fmt.Errorf("the chat was migrated to %d: %w", migrated.MigrateToChatID, err)
	}

	// Timeouts, connection resets and 5xx from Telegram's edge all read as plain
	// errors here, and all of them pass with a later attempt.
	if isTransport(err) {
		return fmt.Errorf("%w: %w", ErrRetryable, err)
	}
	return err
}

func isTransport(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{
		"timeout", "connection reset", "connection refused", "eof",
		"no such host", "temporary failure", "bad gateway", "502", "503", "504",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// RedisClaimer deduplicates deliveries across replicas and restarts.
type RedisClaimer struct {
	client *redis.Client
}

// NewRedisClaimer wraps a client. A nil client yields a claimer that lets
// everything through, so a Redis outage degrades to possible duplicates rather
// than to silence.
func NewRedisClaimer(client *redis.Client) *RedisClaimer {
	return &RedisClaimer{client: client}
}

// Claim reports whether this caller is the first to handle the key.
func (c *RedisClaimer) Claim(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	if c == nil || c.client == nil {
		return true, nil
	}
	ok, err := c.client.SetNX(ctx, dedupKey(key), "1", ttl).Result()
	if err != nil {
		return false, fmt.Errorf("notify: claim %q: %w", key, err)
	}
	return ok, nil
}

// Release gives the key back so a re-queued alert can be delivered.
func (c *RedisClaimer) Release(ctx context.Context, key string) error {
	if c == nil || c.client == nil {
		return nil
	}
	if err := c.client.Del(ctx, dedupKey(key)).Err(); err != nil {
		return fmt.Errorf("notify: release %q: %w", key, err)
	}
	return nil
}

func dedupKey(key string) string { return "dedup:notify:" + key }
