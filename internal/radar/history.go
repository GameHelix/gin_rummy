package radar

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
)

// What followed moves like an alert's on Binance USDT-M perpetuals from 2020
// to 2026, from the research repository: the coin's median return and the share of
// gains for a buyer at the next bar, net of 0.15%, 4 hours and a day later.
// Keyed by direction, size and volume; crashes by what came before them.

//go:embed history_after_move.json
var afterMoveJSON []byte

//go:embed history_crash.json
var crashJSON []byte

//go:embed history_funding.json
var fundingJSON []byte

//go:embed history_triggers.json
var triggersJSON []byte

type outcome struct {
	N         int     `json:"n"`
	Median4h  float64 `json:"median_4h"`
	Up4h      float64 `json:"up_4h"`
	Median24h float64 `json:"median_24h"`
	Up24h     float64 `json:"up_24h"`
}

var afterMove, crash map[string]outcome

// fundingOutcome is what followed a funding payment of a given size on
// Binance perpetuals: the coin's median move and share of gains a day
// later. Keyed high (0.3%+ per 8h, longs pay) and very_negative (−0.2% or
// less, shorts pay); the rest is ordinary.
type fundingOutcome struct {
	N         int     `json:"n"`
	Median24h float64 `json:"median_24h"`
	Up24h     float64 `json:"up_24h"`
	Median72h float64 `json:"median_72h"`
}

var funding map[string]fundingOutcome

// triggerOutcome is what followed each named rule on every perpetual
// from 2021 to 2026, bought at the next bar, before costs.
type triggerOutcome struct {
	N         int     `json:"n"`
	Median4h  float64 `json:"median_4h"`
	Up4h      float64 `json:"up_4h"`
	Median24h float64 `json:"median_24h"`
	Up24h     float64 `json:"up_24h"`
}

var triggers map[string]triggerOutcome

func init() {
	json.Unmarshal(afterMoveJSON, &afterMove)
	json.Unmarshal(crashJSON, &crash)
	json.Unmarshal(fundingJSON, &funding)
	json.Unmarshal(triggersJSON, &triggers)
}

// triggerKey names the study row that matches an alert's rule.
func triggerKey(a Alert) string {
	switch a.Rule {
	case RulePumpAndCrash:
		return "crash 10%+ after 20%+ pump"
	case RuleUnpumpedCrash:
		return "crash 10%+ of unpumped coin"
	case RuleCrowdedRise:
		return "rise 1h 2%+ with OI +10%"
	case RuleSpikeRejected:
		return "upper wick 5%+ rejected"
	case RuleFundingFlip:
		return "funding <= -0.2%"
	case RuleBigMove:
		if a.Change15m > 0 {
			return "rise 15m 3σ on 2x volume (radar)"
		}
		return "drop 15m 3σ on 2x volume (radar)"
	case RuleBigHour:
		if a.Change1h > 0 {
			return "hour +6% (radar)"
		}
		return "hour -6% (radar)"
	case RuleOISwing:
		if a.Change1h < 0 {
			return "drop 1h 2%+ with OI +10%"
		}
	}
	return ""
}

// triggerHistory words what followed this rule, or nothing.
func triggerHistory(a Alert) string {
	o, ok := triggers[triggerKey(a)]
	if !ok || o.N < 50 {
		return ""
	}
	return fmt.Sprintf("After %s such alerts (perps, 2021–2026): 4h median %s, %.0f%% up; 24h %s, %.0f%% up.", count(o.N), pct(o.Median4h), o.Up4h*100, pct(o.Median24h), o.Up24h*100)
}

// Hourly funding rates that match the study's 8-hour extremes.
const (
	fundingHigh         = 0.003 / 8
	fundingVeryNegative = -0.002 / 8
)

// fundingNote words what followed funding this extreme, or nothing.
func fundingNote(hourly float64) string {
	key, what := "", ""
	switch {
	case hourly >= fundingHigh:
		key, what = "high", "Funding this high, longs paying"
	case hourly <= fundingVeryNegative:
		key, what = "very_negative", "Funding this negative, shorts paying"
	default:
		return ""
	}
	o, ok := funding[key]
	if !ok || o.N < 50 {
		return ""
	}
	return fmt.Sprintf("%s (perps, since 2020): 24h median %s, %.0f%% up; 72h %s.", what, pct(o.Median24h), o.Up24h*100, pct(o.Median72h))
}

// historyOf words what followed moves like this one and, when funding is
// extreme, what followed that; nothing for a smaller move at ordinary
// funding.
func historyOf(a Alert) string {
	move, note := triggerHistory(a), fundingNote(a.Funding)
	if move == "" {
		move = moveHistory(a)
	}
	if a.Rule == RuleFundingFlip {
		note = ""
	}
	switch {
	case move != "" && note != "":
		return move + " " + note
	case move != "":
		return move
	}
	return note
}

// moveHistory words what followed 15-minute moves like this one, or
// nothing when the move is under 3%.
func moveHistory(a Alert) string {
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
			return fmt.Sprintf("After %s such crashes of %s (perps, since 2020): 4h median %s, %.0f%% up; 24h %s, %.0f%% up.", count(o.N), what, pct(o.Median4h), o.Up4h*100, pct(o.Median24h), o.Up24h*100)
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
	return fmt.Sprintf("After %s such %s (perps, since 2020): 4h median %s, %.0f%% up; 24h %s, %.0f%% up.", count(o.N), what, pct(o.Median4h), o.Up4h*100, pct(o.Median24h), o.Up24h*100)
}

func count(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%d,%03d", n/1000, n%1000)
	}
	return fmt.Sprint(n)
}
