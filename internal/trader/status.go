package trader

import (
	"fmt"
	"sort"
	"strings"
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

// Text is a one-screen summary; `status --json` has the details.
func (r StatusReport) Text() string {
	var b strings.Builder
	line := func(label, value string) { fmt.Fprintf(&b, "%-10s %s\n", label, value) }
	through := "no session yet"
	if r.LastSession != "" {
		through = "through " + r.LastSession
	}
	fmt.Fprintf(&b, "%s · %s · next %s\n", r.Name, through, r.NextCycle)
	account := func(a AccountStatus) string {
		return fmt.Sprintf("$%.2f  %+.2f%%  dd %.2f%%  exposure %.0f%%", a.Equity.Dollars(), a.Return*100, a.MaxDrawdown*100, a.Exposure*100)
	}
	line("agent", account(r.Agent))
	line("buy&hold", account(r.Benchmark))
	positions := []string{}
	for _, p := range r.Positions {
		positions = append(positions, fmt.Sprintf("%s %d", p.Symbol, p.Shares))
	}
	line("positions", joinOr(positions, ", ", "none"))
	if len(r.Pending) > 0 {
		orders := []string{}
		for _, o := range r.Pending {
			orders = append(orders, fmt.Sprintf("%s %d %s", strings.ToUpper(o.Side), o.Quantity, o.Symbol))
		}
		line("orders", strings.Join(orders, ", "))
	}
	if d := r.LastDecision; d != nil {
		ratings := []string{}
		for _, p := range d.Plan {
			ratings = append(ratings, fmt.Sprintf("%s %s", p.Symbol, p.Rating))
		}
		if len(ratings) == 0 {
			for symbol, sig := range d.Signals {
				ratings = append(ratings, fmt.Sprintf("%s %s", symbol, sig.Rating))
			}
			sort.Strings(ratings)
		}
		value := strings.Join(ratings, " · ") + " (" + d.Session + ")"
		if d.Abstained {
			value = "abstained (" + d.Session + ")"
		}
		line("ratings", value)
	}
	scores := []string{}
	for _, s := range r.Scorecard {
		if score, _ := s.Rating.Score(); score != 0 {
			scores = append(scores, fmt.Sprintf("%s %.0f%% hit n=%d", s.Rating, float64(s.Hits)/float64(s.Count)*100, s.Count))
		}
	}
	line("scorecard", joinOr(scores, " · ", fmt.Sprintf("%d open, none scored yet", r.OpenSignals)))
	if len(r.MissedDecisions) > 0 {
		line("missed", strings.Join(r.MissedDecisions, ", "))
	}
	if r.Budget != nil {
		spend := fmt.Sprintf("today $%.2f/$%.2f · month $%.2f/$%.2f", r.Budget.TodayUSD, r.Budget.DailyLimitUSD, r.Budget.MonthUSD, r.Budget.MonthlyLimitUSD)
		if r.Budget.UnresolvedUSD > 0 {
			spend += fmt.Sprintf(" · unresolved $%.2f", r.Budget.UnresolvedUSD)
		}
		line("spend", spend)
	}
	return b.String()
}

func joinOr(items []string, sep, empty string) string {
	if len(items) == 0 {
		return empty
	}
	return strings.Join(items, sep)
}
