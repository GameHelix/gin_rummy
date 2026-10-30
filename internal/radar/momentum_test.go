package radar

import (
	"context"
	"strings"
	"testing"
	"time"
)

type fakeBooks struct{ bids, asks float64 }

func (f fakeBooks) Book(ctx context.Context, coin string) (float64, float64, error) {
	return f.bids, f.asks, nil
}

// run feeds a track one sample every 30 s for n quarter hours, each
// candle closing higher by step, with the day's volume growing by vol per
// quarter hour (a little more each time) and open interest by oiStep.
func run(tr *track, start time.Time, quarters int, step, vol, oiStep float64) time.Time {
	px, dayVol, oi, at := 100.0, 5e8, 1000.0, start
	for q := 0; q < quarters; q++ {
		for i := 0; i < 30; i++ {
			tr.add(sample{at: at, mid: px, oi: oi, dayVol: dayVol, prevDay: 98, funding: 0.0001})
			at = at.Add(30 * time.Second)
		}
		px *= 1 + step
		dayVol += vol * (1 + 0.2*float64(q))
		oi *= 1 + oiStep
	}
	return at
}

func TestFiveGreenCandlesAreScoredAndCarded(t *testing.T) {
	w := &Watcher{Books: fakeBooks{bids: 650, asks: 350}, coins: map[string]*track{}}
	start := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	btc := &track{}
	run(btc, start, 100, 0.001, 1e6, 0)
	w.coins["BTC"] = btc
	tr := &track{}
	now := run(tr, start, 94, 0, 5e8/96, 0) // a flat day, usual volume
	now = run(tr, now, 6, 0.006, 2.5*5e8/96, 0.002)
	w.coins["XYZ"] = tr
	a, ok := w.momentum(now, "XYZ", tr)
	if !ok || a.Rule != RuleMomentum || a.Momentum == nil {
		t.Fatalf("ok=%v %+v", ok, a)
	}
	m := a.Momentum
	if m.Score < 80 || m.Class != "STRONG LONG" || m.Inflow != "strong" || m.BookBids < 0.64 || m.BTC != "bullish" || m.Move != "accumulation" || a.Suggestion.Side != "long" {
		t.Fatalf("score %d class %s inflow %s book %.2f btc %s move %s suggestion %+v", m.Score, m.Class, m.Inflow, m.BookBids, m.BTC, m.Move, a.Suggestion)
	}
	card := a.String()
	for _, want := range []string{"🚨 MOMENTUM RUN — XYZ", "🟢🟢🟢🟢🟢", "Score ", "STRONG LONG", "📊 Book 65% bids", "₿ BTC bullish", "🧭 Move: accumulation", "📚 1,013,365 five-green runs", "🗣 ", "Fresh money entering: yes", "💡 LONG · stop 5%"} {
		if !strings.Contains(card, want) {
			t.Fatalf("card lacks %q:\n%s", want, card)
		}
	}
	if _, again := w.momentum(now.Add(15*time.Minute), "XYZ", tr); again {
		t.Fatal("alerted again inside the cooldown")
	}
}

func TestLongsPilingInLowerTheScore(t *testing.T) {
	w := &Watcher{coins: map[string]*track{}}
	start := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	tr := &track{}
	now := run(tr, start, 94, 0, 5e8/96, 0)
	now = run(tr, now, 6, 0.006, 1.2*5e8/96, 0.03) // open interest +15% over the run, volume barely above usual
	w.coins["XYZ"] = tr
	a, ok := w.momentum(now, "XYZ", tr)
	if ok && a.Momentum.Class == "STRONG LONG" {
		t.Fatalf("crowded run rated strong: %+v", a.Momentum)
	}
	if ok && (a.Momentum.Move != "new leveraged longs" || !strings.Contains(a.Momentum.Verdict, "piling in")) {
		t.Fatalf("%+v", a.Momentum)
	}
}
