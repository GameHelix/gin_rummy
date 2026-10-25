// Package binance is a minimal Binance spot client: public market data, the
// account, and market orders with exact decimal amounts.
package binance

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/ismatsamadov/trader/internal/market"
	"github.com/ismatsamadov/trader/internal/num"
)

const (
	LiveURL    = "https://api.binance.com"
	TestnetURL = "https://testnet.binance.vision"
	DataURL    = "https://data-api.binance.vision"
)

// Client talks to BaseURL for the account and orders and to DataURL for
// public prices. Both are set explicitly so a test cannot reach a live venue.
type Client struct {
	HTTP    *http.Client
	BaseURL string
	DataURL string
	Key     string
	Secret  string
	Now     func() time.Time

	mu     sync.Mutex
	offset time.Duration
}

// APIError is a response Binance rejected. The request was not executed
// unless Unknown says otherwise.
type APIError struct {
	HTTPStatus int
	Code       int
	Msg        string
}

func (e *APIError) Error() string {
	if e.Code == 0 {
		return fmt.Sprintf("Binance HTTP %d", e.HTTPStatus)
	}
	return fmt.Sprintf("Binance error %d: %s", e.Code, e.Msg)
}

// ErrUnknown marks an order request that may or may not have executed: the
// connection failed after sending, the server timed out, or the reply was
// unreadable. Query the order before doing anything else.
var ErrUnknown = errors.New("order outcome unknown")

var ErrNoSuchOrder = errors.New("order does not exist")

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) request(ctx context.Context, method, base, path string, q url.Values, signed bool, out any) error {
	if q == nil {
		q = url.Values{}
	}
	query := q.Encode()
	if signed {
		if c.Key == "" || c.Secret == "" {
			return errors.New("set BINANCE_API_KEY and BINANCE_API_SECRET in .env")
		}
		c.mu.Lock()
		ts := c.now().Add(c.offset).UnixMilli()
		c.mu.Unlock()
		q.Set("recvWindow", "5000")
		q.Set("timestamp", strconv.FormatInt(ts, 10))
		query = q.Encode()
		query += "&signature=" + Sign(c.Secret, query)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path+"?"+query, nil)
	if err != nil {
		return err
	}
	if signed {
		req.Header.Set("X-MBX-APIKEY", c.Key)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return &netError{fmt.Errorf("%s %s: %w", method, path, err)}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return &netError{fmt.Errorf("%s %s: %w", method, path, err)}
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		_ = json.Unmarshal(body, &e)
		return &APIError{HTTPStatus: resp.StatusCode, Code: e.Code, Msg: e.Msg}
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return &netError{fmt.Errorf("%s %s: unreadable response: %w", method, path, err)}
		}
	}
	return nil
}

// netError is a transport failure or an unreadable reply.
type netError struct{ err error }

func (e *netError) Error() string { return e.err.Error() }
func (e *netError) Unwrap() error { return e.err }

// signed retries once after resynchronising the clock when Binance rejects
// the timestamp; such requests never reach the matching engine.
func (c *Client) signed(ctx context.Context, method, path string, q url.Values, out any) error {
	err := c.request(ctx, method, c.BaseURL, path, cloneValues(q), true, out)
	var api *APIError
	if errors.As(err, &api) && api.Code == -1021 {
		if serr := c.SyncTime(ctx); serr != nil {
			return err
		}
		return c.request(ctx, method, c.BaseURL, path, cloneValues(q), true, out)
	}
	return err
}

func cloneValues(q url.Values) url.Values {
	out := url.Values{}
	for k, v := range q {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// Sign is the HMAC-SHA256 signature Binance expects over the query string.
func Sign(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// SyncTime measures the offset between the local clock and the server's.
func (c *Client) SyncTime(ctx context.Context) error {
	start := c.now()
	var out struct {
		ServerTime int64 `json:"serverTime"`
	}
	if err := c.request(ctx, http.MethodGet, c.BaseURL, "/api/v3/time", nil, false, &out); err != nil {
		return err
	}
	end := c.now()
	mid := start.Add(end.Sub(start) / 2)
	c.mu.Lock()
	c.offset = time.UnixMilli(out.ServerTime).Sub(mid)
	c.mu.Unlock()
	return nil
}

// Klines returns candles oldest first, ending with the one still forming.
// A non-zero start returns candles from that time instead.
func (c *Client) Klines(ctx context.Context, symbol, interval string, limit int, start time.Time) ([]market.Candle, error) {
	q := url.Values{"symbol": {symbol}, "interval": {interval}, "limit": {strconv.Itoa(limit)}}
	if !start.IsZero() {
		q.Set("startTime", strconv.FormatInt(start.UnixMilli(), 10))
	}
	var rows [][]json.RawMessage
	if err := c.request(ctx, http.MethodGet, c.DataURL, "/api/v3/klines", q, false, &rows); err != nil {
		return nil, err
	}
	out := make([]market.Candle, 0, len(rows))
	for _, r := range rows {
		if len(r) < 8 {
			return nil, fmt.Errorf("%s klines: short row", symbol)
		}
		var openMs, closeMs int64
		var o, h, l, cl, v, qv string
		for i, dst := range []any{&openMs, &o, &h, &l, &cl, &v, &closeMs, &qv} {
			if err := json.Unmarshal(r[i], dst); err != nil {
				return nil, fmt.Errorf("%s klines: %w", symbol, err)
			}
		}
		fl := func(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }
		k := market.Candle{Open: time.UnixMilli(openMs).UTC(), O: fl(o), H: fl(h), L: fl(l), C: fl(cl), Volume: fl(v), QuoteVolume: fl(qv), CloseTime: time.UnixMilli(closeMs).UTC()}
		if k.O <= 0 || k.H <= 0 || k.L <= 0 || k.C <= 0 {
			return nil, fmt.Errorf("%s klines: invalid price", symbol)
		}
		out = append(out, k)
	}
	return out, nil
}

// Prices returns the latest trade price of each symbol where orders fill:
// on the testnet that is the testnet's own book, not the live market.
func (c *Client) Prices(ctx context.Context, symbols []string) (map[string]num.Dec, error) {
	list, _ := json.Marshal(symbols)
	var rows []struct {
		Symbol string  `json:"symbol"`
		Price  num.Dec `json:"price"`
	}
	if err := c.request(ctx, http.MethodGet, c.BaseURL, "/api/v3/ticker/price", url.Values{"symbols": {string(list)}}, false, &rows); err != nil {
		return nil, err
	}
	out := map[string]num.Dec{}
	for _, r := range rows {
		if r.Price.Sign() <= 0 {
			return nil, fmt.Errorf("%s: invalid price", r.Symbol)
		}
		out[r.Symbol] = r.Price
	}
	for _, s := range symbols {
		if _, ok := out[s]; !ok {
			return nil, fmt.Errorf("no price for %s", s)
		}
	}
	return out, nil
}

// Symbol holds the trading rules that decide whether an order is accepted.
type Symbol struct {
	Name        string  `json:"symbol"`
	Base        string  `json:"base"`
	Quote       string  `json:"quote"`
	Trading     bool    `json:"trading"`
	QuoteMarket bool    `json:"quote_market_orders"`
	StepSize    num.Dec `json:"step_size"`
	MinQty      num.Dec `json:"min_qty"`
	MaxQty      num.Dec `json:"max_qty"`
	MinNotional num.Dec `json:"min_notional"`
}

func (c *Client) Symbols(ctx context.Context, symbols []string) (map[string]Symbol, error) {
	list, _ := json.Marshal(symbols)
	var info struct {
		Symbols []struct {
			Symbol      string `json:"symbol"`
			Status      string `json:"status"`
			Base        string `json:"baseAsset"`
			Quote       string `json:"quoteAsset"`
			QuoteMarket bool   `json:"quoteOrderQtyMarketAllowed"`
			Filters     []struct {
				Type          string  `json:"filterType"`
				MinQty        num.Dec `json:"minQty"`
				MaxQty        num.Dec `json:"maxQty"`
				StepSize      num.Dec `json:"stepSize"`
				MinNotional   num.Dec `json:"minNotional"`
				ApplyToMarket *bool   `json:"applyToMarket"`
				ApplyMin      *bool   `json:"applyMinToMarket"`
			} `json:"filters"`
		} `json:"symbols"`
	}
	if err := c.request(ctx, http.MethodGet, c.BaseURL, "/api/v3/exchangeInfo", url.Values{"symbols": {string(list)}}, false, &info); err != nil {
		return nil, err
	}
	out := map[string]Symbol{}
	for _, s := range info.Symbols {
		sym := Symbol{Name: s.Symbol, Base: s.Base, Quote: s.Quote, Trading: s.Status == "TRADING", QuoteMarket: s.QuoteMarket}
		for _, f := range s.Filters {
			switch f.Type {
			case "LOT_SIZE":
				sym.StepSize, sym.MinQty, sym.MaxQty = f.StepSize, f.MinQty, f.MaxQty
			case "MARKET_LOT_SIZE":
				if f.MaxQty.Sign() > 0 && (sym.MaxQty.IsZero() || f.MaxQty.Less(sym.MaxQty)) {
					sym.MaxQty = f.MaxQty
				}
				if f.StepSize.Sign() > 0 {
					sym.StepSize = num.Max(sym.StepSize, f.StepSize)
				}
			case "NOTIONAL", "MIN_NOTIONAL":
				if (f.ApplyMin != nil && *f.ApplyMin) || (f.ApplyToMarket != nil && *f.ApplyToMarket) {
					sym.MinNotional = num.Max(sym.MinNotional, f.MinNotional)
				}
			}
		}
		out[s.Symbol] = sym
	}
	for _, s := range symbols {
		if _, ok := out[s]; !ok {
			return nil, fmt.Errorf("Binance does not list %s", s)
		}
	}
	return out, nil
}

type Balance struct {
	Free   num.Dec `json:"free"`
	Locked num.Dec `json:"locked"`
}

func (b Balance) Total() num.Dec { return b.Free.Add(b.Locked) }

type Account struct {
	CanTrade bool
	Taker    num.Dec
	Balances map[string]Balance
}

func (c *Client) Account(ctx context.Context) (Account, error) {
	var out struct {
		CanTrade bool `json:"canTrade"`
		Rates    struct {
			Taker num.Dec `json:"taker"`
		} `json:"commissionRates"`
		Balances []struct {
			Asset  string  `json:"asset"`
			Free   num.Dec `json:"free"`
			Locked num.Dec `json:"locked"`
		} `json:"balances"`
	}
	if err := c.signed(ctx, http.MethodGet, "/api/v3/account", url.Values{"omitZeroBalances": {"true"}}, &out); err != nil {
		return Account{}, err
	}
	a := Account{CanTrade: out.CanTrade, Taker: out.Rates.Taker, Balances: map[string]Balance{}}
	for _, b := range out.Balances {
		a.Balances[b.Asset] = Balance{b.Free, b.Locked}
	}
	return a, nil
}

// Permissions are the API key's own restrictions, available on the live
// venue only.
type Permissions struct {
	IPRestrict        bool  `json:"ipRestrict"`
	EnableWithdrawals *bool `json:"enableWithdrawals"`
	EnableSpotTrading *bool `json:"enableSpotAndMarginTrading"`
}

func (c *Client) Permissions(ctx context.Context) (Permissions, error) {
	var p Permissions
	err := c.signed(ctx, http.MethodGet, "/sapi/v1/account/apiRestrictions", nil, &p)
	return p, err
}

type OrderRequest struct {
	Symbol   string
	Side     string // BUY or SELL
	Quantity num.Dec
	QuoteQty num.Dec
	ClientID string
}

type Fill struct {
	Price           num.Dec `json:"price"`
	Qty             num.Dec `json:"qty"`
	QuoteQty        num.Dec `json:"quote_qty"`
	Commission      num.Dec `json:"commission"`
	CommissionAsset string  `json:"commission_asset"`
}

type Order struct {
	Symbol      string    `json:"symbol"`
	OrderID     int64     `json:"order_id"`
	ClientID    string    `json:"client_id"`
	Side        string    `json:"side"`
	Status      string    `json:"status"`
	ExecutedQty num.Dec   `json:"executed_qty"`
	QuoteQty    num.Dec   `json:"quote_qty"`
	Time        time.Time `json:"time"`
	Fills       []Fill    `json:"fills,omitempty"`
}

// Final reports whether the order can no longer fill further.
func (o Order) Final() bool {
	return o.Status != "NEW" && o.Status != "PARTIALLY_FILLED" && o.Status != "PENDING_NEW"
}

type orderJSON struct {
	Symbol        string  `json:"symbol"`
	OrderID       int64   `json:"orderId"`
	ClientOrderID string  `json:"clientOrderId"`
	TransactTime  int64   `json:"transactTime"`
	UpdateTime    int64   `json:"updateTime"`
	Side          string  `json:"side"`
	Status        string  `json:"status"`
	ExecutedQty   num.Dec `json:"executedQty"`
	QuoteQty      string  `json:"cummulativeQuoteQty"`
	Fills         []struct {
		Price           num.Dec `json:"price"`
		Qty             num.Dec `json:"qty"`
		Commission      num.Dec `json:"commission"`
		CommissionAsset string  `json:"commissionAsset"`
	} `json:"fills"`
}

func (o orderJSON) order() Order {
	ms := o.TransactTime
	if ms == 0 {
		ms = o.UpdateTime
	}
	out := Order{Symbol: o.Symbol, OrderID: o.OrderID, ClientID: o.ClientOrderID, Side: o.Side, Status: o.Status, ExecutedQty: o.ExecutedQty, Time: time.UnixMilli(ms).UTC()}
	if q, err := num.Parse(o.QuoteQty); err == nil && q.Sign() >= 0 {
		out.QuoteQty = q
	}
	for _, f := range o.Fills {
		out.Fills = append(out.Fills, Fill{Price: f.Price, Qty: f.Qty, QuoteQty: f.Price.Mul(f.Qty), Commission: f.Commission, CommissionAsset: f.CommissionAsset})
	}
	return out
}

// PlaceMarket sends a market order and returns its fills. Errors wrapping
// ErrUnknown mean the order may exist: query it before acting again.
func (c *Client) PlaceMarket(ctx context.Context, r OrderRequest) (Order, error) {
	q, err := r.values()
	if err != nil {
		return Order{}, err
	}
	q.Set("newOrderRespType", "FULL")
	var out orderJSON
	err = c.signed(ctx, http.MethodPost, "/api/v3/order", q, &out)
	if unknown(err) {
		return Order{}, fmt.Errorf("%w: %v", ErrUnknown, err)
	}
	if err != nil {
		return Order{}, err
	}
	return out.order(), nil
}

// TestOrder validates an order, the signature and the key without trading.
func (c *Client) TestOrder(ctx context.Context, r OrderRequest) error {
	q, err := r.values()
	if err != nil {
		return err
	}
	return c.signed(ctx, http.MethodPost, "/api/v3/order/test", q, nil)
}

func (r OrderRequest) values() (url.Values, error) {
	if r.Side != "BUY" && r.Side != "SELL" {
		return nil, fmt.Errorf("invalid side %q", r.Side)
	}
	q := url.Values{"symbol": {r.Symbol}, "side": {r.Side}, "type": {"MARKET"}, "newClientOrderId": {r.ClientID}}
	switch {
	case r.Quantity.Sign() > 0 && r.QuoteQty.IsZero():
		q.Set("quantity", r.Quantity.String())
	case r.QuoteQty.Sign() > 0 && r.Quantity.IsZero():
		q.Set("quoteOrderQty", r.QuoteQty.String())
	default:
		return nil, errors.New("set exactly one of quantity and quote quantity")
	}
	return q, nil
}

// unknown reports failures after which an order may have executed.
func unknown(err error) bool {
	var ne *netError
	var api *APIError
	switch {
	case err == nil:
		return false
	case errors.As(err, &ne):
		return true
	case errors.As(err, &api):
		return api.HTTPStatus >= 500 || api.Code == -1000 || api.Code == -1006 || api.Code == -1007
	}
	return false
}

func (c *Client) QueryOrder(ctx context.Context, symbol, clientID string) (Order, error) {
	var out orderJSON
	err := c.signed(ctx, http.MethodGet, "/api/v3/order", url.Values{"symbol": {symbol}, "origClientOrderId": {clientID}}, &out)
	var api *APIError
	if errors.As(err, &api) && api.Code == -2013 {
		return Order{}, ErrNoSuchOrder
	}
	if err != nil {
		return Order{}, err
	}
	return out.order(), nil
}

// OrderFills lists an order's trades with their commissions.
func (c *Client) OrderFills(ctx context.Context, symbol string, orderID int64) ([]Fill, error) {
	var rows []struct {
		Price           num.Dec `json:"price"`
		Qty             num.Dec `json:"qty"`
		QuoteQty        num.Dec `json:"quoteQty"`
		Commission      num.Dec `json:"commission"`
		CommissionAsset string  `json:"commissionAsset"`
	}
	if err := c.signed(ctx, http.MethodGet, "/api/v3/myTrades", url.Values{"symbol": {symbol}, "orderId": {strconv.FormatInt(orderID, 10)}}, &rows); err != nil {
		return nil, err
	}
	out := make([]Fill, 0, len(rows))
	for _, r := range rows {
		out = append(out, Fill{Price: r.Price, Qty: r.Qty, QuoteQty: r.QuoteQty, Commission: r.Commission, CommissionAsset: r.CommissionAsset})
	}
	return out, nil
}
