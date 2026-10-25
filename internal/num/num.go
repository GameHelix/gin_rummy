// Package num is exact fixed-point arithmetic with eight decimal places, the
// precision Binance uses for prices, quantities and balances.
package num

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

const places = 8

const scale = 100_000_000

// limit keeps every value well inside int64 after one multiplication check.
const limit = 10_000_000_000 * scale

// Dec is a decimal number with eight fractional digits.
type Dec struct{ u int64 }

func Int(i int64) Dec { return checked(new(big.Int).Mul(big.NewInt(i), big.NewInt(scale))) }

// Parse reads a plain decimal such as "0.00055000" or "-12.5". Digits beyond
// the eighth decimal place are rejected rather than rounded.
func Parse(s string) (Dec, error) {
	s = strings.TrimSpace(s)
	neg := strings.HasPrefix(s, "-")
	digits := strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")
	whole, frac, _ := strings.Cut(digits, ".")
	if whole == "" && frac == "" || strings.ContainsAny(whole+frac, "+-eE ") {
		return Dec{}, fmt.Errorf("invalid decimal %q", s)
	}
	frac = strings.TrimRight(frac, "0")
	if len(frac) > places {
		return Dec{}, fmt.Errorf("decimal %q has more than %d places", s, places)
	}
	if whole == "" {
		whole = "0"
	}
	w, err := strconv.ParseUint(whole, 10, 63)
	if err != nil || w >= limit/scale {
		return Dec{}, fmt.Errorf("invalid decimal %q", s)
	}
	var f uint64
	if frac != "" {
		if f, err = strconv.ParseUint(frac+strings.Repeat("0", places-len(frac)), 10, 63); err != nil {
			return Dec{}, fmt.Errorf("invalid decimal %q", s)
		}
	}
	u := int64(w)*scale + int64(f)
	if neg {
		u = -u
	}
	return Dec{u}, nil
}

func MustParse(s string) Dec {
	d, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return d
}

// FromFloat rounds to the nearest 1e-8.
func FromFloat(f float64) (Dec, error) {
	v := math.Round(f * scale)
	if math.IsNaN(v) || math.Abs(v) >= limit {
		return Dec{}, errors.New("number out of range")
	}
	return Dec{int64(v)}, nil
}

func (d Dec) String() string {
	sign := ""
	u := d.u
	if u < 0 {
		sign, u = "-", -u
	}
	s := fmt.Sprintf("%s%d.%08d", sign, u/scale, u%scale)
	return strings.TrimSuffix(strings.TrimRight(s, "0"), ".")
}

// Fixed formats with exactly n decimal places, truncating the rest.
func (d Dec) Fixed(n int) string {
	t := d.Floor(Dec{int64(math.Pow10(places - n))})
	s := fmt.Sprintf("%d.%08d", abs(t.u)/scale, abs(t.u)%scale)
	if t.u < 0 {
		s = "-" + s
	}
	return strings.TrimSuffix(s[:len(s)-(places-n)], ".")
}

func (d Dec) Float() float64 { return float64(d.u) / scale }
func (d Dec) Sign() int {
	switch {
	case d.u > 0:
		return 1
	case d.u < 0:
		return -1
	}
	return 0
}
func (d Dec) IsZero() bool    { return d.u == 0 }
func (d Dec) Cmp(e Dec) int   { return Dec{d.u - e.u}.Sign() }
func (d Dec) Less(e Dec) bool { return d.u < e.u }
func (d Dec) Add(e Dec) Dec   { return checked(new(big.Int).Add(big.NewInt(d.u), big.NewInt(e.u))) }
func (d Dec) Sub(e Dec) Dec   { return checked(new(big.Int).Sub(big.NewInt(d.u), big.NewInt(e.u))) }
func (d Dec) Neg() Dec        { return Dec{-d.u} }

// Mul truncates toward zero.
func (d Dec) Mul(e Dec) Dec {
	p := new(big.Int).Mul(big.NewInt(d.u), big.NewInt(e.u))
	return checked(p.Quo(p, big.NewInt(scale)))
}

// Div truncates toward zero. Dividing by zero panics, as with integers.
func (d Dec) Div(e Dec) Dec {
	if e.u == 0 {
		panic("num: division by zero")
	}
	p := new(big.Int).Mul(big.NewInt(d.u), big.NewInt(scale))
	return checked(p.Quo(p, big.NewInt(e.u)))
}

// Floor rounds down to a multiple of step; a zero step leaves d unchanged.
func (d Dec) Floor(step Dec) Dec {
	if step.u <= 0 {
		return d
	}
	q := d.u / step.u
	if d.u < 0 && d.u%step.u != 0 {
		q--
	}
	return Dec{q * step.u}
}

func Min(a, b Dec) Dec {
	if a.u < b.u {
		return a
	}
	return b
}

func Max(a, b Dec) Dec {
	if a.u > b.u {
		return a
	}
	return b
}

func (d Dec) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func (d *Dec) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		var n json.Number
		if err := json.Unmarshal(b, &n); err != nil {
			return fmt.Errorf("decimal must be a string or number: %s", b)
		}
		s = n.String()
	}
	v, err := Parse(s)
	*d = v
	return err
}

// checked panics when a result leaves the supported range. Inputs are
// validated at the edges, so this only fires on a programming error.
func checked(v *big.Int) Dec {
	if !v.IsInt64() || abs(v.Int64()) >= limit {
		panic("num: value out of range")
	}
	return Dec{v.Int64()}
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
