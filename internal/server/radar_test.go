package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/candylario/trader/api"
	"github.com/candylario/trader/internal/radar"
)

func TestTheRadarIsListedAndStreamed(t *testing.T) {
	rl := &radar.Log{}
	token := strings.Repeat("x", 32)
	s := &Server{Token: token, NotifierToken: strings.Repeat("n", 32), Radar: rl, Logf: t.Logf}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	get := func(path, bearer string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	rl.Add(radar.Alert{At: time.Date(2026, 10, 29, 20, 0, 0, 0, time.UTC), Coin: "HYPE", Price: 81.4, Change15m: 0.052, Change1h: 0.07, VolumeRate: 6.2, Reasons: []string{"6× the usual volume"}})
	resp := get("/v1/radar/alerts", s.NotifierToken)
	var list api.RadarAlertList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("alerts: %d %v", resp.StatusCode, err)
	}
	if len(list.Alerts) != 1 || list.Alerts[0].Seq != 1 || list.Alerts[0].Coin != "HYPE" || !strings.HasPrefix(list.Alerts[0].Text, "🚀 HYPE +5.2% in 15m") {
		t.Fatalf("alerts %+v", list.Alerts)
	}
	if resp := get("/v1/radar/alerts?after=1", token); resp.StatusCode != http.StatusOK {
		t.Fatalf("after: %d", resp.StatusCode)
	} else {
		json.NewDecoder(resp.Body).Decode(&list)
		if len(list.Alerts) != 0 {
			t.Fatalf("after 1: %+v", list.Alerts)
		}
	}

	resp = get("/v1/radar/stream", s.NotifierToken)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	defer resp.Body.Close()
	lines := bufio.NewScanner(resp.Body)
	next := func() (id, data string) {
		for lines.Scan() {
			switch line := lines.Text(); {
			case strings.HasPrefix(line, "id: "):
				id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				return id, strings.TrimPrefix(line, "data: ")
			}
		}
		t.Fatalf("stream ended: %v", lines.Err())
		return
	}
	if id, data := next(); id != "1" || !strings.Contains(data, `"coin":"HYPE"`) {
		t.Fatalf("first event %s %s", id, data)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		rl.Add(radar.Alert{Coin: "SOL", Change15m: -0.04, Reasons: []string{"open interest -12.0% in 1h"}})
	}()
	if id, data := next(); id != "2" || !strings.Contains(data, `"coin":"SOL"`) || !strings.Contains(data, `🔻 SOL`) {
		t.Fatalf("second event %s %s", id, data)
	}

	off := httptest.NewServer((&Server{Token: token, Logf: t.Logf}).Handler())
	defer off.Close()
	req, _ := http.NewRequest(http.MethodGet, off.URL+"/v1/radar/alerts", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("radar off: %v %v", resp, err)
	}
}
