package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	tg "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

// statsWindow is the period /stats aggregates over.
const statsWindow = 14 * 24 * time.Hour

// ------------------------------------------------------------------ commands

func (b *Bot) onStart(ctx context.Context, _ *tg.Bot, update *models.Update) {
	chatID := updateChatID(update)
	if _, err := b.currentUser(ctx, update); err != nil {
		b.failure(ctx, chatID, "onStart", err)
		return
	}
	b.sessions.reset(update.Message.From.ID)
	b.reply(ctx, chatID, msgStart)
}

func (b *Bot) onHelp(ctx context.Context, _ *tg.Bot, update *models.Update) {
	b.reply(ctx, updateChatID(update), fmt.Sprintf(msgHelp,
		domain.MaxItemsPerUser,
		humanDuration(domain.MinCheckInterval),
		humanDuration(domain.MaxCheckInterval),
	))
}

// onAdd starts the add-item dialog. It also supports the one-liner form
// "/add <url>" so power users can skip a round trip.
func (b *Bot) onAdd(ctx context.Context, _ *tg.Bot, update *models.Update) {
	chatID := updateChatID(update)
	user, err := b.currentUser(ctx, update)
	if err != nil {
		b.failure(ctx, chatID, "onAdd", err)
		return
	}

	if arg := commandArg(update.Message.Text); arg != "" {
		b.acceptURL(ctx, chatID, user.TelegramID, arg)
		return
	}

	b.sessions.set(user.TelegramID, session{Step: stepAwaitURL})
	b.reply(ctx, chatID, msgAskURL)
}

func (b *Bot) onList(ctx context.Context, _ *tg.Bot, update *models.Update) {
	chatID := updateChatID(update)
	user, err := b.currentUser(ctx, update)
	if err != nil {
		b.failure(ctx, chatID, "onList", err)
		return
	}

	items, err := b.store.ListItems(ctx, user.ID)
	if err != nil {
		b.failure(ctx, chatID, "list items", err)
		return
	}
	if len(items) == 0 {
		b.reply(ctx, chatID, msgNoItems)
		return
	}
	b.reply(ctx, chatID, renderItemList(items, user.DefaultCurrency))
}

func (b *Bot) onRemove(ctx context.Context, _ *tg.Bot, update *models.Update) {
	b.pickItem(ctx, update, actRemove, "Какой товар убрать?")
}

func (b *Bot) onStats(ctx context.Context, _ *tg.Bot, update *models.Update) {
	b.pickItem(ctx, update, actStats, "По какому товару показать статистику?")
}

// pickItem renders the item keyboard shared by /remove and /stats.
func (b *Bot) pickItem(ctx context.Context, update *models.Update, action, prompt string) {
	chatID := updateChatID(update)
	user, err := b.currentUser(ctx, update)
	if err != nil {
		b.failure(ctx, chatID, "pick item", err)
		return
	}

	items, err := b.store.ListItems(ctx, user.ID)
	if err != nil {
		b.failure(ctx, chatID, "list items", err)
		return
	}
	if len(items) == 0 {
		b.reply(ctx, chatID, msgNoItems)
		return
	}
	b.replyMarkup(ctx, chatID, prompt, itemKeyboard(items, action))
}

func (b *Bot) onSettings(ctx context.Context, _ *tg.Bot, update *models.Update) {
	chatID := updateChatID(update)
	user, err := b.currentUser(ctx, update)
	if err != nil {
		b.failure(ctx, chatID, "onSettings", err)
		return
	}

	b.sessions.set(user.TelegramID, session{Step: stepAwaitCurrency})
	text := fmt.Sprintf(
		"Валюта отображения сейчас: <b>%s</b>\n\nВыбери из списка или пришли код ISO-4217 (например <code>PLN</code>).",
		esc(string(user.DefaultCurrency)))
	b.replyMarkup(ctx, chatID, text, currencyKeyboard())
}

func (b *Bot) onCancel(ctx context.Context, _ *tg.Bot, update *models.Update) {
	chatID := updateChatID(update)
	user := updateUser(update)
	if user == nil {
		return
	}
	if b.sessions.get(user.ID).Step == stepIdle {
		b.reply(ctx, chatID, msgNothingToCancel)
		return
	}
	b.sessions.reset(user.ID)
	b.reply(ctx, chatID, msgCancelled)
}

// ------------------------------------------------------------------ free text

// onFreeText is the default handler: it interprets plain messages according to
// the current step of the user's dialog.
func (b *Bot) onFreeText(ctx context.Context, _ *tg.Bot, update *models.Update) {
	if update.Message == nil || update.Message.From == nil {
		return
	}
	chatID := update.Message.Chat.ID
	text := strings.TrimSpace(update.Message.Text)

	// An unknown slash command must not be parsed as a URL or a price.
	if strings.HasPrefix(text, "/") {
		b.reply(ctx, chatID, "Не знаю такой команды. Посмотри /help")
		return
	}

	user, err := b.currentUser(ctx, update)
	if err != nil {
		b.failure(ctx, chatID, "onFreeText", err)
		return
	}
	sess := b.sessions.get(user.TelegramID)

	switch sess.Step {
	case stepAwaitURL:
		b.acceptURL(ctx, chatID, user.TelegramID, text)

	case stepAwaitTarget:
		b.acceptTarget(ctx, chatID, user, sess, text)

	case stepAwaitCurrency:
		b.acceptCurrency(ctx, chatID, user, text)

	case stepAwaitInterval:
		b.acceptTypedInterval(ctx, chatID, user, sess, text)

	case stepIdle:
		// A bare link outside of any dialog is treated as "/add <link>".
		if looksLikeURL(text) {
			b.acceptURL(ctx, chatID, user.TelegramID, text)
			return
		}
		b.reply(ctx, chatID, "Не понял. Начни с /add или посмотри /help")

	default:
		b.log.Warn("unknown dialog step", slog.String("step", string(sess.Step)))
		b.sessions.reset(user.TelegramID)
		b.reply(ctx, chatID, "Что-то пошло не так, диалог сброшен. Попробуй /add")
	}
}

func (b *Bot) acceptURL(ctx context.Context, chatID, telegramID int64, raw string) {
	normalized, err := domain.NormalizeURL(raw)
	if err != nil {
		b.reply(ctx, chatID, "Не похоже на ссылку на товар: "+esc(userMessage(err))+"\n\nПришли http/https ссылку или /cancel")
		return
	}

	b.sessions.set(telegramID, session{
		Step:  stepAwaitTarget,
		Draft: draft{URL: normalized},
	})
	b.reply(ctx, chatID, fmt.Sprintf(msgAskTarget, esc(normalized)))
}

func (b *Bot) acceptTarget(ctx context.Context, chatID int64, user domain.User, sess session, text string) {
	if text != "-" && !strings.EqualFold(text, "skip") {
		money, err := domain.ParseMoney(text, user.DefaultCurrency)
		if errors.Is(err, domain.ErrAmbiguousSeparator) {
			b.reply(ctx, chatID, "Непонятно, что значит точка: <code>1.234</code> — это 1234 или 1.23?\n\nНапиши без разделителя тысяч: <code>1234</code> или <code>1234.50</code>")
			return
		}
		if err != nil {
			b.reply(ctx, chatID, "Не смог разобрать цену. Примеры: <code>19.99 USD</code>, <code>1500 TRY</code>\n\nИли пришли <code>-</code>, чтобы пропустить.")
			return
		}
		sess.Draft.Target = &money
	}

	sess.Step = stepAwaitInterval
	b.sessions.set(user.TelegramID, sess)
	b.replyMarkup(ctx, chatID, msgAskInterval, intervalKeyboard())
}

func (b *Bot) acceptTypedInterval(ctx context.Context, chatID int64, user domain.User, sess session, text string) {
	d, err := time.ParseDuration(strings.ToLower(strings.ReplaceAll(text, " ", "")))
	if err != nil {
		b.reply(ctx, chatID, "Выбери интервал кнопкой или пришли его как <code>30m</code>, <code>2h</code>, <code>12h</code>.")
		return
	}
	b.finishAdd(ctx, chatID, user, sess, d)
}

// finishAdd persists the drafted item and reports the result.
func (b *Bot) finishAdd(ctx context.Context, chatID int64, user domain.User, sess session, interval time.Duration) {
	if sess.Draft.URL == "" {
		b.sessions.reset(user.TelegramID)
		b.reply(ctx, chatID, "Диалог устарел. Начни заново: /add")
		return
	}

	validated, err := domain.ValidateInterval(interval)
	if err != nil {
		b.reply(ctx, chatID, fmt.Sprintf("Интервал должен быть от %s до %s.",
			humanDuration(domain.MinCheckInterval), humanDuration(domain.MaxCheckInterval)))
		return
	}

	item, err := b.store.AddItem(ctx, AddItemInput{
		UserID:   user.ID,
		URL:      sess.Draft.URL,
		Target:   sess.Draft.Target,
		Interval: validated,
	})
	if err != nil {
		b.sessions.reset(user.TelegramID)
		switch {
		case errors.Is(err, domain.ErrAlreadyExist):
			b.reply(ctx, chatID, "Этот товар уже отслеживается. Посмотри /list")
		case errors.Is(err, domain.ErrLimitReached):
			b.reply(ctx, chatID, fmt.Sprintf("Достигнут лимит в %d товаров. Убери что-нибудь: /remove", domain.MaxItemsPerUser))
		default:
			b.failure(ctx, chatID, "add item", err)
		}
		return
	}

	b.sessions.reset(user.TelegramID)

	target := "любое снижение цены"
	if item.TargetPrice != nil {
		target = "цена ниже " + item.TargetPrice.String()
	}
	b.reply(ctx, chatID, fmt.Sprintf(
		"Готово, отслеживаю:\n<a href=\"%s\">%s</a>\n\nусловие: %s\nпроверка: раз в %s\n\nПосмотреть всё: /list",
		esc(item.URL), esc(item.Title), esc(target), esc(humanDuration(item.CheckInterval))))
}

func (b *Bot) acceptCurrency(ctx context.Context, chatID int64, user domain.User, text string) {
	currency := domain.NormalizeCurrency(text)
	if !domain.ValidCurrency(currency) {
		b.reply(ctx, chatID, "Нужен трёхбуквенный код ISO-4217, например <code>USD</code> или <code>TRY</code>.")
		return
	}
	if err := b.store.SetDefaultCurrency(ctx, user.ID, currency); err != nil {
		b.failure(ctx, chatID, "set currency", err)
		return
	}
	b.sessions.reset(user.TelegramID)
	b.reply(ctx, chatID, "Валюта отображения: <b>"+esc(string(currency))+"</b>")
}

// ------------------------------------------------------------------ callbacks

func (b *Bot) onCallback(ctx context.Context, _ *tg.Bot, update *models.Update) {
	cq := update.CallbackQuery
	if cq == nil {
		return
	}
	// Telegram shows a spinner on the button until the query is answered.
	b.ackCallback(ctx, cq.ID, "")

	chatID := updateChatID(update)
	if chatID == 0 {
		return
	}

	action, arg, ok := parseCallback(cq.Data)
	if !ok {
		b.log.Warn("unparsable callback data", slog.String("data", cq.Data))
		return
	}

	user, err := b.currentUser(ctx, update)
	if err != nil {
		b.failure(ctx, chatID, "callback: ensure user", err)
		return
	}

	switch action {
	case actCancel:
		b.sessions.reset(user.TelegramID)
		b.reply(ctx, chatID, msgCancelled)

	case actInterval:
		minutes, convErr := strconv.Atoi(arg)
		if convErr != nil {
			b.log.Warn("bad interval in callback", slog.String("arg", arg))
			return
		}
		sess := b.sessions.get(user.TelegramID)
		if sess.Step != stepAwaitInterval {
			b.reply(ctx, chatID, "Эта кнопка из старого диалога. Начни заново: /add")
			return
		}
		b.finishAdd(ctx, chatID, user, sess, time.Duration(minutes)*time.Minute)

	case actRemove:
		if err := b.store.RemoveItem(ctx, user.ID, arg); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				b.reply(ctx, chatID, "Товар не найден — возможно, он уже удалён.")
				return
			}
			b.failure(ctx, chatID, "remove item", err)
			return
		}
		b.reply(ctx, chatID, "Убрал из отслеживания. Осталось: /list")

	case actStats:
		b.sendStats(ctx, chatID, user, arg)

	case actCurrency:
		b.acceptCurrency(ctx, chatID, user, arg)

	case actNoop:
		// A button whose payload did not fit into 64 bytes.
		b.reply(ctx, chatID, "Кнопка устарела, вызови команду заново.")

	default:
		b.log.Warn("unknown callback action", slog.String("action", action))
	}
}

func (b *Bot) sendStats(ctx context.Context, chatID int64, user domain.User, itemID string) {
	item, err := b.store.GetItem(ctx, user.ID, itemID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			b.reply(ctx, chatID, "Товар не найден.")
			return
		}
		b.failure(ctx, chatID, "get item", err)
		return
	}

	st, err := b.store.Stats(ctx, user.ID, itemID, statsWindow)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			b.reply(ctx, chatID, "По этому товару ещё нет истории цен — дождись первой проверки.")
			return
		}
		b.failure(ctx, chatID, "stats", err)
		return
	}

	history, err := b.store.History(ctx, user.ID, itemID, statsWindow)
	if err != nil {
		b.log.Warn("cannot load history for sparkline", slog.Any("error", err))
	}
	b.reply(ctx, chatID, renderStats(item, st, history))
}

func (b *Bot) ackCallback(ctx context.Context, callbackQueryID, text string) {
	if _, err := b.api.AnswerCallbackQuery(ctx, &tg.AnswerCallbackQueryParams{
		CallbackQueryID: callbackQueryID,
		Text:            text,
	}); err != nil {
		b.log.Warn("answer callback query failed", slog.Any("error", err))
	}
}

// ------------------------------------------------------------------ misc

// failure logs the technical error and shows the user a neutral message.
// Internal details (DSNs, gRPC targets, stack context) never reach the chat.
func (b *Bot) failure(ctx context.Context, chatID int64, op string, err error) {
	b.log.Error("handler failed", slog.String("op", op), slog.Any("error", err))
	if chatID != 0 {
		b.reply(ctx, chatID, "Внутренняя ошибка, попробуй ещё раз через минуту.")
	}
}

// userMessage strips the sentinel wrapper so the chat text stays readable.
func userMessage(err error) string {
	msg := err.Error()
	if idx := strings.Index(msg, ": "); errors.Is(err, domain.ErrValidation) && idx >= 0 {
		return msg[idx+2:]
	}
	return msg
}

// commandArg returns everything after the first space of a command message.
func commandArg(text string) string {
	_, arg, found := strings.Cut(strings.TrimSpace(text), " ")
	if !found {
		return ""
	}
	return strings.TrimSpace(arg)
}

func looksLikeURL(text string) bool {
	if strings.ContainsAny(text, " \n\t") {
		return false
	}
	return strings.HasPrefix(text, "http://") || strings.HasPrefix(text, "https://") ||
		(strings.Contains(text, ".") && strings.Contains(text, "/"))
}
