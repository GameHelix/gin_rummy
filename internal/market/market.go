package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type Bar struct {
	Date   string  `json:"date"`
	Open   float64 `json:"open"`
	High   float64 `json:"high"`
	Low    float64 `json:"low"`
	Close  float64 `json:"close"`
	Volume int64   `json:"volume"`
}

type Dividend struct {
	Date   string  `json:"date"`
	Amount float64 `json:"amount"`
}

// Split is a share-unit change effective at the open of Date (the ex-date).
type Split struct {
	Date        string  `json:"date"`
	Numerator   float64 `json:"numerator"`
	Denominator float64 `json:"denominator"`
}

// Series is one symbol's recent daily history in current (split-adjusted)
// share units, plus the corporate actions needed to keep a ledger in step.
type Series struct {
	Symbol    string     `json:"symbol"`
	Bars      []Bar      `json:"bars"`
	Dividends []Dividend `json:"dividends"`
	Splits    []Split    `json:"splits"`
}

func (s Series) BarOn(date string) (Bar, bool) {
	i := sort.Search(len(s.Bars), func(i int) bool { return s.Bars[i].Date >= date })
	if i < len(s.Bars) && s.Bars[i].Date == date {
		return s.Bars[i], true
	}
	return Bar{}, false
}

// BarsThrough returns bars dated on or before date, oldest first.
func (s Series) BarsThrough(date string) []Bar {
	i := sort.Search(len(s.Bars), func(i int) bool { return s.Bars[i].Date > date })
	return s.Bars[:i]
}

func (s Series) DividendOn(date string) float64 {
	for _, d := range s.Dividends {
		if d.Date == date {
			return d.Amount
		}
	}
	return 0
}

func (s Series) SplitOn(date string) (Split, bool) {
	for _, sp := range s.Splits {
		if sp.Date == date {
			return sp, true
		}
	}
	return Split{}, false
}

// PriceSource downloads daily bars from Yahoo Finance's chart endpoint. It is
// an unofficial source; the raw payload of every cycle is archived.
type PriceSource struct {
	HTTP    *http.Client
	BaseURL string
}

func (m PriceSource) Fetch(ctx context.Context, symbol string) ([]byte, error) {
	base := m.BaseURL
	if base == "" {
		base = "https://query1.finance.yahoo.com"
	}
	u := base + "/v8/finance/chart/" + url.PathEscape(symbol) + "?range=1y&interval=1d&events=div%2Csplits"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; trader-agent/1.0)")
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("price data for %s: %w", symbol, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("price provider returned HTTP %d for %s", resp.StatusCode, symbol)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 20<<20))
}

type yahooResponse struct {
	Chart struct {
		Error *struct {
			Description string `json:"description"`
		} `json:"error"`
		Result []struct {
			Meta struct {
				Symbol   string `json:"symbol"`
				Currency string `json:"currency"`
				Timezone string `json:"exchangeTimezoneName"`
			} `json:"meta"`
			Timestamp  []int64 `json:"timestamp"`
			Indicators struct {
				Quote []struct {
					Open   []*float64 `json:"open"`
					High   []*float64 `json:"high"`
					Low    []*float64 `json:"low"`
					Close  []*float64 `json:"close"`
					Volume []*int64   `json:"volume"`
				} `json:"quote"`
			} `json:"indicators"`
			Events struct {
				Dividends map[string]struct {
					Date   int64   `json:"date"`
					Amount float64 `json:"amount"`
				} `json:"dividends"`
				Splits map[string]struct {
					Date        int64   `json:"date"`
					Numerator   float64 `json:"numerator"`
					Denominator float64 `json:"denominator"`
				} `json:"splits"`
			} `json:"events"`
		} `json:"result"`
	} `json:"chart"`
}

func ParseYahoo(raw []byte, symbol string) (Series, error) {
	s := Series{Symbol: symbol}
	var r yahooResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return s, fmt.Errorf("invalid price response for %s", symbol)
	}
	if r.Chart.Error != nil {
		return s, fmt.Errorf("price provider for %s: %s", symbol, r.Chart.Error.Description)
	}
	if len(r.Chart.Result) != 1 {
		return s, fmt.Errorf("price response for %s has no unique result", symbol)
	}
	res := r.Chart.Result[0]
	if !strings.EqualFold(res.Meta.Symbol, symbol) || res.Meta.Currency != "USD" || res.Meta.Timezone != "America/New_York" {
		return s, fmt.Errorf("%s is not a USD instrument on a New York exchange", symbol)
	}
	if len(res.Indicators.Quote) != 1 {
		return s, fmt.Errorf("%s: missing OHLCV series", symbol)
	}
	q := res.Indicators.Quote[0]
	n := len(res.Timestamp)
	if len(q.Open) != n || len(q.High) != n || len(q.Low) != n || len(q.Close) != n || len(q.Volume) != n {
		return s, fmt.Errorf("%s: OHLCV arrays have different lengths", symbol)
	}
	for i, ts := range res.Timestamp {
		// Rows with missing fields are incomplete (for example a session that
		// has not opened yet); the cycle checks that every needed bar exists.
		if q.Open[i] == nil || q.High[i] == nil || q.Low[i] == nil || q.Close[i] == nil || q.Volume[i] == nil {
			continue
		}
		bar := Bar{time.Unix(ts, 0).In(NewYork).Format(time.DateOnly), *q.Open[i], *q.High[i], *q.Low[i], *q.Close[i], *q.Volume[i]}
		if err := validateBar(bar); err != nil {
			return s, fmt.Errorf("%s: %w", symbol, err)
		}
		switch last := len(s.Bars) - 1; {
		case last >= 0 && s.Bars[last].Date == bar.Date:
			s.Bars[last] = bar // the vendor sometimes repeats the latest session
		case last >= 0 && s.Bars[last].Date > bar.Date:
			return s, fmt.Errorf("%s: bars are not in date order", symbol)
		default:
			s.Bars = append(s.Bars, bar)
		}
	}
	if len(s.Bars) == 0 {
		return s, fmt.Errorf("%s: no complete daily bars", symbol)
	}
	for _, d := range res.Events.Dividends {
		if d.Amount <= 0 || math.IsNaN(d.Amount) || math.IsInf(d.Amount, 0) {
			return s, fmt.Errorf("%s: invalid dividend", symbol)
		}
		s.Dividends = append(s.Dividends, Dividend{time.Unix(d.Date, 0).In(NewYork).Format(time.DateOnly), d.Amount})
	}
	for _, sp := range res.Events.Splits {
		if !(sp.Numerator > 0) || !(sp.Denominator > 0) || math.IsInf(sp.Numerator, 0) || math.IsInf(sp.Denominator, 0) {
			return s, fmt.Errorf("%s: invalid split ratio", symbol)
		}
		s.Splits = append(s.Splits, Split{time.Unix(sp.Date, 0).In(NewYork).Format(time.DateOnly), sp.Numerator, sp.Denominator})
	}
	sort.Slice(s.Dividends, func(i, j int) bool { return s.Dividends[i].Date < s.Dividends[j].Date })
	sort.Slice(s.Splits, func(i, j int) bool { return s.Splits[i].Date < s.Splits[j].Date })
	for i := 1; i < len(s.Dividends); i++ {
		if s.Dividends[i].Date == s.Dividends[i-1].Date {
			return s, fmt.Errorf("%s: duplicate dividend on %s", symbol, s.Dividends[i].Date)
		}
	}
	for i := 1; i < len(s.Splits); i++ {
		if s.Splits[i].Date == s.Splits[i-1].Date {
			return s, fmt.Errorf("%s: duplicate split on %s", symbol, s.Splits[i].Date)
		}
	}
	return s, nil
}

func validateBar(b Bar) error {
	for _, v := range []float64{b.Open, b.High, b.Low, b.Close} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0.01 || v > 1e7 {
			return fmt.Errorf("invalid price on %s", b.Date)
		}
	}
	// Opening and closing auction prints can sit marginally outside the
	// continuous-trading range, so allow a small relative tolerance.
	tol := 0.002 * b.High
	if b.Volume < 0 || b.Low > b.High || b.Low > b.Open+tol || b.Low > b.Close+tol || b.High+tol < b.Open || b.High+tol < b.Close {
		return fmt.Errorf("invalid OHLCV relationships on %s", b.Date)
	}
	return nil
}

// RequireBars fails unless every symbol has a bar for every listed session.
func RequireBars(series map[string]Series, symbols []string, sessions []Session) error {
	var missing []string
	for _, symbol := range symbols {
		for _, s := range sessions {
			if _, ok := series[symbol].BarOn(s.Date); !ok {
				missing = append(missing, symbol+" "+s.Date)
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s", ErrDataNotReady, strings.Join(missing, ", "))
	}
	return nil
}

// ErrDataNotReady means the vendor has not published a required bar yet.
var ErrDataNotReady = errors.New("price data not yet available")
