package agent

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ismatsamadov/trader/internal/analyst"
	"github.com/ismatsamadov/trader/internal/config"
	"github.com/ismatsamadov/trader/internal/market"
	"github.com/ismatsamadov/trader/internal/testkit"
)

type agentRig struct {
	agent  *Agent
	market *testkit.Market
	api    *testkit.API
	clock  time.Time
	config config.Config
}

func newAgentRig(t *testing.T, script ...func(string) (int, any)) *agentRig {
	t.Helper()
	c := testkit.Config(t.TempDir())
	m := testkit.NewMarket(t, []string{"AAA", "BBB", "MKT"}, "2026-01-02", "2026-11-06")
	m.SetThrough("2026-10-23")
	api := &testkit.API{Script: script}
	r := &agentRig{market: m, api: api, config: c, clock: testkit.At(t, "2026-10-23 17:00")}
	r.agent = &Agent{
		Config:  c,
		Market:  market.PriceSource{HTTP: http.DefaultClient, BaseURL: m.Server(t).URL},
		Analyst: newTestAnalyst(t, c, api.Server(t).URL, testkit.NewsServer(t, testkit.At(t, "2026-10-23 12:00")).URL),
		Now:     func() time.Time { return r.clock },
	}
	r.agent.Analyst.Budget.Now = r.agent.Now
	if _, err := InitState(c, r.clock); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *agentRig) advance(t *testing.T, date string) {
	r.market.SetThrough(date)
	r.clock = testkit.At(t, date+" 17:00")
}

func TestRealTimeCycleDecidesFillsMarksAndScores(t *testing.T) {
	r := newAgentRig(t,
		testkit.SubmitStep(map[string]string{"AAA": "strong_buy", "BBB": "hold"}, testkit.CitePrices),
		testkit.SubmitStep(map[string]string{"AAA": "strong_buy", "BBB": "hold"}, testkit.CitePrices),
	)
	ctx := context.Background()
	rep, err := r.agent.RunCycle(ctx, false)
	if err != nil || rep.Status != StatusDecided || len(rep.Orders) != 1 || rep.Orders[0].Symbol != "AAA" || rep.Orders[0].Side != "buy" {
		t.Fatalf("first decision: %+v %v", rep, err)
	}
	if again, err := r.agent.RunCycle(ctx, false); err != nil || again.Status != StatusUpToDate || r.api.Count() != 1 {
		t.Fatalf("re-running the same session must be a no-op: %+v %v", again, err)
	}

	r.advance(t, "2026-10-26")
	rep, err = r.agent.RunCycle(ctx, false)
	if err != nil || len(rep.Fills) != 1 || rep.Fills[0].Session != "2026-10-26" || len(rep.BenchmarkFills) != 2 {
		t.Fatalf("Friday's orders must fill at Monday's open: %+v %v", rep, err)
	}
	st, _ := LoadState(r.config)
	if st.Agent.Positions["AAA"] == 0 || st.Benchmark.Positions["BBB"] == 0 || len(st.History) != 2 {
		t.Fatalf("ledger after fills: %+v", st)
	}
	// strong_buy again at the same target: inside the band, so no new order.
	if len(rep.Orders) != 0 {
		t.Fatalf("an unchanged rating must not trade again: %+v", rep.Orders)
	}

	// The agent is down on Tuesday; Wednesday's cycle catches up.
	r.advance(t, "2026-10-28")
	rep, err = r.agent.RunCycle(ctx, false)
	if err != nil || len(rep.Processed) != 2 || rep.Status != StatusMissed && rep.Status != StatusDecided {
		t.Fatalf("catch-up: %+v %v", rep, err)
	}
	st, _ = LoadState(r.config)
	if len(st.MissedDecisions) != 1 || st.MissedDecisions[0] != "2026-10-27" {
		t.Fatalf("missed decision not recorded: %v", st.MissedDecisions)
	}
	if len(st.Scored) != 4 {
		t.Fatalf("signals from Friday and Monday should be scored by Wednesday, got %d", len(st.Scored))
	}
	for _, s := range st.Scored {
		if (s.Rating == analyst.RatingHold) != (s.Hit == nil) {
			t.Fatalf("hold ratings make no directional claim: %+v", s)
		}
	}
}

func TestLateCycleMissesTheDecisionButStillMarks(t *testing.T) {
	r := newAgentRig(t)
	r.clock = testkit.At(t, "2026-10-26 09:40") // Monday after the open; latest close is Friday
	rep, err := r.agent.RunCycle(context.Background(), false)
	if err != nil || rep.Status != StatusMissed || r.api.Count() != 0 {
		t.Fatalf("orders can no longer reach the open: %+v %v", rep, err)
	}
	st, _ := LoadState(r.config)
	if st.LastSession != "2026-10-23" || !st.BenchmarkStarted || len(st.BenchmarkPending) != 2 {
		t.Fatalf("state after missed decision: %+v", st)
	}
}

func TestUnpublishedBarIsRetriedLaterWithoutSavingState(t *testing.T) {
	r := newAgentRig(t)
	r.market.SetThrough("2026-10-22")
	_, err := r.agent.RunCycle(context.Background(), false)
	if !errors.Is(err, market.ErrDataNotReady) {
		t.Fatalf("expected data-not-ready, got %v", err)
	}
	st, _ := LoadState(r.config)
	if st.LastSession != "" {
		t.Fatal("state advanced without data")
	}
	if _, err := os.Stat(filepath.Join(r.config.DataDir, "cycles", "2026-10-23", "market")); !os.IsNotExist(err) {
		t.Fatal("incomplete data was archived and would be reused")
	}
}

func TestDryRunNeitherCallsTheModelNorSaves(t *testing.T) {
	r := newAgentRig(t)
	rep, err := r.agent.RunCycle(context.Background(), true)
	if err != nil || rep.Status != StatusDryRun || rep.Briefing == nil || len(rep.Briefing.Universe) != 2 || rep.Briefing.Market == nil {
		t.Fatalf("dry run: %+v %v", rep, err)
	}
	st, _ := LoadState(r.config)
	if r.api.Count() != 0 || st.LastSession != "" {
		t.Fatal("dry run must not call the model or change state")
	}
}

func TestBudgetCapRecordsMissedDecision(t *testing.T) {
	r := newAgentRig(t)
	r.agent.Analyst.Budget.DailyUSD = 0.001
	rep, err := r.agent.RunCycle(context.Background(), false)
	if err != nil || rep.Status != StatusMissed || r.api.Count() != 0 {
		t.Fatalf("budget cap: %+v %v", rep, err)
	}
}

func TestPermanentAPIErrorLeavesStateForRetry(t *testing.T) {
	r := newAgentRig(t, func(string) (int, any) {
		return 401, map[string]any{"type": "error", "error": map[string]any{"type": "authentication_error", "message": "bad key"}}
	})
	_, err := r.agent.RunCycle(context.Background(), false)
	var perm *analyst.PermanentError
	if !errors.As(err, &perm) {
		t.Fatalf("expected permanent error, got %v", err)
	}
	if st, _ := LoadState(r.config); st.LastSession != "" {
		t.Fatal("failed cycle must not advance the account")
	}
	r.api.Script = []func(string) (int, any){testkit.SubmitStep(map[string]string{"AAA": "buy", "BBB": "buy"}, testkit.CitePrices)}
	if rep, err := r.agent.RunCycle(context.Background(), false); err != nil || rep.Status != StatusDecided {
		t.Fatalf("retry after fixing credentials: %+v %v", rep, err)
	}
}

// runDaemonUntil drives the daemon on the rig's virtual clock until `until`,
// calling onNap after every nap. It returns the longest single nap.
func runDaemonUntil(t *testing.T, r *agentRig, until time.Time, onNap func()) time.Duration {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var longest time.Duration
	r.agent.Sleep = func(ctx context.Context, d time.Duration) error {
		longest = max(longest, d)
		r.clock = r.clock.Add(d)
		if onNap != nil {
			onNap()
		}
		if !r.clock.Before(until) {
			cancel()
			return ctx.Err()
		}
		return nil
	}
	if err := r.agent.Daemon(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("a long-running daemon must keep going: %v", err)
	}
	return longest
}

func TestDaemonRetriesTransientFailuresAndWakesForTheNextClose(t *testing.T) {
	r := newAgentRig(t,
		testkit.SubmitStep(map[string]string{"AAA": "buy", "BBB": "hold"}, testkit.CitePrices),
		testkit.SubmitStep(map[string]string{"AAA": "hold", "BBB": "hold"}, testkit.CitePrices))
	r.market.SetThrough("2026-10-22") // Friday's bar is late
	published := false
	longest := runDaemonUntil(t, r, testkit.At(t, "2026-10-26 17:00"), func() {
		if !published && !r.clock.Before(testkit.At(t, "2026-10-23 17:05")) {
			r.market.SetThrough("2026-10-23")
			published = true
		}
		if !r.clock.Before(testkit.At(t, "2026-10-26 16:00")) {
			r.market.SetThrough("2026-10-26")
		}
	})
	if longest > time.Minute {
		t.Fatalf("naps must stay short so a sleeping laptop catches up on wake: %v", longest)
	}
	st, _ := LoadState(r.config)
	if st.LastSession != "2026-10-26" || r.api.Count() != 2 {
		t.Fatalf("expected Friday's retried decision and Monday's decision: last %s, %d calls", st.LastSession, r.api.Count())
	}
	// Friday's cycle failed at 17:00 and succeeded on the 17:10 retry; Monday's ran at 16:30.
	if st.LastDecision.DecidedAt != "2026-10-26T20:30:00Z" || len(st.History) != 2 {
		t.Fatalf("unexpected schedule: decided %s, history %d", st.LastDecision.DecidedAt, len(st.History))
	}
}

func TestDaemonWaitsForTheNextCloseAfterPermanentErrors(t *testing.T) {
	r := newAgentRig(t, func(string) (int, any) {
		return 400, map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": "credit balance too low"}}
	})
	runDaemonUntil(t, r, testkit.At(t, "2026-10-26 16:29"), nil)
	// No hammering every ten minutes: the next attempt waits for Monday's close.
	if r.api.Count() != 1 {
		t.Fatalf("expected a single attempt before Monday's close, got %d", r.api.Count())
	}
	if st, _ := LoadState(r.config); st.LastSession != "" {
		t.Fatal("a failed cycle must not advance the account")
	}
}

func TestDaemonCatchesUpPromptlyAfterTheComputerSleeps(t *testing.T) {
	r := newAgentRig(t, testkit.SubmitStep(map[string]string{"AAA": "hold", "BBB": "hold"}, testkit.CitePrices))
	ctx, cancel := context.WithCancel(context.Background())
	naps := 0
	r.agent.Sleep = func(ctx context.Context, d time.Duration) error {
		naps++
		if naps == 1 {
			r.clock = testkit.At(t, "2026-10-26 18:00") // the laptop slept through Monday's close
			r.market.SetThrough("2026-10-26")
			return nil
		}
		cancel()
		return ctx.Err()
	}
	if err := r.agent.Daemon(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if st, _ := LoadState(r.config); st.LastSession != "2026-10-26" {
		t.Fatalf("after waking, the very next nap must lead to Monday's cycle: %s", st.LastSession)
	}
}

func TestSplitWhileHoldingKeepsAccountValue(t *testing.T) {
	r := newAgentRig(t, testkit.SubmitStep(map[string]string{"AAA": "strong_buy", "BBB": "hold"}, testkit.CitePrices), testkit.SubmitStep(map[string]string{"AAA": "hold", "BBB": "hold"}, testkit.CitePrices))
	ctx := context.Background()
	if _, err := r.agent.RunCycle(ctx, false); err != nil {
		t.Fatal(err)
	}
	r.advance(t, "2026-10-26")
	if _, err := r.agent.RunCycle(ctx, false); err != nil {
		t.Fatal(err)
	}
	before, _ := LoadState(r.config)
	held := before.Agent.Positions["AAA"]
	r.market.AddSplit("AAA", market.Split{Date: "2026-10-27", Numerator: 2, Denominator: 1})
	r.advance(t, "2026-10-27")
	rep, err := r.agent.RunCycle(ctx, false)
	if err != nil || len(rep.CorporateEvents) == 0 {
		t.Fatalf("split not applied: %+v %v", rep, err)
	}
	after, _ := LoadState(r.config)
	if after.Agent.Positions["AAA"] != 2*held || after.Benchmark.Positions["AAA"] != 2*before.Benchmark.Positions["AAA"] {
		t.Fatalf("2-for-1 split: %d -> %d", held, after.Agent.Positions["AAA"])
	}
	prev, last := after.History[len(after.History)-2], after.History[len(after.History)-1]
	if ratio := float64(last.AgentEquity) / float64(prev.AgentEquity); ratio < 0.97 || ratio > 1.03 {
		t.Fatalf("a split must not move account value: %v -> %v", prev.AgentEquity, last.AgentEquity)
	}
}

func newTestAnalyst(t *testing.T, c config.Config, apiURL, newsURL string) *analyst.Analyst {
	t.Helper()
	return &analyst.Analyst{
		Config: c, Client: testkit.Client(apiURL), News: market.NewsSource{HTTP: http.DefaultClient, BaseURL: newsURL},
		Budget: &analyst.Budget{Path: c.DataDir + "/budget.json", DailyUSD: c.MaxDailyLLMUSD, MonthlyUSD: c.MaxMonthlyLLMUSD},
		Sleep:  func(context.Context, time.Duration) error { return nil },
	}
}
