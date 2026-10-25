package broker

import "testing"

func TestDividendsBecomeReceivables(t *testing.T) {
	acct := NewAccount(Cents(1000))
	acct.Positions["AAA"] = 30
	credit, err := acct.ApplyDividend("AAA", 0.245, "2026-11-20")
	if err != nil || credit != 735 || acct.Receivable != 735 || acct.Cash != Cents(1000) {
		t.Fatalf("30 × $0.245 = $7.35 receivable, cash unchanged: %+v %d %v", acct, credit, err)
	}
	equity, _ := acct.Equity(map[string]Money{"AAA": 1000})
	if equity != Cents(1000)+735+30*1000 {
		t.Fatalf("receivables count toward equity: %d", equity)
	}
}

func TestSplitsConvertSharesAndPayCashInLieu(t *testing.T) {
	acct := NewAccount(0)
	acct.Positions["AAA"] = 7
	if _, err := acct.ApplySplit("AAA", 10, 1, 1000); err != nil || acct.Positions["AAA"] != 70 {
		t.Fatalf("10-for-1: %+v %v", acct, err)
	}
	cash, err := acct.ApplySplit("AAA", 1, 3, 3000)
	// 70 / 3 = 23 shares plus 1/3 share paid at $30.00 → $10.00.
	if err != nil || acct.Positions["AAA"] != 23 || cash != 1000 || acct.Cash != 1000 {
		t.Fatalf("1-for-3 reverse split: %+v cash %d %v", acct, cash, err)
	}
	orders, err := AdjustOrdersForSplit([]Order{{Symbol: "AAA", Quantity: 5}, {Symbol: "BBB", Quantity: 5}, {Symbol: "AAA", Quantity: 1}}, "AAA", 1, 2)
	if err != nil || len(orders) != 2 || orders[0].Quantity != 2 || orders[1].Symbol != "BBB" {
		t.Fatalf("pending orders follow the split; fractional orders vanish: %+v %v", orders, err)
	}
}

func TestDividendsPayOutOnTheirPayDate(t *testing.T) {
	acct := NewAccount(0)
	acct.Positions["AAA"] = 10
	acct.ApplyDividend("AAA", 0.5, "2026-11-20")
	if paid, _ := acct.SettleDividends("2026-11-19"); paid != 0 || acct.Cash != 0 {
		t.Fatal("paid before the pay date")
	}
	if paid, _ := acct.SettleDividends("2026-11-20"); paid != 500 || acct.Cash != 500 || acct.Receivable != 0 || len(acct.Receivables) != 0 {
		t.Fatalf("not settled: %+v", acct)
	}
}

func TestDividendRoundingIsExact(t *testing.T) {
	// 3 × $0.145 = $0.435 exactly, which rounds half up to 44 cents.
	if got, err := dividendCents(3, 0.145); err != nil || got != 44 {
		t.Fatalf("got %d %v", got, err)
	}
}

func TestValuingAHoldingWithoutAPriceFails(t *testing.T) {
	if _, err := portfolioValue(map[string]int64{"AAA": 10}, map[string]Money{}); err == nil {
		t.Fatal("a missing price must not count as zero")
	}
}
