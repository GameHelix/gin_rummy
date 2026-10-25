package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/ismatsamadov/trader/internal/store"
	"slices"
)

// Config describes one paper-trading agent. Every field is required in the
// JSON file so a missing setting never silently falls back to a default.
type Config struct {
	Name                string   `json:"name"`
	Symbols             []string `json:"symbols"`
	MarketSymbol        string   `json:"market_symbol"`
	InitialCash         float64  `json:"initial_cash"`
	Commission          float64  `json:"commission"`
	SlippageBPS         float64  `json:"slippage_bps"`
	MaxPositionWeight   float64  `json:"max_position_weight"`
	MaxInvestedWeight   float64  `json:"max_invested_weight"`
	RebalanceBand       float64  `json:"rebalance_band"`
	MinTradeUSD         float64  `json:"min_trade_usd"`
	HorizonSessions     int      `json:"horizon_sessions"`
	CloseBufferMinutes  int      `json:"close_buffer_minutes"`
	Model               string   `json:"model"`
	Effort              string   `json:"effort"`
	MaxOutputTokens     int      `json:"max_output_tokens"`
	MaxTurns            int      `json:"max_turns"`
	Samples             int      `json:"samples"`
	NewsItems           int      `json:"news_items"`
	RefusalFallback     bool     `json:"refusal_fallback"`
	InputUSDPerMillion  float64  `json:"input_usd_per_million"`
	OutputUSDPerMillion float64  `json:"output_usd_per_million"`
	MaxDailyLLMUSD      float64  `json:"max_daily_llm_usd"`
	MaxMonthlyLLMUSD    float64  `json:"max_monthly_llm_usd"`
	DataDir             string   `json:"data_dir"`
}

var symbolPattern = regexp.MustCompile(`^[A-Z][A-Z0-9.-]{0,14}$`)
var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,79}$`)
var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func LoadConfig(path string) (Config, error) {
	var c Config
	if err := store.ReadJSON(path, &c); err != nil {
		return c, fmt.Errorf("configuration: %w", err)
	}
	// The model ID is a private setting kept in .env, not in the repository.
	if m := strings.TrimSpace(os.Getenv("TRADER_MODEL")); m != "" {
		c.Model = m
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if !namePattern.MatchString(c.Name) {
		return errors.New("name must be a simple identifier")
	}
	if len(c.Symbols) == 0 || len(c.Symbols) > 20 {
		return errors.New("choose 1–20 symbols")
	}
	seen := map[string]bool{}
	for _, s := range c.Symbols {
		if !symbolPattern.MatchString(s) || seen[s] {
			return fmt.Errorf("invalid or duplicate symbol %q", s)
		}
		seen[s] = true
	}
	if c.MarketSymbol != "" && !symbolPattern.MatchString(c.MarketSymbol) {
		return fmt.Errorf("invalid market_symbol %q", c.MarketSymbol)
	}
	for _, v := range []float64{c.InitialCash, c.Commission, c.SlippageBPS, c.MaxPositionWeight, c.MaxInvestedWeight, c.RebalanceBand, c.MinTradeUSD, c.InputUSDPerMillion, c.OutputUSDPerMillion, c.MaxDailyLLMUSD, c.MaxMonthlyLLMUSD} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return errors.New("configuration numbers must be finite")
		}
	}
	if c.InitialCash < 100 || c.InitialCash > 1e9 || c.Commission < 0 || c.Commission > c.InitialCash {
		return errors.New("invalid initial_cash or commission")
	}
	if !wholeCents(c.InitialCash) || !wholeCents(c.Commission) {
		return errors.New("initial_cash and commission must use whole cents")
	}
	if c.SlippageBPS < 0 || c.SlippageBPS > 1000 {
		return errors.New("slippage_bps must be between 0 and 1000")
	}
	if c.MaxPositionWeight <= 0 || c.MaxPositionWeight > 1 || c.MaxInvestedWeight <= 0 || c.MaxInvestedWeight > 1 {
		return errors.New("max_position_weight and max_invested_weight must be in (0,1]")
	}
	if c.RebalanceBand < 0 || c.RebalanceBand > 0.5 || c.MinTradeUSD < 0 {
		return errors.New("rebalance_band must be in [0,0.5] and min_trade_usd non-negative")
	}
	if c.HorizonSessions < 1 || c.HorizonSessions > 60 {
		return errors.New("horizon_sessions must be 1–60")
	}
	if c.CloseBufferMinutes < 0 || c.CloseBufferMinutes > 360 {
		return errors.New("close_buffer_minutes must be 0–360")
	}
	if strings.TrimSpace(c.Model) == "" {
		return errors.New("set TRADER_MODEL in .env")
	}
	switch c.Effort {
	case "", "low", "medium", "high", "xhigh", "max":
	default:
		return errors.New("effort must be empty or one of low, medium, high, xhigh, max")
	}
	// Without streaming the SDK accepts requests expected to finish within
	// ten minutes, which it estimates as roughly 21,000 output tokens.
	if c.MaxOutputTokens < 1024 || c.MaxOutputTokens > 21000 {
		return errors.New("max_output_tokens must be 1024–21000")
	}
	if c.MaxTurns < 2 || c.MaxTurns > 30 {
		return errors.New("max_turns must be 2–30")
	}
	if c.Samples < 1 || c.Samples > 5 {
		return errors.New("samples must be 1–5")
	}
	if c.NewsItems < 0 || c.NewsItems > 30 {
		return errors.New("news_items must be 0–30")
	}
	if c.InputUSDPerMillion <= 0 || c.OutputUSDPerMillion <= 0 {
		return errors.New("token prices must be positive")
	}
	if c.MaxDailyLLMUSD < 0 || c.MaxDailyLLMUSD > 1e4 || c.MaxMonthlyLLMUSD < 0 || c.MaxMonthlyLLMUSD > 1e5 {
		return errors.New("invalid LLM spending limits")
	}
	if strings.TrimSpace(c.DataDir) == "" {
		return errors.New("data_dir is required")
	}
	return nil
}

// AllSymbols returns the traded universe plus the market context symbol.
func (c Config) AllSymbols() []string {
	all := append([]string{}, c.Symbols...)
	if c.MarketSymbol != "" && !slices.Contains(c.Symbols, c.MarketSymbol) {
		all = append(all, c.MarketSymbol)
	}
	return all
}

func (c Config) CloseBuffer() time.Duration { return time.Duration(c.CloseBufferMinutes) * time.Minute }

func wholeCents(v float64) bool { return math.Abs(math.Round(v*100)/100-v) <= 1e-8 }

// LoadEnv handles simple dotenv assignments without evaluating shell code.
// Existing process variables always win. It never prints values.
func LoadEnv(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || !envKeyPattern.MatchString(key) {
			return fmt.Errorf("invalid dotenv assignment on line %d", i+1)
		}
		if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"')) {
			value = value[1 : len(value)-1]
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return err
			}
		}
	}
	return nil
}
