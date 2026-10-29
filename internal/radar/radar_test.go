package radar

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/candylario/trader/internal/hyperliquid"
)

// market holds a coin's state as the fake exchange reports it.
type market struct {
	mid, oi, dayVol float64
}

func feed(w *Watcher, at time.Time, coins map[string]*market) []Alert {
	var assets []hyperliquid.Asset
	var markets []hyperliquid.Market
	for _, name := range []string{"BTC", "ETH", "TINY", "OIX"} {
		m, ok := coins[name]
		if !ok {
			continue
		}
		assets = append(assets, hyperliquid.Asset{Index: len(assets), Name: name})
		markets = append(markets, hyperliquid.Market{MidPx: m.mid, OpenInterest: m.oi, DayVolumeUSD: m.dayVol, PrevDayPx: m.mid * 0.98})
	}
	return w.Observe(at, assets, markets)
}

// quiet runs hours of small, regular moves: 0.2% wiggles, steady volume.
func quiet(w *Watcher, start time.Time, hours int, coins map[string]*market) time.Time {
	at := start
	for i := 0; i < hours*120; i++ {
		for _, m := range coins {
			m.mid *= 1 + 0.0004*math.Sin(float64(i)/7)
			m.dayVol += m.dayVol / 2880 // the day's volume stays level: what comes in equals what falls out
		}
		if alerts := feed(w, at, coins); len(alerts) != 0 {
			panic("quiet market alerted: " + alerts[0].String())
		}
		at = at.Add(30 * time.Second)
	}
	return at
}

func TestAJumpOnVolumeAlertsOnceAndRespectsTheCooldown(t *testing.T) {
	w := &Watcher{}
	start := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	coins := map[string]*market{"BTC": {mid: 80000, oi: 40000, dayVol: 2e9}, "ETH": {mid: 2400, oi: 1e6, dayVol: 5e8}, "TINY": {mid: 1, oi: 1000, dayVol: 2e5}}
	at := quiet(w, start, 3, coins)

	// ETH and TINY climb 5% over 15 minutes on six times the usual volume.
	var alerts []Alert
	for i := 0; i < 30; i++ {
		for _, name := range []string{"ETH", "TINY"} {
			m := coins[name]
			m.mid *= 1 + 0.05/30
			m.dayVol += 6 * m.dayVol / 96 / 30
		}
		alerts = append(alerts, feed(w, at, coins)...)
		at = at.Add(30 * time.Second)
	}
	if len(alerts) != 1 || alerts[0].Coin != "ETH" {
		t.Fatalf("alerts %+v", alerts)
	}
	a := alerts[0]
	if a.Change15m < 0.03 || a.VolumeRate < 2 || a.Sigma15m < 3 || !strings.Contains(a.String(), "the usual volume") || !strings.Contains(a.String(), "🚀 ETH") {
		t.Fatalf("alert %s (%+v)", a, a)
	}
	if !strings.HasPrefix(a.History, "After ") || !strings.Contains(a.History, "such rises") || !strings.Contains(a.String(), "\n"+a.History) {
		t.Fatalf("history %q", a.History)
	}
	if h := historyOf(Alert{Change15m: -0.12, Change24h: 0.6, VolumeRate: 3}); !strings.Contains(h, "risen 50%+") {
		t.Fatalf("crash history %q", h)
	}
	if h := historyOf(Alert{Change15m: 0.02, VolumeRate: 9}); h != "" {
		t.Fatalf("small move has history %q", h)
	}
	// Within the hour the coin stays quiet in the log however it moves.
	for i := 0; i < 20; i++ {
		coins["ETH"].mid *= 1.003
		if more := feed(w, at, coins); len(more) != 0 {
			t.Fatalf("alerted again inside the cooldown: %s", more[0])
		}
		at = at.Add(30 * time.Second)
	}
}

func TestAnOpenInterestSwingAlertsAndStrongerAlertsComeFirst(t *testing.T) {
	w := &Watcher{}
	start := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	coins := map[string]*market{"BTC": {mid: 80000, oi: 40000, dayVol: 2e9}, "OIX": {mid: 10, oi: 1e6, dayVol: 3e6}}
	at := quiet(w, start, 3, coins)
	var alerts []Alert
	for i := 0; i < 120; i++ {
		coins["OIX"].mid *= 1 + 0.025/120
		coins["OIX"].oi *= 1 + 0.12/120
		coins["BTC"].mid *= 1 + 0.07/120 // a 7% hour for BTC, the bigger move
		alerts = append(alerts, feed(w, at, coins)...)
		at = at.Add(30 * time.Second)
	}
	var oix, btc *Alert
	for i := range alerts {
		switch alerts[i].Coin {
		case "OIX":
			oix = &alerts[i]
		case "BTC":
			btc = &alerts[i]
		}
	}
	if oix == nil || oix.OIChange1h < 0.09 || !strings.Contains(oix.String(), "open interest +") {
		t.Fatalf("OIX %+v", oix)
	}
	if btc == nil || btc.Change1h < 0.055 {
		t.Fatalf("BTC %+v", btc)
	}
}

func TestSeededHistoryAlertsAtOnceAndLinesUpVolume(t *testing.T) {
	w := &Watcher{}
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	var candles []hyperliquid.Candle
	for i := 0; i < 97; i++ {
		open := now.Add(-time.Duration(97-i) * 15 * time.Minute)
		px := 100 * (1 + 0.003*math.Sin(float64(i)))
		candles = append(candles, hyperliquid.Candle{Open: open, Close: open.Add(15 * time.Minute), O: px, H: px, L: px, C: px, Volume: 1000})
	}
	// The last candle is still forming and must be left out.
	candles[96].Close = now.Add(10 * time.Minute)
	w.seed("ETH", now, candles)
	if n := len(w.coins["ETH"].samples); n != 96 {
		t.Fatalf("seeded %d samples", n)
	}
	seededVol := w.coins["ETH"].samples[95].dayVol
	// Live, the exchange reports a different day's volume; nothing moved.
	coins := map[string]*market{"ETH": {mid: candles[95].C, oi: 5000, dayVol: seededVol * 1.3}}
	if alerts := feed(w, now, coins); len(alerts) != 0 {
		t.Fatalf("the volume level difference alerted: %s", alerts[0])
	}
	// Ten minutes later a 4% jump on real volume alerts without an hour of live samples.
	at := now
	var alerts []Alert
	for i := 0; i < 20; i++ {
		at = at.Add(30 * time.Second)
		coins["ETH"].mid *= 1 + 0.04/20
		coins["ETH"].dayVol += 5 * coins["ETH"].dayVol / 96 / 20
		alerts = append(alerts, feed(w, at, coins)...)
	}
	if len(alerts) != 1 || alerts[0].Change15m < 0.03 {
		t.Fatalf("alerts %+v", alerts)
	}
}

func TestNoHistoryMeansNoAlerts(t *testing.T) {
	w := &Watcher{}
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	coins := map[string]*market{"ETH": {mid: 2400, oi: 1e6, dayVol: 5e8}}
	for i := 0; i < 10; i++ {
		coins["ETH"].mid *= 1.01
		if alerts := feed(w, now.Add(time.Duration(i)*30*time.Second), coins); len(alerts) != 0 {
			t.Fatalf("alerted without history: %s", alerts[0])
		}
	}
}
