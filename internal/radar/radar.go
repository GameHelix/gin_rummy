// Package radar watches every perpetual on the exchange and raises the
// moves a trader would want to know about: a price move unusual for that
// coin, on volume, or a swing in open interest.
package radar

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/candylario/trader/internal/hyperliquid"
)

// Source gives the state of every market. The exchange client is one.
type Source interface {
	Markets(ctx context.Context) ([]hyperliquid.Asset, []hyperliquid.Market, error)
}

// Alert is one unusual move. Changes are fractions: 0.05 is +5%.
type Alert struct {
	At           time.Time  `json:"at"`
	Coin         string     `json:"coin"`
	Rule         string     `json:"rule"` // the trigger that raised it
	Price        float64    `json:"price"`
	Change15m    float64    `json:"change_15m"`
	Change1h     float64    `json:"change_1h"`
	Change24h    float64    `json:"change_24h"`
	Sigma15m     float64    `json:"sigma_15m"`   // the 15m move in units of the coin's own 15m volatility
	VolumeRate   float64    `json:"volume_rate"` // the last 15 minutes' volume against the day's 15-minute average
	Volume24hUSD float64    `json:"volume_24h_usd"`
	OIChange1h   float64    `json:"oi_change_1h"`
	Funding      float64    `json:"funding"` // the hourly rate
	Score        float64    `json:"score"`
	Reasons      []string   `json:"reasons"`
	History      string     `json:"history"` // what followed moves like this on perps since 2020
	Suggestion   Suggestion `json:"suggestion"`
}

// String is the alert as a chat shows it.
func (a Alert) String() string {
	arrow := "🚀"
	if a.Change15m < 0 {
		arrow = "🔻"
	}
	head := arrow + " " + a.Coin
	if a.Rule != "" {
		head = arrow + " " + a.Rule + " · " + a.Coin
	}
	s := fmt.Sprintf("%s %s in 15m, %s in 1h, %s today · %s · %s", head, pct(a.Change15m), pct(a.Change1h), pct(a.Change24h), strings.Join(a.Reasons, ", "), money(a.Price))
	if a.History != "" {
		s += "\n" + a.History
	}
	if t := a.Suggestion.String(); t != "" {
		s += "\n" + t
	}
	return s
}

func pct(x float64) string { return fmt.Sprintf("%+.1f%%", x*100) }

func money(x float64) string {
	switch {
	case x >= 1000:
		return fmt.Sprintf("$%.0f", x)
	case x >= 1:
		return fmt.Sprintf("$%.2f", x)
	default:
		return fmt.Sprintf("$%.4g", x)
	}
}

func usd(x float64) string {
	switch {
	case x >= 1e9:
		return fmt.Sprintf("$%.1fB", x/1e9)
	case x >= 1e6:
		return fmt.Sprintf("$%.0fM", x/1e6)
	default:
		return fmt.Sprintf("$%.0fk", x/1e3)
	}
}

// Watcher samples the markets and raises alerts. Zero values take the
// defaults: a sample every 30 s, one alert per coin per hour, coins with
// at least $1M traded in the day.
type Watcher struct {
	Source       Source
	Interval     time.Duration
	Cooldown     time.Duration
	MinVolumeUSD float64
	Alert        func(Alert)
	Logf         func(format string, args ...any)
	Now          func() time.Time

	coins map[string]*track
}

type sample struct {
	at      time.Time
	mid     float64
	oi      float64
	dayVol  float64
	prevDay float64
	funding float64
}

type track struct {
	samples        []sample // oldest first, at most a day
	lastAlert      time.Time
	fundingAlertAt time.Time
	seeded         bool // from candles, until the first live sample
}

// History gives past candles, to warm the watcher up at start.
type History interface {
	Candles(ctx context.Context, coin, interval string, start, end time.Time) ([]hyperliquid.Candle, error)
}

// pace keeps the warm-up well inside the exchange's request weight.
const pace = 1400 * time.Millisecond

// Warm loads the past day of 15-minute candles for every coin that trades
// enough, so alerts can come at once instead of an hour in. It takes a
// few minutes, paced to leave room for the sampling.
func (w *Watcher) Warm(ctx context.Context, h History) error {
	assets, markets, err := w.Source.Markets(ctx)
	if err != nil {
		return err
	}
	minVol := w.MinVolumeUSD
	if minVol <= 0 {
		minVol = defaultMinVol
	}
	var coins []string
	for i, a := range assets {
		if i < len(markets) && !a.Delisted && markets[i].DayVolumeUSD >= minVol {
			coins = append(coins, a.Name)
		}
	}
	w.logf("radar: warming up %d coins, about %.0f minutes", len(coins), (time.Duration(len(coins)) * pace).Minutes())
	for _, coin := range coins {
		now := w.now()
		candles, err := h.Candles(ctx, coin, "15m", now.Add(-history), now)
		if err != nil {
			w.logf("%s candles: %v", coin, err)
		} else {
			w.seed(coin, now, candles)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pace):
		}
	}
	return nil
}

// seed turns candles into samples: the close as the price and the day's
// volume as the last 96 candles' notional, which is what the exchange
// reports live. The candle still forming is left out.
func (w *Watcher) seed(coin string, now time.Time, candles []hyperliquid.Candle) {
	if w.coins == nil {
		w.coins = map[string]*track{}
	}
	tr := &track{seeded: true}
	var window []float64
	var sum float64
	for _, c := range candles {
		if c.Close.After(now) {
			break
		}
		notional := c.Volume * c.C
		window, sum = append(window, notional), sum+notional
		if len(window) > 96 {
			sum, window = sum-window[0], window[1:]
		}
		tr.add(sample{at: c.Close, mid: c.C, dayVol: sum})
	}
	if len(tr.samples) > 0 {
		w.coins[coin] = tr
	}
}

const (
	defaultInterval = 30 * time.Second
	defaultCooldown = time.Hour
	defaultMinVol   = 1_000_000
	history         = 24 * time.Hour
)

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Watcher) logf(format string, args ...any) {
	if w.Logf != nil {
		w.Logf(format, args...)
	}
}

// Run samples until ctx ends. A failed read is logged and the next tick
// tries again.
func (w *Watcher) Run(ctx context.Context) error {
	interval := w.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	// An hourly line tells a quiet market from a stuck loop.
	beat, samples, raised := w.now(), 0, 0
	for {
		assets, markets, err := w.Source.Markets(ctx)
		if err != nil {
			w.logf("markets: %v", err)
		} else {
			alerts := w.Observe(w.now(), assets, markets)
			samples, raised = samples+1, raised+len(alerts)
			for _, a := range alerts {
				if w.Alert != nil {
					w.Alert(a)
				}
			}
		}
		if now := w.now(); now.Sub(beat) >= time.Hour {
			w.logf("radar: %d samples of %d coins and %d alerts in the last hour", samples, len(w.coins), raised)
			beat, samples, raised = now, 0, 0
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Observe takes one sample of every market and returns the alerts it
// raises, strongest first.
func (w *Watcher) Observe(now time.Time, assets []hyperliquid.Asset, markets []hyperliquid.Market) []Alert {
	if w.coins == nil {
		w.coins = map[string]*track{}
	}
	cooldown, minVol := w.Cooldown, w.MinVolumeUSD
	if cooldown <= 0 {
		cooldown = defaultCooldown
	}
	if minVol <= 0 {
		minVol = defaultMinVol
	}
	var alerts []Alert
	for i, a := range assets {
		if i >= len(markets) || a.Delisted || markets[i].MidPx <= 0 {
			continue
		}
		m := markets[i]
		tr := w.coins[a.Name]
		if tr == nil {
			tr = &track{}
			w.coins[a.Name] = tr
		}
		tr.add(sample{at: now, mid: m.MidPx, oi: m.OpenInterest, dayVol: m.DayVolumeUSD, prevDay: m.PrevDayPx, funding: m.Funding})
		if m.DayVolumeUSD < minVol {
			continue
		}
		if now.Sub(tr.lastAlert) >= cooldown {
			if al, ok := tr.judge(now, a.Name); ok {
				tr.lastAlert = now
				alerts = append(alerts, al)
			}
		}
		if al, ok := tr.fundingFlip(now, a.Name); ok {
			alerts = append(alerts, al)
		}
	}
	sort.Slice(alerts, func(i, j int) bool { return alerts[i].Score > alerts[j].Score })
	return alerts
}

func (t *track) add(s sample) {
	// The first live sample lines the seeded day's volume up with the
	// exchange's own figure, so the next 15 minutes compare like with like.
	if t.seeded && s.oi > 0 && len(t.samples) > 0 {
		shift := s.dayVol - t.samples[len(t.samples)-1].dayVol
		for i := range t.samples {
			t.samples[i].dayVol += shift
		}
		t.seeded = false
	}
	t.samples = append(t.samples, s)
	cut := 0
	for cut < len(t.samples) && s.at.Sub(t.samples[cut].at) > history {
		cut++
	}
	t.samples = t.samples[cut:]
}

// back returns the newest sample at least d before now, if there is one.
func (t *track) back(now time.Time, d time.Duration) (sample, bool) {
	for i := len(t.samples) - 1; i >= 0; i-- {
		if now.Sub(t.samples[i].at) >= d {
			return t.samples[i], true
		}
	}
	return sample{}, false
}

// measure takes the latest sample's bearings: the moves, the volume, the
// open interest, and the last 15 minutes' highest and lowest prices.
func (t *track) measure(now time.Time, coin string) (Alert, float64, float64, bool) {
	cur := t.samples[len(t.samples)-1]
	s15, ok15 := t.back(now, 15*time.Minute)
	s1h, ok1h := t.back(now, time.Hour)
	if !ok15 || !ok1h {
		return Alert{}, 0, 0, false
	}
	a := Alert{At: now, Coin: coin, Price: cur.mid, Volume24hUSD: cur.dayVol, Funding: cur.funding}
	a.Change15m = cur.mid/s15.mid - 1
	a.Change1h = cur.mid/s1h.mid - 1
	if cur.prevDay > 0 {
		a.Change24h = cur.mid/cur.prevDay - 1
	}
	if s1h.oi > 0 {
		a.OIChange1h = cur.oi/s1h.oi - 1
	}
	// The day's volume is a rolling sum, so its change over 15 minutes is
	// the volume of those minutes less what fell out of the window.
	if avg := cur.dayVol / 96; avg > 0 {
		a.VolumeRate = math.Max(0, cur.dayVol-s15.dayVol) / avg
	}
	a.Sigma15m = a.Change15m / math.Max(t.sigma15(now), 0.002)
	high, low := s15.mid, s15.mid
	for i := len(t.samples) - 1; i >= 0 && t.samples[i].at.After(s15.at); i-- {
		high, low = math.Max(high, t.samples[i].mid), math.Min(low, t.samples[i].mid)
	}
	return a, high, low, true
}

// The rules, named as the alerts show them. Each was measured on every
// perpetual from 2021 to 2026 (see history_triggers.json).
const (
	RulePumpAndCrash  = "pump-and-crash"
	RuleUnpumpedCrash = "unpumped crash"
	RuleCrowdedRise   = "crowded rise"
	RuleSpikeRejected = "spike rejected"
	RuleBigMove       = "big move"
	RuleBigHour       = "big hour"
	RuleOISwing       = "open interest swing"
	RuleFundingFlip   = "funding flip"
)

// judge decides whether the latest sample is a move worth an alert, and
// names the rule that fires. A crash of 10% is judged by what came
// before it; a rise with open interest piling in is a crowded rise; a
// spike that gave back its gain within the quarter hour is a rejected
// spike; the rest are the moves large for the coin, large hours, and
// swings in open interest.
func (t *track) judge(now time.Time, coin string) (Alert, bool) {
	a, high, _, ok := t.measure(now, coin)
	if !ok {
		return Alert{}, false
	}
	open := a.Price / (1 + a.Change15m)
	wick := (high - math.Max(open, a.Price)) / open
	prior := (1+a.Change24h)/(1+a.Change15m) - 1
	big15 := math.Abs(a.Change15m) >= 0.03 && math.Abs(a.Sigma15m) >= 3 && a.VolumeRate >= 2
	big1h := math.Abs(a.Change1h) >= 0.06
	oiSwing := math.Abs(a.OIChange1h) >= 0.10 && math.Abs(a.Change1h) >= 0.02
	switch {
	case a.Change15m <= -0.10 && prior >= 0.2:
		a.Rule = RulePumpAndCrash
		a.Reasons = append(a.Reasons, "after "+pct(prior)+" the day before")
	case a.Change15m <= -0.10:
		a.Rule = RuleUnpumpedCrash
	case a.Change1h >= 0.02 && a.OIChange1h >= 0.10:
		a.Rule = RuleCrowdedRise
	case wick >= 0.05 && a.Price <= open:
		a.Rule = RuleSpikeRejected
		a.Reasons = append(a.Reasons, fmt.Sprintf("a spike of %s given back", pct(high/open-1)))
	case big15:
		a.Rule = RuleBigMove
	case big1h:
		a.Rule = RuleBigHour
	case oiSwing:
		a.Rule = RuleOISwing
	default:
		return Alert{}, false
	}
	if a.VolumeRate >= 2 {
		a.Reasons = append(a.Reasons, fmt.Sprintf("%.0f× the usual volume", a.VolumeRate))
	}
	if math.Abs(a.OIChange1h) >= 0.10 {
		a.Reasons = append(a.Reasons, "open interest "+pct(a.OIChange1h)+" in 1h")
	}
	if math.Abs(a.Funding) >= 0.0005 {
		a.Reasons = append(a.Reasons, fmt.Sprintf("funding %+.2f%%/h", a.Funding*100))
	}
	a.Reasons = append(a.Reasons, "24h volume "+usd(a.Volume24hUSD))
	a.Score = math.Abs(a.Sigma15m) + math.Abs(a.Change1h)/0.02 + math.Min(a.VolumeRate, 10)/2 + math.Abs(a.OIChange1h)*20 + wick*50
	a.History = historyOf(a)
	a.Suggestion = suggest(a)
	return a, true
}

// fundingFlip raises an alert when funding is very negative, once a day
// per coin, whatever the price is doing: such coins have kept falling.
func (t *track) fundingFlip(now time.Time, coin string) (Alert, bool) {
	cur := t.samples[len(t.samples)-1]
	if cur.funding > fundingVeryNegative || now.Sub(t.fundingAlertAt) < 24*time.Hour {
		return Alert{}, false
	}
	a, _, _, ok := t.measure(now, coin)
	if !ok {
		return Alert{}, false
	}
	t.fundingAlertAt = now
	a.Rule = RuleFundingFlip
	a.Reasons = append(a.Reasons, fmt.Sprintf("funding %+.3f%%/h, shorts paying", a.Funding*100), "24h volume "+usd(a.Volume24hUSD))
	a.Score = 1
	a.History = historyOf(a)
	a.Suggestion = suggest(a)
	return a, true
}

// sigma15 is the standard deviation of the coin's 15-minute returns over
// its history, from samples 15 minutes apart.
func (t *track) sigma15(now time.Time) float64 {
	var rets []float64
	next := t.samples[0].at
	var last float64
	for _, s := range t.samples {
		if s.at.Before(next) {
			continue
		}
		if last > 0 {
			rets = append(rets, s.mid/last-1)
		}
		last, next = s.mid, s.at.Add(15*time.Minute)
	}
	if len(rets) < 8 {
		return 0
	}
	var mean, ss float64
	for _, r := range rets {
		mean += r / float64(len(rets))
	}
	for _, r := range rets {
		ss += (r - mean) * (r - mean)
	}
	return math.Sqrt(ss / float64(len(rets)-1))
}
