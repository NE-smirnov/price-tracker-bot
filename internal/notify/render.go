package notify

import (
	"fmt"
	"html"
	"strings"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

// Messages use Telegram's HTML parse mode, matching the bot: it needs escaping
// of three characters, where MarkdownV2 breaks on any '.' in a product title.
func esc(s string) string { return html.EscapeString(s) }

// maxTitleRunes keeps a notification readable. Shop titles routinely run past
// 200 characters of specifications, which would bury the price.
const maxTitleRunes = 70

// Render builds the message body for an alert.
func Render(alert Alert) string {
	var b strings.Builder

	switch alert.Kind {
	case KindPriceDrop:
		b.WriteString("📉 <b>Цена снизилась</b>\n\n")
	case KindAllTimeLow:
		b.WriteString("🔥 <b>Новый минимум</b>\n\n")
	case KindBackInStock:
		b.WriteString("✅ <b>Товар снова в наличии</b>\n\n")
	case KindOutOfStock:
		b.WriteString("⛔️ <b>Товар закончился</b>\n\n")
	case KindScrapeDegraded:
		b.WriteString("⚠️ <b>Не удаётся проверить цену</b>\n\n")
	default:
		b.WriteString("<b>Обновление по товару</b>\n\n")
	}

	b.WriteString(link(alert))
	b.WriteString("\n")

	switch alert.Kind {
	case KindPriceDrop, KindAllTimeLow:
		if alert.Price != nil {
			b.WriteString("\nЦена: <b>" + esc(alert.Price.String()) + "</b>")
			if alert.PreviousPrice != nil && alert.PreviousPrice.Currency == alert.Price.Currency {
				// The old price and the size of the change are what make the alert
				// actionable: "1499 ₽" alone does not say whether to hurry.
				b.WriteString(" (было " + esc(alert.PreviousPrice.String()) + drop(*alert.PreviousPrice, *alert.Price) + ")")
			}
		}
		if alert.OriginalPrice != nil {
			// The shop charges in its own currency; the user will see this number at
			// checkout even though the alert was judged on the converted one.
			b.WriteString("\nВ магазине: " + esc(alert.OriginalPrice.String()))
		}
		if alert.TargetPrice != nil {
			b.WriteString("\nЖелаемая: " + esc(alert.TargetPrice.String()))
		}

	case KindBackInStock:
		if alert.Price != nil {
			b.WriteString("\nЦена: <b>" + esc(alert.Price.String()) + "</b>")
		}

	case KindOutOfStock:
		if alert.Price != nil {
			b.WriteString("\nПоследняя известная цена: " + esc(alert.Price.String()))
		}

	case KindScrapeDegraded:
		b.WriteString("\nПроверки подряд завершаются ошибкой — возможно, магазин изменил страницу " +
			"или блокирует запросы. Отслеживание продолжается, но данные могут устареть.")
	}

	return b.String()
}

// link renders the title as a link when there is a URL, so a notification is one
// tap away from the shop.
func link(alert Alert) string {
	title := truncate(strings.TrimSpace(alert.Title), maxTitleRunes)
	if title == "" {
		title = "товар"
	}
	if alert.URL == "" {
		return esc(title)
	}
	return `<a href="` + esc(alert.URL) + `">` + esc(title) + `</a>`
}

// drop renders the change as a percentage. It is integer arithmetic on minor
// units, because a price is never a float in this project.
func drop(previous, current domain.Money) string {
	if previous.Amount <= 0 || current.Amount >= previous.Amount {
		return ""
	}
	// Percent with one decimal, computed in tenths to avoid floating point.
	tenths := (previous.Amount - current.Amount) * 1000 / previous.Amount
	if tenths == 0 {
		return ""
	}
	return fmt.Sprintf(", −%d.%d%%", tenths/10, tenths%10)
}

func truncate(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return strings.TrimSpace(string(runes[:limit])) + "…"
}
