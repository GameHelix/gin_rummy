package trader

import (
	"math"
	"testing"
)

func planFixture() (Config, Account, map[string]Money) {
	c := testConfig("unused")
	c.Symbols = []string{"AAA", "BBB", "CCC"}
	c.MaxPositionWeight, c.MaxInvestedWeight = 0.3, 0.8
	return c, NewAccount(Cents(10000)), map[string]Money{"AAA": 10000, "BBB": 5000, "CCC": 2500}
}

func lineFor(plan []PlanLine, symbol string) PlanLine {
	for _, l := range plan {
		if l.Symbol == symbol {
			return l
		}
	}
	return PlanLine{}
}

func TestRatingsMapToDeterministicTargets(t *testing.T) {
	c, acct, closes := planFixture()
	signals := map[string]Signal{"AAA": {Rating: RatingStrongBuy}, "BBB": {Rating: RatingBuy}, "CCC": {Rating: RatingHold}}
	orders, plan, err := PlanOrders(c, "2026-10-23", signals, acct, closes)
	if err != nil {
		t.Fatal(err)
	}
	// strong_buy: floor(0.30 × $10,000 / $100) = 30 shares; buy: floor(0.15 × $10,000 / $50) = 30.
	if len(orders) != 2 || orders[0].Symbol != "AAA" || orders[0].Quantity != 30 || orders[1].Quantity != 30 {
		t.Fatalf("unexpected orders: %+v", orders)
	}
	if l := lineFor(plan, "CCC"); l.Action != "none" || l.Note != "hold" {
		t.Fatalf("hold while flat must not trade: %+v", l)
	}
}

func TestNoTradeBandMinimumSizeAndExits(t *testing.T) {
	c, acct, closes := planFixture()
	c.MinTradeUSD = 200
	acct.Positions = map[string]int64{"AAA": 14, "BBB": 30, "CCC": 50}
	acct.Cash = Cents(10000) - 14*10000 - 30*5000 - 50*2500
	// AAA at 14% with target 15% (buy): inside the 3% band.
	// BBB at 15% with target 0 (sell): exits regardless of size.
	// CCC at 12.5% with strong_buy target 30%: buys.
	signals := map[string]Signal{"AAA": {Rating: RatingBuy}, "BBB": {Rating: RatingSell}, "CCC": {Rating: RatingStrongBuy}}
	orders, plan, err := PlanOrders(c, "2026-10-23", signals, acct, closes)
	if err != nil {
		t.Fatal(err)
	}
	if l := lineFor(plan, "AAA"); l.Action != "none" {
		t.Fatalf("a 1-point change must stay inside the band: %+v", l)
	}
	if l := lineFor(plan, "BBB"); l.Action != "sell" || l.Quantity != 30 {
		t.Fatalf("sell must exit the whole position: %+v", l)
	}
	if l := lineFor(plan, "CCC"); l.Action != "buy" || l.Quantity != 70 {
		t.Fatalf("strong_buy from 12.5%% to 30%%: %+v", l)
	}
	if len(orders) != 2 {
		t.Fatalf("orders: %+v", orders)
	}
	// Without a band, AAA at $50: 28 shares (14%) to the 15% target is 30
	// shares, a $100 trade, still below the $200 minimum.
	c.RebalanceBand = 0
	closes["AAA"] = 5000
	acct.Positions["AAA"] = 28
	_, plan, _ = PlanOrders(c, "2026-10-23", signals, acct, closes)
	if l := lineFor(plan, "AAA"); l.Action != "none" {
		t.Fatalf("trades below min_trade_usd must be skipped: %+v", l)
	}
}

func TestExposureCapCutsNewPurchasesFirst(t *testing.T) {
	c, acct, closes := planFixture()
	c.MaxPositionWeight, c.MaxInvestedWeight = 0.4, 0.5
	acct.Positions = map[string]int64{"AAA": 20}
	acct.Cash = Cents(10000) - 20*10000
	signals := map[string]Signal{"AAA": {Rating: RatingHold}, "BBB": {Rating: RatingStrongBuy}, "CCC": {Rating: RatingStrongBuy}}
	_, plan, err := PlanOrders(c, "2026-10-23", signals, acct, closes)
	if err != nil {
		t.Fatal(err)
	}
	var total float64
	for _, l := range plan {
		total += l.TargetWeight
	}
	if math.Abs(total-0.5) > 1e-3 || lineFor(plan, "AAA").TargetWeight != 0.2 {
		t.Fatalf("held 20%% stays; new buys share the remaining 30%%: %+v", plan)
	}
}

func TestHoldTrimsOnlyBeyondLimitPlusBand(t *testing.T) {
	c, acct, closes := planFixture()
	acct.Positions = map[string]int64{"AAA": 40}
	acct.Cash = Cents(10000) - 40*10000
	_, plan, _ := PlanOrders(c, "2026-10-23", map[string]Signal{"AAA": {Rating: RatingHold}}, acct, closes)
	if l := lineFor(plan, "AAA"); l.Action != "sell" || l.TargetWeight != 0.3 {
		t.Fatalf("a 40%% position above a 30%% limit must be trimmed: %+v", l)
	}
	acct.Positions["AAA"] = 32
	acct.Cash = Cents(10000) - 32*10000
	_, plan, _ = PlanOrders(c, "2026-10-23", map[string]Signal{"AAA": {Rating: RatingHold}}, acct, closes)
	if l := lineFor(plan, "AAA"); l.Action != "none" {
		t.Fatalf("drift inside the band is tolerated: %+v", l)
	}
}

func TestBenchmarkBuysEqualWeights(t *testing.T) {
	c, acct, closes := planFixture()
	orders, err := BenchmarkOrders(c, "2026-10-23", acct, closes)
	if err != nil || len(orders) != 3 {
		t.Fatal(orders, err)
	}
	// min(0.30, 0.80/3) = 26.67% of $10,000 per symbol.
	if orders[0].Quantity != 26 || orders[1].Quantity != 53 || orders[2].Quantity != 106 {
		t.Fatalf("benchmark quantities: %+v", orders)
	}
}

func TestAggregateRoundsDisagreementTowardHold(t *testing.T) {
	samples := []map[string]Signal{{"AAA": {Rating: RatingBuy}}, {"AAA": {Rating: RatingSell}}}
	if got := Aggregate([]string{"AAA", "BBB"}, samples); got["AAA"].Rating != RatingHold || got["BBB"].Rating != RatingHold {
		t.Fatalf("split vote must hold: %+v", got)
	}
}
