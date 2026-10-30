package server

import (
	"net/http"

	"github.com/candylario/trader/api"
	"github.com/candylario/trader/internal/fleet"
)

// openTrade opens a position with its stops in one batch.
func (s *Server) openTrade(w http.ResponseWriter, r *http.Request) (int, any, error) {
	ids, err := pathIDs(r, "tenant")
	if err != nil {
		return 0, nil, err
	}
	var in api.NewTrade
	if err := decode(w, r, &in); err != nil {
		return 0, nil, err
	}
	t, err := s.Fleet.OpenTrade(r.Context(), ids[0], fleet.TradeRequest{Coin: in.Coin, Side: in.Side, USD: in.USD, Leverage: in.Leverage,
		StopLossPct: in.StopLossPct, TakeProfitPct: in.TakeProfitPct, SlippagePct: in.SlippagePct})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, api.Trade{Coin: t.Coin, Side: t.Side, Size: t.Size, EntryPx: t.EntryPx, USD: t.USD, Leverage: t.Leverage, OID: t.OID,
		StopPx: t.StopPx, StopOID: t.StopOID, TakeProfitPx: t.TakeProfitPx, TakeProfitOID: t.TakeProfitOID}, nil
}

// closeTrade closes a position and cancels its stops.
func (s *Server) closeTrade(w http.ResponseWriter, r *http.Request) (int, any, error) {
	ids, err := pathIDs(r, "tenant")
	if err != nil {
		return 0, nil, err
	}
	var in api.CloseTrade
	if err := decode(w, r, &in); err != nil {
		return 0, nil, err
	}
	c, err := s.Fleet.CloseTrade(r.Context(), ids[0], in.Coin, in.SlippagePct)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, api.ClosedTrade{Coin: c.Coin, Size: c.Size, AvgPx: c.AvgPx, Remaining: c.Remaining, StopsCancelled: c.StopsCancelled}, nil
}
