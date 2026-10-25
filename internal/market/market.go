// Package market turns candles into the indicators the analyst reads.
package market

import (
	"fmt"
	"math"
	"time"
)

// Candle is one kline. Volume is in the base asset, QuoteVolume in the quote.
type Candle struct {
	Open        time.Time `json:"open_time"`
	O           float64   `json:"open"`
	H           float64   `json:"high"`
	L           float64   `json:"low"`
	C           float64   `json:"close"`
	Volume      float64   `json:"volume"`
	QuoteVolume float64   `json:"quote_volume"`
	CloseTime   time.Time `json:"close_time"`
}

// Closed drops the candle still forming at now.
func Closed(candles []Candle, now time.Time) []Candle {
	for len(candles) > 0 && !candles[len(candles)-1].CloseTime.Before(now) {
		candles = candles[:len(candles)-1]
	}
	return candles
}

// View summarises one coin for the analyst. Returns use completed candles;
// ChangeSinceHour compares the live price with the last hourly close.
type View struct {
	Symbol          string  `json:"symbol"`
	EvidenceID      string  `json:"evidence_id"`
	Price           float64 `json:"price"`
	ChangeSinceHour float64 `json:"change_since_last_hourly_close"`
	Return1H        float64 `json:"return_1h"`
	Return4H        float64 `json:"return_4h"`
	Return24H       float64 `json:"return_24h"`
	Return7D        float64 `json:"return_7d"`
	Return30D       float64 `json:"return_30d"`
	Return90D       float64 `json:"return_90d"`
	HourlyVol24H    float64 `json:"hourly_volatility_24h"`
	RSI14H          float64 `json:"rsi_14_hourly"`
	RSI14D          float64 `json:"rsi_14_daily"`
	SMA20D          float64 `json:"sma_20d"`
	SMA50D          float64 `json:"sma_50d"`
	SMA200D         float64 `json:"sma_200d"`
	High30D         float64 `json:"high_30d"`
	Low30D          float64 `json:"low_30d"`
	VolumeRatio     float64 `json:"quote_volume_24h_vs_7d_daily_avg"`
	Weight          float64 `json:"portfolio_weight"`
}

// NewView needs at least 192 completed hourly and 201 completed daily candles.
func NewView(symbol, evidenceID string, price float64, hourly, daily []Candle) (View, error) {
	h, d := len(hourly), len(daily)
	if h < 192 || d < 201 {
		return View{}, fmt.Errorf("%s: need 192 hourly and 201 daily candles, have %d and %d", symbol, h, d)
	}
	if price <= 0 {
		return View{}, fmt.Errorf("%s: no live price", symbol)
	}
	ret := func(c []Candle, back int) float64 { return round(c[len(c)-1].C/c[len(c)-1-back].C-1, 5) }
	v := View{Symbol: symbol, EvidenceID: evidenceID, Price: price}
	v.ChangeSinceHour = round(price/hourly[h-1].C-1, 5)
	v.Return1H, v.Return4H, v.Return24H = ret(hourly, 1), ret(hourly, 4), ret(hourly, 24)
	v.Return7D, v.Return30D, v.Return90D = ret(daily, 7), ret(daily, 30), ret(daily, 90)
	returns := make([]float64, 0, 24)
	for i := h - 24; i < h; i++ {
		returns = append(returns, hourly[i].C/hourly[i-1].C-1)
	}
	v.HourlyVol24H = round(stdev(returns), 5)
	v.RSI14H, v.RSI14D = round(rsi(hourly, 14), 1), round(rsi(daily, 14), 1)
	v.SMA20D, v.SMA50D, v.SMA200D = round(sma(daily, 20), 2), round(sma(daily, 50), 2), round(sma(daily, 200), 2)
	v.High30D, v.Low30D = daily[d-30].H, daily[d-30].L
	for _, c := range daily[d-30:] {
		v.High30D, v.Low30D = math.Max(v.High30D, c.H), math.Min(v.Low30D, c.L)
	}
	var day, week float64
	for _, c := range hourly[h-24:] {
		day += c.QuoteVolume
	}
	for _, c := range hourly[h-192 : h-24] {
		week += c.QuoteVolume / 7
	}
	if week > 0 {
		v.VolumeRatio = round(day/week, 3)
	}
	return v, nil
}

func sma(c []Candle, n int) float64 {
	var s float64
	for _, x := range c[len(c)-n:] {
		s += x.C / float64(n)
	}
	return s
}

// rsi is Wilder's relative strength index over the whole series.
func rsi(c []Candle, n int) float64 {
	var gain, loss float64
	for i := 1; i < len(c); i++ {
		d := c[i].C - c[i-1].C
		up, down := math.Max(d, 0), math.Max(-d, 0)
		if i <= n {
			gain, loss = gain+up/float64(n), loss+down/float64(n)
			continue
		}
		gain = (gain*float64(n-1) + up) / float64(n)
		loss = (loss*float64(n-1) + down) / float64(n)
	}
	if loss == 0 {
		return 100
	}
	return 100 - 100/(1+gain/loss)
}

func stdev(v []float64) float64 {
	if len(v) < 2 {
		return 0
	}
	var mean, ss float64
	for _, x := range v {
		mean += x / float64(len(v))
	}
	for _, x := range v {
		ss += (x - mean) * (x - mean)
	}
	return math.Sqrt(ss / float64(len(v)-1))
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}
