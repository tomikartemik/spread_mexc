package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
)

// Symbol describes a tradable instrument mapping between MEXC and DexScreener.
type Symbol struct {
	MexcSymbol   string `json:"mexc"`
	MexcContract string `json:"mexc_contract"`
	DexPair      string `json:"dex"`
	DexChain     string `json:"chain"`
	DexAddress   string `json:"address"`
	DexName      string `json:"name"`
	ChainIndex   string `json:"chainIndex"`
}

// Settings holds runtime configuration derived from environment variables.
type Settings struct {
	MexcAuthToken    string
	MexcBaseURL      string
	MexcTimeout      time.Duration
	QuoteSize        float64
	Leverage         int
	EntryThreshold   float64
	ExitThreshold    float64
	StopLoss         float64
	EnterDelta       float64
	ConfirmDrop      float64
	ConfirmDuration  time.Duration
	MinEntrySpread   float64
	DecisionInterval time.Duration
	MexcPollInterval time.Duration
	MexcPriceWorkers int
	DebugPrices      bool
	Symbols          []ResolvedSymbol
	Telegram         *TelegramSettings
	OKX              OKXSettings
}

// TelegramSettings описывает параметры уведомлений в Telegram.
type TelegramSettings struct {
	BotToken string
	ChatID   string
}

// OKXSettings определяет параметры доступа к OKX Web3 API.
type OKXSettings struct {
	BaseURL      string
	AccessKey    string
	SecretKey    string
	Passphrase   string
	PollInterval time.Duration
}

// ResolvedSymbol contains derived fields used by the trading engine.
type ResolvedSymbol struct {
	MexcSymbol string
	RestSymbol string
	DexPair    string
	DexChain   string
	DexAddress string
	ChainIndex string
}

// Load reads configuration from the current environment.
func Load() (Settings, error) {
	ensureEnvLoaded()
	cfg := Settings{}

	token := strings.TrimSpace(os.Getenv("MEXC_WEB_AUTH_TOKEN"))
	if token == "" {
		return cfg, errors.New("MEXC_WEB_AUTH_TOKEN is required")
	}
	cfg.MexcAuthToken = token
	cfg.MexcBaseURL = strings.TrimSpace(os.Getenv("MEXC_WEB_BASE_URL"))
	if cfg.MexcBaseURL == "" {
		cfg.MexcBaseURL = "https://futures.mexc.com/api/v1"
	}
	cfg.MexcTimeout = durationFromEnv("MEXC_WEB_TIMEOUT", 30*time.Second)

	cfg.QuoteSize = floatFromEnv("ARBITRAGE_QUOTE_SIZE", 5)
	cfg.Leverage = intFromEnv("ARBITRAGE_LEVERAGE", 10)
	if cfg.Leverage < 1 {
		return cfg, errors.New("ARBITRAGE_LEVERAGE must be >= 1")
	}

	cfg.EntryThreshold = floatFromEnv("ARBITRAGE_ENTRY_THRESHOLD", 7)
	cfg.ExitThreshold = floatFromEnv("ARBITRAGE_EXIT_THRESHOLD", 2)
	cfg.StopLoss = floatFromEnv("ARBITRAGE_STOP_LOSS", 1)
	cfg.EnterDelta = floatFromEnv("ARBITRAGE_ENTER_DELTA", 2)
	cfg.ConfirmDrop = floatFromEnv("ARBITRAGE_CONFIRM_DROP", 0.3)
	cfg.MinEntrySpread = floatFromEnv("ARBITRAGE_MIN_ENTRY_SPREAD", 8)
	cfg.ConfirmDuration = durationFromEnv("ARBITRAGE_CONFIRM_DURATION", 500*time.Millisecond)
	cfg.DecisionInterval = durationFromEnv("ARBITRAGE_DECISION_INTERVAL", time.Second)
	cfg.MexcPollInterval = durationFromEnv("MEXC_PRICE_POLL_INTERVAL", 5*time.Second)
	cfg.MexcPriceWorkers = intFromEnv("MEXC_PRICE_WORKERS", 8)
	if cfg.MexcPriceWorkers < 1 {
		cfg.MexcPriceWorkers = 1
	}
	cfg.DebugPrices = boolFromEnv("ARBITRAGE_DEBUG_PRICES", false)

	symbols, err := loadSymbols()
	if err != nil {
		return cfg, err
	}
	cfg.Symbols = symbols

	botToken := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	chatID := strings.TrimSpace(os.Getenv("TELEGRAM_CHAT_ID"))
	if botToken != "" && chatID != "" {
		cfg.Telegram = &TelegramSettings{
			BotToken: botToken,
			ChatID:   chatID,
		}
	}

	cfg.OKX = OKXSettings{
		BaseURL:      strings.TrimSpace(os.Getenv("OKX_WEB3_BASE_URL")),
		AccessKey:    firstNonEmpty(os.Getenv("OKX_WEB3_ACCESS_KEY"), os.Getenv("OKX_ACCESS_KEY")),
		SecretKey:    firstNonEmpty(os.Getenv("OKX_WEB3_SECRET"), os.Getenv("OKX_ACCESS_SECRET")),
		Passphrase:   firstNonEmpty(os.Getenv("OKX_WEB3_PASSPHRASE"), os.Getenv("OKX_ACCESS_PASSPHRASE")),
		PollInterval: durationFromEnv("OKX_PRICE_POLL_INTERVAL", 2*time.Second),
	}
	if cfg.OKX.BaseURL == "" {
		cfg.OKX.BaseURL = "https://web3.okx.com"
	}
	if cfg.OKX.PollInterval <= 0 {
		cfg.OKX.PollInterval = 2 * time.Second
	}
	if cfg.OKX.AccessKey == "" || cfg.OKX.SecretKey == "" || cfg.OKX.Passphrase == "" {
		return cfg, errors.New("OKX_WEB3_ACCESS_KEY/SECRET/PASSPHRASE are required")
	}

	return cfg, nil
}

func loadSymbols() ([]ResolvedSymbol, error) {
	raw := strings.TrimSpace(os.Getenv("SYMBOLS_CONFIG"))
	var entries []Symbol
	sourceFile := ""
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &entries); err != nil {
			return nil, fmt.Errorf("failed to parse SYMBOLS_CONFIG: %w", err)
		}
	} else {
		path := strings.TrimSpace(os.Getenv("SYMBOLS_FILE"))
		if path == "" {
			path = "spread_pairs.json"
		}
		sourceFile = path
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read symbols file %s: %w", path, err)
		}
		if err := json.Unmarshal(data, &entries); err != nil {
			return nil, fmt.Errorf("parse symbols file %s: %w", path, err)
		}
	}
	if len(entries) == 0 {
		return nil, errors.New("symbols list is empty")
	}
	if len(entries) > 400 && raw == "" {
		if sourceFile == "" {
			sourceFile = strings.TrimSpace(os.Getenv("SYMBOLS_FILE"))
			if sourceFile == "" {
				sourceFile = "spread_pairs.json"
			}
		}
		fmt.Printf("Loaded %d symbols from %s; consider reducing to avoid rate limits.\n", len(entries), sourceFile)
	}
	resolved := make([]ResolvedSymbol, 0, len(entries))
	for _, entry := range entries {
		mexc := strings.TrimSpace(entry.MexcSymbol)
		if mexc == "" {
			return nil, errors.New("SYMBOLS_CONFIG entries must include 'mexc'")
		}
		dexPair := strings.TrimSpace(entry.DexPair)
		if dexPair == "" {
			dexPair = buildDexPair(entry)
			if dexPair == "" {
				return nil, fmt.Errorf("symbol %s is missing Dex mapping", mexc)
			}
		}
		chainIndex := strings.TrimSpace(entry.ChainIndex)
		if chainIndex == "" {
			chainIndex = deriveChainIndex(entry.DexChain)
		}
		if chainIndex == "" {
			return nil, fmt.Errorf("symbol %s is missing chain index", mexc)
		}
		address := strings.TrimSpace(entry.DexAddress)
		if address == "" {
			address = strings.TrimSpace(entry.DexName)
		}
		if address == "" {
			return nil, fmt.Errorf("symbol %s is missing Dex address", mexc)
		}
		restSymbol := strings.TrimSpace(entry.MexcContract)
		if restSymbol == "" {
			restSymbol = symbolToRest(mexc)
		}
		resolved = append(resolved, ResolvedSymbol{
			MexcSymbol: mexc,
			RestSymbol: restSymbol,
			DexPair:    dexPair,
			DexChain:   strings.TrimSpace(entry.DexChain),
			DexAddress: address,
			ChainIndex: chainIndex,
		})
	}
	return resolved, nil
}

func buildDexPair(entry Symbol) string {
	chain := strings.TrimSpace(entry.DexChain)
	if chain == "" {
		return ""
	}
	identifier := strings.TrimSpace(entry.DexAddress)
	if identifier == "" {
		identifier = strings.TrimSpace(entry.DexName)
	}
	if identifier == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s", chain, identifier)
}

func deriveChainIndex(chain string) string {
	switch strings.ToLower(strings.TrimSpace(chain)) {
	case "sol", "solana":
		return "501"
	case "eth", "ethereum":
		return "1"
	case "bsc", "bscscan", "bnb", "binance smart chain":
		return "102"
	default:
		return strings.TrimSpace(chain)
	}
}

func symbolToRest(symbol string) string {
	base := symbol
	if idx := strings.Index(symbol, ":"); idx >= 0 {
		base = symbol[:idx]
	}
	return strings.ReplaceAll(base, "/", "_")
}

func floatFromEnv(key string, def float64) float64 {
	val := strings.TrimSpace(os.Getenv(key))
	if val == "" {
		return def
	}
	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return def
	}
	return f
}

func intFromEnv(key string, def int) int {
	val := strings.TrimSpace(os.Getenv(key))
	if val == "" {
		return def
	}
	i, err := strconv.Atoi(val)
	if err != nil {
		return def
	}
	return i
}

func boolFromEnv(key string, def bool) bool {
	val := strings.TrimSpace(os.Getenv(key))
	if val == "" {
		return def
	}
	lower := strings.ToLower(val)
	if lower == "1" || lower == "true" || lower == "yes" || lower == "on" {
		return true
	}
	if lower == "0" || lower == "false" || lower == "no" || lower == "off" {
		return false
	}
	return def
}

func durationFromEnv(key string, def time.Duration) time.Duration {
	val := strings.TrimSpace(os.Getenv(key))
	if val == "" {
		return def
	}
	if dur, err := time.ParseDuration(val); err == nil {
		return dur
	}
	if f, err := strconv.ParseFloat(val, 64); err == nil {
		return time.Duration(f * float64(time.Second))
	}
	return def
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		trimmed := strings.TrimSpace(v)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

var envOnce sync.Once

func ensureEnvLoaded() {
	envOnce.Do(func() {
		_ = godotenv.Load()
	})
}
