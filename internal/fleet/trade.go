package fleet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/google/uuid"

	"github.com/candylario/trader/internal/hyperliquid"
	"github.com/candylario/trader/internal/pg"
)

// A trade is one tap: a market order with its stop-loss, and a take-profit
// when asked, sent to the exchange together, so the position is protected
// from its first moment even if the engine goes down right after.

// TradeRequest opens a position. USD is the position's size; Leverage
// sets how much of it is margin. The stop and the take-profit are
// fractions of the entry price: 0.10 is 10% against the entry.
type TradeRequest struct {
	Coin          string
	Side          string // long or short
	USD           float64
	Leverage      int
	StopLossPct   float64
	TakeProfitPct float64
	SlippagePct   float64 // how far from the mid price the market order may fill; 0.01 by default
}

// Trade is an opened position with its orders on the exchange.
type Trade struct {
	Coin          string
	Side          string
	Size          float64
	EntryPx       float64
	USD           float64
	Leverage      int
	OID           int64
	StopPx        float64
	StopOID       int64
	TakeProfitPx  float64
	TakeProfitOID int64
}

// Closed is the result of closing a position.
type Closed struct {
	Coin           string
	Size           float64 // what the close filled
	AvgPx          float64
	Remaining      float64 // what is still open, when the close did not fill in full
	StopsCancelled int
}

// Defaults for the trading limits, in force until a Service sets its own.
const (
	defaultTradeMaxUSD      = 100.0
	defaultTradeMaxLeverage = 5
	minOrderUSD             = 10.0 // the exchange's minimum
	stopRoom                = 0.05 // how far past its trigger a stop may fill
)

func (s *Service) tradeLimits() (float64, int) {
	maxUSD, maxLev := s.TradeMaxUSD, s.TradeMaxLeverage
	if maxUSD <= 0 {
		maxUSD = defaultTradeMaxUSD
	}
	if maxLev <= 0 {
		maxLev = defaultTradeMaxLeverage
	}
	return maxUSD, maxLev
}

// tradeClient opens the account's wallet for trading.
func (s *Service) tradeClient(ctx context.Context, tenant uuid.UUID) (Wallet, *hyperliquid.Client, error) {
	if err := s.open(ctx, tenant); err != nil {
		return Wallet{}, nil, err
	}
	w, signer, err := s.WalletSigner(ctx, tenant)
	if errors.Is(err, pg.ErrNotFound) {
		return Wallet{}, nil, refusef("the account has no wallet yet")
	}
	if err != nil {
		return Wallet{}, nil, err
	}
	if !w.Approved() {
		return Wallet{}, nil, refusef("the account has not approved its wallet yet")
	}
	ex, err := s.exchange(w.Mode)
	if err != nil {
		return Wallet{}, nil, err
	}
	ex.Signer = signer
	return w, ex, nil
}

// market finds a coin's perpetual and its mid price.
func market(ctx context.Context, ex *hyperliquid.Client, coin string) (hyperliquid.Asset, float64, error) {
	assets, markets, err := ex.Markets(ctx)
	if err != nil {
		return hyperliquid.Asset{}, 0, err
	}
	for i, a := range assets {
		if a.Name != coin || a.Delisted {
			continue
		}
		px := markets[i].MidPx
		if px <= 0 {
			px = markets[i].MarkPx
		}
		if px <= 0 {
			return a, 0, refusef("%s has no price right now", coin)
		}
		return a, px, nil
	}
	return hyperliquid.Asset{}, 0, refusef("%s is not a perpetual on this exchange", coin)
}

func (s *Service) builderFor(w Wallet) *hyperliquid.Builder {
	if s.Builder.Fee > 0 && s.Builder.Address != "" && w.BuilderFee >= s.Builder.Fee {
		b := s.Builder
		return &b
	}
	return nil
}

func cloid() string {
	var b [16]byte
	rand.Read(b[:])
	return "0x" + hex.EncodeToString(b[:])
}

// OpenTrade opens a position with its stop-loss, and take-profit when
// asked, in one batch. It refuses what the limits do not allow and what
// the exchange will not fill at once.
func (s *Service) OpenTrade(ctx context.Context, tenant uuid.UUID, r TradeRequest) (Trade, error) {
	maxUSD, maxLev := s.tradeLimits()
	r.Coin = strings.ToUpper(strings.TrimSpace(r.Coin))
	r.Side = strings.ToLower(strings.TrimSpace(r.Side))
	if r.Coin == "" {
		return Trade{}, refusef("say which coin")
	}
	if r.Side != "long" && r.Side != "short" {
		return Trade{}, refusef("side must be long or short")
	}
	if math.IsNaN(r.USD) || r.USD < minOrderUSD || r.USD > maxUSD {
		return Trade{}, refusef("size must be between $%.0f and $%.0f", minOrderUSD, maxUSD)
	}
	if r.Leverage < 1 || r.Leverage > maxLev {
		return Trade{}, refusef("leverage must be between 1 and %d", maxLev)
	}
	if math.IsNaN(r.StopLossPct) || r.StopLossPct <= 0 || r.StopLossPct > 0.5 {
		return Trade{}, refusef("a stop-loss between 0 and 50%% is required")
	}
	if math.IsNaN(r.TakeProfitPct) || r.TakeProfitPct < 0 || r.TakeProfitPct > 5 {
		return Trade{}, refusef("take-profit must be between 0 and 500%%")
	}
	if r.SlippagePct == 0 {
		r.SlippagePct = 0.01
	}
	if math.IsNaN(r.SlippagePct) || r.SlippagePct < 0.001 || r.SlippagePct > 0.05 {
		return Trade{}, refusef("slippage must be between 0.1%% and 5%%")
	}
	w, ex, err := s.tradeClient(ctx, tenant)
	if err != nil {
		return Trade{}, err
	}
	asset, mid, err := market(ctx, ex, r.Coin)
	if err != nil {
		return Trade{}, err
	}
	if asset.MaxLeverage > 0 && r.Leverage > asset.MaxLeverage {
		return Trade{}, refusef("%s allows at most %dx", r.Coin, asset.MaxLeverage)
	}
	size := hyperliquid.RoundSize(r.USD/mid, asset.SzDecimals)
	if size*mid < minOrderUSD {
		return Trade{}, refusef("$%.0f of %s is below the exchange's $10 minimum once rounded", r.USD, r.Coin)
	}
	buy := r.Side == "long"
	dir := 1.0 // the way the position gains
	if !buy {
		dir = -1
	}
	price := hyperliquid.RoundPrice(mid*(1+dir*r.SlippagePct), asset.SzDecimals)
	stopPx := hyperliquid.RoundPrice(mid*(1-dir*r.StopLossPct), asset.SzDecimals)
	if err := ex.SetLeverage(ctx, asset.Index, true, r.Leverage); err != nil {
		return Trade{}, refusef("the exchange refused %dx on %s: %v", r.Leverage, r.Coin, err)
	}
	orders := []hyperliquid.Order{
		{Asset: asset.Index, Buy: buy, Price: price, Size: size, Tif: hyperliquid.Ioc, Cloid: cloid()},
		{Asset: asset.Index, Buy: !buy, Price: hyperliquid.RoundPrice(stopPx*(1-dir*stopRoom), asset.SzDecimals), Size: size, ReduceOnly: true,
			Trigger: &hyperliquid.Trigger{Price: stopPx, Market: true, Kind: "sl"}},
	}
	var tpPx float64
	if r.TakeProfitPct > 0 {
		tpPx = hyperliquid.RoundPrice(mid*(1+dir*r.TakeProfitPct), asset.SzDecimals)
		orders = append(orders, hyperliquid.Order{Asset: asset.Index, Buy: !buy, Price: hyperliquid.RoundPrice(tpPx*(1-dir*stopRoom), asset.SzDecimals), Size: size, ReduceOnly: true,
			Trigger: &hyperliquid.Trigger{Price: tpPx, Market: true, Kind: "tp"}})
	}
	placed, err := ex.Place(ctx, orders, hyperliquid.GroupNormalTPSL, s.builderFor(w))
	if err != nil {
		if errors.Is(err, hyperliquid.ErrUnknown) {
			return Trade{}, fmt.Errorf("%s: no clear answer from the exchange; check the positions before trading again", r.Coin)
		}
		return Trade{}, refusef("the exchange refused the order: %v", err)
	}
	main := placed[0]
	if main.Error != "" {
		cancelResting(ctx, ex, asset.Index, placed[1:])
		return Trade{}, refusef("the exchange refused the order: %s", main.Error)
	}
	if main.Filled <= 0 {
		cancelResting(ctx, ex, asset.Index, placed[1:])
		return Trade{}, refusef("%s did not fill within %.1f%% of the price; try again", r.Coin, r.SlippagePct*100)
	}
	t := Trade{Coin: r.Coin, Side: r.Side, Size: main.Filled, EntryPx: main.AvgPx, USD: main.Filled * main.AvgPx, Leverage: r.Leverage, OID: main.OID, StopPx: stopPx, StopOID: placed[1].OID}
	if len(placed) > 2 {
		t.TakeProfitPx, t.TakeProfitOID = tpPx, placed[2].OID
	}
	return t, nil
}

func cancelResting(ctx context.Context, ex *hyperliquid.Client, asset int, placed []hyperliquid.Placed) {
	for _, p := range placed {
		if p.OID != 0 {
			ex.Cancel(ctx, asset, p.OID)
		}
	}
}

// CloseTrade closes the account's position in a coin with a market order,
// then cancels the stops left for it.
func (s *Service) CloseTrade(ctx context.Context, tenant uuid.UUID, coin string, slippage float64) (Closed, error) {
	coin = strings.ToUpper(strings.TrimSpace(coin))
	if slippage == 0 {
		slippage = 0.01
	}
	if coin == "" || math.IsNaN(slippage) || slippage < 0.001 || slippage > 0.05 {
		return Closed{}, refusef("say which coin; slippage must be between 0.1%% and 5%%")
	}
	w, ex, err := s.tradeClient(ctx, tenant)
	if err != nil {
		return Closed{}, err
	}
	acct, err := ex.Account(ctx, w.User)
	if err != nil {
		return Closed{}, err
	}
	var pos *hyperliquid.Position
	for i := range acct.Positions {
		if acct.Positions[i].Coin == coin {
			pos = &acct.Positions[i]
		}
	}
	if pos == nil {
		return Closed{}, refusef("no position in %s", coin)
	}
	asset, mid, err := market(ctx, ex, coin)
	if err != nil {
		return Closed{}, err
	}
	buy := pos.Size < 0
	dir := -1.0
	if buy {
		dir = 1
	}
	size := hyperliquid.RoundSize(math.Abs(pos.Size), asset.SzDecimals)
	price := hyperliquid.RoundPrice(mid*(1+dir*slippage), asset.SzDecimals)
	placed, err := ex.Place(ctx, []hyperliquid.Order{{Asset: asset.Index, Buy: buy, Price: price, Size: size, ReduceOnly: true, Tif: hyperliquid.Ioc, Cloid: cloid()}}, hyperliquid.GroupNone, s.builderFor(w))
	if err != nil {
		if errors.Is(err, hyperliquid.ErrUnknown) {
			return Closed{}, fmt.Errorf("%s: no clear answer from the exchange; check the positions before trading again", coin)
		}
		return Closed{}, refusef("the exchange refused the close: %v", err)
	}
	if placed[0].Error != "" {
		return Closed{}, refusef("the exchange refused the close: %s", placed[0].Error)
	}
	c := Closed{Coin: coin, Size: placed[0].Filled, AvgPx: placed[0].AvgPx, Remaining: size - placed[0].Filled}
	if c.Remaining > 0 {
		return c, nil // the stops still guard what is left
	}
	open, err := ex.OpenOrders(ctx, w.User)
	if err != nil {
		return c, nil
	}
	for _, o := range open {
		if o.Coin == coin && o.Trigger {
			if ex.Cancel(ctx, asset.Index, o.OID) == nil {
				c.StopsCancelled++
			}
		}
	}
	return c, nil
}
