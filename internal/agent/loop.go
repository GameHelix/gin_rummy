package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/wickwise/trader/internal/analyst"
	"github.com/wickwise/trader/internal/config"
	"github.com/wickwise/trader/internal/news"
	"github.com/wickwise/trader/internal/num"
)

// tick is one pass of the loop: settle orders, refresh prices, record
// equity, enforce loss limits, keep selling if a sell-off is under way,
// grade old calls, read news, and start a decision if one is due.
func (e *Engine) tick(ctx context.Context) {
	now := e.now()
	if e.Config.Mode == config.Live && !e.keyOK && now.Sub(e.keyAt) >= 10*time.Minute {
		e.verifyKey(ctx)
	}
	if len(e.st.Pending) > 0 {
		e.recover(ctx)
	}
	if err := e.watch(ctx); err != nil {
		e.alert(ctx, "prices", "⚠️ Price data unavailable: "+err.Error())
		return
	}
	equity, err := e.st.Ledger.Equity(e.prices)
	if err != nil {
		e.alert(ctx, "equity", "⚠️ "+err.Error())
		return
	}
	e.record(now, equity)
	e.rollover(ctx, now, equity)
	e.limits(ctx, equity)
	if e.st.Liquidate {
		e.sellAll(ctx)
	}
	e.score(ctx, now)
	e.readNews(ctx, now)
	if kind, trigger := e.trigger(now); trigger != "" {
		if err := e.startDecision(ctx, kind, trigger); err != nil {
			e.failed(ctx, now, 0, err)
		}
	}
}

// failed backs off after a decision could not be made. A spent budget only
// resets with the UTC day, and the message says what that means.
func (e *Engine) failed(ctx context.Context, now time.Time, id int, err error) {
	e.emit(evDecisionFailed, decisionFailed{Decision: id, Error: err.Error()})
	e.st.RetryAt = now.Add(10 * time.Minute)
	msg := "⚠️ Decision skipped: " + untrusted(err.Error()) + ". Retrying in 10 minutes."
	if errors.Is(err, analyst.ErrBudgetExceeded) {
		e.st.RetryAt = now.Truncate(24 * time.Hour).Add(24 * time.Hour)
		msg = fmt.Sprintf("⚠️ AI budget used up: %v.\nNo new decisions until %s. Holdings are kept and the loss limits still apply; /sellall sells everything.", err, e.st.RetryAt.Format("Jan 2 15:04 UTC"))
	}
	e.alert(ctx, "decide", msg)
}

// watch reads the last hour of one-minute candles from the live market, for
// the model and the move triggers, and the trading venue's own prices, for
// valuing the account and sizing orders. On testnet the two differ slightly;
// live they are the same market.
func (e *Engine) watch(ctx context.Context) error {
	live, moves := map[string]num.Dec{}, map[string]float64{}
	for _, s := range e.Config.Symbols {
		k, err := e.Exchange.Klines(ctx, s, "1m", 61, time.Time{})
		if err != nil {
			return err
		}
		if len(k) < 2 {
			return fmt.Errorf("%s: no recent candles", s)
		}
		p, err := num.FromFloat(k[len(k)-1].C)
		if err != nil {
			return err
		}
		live[s], moves[s] = p, k[len(k)-1].C/k[0].O-1
	}
	prices, err := e.Exchange.Prices(ctx, e.Config.Symbols)
	if err != nil {
		return err
	}
	e.prices, e.live, e.moves = prices, live, moves
	return nil
}

func (e *Engine) record(now time.Time, equity num.Dec) {
	hour := now.Truncate(time.Hour)
	if n := len(e.st.History); n > 0 && !e.st.History[n-1].Time.Before(hour) {
		return
	}
	bench, _ := e.st.Benchmark.Equity(e.prices)
	e.st.History = keep(append(e.st.History, Point{hour, equity, bench}), 24*90)
	e.emit(evEquity, equityHourly{Equity: equity, Benchmark: bench, Cash: e.st.Ledger.Cash, Prices: e.prices})
}

func (e *Engine) rollover(ctx context.Context, now time.Time, equity num.Dec) {
	day := now.Format(time.DateOnly)
	if e.st.Day == day {
		return
	}
	if e.st.Day != "" {
		e.notify(ctx, e.dailyText(ctx, equity))
	}
	e.st.Day, e.st.DayEquity = day, equity
	if e.st.HaltKind == haltDaily {
		e.clearHalt(haltDaily)
		e.notify(ctx, "▶️ New day: trading resumed after yesterday's loss limit.")
	}
}

// limits pauses trading after a bad day and starts a sell-off after a deep
// drawdown. Under a manual stop nothing is sold: the drawdown is reported
// and the decision left to the user.
func (e *Engine) limits(ctx context.Context, equity num.Dec) {
	c := e.Config
	floor := func(base num.Dec, pct float64) num.Dec {
		f, _ := num.FromFloat(1 - pct/100)
		return base.Mul(f)
	}
	if c.MaxDrawdownPct > 0 && !e.st.DrawdownHit && !floor(e.st.RiskBase, c.MaxDrawdownPct).Less(equity) {
		e.st.DrawdownHit = true
		loss := fmt.Sprintf("equity %s is %.1f%% below %s, past the %.0f%% limit", usd(equity), 100*(1-equity.Float()/e.st.RiskBase.Float()), usd(e.st.RiskBase), c.MaxDrawdownPct)
		if e.st.HaltKind == haltManual {
			e.notify(ctx, "⚠️ Loss limit reached: "+loss+". You stopped trading, so nothing is sold; /sellall sells everything.")
		} else {
			e.st.Liquidate = true
			e.halt(ctx, haltDrawdown, loss+", so everything is being sold. /resume to trade again")
		}
	}
	if c.DailyLossLimitPct > 0 && e.st.Halted == "" && !floor(e.st.DayEquity, c.DailyLossLimitPct).Less(equity) {
		e.halt(ctx, haltDaily, fmt.Sprintf("down %.1f%% today, past the %.0f%% daily limit. No new trades until 00:00 UTC", 100*(1-equity.Float()/e.st.DayEquity.Float()), c.DailyLossLimitPct))
	}
}

// guard re-checks the loss limits at current prices and reports whether new
// orders may go out.
func (e *Engine) guard(ctx context.Context) bool {
	if equity, err := e.st.Ledger.Equity(e.prices); err == nil {
		e.limits(ctx, equity)
	}
	return e.st.Halted == "" && !e.st.Liquidate && (e.Config.Mode != config.Live || e.keyOK)
}

// score grades calls whose horizon has passed against the price at Due.
func (e *Engine) score(ctx context.Context, now time.Time) {
	var open []OpenCall
	for _, oc := range e.st.Open {
		if now.Before(oc.Due) {
			open = append(open, oc)
			continue
		}
		k, err := e.Exchange.Klines(ctx, oc.Symbol, "1m", 1, oc.Due)
		if err != nil || len(k) == 0 {
			open = append(open, oc)
			continue
		}
		exit, err := num.FromFloat(k[0].O)
		if err != nil || oc.Entry.Sign() <= 0 {
			continue
		}
		ret := exit.Float()/oc.Entry.Float() - 1
		score, _ := oc.Rating.Score()
		graded := ScoredCall{OpenCall: oc, Exit: exit, Return: round(ret, 5), Hit: (score > 0 && ret > 0) || (score < 0 && ret < 0)}
		e.st.Scored = keep(append(e.st.Scored, graded), 1000)
		e.emit(evCallGraded, graded)
	}
	e.st.Open = open
}

// readNews refreshes headlines every five minutes. New, recent headlines
// about a traded coin that match an alert keyword are reported and queued
// until a decision runs. The first read after a fresh start only marks what
// is already out there as seen.
func (e *Engine) readNews(ctx context.Context, now time.Time) {
	if e.News == nil || now.Sub(e.newsAt) < 5*time.Minute {
		return
	}
	e.newsAt = now
	items, err := e.News.Fetch(ctx, now, 72*time.Hour, e.bases())
	if err != nil {
		e.alert(ctx, "news", "⚠️ News feeds unavailable: "+err.Error())
		return
	}
	e.headlines = items
	first := !e.st.NewsPrimed
	e.st.NewsPrimed = true
	seen := map[string]bool{}
	for _, id := range e.st.SeenNews {
		seen[id] = true
	}
	var alerts []news.Item
	for _, it := range items {
		if seen[it.ID] {
			continue
		}
		e.st.SeenNews = append(e.st.SeenNews, it.ID)
		if !first && len(it.Coins) > 0 && now.Sub(it.PublishedAt) < 2*time.Hour && news.Mentions(it.Title, e.Config.NewsAlertKeywords) {
			alerts = append(alerts, it)
		}
	}
	e.st.SeenNews = keep(e.st.SeenNews, 2000)
	if len(alerts) == 0 {
		return
	}
	e.st.NewsQueue = append(e.st.NewsQueue, alerts...)
	e.emit(evNewsAlert, newsAlert{Items: alerts})
	lines := []string{"📰 Market news"}
	for _, it := range alerts[:min(3, len(alerts))] {
		lines = append(lines, fmt.Sprintf("• %s (%s)", untrusted(it.Title), it.Source))
	}
	if len(alerts) > 3 {
		lines = append(lines, fmt.Sprintf("and %d more", len(alerts)-3))
	}
	e.notify(ctx, strings.Join(lines, "\n"))
}

func (e *Engine) bases() []string {
	var out []string
	for _, s := range e.Config.Symbols {
		out = append(out, e.Config.Base(s))
	}
	return out
}

const (
	kindScheduled = "scheduled"
	kindMove      = "move"
	kindNews      = "news"
	kindManual    = "manual"
)

const newsTrigger = "headline "

// trigger says what kind of decision is due now and why, or "" if none is.
// Nothing is due while a decision runs, trading is stopped, a sell-off runs
// or an order is unresolved.
func (e *Engine) trigger(now time.Time) (string, string) {
	c := e.Config
	if e.job != nil || e.st.Halted != "" || e.st.Liquidate || len(e.st.Pending) > 0 || now.Before(e.st.RetryAt) || (c.Mode == config.Live && !e.keyOK) {
		return "", ""
	}
	var fresh []news.Item
	for _, it := range e.st.NewsQueue {
		if now.Sub(it.PublishedAt) < 2*time.Hour {
			fresh = append(fresh, it)
		}
	}
	e.st.NewsQueue = fresh
	last := e.st.Last()
	if last == nil || now.Sub(last.Time) >= c.DecisionInterval() {
		return kindScheduled, "scheduled check"
	}
	if now.Sub(last.Time) < time.Duration(c.EventCooldownMinutes)*time.Minute || e.eventsToday(now) >= c.MaxEventDecisionsPerDay {
		return "", ""
	}
	for _, s := range c.Symbols {
		if m := e.moves[s]; c.MoveTrigger1HPct > 0 && math.Abs(m)*100 >= c.MoveTrigger1HPct {
			return kindMove, fmt.Sprintf("%s moved %+.1f%% in the last hour", c.Base(s), m*100)
		}
	}
	for _, s := range c.Symbols {
		if p := last.Prices[s]; c.MoveTriggerSinceDecisionPct > 0 && p.Sign() > 0 {
			if m := e.live[s].Float()/p.Float() - 1; math.Abs(m)*100 >= c.MoveTriggerSinceDecisionPct {
				return kindMove, fmt.Sprintf("%s moved %+.1f%% since the last decision", c.Base(s), m*100)
			}
		}
	}
	if len(e.st.NewsQueue) > 0 {
		// Only the ID: the headline is third-party text and reaches the model
		// in the briefing's headlines, which are marked as untrusted.
		return kindNews, newsTrigger + e.st.NewsQueue[0].ID
	}
	return "", ""
}

// eventsToday counts today's decisions triggered by moves or news.
func (e *Engine) eventsToday(now time.Time) int {
	n := 0
	for _, d := range e.st.Decisions {
		if d.Time.Format(time.DateOnly) == now.Format(time.DateOnly) && (d.Kind == kindMove || d.Kind == kindNews) {
			n++
		}
	}
	return n
}
