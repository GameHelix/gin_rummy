package trader

import "testing"

func TestDividendsBecomeReceivables(t *testing.T) {
	acct := NewAccount(Cents(1000))
	acct.Positions["AAA"] = 30
	credit, err := acct.ApplyDividend("AAA", 0.245)
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
	if _, err := acct.ApplySplit("AAA", Split{"2026-10-27", 10, 1}, 1000); err != nil || acct.Positions["AAA"] != 70 {
		t.Fatalf("10-for-1: %+v %v", acct, err)
	}
	cash, err := acct.ApplySplit("AAA", Split{"2026-10-28", 1, 3}, 3000)
	// 70 / 3 = 23 shares plus 1/3 share paid at $30.00 → $10.00.
	if err != nil || acct.Positions["AAA"] != 23 || cash != 1000 || acct.Cash != 1000 {
		t.Fatalf("1-for-3 reverse split: %+v cash %d %v", acct, cash, err)
	}
	orders, err := AdjustOrdersForSplit([]Order{{Symbol: "AAA", Quantity: 5}, {Symbol: "BBB", Quantity: 5}, {Symbol: "AAA", Quantity: 1}}, "AAA", Split{"x", 1, 2})
	if err != nil || len(orders) != 2 || orders[0].Quantity != 2 || orders[1].Symbol != "BBB" {
		t.Fatalf("pending orders follow the split; fractional orders vanish: %+v %v", orders, err)
	}
}
