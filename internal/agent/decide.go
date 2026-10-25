package agent

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ismatsamadov/trader/internal/analyst"
	"github.com/ismatsamadov/trader/internal/market"
	"github.com/ismatsamadov/trader/internal/news"
	"github.com/ismatsamadov/trader/internal/num"
	"github.com/ismatsamadov/trader/internal/store"
)

// decide asks the analyst for ratings and trades on them. It never runs
// while an earlier order is unresolved, saves the decision before any order
// is sent, and re-checks the loss limits at fresh prices before trading.
func (e *Engine) decide(ctx context.Context, trigger string) error {
	if len(e.st.Pending) > 0 {
		e.recover(ctx)
		if len(e.st.Pending) > 0 {
			return errors.New("an earlier order is still being checked; deciding once it settles")
		}
	}
	c := e.Config
	e.deciding = true
	defer func() { e.deciding = false }()
	now := e.now()
	id := e.st.NextID
	dir := filepath.Join(c.Dir(), "decisions", strconv.Itoa(id))
	in, err := e.snapshot(ctx, id, trigger, now, dir)
	if err != nil {
		return err
	}
	e.st.NextID++
	e.newsQueue = nil
	if err := e.save(); err != nil {
		return err
	}
	actx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	res, err := e.Analyst.Analyze(actx, in)
	cancel()
	if err != nil {
		return err
	}
	d := Decision{ID: id, Time: now, Trigger: trigger, Summary: res.Summary, Signals: res.Signals, Prices: map[string]num.Dec{}, CostUSD: res.CostUSD, Abstained: res.Abstained}
	for _, v := range in.Briefing.Coins {
		d.Prices[v.Symbol], _ = num.FromFloat(v.Price)
	}
	e.st.Decisions = keep(append(e.st.Decisions, d), 100)
	if !res.Abstained {
		horizon := time.Duration(c.ScoreHorizonHours) * time.Hour
		for _, s := range c.Symbols {
			if score, _ := res.Signals[s].Rating.Score(); score != 0 {
				e.st.Open = append(e.st.Open, OpenCall{Decision: id, Symbol: s, Rating: res.Signals[s].Rating, Entry: d.Prices[s], Due: now.Add(horizon)})
			}
		}
	}
	if err := e.save(); err != nil {
		return err
	}
	var trades []Trade
	note := ""
	switch {
	case res.Abstained:
	case e.watch(ctx) != nil:
		note = "No trades: prices were unavailable after the analysis."
	case !e.guard(ctx):
		note = "No trades: trading is stopped (" + e.st.Halted + ")."
	default:
		trades = e.rebalance(ctx, id, res.Signals)
	}
	e.notify(ctx, e.decisionText(d, trades, note))
	if err := store.WriteJSON(filepath.Join(dir, "decision.json"), map[string]any{"decision": d, "trades": trades, "note": note}); err != nil {
		return err
	}
	return e.save()
}

func (e *Engine) snapshot(ctx context.Context, id int, trigger string, now time.Time, dir string) (analyst.Input, error) {
	c := e.Config
	candles := map[string]map[string][]market.Candle{}
	for _, s := range c.Symbols {
		candles[s] = map[string][]market.Candle{}
		for interval, n := range map[string]int{"1h": 200, "4h": 200, "1d": 365} {
			k, err := e.Exchange.Klines(ctx, s, interval, n, time.Time{})
			if err != nil {
				return analyst.Input{}, err
			}
			candles[s][interval] = k
		}
	}
	if err := e.watch(ctx); err != nil {
		return analyst.Input{}, err
	}
	equity, err := e.st.Ledger.Equity(e.prices)
	if err != nil {
		return analyst.Input{}, err
	}
	if equity.Sign() <= 0 {
		return analyst.Input{}, errors.New("no equity left")
	}
	bench, _ := e.st.Benchmark.Equity(e.prices)
	eq, capital := equity.Float(), e.st.Capital.Float()
	right := 0
	for _, sc := range e.st.Scored {
		if sc.Hit {
			right++
		}
	}
	b := analyst.Briefing{
		DecisionID: "d" + strconv.Itoa(id), DecisionTime: now.Format(time.RFC3339), Trigger: trigger, HorizonHours: c.ScoreHorizonHours,
		Account: analyst.Account{EquityUSD: round(eq, 2), CashWeight: round(e.st.Ledger.Cash.Float()/eq, 4), ReturnSinceStart: round(eq/capital-1, 4), BuyAndHoldReturn: round(bench.Float()/capital-1, 4),
			CallsGraded: len(e.st.Scored), CallsRight: right},
		RecentCalls: e.recentCalls(5),
		Headlines:   briefHeadlines(e.headlines, c.NewsItems, strings.TrimPrefix(trigger, newsTrigger)),
	}
	for _, s := range c.Symbols {
		v, err := market.NewView(s, analyst.PriceEvidenceID(s), e.prices[s].Float(), market.Closed(candles[s]["1h"], now), market.Closed(candles[s]["1d"], now))
		if err != nil {
			return analyst.Input{}, err
		}
		v.Weight = round(e.st.Ledger.Holdings[s].Mul(e.prices[s]).Float()/eq, 4)
		b.Coins = append(b.Coins, v)
	}
	if dir != "" {
		if err := store.WriteJSON(filepath.Join(dir, "briefing.json"), b); err != nil {
			return analyst.Input{}, err
		}
	}
	return analyst.Input{Briefing: b, Candles: candles, News: e.headlines, Dir: dir}, nil
}

// briefHeadlines puts the headline that triggered the decision first, then
// prefers stories about the traded coins and fills the rest with general
// market news, newest first.
func briefHeadlines(items []news.Item, n int, pinned string) []news.Item {
	var out, coin, other []news.Item
	for _, it := range items {
		switch {
		case it.ID == pinned && n > 0:
			out = append(out, it)
		case len(it.Coins) > 0:
			coin = append(coin, it)
		default:
			other = append(other, it)
		}
	}
	out = append(out, coin[:min(n-len(out), len(coin))]...)
	out = append(out, other[:min(n-len(out), len(other))]...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].PublishedAt.After(out[j].PublishedAt) })
	return out
}

func (e *Engine) recentCalls(n int) []analyst.Call {
	var out []analyst.Call
	for _, d := range e.st.Decisions[max(0, len(e.st.Decisions)-n):] {
		call := analyst.Call{Time: d.Time.Format(time.RFC3339), Ratings: map[string]analyst.Rating{}, ChangesSince: map[string]float64{}}
		for s, sig := range d.Signals {
			call.Ratings[s] = sig.Rating
		}
		for s, p := range d.Prices {
			if p.Sign() > 0 && e.prices[s].Sign() > 0 {
				call.ChangesSince[s] = round(e.prices[s].Float()/p.Float()-1, 4)
			}
		}
		out = append(out, call)
	}
	return out
}
