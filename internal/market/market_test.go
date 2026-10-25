package market

import (
	"math"
	"testing"
	"time"
)

func series(n int, step time.Duration, end time.Time, price func(i int) float64) []Candle {
	out := make([]Candle, n)
	for i := range out {
		open := end.Add(-time.Duration(n-i) * step)
		p := price(i)
		out[i] = Candle{Open: open, O: p, H: p * 1.01, L: p * 0.99, C: p, Volume: 1, QuoteVolume: p, CloseTime: open.Add(step - time.Millisecond)}
	}
	return out
}

func TestViewReadsCompletedCandles(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 30, 0, 0, time.UTC)
	hourStart := now.Truncate(time.Hour)
	hourly := series(200, time.Hour, hourStart.Add(time.Hour), func(i int) float64 { return 100 + float64(i) })
	daily := series(365, 24*time.Hour, hourStart.Add(time.Hour), func(i int) float64 { return 1000 + float64(i) })
	if got := len(Closed(hourly, now)); got != 199 {
		t.Fatalf("the forming hourly candle must be dropped, kept %d", got)
	}
	v, err := NewView("BTCUSDT", "prices:BTCUSDT", 300, Closed(hourly, now), Closed(daily, now))
	if err != nil {
		t.Fatal(err)
	}
	// Completed hourly closes end at 298; the one before is 297.
	if math.Abs(v.Return1H-(298.0/297-1)) > 1e-5 || math.Abs(v.ChangeSinceHour-(300.0/298-1)) > 1e-5 {
		t.Fatalf("returns: %+v", v)
	}
	if v.RSI14H != 100 || v.SMA20D == 0 || v.High30D <= v.Low30D {
		t.Fatalf("indicators: %+v", v)
	}
}

func TestViewNeedsHistory(t *testing.T) {
	now := time.Now()
	short := series(50, time.Hour, now, func(int) float64 { return 1 })
	if _, err := NewView("X", "prices:X", 1, short, short); err == nil {
		t.Fatal("too little history must be an error")
	}
}
