package trader

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

type AccountStatus struct {
	Equity      Money   `json:"equity_cents"`
	Cash        Money   `json:"cash_cents"`
	Receivable  Money   `json:"dividend_receivable_cents"`
	Fees        Money   `json:"fees_paid_cents"`
	Return      float64 `json:"return"`
	MaxDrawdown float64 `json:"max_drawdown"`
	Exposure    float64 `json:"exposure"`
}

type PositionLine struct {
	Symbol string  `json:"symbol"`
	Shares int64   `json:"shares"`
	Price  Money   `json:"price_cents"`
	Value  Money   `json:"value_cents"`
	Weight float64 `json:"weight"`
	Rating Rating  `json:"last_rating,omitempty"`
}

type StatusReport struct {
	Name            string         `json:"name"`
	Inception       string         `json:"inception,omitempty"`
	LastSession     string         `json:"last_session,omitempty"`
	NextCycle       string         `json:"next_cycle,omitempty"`
	Agent           AccountStatus  `json:"agent"`
	Benchmark       AccountStatus  `json:"benchmark"`
	Positions       []PositionLine `json:"positions"`
	Pending         []Order        `json:"pending_orders"`
	LastDecision    *Decision      `json:"last_decision,omitempty"`
	Scorecard       []ScoreSummary `json:"scorecard"`
	OpenSignals     int            `json:"open_signals"`
	MissedDecisions []string       `json:"missed_decisions,omitempty"`
	Budget          *BudgetSummary `json:"llm_budget,omitempty"`
}

func BuildStatus(c Config, st State, budget *BudgetSummary, now time.Time) StatusReport {
	r := StatusReport{Name: st.Name, LastSession: st.LastSession, Pending: st.Pending, LastDecision: st.LastDecision, Scorecard: Summarize(st.Scored), OpenSignals: len(st.OpenSignals), MissedDecisions: st.MissedDecisions, Budget: budget}
	if next, err := NextCycleTime(now, c.CloseBuffer()); err == nil {
		r.NextCycle = next.In(newYork).Format("Mon Jan 2 15:04 MST")
	}
	if len(st.History) > 0 {
		r.Inception = st.History[0].Session
	}
	r.Agent = accountStatus(st.Agent, st.InitialCash, st.LastCloses, st.History, func(p EquityPoint) Money { return p.AgentEquity })
	r.Benchmark = accountStatus(st.Benchmark, st.InitialCash, st.LastCloses, st.History, func(p EquityPoint) Money { return p.BenchmarkEquity })
	for symbol, shares := range st.Agent.Positions {
		price := st.LastCloses[symbol]
		value, _ := shareValue(shares, price)
		line := PositionLine{Symbol: symbol, Shares: shares, Price: price, Value: value}
		if r.Agent.Equity > 0 {
			line.Weight = float64(value) / float64(r.Agent.Equity)
		}
		if st.LastDecision != nil {
			line.Rating = st.LastDecision.Signals[symbol].Rating
		}
		r.Positions = append(r.Positions, line)
	}
	sort.Slice(r.Positions, func(i, j int) bool { return r.Positions[i].Symbol < r.Positions[j].Symbol })
	return r
}

func accountStatus(a Account, initial Money, closes map[string]Money, history []EquityPoint, equityOf func(EquityPoint) Money) AccountStatus {
	s := AccountStatus{Cash: a.Cash, Receivable: a.Receivable, Fees: a.FeesPaid}
	invested, _ := a.Invested(closes)
	s.Equity, _ = a.Equity(closes)
	if initial > 0 {
		s.Return = float64(s.Equity)/float64(initial) - 1
	}
	if s.Equity > 0 {
		s.Exposure = float64(invested) / float64(s.Equity)
	}
	peak := initial
	for _, p := range history {
		e := equityOf(p)
		peak = max(peak, e)
		if peak > 0 {
			s.MaxDrawdown = max(s.MaxDrawdown, 1-float64(e)/float64(peak))
		}
	}
	return s
}

func (r StatusReport) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s · paper account\n", r.Name)
	if r.LastSession == "" {
		fmt.Fprintf(&b, "No session processed yet. Next cycle: %s.\n", r.NextCycle)
	} else {
		fmt.Fprintf(&b, "Through %s (since %s). Next cycle: %s.\n", r.LastSession, r.Inception, r.NextCycle)
	}
	b.WriteString("\n")
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "\tequity\treturn\tmax drawdown\texposure\tfees\t")
	fmt.Fprintf(w, "agent\t$%.2f\t%+.2f%%\t%.2f%%\t%.0f%%\t$%.2f\t\n", r.Agent.Equity.Dollars(), r.Agent.Return*100, r.Agent.MaxDrawdown*100, r.Agent.Exposure*100, r.Agent.Fees.Dollars())
	fmt.Fprintf(w, "buy & hold\t$%.2f\t%+.2f%%\t%.2f%%\t%.0f%%\t$%.2f\t\n", r.Benchmark.Equity.Dollars(), r.Benchmark.Return*100, r.Benchmark.MaxDrawdown*100, r.Benchmark.Exposure*100, r.Benchmark.Fees.Dollars())
	w.Flush()
	verdict := "ahead of"
	if r.Agent.Return < r.Benchmark.Return {
		verdict = "behind"
	}
	fmt.Fprintf(&b, "\nAgent is %s buy-and-hold by %.2f percentage points (before LLM costs).\n", verdict, (r.Agent.Return-r.Benchmark.Return)*100)
	if len(r.Positions) > 0 {
		b.WriteString("\nPositions:\n")
		w = tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		for _, p := range r.Positions {
			fmt.Fprintf(w, "  %s\t%d sh\t$%.2f\t%.1f%%\t%s\n", p.Symbol, p.Shares, p.Value.Dollars(), p.Weight*100, p.Rating)
		}
		w.Flush()
	}
	fmt.Fprintf(&b, "Cash $%.2f", r.Agent.Cash.Dollars())
	if r.Agent.Receivable > 0 {
		fmt.Fprintf(&b, " · dividends receivable $%.2f", r.Agent.Receivable.Dollars())
	}
	b.WriteString("\n")
	if len(r.Pending) > 0 {
		b.WriteString("\nOrders for the next open:\n")
		for _, o := range r.Pending {
			fmt.Fprintf(&b, "  %s %d %s — %s\n", strings.ToUpper(o.Side), o.Quantity, o.Symbol, o.Reason)
		}
	}
	if d := r.LastDecision; d != nil {
		fmt.Fprintf(&b, "\nLast decision (%s, est. $%.2f):", d.Session, d.CostUSD)
		if d.Abstained {
			fmt.Fprintf(&b, " abstained — %s", d.Note)
		}
		if d.Summary != "" {
			fmt.Fprintf(&b, "\n  %s", d.Summary)
		}
		b.WriteString("\n")
		for _, line := range d.Plan {
			fmt.Fprintf(&b, "  %-6s %-11s %s\n", line.Symbol, line.Rating, line.Note)
		}
	}
	b.WriteString("\nSignal scorecard (excess return vs benchmark over the horizon):\n")
	if len(r.Scorecard) == 0 {
		fmt.Fprintf(&b, "  no signals have matured yet (%d open)\n", r.OpenSignals)
	}
	for _, s := range r.Scorecard {
		if score, _ := s.Rating.Score(); score == 0 {
			fmt.Fprintf(&b, "  %-11s n=%-4d mean excess %+.2f%%\n", s.Rating, s.Count, s.MeanExcess*100)
		} else {
			fmt.Fprintf(&b, "  %-11s n=%-4d hit rate %3.0f%%  mean excess %+.2f%%\n", s.Rating, s.Count, float64(s.Hits)/float64(s.Count)*100, s.MeanExcess*100)
		}
	}
	if len(r.MissedDecisions) > 0 {
		fmt.Fprintf(&b, "\nMissed decisions: %s\n", strings.Join(r.MissedDecisions, ", "))
	}
	if r.Budget != nil {
		fmt.Fprintf(&b, "\nLLM spend: today $%.2f of $%.2f · this month $%.2f of $%.2f (%d calls)", r.Budget.TodayUSD, r.Budget.DailyLimitUSD, r.Budget.MonthUSD, r.Budget.MonthlyLimitUSD, r.Budget.MonthCalls)
		if r.Budget.UnresolvedUSD > 0 {
			fmt.Fprintf(&b, " · unresolved $%.2f", r.Budget.UnresolvedUSD)
		}
		b.WriteString("\n")
	}
	return b.String()
}
