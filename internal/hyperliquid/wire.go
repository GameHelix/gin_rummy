package hyperliquid

import (
	"math"
	"strconv"
	"strings"
)

// Tif says how long a limit order lives: Gtc until cancelled, Ioc only for
// what fills at once, Alo only if it rests (maker).
type Tif string

const (
	Gtc Tif = "Gtc"
	Ioc Tif = "Ioc"
	Alo Tif = "Alo"
)

// Trigger turns an order into a stop or take-profit: it is placed when the
// mark price crosses Price, as a market order or at the order's limit price.
type Trigger struct {
	Price  float64
	Market bool
	Kind   string // "sl" or "tp"
}

// Order is one order to place. Price and Size must already be rounded
// with RoundPrice and RoundSize for the asset.
type Order struct {
	Asset      int
	Buy        bool
	Price      float64
	Size       float64
	ReduceOnly bool
	Tif        Tif      // for a limit order
	Trigger    *Trigger // for a stop or take-profit instead
	Cloid      string   // optional client order ID: 0x + 32 hex digits
}

// Builder is the fee a front end charges on an order, in tenths of a
// basis point: 10 is 0.01%. The user must have approved at least that
// rate for the address.
type Builder struct {
	Address string
	Fee     int
}

// Groupings for an order batch.
const (
	GroupNone         = "na"
	GroupNormalTPSL   = "normalTpsl"   // stops tied to the order in the same batch
	GroupPositionTPSL = "positionTpsl" // stops on the whole position
)

// wire formats a number as the exchange hashes it: up to 8 decimals,
// no trailing zeros, no exponent.
func wire(x float64) string {
	s := strconv.FormatFloat(x, 'f', 8, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	if s == "-0" {
		s = "0"
	}
	return s
}

// RoundPrice fits a perp price to the exchange's rules: at most five
// significant figures and at most 6-szDecimals decimals.
func RoundPrice(px float64, szDecimals int) float64 {
	px, _ = strconv.ParseFloat(strconv.FormatFloat(px, 'g', 5, 64), 64)
	return roundTo(px, 6-szDecimals)
}

// RoundSize rounds a size down to the asset's szDecimals.
func RoundSize(sz float64, szDecimals int) float64 {
	p := math.Pow(10, float64(szDecimals))
	return math.Floor(sz*p+1e-9) / p
}

func roundTo(x float64, decimals int) float64 {
	p := math.Pow(10, float64(decimals))
	return math.Round(x*p) / p
}

func orderWire(o Order) omap {
	var t omap
	if o.Trigger != nil {
		t = omap{{"trigger", omap{{"isMarket", o.Trigger.Market}, {"triggerPx", wire(o.Trigger.Price)}, {"tpsl", o.Trigger.Kind}}}}
	} else {
		t = omap{{"limit", omap{{"tif", string(o.Tif)}}}}
	}
	w := omap{{"a", o.Asset}, {"b", o.Buy}, {"p", wire(o.Price)}, {"s", wire(o.Size)}, {"r", o.ReduceOnly}, {"t", t}}
	if o.Cloid != "" {
		w = append(w, kv{"c", o.Cloid})
	}
	return w
}

func orderAction(orders []Order, grouping string, b *Builder) omap {
	wires := make([]omap, len(orders))
	for i, o := range orders {
		wires[i] = orderWire(o)
	}
	a := omap{{"type", "order"}, {"orders", wires}, {"grouping", grouping}}
	if b != nil {
		a = append(a, kv{"builder", omap{{"b", strings.ToLower(b.Address)}, {"f", b.Fee}}})
	}
	return a
}

func cancelAction(asset int, oid int64) omap {
	return omap{{"type", "cancel"}, {"cancels", []omap{{{"a", asset}, {"o", oid}}}}}
}

func cancelByCloidAction(asset int, cloid string) omap {
	return omap{{"type", "cancelByCloid"}, {"cancels", []omap{{{"asset", asset}, {"cloid", cloid}}}}}
}

func leverageAction(asset int, cross bool, leverage int) omap {
	return omap{{"type", "updateLeverage"}, {"asset", asset}, {"isCross", cross}, {"leverage", leverage}}
}
