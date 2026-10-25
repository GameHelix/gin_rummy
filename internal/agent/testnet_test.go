package agent

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wickwise/trader/internal/binance"
	"github.com/wickwise/trader/internal/config"
	"github.com/wickwise/trader/internal/num"
	"github.com/wickwise/trader/internal/testkit"
)

// TestTestnetRoundTrip buys $10 of BTC on the Binance Spot Test Network and
// sells it back through the agent's own order code, in a throwaway account,
// then checks the ledger against the exchange. It needs testnet keys in
// .env and runs only with TRADER_TESTNET=1 (make smoke).
func TestTestnetRoundTrip(t *testing.T) {
	if os.Getenv("TRADER_TESTNET") != "1" {
		t.Skip("set TRADER_TESTNET=1 to trade test funds on the Binance testnet")
	}
	if err := config.LoadEnv(filepath.Join("..", "..", ".env")); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("TRADER_MODE") == config.Live {
		t.Fatal("the round trip only runs with testnet keys (TRADER_MODE=testnet)")
	}
	ctx := context.Background()
	e := &Engine{
		Config:   testkit.Config(t.TempDir()),
		Exchange: &binance.Client{HTTP: &http.Client{Timeout: 30 * time.Second}, BaseURL: binance.TestnetURL, DataURL: binance.DataURL, Key: os.Getenv("BINANCE_API_KEY"), Secret: os.Getenv("BINANCE_API_SECRET")},
	}
	if _, err := e.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.open(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.watch(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := e.Exchange.Account(ctx)
	if err != nil {
		t.Fatal(err)
	}
	buy, err := e.send(ctx, 1, Plan{Symbol: "BTCUSDT", Side: "BUY", QuoteQty: num.Int(10)}, false)
	if err != nil || buy == nil {
		t.Fatalf("buy: %v", err)
	}
	held := e.st.Ledger.Holdings["BTCUSDT"]
	sell, err := e.send(ctx, 1, Plan{Symbol: "BTCUSDT", Side: "SELL", Qty: held.Floor(e.rules["BTCUSDT"].StepSize)}, false)
	if err != nil || sell == nil {
		t.Fatalf("sell: %v", err)
	}
	after, err := e.Exchange.Account(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("bought %s BTC for %s USDT, sold %s for %s; fees $%s", buy.Qty, buy.QuoteQty, sell.Qty, sell.QuoteQty, e.st.Ledger.FeesUSD)
	if len(e.st.Pending) != 0 || len(e.st.Trades) != 2 || e.st.Halted != "" {
		t.Fatalf("pending %v, trades %d, halted %q", e.st.Pending, len(e.st.Trades), e.st.Halted)
	}
	// What moved on the exchange is exactly what the ledger booked, apart from
	// the live fee the testnet does not charge but the ledger pays on paper.
	for asset, booked := range map[string]num.Dec{"USDT": e.st.Ledger.Cash.Add(e.st.Ledger.FeesUSD).Sub(num.Int(100)), "BTC": e.st.Ledger.Holdings["BTCUSDT"]} {
		if moved := after.Balances[asset].Total().Sub(before.Balances[asset].Total()); moved.Cmp(booked) != 0 {
			t.Errorf("%s: exchange moved %s, ledger booked %s", asset, moved, booked)
		}
	}
	// The lookups that recovery depends on agree with the fills.
	for _, tr := range []*Trade{buy, sell} {
		o, err := e.Exchange.QueryOrder(ctx, "BTCUSDT", tr.ClientID)
		if err != nil || !o.Final() || o.ExecutedQty.Cmp(tr.Qty) != 0 || o.Side != tr.Side {
			t.Fatalf("query %s: %+v %v", tr.ClientID, o, err)
		}
		fills, err := e.Exchange.OrderFills(ctx, "BTCUSDT", o.OrderID)
		var qty num.Dec
		for _, f := range fills {
			qty = qty.Add(f.Qty)
		}
		if err != nil || qty.Cmp(tr.Qty) != 0 {
			t.Fatalf("trades of %s: %+v %v", tr.ClientID, fills, err)
		}
	}
}
