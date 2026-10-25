package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ismatsamadov/trader/internal/analyst"
	"github.com/ismatsamadov/trader/internal/binance"
	"github.com/ismatsamadov/trader/internal/num"
)

var errStopped = errors.New("trading is stopped")

// rebalance sells first, then buys with the proceeds, planning each side
// from the ledger as it stands.
func (e *Engine) rebalance(ctx context.Context, id int, signals map[string]analyst.Signal) []Trade {
	var trades []Trade
	for _, side := range []string{"SELL", "BUY"} {
		plans, err := PlanOrders(e.Config, e.rules, e.st.Ledger, e.prices, signals)
		if err != nil {
			e.alert(ctx, "plan", "Could not plan orders: "+err.Error())
			return trades
		}
		for _, p := range plans {
			if p.Side != side {
				continue
			}
			t, err := e.send(ctx, id, p, false)
			if t != nil {
				trades = append(trades, *t)
			}
			if err != nil {
				return trades
			}
		}
	}
	return trades
}

// send places one market order. It is saved as pending before it leaves,
// under an ID never used before, so after a crash or an unclear reply it is
// looked up, never sent twice. force lets a sell-off run while stopped.
func (e *Engine) send(ctx context.Context, id int, p Plan, force bool) (*Trade, error) {
	e.checkInbox()
	if !force && (e.st.Halted != "" || e.st.Liquidate) {
		return nil, errStopped
	}
	if err := e.covered(ctx, p); err != nil {
		return nil, err
	}
	e.st.OrderSeq++
	clientID := fmt.Sprintf("%s-%d-%s-%s", e.st.Nonce, e.st.OrderSeq, strings.ToLower(e.Config.Base(p.Symbol)), strings.ToLower(p.Side))
	e.st.Pending = append(e.st.Pending, Pending{ClientID: clientID, Symbol: p.Symbol, Side: p.Side, Qty: p.Qty, QuoteQty: p.QuoteQty, Decision: id, SentAt: e.now()})
	if err := e.save(); err != nil {
		e.st.Pending = e.st.Pending[:len(e.st.Pending)-1]
		return nil, err
	}
	o, err := e.Exchange.PlaceMarket(ctx, binance.OrderRequest{Symbol: p.Symbol, Side: p.Side, Quantity: p.Qty, QuoteQty: p.QuoteQty, ClientID: clientID})
	if errors.Is(err, binance.ErrUnknown) {
		e.alert(ctx, "order "+p.Symbol, fmt.Sprintf("%s %s: no clear answer from Binance; checking the order before trading again.", p.Side, p.Symbol))
		return nil, err
	}
	if err != nil {
		e.dropPending(clientID)
		e.alert(ctx, "order "+p.Symbol, fmt.Sprintf("%s %s rejected: %s", p.Side, p.Symbol, untrusted(err.Error())))
		return nil, errors.Join(err, e.save())
	}
	t, err := e.book(ctx, id, o)
	if err != nil {
		// Executed but not yet booked: recover books it on a later pass.
		e.alert(ctx, "book", fmt.Sprintf("%s %s executed; its fees could not be read yet: %v", p.Side, p.Symbol, err))
		return nil, err
	}
	e.dropPending(clientID)
	return t, e.save()
}

// covered checks the exchange really holds what the order needs. A shortfall
// means funds moved outside the agent, so trading stops.
func (e *Engine) covered(ctx context.Context, p Plan) error {
	acct, err := e.Exchange.Account(ctx)
	if err != nil {
		e.alert(ctx, "account", "Binance account unavailable: "+err.Error())
		return err
	}
	asset, need := e.Config.QuoteAsset, p.QuoteQty
	if p.Side == "SELL" {
		asset, need = e.Config.Base(p.Symbol), p.Qty
	}
	if have := acct.Balances[asset].Free; have.Less(need) {
		e.halt(ctx, haltMismatch, fmt.Sprintf("Binance has %s %s free but an order needs %s; fix the balance, then /resume", have, asset, need))
		return errStopped
	}
	return nil
}

func (e *Engine) dropPending(clientID string) {
	var out []Pending
	for _, p := range e.st.Pending {
		if p.ClientID != clientID {
			out = append(out, p)
		}
	}
	e.st.Pending = out
}

// book records an executed order in the ledger, fees included. An order is
// only booked once its trades, and so its fees, are known.
func (e *Engine) book(ctx context.Context, id int, o binance.Order) (*Trade, error) {
	c := e.Config
	if o.ExecutedQty.Sign() <= 0 {
		return nil, nil
	}
	if len(o.Fills) == 0 {
		fills, err := e.Exchange.OrderFills(ctx, o.Symbol, o.OrderID)
		if err != nil {
			return nil, err
		}
		if len(fills) == 0 {
			return nil, errors.New("Binance lists no trades for it yet")
		}
		o.Fills = fills
	}
	if o.QuoteQty.Sign() <= 0 {
		for _, f := range o.Fills {
			o.QuoteQty = o.QuoteQty.Add(f.QuoteQty)
		}
	}
	price := o.QuoteQty.Div(o.ExecutedQty)
	var feeBase, feeQuote, feeOther num.Dec
	for _, f := range o.Fills {
		if f.Commission.IsZero() {
			continue // the testnet charges no fees
		}
		switch f.CommissionAsset {
		case c.Base(o.Symbol):
			feeBase = feeBase.Add(f.Commission)
		case c.QuoteAsset:
			feeQuote = feeQuote.Add(f.Commission)
		case "":
		default:
			feeOther = feeOther.Add(e.valueFee(ctx, f.CommissionAsset, f.Commission))
		}
	}
	l := &e.st.Ledger
	if o.Side == "BUY" {
		l.Cash = l.Cash.Sub(o.QuoteQty).Sub(feeQuote)
		l.Holdings[o.Symbol] = l.Holdings[o.Symbol].Add(o.ExecutedQty).Sub(feeBase)
	} else {
		l.Cash = l.Cash.Add(o.QuoteQty).Sub(feeQuote)
		l.Holdings[o.Symbol] = l.Holdings[o.Symbol].Sub(o.ExecutedQty).Sub(feeBase)
	}
	fee := feeBase.Mul(price).Add(feeQuote).Add(feeOther)
	l.FeesUSD, l.OffBookFees = l.FeesUSD.Add(fee), l.OffBookFees.Add(feeOther)
	t := Trade{Time: o.Time, Decision: id, Symbol: o.Symbol, Side: o.Side, Qty: o.ExecutedQty, QuoteQty: o.QuoteQty, Price: price, FeeUSD: fee, OrderID: o.OrderID, ClientID: o.ClientID}
	e.st.Trades = keep(append(e.st.Trades, t), 1000)
	e.checkLedger(ctx)
	e.notify(ctx, e.tradeText(t))
	return &t, nil
}

// checkLedger stops trading if the ledger ever goes negative.
func (e *Engine) checkLedger(ctx context.Context) {
	l := e.st.Ledger
	bad := l.Cash.Sign() < 0
	for _, q := range l.Holdings {
		bad = bad || q.Sign() < 0
	}
	if bad {
		e.halt(ctx, haltMismatch, "the ledger went negative; check the account, then /resume")
	}
}

// valueFee prices a fee paid in another asset, such as BNB, in the quote.
func (e *Engine) valueFee(ctx context.Context, asset string, amount num.Dec) num.Dec {
	symbol := asset + e.Config.QuoteAsset
	prices, err := e.Exchange.Prices(ctx, []string{symbol})
	if err != nil {
		e.alert(ctx, "fees", fmt.Sprintf("A fee of %s %s could not be valued: %v", amount, asset, err))
		return num.Dec{}
	}
	return amount.Mul(prices[symbol])
}

// recover settles pending orders: it books those that executed once their
// fees are known and drops those that never reached the exchange.
func (e *Engine) recover(ctx context.Context) {
	var still []Pending
	for _, p := range e.st.Pending {
		o, err := e.Exchange.QueryOrder(ctx, p.Symbol, p.ClientID)
		switch {
		case errors.Is(err, binance.ErrNoSuchOrder):
			if e.now().Sub(p.SentAt) < 2*time.Minute {
				still = append(still, p)
				continue
			}
			e.notify(ctx, fmt.Sprintf("%s %s never reached Binance; nothing was traded.", p.Side, p.Symbol))
		case err != nil:
			still = append(still, p)
			e.alert(ctx, "recover", "Could not check order "+p.ClientID+": "+err.Error())
		case o.Side != p.Side || o.Symbol != p.Symbol || o.Time.Before(p.SentAt.Add(-time.Minute)):
			still = append(still, p)
			e.alert(ctx, "recover", "Order "+p.ClientID+" looks like a different order on Binance; check it by hand. Trading waits until it is resolved.")
		case !o.Final():
			still = append(still, p)
		default:
			if _, err := e.book(ctx, p.Decision, o); err != nil {
				still = append(still, p)
				e.alert(ctx, "recover", fmt.Sprintf("%s %s executed; its fees could not be read yet: %v", p.Side, p.Symbol, err))
			}
		}
	}
	e.st.Pending = still
	e.saveOrAlert(ctx)
}

// reconcile stops trading when the exchange holds less than the ledger.
func (e *Engine) reconcile(ctx context.Context) {
	if len(e.st.Pending) > 0 {
		return
	}
	acct, err := e.Exchange.Account(ctx)
	if err != nil {
		e.alert(ctx, "account", "Binance account unavailable: "+err.Error())
		return
	}
	c := e.Config
	var short []string
	if have := acct.Balances[c.QuoteAsset].Total(); have.Less(e.st.Ledger.Cash) {
		short = append(short, fmt.Sprintf("%s %s of %s", have, c.QuoteAsset, e.st.Ledger.Cash))
	}
	for _, s := range c.Symbols {
		if have := acct.Balances[c.Base(s)].Total(); have.Less(e.st.Ledger.Holdings[s]) {
			short = append(short, fmt.Sprintf("%s %s of %s", have, c.Base(s), e.st.Ledger.Holdings[s]))
		}
	}
	if len(short) > 0 {
		e.halt(ctx, haltMismatch, "Binance holds less than the ledger ("+strings.Join(short, ", ")+"); fix the balance, then /resume")
	}
}

// sellAll sells every position the exchange will accept, even while
// trading is stopped. It runs on every pass until nothing sellable is left;
// a failure on one coin does not stop the others.
func (e *Engine) sellAll(ctx context.Context) {
	if len(e.st.Pending) > 0 {
		e.recover(ctx)
		if len(e.st.Pending) > 0 {
			e.alert(ctx, "sellall", "The sell-off waits for an unresolved order to settle.")
			return
		}
	}
	acct, err := e.Exchange.Account(ctx)
	if err != nil {
		e.alert(ctx, "account", "Binance account unavailable: "+err.Error())
		return
	}
	left := false
	for _, s := range e.Config.Symbols {
		r := e.rules[s]
		qty := num.Min(e.st.Ledger.Holdings[s], acct.Balances[e.Config.Base(s)].Free).Floor(r.StepSize)
		if qty.Sign() <= 0 {
			continue
		}
		if p, ok := e.prices[s]; ok && qty.Mul(p).Less(r.MinNotional) {
			continue
		}
		left = true
		e.send(ctx, 0, Plan{Symbol: s, Side: "SELL", Qty: qty}, true)
	}
	if !left {
		e.st.Liquidate = false
		e.notify(ctx, "Everything sellable is sold; trading stays stopped until /resume.")
	}
}
