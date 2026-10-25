package trader

import (
	"fmt"
	"math"
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

// PlanLine explains what the rules did with one symbol's rating.
type PlanLine struct {
	Symbol        string  `json:"symbol"`
	Rating        Rating  `json:"rating"`
	Shares        int64   `json:"current_shares"`
	CurrentWeight float64 `json:"current_weight"`
	TargetWeight  float64 `json:"target_weight"`
	Action        string  `json:"action"`
	Quantity      int64   `json:"quantity"`
	Note          string  `json:"note"`
}

// TargetWeight maps a rating to a position size. Hold keeps the current weight
// unless price drift has pushed it beyond the position limit plus the band.
func TargetWeight(c Config, r Rating, current float64) float64 {
	switch r {
	case RatingStrongBuy:
		return c.MaxPositionWeight
	case RatingBuy:
		return c.MaxPositionWeight / 2
	case RatingSell, RatingStrongSell:
		return 0
	}
	if current > c.MaxPositionWeight+c.RebalanceBand {
		return c.MaxPositionWeight
	}
	return current
}

// PlanOrders converts ratings into whole-share orders using closing prices.
// Weight changes inside the no-trade band, and trades smaller than
// min_trade_usd, are skipped so noise does not become commissions. Exits
// always proceed.
func PlanOrders(c Config, session string, signals map[string]Signal, acct Account, closes map[string]Money) ([]Order, []PlanLine, error) {
	equity, err := acct.Equity(closes)
	if err != nil {
		return nil, nil, err
	}
	if equity <= 0 {
		return nil, nil, fmt.Errorf("account has no equity")
	}
	current, target, rating := map[string]float64{}, map[string]float64{}, map[string]Rating{}
	for _, s := range c.Symbols {
		if closes[s] <= 0 {
			return nil, nil, fmt.Errorf("missing closing price for %s", s)
		}
		value, err := shareValue(acct.Positions[s], closes[s])
		if err != nil {
			return nil, nil, err
		}
		current[s] = float64(value) / float64(equity)
		rating[s] = RatingHold
		if sig, ok := signals[s]; ok {
			rating[s] = sig.Rating
		}
		target[s] = TargetWeight(c, rating[s], current[s])
	}
	// Keep total exposure within the limit, cutting new purchases first.
	var total, increase float64
	for _, s := range c.Symbols {
		total += target[s]
		increase += math.Max(0, target[s]-current[s])
	}
	if excess := total - c.MaxInvestedWeight; excess > 1e-12 {
		if increase > 0 {
			cut := math.Min(excess, increase)
			for _, s := range c.Symbols {
				if d := target[s] - current[s]; d > 0 {
					target[s] -= d * cut / increase
				}
			}
			total -= cut
		}
		if total > c.MaxInvestedWeight+1e-12 {
			for _, s := range c.Symbols {
				target[s] *= c.MaxInvestedWeight / total
			}
		}
	}
	var orders []Order
	var plan []PlanLine
	for _, s := range c.Symbols {
		held := acct.Positions[s]
		line := PlanLine{Symbol: s, Rating: rating[s], Shares: held, CurrentWeight: round(current[s], 4), TargetWeight: round(target[s], 4), Action: "none"}
		desired := int64(math.Floor(target[s] * float64(equity) / float64(closes[s])))
		delta := desired - held
		switch {
		case target[s] == 0 && held > 0:
			delta = -held
			line.Note = "exit position"
		case rating[s] == RatingHold && target[s] == current[s]:
			line.Note = "hold"
			delta = 0
		case target[s] == 0:
			line.Note = "no position"
		case math.Abs(target[s]-current[s]) < c.RebalanceBand:
			line.Note = fmt.Sprintf("change of %.1f%% is inside the %.1f%% no-trade band", math.Abs(target[s]-current[s])*100, c.RebalanceBand*100)
			delta = 0
		case delta == 0:
			line.Note = "already at target"
		case math.Abs(float64(delta))*closes[s].Dollars() < c.MinTradeUSD:
			line.Note = fmt.Sprintf("trade below $%.0f minimum", c.MinTradeUSD)
			delta = 0
		}
		if delta != 0 {
			side, qty := "buy", delta
			if delta < 0 {
				side, qty = "sell", -delta
			}
			if line.Note == "" {
				line.Note = fmt.Sprintf("rebalance toward %.1f%%", target[s]*100)
			}
			line.Action, line.Quantity = side, qty
			orders = append(orders, Order{ID: Hash([]byte(session + "/" + s + "/" + side))[:16], Symbol: s, Side: side, Quantity: qty, DecidedOn: session, TargetWeight: round(target[s], 4), Reason: string(rating[s]) + ": " + line.Note})
		}
		plan = append(plan, line)
	}
	return orders, plan, nil
}

// BenchmarkOrders buys an equal-weight basket once and then holds it, giving the
// agent an honest same-universe comparison under identical costs.
func BenchmarkOrders(c Config, session string, acct Account, closes map[string]Money) ([]Order, error) {
	equity, err := acct.Equity(closes)
	if err != nil {
		return nil, err
	}
	weight := math.Min(c.MaxPositionWeight, c.MaxInvestedWeight/float64(len(c.Symbols)))
	var orders []Order
	for _, s := range c.Symbols {
		qty := int64(math.Floor(weight * float64(equity) / float64(closes[s])))
		if qty > 0 {
			orders = append(orders, Order{ID: Hash([]byte("benchmark/" + session + "/" + s))[:16], Symbol: s, Side: "buy", Quantity: qty, DecidedOn: session, TargetWeight: round(weight, 4), Reason: "benchmark initial equal-weight purchase"})
		}
	}
	return orders, nil
}
