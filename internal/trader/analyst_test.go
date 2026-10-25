package trader

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

func analystInput(t *testing.T, c Config, m *fakeMarket, dir string) AnalystInput {
	t.Helper()
	bars := map[string][]Bar{}
	b := Briefing{Session: "2026-10-23", DecisionTime: "2026-10-23T17:00:00-04:00", HorizonSessions: c.HorizonSessions}
	for _, sym := range c.AllSymbols() {
		var series []Bar
		for _, bar := range m.bars[sym] {
			if bar.Date <= "2026-10-23" {
				series = append(series, bar)
			}
		}
		bars[sym] = series
	}
	for _, sym := range c.Symbols {
		ind, err := ComputeIndicators(sym, "prices:"+sym, bars[sym])
		if err != nil {
			t.Fatal(err)
		}
		b.Universe = append(b.Universe, ind)
	}
	ind, _ := ComputeIndicators(c.MarketSymbol, "market:"+c.MarketSymbol, bars[c.MarketSymbol])
	b.Market = &ind
	return AnalystInput{Briefing: b, Bars: bars, Cutoff: at(t, "2026-10-23 17:00"), Dir: dir}
}

func TestAnalystUsesToolsStrictSchemaAndCitedNews(t *testing.T) {
	dir := t.TempDir()
	c := testConfig(dir)
	m := newFakeMarket(t, []string{"AAA", "BBB", "MKT"}, "2026-01-02", "2026-10-30")
	news := newsServer(t, at(t, "2026-10-23 12:00"))
	api := &fakeAPI{script: []func(string) (int, any){
		func(string) (int, any) {
			return 200, message("tool_use", thinking(),
				toolUse("toolu_1", "get_news", map[string]any{"symbol": "AAA"}),
				toolUse("toolu_2", "get_price_history", map[string]any{"symbol": "BBB", "sessions": 3}))
		},
		submitStep(map[string]string{"AAA": "strong_buy", "BBB": "hold"}, func(req, sym string) []string {
			if sym == "AAA" {
				return []string{"prices:AAA", newsIDPattern.FindString(req)}
			}
			return []string{"prices:BBB", "market:MKT"}
		}),
	}}
	srv := api.server(t)
	a := newTestAnalyst(t, c, srv.URL, news.URL)
	res, err := a.Analyze(context.Background(), analystInput(t, c, m, dir))
	if err != nil {
		t.Fatal(err)
	}
	if res.Abstained || res.Signals["AAA"].Rating != RatingStrongBuy || res.Signals["BBB"].Rating != RatingHold {
		t.Fatalf("unexpected signals: %+v", res)
	}
	if len(res.Samples[0].News) != 1 || !strings.HasPrefix(res.Signals["AAA"].Evidence[1], "news:") {
		t.Fatalf("news evidence was not recorded: %+v", res.Samples[0])
	}
	first := api.requests[0]
	if tc := first["tool_choice"].(map[string]any); tc["type"] != "auto" {
		t.Fatalf("forced tool choice is rejected by current models; got %v", tc)
	}
	if _, ok := first["temperature"]; ok {
		t.Fatal("sampling parameters are rejected by current models")
	}
	if first["fallbacks"] != "default" || !strings.Contains(api.headers[0].Get("anthropic-beta"), "server-side-fallback-2026-07-01") {
		t.Fatalf("refusal fallback not requested: %v %v", first["fallbacks"], api.headers[0])
	}
	if oc := first["output_config"].(map[string]any); oc["effort"] != "high" {
		t.Fatalf("effort not set: %v", oc)
	}
	for _, tool := range first["tools"].([]any) {
		tool := tool.(map[string]any)
		schema := tool["input_schema"].(map[string]any)
		if tool["strict"] != true || schema["additionalProperties"] != false {
			t.Fatalf("tool %v is not strict", tool["name"])
		}
	}
	second := api.bodies[1]
	if !strings.Contains(second, `"tool_use_id":"toolu_1"`) || !strings.Contains(second, `"tool_use_id":"toolu_2"`) || !strings.Contains(second, `"signature":"sig-test"`) {
		t.Fatal("tool results or the thinking block were not passed back in the follow-up turn")
	}
	if !strings.Contains(second, "untrusted third-party text") {
		t.Fatal("news results must be labelled as untrusted data")
	}
	sum, _ := a.Budget.Summary(context.Background())
	if sum.MonthCalls != 2 || sum.UnresolvedUSD != 0 || sum.TodayUSD <= 0 {
		t.Fatalf("budget not settled per call: %+v", sum)
	}
	if _, err := os.Stat(filepath.Join(dir, "calls", "sample-1-turn-2.json")); err != nil {
		t.Fatal("provider response receipt not archived")
	}
}

func TestInventedEvidenceIsRejectedAndCorrected(t *testing.T) {
	dir := t.TempDir()
	c := testConfig(dir)
	m := newFakeMarket(t, []string{"AAA", "BBB", "MKT"}, "2026-01-02", "2026-10-30")
	api := &fakeAPI{script: []func(string) (int, any){
		submitStep(map[string]string{"AAA": "buy", "BBB": "sell"}, func(_, sym string) []string { return []string{"news:000000000000"} }),
		submitStep(map[string]string{"AAA": "buy", "BBB": "sell"}, citePrices),
	}}
	a := newTestAnalyst(t, c, api.server(t).URL, "")
	res, err := a.Analyze(context.Background(), analystInput(t, c, m, dir))
	if err != nil || res.Signals["BBB"].Rating != RatingSell {
		t.Fatalf("correction not accepted: %+v %v", res, err)
	}
	if !strings.Contains(api.bodies[1], `"is_error":true`) || !strings.Contains(api.bodies[1], "was not provided to you") {
		t.Fatal("invented evidence was not reported back as a tool error")
	}
}

func TestMiscopiedEvidenceGetsASuggestion(t *testing.T) {
	// The first live cycle cited news:f75d3bc590a2 for news:f56d3bc590a2.
	shown := map[string]bool{"prices:AAA": true, "news:f56d3bc590a2": true, "market:MKT": true}
	raw := `{"summary":"s","signals":{"AAA":{"rating":"buy","confidence":"low","rationale":"r","evidence_ids":["news:f75d3bc590a2"]}}}`
	_, _, err := parseSubmission(raw, []string{"AAA"}, shown)
	if err == nil || !strings.Contains(err.Error(), `did you mean "news:f56d3bc590a2"`) {
		t.Fatalf("expected a suggestion, got %v", err)
	}
	raw = strings.Replace(raw, "news:f75d3bc590a2", "news:zzzzzzzzzzzz", 1)
	if _, _, err := parseSubmission(raw, []string{"AAA"}, shown); err == nil || strings.Contains(err.Error(), "did you mean") {
		t.Fatalf("an unrelated ID must not get a misleading suggestion: %v", err)
	}
}

func TestPriceHistoryCitesTheSameIDAsTheBriefing(t *testing.T) {
	c := testConfig("unused")
	bars := map[string][]Bar{"AAA": {{Date: "2026-10-23", Close: 1}}, "MKT": {{Date: "2026-10-23", Close: 1}}}
	for symbol, want := range map[string]string{"AAA": `"evidence_id":"prices:AAA"`, "MKT": `"evidence_id":"market:MKT"`} {
		out, err := priceHistory(c, `{"symbol":"`+symbol+`","sessions":5}`, bars)
		if err != nil || !strings.Contains(out, want) {
			t.Fatalf("%s: %s %v", symbol, out, err)
		}
	}
}

func TestRefusalAbstainsAndHoldsEverything(t *testing.T) {
	dir := t.TempDir()
	c := testConfig(dir)
	m := newFakeMarket(t, []string{"AAA", "BBB", "MKT"}, "2026-01-02", "2026-10-30")
	api := &fakeAPI{script: []func(string) (int, any){func(string) (int, any) {
		msg := message("refusal")
		msg["stop_details"] = map[string]any{"type": "refusal", "category": "cyber", "explanation": "test"}
		return 200, msg
	}}}
	a := newTestAnalyst(t, c, api.server(t).URL, "")
	res, err := a.Analyze(context.Background(), analystInput(t, c, m, dir))
	if err != nil || !res.Abstained || res.Signals["AAA"].Rating != RatingHold || !strings.Contains(res.Samples[0].Error, "declined") {
		t.Fatalf("refusal handling: %+v %v", res, err)
	}
}

func TestTransientErrorsRetryAndPermanentErrorsStop(t *testing.T) {
	dir := t.TempDir()
	c := testConfig(dir)
	m := newFakeMarket(t, []string{"AAA", "BBB", "MKT"}, "2026-01-02", "2026-10-30")
	overloaded := func(string) (int, any) {
		return 529, map[string]any{"type": "error", "error": map[string]any{"type": "overloaded_error", "message": "busy"}}
	}
	dropped := func(string) (int, any) { return -1, nil }
	api := &fakeAPI{script: []func(string) (int, any){overloaded, dropped, submitStep(map[string]string{"AAA": "hold", "BBB": "hold"}, citePrices)}}
	a := newTestAnalyst(t, c, api.server(t).URL, "")
	var waits []time.Duration
	a.Sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	if _, err := a.Analyze(context.Background(), analystInput(t, c, m, dir)); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 2 {
		t.Fatalf("expected two backoffs, got %v", waits)
	}
	j, _ := a.Budget.load()
	statuses := []string{j.Attempts[0].Status, j.Attempts[1].Status, j.Attempts[2].Status}
	if statuses[0] != attemptReleased || statuses[1] != attemptUncertain || statuses[2] != attemptSettled {
		t.Fatalf("529 must release, a dropped connection must stay charged: %v", statuses)
	}

	dir2 := t.TempDir()
	c2 := testConfig(dir2)
	bad := &fakeAPI{script: []func(string) (int, any){func(string) (int, any) {
		return 400, map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": "bad"}}
	}}}
	a2 := newTestAnalyst(t, c2, bad.server(t).URL, "")
	_, err := a2.Analyze(context.Background(), analystInput(t, c2, m, dir2))
	var perm *PermanentError
	if !errors.As(err, &perm) || bad.count() != 1 {
		t.Fatalf("a 400 must stop immediately without retries: %v (%d calls)", err, bad.count())
	}
	if sum, _ := a2.Budget.Summary(context.Background()); sum.TodayUSD != 0 {
		t.Fatalf("rejected request must not be charged: %+v", sum)
	}
}

func TestUndecodableResponseStaysChargedAndStops(t *testing.T) {
	dir := t.TempDir()
	c := testConfig(dir)
	m := newFakeMarket(t, []string{"AAA", "BBB", "MKT"}, "2026-01-02", "2026-10-30")
	api := &fakeAPI{script: []func(string) (int, any){func(string) (int, any) { return -2, nil }}}
	a := newTestAnalyst(t, c, api.server(t).URL, "")
	_, err := a.Analyze(context.Background(), analystInput(t, c, m, dir))
	var perm *PermanentError
	if !errors.As(err, &perm) || api.count() != 1 {
		t.Fatalf("an unexplained client failure must stop, not retry: %v", err)
	}
	if sum, _ := a.Budget.Summary(context.Background()); sum.UnresolvedUSD <= 0 {
		t.Fatalf("the request may have been billed, so its reservation must stay charged: %+v", sum)
	}
}

func TestFallbackIterationsAreAllCharged(t *testing.T) {
	var m anthropic.BetaMessage
	raw := `{"id":"m","type":"message","role":"assistant","model":"fallback-model","content":[],"stop_reason":"end_turn","stop_sequence":null,
	 "usage":{"input_tokens":1000,"output_tokens":500,"cache_creation_input_tokens":0,"cache_read_input_tokens":2000,"iterations":[
	  {"type":"message","model":"test-model","input_tokens":1000,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},
	  {"type":"fallback_message","model":"fallback-model","input_tokens":1000,"output_tokens":500,"cache_creation_input_tokens":0,"cache_read_input_tokens":2000}]}}`
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	// Both the declined attempt and the fallback are billed: $4/$20 per million,
	// cache reads at 0.1x the input rate.
	want := 1000*4.0/1e6 + (1000*4.0+2000*4*0.1+500*20.0)/1e6
	if got := usageCost(testConfig("x"), &m); math.Abs(got-want) > 1e-12 {
		t.Fatalf("cost %v, want %v", got, want)
	}
}

func TestBudgetCapStopsBeforeCalling(t *testing.T) {
	dir := t.TempDir()
	c := testConfig(dir)
	c.MaxDailyLLMUSD = 0.01
	m := newFakeMarket(t, []string{"AAA", "BBB", "MKT"}, "2026-01-02", "2026-10-30")
	api := &fakeAPI{}
	a := newTestAnalyst(t, c, api.server(t).URL, "")
	if _, err := a.Analyze(context.Background(), analystInput(t, c, m, dir)); !errors.Is(err, ErrBudgetExceeded) || api.count() != 0 {
		t.Fatalf("expected budget stop with no request, got %v after %d calls", err, api.count())
	}
}

func TestSamplesAggregateByMedianAndCompletedSamplesAreReused(t *testing.T) {
	dir := t.TempDir()
	c := testConfig(dir)
	c.Samples = 3
	m := newFakeMarket(t, []string{"AAA", "BBB", "MKT"}, "2026-01-02", "2026-10-30")
	api := &fakeAPI{script: []func(string) (int, any){
		submitStep(map[string]string{"AAA": "buy", "BBB": "sell"}, citePrices),
		submitStep(map[string]string{"AAA": "strong_buy", "BBB": "hold"}, citePrices),
		submitStep(map[string]string{"AAA": "hold", "BBB": "strong_sell"}, citePrices),
	}}
	a := newTestAnalyst(t, c, api.server(t).URL, "")
	in := analystInput(t, c, m, dir)
	res, err := a.Analyze(context.Background(), in)
	if err != nil || res.Signals["AAA"].Rating != RatingBuy || res.Signals["BBB"].Rating != RatingSell {
		t.Fatalf("median aggregation: %+v %v", res.Signals, err)
	}
	if res.Signals["AAA"].Agreement < 0.33 || res.Signals["AAA"].Agreement > 0.34 {
		t.Fatalf("agreement: %v", res.Signals["AAA"].Agreement)
	}
	again, err := a.Analyze(context.Background(), in)
	if err != nil || api.count() != 3 || again.Signals["AAA"].Rating != RatingBuy {
		t.Fatalf("a retried cycle must reuse completed samples: %d calls, %v", api.count(), err)
	}
}
