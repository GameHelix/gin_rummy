package broker

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
)

// Money is integer USD cents. Shares are whole, nonnegative units.
type Money int64

func Cents(dollars float64) Money { return Money(math.Round(dollars * 100)) }
func (m Money) Dollars() float64  { return float64(m) / 100 }

// Keep account values within a conservative reporting/arithmetic range. Initial
// capital is capped separately; extreme market data must fail, never wrap.
const maxAccountCents Money = 100_000_000_000_000 // USD 1 trillion

// Interpret stored quotes and basis points as their round-trip decimal values.
// Round once to an adverse cent tick, after applying exact decimal slippage.
func executionPrice(open, slippageBPS float64, buy bool) (Money, error) {
	if math.IsNaN(open) || math.IsInf(open, 0) || open <= 0 || math.IsNaN(slippageBPS) || math.IsInf(slippageBPS, 0) || slippageBPS < 0 || slippageBPS > 1000 {
		return 0, errors.New("invalid modeled execution price or slippage")
	}
	quote, _ := new(big.Rat).SetString(strconv.FormatFloat(open, 'f', -1, 64))
	slippage, _ := new(big.Rat).SetString(strconv.FormatFloat(slippageBPS, 'f', -1, 64))
	factor := big.NewRat(10000, 1)
	if buy {
		factor.Add(factor, slippage)
	} else {
		factor.Sub(factor, slippage)
	}
	cents := new(big.Rat).Mul(quote, factor)
	cents.Quo(cents, big.NewRat(100, 1))
	whole, remainder := new(big.Int), new(big.Int)
	whole.QuoRem(cents.Num(), cents.Denom(), remainder)
	if buy && remainder.Sign() != 0 {
		whole.Add(whole, big.NewInt(1))
	}
	if !whole.IsInt64() || whole.Int64() > int64(maxAccountCents) {
		return 0, errors.New("modeled execution price exceeds the supported USD range")
	}
	return Money(whole.Int64()), nil
}

func addMoney(values ...Money) (Money, error) {
	var total Money
	for _, value := range values {
		if value < 0 || value > maxAccountCents-total {
			return 0, errors.New("account value exceeds the supported USD range")
		}
		total += value
	}
	return total, nil
}

func ShareValue(quantity int64, price Money) (Money, error) {
	if quantity < 0 || price < 0 {
		return 0, errors.New("negative shares or price in account valuation")
	}
	if quantity == 0 || price == 0 {
		return 0, nil
	}
	if quantity > int64(maxAccountCents/price) {
		return 0, errors.New("position value exceeds the supported USD range")
	}
	return Money(quantity) * price, nil
}

func portfolioValue(positions map[string]int64, prices map[string]Money) (Money, error) {
	var total Money
	for symbol, quantity := range positions {
		if quantity == 0 {
			continue
		}
		price, ok := prices[symbol]
		if !ok || price <= 0 {
			return 0, fmt.Errorf("no price to value %s", symbol)
		}
		value, err := ShareValue(quantity, price)
		if err != nil {
			return 0, err
		}
		total, err = addMoney(total, value)
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}
