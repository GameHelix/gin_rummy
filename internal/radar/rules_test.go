package radar

import (
	"strings"
	"testing"
	"time"
)

// series feeds a track one sample a minute from prices, with a steady
// day's volume, open interest and funding, and returns the last time.
func series(tr *track, start time.Time, prices []float64, prevDay, funding float64) time.Time {
	at := start
	for _, p := range prices {
		tr.add(sample{at: at, mid: p, oi: 1000, dayVol: 5e8, prevDay: prevDay, funding: funding})
		at = at.Add(time.Minute)
	}
	return at.Add(-time.Minute)
}

func flat(n int, p float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = p
	}
	return out
}

func TestACrashIsJudgedByWhatCameBefore(t *testing.T) {
	start := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	crash := append(flat(120, 100), 97, 94, 92, 90, 89, 88, 88, 87.5, 87, 87, 88, 88, 88, 88, 88)
	pumped := &track{}
	now := series(pumped, start, crash, 100/1.6, 0)
	a, ok := pumped.judge(now, "X")
	if !ok || a.Rule != RulePumpAndCrash || a.Suggestion.Side != "short" || a.Suggestion.StopLossPct != 0.10 || !strings.Contains(a.History, "such alerts") || !strings.Contains(a.String(), "🔻 pump-and-crash · X") {
		t.Fatalf("%+v", a)
	}
	beaten := &track{}
	now = series(beaten, start, crash, 100*1.3, 0)
	if a, ok := beaten.judge(now, "Y"); !ok || a.Rule != RuleUnpumpedCrash || a.Suggestion.Side != "long" || a.Suggestion.StopLossPct != 0.15 {
		t.Fatalf("%+v", a)
	}
}

func TestASpikeGivenBackIsRejected(t *testing.T) {
	start := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	spike := append(flat(120, 100), 102, 104, 106, 107, 106, 104, 102, 101, 100.5, 100, 99.8, 99.7, 99.6, 99.6, 99.5)
	tr := &track{}
	now := series(tr, start, spike, 100*0.98, 0)
	a, ok := tr.judge(now, "Z")
	if !ok || a.Rule != RuleSpikeRejected || a.Suggestion.Side != "short" || !strings.Contains(strings.Join(a.Reasons, " "), "spike of +7.0% given back") {
		t.Fatalf("ok=%v %+v", ok, a)
	}
}

func TestVeryNegativeFundingAlertsOnceADay(t *testing.T) {
	start := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	tr := &track{}
	now := series(tr, start, flat(130, 50), 50*0.99, -0.0004)
	if _, ok := tr.judge(now, "F"); ok {
		t.Fatal("a flat coin raised a move alert")
	}
	a, ok := tr.fundingFlip(now, "F")
	if !ok || a.Rule != RuleFundingFlip || a.Suggestion.Side != "short" || !strings.Contains(a.History, "such alerts") || !strings.Contains(a.String(), "shorts paying") {
		t.Fatalf("ok=%v %+v", ok, a)
	}
	if _, ok := tr.fundingFlip(now.Add(6*time.Hour), "F"); ok {
		t.Fatal("alerted again within the day")
	}
	if _, ok := tr.fundingFlip(now.Add(25*time.Hour), "F"); !ok {
		t.Fatal("did not alert again after a day")
	}
}
