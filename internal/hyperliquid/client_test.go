package hyperliquid

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

type fakeExchange struct {
	t       *testing.T
	replies []string
	actions []json.RawMessage
	nonces  []uint64
	sigs    []Signature
	info    string
}

func (f *fakeExchange) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/info" {
		w.Write([]byte(f.info))
		return
	}
	var body struct {
		Action    json.RawMessage `json:"action"`
		Nonce     uint64          `json:"nonce"`
		Signature Signature       `json:"signature"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Error(err)
	}
	f.actions, f.nonces, f.sigs = append(f.actions, body.Action), append(f.nonces, body.Nonce), append(f.sigs, body.Signature)
	reply := f.replies[0]
	f.replies = f.replies[1:]
	w.Write([]byte(reply))
}

func TestOrdersAreSentAsSignedAndRepliesRead(t *testing.T) {
	s := signer(t)
	f := &fakeExchange{t: t, replies: []string{
		`{"status":"ok","response":{"type":"order","data":{"statuses":[{"filled":{"totalSz":"100","avgPx":"99.5","oid":77}},{"resting":{"oid":78}},{"error":"Insufficient margin to place order."}]}}}`,
		`{"status":"ok","response":{"type":"cancel","data":{"statuses":["success"]}}}`,
		`{"status":"ok","response":{"type":"cancel","data":{"statuses":[{"error":"Order 5 was never placed, already canceled, or filled."}]}}}`,
		`{"status":"err","response":"User or API Wallet 0xabc does not exist."}`,
		`{"status":"ok","response":{"type":"default"}}`,
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	clock := time.UnixMilli(1700000000000)
	c := &Client{BaseURL: srv.URL, Signer: s, Now: func() time.Time { return clock }}
	ctx := context.Background()

	orders := []Order{
		{Asset: 1, Buy: true, Price: 100, Size: 100, Tif: Gtc, Cloid: "0x00000000000000000000000000000001"},
		{Asset: 1, Buy: false, Price: 110, Size: 50, Tif: Alo},
		{Asset: 1, Buy: false, Price: 90, Size: 100, ReduceOnly: true, Trigger: &Trigger{Price: 95, Market: true, Kind: "sl"}},
	}
	placed, err := c.Place(ctx, orders, GroupNormalTPSL, &Builder{Address: "0xAbCdEf0123456789AbCdEf0123456789AbCdEf01", Fee: 10})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"order","orders":[{"a":1,"b":true,"p":"100","s":"100","r":false,"t":{"limit":{"tif":"Gtc"}},"c":"0x00000000000000000000000000000001"},{"a":1,"b":false,"p":"110","s":"50","r":false,"t":{"limit":{"tif":"Alo"}}},{"a":1,"b":false,"p":"90","s":"100","r":true,"t":{"trigger":{"isMarket":true,"triggerPx":"95","tpsl":"sl"}}}],"grouping":"normalTpsl","builder":{"b":"0xabcdef0123456789abcdef0123456789abcdef01","f":10}}`
	if string(f.actions[0]) != want {
		t.Fatalf("sent %s", f.actions[0])
	}
	if f.nonces[0] != 1700000000000 {
		t.Fatalf("nonce %d", f.nonces[0])
	}
	// The signature must be over the action as sent, for the testnet.
	expected, _ := s.signL1(orderAction(orders, GroupNormalTPSL, &Builder{Address: "0xabcdef0123456789abcdef0123456789abcdef01", Fee: 10}), "", f.nonces[0], nil, false)
	if f.sigs[0] != expected {
		t.Fatalf("signature %+v, want %+v", f.sigs[0], expected)
	}
	if len(placed) != 3 || placed[0] != (Placed{OID: 77, Filled: 100, AvgPx: 99.5}) || placed[1] != (Placed{OID: 78, Resting: true}) || placed[2].Error != "Insufficient margin to place order." {
		t.Fatalf("placed %+v", placed)
	}

	if err := c.Cancel(ctx, 1, 77); err != nil {
		t.Fatal(err)
	}
	if string(f.actions[1]) != `{"type":"cancel","cancels":[{"a":1,"o":77}]}` || f.nonces[1] != 1700000000001 {
		t.Fatalf("cancel sent %s with nonce %d", f.actions[1], f.nonces[1])
	}
	var api *APIError
	if err := c.CancelByCloid(ctx, 1, "0x00000000000000000000000000000005"); !errors.As(err, &api) || api.Msg != "Order 5 was never placed, already canceled, or filled." {
		t.Fatalf("cancel by cloid: %v", err)
	}
	if string(f.actions[2]) != `{"type":"cancelByCloid","cancels":[{"asset":1,"cloid":"0x00000000000000000000000000000005"}]}` {
		t.Fatalf("sent %s", f.actions[2])
	}
	if _, err := c.Place(ctx, orders[:1], GroupNone, nil); !errors.As(err, &api) || api.Msg != "User or API Wallet 0xabc does not exist." || errors.Is(err, ErrUnknown) {
		t.Fatalf("refused batch: %v", err)
	}
	if err := c.SetLeverage(ctx, 1, true, 5); err != nil || string(f.actions[4]) != `{"type":"updateLeverage","asset":1,"isCross":true,"leverage":5}` {
		t.Fatalf("leverage: %v %s", err, f.actions[4])
	}
}

func TestAccountActionsArePreparedForTheWallet(t *testing.T) {
	s := signer(t)
	f := &fakeExchange{t: t, replies: []string{`{"status":"ok","response":{"type":"default"}}`}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Now: func() time.Time { return time.UnixMilli(1700000000000) }}

	a := c.ApproveBuilderFee("0xAbCdEf0123456789AbCdEf0123456789AbCdEf01", "0.01%")
	raw, _ := json.Marshal(a)
	var view struct {
		Action    json.RawMessage `json:"action"`
		TypedData struct {
			PrimaryType string                         `json:"primaryType"`
			Types       map[string][]map[string]string `json:"types"`
			Domain      map[string]any                 `json:"domain"`
		} `json:"typed_data"`
	}
	json.Unmarshal(raw, &view)
	if string(view.Action) != `{"type":"approveBuilderFee","hyperliquidChain":"Testnet","signatureChainId":"0x66eee","maxFeeRate":"0.01%","builder":"0xabcdef0123456789abcdef0123456789abcdef01","nonce":1700000000000}` {
		t.Fatalf("action %s", view.Action)
	}
	if view.TypedData.PrimaryType != "HyperliquidTransaction:ApproveBuilderFee" || len(view.TypedData.Types["HyperliquidTransaction:ApproveBuilderFee"]) != 4 || view.TypedData.Domain["chainId"] != float64(0x66eee) {
		t.Fatalf("typed data %+v", view.TypedData)
	}
	sig, err := a.Sign(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Submit(context.Background(), a, sig); err != nil {
		t.Fatal(err)
	}
	if string(f.actions[0]) != string(view.Action) || f.sigs[0] != sig || f.nonces[0] != a.Nonce {
		t.Fatalf("submitted %s %+v %d", f.actions[0], f.sigs[0], f.nonces[0])
	}
	if b := c.ApproveAgent(s.Address(), "candylario"); b.Nonce <= a.Nonce {
		t.Fatalf("nonce did not advance: %d after %d", b.Nonce, a.Nonce)
	}
}

func TestAccountIsReadFromTheExchangeReply(t *testing.T) {
	f := &fakeExchange{t: t, info: `{"marginSummary":{"accountValue":"1234.5","totalNtlPos":"500.0","totalRawUsd":"1000.0","totalMarginUsed":"100.0"},"crossMarginSummary":{},"crossMaintenanceMarginUsed":"10.0","withdrawable":"900.25","assetPositions":[{"type":"oneWay","position":{"coin":"ETH","szi":"-0.5","leverage":{"type":"cross","value":5},"entryPx":"2400.0","positionValue":"1180.0","unrealizedPnl":"20.0","returnOnEquity":"0.08","liquidationPx":"2900.5","marginUsed":"236.0","maxLeverage":25}},{"type":"oneWay","position":{"coin":"BTC","szi":"0.0","leverage":{"type":"isolated","value":3},"entryPx":null,"positionValue":"0.0","unrealizedPnl":"0.0","liquidationPx":null,"marginUsed":"0.0"}}],"time":1700000000000}`}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	a, err := c.Account(context.Background(), "0xAbC")
	if err != nil {
		t.Fatal(err)
	}
	if a.Value != 1234.5 || a.Withdrawable != 900.25 || a.MarginUsed != 100 || len(a.Positions) != 1 {
		t.Fatalf("%+v", a)
	}
	if p := a.Positions[0]; p != (Position{Coin: "ETH", Size: -0.5, EntryPx: 2400, Value: 1180, UnrealizedPnl: 20, LiquidationPx: 2900.5, MarginUsed: 236, Leverage: 5, Cross: true}) {
		t.Fatalf("%+v", p)
	}
}

func TestTestnetReads(t *testing.T) {
	if os.Getenv("TRADER_TESTNET") != "1" {
		t.Skip("set TRADER_TESTNET=1 to read the Hyperliquid testnet")
	}
	c := &Client{BaseURL: TestnetURL}
	ctx := context.Background()
	assets, markets, err := c.Markets(ctx)
	if err != nil || len(assets) < 50 || len(markets) != len(assets) {
		t.Fatalf("markets: %d %d %v", len(assets), len(markets), err)
	}
	var btc Asset
	for _, a := range assets {
		if a.Name == "BTC" {
			btc = a
		}
	}
	if btc.Name == "" || btc.SzDecimals != 5 || markets[btc.Index].MarkPx <= 0 {
		t.Fatalf("BTC %+v %+v", btc, markets[btc.Index])
	}
	mids, err := c.Mids(ctx)
	if err != nil || mids["BTC"] <= 0 {
		t.Fatalf("mids: %v %v", mids["BTC"], err)
	}
	now := time.Now()
	candles, err := c.Candles(ctx, "ETH", "1m", now.Add(-10*time.Minute), now)
	if err != nil || len(candles) < 5 || candles[0].Trades < 0 {
		t.Fatalf("candles: %d %v", len(candles), err)
	}
	role, err := c.Role(ctx, "0x0000000000000000000000000000000000000001")
	if err != nil || role.Role == "" || role.Role == "agent" {
		t.Fatalf("role: %+v %v", role, err)
	}
	if _, found, err := c.OrderStatus(ctx, "0x0000000000000000000000000000000000000001", 1, ""); err != nil || found {
		t.Fatalf("order status: %v %v", found, err)
	}
}
