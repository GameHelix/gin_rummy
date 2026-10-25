package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ismatsamadov/trader/internal/analyst"
	"github.com/ismatsamadov/trader/internal/news"
	"github.com/ismatsamadov/trader/internal/num"
)

// tick is one pass of the loop: settle orders, refresh prices, record
// equity, enforce loss limits, keep selling if a sell-off is under way,
// grade old calls, read news, and decide if something is due.
func (e *Engine) tick(ctx context.Context) {
	now := e.now()
	if len(e.st.Pending) > 0 {
		e.recover(ctx)
	}
	if err := e.watch(ctx); err != nil {
		e.alert(ctx, "prices", "Price data unavailable: "+err.Error())
		return
	}
	equity, err := e.st.Ledger.Equity(e.prices)
	if err != nil {
		e.alert(ctx, "equity", err.Error())
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
	if trigger := e.trigger(now); trigger != "" {
		if err := e.decide(ctx, trigger); err != nil {
			// A spent budget only resets with the UTC day; other failures retry soon.
			e.retryAt = now.Add(10 * time.Minute)
			if errors.Is(err, analyst.ErrBudgetExceeded) {
				e.retryAt = now.Truncate(24 * time.Hour).Add(24 * time.Hour)
			}
			e.alert(ctx, "decide", "Decision skipped: "+untrusted(err.Error()))
		}
	}
}

// watch reads the last hour of one-minute candles for each symbol.
func (e *Engine) watch(ctx context.Context) error {
	prices, moves := map[string]num.Dec{}, map[string]float64{}
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
		prices[s], moves[s] = p, k[len(k)-1].C/k[0].O-1
	}
	e.prices, e.moves = prices, moves
	return nil
}

func (e *Engine) record(now time.Time, equity num.Dec) {
	hour := now.Truncate(time.Hour)
	if n := len(e.st.History); n > 0 && !e.st.History[n-1].Time.Before(hour) {
		return
	}
	bench, _ := e.st.Benchmark.Equity(e.prices)
	e.st.History = keep(append(e.st.History, Point{hour, equity, bench}), 24*90)
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
		e.notify(ctx, "New day: trading resumed.")
	}
}

// limits stops trading after a bad day, and starts a sell-off after a deep
// drawdown from the start (or from the last /resume).
func (e *Engine) limits(ctx context.Context, equity num.Dec) {
	c := e.Config
	floor := func(base num.Dec, pct float64) num.Dec {
		f, _ := num.FromFloat(1 - pct/100)
		return base.Mul(f)
	}
	if c.MaxDrawdownPct > 0 && !e.st.DrawdownHit && !floor(e.st.RiskBase, c.MaxDrawdownPct).Less(equity) {
		e.st.DrawdownHit, e.st.Liquidate = true, true
		e.halt(ctx, haltDrawdown, fmt.Sprintf("equity $%s is %.1f%% below $%s (limit %.0f%%), so everything is being sold; /resume to trade again", equity.Fixed(2), 100*(1-equity.Float()/e.st.RiskBase.Float()), e.st.RiskBase.Fixed(2), c.MaxDrawdownPct))
	}
	if c.DailyLossLimitPct > 0 && e.st.Halted == "" && !floor(e.st.DayEquity, c.DailyLossLimitPct).Less(equity) {
		e.halt(ctx, haltDaily, fmt.Sprintf("down %.1f%% today (limit %.0f%%); no new trades until 00:00 UTC", 100*(1-equity.Float()/e.st.DayEquity.Float()), c.DailyLossLimitPct))
	}
}

// guard re-checks the loss limits at current prices and reports whether new
// orders may go out.
func (e *Engine) guard(ctx context.Context) bool {
	if equity, err := e.st.Ledger.Equity(e.prices); err == nil {
		e.limits(ctx, equity)
	}
	return e.st.Halted == "" && !e.st.Liquidate
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
		e.st.Scored = keep(append(e.st.Scored, ScoredCall{OpenCall: oc, Exit: exit, Return: round(ret, 5), Hit: (score > 0 && ret > 0) || (score < 0 && ret < 0)}), 1000)
	}
	e.st.Open = open
}

// readNews refreshes headlines every five minutes. New, recent headlines
// about a traded coin that match an alert keyword are reported and queued
// until a decision runs. The first read after a fresh start only marks what
// is already out there as seen.
func (e *Engine) readNews(ctx context.Context, now time.Time) {
	if len(e.News.URLs) == 0 || now.Sub(e.newsAt) < 5*time.Minute {
		return
	}
	e.newsAt = now
	items, err := e.News.Fetch(ctx, now, 72*time.Hour, e.bases())
	if err != nil {
		e.alert(ctx, "news", "News feeds unavailable: "+err.Error())
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
	e.newsQueue = append(e.newsQueue, alerts...)
	var lines []string
	for _, it := range alerts[:min(3, len(alerts))] {
		lines = append(lines, fmt.Sprintf("News (%s): %s", it.Source, untrusted(it.Title)))
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

const newsTrigger = "headline "

// trigger says why a decision is due now, or "" if none is. Nothing is due
// while trading is stopped, a sell-off runs or an order is unresolved.
func (e *Engine) trigger(now time.Time) string {
	c := e.Config
	if e.st.Halted != "" || e.st.Liquidate || len(e.st.Pending) > 0 || now.Before(e.retryAt) {
		return ""
	}
	var fresh []news.Item
	for _, it := range e.newsQueue {
		if now.Sub(it.PublishedAt) < 2*time.Hour {
			fresh = append(fresh, it)
		}
	}
	e.newsQueue = fresh
	last := e.st.Last()
	if last == nil || now.Sub(last.Time) >= c.DecisionInterval() {
		return "scheduled"
	}
	if now.Sub(last.Time) < time.Duration(c.EventCooldownMinutes)*time.Minute || e.eventsToday(now) >= c.MaxEventDecisionsPerDay {
		return ""
	}
	for _, s := range c.Symbols {
		if m := e.moves[s]; c.MoveTrigger1HPct > 0 && math.Abs(m)*100 >= c.MoveTrigger1HPct {
			return fmt.Sprintf("%s moved %+.1f%% in the last hour", s, m*100)
		}
	}
	for _, s := range c.Symbols {
		if p := last.Prices[s]; c.MoveTriggerSinceDecisionPct > 0 && p.Sign() > 0 {
			if m := e.prices[s].Float()/p.Float() - 1; math.Abs(m)*100 >= c.MoveTriggerSinceDecisionPct {
				return fmt.Sprintf("%s moved %+.1f%% since the last decision", s, m*100)
			}
		}
	}
	if len(e.newsQueue) > 0 {
		// Only the ID: the headline is third-party text and reaches the model
		// in the briefing's headlines, which are marked as untrusted.
		return newsTrigger + e.newsQueue[0].ID
	}
	return ""
}

// eventsToday counts today's decisions triggered by moves or news.
func (e *Engine) eventsToday(now time.Time) int {
	n := 0
	for _, d := range e.st.Decisions {
		if d.Time.Format(time.DateOnly) == now.Format(time.DateOnly) && (strings.Contains(d.Trigger, " moved ") || strings.HasPrefix(d.Trigger, newsTrigger)) {
			n++
		}
	}
	return n
}
