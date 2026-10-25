package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/candylario/trader/internal/agent"
	"github.com/candylario/trader/internal/testkit"
)

// Synthetic fixtures only: no network access and no paid requests.

var now = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

func writeConfig(t *testing.T, feed string) string {
	t.Helper()
	dir := t.TempDir()
	c := map[string]any{
		"name": "cli-test", "symbols": []string{"BTCUSDT", "ETHUSDT"}, "quote_asset": "USDT", "capital_usd": 100,
		"max_position_weight": 0.45, "max_invested_weight": 0.9, "rebalance_band": 0.05, "min_trade_usd": 10,
		"decision_interval_hours": 4, "move_trigger_1h_pct": 3, "move_trigger_since_decision_pct": 5, "event_cooldown_minutes": 60, "poll_seconds": 30,
		"news_feeds": []string{feed}, "news_alert_keywords": []string{"etf"}, "max_event_decisions_per_day": 8, "news_items": 5, "score_horizon_hours": 24,
		"daily_loss_limit_pct": 10, "max_drawdown_pct": 30,
		"model": "test-model", "effort": "high", "max_output_tokens": 4000, "max_turns": 4, "samples": 1,
		"refusal_fallback": false, "input_usd_per_million": 4, "output_usd_per_million": 20, "cache_write_usd_per_million": 5, "cache_read_usd_per_million": 0.2,
		"max_daily_llm_usd": 5, "max_monthly_llm_usd": 50, "data_dir": filepath.Join(dir, "data"),
	}
	raw, _ := json.Marshal(c)
	path := filepath.Join(dir, "agent.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// apiServer answers model lookups and replies to every message with ratings.
func apiServer(t *testing.T, calls *int) *httptest.Server {
	script := testkit.SubmitStep(map[string]string{"BTCUSDT": "strong_buy", "ETHUSDT": "hold"}, testkit.CitePrices)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/v1/models/") {
			json.NewEncoder(w).Encode(map[string]any{"id": "test-model", "type": "model", "display_name": "Test", "created_at": "2026-01-01T00:00:00Z"})
			return
		}
		*calls++
		_, body := script("")
		json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(s.Close)
	return s
}

func setup(t *testing.T) (string, env, *int) {
	t.Helper()
	feed := testkit.NewsServer(t, func() []testkit.Headline {
		return []testkit.Headline{{Title: "Bitcoin ETF flows steady", At: now.Add(-time.Hour)}}
	})
	ex := testkit.NewBinance(func() time.Time { return now })
	calls := 0
	e := env{HTTP: feed.Client(), BinanceURL: ex.Server(t).URL, APIURL: apiServer(t, &calls).URL, Now: func() time.Time { return now }}
	for k, v := range map[string]string{"ANTHROPIC_API_KEY": "test-key", "ANTROPIC_API_KEY": "", "BINANCE_API_KEY": testkit.Key, "BINANCE_API_SECRET": testkit.Secret,
		"TELEGRAM_BOT_TOKEN": "", "TELEGRAM_CHAT_ID": "", "TRADER_MODE": "", "TRADER_MODEL": ""} {
		t.Setenv(k, v)
	}
	return writeConfig(t, feed.URL), e, &calls
}

func TestHelpAndArgumentValidation(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"help"}, &out, env{}); err != nil || !strings.Contains(out.String(), "trader run") {
		t.Fatalf("help: %v %q", err, out.String())
	}
	for _, args := range [][]string{{"trade"}, {"status", "--dry-run"}, {"init", "extra"}, {"budget"}, {"budget", "delete"}, {"decide", "--json"}} {
		if err := run(context.Background(), args, io.Discard, env{}); err == nil {
			t.Fatalf("%v should be rejected", args)
		}
	}
}

func TestInitDecideAndStatus(t *testing.T) {
	cfg, e, calls := setup(t)
	ctx := context.Background()
	var out bytes.Buffer
	if err := run(ctx, []string{"init", "--config", cfg}, &out, e); err != nil || !strings.Contains(out.String(), "testnet account with 100 USDT") {
		t.Fatalf("init: %v %s", err, out.String())
	}
	if err := run(ctx, []string{"init", "--config", cfg}, io.Discard, e); err == nil {
		t.Fatal("init must not overwrite an existing account")
	}
	out.Reset()
	if err := run(ctx, []string{"decide", "--dry-run", "--config", cfg}, &out, e); err != nil || *calls != 0 || !strings.Contains(out.String(), "BTCUSDT") || !strings.Contains(out.String(), "Bitcoin ETF flows steady") {
		t.Fatalf("dry run: %v (calls %d) %s", err, *calls, out.String())
	}
	out.Reset()
	if err := run(ctx, []string{"decide", "--config", cfg}, &out, e); err != nil || *calls != 1 || !strings.Contains(out.String(), "Decision #1 saved") {
		t.Fatalf("decide: %v (calls %d) %s", err, *calls, out.String())
	}
	out.Reset()
	if err := run(ctx, []string{"status", "--config", cfg}, &out, e); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"📊 Status (testnet: test money, real prices)", "Trading: on", "BTC: 0.0005", "Last decision #1, 0m ago (requested from the command line)", "BTC: STRONG BUY · ETH: HOLD", "AI cost: $0.01 today"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("status lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if err := run(ctx, []string{"status", "--json", "--config", cfg}, &out, e); err != nil {
		t.Fatal(err)
	}
	var r agent.Report
	if err := json.Unmarshal(out.Bytes(), &r); err != nil || r.Trades != 1 || r.Last == nil || r.Mode != "testnet" {
		t.Fatalf("status json: %v %+v", err, r)
	}
	out.Reset()
	if err := run(ctx, []string{"budget", "status", "--config", cfg}, &out, e); err != nil || !strings.Contains(out.String(), `"month_calls": 1`) {
		t.Fatalf("budget status: %v %s", err, out.String())
	}
}

func TestCheckReportsEachDependency(t *testing.T) {
	cfg, e, calls := setup(t)
	var out bytes.Buffer
	if err := run(context.Background(), []string{"check", "--config", cfg}, &out, e); err != nil {
		t.Fatalf("check: %v\n%s", err, out.String())
	}
	for _, want := range []string{"ok   model test-model", "ok   API key reads the account; 1000 USDT free", "ok   API key may place orders", "ok   1 headlines from 1 feeds", "--   Telegram off"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("check lacks %q:\n%s", want, out.String())
		}
	}
	if *calls != 0 {
		t.Fatal("check must not call the model")
	}
	tg := &testkit.Telegram{ChatID: 4242, Now: func() time.Time { return now }}
	tg.Command("hi")
	e.TelegramURL = tg.Server(t).URL
	t.Setenv("TELEGRAM_BOT_TOKEN", "T")
	out.Reset()
	if err := run(context.Background(), []string{"check", "--config", cfg}, &out, e); err == nil || !strings.Contains(out.String(), "TELEGRAM_CHAT_ID=4242") {
		t.Fatalf("check must show the chat ID to configure: %v\n%s", err, out.String())
	}
	t.Setenv("TELEGRAM_CHAT_ID", "4242")
	out.Reset()
	if err := run(context.Background(), []string{"check", "--config", cfg}, &out, e); err != nil || len(tg.Sent()) != 1 {
		t.Fatalf("check must send a test message: %v\n%s", err, out.String())
	}
}
