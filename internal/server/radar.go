package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/candylario/trader/api"
	"github.com/candylario/trader/internal/radar"
)

func radarOut(e radar.Entry) api.RadarAlert {
	reasons := e.Reasons
	if reasons == nil {
		reasons = []string{}
	}
	return api.RadarAlert{Seq: e.Seq, At: e.At, Coin: e.Coin, Price: e.Price, Change15m: e.Change15m, Change1h: e.Change1h, Change24h: e.Change24h,
		Sigma15m: e.Sigma15m, VolumeRate: e.VolumeRate, Volume24hUSD: e.Volume24hUSD, OIChange1h: e.OIChange1h, Funding: e.Funding, Score: e.Score, Reasons: reasons, Text: e.String(), History: e.History}
}

func (s *Server) radarOn() error {
	if s.Radar == nil {
		return &apiError{http.StatusServiceUnavailable, "radar_off", "the radar is not running"}
	}
	return nil
}

// radarAlerts lists the latest alerts, or those after a seq.
func (s *Server) radarAlerts(w http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := s.radarOn(); err != nil {
		return 0, nil, err
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			return 0, nil, badRequest("limit must be 1–500")
		}
		limit = n
	}
	var entries []radar.Entry
	if r.URL.Query().Get("after") != "" {
		seq, err := after(r)
		if err != nil {
			return 0, nil, err
		}
		entries = s.Radar.After(seq, limit)
	} else {
		entries = s.Radar.Last(limit)
	}
	out := api.RadarAlertList{Alerts: make([]api.RadarAlert, len(entries))}
	for i, e := range entries {
		out.Alerts[i] = radarOut(e)
	}
	return http.StatusOK, out, nil
}

// radarStream sends alerts as server-sent events as they are raised.
func (s *Server) radarStream(w http.ResponseWriter, r *http.Request) {
	if err := s.radarOn(); err != nil {
		s.fail(w, r, err)
		return
	}
	seq, err := after(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	wake, done := s.Radar.Subscribe()
	defer done()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	rc.Flush()
	for {
		entries := s.Radar.After(seq, 500)
		for _, e := range entries {
			raw, _ := json.Marshal(radarOut(e))
			fmt.Fprintf(w, "id: %d\nevent: radar.alert\ndata: %s\n\n", e.Seq, raw)
			seq = e.Seq
		}
		if len(entries) > 0 {
			if rc.Flush() != nil {
				return
			}
			continue
		}
		select {
		case <-r.Context().Done():
			return
		case <-wake:
		case <-time.After(15 * time.Second):
			io.WriteString(w, ": waiting\n\n")
			if rc.Flush() != nil {
				return
			}
		}
	}
}
