package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ismatsamadov/trader/internal/trader"
)

// Synthetic fixtures only: no network access and no paid requests.

func writeConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	c := map[string]any{
		"name": "cli-test", "symbols": []string{"AAA", "BBB"}, "market_symbol": "MKT",
		"initial_cash": 10000, "commission": 1, "slippage_bps": 5,
		"max_position_weight": 0.4, "max_invested_weight": 0.8, "rebalance_band": 0.03, "min_trade_usd": 50,
		"horizon_sessions": 3, "close_buffer_minutes": 30,
		"model": "test-model", "effort": "high", "max_output_tokens": 4000, "max_turns": 4, "samples": 1, "news_items": 0,
		"refusal_fallback": true, "input_usd_per_million": 4, "output_usd_per_million": 20,
		"max_daily_llm_usd": 5, "max_monthly_llm_usd": 50, "data_dir": filepath.Join(dir, "data"),
	}
	raw, _ := json.Marshal(c)
	path := filepath.Join(dir, "agent.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func marketServer(t *testing.T) *httptest.Server {
	ny, _ := time.LoadLocation("America/New_York")
	sessions, _ := trader.NYSESessions("2026-01-02", "2026-10-23")
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		symbol := strings.TrimPrefix(r.URL.Path, "/v8/finance/chart/")
		var ts []int64
		var o, h, l, c []float64
		var v []int64
		price := 100.0
		for i, s := range sessions {
			d, _ := time.ParseInLocation(time.DateOnly, s.Date, ny)
			next := price * (1 + 0.003*float64(i%5-2))
			ts = append(ts, d.Add(9*time.Hour+30*time.Minute).Unix())
			o, c = append(o, price), append(c, next)
			h, l = append(h, max(price, next)+0.5), append(l, min(price, next)-0.5)
			v = append(v, 1000)
			price = next
		}
		json.NewEncoder(w).Encode(map[string]any{"chart": map[string]any{"result": []any{map[string]any{
			"meta":       map[string]any{"symbol": symbol, "currency": "USD", "exchangeTimezoneName": "America/New_York"},
			"timestamp":  ts,
			"indicators": map[string]any{"quote": []any{map[string]any{"open": o, "high": h, "low": l, "close": c, "volume": v}}},
		}}}})
	}))
	t.Cleanup(s.Close)
	return s
}

func apiServer(t *testing.T, calls *int) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		*calls++
		w.Header().Set("Content-Type", "application/json")
		signal := func(rating string, sym string) map[string]any {
			return map[string]any{"rating": rating, "confidence": "high", "rationale": "Trend above both averages.", "evidence_ids": []string{"prices:" + sym}}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant", "model": "test-model", "stop_reason": "tool_use", "stop_sequence": nil,
			"content": []any{map[string]any{"type": "tool_use", "id": "toolu_1", "name": "submit_signals", "input": map[string]any{
				"summary": "Constructive on AAA.",
				"signals": map[string]any{"AAA": signal("strong_buy", "AAA"), "BBB": signal("hold", "BBB")},
			}}},
			"usage": map[string]any{"input_tokens": 2000, "output_tokens": 500, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0},
		})
	}))
	t.Cleanup(s.Close)
	return s
}

func testEnv(market, api string) env {
	ny, _ := time.LoadLocation("America/New_York")
	now := time.Date(2026, 10, 23, 17, 0, 0, 0, ny)
	return env{HTTP: http.DefaultClient, MarketURL: market, APIURL: api, Now: func() time.Time { return now }, Sleep: func(context.Context, time.Duration) error { return nil }}
}

func TestHelpAndArgumentValidation(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"help"}, &out, env{}); err != nil || !strings.Contains(out.String(), "trader daemon") {
		t.Fatalf("help: %v %q", err, out.String())
	}
	for _, args := range [][]string{{"trade"}, {"status", "--dry-run"}, {"init", "extra"}, {"budget"}, {"budget", "delete"}} {
		if err := run(context.Background(), args, io.Discard, env{}); err == nil {
			t.Fatalf("%v should be rejected", args)
		}
	}
}

func TestInitDryRunRunAndStatus(t *testing.T) {
	cfg := writeConfig(t)
	calls := 0
	e := testEnv(marketServer(t).URL, apiServer(t, &calls).URL)
	ctx := context.Background()
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTROPIC_API_KEY", "")
	var out bytes.Buffer
	if err := run(ctx, []string{"init", "--config", cfg}, &out, e); err != nil || !strings.Contains(out.String(), "$10000.00") {
		t.Fatalf("init: %v %s", err, out.String())
	}
	if err := run(ctx, []string{"init", "--config", cfg}, io.Discard, e); err == nil {
		t.Fatal("init must not overwrite an existing account")
	}
	out.Reset()
	if err := run(ctx, []string{"run", "--dry-run", "--config", cfg}, &out, e); err != nil || !strings.Contains(out.String(), "dry run") || calls != 0 {
		t.Fatalf("dry run needs no API key and makes no model call: %v %s", err, out.String())
	}
	if err := run(ctx, []string{"run", "--config", cfg}, io.Discard, e); err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("a real run without a key must fail clearly: %v", err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	out.Reset()
	if err := run(ctx, []string{"run", "--config", cfg}, &out, e); err != nil || calls != 1 {
		t.Fatalf("run: %v (calls %d) %s", err, calls, out.String())
	}
	if !strings.Contains(out.String(), "order for next open: BUY") || !strings.Contains(out.String(), "strong_buy") {
		t.Fatalf("run output: %s", out.String())
	}
	out.Reset()
	if err := run(ctx, []string{"status", "--config", cfg}, &out, e); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"buy & hold", "Orders for the next open", "LLM spend: today $0.02"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("status lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if err := run(ctx, []string{"status", "--json", "--config", cfg}, &out, e); err != nil {
		t.Fatal(err)
	}
	var report trader.StatusReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil || len(report.Pending) != 1 || report.Model != "test-model" {
		t.Fatalf("status json: %v %+v", err, report)
	}
	out.Reset()
	if err := run(ctx, []string{"budget", "status", "--config", cfg}, &out, e); err != nil || !strings.Contains(out.String(), fmt.Sprintf(`"month_calls": %d`, 1)) {
		t.Fatalf("budget status: %v %s", err, out.String())
	}
}
