package market

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func yahooFixture() map[string]any {
	day := func(date string) int64 {
		d, _ := time.ParseInLocation(time.DateOnly, date, NewYork)
		return d.Add(9*time.Hour + 30*time.Minute).Unix()
	}
	return map[string]any{"chart": map[string]any{"error": nil, "result": []any{map[string]any{
		"meta":      map[string]any{"symbol": "AAA", "currency": "USD", "exchangeTimezoneName": "America/New_York"},
		"timestamp": []int64{day("2026-10-22"), day("2026-10-23"), day("2026-10-23"), day("2026-10-26")},
		"indicators": map[string]any{"quote": []any{map[string]any{
			"open": []any{100, 101, 101, nil}, "high": []any{102, 103, 104, nil}, "low": []any{99, 100, 100, nil},
			"close": []any{101, 102, 103.5, nil}, "volume": []any{1000, 1100, 1200, nil},
		}}},
		"events": map[string]any{
			"dividends": map[string]any{"1": map[string]any{"date": day("2026-10-22"), "amount": 0.25}},
			"splits":    map[string]any{"2": map[string]any{"date": day("2026-10-23"), "numerator": 4, "denominator": 1}},
		},
	}}}}
}

func TestParseYahooKeepsCompleteBarsAndCorporateActions(t *testing.T) {
	raw, _ := json.Marshal(yahooFixture())
	s, err := ParseYahoo(raw, "AAA")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Bars) != 2 || s.Bars[1].Close != 103.5 {
		t.Fatalf("the incomplete row is skipped and a repeated date keeps the latest: %+v", s.Bars)
	}
	if s.DividendOn("2026-10-22") != 0.25 {
		t.Fatal("dividend not parsed on its ex-date")
	}
	if sp, ok := s.SplitOn("2026-10-23"); !ok || sp.Numerator != 4 {
		t.Fatal("split not parsed")
	}
	if got := s.BarsThrough("2026-10-22"); len(got) != 1 {
		t.Fatalf("BarsThrough must exclude later sessions: %+v", got)
	}
}

func TestParseYahooRejectsWrongInstruments(t *testing.T) {
	for name, edit := range map[string]func(map[string]any){
		"currency": func(meta map[string]any) { meta["currency"] = "EUR" },
		"exchange": func(meta map[string]any) { meta["exchangeTimezoneName"] = "Europe/London" },
		"symbol":   func(meta map[string]any) { meta["symbol"] = "ZZZ" },
	} {
		fx := yahooFixture()
		edit(fx["chart"].(map[string]any)["result"].([]any)[0].(map[string]any)["meta"].(map[string]any))
		raw, _ := json.Marshal(fx)
		if _, err := ParseYahoo(raw, "AAA"); err == nil {
			t.Fatalf("%s mismatch accepted", name)
		}
	}
	fx := yahooFixture()
	q := fx["chart"].(map[string]any)["result"].([]any)[0].(map[string]any)["indicators"].(map[string]any)["quote"].([]any)[0].(map[string]any)
	q["low"] = []any{99, 110, 100, nil}
	raw, _ := json.Marshal(fx)
	if _, err := ParseYahoo(raw, "AAA"); err == nil || !strings.Contains(err.Error(), "OHLCV") {
		t.Fatalf("a low above the high must be rejected: %v", err)
	}
}

func FuzzParseYahooNeverPanics(f *testing.F) {
	raw, _ := json.Marshal(yahooFixture())
	f.Add(raw)
	f.Add([]byte(`{"chart":{"result":[{"timestamp":[1],"indicators":{"quote":[{}]}}]}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		s, err := ParseYahoo(data, "AAA")
		if err != nil {
			return
		}
		for i, b := range s.Bars {
			if validateBar(b) != nil || (i > 0 && s.Bars[i-1].Date >= b.Date) {
				t.Fatalf("invalid bar accepted: %+v", b)
			}
		}
	})
}

func TestRequireBarsReportsUnpublishedSessions(t *testing.T) {
	s := map[string]Series{"AAA": {Bars: []Bar{{Date: "2026-10-22"}}}}
	sessions, _ := NYSESessions("2026-10-22", "2026-10-23")
	if err := RequireBars(s, []string{"AAA"}, sessions); err == nil || !strings.Contains(err.Error(), "AAA 2026-10-23") {
		t.Fatalf("missing bar not reported: %v", err)
	}
}

func TestIndicatorsUseOnlyTheSuppliedHistory(t *testing.T) {
	var bars []Bar
	for i := 0; i < 70; i++ {
		p := 100 + float64(i)
		bars = append(bars, Bar{Date: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format(time.DateOnly), Open: p, High: p + 1, Low: p - 1, Close: p, Volume: 1000})
	}
	ind, err := ComputeIndicators("AAA", "prices:AAA", bars)
	if err != nil {
		t.Fatal(err)
	}
	if ind.Close != 169 || ind.Return5D != round(169.0/164-1, 5) || ind.High1Y != 170 || ind.Low1Y != 99 || ind.VolumeRatio != 1 {
		t.Fatalf("indicators: %+v", ind)
	}
	if _, err := ComputeIndicators("AAA", "prices:AAA", bars[:60]); err == nil {
		t.Fatal("too little history accepted")
	}
}

func TestNativeFactorRestoresHistoricalShareUnits(t *testing.T) {
	s := Series{Splits: []Split{{"2026-10-28", 2, 1}}, Dividends: []Dividend{{"2026-10-26", 0.5}, {"2026-10-29", 0.25}}}
	if f := s.NativeFactor("2026-10-27"); f != 2 {
		t.Fatalf("a bar before a 2-for-1 split is worth twice its adjusted price: %v", f)
	}
	if f := s.NativeFactor("2026-10-28"); f != 1 {
		t.Fatalf("on the ex-date prices are already in new units: %v", f)
	}
	if got := s.DividendsBetween("2026-10-26", "2026-10-29"); got != 0.25 {
		t.Fatalf("ex-date on the entry day is excluded: %v", got)
	}
}

func TestFetchRequestsTheNeededRange(t *testing.T) {
	var query url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { query = r.URL.Query() }))
	defer srv.Close()
	from := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	PriceSource{HTTP: srv.Client(), BaseURL: srv.URL}.Fetch(context.Background(), "AAA", from)
	if query.Get("period1") != strconv.FormatInt(from.Unix(), 10) || query.Get("range") != "" {
		t.Fatalf("expected an explicit start, got %v", query)
	}
}
