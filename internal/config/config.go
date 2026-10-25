package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Config describes one trading agent. Every field except model and mode must
// be present in the JSON file; those two come from TRADER_MODEL and TRADER_MODE.
type Config struct {
	Name                        string   `json:"name"`
	Symbols                     []string `json:"symbols"`
	QuoteAsset                  string   `json:"quote_asset"`
	CapitalUSD                  float64  `json:"capital_usd"`
	MaxPositionWeight           float64  `json:"max_position_weight"`
	MaxInvestedWeight           float64  `json:"max_invested_weight"`
	RebalanceBand               float64  `json:"rebalance_band"`
	MinTradeUSD                 float64  `json:"min_trade_usd"`
	DecisionIntervalHours       float64  `json:"decision_interval_hours"`
	MoveTrigger1HPct            float64  `json:"move_trigger_1h_pct"`
	MoveTriggerSinceDecisionPct float64  `json:"move_trigger_since_decision_pct"`
	EventCooldownMinutes        int      `json:"event_cooldown_minutes"`
	PollSeconds                 int      `json:"poll_seconds"`
	NewsFeeds                   []string `json:"news_feeds"`
	NewsAlertKeywords           []string `json:"news_alert_keywords"`
	MaxEventDecisionsPerDay     int      `json:"max_event_decisions_per_day"`
	NewsItems                   int      `json:"news_items"`
	ScoreHorizonHours           int      `json:"score_horizon_hours"`
	DailyLossLimitPct           float64  `json:"daily_loss_limit_pct"`
	MaxDrawdownPct              float64  `json:"max_drawdown_pct"`
	Model                       string   `json:"model"`
	Effort                      string   `json:"effort"`
	MaxOutputTokens             int      `json:"max_output_tokens"`
	MaxTurns                    int      `json:"max_turns"`
	Samples                     int      `json:"samples"`
	RefusalFallback             bool     `json:"refusal_fallback"`
	InputUSDPerMillion          float64  `json:"input_usd_per_million"`
	OutputUSDPerMillion         float64  `json:"output_usd_per_million"`
	CacheWriteUSDPerMillion     float64  `json:"cache_write_usd_per_million"`
	CacheReadUSDPerMillion      float64  `json:"cache_read_usd_per_million"`
	MaxDailyLLMUSD              float64  `json:"max_daily_llm_usd"`
	MaxMonthlyLLMUSD            float64  `json:"max_monthly_llm_usd"`
	DataDir                     string   `json:"data_dir"`
	Mode                        string   `json:"-"`
}

const (
	Testnet = "testnet"
	Live    = "live"
)

var symbolPattern = regexp.MustCompile(`^[A-Z0-9]{2,20}$`)
var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,79}$`)
var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("configuration: %w", err)
	}
	c, err := Parse(raw, "model")
	if err != nil {
		return c, err
	}
	if m := strings.TrimSpace(os.Getenv("TRADER_MODEL")); m != "" {
		c.Model = m
	}
	c.Mode = strings.ToLower(strings.TrimSpace(os.Getenv("TRADER_MODE")))
	if c.Mode == "" {
		c.Mode = Testnet
	}
	return c, c.Validate()
}

// Parse reads a config strictly: unknown fields are errors, and every field
// must be present except those named optional. It does not validate values.
func Parse(raw []byte, optional ...string) (Config, error) {
	var c Config
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("configuration: %w", err)
	}
	var present map[string]json.RawMessage
	if err := json.Unmarshal(raw, &present); err != nil {
		return c, fmt.Errorf("configuration: %w", err)
	}
	t := reflect.TypeOf(c)
	for i := 0; i < t.NumField(); i++ {
		key := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		if key != "-" && !slices.Contains(optional, key) && present[key] == nil {
			return c, fmt.Errorf("configuration: missing %q", key)
		}
	}
	return c, nil
}

func (c Config) Validate() error {
	if !namePattern.MatchString(c.Name) {
		return errors.New("name must be a simple identifier")
	}
	if !symbolPattern.MatchString(c.QuoteAsset) {
		return fmt.Errorf("invalid quote_asset %q", c.QuoteAsset)
	}
	if len(c.Symbols) == 0 || len(c.Symbols) > 10 {
		return errors.New("choose 1–10 symbols")
	}
	seen := map[string]bool{}
	for _, s := range c.Symbols {
		if !symbolPattern.MatchString(s) || seen[s] || !strings.HasSuffix(s, c.QuoteAsset) || s == c.QuoteAsset {
			return fmt.Errorf("invalid or duplicate symbol %q (each must be a %s pair)", s, c.QuoteAsset)
		}
		seen[s] = true
	}
	for _, v := range []float64{c.CapitalUSD, c.MaxPositionWeight, c.MaxInvestedWeight, c.RebalanceBand, c.MinTradeUSD, c.DecisionIntervalHours, c.MoveTrigger1HPct, c.MoveTriggerSinceDecisionPct, c.DailyLossLimitPct, c.MaxDrawdownPct, c.InputUSDPerMillion, c.OutputUSDPerMillion, c.CacheWriteUSDPerMillion, c.CacheReadUSDPerMillion, c.MaxDailyLLMUSD, c.MaxMonthlyLLMUSD} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return errors.New("configuration numbers must be finite")
		}
	}
	if c.CapitalUSD < 10 || c.CapitalUSD > 1e6 {
		return errors.New("capital_usd must be 10–1,000,000")
	}
	if c.MaxPositionWeight <= 0 || c.MaxPositionWeight > 1 || c.MaxInvestedWeight <= 0 || c.MaxInvestedWeight > 1 || c.MaxPositionWeight > c.MaxInvestedWeight {
		return errors.New("weights must be in (0,1] with max_position_weight ≤ max_invested_weight")
	}
	if c.RebalanceBand < 0 || c.RebalanceBand > 0.5 || c.MinTradeUSD < 0 || c.MinTradeUSD > c.CapitalUSD {
		return errors.New("rebalance_band must be in [0,0.5] and min_trade_usd within capital")
	}
	// A buy targets half a position; it must clear min_trade_usd and the
	// exchange's minimum order (about $5 for these pairs) or nothing trades.
	if c.CapitalUSD*c.MaxPositionWeight/2 < math.Max(c.MinTradeUSD, 6) {
		return errors.New("with this capital_usd and max_position_weight a buy rating could never place an order; raise one of them or lower min_trade_usd")
	}
	if c.DecisionIntervalHours < 0.25 || c.DecisionIntervalHours > 168 {
		return errors.New("decision_interval_hours must be 0.25–168")
	}
	if c.MoveTrigger1HPct < 0 || c.MoveTrigger1HPct > 100 || c.MoveTriggerSinceDecisionPct < 0 || c.MoveTriggerSinceDecisionPct > 100 {
		return errors.New("move triggers must be 0–100 (0 turns one off)")
	}
	if c.EventCooldownMinutes < 0 || c.EventCooldownMinutes > 1440 || c.PollSeconds < 5 || c.PollSeconds > 600 {
		return errors.New("event_cooldown_minutes must be 0–1440 and poll_seconds 5–600")
	}
	for _, f := range c.NewsFeeds {
		if u, err := url.Parse(f); err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("news feed %q must be an https URL", f)
		}
	}
	if c.NewsItems < 0 || c.NewsItems > 50 || c.ScoreHorizonHours < 1 || c.ScoreHorizonHours > 720 || c.MaxEventDecisionsPerDay < 0 || c.MaxEventDecisionsPerDay > 48 {
		return errors.New("news_items must be 0–50, score_horizon_hours 1–720 and max_event_decisions_per_day 0–48")
	}
	if c.DailyLossLimitPct < 0 || c.DailyLossLimitPct >= 100 || c.MaxDrawdownPct < 0 || c.MaxDrawdownPct >= 100 {
		return errors.New("loss limits must be 0–99 (0 turns one off)")
	}
	if strings.TrimSpace(c.Model) == "" {
		return errors.New("set TRADER_MODEL in .env")
	}
	if c.Mode != Testnet && c.Mode != Live {
		return errors.New("TRADER_MODE must be testnet or live")
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
	if c.InputUSDPerMillion <= 0 || c.OutputUSDPerMillion <= 0 || c.CacheWriteUSDPerMillion <= 0 || c.CacheReadUSDPerMillion <= 0 {
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

// Dir keeps testnet and live accounts apart.
func (c Config) Dir() string { return filepath.Join(c.DataDir, c.Mode) }

// Base is the coin a symbol trades, such as BTC for BTCUSDT.
func (c Config) Base(symbol string) string { return strings.TrimSuffix(symbol, c.QuoteAsset) }

func (c Config) DecisionInterval() time.Duration {
	return time.Duration(c.DecisionIntervalHours * float64(time.Hour))
}

// LoadEnv handles simple dotenv assignments without evaluating shell code,
// reading them the way Docker Compose does: an unquoted value ends at " #".
// Duplicate keys are rejected so local runs and the container never differ.
// Existing process variables always win. It never prints values.
func LoadEnv(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	seen := map[string]bool{}
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
		if seen[key] {
			return fmt.Errorf("%s is set twice in %s", key, path)
		}
		seen[key] = true
		if value != "" && (value[0] == '\'' || value[0] == '"') {
			end := strings.IndexByte(value[1:], value[0])
			if end < 0 {
				return fmt.Errorf("unterminated quote on line %d", i+1)
			}
			if rest := strings.TrimSpace(value[end+2:]); rest != "" && !strings.HasPrefix(rest, "#") {
				return fmt.Errorf("invalid dotenv assignment on line %d", i+1)
			}
			value = value[1 : end+1]
		} else if cut := strings.Index(value, " #"); cut >= 0 {
			value = strings.TrimSpace(value[:cut])
		} else if strings.HasPrefix(value, "#") {
			value = ""
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return err
			}
		}
	}
	return nil
}
