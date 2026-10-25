package agent

import (
	"errors"
	"math"

	"github.com/candylario/trader/internal/analyst"
	"github.com/candylario/trader/internal/binance"
	"github.com/candylario/trader/internal/config"
	"github.com/candylario/trader/internal/num"
)

// Plan is one market order: a SELL of Qty coins or a BUY spending QuoteQty.
type Plan struct {
	Symbol   string  `json:"symbol"`
	Side     string  `json:"side"`
	Qty      num.Dec `json:"qty"`
	QuoteQty num.Dec `json:"quote_qty"`
	Current  float64 `json:"current_weight"`
	Target   float64 `json:"target_weight"`
}

// TargetWeight maps a rating to a share of equity. Hold keeps the position
// unless it has grown past the cap.
func TargetWeight(c config.Config, r analyst.Rating, current float64) float64 {
	switch r {
	case analyst.RatingStrongBuy:
		return c.MaxPositionWeight
	case analyst.RatingBuy:
		return c.MaxPositionWeight / 2
	case analyst.RatingSell, analyst.RatingStrongSell:
		return 0
	}
	if current > c.MaxPositionWeight+c.RebalanceBand {
		return c.MaxPositionWeight
	}
	return current
}

var cent = num.MustParse("0.01")

// notionalMargin keeps orders clear of the minimum order value: Binance
// checks it against a five-minute average price, not the last one.
var notionalMargin = num.MustParse("1.1")

// PlanOrders turns ratings into orders that respect the exposure cap, the
// rebalance band, the minimum trade and the exchange's lot and notional
// rules. Buys never spend more than the ledger's cash. While holdings are
// above the exposure cap, trims happen even inside the band.
func PlanOrders(c config.Config, rules map[string]binance.Symbol, l Ledger, prices map[string]num.Dec, signals map[string]analyst.Signal) ([]Plan, error) {
	equity, err := l.Equity(prices)
	if err != nil {
		return nil, err
	}
	eq := equity.Float()
	if eq <= 0 {
		return nil, errors.New("no equity to trade")
	}
	current, target := map[string]float64{}, map[string]float64{}
	var total, invested float64
	for _, s := range c.Symbols {
		if prices[s].Sign() <= 0 {
			return nil, errors.New("no price for " + s)
		}
		current[s] = l.Holdings[s].Mul(prices[s]).Float() / eq
		target[s] = TargetWeight(c, signals[s].Rating, current[s])
		total, invested = total+target[s], invested+current[s]
	}
	overCap := invested > c.MaxInvestedWeight+1e-9
	if total > c.MaxInvestedWeight {
		for s := range target {
			target[s] *= c.MaxInvestedWeight / total
		}
	}
	cash := l.Cash
	var plans []Plan
	for _, s := range c.Symbols {
		r, price, diff := rules[s], prices[s], target[s]-current[s]
		usd, _ := num.FromFloat(math.Abs(diff) * eq)
		p := Plan{Symbol: s, Current: round(current[s], 4), Target: round(target[s], 4)}
		switch {
		case diff < 0 && target[s] == 0:
			p.Side, p.Qty = "SELL", l.Holdings[s].Floor(r.StepSize)
		case diff < 0 && (-diff >= c.RebalanceBand || overCap) && usd.Float() >= c.MinTradeUSD:
			p.Side, p.Qty = "SELL", num.Min(usd.Div(price), l.Holdings[s]).Floor(r.StepSize)
		case diff > 0 && diff >= c.RebalanceBand && usd.Float() >= c.MinTradeUSD:
			p.Side, p.QuoteQty = "BUY", num.Min(usd, cash).Floor(cent)
		default:
			continue
		}
		value := p.QuoteQty
		if p.Side == "SELL" {
			if r.MaxQty.Sign() > 0 {
				p.Qty = num.Min(p.Qty, r.MaxQty)
			}
			if p.Qty.Less(r.MinQty) {
				continue
			}
			value = p.Qty.Mul(price)
		}
		if value.Sign() <= 0 || value.Less(r.MinNotional.Mul(notionalMargin)) || (p.Side == "BUY" && value.Float() < c.MinTradeUSD) {
			continue
		}
		if p.Side == "BUY" {
			cash = cash.Sub(p.QuoteQty)
		}
		plans = append(plans, p)
	}
	return plans, nil
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}
