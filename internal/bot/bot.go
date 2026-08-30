// Package bot implements the Telegram front-end of the price tracker.
//
// The Telegram layer is deliberately thin: it parses user input, keeps the
// per-user conversation state and delegates every domain decision to a Store.
// Swapping the in-memory Store for the gRPC client of the core service does not
// touch a single handler.
package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	tg "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

// Options configures the bot.
type Options struct {
	Token string
	// AllowedUsers restricts access to the listed Telegram ids. Empty = public.
	AllowedUsers []int64
	// SessionTTL is how long an unfinished dialog survives.
	SessionTTL time.Duration
	Logger     *slog.Logger
}

// Bot wires the Telegram API, the conversation state and the Store together.
type Bot struct {
	api      *tg.Bot
	store    Store
	sessions *sessionStore
	log      *slog.Logger
	allowed  map[int64]struct{}
}

// New builds a Bot. It does not perform any network call yet.
func New(store Store, opts Options) (*Bot, error) {
	if store == nil {
		return nil, errors.New("bot: store is required")
	}
	if opts.Token == "" {
		return nil, errors.New("bot: token is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	b := &Bot{
		store:    store,
		sessions: newSessionStore(opts.SessionTTL),
		log:      opts.Logger,
		allowed:  make(map[int64]struct{}, len(opts.AllowedUsers)),
	}
	for _, id := range opts.AllowedUsers {
		b.allowed[id] = struct{}{}
	}

	api, err := tg.New(opts.Token,
		tg.WithDefaultHandler(b.onFreeText),
		tg.WithMiddlewares(b.recoverPanic, b.logUpdate, b.authorize),
	)
	if err != nil {
		return nil, fmt.Errorf("bot: init telegram client: %w", err)
	}
	b.api = api

	type route struct {
		pattern string
		handler tg.HandlerFunc
	}
	for _, r := range []route{
		{"/start", b.onStart},
		{"/help", b.onHelp},
		{"/add", b.onAdd},
		{"/list", b.onList},
		{"/remove", b.onRemove},
		{"/stats", b.onStats},
		{"/settings", b.onSettings},
		{"/cancel", b.onCancel},
	} {
		api.RegisterHandler(tg.HandlerTypeMessageText, r.pattern, tg.MatchTypePrefix, r.handler)
	}

	api.RegisterHandler(tg.HandlerTypeCallbackQueryData, cbPrefix, tg.MatchTypePrefix, b.onCallback)

	return b, nil
}

// Run publishes the command list and blocks polling updates until ctx is done.
func (b *Bot) Run(ctx context.Context) error {
	if err := b.publishCommands(ctx); err != nil {
		// Not fatal: the bot works without the command menu.
		b.log.Warn("cannot publish command list", slog.Any("error", err))
	}

	go b.gcSessions(ctx, time.Minute)

	b.log.Info("telegram bot started", slog.Int("allowlist_size", len(b.allowed)))
	b.api.Start(ctx) // returns when ctx is cancelled
	b.log.Info("telegram bot stopped")
	return nil
}

func (b *Bot) publishCommands(ctx context.Context) error {
	_, err := b.api.SetMyCommands(ctx, &tg.SetMyCommandsParams{
		Commands: []models.BotCommand{
			{Command: "add", Description: "добавить товар"},
			{Command: "list", Description: "мои товары"},
			{Command: "stats", Description: "статистика по товару"},
			{Command: "remove", Description: "убрать товар"},
			{Command: "settings", Description: "валюта отображения"},
			{Command: "cancel", Description: "прервать диалог"},
			{Command: "help", Description: "как это работает"},
		},
	})
	if err != nil {
		return fmt.Errorf("set my commands: %w", err)
	}
	return nil
}

func (b *Bot) gcSessions(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n := b.sessions.gc(); n > 0 {
				b.log.Debug("expired dialogs dropped", slog.Int("count", n))
			}
		}
	}
}

// ------------------------------------------------------------------ middlewares

// recoverPanic keeps one bad update from taking the whole process down.
func (b *Bot) recoverPanic(next tg.HandlerFunc) tg.HandlerFunc {
	return func(ctx context.Context, api *tg.Bot, update *models.Update) {
		defer func() {
			if r := recover(); r != nil {
				b.log.Error("panic while handling update",
					slog.Any("panic", r),
					slog.Int64("update_id", update.ID))
			}
		}()
		next(ctx, api, update)
	}
}

func (b *Bot) logUpdate(next tg.HandlerFunc) tg.HandlerFunc {
	return func(ctx context.Context, api *tg.Bot, update *models.Update) {
		started := time.Now()
		user := updateUser(update)

		next(ctx, api, update)

		attrs := []any{
			slog.Int64("update_id", update.ID),
			slog.Duration("took", time.Since(started)),
		}
		if user != nil {
			attrs = append(attrs, slog.Int64("telegram_id", user.ID))
		}
		if update.Message != nil {
			attrs = append(attrs, slog.String("text", truncate(update.Message.Text, 64)))
		}
		if update.CallbackQuery != nil {
			attrs = append(attrs, slog.String("callback", update.CallbackQuery.Data))
		}
		b.log.Debug("update handled", attrs...)
	}
}

// authorize enforces the optional allowlist.
func (b *Bot) authorize(next tg.HandlerFunc) tg.HandlerFunc {
	return func(ctx context.Context, api *tg.Bot, update *models.Update) {
		if len(b.allowed) == 0 {
			next(ctx, api, update)
			return
		}
		user := updateUser(update)
		if user == nil {
			return
		}
		if _, ok := b.allowed[user.ID]; !ok {
			b.log.Warn("rejected update from non-allowlisted user", slog.Int64("telegram_id", user.ID))
			if chatID := updateChatID(update); chatID != 0 {
				b.reply(ctx, chatID, "Доступ к этому боту ограничен.")
			}
			return
		}
		next(ctx, api, update)
	}
}

// ------------------------------------------------------------------ helpers

// currentUser makes sure the Telegram user exists in the Store.
func (b *Bot) currentUser(ctx context.Context, update *models.Update) (domain.User, error) {
	tgUser := updateUser(update)
	if tgUser == nil {
		return domain.User{}, errors.New("update has no sender")
	}
	user, err := b.store.EnsureUser(ctx, tgUser.ID, tgUser.Username, tgUser.LanguageCode)
	if err != nil {
		return domain.User{}, fmt.Errorf("ensure user: %w", err)
	}
	return user, nil
}

// reply sends an HTML message and swallows-but-logs transport errors: there is
// nothing useful to do with them inside an update handler.
func (b *Bot) reply(ctx context.Context, chatID int64, text string) {
	b.replyMarkup(ctx, chatID, text, nil)
}

func (b *Bot) replyMarkup(ctx context.Context, chatID int64, text string, markup models.ReplyMarkup) {
	_, err := b.api.SendMessage(ctx, &tg.SendMessageParams{
		ChatID:             chatID,
		Text:               text,
		ParseMode:          models.ParseModeHTML,
		ReplyMarkup:        markup,
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: tg.True()},
	})
	if err != nil {
		b.log.Error("send message failed", slog.Int64("chat_id", chatID), slog.Any("error", err))
	}
}

func updateUser(update *models.Update) *models.User {
	switch {
	case update.Message != nil && update.Message.From != nil:
		return update.Message.From
	case update.CallbackQuery != nil:
		return &update.CallbackQuery.From
	default:
		return nil
	}
}

func updateChatID(update *models.Update) int64 {
	switch {
	case update.Message != nil:
		return update.Message.Chat.ID
	case update.CallbackQuery != nil && update.CallbackQuery.Message.Message != nil:
		return update.CallbackQuery.Message.Message.Chat.ID
	default:
		return 0
	}
}

func truncate(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}
