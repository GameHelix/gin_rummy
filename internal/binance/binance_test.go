package binance

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ismatsamadov/trader/internal/num"
)

func TestSignatureMatchesTheDocumentedExample(t *testing.T) {
	payload := "symbol=LTCBTC&side=BUY&type=LIMIT&timeInForce=GTC&quantity=1&price=0.1&recvWindow=5000&timestamp=1499827319559"
	secret := "NhqPtmdSJYdKjVHjA7PZj4Mge3R5YNiP1e3UZjInClVN65XAbvqqM6A7H5fATj0j"
	if got := Sign(secret, payload); got != "c8db56825ae71d6d79447849e617115f4a920fa2acdcab2b053c4b2838bd6b71" {
		t.Fatalf("signature %s", got)
	}
}

// server checks the signature of every signed request before answering.
func server(t *testing.T, handle func(w http.ResponseWriter, r *http.Request, q url.Values)) *Client {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.RawQuery
		if i := strings.LastIndex(raw, "&signature="); i >= 0 {
			if r.Header.Get("X-MBX-APIKEY") != "key" || Sign("secret", raw[:i]) != raw[i+len("&signature="):] {
				w.WriteHeader(401)
				w.Write([]byte(`{"code":-1022,"msg":"Signature for this request is not valid."}`))
				return
			}
		}
		handle(w, r, r.URL.Query())
	}))
	t.Cleanup(s.Close)
	return &Client{HTTP: s.Client(), BaseURL: s.URL, DataURL: s.URL, Key: "key", Secret: "secret"}
}

const filled = `{"symbol":"BTCUSDT","orderId":7,"clientOrderId":"d1-BTCUSDT","transactTime":1792928877494,"side":"BUY","status":"FILLED","executedQty":"0.00055000","cummulativeQuoteQty":"44.30580550",
"fills":[{"price":"80556.01000000","qty":"0.00055000","commission":"0.00000055","commissionAsset":"BTC","tradeId":1}]}`

func TestPlaceMarketSendsExactAmountsAndParsesFills(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, r *http.Request, q url.Values) {
		if r.Method != http.MethodPost || q.Get("quoteOrderQty") != "44.31" || q.Get("type") != "MARKET" || q.Get("newClientOrderId") != "d1-BTCUSDT" || q.Get("quantity") != "" {
			t.Errorf("unexpected order %s %v", r.Method, q)
		}
		w.Write([]byte(filled))
	})
	o, err := c.PlaceMarket(context.Background(), OrderRequest{Symbol: "BTCUSDT", Side: "BUY", QuoteQty: num.MustParse("44.31"), ClientID: "d1-BTCUSDT"})
	if err != nil {
		t.Fatal(err)
	}
	if o.ExecutedQty.String() != "0.00055" || o.QuoteQty.String() != "44.3058055" || len(o.Fills) != 1 || o.Fills[0].CommissionAsset != "BTC" || !o.Final() {
		t.Fatalf("parsed %+v", o)
	}
}

func TestOrderFailuresAreClassified(t *testing.T) {
	for name, tc := range map[string]struct {
		reply   func(w http.ResponseWriter)
		unknown bool
	}{
		"server error":     {func(w http.ResponseWriter) { w.WriteHeader(503) }, true},
		"engine timeout":   {func(w http.ResponseWriter) { w.WriteHeader(400); w.Write([]byte(`{"code":-1007,"msg":"Timeout"}`)) }, true},
		"unreadable reply": {func(w http.ResponseWriter) { w.Write([]byte(`{"orderId":`)) }, true},
		"dropped":          {func(w http.ResponseWriter) { conn, _, _ := w.(http.Hijacker).Hijack(); conn.Close() }, true},
		"insufficient": {func(w http.ResponseWriter) {
			w.WriteHeader(400)
			w.Write([]byte(`{"code":-2010,"msg":"Account has insufficient balance"}`))
		}, false},
		"rate limited": {func(w http.ResponseWriter) {
			w.WriteHeader(429)
			w.Write([]byte(`{"code":-1003,"msg":"Too many requests"}`))
		}, false},
		"invalid key/perms": {func(w http.ResponseWriter) {
			w.WriteHeader(401)
			w.Write([]byte(`{"code":-2015,"msg":"Invalid API-key, IP, or permissions for action."}`))
		}, false},
	} {
		c := server(t, func(w http.ResponseWriter, r *http.Request, q url.Values) { tc.reply(w) })
		_, err := c.PlaceMarket(context.Background(), OrderRequest{Symbol: "BTCUSDT", Side: "SELL", Quantity: num.MustParse("0.001"), ClientID: "x"})
		if err == nil || errors.Is(err, ErrUnknown) != tc.unknown {
			t.Errorf("%s: err %v, unknown %v", name, err, errors.Is(err, ErrUnknown))
		}
	}
}

func TestClockSkewIsCorrectedAndRetried(t *testing.T) {
	serverTime := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	var orders atomic.Int32
	c := server(t, func(w http.ResponseWriter, r *http.Request, q url.Values) {
		switch r.URL.Path {
		case "/api/v3/time":
			w.Write([]byte(`{"serverTime":` + itoa(serverTime.UnixMilli()) + `}`))
		case "/api/v3/order":
			orders.Add(1)
			if q.Get("timestamp") != itoa(serverTime.UnixMilli()) {
				w.WriteHeader(400)
				w.Write([]byte(`{"code":-1021,"msg":"Timestamp for this request is outside of the recvWindow."}`))
				return
			}
			w.Write([]byte(filled))
		}
	})
	c.Now = func() time.Time { return serverTime.Add(-90 * time.Second) } // local clock 90 s slow
	if _, err := c.PlaceMarket(context.Background(), OrderRequest{Symbol: "BTCUSDT", Side: "BUY", QuoteQty: num.MustParse("10"), ClientID: "x"}); err != nil || orders.Load() != 2 {
		t.Fatalf("expected one rejected and one accepted attempt: %v, %d", err, orders.Load())
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func TestQueryMissingOrder(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, r *http.Request, q url.Values) {
		w.WriteHeader(400)
		w.Write([]byte(`{"code":-2013,"msg":"Order does not exist."}`))
	})
	if _, err := c.QueryOrder(context.Background(), "BTCUSDT", "x"); !errors.Is(err, ErrNoSuchOrder) {
		t.Fatalf("got %v", err)
	}
}

func TestMarketDataAndRules(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, r *http.Request, q url.Values) {
		switch r.URL.Path {
		case "/api/v3/klines":
			w.Write([]byte(`[[1792926000000,"80000.00","80600.00","79900.00","80556.00","12.5",1792929599999,"1006950.00",100,"6","483000","0"]]`))
		case "/api/v3/ticker/price":
			if q.Get("symbols") != `["BTCUSDT","ETHUSDT"]` {
				t.Errorf("symbols param %q", q.Get("symbols"))
			}
			w.Write([]byte(`[{"symbol":"BTCUSDT","price":"80556.00000000"},{"symbol":"ETHUSDT","price":"2338.57000000"}]`))
		case "/api/v3/exchangeInfo":
			w.Write([]byte(`{"symbols":[{"symbol":"BTCUSDT","status":"TRADING","baseAsset":"BTC","quoteAsset":"USDT","quoteOrderQtyMarketAllowed":true,"filters":[
{"filterType":"LOT_SIZE","minQty":"0.00001000","maxQty":"9000.00000000","stepSize":"0.00001000"},
{"filterType":"MARKET_LOT_SIZE","minQty":"0.00000000","maxQty":"126.39141716","stepSize":"0.00000000"},
{"filterType":"NOTIONAL","minNotional":"5.00000000","applyMinToMarket":true,"maxNotional":"9000000.00000000","applyMaxToMarket":false,"avgPriceMins":5}]}]}`))
		}
	})
	ctx := context.Background()
	k, err := c.Klines(ctx, "BTCUSDT", "1h", 1, time.Time{})
	if err != nil || len(k) != 1 || k[0].C != 80556 || k[0].QuoteVolume != 1006950 {
		t.Fatalf("klines %+v %v", k, err)
	}
	p, err := c.Prices(ctx, []string{"BTCUSDT", "ETHUSDT"})
	if err != nil || p["ETHUSDT"].String() != "2338.57" {
		t.Fatalf("prices %v %v", p, err)
	}
	s, err := c.Symbols(ctx, []string{"BTCUSDT"})
	b := s["BTCUSDT"]
	if err != nil || !b.Trading || b.Base != "BTC" || b.StepSize.String() != "0.00001" || b.MinNotional.String() != "5" || b.MaxQty.String() != "126.39141716" {
		t.Fatalf("rules %+v %v", b, err)
	}
}
