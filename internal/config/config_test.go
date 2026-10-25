package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShippedConfigTakesTheModelFromTheEnvironment(t *testing.T) {
	path := filepath.Join("..", "..", "config", "agent.json")
	t.Setenv("TRADER_MODEL", "")
	t.Setenv("TRADER_MODE", "")
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "TRADER_MODEL") {
		t.Fatalf("a missing model must point to TRADER_MODEL: %v", err)
	}
	t.Setenv("TRADER_MODEL", "test-model")
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != "test-model" || c.RefusalFallback || c.Mode != Testnet || c.Dir() != filepath.Join("data", "testnet") || c.Base("ETHUSDT") != "ETH" {
		t.Fatalf("shipped defaults changed: %+v", c)
	}
	t.Setenv("TRADER_MODE", "LIVE")
	if c, err := LoadConfig(path); err != nil || c.Mode != Live {
		t.Fatalf("TRADER_MODE=live must select live: %v", err)
	}
	t.Setenv("TRADER_MODE", "real")
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("an unknown mode must be rejected")
	}
}

func TestConfigRejectsUnsafeOrUnknownSettings(t *testing.T) {
	for name, edit := range map[string]func(*Config){
		"output too large without streaming": func(c *Config) { c.MaxOutputTokens = 32000 },
		"unknown effort":                     func(c *Config) { c.Effort = "extreme" },
		"pair in another quote":              func(c *Config) { c.Symbols = []string{"BTCEUR"} },
		"duplicate symbol":                   func(c *Config) { c.Symbols = []string{"BTCUSDT", "BTCUSDT"} },
		"position above exposure":            func(c *Config) { c.MaxPositionWeight = 0.95 },
		"loss limit of 100%":                 func(c *Config) { c.MaxDrawdownPct = 100 },
		"feed without scheme":                func(c *Config) { c.NewsFeeds = []string{"coindesk.com/rss"} },
		"plain http feed":                    func(c *Config) { c.NewsFeeds = []string{"http://coindesk.com/rss"} },
		"no mode":                            func(c *Config) { c.Mode = "" },
		"capital too small to ever buy":      func(c *Config) { c.CapitalUSD, c.MinTradeUSD = 30, 10 },
		"no data dir":                        func(c *Config) { c.DataDir = " " },
		"too many samples":                   func(c *Config) { c.Samples = 9 },
	} {
		c := testConfig("x")
		edit(&c)
		if err := c.Validate(); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	c := testConfig(t.TempDir())
	raw, _ := json.Marshal(c)
	path := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(path, []byte(strings.Replace(string(raw), `"name"`, `"nmae"`, 1)), 0o600)
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("a misspelled field must not silently fall back to a default")
	}
}

func TestEveryConfigKeyIsRequired(t *testing.T) {
	t.Setenv("TRADER_MODEL", "test-model")
	raw, _ := os.ReadFile(filepath.Join("..", "..", "config", "agent.json"))
	for _, key := range []string{"capital_usd", "max_drawdown_pct", "rebalance_band", "refusal_fallback", "news_feeds"} {
		var m map[string]any
		json.Unmarshal(raw, &m)
		delete(m, key)
		edited, _ := json.Marshal(m)
		path := filepath.Join(t.TempDir(), "c.json")
		os.WriteFile(path, edited, 0o600)
		if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), key) {
			t.Fatalf("missing %s accepted: %v", key, err)
		}
	}
}

func TestDotenvDoesNotEvaluateShellOrOverwriteEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(path, []byte("# comment\nexport TRADER_TEST_A='$(touch /tmp/pwned)'\nTRADER_TEST_B=from-file\n"), 0o600)
	t.Setenv("TRADER_TEST_B", "from-process")
	os.Unsetenv("TRADER_TEST_A")
	t.Cleanup(func() { os.Unsetenv("TRADER_TEST_A") })
	if err := LoadEnv(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("TRADER_TEST_A") != "$(touch /tmp/pwned)" || os.Getenv("TRADER_TEST_B") != "from-process" {
		t.Fatal("dotenv evaluated shell code or overrode the process environment")
	}
	os.WriteFile(path, []byte("not an assignment\n"), 0o600)
	if err := LoadEnv(path); err == nil {
		t.Fatal("malformed dotenv accepted")
	}
	os.WriteFile(path, []byte("TRADER_TEST_C=testnet\nTRADER_TEST_C=live\n"), 0o600)
	if err := LoadEnv(path); err == nil || !strings.Contains(err.Error(), "TRADER_TEST_C is set twice") {
		t.Fatalf("a duplicate key must be rejected, as Docker Compose would read the other one: %v", err)
	}
	os.Unsetenv("TRADER_TEST_D")
	t.Cleanup(func() { os.Unsetenv("TRADER_TEST_D") })
	os.WriteFile(path, []byte("TRADER_TEST_D=live # real money\n"), 0o600)
	if err := LoadEnv(path); err != nil || os.Getenv("TRADER_TEST_D") != "live" {
		t.Fatalf("inline comments must be dropped like Docker Compose does: %q %v", os.Getenv("TRADER_TEST_D"), err)
	}
	os.Unsetenv("TRADER_TEST_D")
	os.WriteFile(path, []byte(`TRADER_TEST_D="live" # real money`+"\n"), 0o600)
	if err := LoadEnv(path); err != nil || os.Getenv("TRADER_TEST_D") != "live" {
		t.Fatalf("a quoted value followed by a comment: %q %v", os.Getenv("TRADER_TEST_D"), err)
	}
}

func testConfig(dir string) Config {
	return Config{
		Name: "test-agent", Symbols: []string{"BTCUSDT", "ETHUSDT"}, QuoteAsset: "USDT", CapitalUSD: 100,
		MaxPositionWeight: 0.45, MaxInvestedWeight: 0.9, RebalanceBand: 0.05, MinTradeUSD: 10,
		DecisionIntervalHours: 4, MoveTrigger1HPct: 3, MoveTriggerSinceDecisionPct: 5, EventCooldownMinutes: 60, PollSeconds: 30,
		NewsFeeds: []string{"https://example.com/rss"}, NewsAlertKeywords: []string{"etf"}, MaxEventDecisionsPerDay: 8, NewsItems: 5, ScoreHorizonHours: 24,
		DailyLossLimitPct: 10, MaxDrawdownPct: 30,
		Model: "test-model", Effort: "high", MaxOutputTokens: 4000, MaxTurns: 6, Samples: 1,
		RefusalFallback: true, InputUSDPerMillion: 4, OutputUSDPerMillion: 20, CacheWriteUSDPerMillion: 5, CacheReadUSDPerMillion: 0.2,
		MaxDailyLLMUSD: 5, MaxMonthlyLLMUSD: 50, DataDir: dir, Mode: Testnet,
	}
}
