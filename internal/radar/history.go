package radar

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
)

// What followed moves like an alert's on Binance spot from 2017 to 2026,
// from the research repository: the coin's median return and the share of
// gains for a buyer at the next bar, net of 0.3%, 4 hours and a day later.
// Keyed by direction, size and volume; crashes by what came before them.

//go:embed history_after_move.json
var afterMoveJSON []byte

//go:embed history_crash.json
var crashJSON []byte

type outcome struct {
	N         int     `json:"n"`
	Median4h  float64 `json:"median_4h"`
	Up4h      float64 `json:"up_4h"`
	Median24h float64 `json:"median_24h"`
	Up24h     float64 `json:"up_24h"`
}

var afterMove, crash map[string]outcome

func init() {
	json.Unmarshal(afterMoveJSON, &afterMove)
	json.Unmarshal(crashJSON, &crash)
}

// historyOf words what followed moves like this one, or nothing when the
// alert is not a 15-minute move of 3% or more.
func historyOf(a Alert) string {
	m := math.Abs(a.Change15m)
	if m < 0.03 {
		return ""
	}
	if a.Change15m < 0 && m >= 0.10 {
		prior := (1+a.Change24h)/(1+a.Change15m) - 1
		key, what := "flat", "a coin that was within 20% in the day before"
		switch {
		case prior >= 0.5:
			key, what = "pumped50", "a coin that had risen 50%+ in the day before"
		case prior >= 0.2:
			key, what = "pumped20", "a coin that had risen 20–50% in the day before"
		case prior < -0.2:
			key, what = "beaten", "a coin already down 20%+ in the day before"
		}
		if o, ok := crash[key]; ok {
			return fmt.Sprintf("After %s such crashes of %s (spot, since 2017): 4h median %s, %.0f%% up; 24h %s, %.0f%% up.", count(o.N), what, pct(o.Median4h), o.Up4h*100, pct(o.Median24h), o.Up24h*100)
		}
		return ""
	}
	size := "3-5"
	switch {
	case m >= 0.20:
		size = "20+"
	case m >= 0.10:
		size = "10-20"
	case m >= 0.05:
		size = "5-10"
	}
	vol := "<2"
	switch {
	case a.VolumeRate >= 5:
		vol = "5+"
	case a.VolumeRate >= 2:
		vol = "2-5"
	}
	dir, what := "up", "rises"
	if a.Change15m < 0 {
		dir, what = "down", "drops"
	}
	o, ok := afterMove[dir+"|"+size+"|"+vol]
	if !ok || o.N < 50 {
		return ""
	}
	return fmt.Sprintf("After %s such %s (spot, since 2017): 4h median %s, %.0f%% up; 24h %s, %.0f%% up.", count(o.N), what, pct(o.Median4h), o.Up4h*100, pct(o.Median24h), o.Up24h*100)
}

func count(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%d,%03d", n/1000, n%1000)
	}
	return fmt.Sprint(n)
}
