package trader

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
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "TRADER_MODEL") {
		t.Fatalf("a missing model must point to TRADER_MODEL: %v", err)
	}
	t.Setenv("TRADER_MODEL", "test-model")
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != "test-model" || !c.RefusalFallback || c.MarketSymbol != "SPY" {
		t.Fatalf("shipped defaults changed: %+v", c)
	}
}

func TestConfigRejectsUnsafeOrUnknownSettings(t *testing.T) {
	for name, edit := range map[string]func(*Config){
		"output too large without streaming": func(c *Config) { c.MaxOutputTokens = 32000 },
		"unknown effort":                     func(c *Config) { c.Effort = "extreme" },
		"fractional cents":                   func(c *Config) { c.InitialCash = 10000.005 },
		"duplicate symbol":                   func(c *Config) { c.Symbols = []string{"AAA", "AAA"} },
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
}
