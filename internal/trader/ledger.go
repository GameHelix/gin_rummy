package trader

import (
	"errors"
	"math/big"
	"strconv"
)

// Account is a cash-funded, long-only paper account in integer cents.
type Account struct {
	Cash       Money            `json:"cash_cents"`
	Receivable Money            `json:"dividend_receivable_cents"`
	Positions  map[string]int64 `json:"positions"`
	FeesPaid   Money            `json:"fees_paid_cents"`
}

func NewAccount(cash Money) Account { return Account{Cash: cash, Positions: map[string]int64{}} }

func (a Account) Invested(prices map[string]Money) (Money, error) {
	return portfolioValue(a.Positions, prices)
}

// Equity includes dividend receivables. Yahoo supplies ex-dates but not
// payment dates, so receivables count toward value but cannot fund purchases.
func (a Account) Equity(prices map[string]Money) (Money, error) {
	invested, err := a.Invested(prices)
	if err != nil {
		return 0, err
	}
	return addMoney(a.Cash, a.Receivable, invested)
}

// ApplyDividend books the ex-date entitlement of the shares held before the open.
func (a *Account) ApplyDividend(symbol string, perShare float64) (Money, error) {
	if a.Positions[symbol] == 0 || perShare == 0 {
		return 0, nil
	}
	credit, err := checkedCents(float64(a.Positions[symbol]) * perShare)
	if err != nil {
		return 0, err
	}
	a.Receivable, err = addMoney(a.Receivable, credit)
	return credit, err
}

// ApplySplit converts a holding into post-split shares at the open of the
// ex-date. A fractional share left by a reverse split is paid in cash at price.
func (a *Account) ApplySplit(symbol string, sp Split, price Money) (Money, error) {
	held := a.Positions[symbol]
	if held == 0 {
		return 0, nil
	}
	whole, frac, err := splitShares(held, sp)
	if err != nil {
		return 0, err
	}
	cashInLieu := Money(0)
	if frac.Sign() > 0 {
		v := new(big.Rat).Mul(frac, new(big.Rat).SetInt64(int64(price)))
		q := new(big.Int).Quo(v.Num(), v.Denom())
		if !q.IsInt64() {
			return 0, errors.New("cash in lieu exceeds the supported range")
		}
		cashInLieu = Money(q.Int64())
		if a.Cash, err = addMoney(a.Cash, cashInLieu); err != nil {
			return 0, err
		}
	}
	if whole == 0 {
		delete(a.Positions, symbol)
	} else {
		a.Positions[symbol] = whole
	}
	return cashInLieu, nil
}

// splitShares returns floor(shares × ratio) and the fractional remainder,
// using the decimal ratio the vendor published.
func splitShares(shares int64, sp Split) (int64, *big.Rat, error) {
	num, ok1 := new(big.Rat).SetString(strconv.FormatFloat(sp.Numerator, 'f', -1, 64))
	den, ok2 := new(big.Rat).SetString(strconv.FormatFloat(sp.Denominator, 'f', -1, 64))
	if !ok1 || !ok2 || num.Sign() <= 0 || den.Sign() <= 0 {
		return 0, nil, errors.New("invalid split ratio")
	}
	exact := new(big.Rat).Mul(new(big.Rat).SetInt64(shares), new(big.Rat).Quo(num, den))
	whole := new(big.Int).Quo(exact.Num(), exact.Denom())
	if !whole.IsInt64() || whole.Int64() > int64(maxAccountCents) {
		return 0, nil, errors.New("post-split position exceeds the supported range")
	}
	frac := new(big.Rat).Sub(exact, new(big.Rat).SetInt(whole))
	return whole.Int64(), frac, nil
}

// AdjustOrdersForSplit restates pending quantities in post-split shares.
func AdjustOrdersForSplit(orders []Order, symbol string, sp Split) ([]Order, error) {
	var out []Order
	for _, o := range orders {
		if o.Symbol == symbol {
			q, _, err := splitShares(o.Quantity, sp)
			if err != nil {
				return nil, err
			}
			if q == 0 {
				continue
			}
			o.Quantity = q
		}
		out = append(out, o)
	}
	return out, nil
}
