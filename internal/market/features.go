package market

import (
	"fmt"
	"math"
)

// Indicators summarise one symbol's recent price action at a session close.
type Indicators struct {
	Symbol        string  `json:"symbol"`
	EvidenceID    string  `json:"evidence_id"`
	Session       string  `json:"session"`
	Close         float64 `json:"close"`
	Return1D      float64 `json:"return_1d"`
	Return5D      float64 `json:"return_5d"`
	Return20D     float64 `json:"return_20d"`
	Return60D     float64 `json:"return_60d"`
	SMA20         float64 `json:"sma_20"`
	SMA60         float64 `json:"sma_60"`
	Volatility20D float64 `json:"daily_volatility_20d"`
	VolumeRatio   float64 `json:"volume_vs_20d_average"`
	High1Y        float64 `json:"high_1y"`
	Low1Y         float64 `json:"low_1y"`
}

const minHistoryBars = 61

// ComputeIndicators uses bars oldest-first; the last bar is the decision session.
func ComputeIndicators(symbol, evidenceID string, bars []Bar) (Indicators, error) {
	n := len(bars)
	if n < minHistoryBars {
		return Indicators{}, fmt.Errorf("%s: need at least %d daily bars, have %d", symbol, minHistoryBars, n)
	}
	last := bars[n-1]
	ind := Indicators{Symbol: symbol, EvidenceID: evidenceID, Session: last.Date, Close: round(last.Close, 4)}
	ind.Return1D = round(last.Close/bars[n-2].Close-1, 5)
	ind.Return5D = round(last.Close/bars[n-6].Close-1, 5)
	ind.Return20D = round(last.Close/bars[n-21].Close-1, 5)
	ind.Return60D = round(last.Close/bars[n-61].Close-1, 5)
	var sma20, sma60, volume20 float64
	for _, b := range bars[n-20:] {
		sma20 += b.Close / 20
		volume20 += float64(b.Volume) / 20
	}
	for _, b := range bars[n-60:] {
		sma60 += b.Close / 60
	}
	ind.SMA20, ind.SMA60 = round(sma20, 4), round(sma60, 4)
	returns := make([]float64, 0, 20)
	for i := n - 20; i < n; i++ {
		returns = append(returns, bars[i].Close/bars[i-1].Close-1)
	}
	_, sd := meanSD(returns)
	ind.Volatility20D = round(sd, 5)
	if volume20 > 0 {
		ind.VolumeRatio = round(float64(last.Volume)/volume20, 3)
	}
	ind.High1Y, ind.Low1Y = last.High, last.Low
	for _, b := range bars[max(0, n-252):] {
		ind.High1Y = math.Max(ind.High1Y, b.High)
		ind.Low1Y = math.Min(ind.Low1Y, b.Low)
	}
	ind.High1Y, ind.Low1Y = round(ind.High1Y, 4), round(ind.Low1Y, 4)
	return ind, nil
}

func meanSD(values []float64) (float64, float64) {
	if len(values) == 0 {
		return 0, 0
	}
	var mean float64
	for _, v := range values {
		mean += v / float64(len(values))
	}
	if len(values) < 2 {
		return mean, 0
	}
	var variance float64
	for _, v := range values {
		variance += (v - mean) * (v - mean) / float64(len(values)-1)
	}
	return mean, math.Sqrt(variance)
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}
