package bot

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/NE-smirnov/price-tracker-bot/internal/domain"
)

// Callback data is versioned: Telegram keeps old inline keyboards alive in chat
// history forever, so a v2 codec can be added later without misreading v1 taps.
// Hard limit from the Bot API: 64 bytes.
const (
	cbPrefix        = "v1:"
	cbMaxDataLength = 64
)

// Callback actions.
const (
	actInterval = "int"    // int:<minutes>
	actRemove   = "rm"     // rm:<item-uuid>
	actStats    = "st"     // st:<item-uuid>
	actCurrency = "cur"    // cur:<ISO-4217>
	actCancel   = "cancel" // cancel
	actNoop     = "noop"
)

func cbData(action, arg string) string {
	data := cbPrefix + action
	if arg != "" {
		data += ":" + arg
	}
	if len(data) > cbMaxDataLength {
		// Better to lose a button than to have Telegram reject the whole keyboard.
		return cbPrefix + actNoop
	}
	return data
}

// parseCallback splits "v1:<action>:<arg>" into its parts.
func parseCallback(data string) (action, arg string, ok bool) {
	rest, found := strings.CutPrefix(data, cbPrefix)
	if !found || rest == "" {
		return "", "", false
	}
	action, arg, _ = strings.Cut(rest, ":")
	return action, arg, action != ""
}

// intervalChoices are the presets offered in the /add flow.
var intervalChoices = []time.Duration{
	15 * time.Minute,
	time.Hour,
	6 * time.Hour,
	12 * time.Hour,
	24 * time.Hour,
}

func intervalKeyboard() *models.InlineKeyboardMarkup {
	row := make([]models.InlineKeyboardButton, 0, len(intervalChoices))
	rows := make([][]models.InlineKeyboardButton, 0, 3)
	for i, d := range intervalChoices {
		row = append(row, models.InlineKeyboardButton{
			Text:         humanDuration(d),
			CallbackData: cbData(actInterval, strconv.Itoa(int(d.Minutes()))),
		})
		if len(row) == 3 || i == len(intervalChoices)-1 {
			rows = append(rows, row)
			row = make([]models.InlineKeyboardButton, 0, 3)
		}
	}
	rows = append(rows, []models.InlineKeyboardButton{{
		Text:         "отмена",
		CallbackData: cbData(actCancel, ""),
	}})
	return &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// itemKeyboard renders one button per item, used by /remove and /stats.
func itemKeyboard(items []domain.TrackedItem, action string) *models.InlineKeyboardMarkup {
	rows := make([][]models.InlineKeyboardButton, 0, len(items)+1)
	for i, it := range items {
		rows = append(rows, []models.InlineKeyboardButton{{
			Text:         fmt.Sprintf("%d. %s", i+1, truncate(it.Title, 40)),
			CallbackData: cbData(action, it.ID),
		}})
	}
	rows = append(rows, []models.InlineKeyboardButton{{
		Text:         "отмена",
		CallbackData: cbData(actCancel, ""),
	}})
	return &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// popularCurrencies are offered in /settings; any ISO code can still be typed.
var popularCurrencies = []domain.Currency{"USD", "EUR", "TRY", "RUB", "GBP", "KZT"}

func currencyKeyboard() *models.InlineKeyboardMarkup {
	rows := make([][]models.InlineKeyboardButton, 0, 3)
	row := make([]models.InlineKeyboardButton, 0, 3)
	for i, c := range popularCurrencies {
		row = append(row, models.InlineKeyboardButton{
			Text:         string(c),
			CallbackData: cbData(actCurrency, string(c)),
		})
		if len(row) == 3 || i == len(popularCurrencies)-1 {
			rows = append(rows, row)
			row = make([]models.InlineKeyboardButton, 0, 3)
		}
	}
	rows = append(rows, []models.InlineKeyboardButton{{
		Text:         "отмена",
		CallbackData: cbData(actCancel, ""),
	}})
	return &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}
