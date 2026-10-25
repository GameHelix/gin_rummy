package trader

import (
	"strings"
	"testing"
)

func TestStatusComparesAgainstBuyAndHold(t *testing.T) {
	c := testConfig("unused")
	st := State{Name: "t", InitialCash: Cents(10000), LastSession: "2026-10-27",
		Agent:      Account{Cash: Cents(6000), Positions: map[string]int64{"AAA": 40}, FeesPaid: Cents(3)},
		Benchmark:  Account{Cash: Cents(2000), Positions: map[string]int64{"AAA": 40, "BBB": 40}, FeesPaid: Cents(2)},
		LastCloses: map[string]Money{"AAA": Cents(105), "BBB": Cents(90)},
		History: []EquityPoint{
			{Session: "2026-10-23", AgentEquity: Cents(10000), BenchmarkEquity: Cents(10000)},
			{Session: "2026-10-26", AgentEquity: Cents(10500), BenchmarkEquity: Cents(9800)},
			{Session: "2026-10-27", AgentEquity: Cents(10200), BenchmarkEquity: Cents(9800)},
		},
		LastDecision: &Decision{Session: "2026-10-27", Signals: map[string]Signal{"AAA": {Rating: RatingStrongBuy}}},
	}
	r := BuildStatus(c, st, &BudgetSummary{TodayUSD: 0.4, DailyLimitUSD: 3, MonthUSD: 2, MonthlyLimitUSD: 40, MonthCalls: 9}, at(t, "2026-10-27 18:00"))
	// Agent: $6,000 + 40 × $105 = $10,200. Benchmark: $2,000 + $4,200 + $3,600 = $9,800.
	if r.Agent.Equity != Cents(10200) || r.Benchmark.Equity != Cents(9800) {
		t.Fatalf("equity: %+v %+v", r.Agent, r.Benchmark)
	}
	if r.Agent.MaxDrawdown < 0.0285 || r.Agent.MaxDrawdown > 0.0286 || r.Benchmark.MaxDrawdown < 0.0199 || r.Benchmark.MaxDrawdown > 0.0201 {
		t.Fatalf("drawdowns: %v %v", r.Agent.MaxDrawdown, r.Benchmark.MaxDrawdown)
	}
	text := r.Text()
	for _, want := range []string{"ahead of buy-and-hold by 4.00 percentage points", "AAA", "strong_buy", "this month $2.00 of $40.00 (9 calls)", "Next cycle: Wed Oct 28 16:30"} {
		if !strings.Contains(text, want) {
			t.Fatalf("status text lacks %q:\n%s", want, text)
		}
	}
}
