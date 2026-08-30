// Package config loads service configuration from the environment.
// A local .env file is loaded first when present, real environment variables
// always win over it.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Common is shared by every binary in the monorepo.
type Common struct {
	AppEnv    string
	LogLevel  string
	LogFormat string
}

// Bot configures the Telegram bot process.
type Bot struct {
	Common
	Token        string
	AllowedUsers []int64
	Storage      string // "memory" | "core"
	CoreGRPCAddr string
	RedisAddr    string
	RedisPass    string
	RedisDB      int
}

// Core configures the data-owning service.
type Core struct {
	Common
	GRPCAddr string
	// PostgresDSN is required: core is the only service that owns the database,
	// so there is no meaningful default to fall back to.
	PostgresDSN string
	RedisAddr   string
	RedisPass   string
	RedisDB     int
	// StatsTTL is how long a computed Stats response stays cacheable. Long
	// enough to absorb a burst of /stats, short enough that a fresh scrape is
	// visible quickly.
	StatsTTL time.Duration
}

// Currency configures the exchange-rate service.
type Currency struct {
	Common
	GRPCAddr string
	// ProviderURL is the rate source. Empty means the built-in default, which is
	// the only keyless provider that publishes RUB.
	ProviderURL     string
	ProviderTimeout time.Duration
	// RateTTL is how long a fetched rate table is reused. Rates published daily
	// do not need to be fetched more often than hourly, and a free provider's
	// quota is the real constraint.
	RateTTL   time.Duration
	RedisAddr string
	RedisPass string
	RedisDB   int
}

// LoadCurrency reads the currency service configuration.
func LoadCurrency() (Currency, error) {
	cfg := Currency{
		Common:      LoadCommon(),
		GRPCAddr:    str("CURRENCY_GRPC_LISTEN", ":9091"),
		ProviderURL: str("CURRENCY_PROVIDER_URL", ""),
		RedisAddr:   str("REDIS_ADDR", ""),
		RedisPass:   str("REDIS_PASSWORD", ""),
	}

	var err error
	if cfg.RedisDB, err = intVal("REDIS_DB", 0); err != nil {
		return Currency{}, err
	}
	if cfg.RateTTL, err = Duration("CURRENCY_RATE_TTL", time.Hour); err != nil {
		return Currency{}, err
	}
	if cfg.ProviderTimeout, err = Duration("CURRENCY_PROVIDER_TIMEOUT", 10*time.Second); err != nil {
		return Currency{}, err
	}
	return cfg, nil
}

// LoadCore reads the core service configuration.
func LoadCore() (Core, error) {
	cfg := Core{
		Common:      LoadCommon(),
		GRPCAddr:    str("CORE_GRPC_LISTEN", ":9090"),
		PostgresDSN: str("POSTGRES_DSN", ""),
		RedisAddr:   str("REDIS_ADDR", ""),
		RedisPass:   str("REDIS_PASSWORD", ""),
	}

	var err error
	if cfg.RedisDB, err = intVal("REDIS_DB", 0); err != nil {
		return Core{}, err
	}
	if cfg.StatsTTL, err = Duration("CORE_STATS_TTL", 5*time.Minute); err != nil {
		return Core{}, err
	}
	if cfg.PostgresDSN == "" {
		return Core{}, errors.New("POSTGRES_DSN is required for the core service")
	}
	return cfg, nil
}

// LoadDotEnv loads ./.env if it exists. Missing file is not an error.
func LoadDotEnv() error {
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("load .env: %w", err)
	}
	return nil
}

// LoadCommon reads the shared settings.
func LoadCommon() Common {
	return Common{
		AppEnv:    str("APP_ENV", "local"),
		LogLevel:  str("LOG_LEVEL", "info"),
		LogFormat: str("LOG_FORMAT", "text"),
	}
}

// LoadBot reads the bot configuration and validates the required fields.
func LoadBot() (Bot, error) {
	cfg := Bot{
		Common:       LoadCommon(),
		Token:        str("TELEGRAM_BOT_TOKEN", ""),
		Storage:      strings.ToLower(str("BOT_STORAGE", "memory")),
		CoreGRPCAddr: str("CORE_GRPC_ADDR", "localhost:9090"),
		RedisAddr:    str("REDIS_ADDR", ""),
		RedisPass:    str("REDIS_PASSWORD", ""),
	}

	var err error
	if cfg.AllowedUsers, err = int64List("TELEGRAM_ALLOWED_USERS"); err != nil {
		return Bot{}, err
	}
	if cfg.RedisDB, err = intVal("REDIS_DB", 0); err != nil {
		return Bot{}, err
	}

	if cfg.Token == "" {
		return Bot{}, errors.New("TELEGRAM_BOT_TOKEN is required (get one from @BotFather)")
	}
	switch cfg.Storage {
	case "memory", "core":
	default:
		return Bot{}, fmt.Errorf("BOT_STORAGE must be 'memory' or 'core', got %q", cfg.Storage)
	}
	if cfg.Storage == "core" && cfg.CoreGRPCAddr == "" {
		return Bot{}, errors.New("CORE_GRPC_ADDR is required when BOT_STORAGE=core")
	}
	return cfg, nil
}

// ------------------------------------------------------------------ helpers

func str(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func intVal(key string, def int) (int, error) {
	raw := str(key, "")
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer, got %q", key, raw)
	}
	return v, nil
}

// Duration reads a Go duration such as "15s" or "1h".
func Duration(key string, def time.Duration) (time.Duration, error) {
	raw := str(key, "")
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration like 15s or 1h, got %q", key, raw)
	}
	return d, nil
}

func int64List(key string) ([]int64, error) {
	raw := str(key, "")
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		v, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s must be a comma-separated list of telegram ids, got %q", key, raw)
		}
		out = append(out, v)
	}
	return out, nil
}
