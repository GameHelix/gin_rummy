package broker

import (
	"math"
	"math/big"
	"testing"
)

func FuzzExecutionPriceMatchesIntegerOracle(f *testing.F) {
	for _, seed := range [][2]uint32{{14355, 0}, {1999, 0}, {10000, 1000}, {20000, 500}, {1, 1_000_000}} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, quote, slip uint32) {
		// Cent quotes up to $10 million, and slippage in thousandths of a
		// basis point. This bounded integer oracle cannot overflow int64.
		cents := int64(quote%1_000_000_000) + 1
		milliBPS := int64(slip % 1_000_001)
		const denominator int64 = 10_000_000
		for _, buy := range []bool{false, true} {
			factor := denominator - milliBPS
			if buy {
				factor = denominator + milliBPS
			}
			numerator := cents * factor
			want := numerator / denominator
			if buy && numerator%denominator != 0 {
				want++
			}
			got, err := executionPrice(float64(cents)/100, float64(milliBPS)/1000, buy)
			if err != nil || int64(got) != want {
				t.Fatalf("execution differs from integer oracle: cents=%d milliBPS=%d buy=%v got=%d want=%d err=%v", cents, milliBPS, buy, got, want, err)
			}
		}
	})
}

func FuzzCheckedShareValue(f *testing.F) {
	for _, values := range [][2]int64{{0, 0}, {10, 10000}, {math.MaxInt64, math.MaxInt64}, {1, int64(maxAccountCents)}, {-1, 1}} {
		f.Add(values[0], values[1])
	}
	f.Fuzz(func(t *testing.T, quantity, price int64) {
		value, err := ShareValue(quantity, Money(price))
		if quantity < 0 || price < 0 {
			if err == nil {
				t.Fatal("negative money accepted")
			}
			return
		}
		oracle := new(big.Int).Mul(big.NewInt(quantity), big.NewInt(price))
		if oracle.Cmp(big.NewInt(int64(maxAccountCents))) > 0 {
			if err == nil {
				t.Fatal("unsupported position value accepted")
			}
			return
		}
		if err != nil || int64(value) != oracle.Int64() {
			t.Fatalf("checked multiplication differs from big-integer oracle: %d %d", quantity, price)
		}
	})
}

func TestExecutionPricesUseDecimalTickRounding(t *testing.T) {
	// Zero-slippage round trips must not create phantom cents from binary floats.
	for _, open := range []float64{143.55, 19.99, 0.07} {
		buy, err1 := executionPrice(open, 0, true)
		sell, err2 := executionPrice(open, 0, false)
		if err1 != nil || err2 != nil || buy != sell || buy != Cents(open) {
			t.Fatalf("%v: buy %d sell %d", open, buy, sell)
		}
	}
	// 5 bps on $100.00 is exactly $100.05 / $99.95; a fractional result rounds against us.
	if buy, _ := executionPrice(100, 5, true); buy != 10005 {
		t.Fatalf("buy %d", buy)
	}
	if sell, _ := executionPrice(100, 5, false); sell != 9995 {
		t.Fatalf("sell %d", sell)
	}
	if buy, _ := executionPrice(19.99, 2.5, true); buy != 2000 {
		t.Fatalf("$19.99 + 2.5 bps rounds up to $20.00, got %d", buy)
	}
	if sell, _ := executionPrice(0.01, 5, false); sell != 0 {
		t.Fatalf("a subcent sale rounds to zero and must be rejected by the broker, got %d", sell)
	}
}
