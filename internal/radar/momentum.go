package radar

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"
)

// Momentum is the five-green-candle setup read the way a desk reads it:
// volume, the book, open interest, funding, BTC, structure and extension,
// scored 0–100. The record of such runs on every perpetual since 2021 is
// flat (a median of −0.4% a day later even with every confirmation), so
// the card says so and the score ranks runs by their look, not by any
// proven edge. Open interest is scored by what the data shows: longs
// piling in during a rise is the worst case, not the best.
type Momentum struct {
	Run          float64 `json:"run"` // the five candles' rise
	Score        int     `json:"score"`
	Class        string  `json:"class"`
	Inflow       string  `json:"inflow"`
	VolumeRate   float64 `json:"volume_rate"` // the five candles' volume against the usual
	VolumeRising bool    `json:"volume_rising"`
	BookBids     float64 `json:"book_bids"` // share of the top of the book on the bid side; 0 when unknown
	OIChange     float64 `json:"oi_change"` // over the run
	Funding      float64 `json:"funding"`
	BTC1h        float64 `json:"btc_1h"`
	BTC4h        float64 `json:"btc_4h"`
	BTC          string  `json:"btc"`
	Resistance   float64 `json:"resistance"`
	Support      float64 `json:"support"`
	Extended     float64 `json:"extended"` // above the day's average price
	RSI          float64 `json:"rsi"`
	Move         string  `json:"move"`
	Sustain      string  `json:"sustainability"`
	Pullback     string  `json:"pullback_risk"`
	FreshMoney   string  `json:"fresh_money"`
	Verdict      string  `json:"verdict"`
}

// Books reads the top of a coin's order book: the notional resting on
// each side. The exchange client is one.
type Books interface {
	Book(ctx context.Context, coin string) (bids, asks float64, err error)
}

const (
	RuleMomentum     = "momentum run"
	momentumCooldown = 2 * time.Hour
	// The record of five-green runs on every perpetual, 2021–2026, from
	// research/results/five_green_confirmed.md.
	momentumRuns, momentumMedian24h, momentumUp24h    = "1,013,365", -0.003, 0.47
	confirmedRuns, confirmedMedian24h, confirmedUp24h = "7,021", -0.004, 0.46
)

// closeAt is the price a 15-minute candle closed at: the newest sample
// strictly before its boundary, since a sample on the boundary opens the
// next candle.
func (t *track) closeAt(boundary time.Time) (sample, bool) {
	return t.back(boundary.Add(-time.Nanosecond), 0)
}

// momentum looks for five completed green 15-minute candles once per
// candle close and scores the run.
func (w *Watcher) momentum(now time.Time, coin string, tr *track) (Alert, bool) {
	boundary := now.Truncate(15 * time.Minute)
	if !boundary.After(tr.lastCandle) || now.Sub(tr.momentumAt) < momentumCooldown {
		return Alert{}, false
	}
	var s [6]sample
	for i := range s {
		var ok bool
		if s[i], ok = tr.closeAt(boundary.Add(-time.Duration(5-i) * 15 * time.Minute)); !ok {
			return Alert{}, false
		}
	}
	tr.lastCandle = boundary
	for i := 1; i < 6; i++ {
		if s[i].mid <= s[i-1].mid {
			return Alert{}, false
		}
	}
	cur := tr.samples[len(tr.samples)-1]
	m := Momentum{Run: s[5].mid/s[0].mid - 1, Funding: cur.funding}
	// Volume: the candles' volume from the day's rolling sum.
	usual := cur.dayVol / 96
	var vols [5]float64
	for i := 1; i < 6; i++ {
		vols[i-1] = math.Max(0, s[i].dayVol-s[i-1].dayVol)
	}
	if usual > 0 {
		m.VolumeRate = (vols[0] + vols[1] + vols[2] + vols[3] + vols[4]) / (5 * usual)
	}
	m.VolumeRising = vols[4] > vols[0]
	if s[0].oi > 0 && s[5].oi > 0 {
		m.OIChange = s[5].oi/s[0].oi - 1
	}
	if btc := w.coins["BTC"]; btc != nil && len(btc.samples) > 0 {
		b := btc.samples[len(btc.samples)-1]
		if h, ok := btc.back(now, time.Hour); ok {
			m.BTC1h = b.mid/h.mid - 1
		}
		if h, ok := btc.back(now, 4*time.Hour); ok {
			m.BTC4h = b.mid/h.mid - 1
		}
	}
	var sum float64
	m.Resistance, m.Support = cur.mid, cur.mid
	for _, x := range tr.samples {
		sum += x.mid
		m.Resistance, m.Support = math.Max(m.Resistance, x.mid), math.Min(m.Support, x.mid)
	}
	m.Extended = cur.mid/(sum/float64(len(tr.samples))) - 1
	m.RSI = tr.rsi(boundary, 14)
	if w.Books != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if bids, asks, err := w.Books.Book(ctx, coin); err == nil && bids+asks > 0 {
			m.BookBids = bids / (bids + asks)
		}
		cancel()
	}
	score := 15 // five green candles
	switch {
	case m.VolumeRate >= 2 && m.VolumeRising:
		score, m.Inflow = score+20, "strong"
	case m.VolumeRate >= 1.5:
		score, m.Inflow = score+12, "moderate"
	case m.VolumeRate >= 1:
		score, m.Inflow = score+6, "weak"
	default:
		m.Inflow = "weak"
	}
	switch {
	case m.BookBids == 0:
		score += 5 // unknown: neutral
	case m.BookBids >= 0.60:
		score += 15
	case m.BookBids >= 0.55:
		score += 10
	case m.BookBids >= 0.50:
		score += 5
	}
	switch {
	case m.OIChange >= -0.02 && m.OIChange <= 0.03:
		score += 15 // positions steady: the move is spot-like
	case m.OIChange > 0.03 && m.OIChange <= 0.08:
		score += 8
	case m.OIChange < -0.02:
		score += 5 // short covering
	}
	high := s[5].mid
	for _, x := range s {
		high = math.Max(high, x.mid)
	}
	switch {
	case m.Run >= 0.02 && cur.mid >= high*0.99:
		score += 10
	case m.Run >= 0.01:
		score += 6
	default:
		score += 3
	}
	switch {
	case m.RSI < 70:
		score += 10
	case m.RSI < 80:
		score += 5
	}
	switch {
	case m.BTC1h >= 0 && m.BTC4h >= 0:
		score, m.BTC = score+10, "bullish"
	case m.BTC1h >= 0 || m.BTC4h >= 0:
		score, m.BTC = score+5, "neutral"
	default:
		m.BTC = "bearish"
	}
	switch {
	case cur.dayVol >= 20e6:
		score += 5
	case cur.dayVol >= 5e6:
		score += 3
	}
	m.Score = score
	switch {
	case score >= 80:
		m.Class = "STRONG LONG"
	case score >= 70:
		m.Class = "WATCHLIST"
	default:
		m.Class = "NO TRADE"
	}
	switch {
	case cur.dayVol < 5e6:
		m.Move = "low-liquidity spike"
	case m.OIChange > 0.03:
		m.Move = "new leveraged longs"
	case m.OIChange < -0.02:
		m.Move = "short covering"
	case m.VolumeRate >= 1.5:
		m.Move = "accumulation"
	default:
		m.Move = "mixed"
	}
	m.Sustain = map[bool]string{true: "high", false: "medium"}[score >= 85]
	if score < 75 {
		m.Sustain = "low"
	}
	switch {
	case m.Extended >= 0.05:
		m.Pullback = "high"
	case m.Extended >= 0.02:
		m.Pullback = "medium"
	default:
		m.Pullback = "low"
	}
	switch {
	case m.Inflow == "strong" && m.OIChange >= -0.02 && m.OIChange <= 0.08:
		m.FreshMoney = "yes, and accelerating"
		if !m.VolumeRising {
			m.FreshMoney = "yes, but not accelerating"
		}
	case m.Inflow == "moderate":
		m.FreshMoney = "partly"
	default:
		m.FreshMoney = "no"
	}
	m.Verdict = verdict(m)
	if m.Class == "NO TRADE" {
		return Alert{}, false
	}
	tr.momentumAt, tr.lastAlert = now, now
	a := Alert{At: now, Coin: coin, Rule: RuleMomentum, Price: cur.mid, Volume24hUSD: cur.dayVol, Funding: cur.funding, VolumeRate: m.VolumeRate, OIChange1h: m.OIChange, Score: float64(score), Momentum: &m}
	a.Change15m = s[5].mid/s[4].mid - 1
	if h, ok := tr.back(now, time.Hour); ok {
		a.Change1h = cur.mid/h.mid - 1
	}
	if cur.prevDay > 0 {
		a.Change24h = cur.mid/cur.prevDay - 1
	}
	a.History = fmt.Sprintf("%s five-green runs since 2021 → 24h %s (%.0f%% up); with every confirmation %s runs → %s (%.0f%% up)", momentumRuns, pct(momentumMedian24h), momentumUp24h*100, confirmedRuns, pct(confirmedMedian24h), confirmedUp24h*100)
	if m.Class == "STRONG LONG" {
		a.Suggestion = Suggestion{Side: "long", StopLossPct: 0.05, Reason: "the run reads well; the record of such runs is flat, so keep it small"}
	}
	return a, true
}

// verdict words the read in a few sentences.
func verdict(m Momentum) string {
	var s []string
	switch m.Inflow {
	case "strong":
		s = append(s, fmt.Sprintf("Volume ran %.1f× the usual over the five candles%s.", m.VolumeRate, map[bool]string{true: " and kept rising", false: " but faded by the last one"}[m.VolumeRising]))
	case "moderate":
		s = append(s, fmt.Sprintf("Volume was %.1f× the usual, a moderate inflow.", m.VolumeRate))
	default:
		s = append(s, "Volume stayed near the usual, so the rise rests on little new money.")
	}
	switch {
	case m.OIChange > 0.08:
		s = append(s, fmt.Sprintf("Open interest jumped %s over the run: longs piling in, which the record says ends badly.", pct(m.OIChange)))
	case m.OIChange > 0.03:
		s = append(s, fmt.Sprintf("Open interest rose %s: some of the move is new leverage.", pct(m.OIChange)))
	case m.OIChange < -0.02:
		s = append(s, fmt.Sprintf("Open interest fell %s: part of the rise is shorts covering, not new buyers.", pct(m.OIChange)))
	default:
		s = append(s, "Open interest barely moved, so the move looks spot-driven rather than leveraged.")
	}
	if m.BookBids > 0 {
		s = append(s, fmt.Sprintf("The top of the book is %.0f%% bids.", m.BookBids*100))
	}
	s = append(s, fmt.Sprintf("BTC is %s (1h %s, 4h %s)%s.", m.BTC, pct(m.BTC1h), pct(m.BTC4h), map[bool]string{true: fmt.Sprintf(" and the price sits %s above its day average", pct(m.Extended)), false: ""}[m.Extended >= 0.02]))
	s = append(s, "Fresh money entering: "+m.FreshMoney+".")
	return strings.Join(s, " ")
}

// rsi is Wilder's RSI on the 15-minute closes before a boundary; 50 when
// there is not enough history.
func (t *track) rsi(boundary time.Time, n int) float64 {
	var closes []float64
	for i := n + 1; i >= 0; i-- {
		s, ok := t.closeAt(boundary.Add(-time.Duration(i) * 15 * time.Minute))
		if !ok {
			return 50
		}
		closes = append(closes, s.mid)
	}
	var gain, loss float64
	for i := 1; i < len(closes); i++ {
		d := closes[i] - closes[i-1]
		gain, loss = gain+math.Max(d, 0)/float64(n), loss+math.Max(-d, 0)/float64(n)
	}
	if loss == 0 {
		return 100
	}
	return 100 - 100/(1+gain/loss)
}

// card is the momentum alert as the chat shows it.
func (a Alert) momentumCard() string {
	m := a.Momentum
	na := func(v float64, f string) string {
		if v == 0 {
			return "n/a"
		}
		return fmt.Sprintf(f, v)
	}
	lines := []string{
		fmt.Sprintf("🚨 MOMENTUM RUN — %s  %s", a.Coin, money(a.Price)),
		fmt.Sprintf("🟢🟢🟢🟢🟢 five green 15-minute candles, %s", pct(m.Run)),
		fmt.Sprintf("🎯 Score %d/100 — %s", m.Score, m.Class),
		fmt.Sprintf("💰 Inflow %s · volume %.1f× usual%s", m.Inflow, m.VolumeRate, map[bool]string{true: ", rising", false: ""}[m.VolumeRising]),
		fmt.Sprintf("📊 Book %s bids · OI %s · funding %+.3f%%/h · liquidations n/a", na(m.BookBids*100, "%.0f%%"), pct(m.OIChange), m.Funding*100),
		fmt.Sprintf("₿ BTC %s · 1h %s · 4h %s", m.BTC, pct(m.BTC1h), pct(m.BTC4h)),
		fmt.Sprintf("🏗 Resistance %s · support %s · RSI %.0f · %s above day avg", money(m.Resistance), money(m.Support), m.RSI, pct(m.Extended)),
		fmt.Sprintf("🧭 Move: %s · sustainability %s · pullback risk %s", m.Move, m.Sustain, m.Pullback),
		"📚 " + a.History,
		"🗣 " + m.Verdict,
	}
	if t := a.Suggestion.String(); t != "" {
		lines = append(lines, t)
	}
	return strings.Join(lines, "\n")
}
