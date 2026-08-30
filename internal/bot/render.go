package bot

import (
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

// All outgoing messages use Telegram's HTML parse mode: it needs escaping of
// exactly three characters, unlike MarkdownV2 where a single unescaped '.' in a
// product title breaks the whole message.
func esc(s string) string { return html.EscapeString(s) }

const (
	msgStart = `<b>Price Tracker</b> — следит за ценами и наличием товаров.

Что умею:
• /add — добавить товар в отслеживание
• /list — список отслеживаемых товаров
• /stats — график и статистика по товару
• /remove — убрать товар
• /settings — валюта отображения
• /cancel — прервать текущий диалог

Начни с /add и пришли ссылку на товар.`

	msgHelp = `<b>Как это работает</b>

1. /add — присылаешь ссылку, желаемую цену и интервал проверки.
2. Бот периодически опрашивает страницу товара и пишет историю цен.
3. Как только цена падает ниже желаемой или товар появляется в наличии — приходит уведомление.
4. /stats показывает минимум, максимум, среднее и тренд за период.

Ограничения: до %d товаров, интервал проверки от %s до %s.`

	msgAskURL = `Пришли ссылку на товар (http/https).

Отменить: /cancel`

	msgAskTarget = `Ссылка принята: %s

Теперь желаемая цена — на неё сработает уведомление.
Примеры: <code>19.99 USD</code>, <code>1500 TRY</code>, <code>4990 RUB</code>

Можно пропустить (тогда уведомлю о любом снижении): напиши <code>-</code>`

	msgAskInterval = `Как часто проверять страницу?`

	msgCancelled = `Диалог прерван. Ничего не сохранено.`

	msgNoItems = `Пока пусто. Добавь первый товар: /add`

	msgNothingToCancel = `Нечего прерывать — активного диалога нет.`
)

// renderItemList builds the /list message.
func renderItemList(items []domain.TrackedItem, userCurrency domain.Currency) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<b>Отслеживаемые товары (%d/%d)</b>\n", len(items), domain.MaxItemsPerUser)

	for i, it := range items {
		fmt.Fprintf(&b, "\n%d. <a href=\"%s\">%s</a>\n", i+1, esc(it.URL), esc(it.Title))

		if it.LastSnapshot != nil {
			stock := "нет в наличии"
			if it.LastSnapshot.InStock {
				stock = "в наличии"
			}
			fmt.Fprintf(&b, "   цена: <b>%s</b> · %s\n", esc(it.LastSnapshot.Price.String()), stock)
			if it.LastSnapshot.Converted != nil && it.LastSnapshot.Converted.Currency != it.LastSnapshot.Price.Currency {
				fmt.Fprintf(&b, "   ≈ %s\n", esc(it.LastSnapshot.Converted.String()))
			}
		} else {
			b.WriteString("   цена: ещё не проверялась\n")
		}

		if it.TargetPrice != nil {
			fmt.Fprintf(&b, "   цель: %s\n", esc(it.TargetPrice.String()))
		}
		fmt.Fprintf(&b, "   проверка: раз в %s", esc(humanDuration(it.CheckInterval)))
		if !it.Active {
			b.WriteString(" · <i>на паузе</i>")
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "\nВалюта отображения: <b>%s</b> (/settings)", esc(string(userCurrency)))
	return b.String()
}

// renderStats builds the /stats message including a compact ASCII sparkline.
func renderStats(item domain.TrackedItem, st domain.Stats, history []domain.PriceSnapshot) string {
	var b strings.Builder

	fmt.Fprintf(&b, "<b>%s</b>\n<a href=\"%s\">открыть страницу товара</a>\n\n", esc(item.Title), esc(item.URL))
	fmt.Fprintf(&b, "текущая:  <b>%s</b>\n", esc(st.Current.String()))
	fmt.Fprintf(&b, "минимум:  %s\n", esc(st.Min.String()))
	fmt.Fprintf(&b, "максимум: %s\n", esc(st.Max.String()))
	fmt.Fprintf(&b, "средняя:  %s\n", esc(st.Avg.String()))

	fmt.Fprintf(&b, "тренд:    %s %s%.1f%%\n", trendIcon(st.Trend()), sign(st.ChangePercent()), st.ChangePercent())

	stock := "нет в наличии"
	if st.InStock {
		stock = "в наличии"
	}
	fmt.Fprintf(&b, "наличие:  %s\n", stock)
	fmt.Fprintf(&b, "выборка:  %d наблюдений за %s\n", st.Samples, esc(humanDuration(st.WindowTo.Sub(st.WindowFrom))))

	if spark := sparkline(history, 24); spark != "" {
		fmt.Fprintf(&b, "\n<code>%s</code>\n<i>динамика: %s → %s</i>",
			esc(spark), esc(st.First.String()), esc(st.Current.String()))
	}
	return b.String()
}

var sparkRunes = []rune("▁▂▃▄▅▆▇█")

// sparkline downsamples a price history into a fixed-width unicode bar chart.
// A picture beats four numbers in a chat window, and it costs no dependency.
func sparkline(history []domain.PriceSnapshot, width int) string {
	if len(history) < 2 || width < 2 {
		return ""
	}

	// Bucket the samples so the output always has exactly `width` columns.
	buckets := make([]int64, 0, width)
	step := float64(len(history)) / float64(width)
	for i := range width {
		start := int(float64(i) * step)
		end := int(float64(i+1) * step)
		if end > len(history) {
			end = len(history)
		}
		if start >= end {
			if start >= len(history) {
				start = len(history) - 1
			}
			end = start + 1
		}
		var sum int64
		for _, snap := range history[start:end] {
			sum += snap.Price.Amount
		}
		buckets = append(buckets, sum/int64(end-start))
	}

	minV, maxV := buckets[0], buckets[0]
	for _, v := range buckets {
		minV = min(minV, v)
		maxV = max(maxV, v)
	}

	var b strings.Builder
	span := maxV - minV
	for _, v := range buckets {
		idx := 0
		if span > 0 {
			idx = int((v - minV) * int64(len(sparkRunes)-1) / span)
		}
		b.WriteRune(sparkRunes[idx])
	}
	return b.String()
}

func trendIcon(d domain.TrendDirection) string {
	switch d {
	case domain.TrendDown:
		return "↓ вниз"
	case domain.TrendUp:
		return "↑ вверх"
	default:
		return "→ ровно"
	}
}

func sign(v float64) string {
	if v > 0 {
		return "+"
	}
	return ""
}

// humanDuration renders a duration the way a person would say it in Russian.
func humanDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "—"
	case d < time.Hour:
		return fmt.Sprintf("%d мин", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		if m := int(d.Minutes()) % 60; m != 0 {
			return fmt.Sprintf("%d ч %d мин", h, m)
		}
		return fmt.Sprintf("%d ч", h)
	default:
		days := int(d.Hours()) / 24
		if h := int(d.Hours()) % 24; h != 0 {
			return fmt.Sprintf("%d д %d ч", days, h)
		}
		return fmt.Sprintf("%d д", days)
	}
}
