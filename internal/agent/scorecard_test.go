package agent

import (
	"github.com/ismatsamadov/trader/internal/analyst"
	"github.com/ismatsamadov/trader/internal/market"
	"github.com/ismatsamadov/trader/internal/testkit"
)

import "testing"

func TestSignalsAreScoredAgainstTheMarketAfterTheirHorizon(t *testing.T) {
	c := testkit.Config("unused")
	open, err := NewOpenSignals(c, "2026-10-23", map[string]analyst.Signal{"AAA": {Rating: analyst.RatingStrongBuy}, "BBB": {Rating: analyst.RatingSell}})
	if err != nil || len(open) != 2 || open[0].EntrySession != "2026-10-26" || open[0].ExitSession != "2026-10-27" {
		t.Fatalf("Friday's signal enters Monday's open and exits Tuesday's close: %+v %v", open, err)
	}
	series := map[string]market.Series{
		"AAA": {Bars: []market.Bar{{Date: "2026-10-26", Open: 100, Close: 101}, {Date: "2026-10-27", Open: 101, Close: 103}}},
		"BBB": {Bars: []market.Bar{{Date: "2026-10-26", Open: 50, Close: 50}, {Date: "2026-10-27", Open: 50, Close: 49}}},
		"MKT": {Bars: []market.Bar{{Date: "2026-10-26", Open: 400, Close: 402}, {Date: "2026-10-27", Open: 402, Close: 404}}},
	}
	keep, scored, err := ScoreThrough(c, open, "2026-10-26", series)
	if err != nil || len(keep) != 2 || len(scored) != 0 {
		t.Fatal("signals must wait for their exit session")
	}
	keep, scored, err = ScoreThrough(c, open, "2026-10-27", series)
	if err != nil || len(keep) != 0 || len(scored) != 2 {
		t.Fatal(scored, err)
	}
	// AAA +3% vs market +1%: a hit. BBB −2% vs +1%: a correct sell.
	if !*scored[0].Hit || scored[0].Excess < 0.0199 || !*scored[1].Hit {
		t.Fatalf("scores: %+v", scored)
	}
	summary := Summarize(scored)
	if len(summary) != 2 || summary[0].Rating != analyst.RatingStrongBuy || summary[0].Hits != 1 {
		t.Fatalf("summary: %+v", summary)
	}
}
