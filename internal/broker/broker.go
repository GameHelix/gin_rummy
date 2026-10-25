package broker

import (
	"errors"
	"math"
	"sort"

	"github.com/ismatsamadov/trader/internal/config"
	"github.com/ismatsamadov/trader/internal/store"
)

// Order is a market-on-open instruction for the next session.
type Order struct {
	ID           string  `json:"id"`
	Symbol       string  `json:"symbol"`
	Side         string  `json:"side"`
	Quantity     int64   `json:"quantity"`
	DecidedOn    string  `json:"decided_on"`
	TargetWeight float64 `json:"target_weight"`
	Reason       string  `json:"reason"`
}

type Fill struct {
	ID        string `json:"id"`
	OrderID   string `json:"order_id"`
	DecidedOn string `json:"decided_on"`
	Session   string `json:"session"`
	Symbol    string `json:"symbol"`
	Side      string `json:"side"`
	Requested int64  `json:"requested_quantity"`
	Quantity  int64  `json:"quantity"`
	Price     Money  `json:"price_cents"`
	Fee       Money  `json:"fee_cents"`
	CashAfter Money  `json:"cash_after_cents"`
	Note      string `json:"note"`
}

type Rejection struct {
	OrderID string `json:"order_id"`
	Session string `json:"session"`
	Symbol  string `json:"symbol"`
	Reason  string `json:"reason"`
}

// ExecuteAtOpen is the paper broker: it fills market-on-open orders at the
// session's opening price plus adverse slippage. Sales run first, then
// purchases in symbol order; purchases are rechecked against cash, exposure and
// position limits at the actual open, which the analyst never saw.
func ExecuteAtOpen(c config.Config, session string, orders []Order, opens map[string]float64, acct *Account) ([]Fill, []Rejection, error) {
	if acct.Positions == nil {
		acct.Positions = map[string]int64{}
	}
	pending := append([]Order(nil), orders...)
	sort.SliceStable(pending, func(i, j int) bool {
		if (pending[i].Side == "sell") != (pending[j].Side == "sell") {
			return pending[i].Side == "sell"
		}
		return pending[i].Symbol < pending[j].Symbol
	})
	openPrices := map[string]Money{}
	for s, p := range opens {
		openPrices[s] = Cents(p)
	}
	fee := Cents(c.Commission)
	var fills []Fill
	var rejections []Rejection
	reject := func(o Order, reason string) {
		rejections = append(rejections, Rejection{o.ID, session, o.Symbol, reason})
	}
	for _, o := range pending {
		open, ok := opens[o.Symbol]
		if !ok || o.Quantity <= 0 {
			reject(o, "no opening price or empty order")
			continue
		}
		buy := o.Side == "buy"
		price, err := executionPrice(open, c.SlippageBPS, buy)
		if err != nil {
			return nil, nil, err
		}
		quantity := o.Quantity
		if !buy {
			quantity = min(quantity, acct.Positions[o.Symbol])
			if quantity <= 0 {
				reject(o, "no shares left to sell")
				continue
			}
			if price <= 0 {
				reject(o, "modeled execution price rounds below one cent")
				continue
			}
			proceeds, err := ShareValue(quantity, price)
			if err != nil {
				return nil, nil, err
			}
			if proceeds < fee {
				reject(o, "sale proceeds would not cover the commission")
				continue
			}
		} else {
			invested, err := acct.Invested(openPrices)
			if err != nil {
				return nil, nil, err
			}
			equity, err := addMoney(acct.Cash, acct.Receivable, invested)
			if err != nil {
				return nil, nil, err
			}
			maxInvested := Money(math.Floor(float64(equity-fee) * c.MaxInvestedWeight))
			maxPosition := Money(math.Floor(float64(equity-fee) * c.MaxPositionWeight))
			cashReserve := Money(math.Ceil(float64(equity-fee) * (1 - c.MaxInvestedWeight)))
			held, err := ShareValue(acct.Positions[o.Symbol], openPrices[o.Symbol])
			if err != nil {
				return nil, nil, err
			}
			unit := max(price, openPrices[o.Symbol])
			quantity = min(quantity, int64((acct.Cash-fee-cashReserve)/price))
			quantity = min(quantity, int64((maxInvested-invested)/unit))
			quantity = min(quantity, int64((maxPosition-held)/unit))
		}
		if quantity <= 0 {
			reject(o, "cash, exposure, position or whole-share limit at the open")
			continue
		}
		notional, err := ShareValue(quantity, price)
		if err != nil {
			return nil, nil, err
		}
		if buy {
			cost, err := addMoney(notional, fee)
			if err != nil {
				return nil, nil, err
			}
			acct.Cash -= cost
			acct.Positions[o.Symbol] += quantity
		} else {
			if acct.Cash, err = addMoney(acct.Cash, notional-fee); err != nil {
				return nil, nil, err
			}
			acct.Positions[o.Symbol] -= quantity
			if acct.Positions[o.Symbol] == 0 {
				delete(acct.Positions, o.Symbol)
			}
		}
		if acct.FeesPaid, err = addMoney(acct.FeesPaid, fee); err != nil {
			return nil, nil, err
		}
		if acct.Cash < 0 || acct.Positions[o.Symbol] < 0 {
			return nil, nil, errors.New("internal ledger invariant violated")
		}
		note := "filled at the open with modeled slippage"
		if quantity < o.Quantity {
			note = "reduced by limits rechecked at the open"
		}
		fills = append(fills, Fill{
			ID: store.Hash([]byte(o.ID + "/" + session))[:16], OrderID: o.ID, DecidedOn: o.DecidedOn, Session: session,
			Symbol: o.Symbol, Side: o.Side, Requested: o.Quantity, Quantity: quantity,
			Price: price, Fee: fee, CashAfter: acct.Cash, Note: note,
		})
	}
	return fills, rejections, nil
}
