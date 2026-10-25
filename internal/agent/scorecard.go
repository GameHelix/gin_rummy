package agent

import (
	"fmt"
	"sort"

	"github.com/ismatsamadov/trader/internal/analyst"
	"github.com/ismatsamadov/trader/internal/config"
	"github.com/ismatsamadov/trader/internal/market"

	"math"
)

// OpenSignal is a rating waiting for its horizon to elapse. Entry is the next
// session's open (when the orders fill); exit is the close horizon-1 sessions later.
type OpenSignal struct {
	DecidedOn    string         `json:"decided_on"`
	Symbol       string         `json:"symbol"`
	Rating       analyst.Rating `json:"rating"`
	Confidence   string         `json:"confidence"`
	EntrySession string         `json:"entry_session"`
	ExitSession  string         `json:"exit_session"`
}

type ScoredSignal struct {
	OpenSignal
	Return    float64 `json:"return"`
	Benchmark float64 `json:"benchmark_return"`
	Excess    float64 `json:"excess_return"`
	// Hit is nil for hold ratings, which make no directional claim.
	Hit *bool `json:"hit,omitempty"`
}

func NewOpenSignals(c config.Config, decidedOn string, signals map[string]analyst.Signal) ([]OpenSignal, error) {
	win, err := signalWindow(c, decidedOn)
	if err != nil {
		return nil, err
	}
	return openSignals(c, win, decidedOn, signals), nil
}

type window struct{ entry, exit string }

// signalWindow is checked before any paid analysis, so a calendar that cannot
// schedule the exit never fails a decision after the money is spent.
func signalWindow(c config.Config, decidedOn string) (window, error) {
	entry, err := market.NextSessionAfter(decidedOn)
	if err != nil {
		return window{}, err
	}
	exit := entry
	for i := 1; i < c.HorizonSessions; i++ {
		if exit, err = market.NextSessionAfter(exit.Date); err != nil {
			return window{}, err
		}
	}
	return window{entry.Date, exit.Date}, nil
}

func openSignals(c config.Config, win window, decidedOn string, signals map[string]analyst.Signal) []OpenSignal {
	var out []OpenSignal
	for _, s := range c.Symbols {
		if sig, ok := signals[s]; ok {
			out = append(out, OpenSignal{decidedOn, s, sig.Rating, sig.Confidence, win.entry, win.exit})
		}
	}
	return out
}

// ScoreThrough grades signals whose exit session is on or before `through`,
// against the market symbol (or the equal-weight universe when none is set).
func ScoreThrough(c config.Config, open []OpenSignal, through string, series map[string]market.Series) ([]OpenSignal, []ScoredSignal, error) {
	var keep []OpenSignal
	var scored []ScoredSignal
	windowReturn := func(symbol, entry, exit string) (float64, error) {
		in, ok1 := series[symbol].BarOn(entry)
		out, ok2 := series[symbol].BarOn(exit)
		if !ok1 || !ok2 {
			return 0, fmt.Errorf("%s has no bars for %s–%s to score a signal", symbol, entry, exit)
		}
		// Total return: dividends going ex after the entry open count too.
		return (out.Close+series[symbol].DividendsBetween(entry, exit))/in.Open - 1, nil
	}
	for _, sig := range open {
		if sig.ExitSession > through {
			keep = append(keep, sig)
			continue
		}
		ret, err := windowReturn(sig.Symbol, sig.EntrySession, sig.ExitSession)
		if err != nil {
			return nil, nil, err
		}
		var bench float64
		if c.MarketSymbol != "" {
			if bench, err = windowReturn(c.MarketSymbol, sig.EntrySession, sig.ExitSession); err != nil {
				return nil, nil, err
			}
		} else {
			for _, s := range c.Symbols {
				r, err := windowReturn(s, sig.EntrySession, sig.ExitSession)
				if err != nil {
					return nil, nil, err
				}
				bench += r / float64(len(c.Symbols))
			}
		}
		result := ScoredSignal{OpenSignal: sig, Return: round(ret, 6), Benchmark: round(bench, 6), Excess: round(ret-bench, 6)}
		if score, _ := sig.Rating.Score(); score != 0 {
			hit := (score > 0 && ret-bench > 0) || (score < 0 && ret-bench < 0)
			result.Hit = &hit
		}
		scored = append(scored, result)
	}
	return keep, scored, nil
}

type ScoreSummary struct {
	Rating     analyst.Rating `json:"rating"`
	Count      int            `json:"count"`
	Hits       int            `json:"hits"`
	MeanExcess float64        `json:"mean_excess_return"`
}

func Summarize(scored []ScoredSignal) []ScoreSummary {
	by := map[analyst.Rating]*ScoreSummary{}
	for _, s := range scored {
		sum := by[s.Rating]
		if sum == nil {
			sum = &ScoreSummary{Rating: s.Rating}
			by[s.Rating] = sum
		}
		sum.Count++
		sum.MeanExcess += s.Excess
		if s.Hit != nil && *s.Hit {
			sum.Hits++
		}
	}
	var out []ScoreSummary
	for _, sum := range by {
		sum.MeanExcess = round(sum.MeanExcess/float64(sum.Count), 6)
		out = append(out, *sum)
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := out[i].Rating.Score()
		b, _ := out[j].Rating.Score()
		return a > b
	})
	return out
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}
