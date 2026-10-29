package server

import (
	"encoding/json"
	"net/http"

	"github.com/candylario/trader/api"
	"github.com/candylario/trader/internal/fleet"
	"github.com/candylario/trader/internal/hyperliquid"
)

func walletOut(w fleet.Wallet, acct *hyperliquid.Account) api.Wallet {
	out := api.Wallet{ID: w.ID.String(), AgentAddress: w.Agent, User: w.User, Mode: w.Mode, AgentApproved: w.Approved(), BuilderFee: w.BuilderFee}
	if acct != nil {
		a := &api.WalletAccount{Value: acct.Value, Withdrawable: acct.Withdrawable, Positions: []api.PerpPosition{}}
		for _, p := range acct.Positions {
			a.Positions = append(a.Positions, api.PerpPosition{Coin: p.Coin, Size: p.Size, EntryPx: p.EntryPx, Value: p.Value, UnrealizedPnl: p.UnrealizedPnl, LiquidationPx: p.LiquidationPx, Leverage: p.Leverage, Cross: p.Cross})
		}
		out.Account = a
	}
	return out
}

// createWallet makes the account's agent wallet, or returns the one it has.
func (s *Server) createWallet(w http.ResponseWriter, r *http.Request) (int, any, error) {
	ids, err := pathIDs(r, "tenant")
	if err != nil {
		return 0, nil, err
	}
	if _, err := s.Fleet.Store.Tenant(r.Context(), ids[0]); err != nil {
		return 0, nil, err
	}
	wl, isNew, err := s.Fleet.Wallet(r.Context(), ids[0])
	if err != nil {
		return 0, nil, err
	}
	return created(isNew), walletOut(wl, nil), nil
}

// getWallet reports the wallet and, once approved, the account.
func (s *Server) getWallet(w http.ResponseWriter, r *http.Request) (int, any, error) {
	ids, err := pathIDs(r, "tenant")
	if err != nil {
		return 0, nil, err
	}
	wl, acct, err := s.Fleet.WalletAccount(r.Context(), ids[0])
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, walletOut(wl, acct), nil
}

// walletApprovals gives the typed data the account's wallet signs.
func (s *Server) walletApprovals(w http.ResponseWriter, r *http.Request) (int, any, error) {
	ids, err := pathIDs(r, "tenant")
	if err != nil {
		return 0, nil, err
	}
	if _, err := s.Fleet.Store.Tenant(r.Context(), ids[0]); err != nil {
		return 0, nil, err
	}
	a, err := s.Fleet.WalletApprovals(r.Context(), ids[0], r.URL.Query().Get("user"), r.URL.Query().Get("mode"))
	if err != nil {
		return 0, nil, err
	}
	out := api.WalletApprovals{User: a.User, Mode: a.Mode, AgentAddress: a.Agent}
	if out.ApproveAgent, err = json.Marshal(a.ApproveAgent); err != nil {
		return 0, nil, err
	}
	if a.ApproveBuilderFee != nil {
		if out.ApproveBuilderFee, err = json.Marshal(a.ApproveBuilderFee); err != nil {
			return 0, nil, err
		}
	}
	return http.StatusOK, out, nil
}

func signed(a api.SignedAction) fleet.Signed {
	return fleet.Signed{Action: a.Action, Nonce: a.Nonce, Signature: hyperliquid.Signature{R: a.Signature.R, S: a.Signature.S, V: a.Signature.V}}
}

// approveWallet takes the signed approvals and confirms them with the
// exchange.
func (s *Server) approveWallet(w http.ResponseWriter, r *http.Request) (int, any, error) {
	ids, err := pathIDs(r, "tenant")
	if err != nil {
		return 0, nil, err
	}
	var in api.WalletApproval
	if err := decode(w, r, &in); err != nil {
		return 0, nil, err
	}
	var builder *fleet.Signed
	if in.ApproveBuilderFee != nil {
		b := signed(*in.ApproveBuilderFee)
		builder = &b
	}
	wl, err := s.Fleet.ApproveWallet(r.Context(), ids[0], in.User, in.Mode, signed(in.ApproveAgent), builder)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, walletOut(wl, nil), nil
}
