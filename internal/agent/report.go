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
	Coin   string  `json:"coin"`
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
	Ratings    string                 `json:"last_ratings,omitempty"`
	NextAt     time.Time              `json:"next_scheduled,omitzero"`
	Now        time.Time              `json:"time"`
	Trades     int                    `json:"trades"`
	Pending    int                    `json:"pending_orders"`
	Calls      int                    `json:"graded_calls"`
	Hits       int                    `json:"right_calls"`
	Budget     *analyst.BudgetSummary `json:"llm_budget,omitempty"`
}

func BuildReport(c config.Config, st State, prices map[string]num.Dec, budget *analyst.BudgetSummary, now time.Time) (Report, error) {
	equity, err := st.Ledger.Equity(prices)
	if err != nil {
		return Report{}, err
	}
	bench, err := st.Benchmark.Equity(prices)
	if err != nil {
		return Report{}, err
	}
	r := Report{Name: st.Name, Mode: st.Mode, Halted: st.Halted, Selling: st.Liquidate, Capital: st.Capital, Equity: equity, BuyAndHold: bench, Cash: st.Ledger.Cash,
		FeesUSD: st.Ledger.FeesUSD, Last: st.Last(), Now: now, Trades: len(st.Trades), Pending: len(st.Pending), Budget: budget}
	for _, s := range c.Symbols {
		v := st.Ledger.Holdings[s].Mul(prices[s])
		p := Position{Symbol: s, Coin: c.Base(s), Qty: st.Ledger.Holdings[s], Value: v}
		if equity.Sign() > 0 {
			p.Weight = round(v.Float()/equity.Float(), 4)
		}
		r.Positions = append(r.Positions, p)
	}
	if r.Last != nil {
		r.NextAt, r.Ratings = r.Last.Time.Add(c.DecisionInterval()), ratings(c, r.Last)
	}
	for _, sc := range st.Scored {
		r.Calls++
		if sc.Hit {
			r.Hits++
		}
	}
	return r, nil
}

// Text is the status message, one fact per line.
func (r Report) Text() string {
	var b strings.Builder
	where := "real money"
	if r.Mode != config.Live {
		where = "testnet: test money, real prices"
	}
	fmt.Fprintf(&b, "📊 Status (%s)\n", where)
	switch {
	case r.Selling:
		fmt.Fprintf(&b, "Trading: stopped, selling everything (%s)\n", r.Halted)
	case r.Halted != "":
		fmt.Fprintf(&b, "Trading: stopped (%s)\n", r.Halted)
	default:
		b.WriteString("Trading: on\n")
	}
	fmt.Fprintf(&b, "\nAccount: %s (%s since start)\n", usd(r.Equity), pct(r.Equity, r.Capital))
	fmt.Fprintf(&b, "Buy & hold would have: %s (%s)\n", usd(r.BuyAndHold), pct(r.BuyAndHold, r.Capital))
	held := false
	for _, p := range r.Positions {
		if !p.Qty.IsZero() {
			fmt.Fprintf(&b, "%s: %s = %s (%s)\n", p.Coin, p.Qty, usd(p.Value), share(p.Weight))
			held = true
		}
	}
	if !held {
		b.WriteString("Holdings: none\n")
	}
	if r.Equity.Sign() > 0 {
		fmt.Fprintf(&b, "Cash: %s (%s)\n", usd(r.Cash), share(r.Cash.Float()/r.Equity.Float()))
	}
	if d := r.Last; d != nil {
		fmt.Fprintf(&b, "\nLast decision #%d, %s ago (%s)\n%s\n", d.ID, ago(r.Now.Sub(d.Time)), d.Trigger, r.Ratings)
		if r.Halted == "" {
			fmt.Fprintf(&b, "Next check in %s, sooner on big moves or news\n", ago(r.NextAt.Sub(r.Now)))
		}
	} else {
		b.WriteString("\nNo decision yet\n")
	}
	fmt.Fprintf(&b, "\nTrades: %d · fees %s", r.Trades, usd(r.FeesUSD))
	if r.Pending > 0 {
		fmt.Fprintf(&b, " · %d order(s) being checked", r.Pending)
	}
	if r.Calls > 0 {
		fmt.Fprintf(&b, "\nCalls right: %d of %d", r.Hits, r.Calls)
	}
	if s := r.Budget; s != nil {
		fmt.Fprintf(&b, "\nAI cost: $%.2f today (limit $%.0f), $%.2f this month (limit $%.0f)", s.TodayUSD, s.DailyLimitUSD, s.MonthUSD, s.MonthlyLimitUSD)
	}
	return b.String()
}

func ratings(c config.Config, d *Decision) string {
	if d.Abstained {
		return "No valid ratings; nothing changed"
	}
	var out []string
	for _, s := range slices.Sorted(maps.Keys(d.Signals)) {
		out = append(out, c.Base(s)+": "+label(d.Signals[s].Rating))
	}
	return strings.Join(out, " · ")
}

var labels = map[analyst.Rating]string{
	analyst.RatingStrongBuy: "STRONG BUY", analyst.RatingBuy: "BUY", analyst.RatingHold: "HOLD",
	analyst.RatingSell: "SELL", analyst.RatingStrongSell: "STRONG SELL",
}

func label(r analyst.Rating) string { return labels[r] }

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
	return BuildReport(e.Config, st, prices, budget, e.now())
}

// report is the running account at the loop's latest prices.
func (e *Engine) report(ctx context.Context) (Report, error) {
	var budget *analyst.BudgetSummary
	if s, err := e.Analyst.Budget.Summary(ctx); err == nil {
		budget = &s
	}
	return BuildReport(e.Config, e.st, e.prices, budget, e.now())
}

func (e *Engine) statusText(ctx context.Context) string {
	r, err := e.report(ctx)
	if err != nil {
		return "⚠️ Status unavailable: " + err.Error()
	}
	return r.Text()
}

func (e *Engine) startText() string {
	c := e.Config
	where := "with real money"
	if c.Mode != config.Live {
		where = "on testnet: test money, real prices. Messages marked 🧪 are not real trades"
	}
	coins := strings.Join(e.bases(), " and ")
	return fmt.Sprintf(`▶️ Agent started %s.
Capital: %s in %s
Decides every %gh, sooner on a %g%% move within an hour or major news (at most %d extra a day)
Loss limits: pause after −%g%% in a day; sell everything at −%g%%

%s`, where, usd(e.st.Capital), coins, c.DecisionIntervalHours, c.MoveTrigger1HPct, c.MaxEventDecisionsPerDay, c.DailyLossLimitPct, c.MaxDrawdownPct, helpText)
}

func (e *Engine) dailyText(ctx context.Context, equity num.Dec) string {
	bench, _ := e.st.Benchmark.Equity(e.prices)
	trades := 0
	for _, t := range e.st.Trades {
		if t.Time.Format(time.DateOnly) == e.st.Day {
			trades++
		}
	}
	day, _ := time.Parse(time.DateOnly, e.st.Day)
	text := fmt.Sprintf("📅 Summary for %s\nAccount: %s (%s today, %s since start)\nBuy & hold would have: %s (%s since start)\nTrades today: %d",
		day.Format("Mon Jan 2"), usd(equity), pct(equity, e.st.DayEquity), pct(equity, e.st.Capital), usd(bench), pct(bench, e.st.Capital), trades)
	if s, err := e.Analyst.Budget.Summary(ctx); err == nil {
		text += fmt.Sprintf("\nAI cost this month: $%.2f", s.MonthUSD)
	}
	return text
}

// decisionText reports the ratings with their reasons and the orders about
// to be placed; each fill follows as its own message.
func (e *Engine) decisionText(d Decision, plans []Plan, note string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🧠 Decision #%d: %s\n", d.ID, d.Trigger)
	if !d.Abstained {
		for _, s := range slices.Sorted(maps.Keys(d.Signals)) {
			sig := d.Signals[s]
			fmt.Fprintf(&b, "\n%s: %s (%s confidence)\n%s\n", e.Config.Base(s), label(sig.Rating), sig.Confidence, untrusted(sig.Rationale))
		}
		fmt.Fprintf(&b, "\nOverall: %s\n", untrusted(d.Summary))
	}
	switch {
	case note != "":
		b.WriteString("\n" + note)
	case len(plans) == 0:
		b.WriteString("\n➡️ No trades: holdings already match the ratings, or the change is too small.")
	default:
		var orders []string
		for _, p := range plans {
			if p.Side == "SELL" {
				orders = append(orders, fmt.Sprintf("sell %s %s", p.Qty, e.Config.Base(p.Symbol)))
			} else {
				orders = append(orders, fmt.Sprintf("buy %s of %s", usd(p.QuoteQty), e.Config.Base(p.Symbol)))
			}
		}
		b.WriteString("\n➡️ Placing orders: " + strings.Join(orders, ", "))
	}
	return b.String()
}

func (e *Engine) tradeText(t Trade) string {
	coin := e.Config.Base(t.Symbol)
	action := "Bought"
	if t.Side == "SELL" {
		action = "Sold"
	}
	text := fmt.Sprintf("✅ %s %s %s for %s (price %s, fee %s)", action, t.Qty, coin, usd(t.QuoteQty), usd(t.Price), usd(t.FeeUSD))
	if held := e.st.Ledger.Holdings[t.Symbol]; held.Sign() > 0 {
		text += fmt.Sprintf("\nNow holding %s %s", held, coin)
	}
	return text
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

func share(w float64) string { return fmt.Sprintf("%.0f%%", w*100) }

// ago renders a duration as "3h 05m" or "12m".
func ago(d time.Duration) string {
	d = max(d, 0).Round(time.Minute)
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
}
