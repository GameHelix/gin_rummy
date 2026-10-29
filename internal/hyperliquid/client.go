// Package hyperliquid is a client for the Hyperliquid perpetuals exchange:
// market data, an account's state, and orders signed by an agent wallet
// the account has approved. An agent wallet can trade for the account but
// cannot move funds out of it.
package hyperliquid

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	MainnetURL = "https://api.hyperliquid.xyz"
	TestnetURL = "https://api.hyperliquid-testnet.xyz"
)

// Client talks to one exchange. Reads need no Signer; orders do.
type Client struct {
	HTTP    *http.Client
	BaseURL string
	Signer  *Signer
	Now     func() time.Time
	Logf    func(format string, args ...any)

	mu    sync.Mutex
	nonce uint64
}

// Mainnet says whether orders are real money.
func (c *Client) Mainnet() bool { return c.BaseURL == MainnetURL }

// APIError is a request the exchange refused.
type APIError struct {
	HTTPStatus int
	Msg        string
}

func (e *APIError) Error() string {
	if e.Msg == "" {
		return fmt.Sprintf("Hyperliquid HTTP %d", e.HTTPStatus)
	}
	return "Hyperliquid: " + e.Msg
}

// ErrUnknown marks an order request that may or may not have executed: the
// connection failed after sending or the reply was unreadable. Look the
// order up by its client ID before doing anything else.
var ErrUnknown = errors.New("order outcome unknown")

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) post(ctx context.Context, path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return &APIError{HTTPStatus: resp.StatusCode, Msg: strings.TrimSpace(string(data))}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s: unreadable reply: %w", path, err)
		}
	}
	return nil
}

// info reads public or account data, retrying on transient failures.
func (c *Client) info(ctx context.Context, body any, out any) error {
	var err error
	for attempt, wait := range []time.Duration{0, time.Second, 3 * time.Second} {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		if err = c.post(ctx, "/info", body, out); err == nil {
			return nil
		}
		var api *APIError
		if errors.As(err, &api) && api.HTTPStatus < 500 && api.HTTPStatus != http.StatusTooManyRequests {
			return err
		}
	}
	return err
}

// Asset is one perpetual market. Index is what orders name it by.
type Asset struct {
	Index        int
	Name         string
	SzDecimals   int
	MaxLeverage  int
	OnlyIsolated bool
	Delisted     bool
}

type assetJSON struct {
	Name         string `json:"name"`
	SzDecimals   int    `json:"szDecimals"`
	MaxLeverage  int    `json:"maxLeverage"`
	OnlyIsolated bool   `json:"onlyIsolated"`
	IsDelisted   bool   `json:"isDelisted"`
}

func assets(rows []assetJSON) []Asset {
	out := make([]Asset, len(rows))
	for i, r := range rows {
		out[i] = Asset{Index: i, Name: r.Name, SzDecimals: r.SzDecimals, MaxLeverage: r.MaxLeverage, OnlyIsolated: r.OnlyIsolated, Delisted: r.IsDelisted}
	}
	return out
}

// Assets lists every perpetual, delisted ones included, in index order.
func (c *Client) Assets(ctx context.Context) ([]Asset, error) {
	var meta struct {
		Universe []assetJSON `json:"universe"`
	}
	if err := c.info(ctx, map[string]string{"type": "meta"}, &meta); err != nil {
		return nil, err
	}
	return assets(meta.Universe), nil
}

// Market is the live state of one perpetual.
type Market struct {
	Funding      float64 // the current hourly rate
	OpenInterest float64 // in the base asset
	PrevDayPx    float64
	DayVolumeUSD float64
	MarkPx       float64
	MidPx        float64
	OraclePx     float64
}

type marketJSON struct {
	Funding      num `json:"funding"`
	OpenInterest num `json:"openInterest"`
	PrevDayPx    num `json:"prevDayPx"`
	DayNtlVlm    num `json:"dayNtlVlm"`
	MarkPx       num `json:"markPx"`
	MidPx        num `json:"midPx"`
	OraclePx     num `json:"oraclePx"`
}

// num is a number the exchange sends as a string.
type num float64

func (n *num) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	*n = num(f)
	return err
}

// Markets returns every perpetual with its live state, aligned by index.
func (c *Client) Markets(ctx context.Context) ([]Asset, []Market, error) {
	var raw []json.RawMessage
	if err := c.info(ctx, map[string]string{"type": "metaAndAssetCtxs"}, &raw); err != nil {
		return nil, nil, err
	}
	var meta struct {
		Universe []assetJSON `json:"universe"`
	}
	var ctxs []marketJSON
	if len(raw) != 2 || json.Unmarshal(raw[0], &meta) != nil || json.Unmarshal(raw[1], &ctxs) != nil || len(ctxs) != len(meta.Universe) {
		return nil, nil, errors.New("metaAndAssetCtxs: unexpected reply")
	}
	markets := make([]Market, len(ctxs))
	for i, m := range ctxs {
		markets[i] = Market{Funding: float64(m.Funding), OpenInterest: float64(m.OpenInterest), PrevDayPx: float64(m.PrevDayPx), DayVolumeUSD: float64(m.DayNtlVlm), MarkPx: float64(m.MarkPx), MidPx: float64(m.MidPx), OraclePx: float64(m.OraclePx)}
	}
	return assets(meta.Universe), markets, nil
}

// Mids returns the mid price of every perpetual by name.
func (c *Client) Mids(ctx context.Context) (map[string]float64, error) {
	var raw map[string]num
	if err := c.info(ctx, map[string]string{"type": "allMids"}, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(raw))
	for k, v := range raw {
		out[k] = float64(v)
	}
	return out, nil
}

// Candle is one bar. Volume is in the base asset.
type Candle struct {
	Open   time.Time
	Close  time.Time
	O      float64
	H      float64
	L      float64
	C      float64
	Volume float64
	Trades int
}

// Candles returns bars from start to end at most 5000 back. Intervals are
// 1m 3m 5m 15m 30m 1h 2h 4h 8h 12h 1d 3d 1w 1M.
func (c *Client) Candles(ctx context.Context, coin, interval string, start, end time.Time) ([]Candle, error) {
	var rows []struct {
		T  int64 `json:"t"`
		TC int64 `json:"T"`
		O  num   `json:"o"`
		H  num   `json:"h"`
		L  num   `json:"l"`
		C  num   `json:"c"`
		V  num   `json:"v"`
		N  int   `json:"n"`
	}
	req := map[string]any{"type": "candleSnapshot", "req": map[string]any{"coin": coin, "interval": interval, "startTime": start.UnixMilli(), "endTime": end.UnixMilli()}}
	if err := c.info(ctx, req, &rows); err != nil {
		return nil, err
	}
	out := make([]Candle, 0, len(rows))
	for _, r := range rows {
		if r.O <= 0 || r.H <= 0 || r.L <= 0 || r.C <= 0 {
			return nil, fmt.Errorf("%s candles: invalid price", coin)
		}
		out = append(out, Candle{Open: time.UnixMilli(r.T).UTC(), Close: time.UnixMilli(r.TC).UTC(), O: float64(r.O), H: float64(r.H), L: float64(r.L), C: float64(r.C), Volume: float64(r.V), Trades: r.N})
	}
	return out, nil
}

// Position is an open perpetual position. Size is negative when short.
type Position struct {
	Coin          string
	Size          float64
	EntryPx       float64
	Value         float64
	UnrealizedPnl float64
	LiquidationPx float64
	MarginUsed    float64
	Leverage      int
	Cross         bool
}

// Account is a perpetuals account as the exchange sees it.
type Account struct {
	Value        float64 // equity, in USDC
	MarginUsed   float64
	Withdrawable float64
	Positions    []Position
	Time         time.Time
}

// Account reads the state of user's perpetuals account. The address is
// the account's own, never the agent's.
func (c *Client) Account(ctx context.Context, user string) (Account, error) {
	var raw struct {
		MarginSummary struct {
			AccountValue    num `json:"accountValue"`
			TotalMarginUsed num `json:"totalMarginUsed"`
		} `json:"marginSummary"`
		Withdrawable   num `json:"withdrawable"`
		AssetPositions []struct {
			Position struct {
				Coin          string `json:"coin"`
				Szi           num    `json:"szi"`
				EntryPx       num    `json:"entryPx"`
				PositionValue num    `json:"positionValue"`
				UnrealizedPnl num    `json:"unrealizedPnl"`
				LiquidationPx num    `json:"liquidationPx"`
				MarginUsed    num    `json:"marginUsed"`
				Leverage      struct {
					Type  string `json:"type"`
					Value int    `json:"value"`
				} `json:"leverage"`
			} `json:"position"`
		} `json:"assetPositions"`
		Time int64 `json:"time"`
	}
	if err := c.info(ctx, map[string]string{"type": "clearinghouseState", "user": strings.ToLower(user)}, &raw); err != nil {
		return Account{}, err
	}
	a := Account{Value: float64(raw.MarginSummary.AccountValue), MarginUsed: float64(raw.MarginSummary.TotalMarginUsed), Withdrawable: float64(raw.Withdrawable), Time: time.UnixMilli(raw.Time).UTC()}
	for _, p := range raw.AssetPositions {
		q := p.Position
		if q.Szi == 0 {
			continue
		}
		a.Positions = append(a.Positions, Position{Coin: q.Coin, Size: float64(q.Szi), EntryPx: float64(q.EntryPx), Value: float64(q.PositionValue), UnrealizedPnl: float64(q.UnrealizedPnl), LiquidationPx: float64(q.LiquidationPx), MarginUsed: float64(q.MarginUsed), Leverage: q.Leverage.Value, Cross: q.Leverage.Type == "cross"})
	}
	return a, nil
}

// OpenOrder is an order resting on the book or waiting for its trigger.
type OpenOrder struct {
	Coin             string
	OID              int64
	Cloid            string
	Buy              bool
	Price            float64
	Size             float64 // what is left
	OrigSize         float64
	Type             string // Limit, Stop Market, Take Profit Market, ...
	Trigger          bool
	TriggerPx        float64
	TriggerCondition string
	ReduceOnly       bool
	PositionTPSL     bool
	Time             time.Time
}

type openOrderJSON struct {
	Coin             string `json:"coin"`
	Side             string `json:"side"`
	LimitPx          num    `json:"limitPx"`
	Sz               num    `json:"sz"`
	OrigSz           num    `json:"origSz"`
	OID              int64  `json:"oid"`
	Cloid            string `json:"cloid"`
	Timestamp        int64  `json:"timestamp"`
	OrderType        string `json:"orderType"`
	IsTrigger        bool   `json:"isTrigger"`
	TriggerPx        num    `json:"triggerPx"`
	TriggerCondition string `json:"triggerCondition"`
	ReduceOnly       bool   `json:"reduceOnly"`
	IsPositionTpsl   bool   `json:"isPositionTpsl"`
}

func (o openOrderJSON) order() OpenOrder {
	return OpenOrder{Coin: o.Coin, OID: o.OID, Cloid: o.Cloid, Buy: o.Side == "B", Price: float64(o.LimitPx), Size: float64(o.Sz), OrigSize: float64(o.OrigSz), Type: o.OrderType, Trigger: o.IsTrigger, TriggerPx: float64(o.TriggerPx), TriggerCondition: o.TriggerCondition, ReduceOnly: o.ReduceOnly, PositionTPSL: o.IsPositionTpsl, Time: time.UnixMilli(o.Timestamp).UTC()}
}

// OpenOrders lists user's resting and trigger orders.
func (c *Client) OpenOrders(ctx context.Context, user string) ([]OpenOrder, error) {
	var rows []openOrderJSON
	if err := c.info(ctx, map[string]string{"type": "frontendOpenOrders", "user": strings.ToLower(user)}, &rows); err != nil {
		return nil, err
	}
	out := make([]OpenOrder, len(rows))
	for i, r := range rows {
		out[i] = r.order()
	}
	return out, nil
}

// Fill is one execution.
type Fill struct {
	Coin          string
	OID           int64
	Cloid         string
	TID           int64
	Buy           bool
	Price         float64
	Size          float64
	Fee           float64 // in USDC; negative is a rebate
	BuilderFee    float64
	ClosedPnl     float64
	StartPosition float64
	Dir           string // Open Long, Close Short, ...
	Hash          string
	Crossed       bool // taker
	Time          time.Time
}

// Fills returns user's executions since a time, newest last, at most 2000.
func (c *Client) Fills(ctx context.Context, user string, since time.Time) ([]Fill, error) {
	var rows []struct {
		Coin          string `json:"coin"`
		Px            num    `json:"px"`
		Sz            num    `json:"sz"`
		Side          string `json:"side"`
		Time          int64  `json:"time"`
		StartPosition num    `json:"startPosition"`
		Dir           string `json:"dir"`
		ClosedPnl     num    `json:"closedPnl"`
		Hash          string `json:"hash"`
		OID           int64  `json:"oid"`
		Cloid         string `json:"cloid"`
		Crossed       bool   `json:"crossed"`
		Fee           num    `json:"fee"`
		BuilderFee    num    `json:"builderFee"`
		TID           int64  `json:"tid"`
	}
	req := map[string]any{"type": "userFillsByTime", "user": strings.ToLower(user), "startTime": since.UnixMilli()}
	if err := c.info(ctx, req, &rows); err != nil {
		return nil, err
	}
	out := make([]Fill, len(rows))
	for i, r := range rows {
		out[i] = Fill{Coin: r.Coin, OID: r.OID, Cloid: r.Cloid, TID: r.TID, Buy: r.Side == "B", Price: float64(r.Px), Size: float64(r.Sz), Fee: float64(r.Fee), BuilderFee: float64(r.BuilderFee), ClosedPnl: float64(r.ClosedPnl), StartPosition: float64(r.StartPosition), Dir: r.Dir, Hash: r.Hash, Crossed: r.Crossed, Time: time.UnixMilli(r.Time).UTC()}
	}
	return out, nil
}

// OrderState is what became of an order: open, filled, canceled,
// triggered, rejected, marginCanceled and the like.
type OrderState struct {
	Status string
	Order  OpenOrder
	At     time.Time
}

// OrderStatus looks an order up by its exchange ID or, with oid 0, by its
// client ID. The second result is false when the exchange never saw it.
func (c *Client) OrderStatus(ctx context.Context, user string, oid int64, cloid string) (OrderState, bool, error) {
	req := map[string]any{"type": "orderStatus", "user": strings.ToLower(user)}
	if oid != 0 {
		req["oid"] = oid
	} else {
		req["oid"] = cloid
	}
	var raw struct {
		Status string `json:"status"`
		Order  struct {
			Order           openOrderJSON `json:"order"`
			Status          string        `json:"status"`
			StatusTimestamp int64         `json:"statusTimestamp"`
		} `json:"order"`
	}
	if err := c.info(ctx, req, &raw); err != nil {
		return OrderState{}, false, err
	}
	if raw.Status != "order" {
		return OrderState{}, false, nil
	}
	return OrderState{Status: raw.Order.Status, Order: raw.Order.Order.order(), At: time.UnixMilli(raw.Order.StatusTimestamp).UTC()}, true, nil
}

// MaxBuilderFee is the rate user has approved for builder, in tenths of a
// basis point; 0 means no approval.
func (c *Client) MaxBuilderFee(ctx context.Context, user, builder string) (int, error) {
	var fee int
	err := c.info(ctx, map[string]string{"type": "maxBuilderFee", "user": strings.ToLower(user), "builder": strings.ToLower(builder)}, &fee)
	return fee, err
}

// Role says what an address is: user, agent (acting for User), vault,
// subAccount, or missing.
type Role struct {
	Role string
	User string
}

// Role looks an address up; for an agent wallet it names the account.
func (c *Client) Role(ctx context.Context, address string) (Role, error) {
	var raw struct {
		Role string `json:"role"`
		Data struct {
			User   string `json:"user"`
			Master string `json:"master"`
		} `json:"data"`
	}
	if err := c.info(ctx, map[string]string{"type": "userRole", "user": strings.ToLower(address)}, &raw); err != nil {
		return Role{}, err
	}
	r := Role{Role: raw.Role, User: raw.Data.User}
	if r.User == "" {
		r.User = raw.Data.Master
	}
	return r, nil
}

func (c *Client) nextNonce() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := uint64(c.now().UnixMilli())
	if n <= c.nonce {
		n = c.nonce + 1
	}
	c.nonce = n
	return n
}

type exchangeReply struct {
	Status   string          `json:"status"`
	Response json.RawMessage `json:"response"`
}

// exchange signs an action with the agent wallet and sends it.
func (c *Client) exchange(ctx context.Context, action omap) (json.RawMessage, error) {
	if c.Signer == nil {
		return nil, errors.New("no wallet to sign with")
	}
	nonce := c.nextNonce()
	sig, err := c.Signer.signL1(action, "", nonce, nil, c.Mainnet())
	if err != nil {
		return nil, err
	}
	return c.submit(ctx, action, nonce, sig)
}

func (c *Client) submit(ctx context.Context, action any, nonce uint64, sig Signature) (json.RawMessage, error) {
	body := map[string]any{"action": action, "nonce": nonce, "signature": sig}
	var reply exchangeReply
	if err := c.post(ctx, "/exchange", body, &reply); err != nil {
		var api *APIError
		if errors.As(err, &api) && api.HTTPStatus < 500 {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", ErrUnknown, err)
	}
	if reply.Status != "ok" {
		msg := strings.Trim(string(reply.Response), `"`)
		return nil, &APIError{HTTPStatus: http.StatusOK, Msg: msg}
	}
	return reply.Response, nil
}

// Placed is the result of one order in a batch.
type Placed struct {
	OID     int64
	Resting bool
	Filled  float64 // size filled at once
	AvgPx   float64
	Error   string // the exchange's reason, when it refused this order
}

// Place sends orders in one batch and returns a result per order in the
// same order. An error is for the whole batch; a refused single order has
// its reason in Error instead.
func (c *Client) Place(ctx context.Context, orders []Order, grouping string, b *Builder) ([]Placed, error) {
	if len(orders) == 0 {
		return nil, errors.New("no orders")
	}
	raw, err := c.exchange(ctx, orderAction(orders, grouping, b))
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data struct {
			Statuses []struct {
				Resting *struct {
					OID int64 `json:"oid"`
				} `json:"resting"`
				Filled *struct {
					OID     int64 `json:"oid"`
					TotalSz num   `json:"totalSz"`
					AvgPx   num   `json:"avgPx"`
				} `json:"filled"`
				Error string `json:"error"`
			} `json:"statuses"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || len(resp.Data.Statuses) != len(orders) {
		return nil, fmt.Errorf("%w: unexpected reply %s", ErrUnknown, raw)
	}
	out := make([]Placed, len(orders))
	for i, s := range resp.Data.Statuses {
		switch {
		case s.Filled != nil:
			out[i] = Placed{OID: s.Filled.OID, Filled: float64(s.Filled.TotalSz), AvgPx: float64(s.Filled.AvgPx)}
		case s.Resting != nil:
			out[i] = Placed{OID: s.Resting.OID, Resting: true}
		default:
			out[i] = Placed{Error: s.Error}
			if s.Error == "" {
				out[i].Error = "refused without a reason"
			}
		}
	}
	return out, nil
}

func (c *Client) cancelWith(ctx context.Context, action omap) error {
	raw, err := c.exchange(ctx, action)
	if err != nil {
		return err
	}
	var resp struct {
		Data struct {
			Statuses []json.RawMessage `json:"statuses"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || len(resp.Data.Statuses) != 1 {
		return fmt.Errorf("cancel: unexpected reply %s", raw)
	}
	if string(resp.Data.Statuses[0]) == `"success"` {
		return nil
	}
	var st struct {
		Error string `json:"error"`
	}
	json.Unmarshal(resp.Data.Statuses[0], &st)
	return &APIError{HTTPStatus: http.StatusOK, Msg: st.Error}
}

// Cancel removes an order by its exchange ID.
func (c *Client) Cancel(ctx context.Context, asset int, oid int64) error {
	return c.cancelWith(ctx, cancelAction(asset, oid))
}

// CancelByCloid removes an order by its client ID.
func (c *Client) CancelByCloid(ctx context.Context, asset int, cloid string) error {
	return c.cancelWith(ctx, cancelByCloidAction(asset, cloid))
}

// SetLeverage sets the account's leverage on an asset, cross or isolated.
func (c *Client) SetLeverage(ctx context.Context, asset int, cross bool, leverage int) error {
	_, err := c.exchange(ctx, leverageAction(asset, cross, leverage))
	return err
}

// UserAction is an account action prepared for the account's own wallet
// to sign: the typed data a wallet shows and signs, and the action to
// submit with that signature. Nothing in it is secret.
type UserAction struct {
	Action    omap      `json:"action"`
	Nonce     uint64    `json:"nonce"`
	TypedData typedData `json:"typed_data"`
	primary   string
	fields    []field
}

func (c *Client) chain() string {
	if c.Mainnet() {
		return "Mainnet"
	}
	return "Testnet"
}

var (
	approveAgentFields      = []field{{"hyperliquidChain", "string"}, {"agentAddress", "address"}, {"agentName", "string"}, {"nonce", "uint64"}}
	approveBuilderFeeFields = []field{{"hyperliquidChain", "string"}, {"maxFeeRate", "string"}, {"builder", "address"}, {"nonce", "uint64"}}
)

// ApproveAgent prepares the approval that lets wallet agent trade for the
// signing account. An account keeps one unnamed agent and up to three
// named ones; a name may end in "valid_until MILLIS" to expire.
func (c *Client) ApproveAgent(agent, name string) UserAction {
	nonce := c.nextNonce()
	msg := omap{{"type", "approveAgent"}, {"hyperliquidChain", c.chain()}, {"signatureChainId", signatureChainID},
		{"agentAddress", strings.ToLower(agent)}, {"agentName", name}, {"nonce", nonce}}
	return c.userAction("HyperliquidTransaction:ApproveAgent", approveAgentFields, msg, nonce)
}

// ApproveBuilderFee prepares the approval of a builder's maximum fee on the
// signing account's orders, as a percent string such as "0.01%".
func (c *Client) ApproveBuilderFee(builder, maxFeeRate string) UserAction {
	nonce := c.nextNonce()
	msg := omap{{"type", "approveBuilderFee"}, {"hyperliquidChain", c.chain()}, {"signatureChainId", signatureChainID},
		{"maxFeeRate", maxFeeRate}, {"builder", strings.ToLower(builder)}, {"nonce", nonce}}
	return c.userAction("HyperliquidTransaction:ApproveBuilderFee", approveBuilderFeeFields, msg, nonce)
}

func (c *Client) userAction(primary string, fields []field, msg omap, nonce uint64) UserAction {
	return UserAction{Action: msg, Nonce: nonce, TypedData: userTypedData(primary, fields, msg), primary: primary, fields: fields}
}

// Submit sends a user action with the signature the account's wallet gave.
func (c *Client) Submit(ctx context.Context, a UserAction, sig Signature) error {
	_, err := c.submit(ctx, a.Action, a.Nonce, sig)
	return err
}

// Sign signs a user action with a wallet the caller holds, for tests and
// an operator's own account.
func (a UserAction) Sign(s *Signer) (Signature, error) {
	return s.signUser(a.primary, a.fields, a.Action)
}
