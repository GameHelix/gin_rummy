package agent

import (
	"strings"
	"testing"

	"github.com/ismatsamadov/trader/internal/analyst"
	"github.com/ismatsamadov/trader/internal/broker"
	"github.com/ismatsamadov/trader/internal/testkit"
)

func TestStatusComparesAgainstBuyAndHold(t *testing.T) {
	c := testkit.Config("unused")
	st := State{Name: "t", InitialCash: broker.Cents(10000), LastSession: "2026-10-27",
		Agent:      broker.Account{Cash: broker.Cents(6000), Positions: map[string]int64{"AAA": 40}, FeesPaid: broker.Cents(3)},
		Benchmark:  broker.Account{Cash: broker.Cents(2000), Positions: map[string]int64{"AAA": 40, "BBB": 40}, FeesPaid: broker.Cents(2)},
		LastCloses: map[string]broker.Money{"AAA": broker.Cents(105), "BBB": broker.Cents(90)},
		History: []EquityPoint{
			{Session: "2026-10-23", AgentEquity: broker.Cents(10000), BenchmarkEquity: broker.Cents(10000)},
			{Session: "2026-10-26", AgentEquity: broker.Cents(10500), BenchmarkEquity: broker.Cents(9800)},
			{Session: "2026-10-27", AgentEquity: broker.Cents(10200), BenchmarkEquity: broker.Cents(9800)},
		},
		LastDecision: &Decision{Session: "2026-10-27", Signals: map[string]analyst.Signal{"AAA": {Rating: analyst.RatingStrongBuy}}},
	}
	r := BuildStatus(c, st, &analyst.BudgetSummary{TodayUSD: 0.4, DailyLimitUSD: 3, MonthUSD: 2, MonthlyLimitUSD: 40, MonthCalls: 9}, testkit.At(t, "2026-10-27 18:00"))
	// Agent: $6,000 + 40 × $105 = $10,200. Benchmark: $2,000 + $4,200 + $3,600 = $9,800.
	if r.Agent.Equity != broker.Cents(10200) || r.Benchmark.Equity != broker.Cents(9800) {
		t.Fatalf("equity: %+v %+v", r.Agent, r.Benchmark)
	}
	if r.Agent.MaxDrawdown < 0.0285 || r.Agent.MaxDrawdown > 0.0286 || r.Benchmark.MaxDrawdown < 0.0199 || r.Benchmark.MaxDrawdown > 0.0201 {
		t.Fatalf("drawdowns: %v %v", r.Agent.MaxDrawdown, r.Benchmark.MaxDrawdown)
	}
	text := r.Text()
	for _, want := range []string{"next Wed Oct 28 16:30", "agent      $10200.00  +2.00%", "buy&hold   $9800.00  -2.00%", "positions  AAA 40", "ratings    AAA strong_buy (2026-10-27)", "spend      today $0.40/$3.00 · month $2.00/$40.00"} {
		if !strings.Contains(text, want) {
			t.Fatalf("status text lacks %q:\n%s", want, text)
		}
	}
}
