package agent

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ismatsamadov/trader/internal/analyst"
	"github.com/ismatsamadov/trader/internal/config"
	"github.com/ismatsamadov/trader/internal/num"
)

type Position struct {
	Symbol string  `json:"symbol"`
	Qty    num.Dec `json:"qty"`
	Value  num.Dec `json:"value"`
	Weight float64 `json:"weight"`
}

type Report struct {
	Name       string                 `json:"name"`
	Mode       string                 `json:"mode"`
	Halted     string                 `json:"halted,omitempty"`
	Selling    bool                   `json:"selling_everything,omitempty"`
	Capital    num.Dec                `json:"capital"`
	Equity     num.Dec                `json:"equity"`
	BuyAndHold num.Dec                `json:"buy_and_hold"`
	Cash       num.Dec                `json:"cash"`
	FeesUSD    num.Dec                `json:"fees_usd"`
	Positions  []Position             `json:"positions"`
	Last       *Decision              `json:"last_decision,omitempty"`
	NextAt     time.Time              `json:"next_scheduled"`
	Trades     int                    `json:"trades"`
	Pending    int                    `json:"pending_orders"`
	Calls      int                    `json:"graded_calls"`
	Hits       int                    `json:"right_calls"`
	Budget     *analyst.BudgetSummary `json:"llm_budget,omitempty"`
}

func BuildReport(c config.Config, st State, prices map[string]num.Dec, budget *analyst.BudgetSummary) (Report, error) {
	equity, err := st.Ledger.Equity(prices)
	if err != nil {
		return Report{}, err
	}
	bench, err := st.Benchmark.Equity(prices)
	if err != nil {
		return Report{}, err
	}
	r := Report{Name: st.Name, Mode: st.Mode, Halted: st.Halted, Selling: st.Liquidate, Capital: st.Capital, Equity: equity, BuyAndHold: bench, Cash: st.Ledger.Cash,
		FeesUSD: st.Ledger.FeesUSD, Last: st.Last(), Trades: len(st.Trades), Pending: len(st.Pending), Budget: budget}
	for _, s := range c.Symbols {
		v := st.Ledger.Holdings[s].Mul(prices[s])
		r.Positions = append(r.Positions, Position{Symbol: s, Qty: st.Ledger.Holdings[s], Value: v, Weight: round(v.Float()/equity.Float(), 4)})
	}
	if r.Last != nil {
		r.NextAt = r.Last.Time.Add(c.DecisionInterval())
	}
	for _, sc := range st.Scored {
		r.Calls++
		if sc.Hit {
			r.Hits++
		}
	}
	return r, nil
}

func (r Report) Text() string {
	var b strings.Builder
	state := "trading"
	if r.Halted != "" {
		state = "stopped: " + r.Halted
	}
	if r.Selling {
		state += " · selling everything"
	}
	fmt.Fprintf(&b, "%s · %s · %s\n", r.Name, r.Mode, state)
	fmt.Fprintf(&b, "equity %s (%s) · buy&hold %s (%s)\n", usd(r.Equity), pct(r.Equity, r.Capital), usd(r.BuyAndHold), pct(r.BuyAndHold, r.Capital))
	var pos []string
	for _, p := range r.Positions {
		coin := strings.TrimSuffix(p.Symbol, "USDT")
		if p.Qty.IsZero() {
			pos = append(pos, coin+" none")
			continue
		}
		pos = append(pos, fmt.Sprintf("%s %s %s (%.0f%%)", coin, p.Qty, usd(p.Value), p.Weight*100))
	}
	fmt.Fprintf(&b, "%s · cash %s\n", strings.Join(pos, " · "), usd(r.Cash))
	if d := r.Last; d != nil {
		fmt.Fprintf(&b, "#%d %s %s: %s\n", d.ID, d.Time.Format("Jan 2 15:04 UTC"), d.Trigger, ratings(d))
		fmt.Fprintf(&b, "next scheduled %s · ", r.NextAt.Format("Jan 2 15:04 UTC"))
	} else {
		b.WriteString("no decision yet · ")
	}
	fmt.Fprintf(&b, "%d trades · fees %s", r.Trades, usd(r.FeesUSD))
	if r.Pending > 0 {
		fmt.Fprintf(&b, " · %d order(s) being checked", r.Pending)
	}
	b.WriteString("\n")
	if r.Calls > 0 {
		fmt.Fprintf(&b, "calls %d/%d right · ", r.Hits, r.Calls)
	}
	if s := r.Budget; s != nil {
		fmt.Fprintf(&b, "AI today $%.2f/$%.0f · month $%.2f/$%.0f", s.TodayUSD, s.DailyLimitUSD, s.MonthUSD, s.MonthlyLimitUSD)
	}
	return strings.TrimRight(b.String(), " ·\n")
}

func ratings(d *Decision) string {
	if d.Abstained {
		return "no valid ratings, holding"
	}
	var out []string
	for _, s := range slices.Sorted(maps.Keys(d.Signals)) {
		out = append(out, strings.TrimSuffix(s, "USDT")+" "+string(d.Signals[s].Rating))
	}
	return strings.Join(out, " · ")
}

// Report reads the saved account and values it at live prices.
func (e *Engine) Report(ctx context.Context) (Report, error) {
	st, err := LoadState(e.Config)
	if err != nil {
		return Report{}, err
	}
	prices, err := e.Exchange.Prices(ctx, e.Config.Symbols)
	if err != nil {
		return Report{}, err
	}
	var budget *analyst.BudgetSummary
	if e.Analyst != nil && e.Analyst.Budget != nil {
		if s, err := e.Analyst.Budget.Summary(ctx); err == nil {
			budget = &s
		}
	}
	return BuildReport(e.Config, st, prices, budget)
}

func (e *Engine) statusText(ctx context.Context) string {
	var budget *analyst.BudgetSummary
	if s, err := e.Analyst.Budget.Summary(ctx); err == nil {
		budget = &s
	}
	r, err := BuildReport(e.Config, e.st, e.prices, budget)
	if err != nil {
		return "Status unavailable: " + err.Error()
	}
	return r.Text()
}

func (e *Engine) startText() string {
	c := e.Config
	return fmt.Sprintf("Started %s with %s capital. Decides every %gh, on %g%% hourly moves and on major news. Send /help for commands.",
		c.Name, usd(e.st.Capital), c.DecisionIntervalHours, c.MoveTrigger1HPct)
}

func (e *Engine) dailyText(ctx context.Context, equity num.Dec) string {
	bench, _ := e.st.Benchmark.Equity(e.prices)
	text := fmt.Sprintf("Day %s closed: equity %s (day %s, total %s) · buy&hold %s (%s)", e.st.Day, usd(equity), pct(equity, e.st.DayEquity), pct(equity, e.st.Capital), usd(bench), pct(bench, e.st.Capital))
	if s, err := e.Analyst.Budget.Summary(ctx); err == nil {
		text += fmt.Sprintf(" · AI this month $%.2f", s.MonthUSD)
	}
	return text
}

func (e *Engine) decisionText(d Decision, trades []Trade, note string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Decision #%d (%s)\n", d.ID, d.Trigger)
	if d.Abstained {
		b.WriteString("No valid ratings; holding everything.")
		return b.String()
	}
	for _, s := range slices.Sorted(maps.Keys(d.Signals)) {
		sig := d.Signals[s]
		fmt.Fprintf(&b, "%s %s (%s): %s\n", strings.TrimSuffix(s, "USDT"), sig.Rating, sig.Confidence, untrusted(sig.Rationale))
	}
	b.WriteString(untrusted(d.Summary))
	switch {
	case note != "":
		b.WriteString("\n" + note)
	case len(trades) == 0:
		b.WriteString("\nNo trades.")
	}
	return b.String()
}

func (e *Engine) tradeText(t Trade) string {
	verb := "Bought"
	if t.Side == "SELL" {
		verb = "Sold"
	}
	return fmt.Sprintf("%s %s %s at %s for %s (fee %s)", verb, t.Qty, e.Config.Base(t.Symbol), usd(t.Price), usd(t.QuoteQty), usd(t.FeeUSD))
}

var slashCommand = regexp.MustCompile(`(^|[\s(\[])/([A-Za-z])`)

// untrusted defuses text written by the model or a third party before it
// goes to Telegram: a "/command" in it would be one tap away from running.
func untrusted(s string) string { return slashCommand.ReplaceAllString(s, "$1∕$2") }

// usd formats dollars with cents and thousands separators.
func usd(d num.Dec) string {
	s := d.Fixed(2)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	whole, frac, _ := strings.Cut(s, ".")
	for i := len(whole) - 3; i > 0; i -= 3 {
		whole = whole[:i] + "," + whole[i:]
	}
	return sign + "$" + whole + "." + frac
}

func pct(v, base num.Dec) string {
	if base.Sign() <= 0 {
		return "n/a"
	}
	return fmt.Sprintf("%+.2f%%", (v.Float()/base.Float()-1)*100)
}
