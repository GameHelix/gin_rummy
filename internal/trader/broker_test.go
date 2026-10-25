package trader

import (
	"math"
	"testing"
)

func brokerConfig() Config {
	c := testConfig("unused")
	c.MaxPositionWeight, c.MaxInvestedWeight, c.SlippageBPS = 1, 1, 0
	return c
}

func TestHandCalculatedFillsAndFees(t *testing.T) {
	c := brokerConfig()
	acct := NewAccount(Cents(10000))
	fills, rej, err := ExecuteAtOpen(c, "2026-10-26", []Order{{ID: "b", Symbol: "AAA", Side: "buy", Quantity: 10}}, map[string]float64{"AAA": 100}, &acct)
	if err != nil || len(rej) != 0 || len(fills) != 1 {
		t.Fatal(fills, rej, err)
	}
	// 10 × $100 + $1 commission.
	if acct.Cash != Cents(8999) || acct.Positions["AAA"] != 10 || fills[0].CashAfter != Cents(8999) || acct.FeesPaid != Cents(1) {
		t.Fatalf("ledger after buy: %+v", acct)
	}
	fills, _, err = ExecuteAtOpen(c, "2026-10-27", []Order{{ID: "s", Symbol: "AAA", Side: "sell", Quantity: 10}}, map[string]float64{"AAA": 105}, &acct)
	if err != nil || len(fills) != 1 || acct.Cash != Cents(10048) || len(acct.Positions) != 0 {
		t.Fatalf("ledger after sell: %+v %+v %v", acct, fills, err)
	}
}

func TestSlippageAndGapAreRecheckedAtTheOpen(t *testing.T) {
	c := testConfig("unused")
	c.MaxPositionWeight, c.MaxInvestedWeight = 0.5, 0.5
	acct := NewAccount(Cents(10000))
	// Sized at a $100 close for 50 shares (50%); the stock gaps to $120.
	fills, _, err := ExecuteAtOpen(c, "2026-10-26", []Order{{ID: "b", Symbol: "AAA", Side: "buy", Quantity: 50}}, map[string]float64{"AAA": 120}, &acct)
	if err != nil || len(fills) != 1 {
		t.Fatal(fills, err)
	}
	f := fills[0]
	if f.Price != 12006 || f.Quantity >= 50 || f.Note != "reduced by limits rechecked at the open" {
		t.Fatalf("5 bps adverse slippage on $120 is $120.06 and the gap must shrink the order: %+v", f)
	}
	if spent := Cents(10000) - acct.Cash; float64(spent) > 0.5*float64(Cents(10000)) {
		t.Fatalf("exposure limit breached at the open: spent %d", spent)
	}
}

func TestSellsRunBeforeBuysAndEmptySellsAreRejected(t *testing.T) {
	c := brokerConfig()
	acct := NewAccount(Cents(100))
	acct.Positions["BBB"] = 10
	orders := []Order{{ID: "1", Symbol: "AAA", Side: "buy", Quantity: 10}, {ID: "2", Symbol: "BBB", Side: "sell", Quantity: 10}, {ID: "3", Symbol: "CCC", Side: "sell", Quantity: 5}}
	fills, rej, err := ExecuteAtOpen(c, "2026-10-26", orders, map[string]float64{"AAA": 50, "BBB": 50, "CCC": 10}, &acct)
	if err != nil || len(fills) != 2 || fills[0].Side != "sell" || fills[1].Quantity != 10 {
		t.Fatalf("sale proceeds must fund the purchase: %+v %v", fills, err)
	}
	if len(rej) != 1 || rej[0].Symbol != "CCC" {
		t.Fatalf("selling shares that are not held must be rejected: %+v", rej)
	}
}

func FuzzFillsReconcileWithIndependentLedger(f *testing.F) {
	f.Add(100.0, 105.0, 0.3, 5.0)
	f.Add(0.01, 0.02, 1.0, 1000.0)
	f.Add(9999.0, 1.0, 0.5, 0.0)
	f.Fuzz(func(t *testing.T, first, second, weight, slippage float64) {
		for _, v := range []float64{first, second} {
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0.01 || v > 1e6 {
				return
			}
		}
		if math.IsNaN(weight) || weight <= 0 || weight > 1 || math.IsNaN(slippage) || slippage < 0 || slippage > 1000 {
			return
		}
		c := testConfig("unused")
		c.SlippageBPS, c.MaxPositionWeight, c.MaxInvestedWeight = slippage, weight, weight
		acct := NewAccount(Cents(10000))
		qty := int64(math.Floor(weight * 10000 / first))
		var fills []Fill
		for _, step := range []struct {
			side  string
			price float64
		}{{"buy", first}, {"sell", second}} {
			got, _, err := ExecuteAtOpen(c, "2026-10-26", []Order{{ID: step.side, Symbol: "AAA", Side: step.side, Quantity: max(qty, 1)}}, map[string]float64{"AAA": step.price}, &acct)
			if err != nil {
				return // values beyond the supported range fail explicitly
			}
			fills = append(fills, got...)
		}
		cash, held := int64(Cents(10000)), int64(0)
		for _, fill := range fills {
			if fill.Price <= 0 || fill.Quantity <= 0 || fill.Quantity > fill.Requested {
				t.Fatalf("invalid fill %+v", fill)
			}
			if fill.Side == "buy" {
				cash -= fill.Quantity*int64(fill.Price) + int64(fill.Fee)
				held += fill.Quantity
			} else {
				cash += fill.Quantity*int64(fill.Price) - int64(fill.Fee)
				held -= fill.Quantity
			}
			if cash < 0 || held < 0 || cash != int64(fill.CashAfter) {
				t.Fatalf("fill does not reconcile: %+v", fill)
			}
		}
		if cash != int64(acct.Cash) || held != acct.Positions["AAA"] {
			t.Fatal("account disagrees with the independently recomputed ledger")
		}
	})
}
