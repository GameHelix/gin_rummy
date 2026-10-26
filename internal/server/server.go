// Package server answers the engine API that the app uses: tenants and
// their plans, keys into the vault, agents and their configs, commands,
// status, and each agent's audit log as history and as a live stream. The
// contract is in package api.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/candylario/trader/api"
	"github.com/candylario/trader/internal/agent"
	"github.com/candylario/trader/internal/audit"
	"github.com/candylario/trader/internal/fleet"
	"github.com/candylario/trader/internal/pg"
)

type Server struct {
	Fleet  *fleet.Service
	Agents *fleet.Supervisor
	// Report values an agent that is not running from its saved account.
	Report func(ctx context.Context, spec fleet.Spec) (agent.Report, error)
	Token  string
	Logf   func(format string, args ...any)

	hub hub
}

// Serve answers on the listener until ctx ends, then closes every stream.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	go s.listen(ctx)
	hs := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(l) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return hs.Shutdown(sctx)
	}
}

// listen wakes the streams of agents whose events were just stored.
func (s *Server) listen(ctx context.Context) {
	for ctx.Err() == nil {
		if err := s.Fleet.Store.Listen(ctx, s.hub.wake); ctx.Err() == nil {
			s.Logf("event notifications: %v", err)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
		}
	}
}

type route struct {
	pattern string
	open    bool // answered without the token
	json    func(w http.ResponseWriter, r *http.Request) (int, any, error)
	raw     http.HandlerFunc
}

func (s *Server) routes() []route {
	return []route{
		{pattern: "GET /v1/health", open: true, json: s.health},
		{pattern: "GET /v1/openapi.yaml", open: true, raw: spec},
		{pattern: "POST /v1/tenants", json: s.createTenant},
		{pattern: "GET /v1/tenants/{tenant}", json: s.getTenant},
		{pattern: "PUT /v1/tenants/{tenant}/plan", json: s.setPlan},
		{pattern: "POST /v1/tenants/{tenant}/keys", json: s.addKey},
		{pattern: "GET /v1/tenants/{tenant}/keys", json: s.listKeys},
		{pattern: "DELETE /v1/tenants/{tenant}/keys/{key}", json: s.revokeKey},
		{pattern: "DELETE /v1/tenants/{tenant}", json: s.closeTenant},
		{pattern: "POST /v1/tenants/{tenant}/agents", json: s.createAgent},
		{pattern: "GET /v1/tenants/{tenant}/agents", json: s.listAgents},
		{pattern: "GET /v1/tenants/{tenant}/agents/{agent}", json: s.getAgent},
		{pattern: "GET /v1/tenants/{tenant}/agents/{agent}/config", json: s.getConfig},
		{pattern: "PUT /v1/tenants/{tenant}/agents/{agent}/config", json: s.setConfig},
		{pattern: "PUT /v1/tenants/{tenant}/agents/{agent}/keys/{key}", json: s.attachKey},
		{pattern: "POST /v1/tenants/{tenant}/agents/{agent}/start", json: s.start},
		{pattern: "POST /v1/tenants/{tenant}/agents/{agent}/stop", json: s.stop},
		{pattern: "GET /v1/tenants/{tenant}/agents/{agent}/status", json: s.status},
		{pattern: "POST /v1/tenants/{tenant}/agents/{agent}/commands", json: s.command},
		{pattern: "GET /v1/tenants/{tenant}/agents/{agent}/events", json: s.events},
		{pattern: "GET /v1/tenants/{tenant}/agents/{agent}/events/stream", raw: s.stream},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		mux.HandleFunc(rt.pattern, func(w http.ResponseWriter, r *http.Request) {
			if !rt.open && !s.authorized(r) {
				s.fail(w, r, &apiError{http.StatusUnauthorized, "unauthorized", "a valid bearer token is required"})
				return
			}
			if rt.raw != nil {
				rt.raw(w, r)
				return
			}
			status, body, err := rt.json(w, r)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			if r.Method != http.MethodGet {
				s.Logf("api %s %d", r.Pattern, status)
			}
			writeJSON(w, status, body)
		})
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.fail(w, r, &apiError{http.StatusNotFound, "not_found", "no such endpoint"})
	})
	return mux
}

func (s *Server) authorized(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(s.Token))
	return ok && s.Token != "" && subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// apiError is a failure with its HTTP status and code.
type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string { return e.message }

func badRequest(format string, args ...any) error {
	return &apiError{http.StatusBadRequest, "bad_request", fmt.Sprintf(format, args...)}
}

// fail answers with the error. Messages of unexpected errors stay in the
// engine's log, never in the answer.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var ae *apiError
	var refused *fleet.Refused
	switch {
	case errors.As(err, &ae):
	case errors.As(err, &refused):
		ae = &apiError{http.StatusUnprocessableEntity, "refused", refused.Error()}
	case errors.Is(err, pg.ErrNotFound):
		ae = &apiError{http.StatusNotFound, "not_found", "no such tenant, agent, key or version"}
	case errors.Is(err, pg.ErrExists):
		ae = &apiError{http.StatusConflict, "conflict", "that ID is already taken"}
	case errors.Is(err, agent.ErrNotRunning):
		ae = &apiError{http.StatusConflict, "not_running", "the agent is not running; start it first"}
	case errors.Is(err, context.DeadlineExceeded):
		ae = &apiError{http.StatusGatewayTimeout, "timeout", "the agent did not answer in time; the command may still run"}
	default:
		s.Logf("api %s: %v", r.Pattern, err)
		ae = &apiError{http.StatusInternalServerError, "internal", "internal error"}
	}
	if r.Method != http.MethodGet || ae.status >= 500 {
		s.Logf("api %s %d %s", r.Pattern, ae.status, ae.code)
	}
	writeJSON(w, ae.status, map[string]any{"error": api.Error{Code: ae.code, Message: ae.message}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// decode reads a JSON body strictly: no unknown fields, nothing after it.
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return badRequest("invalid JSON body: %v", err)
	}
	if d.More() {
		return badRequest("invalid JSON body: data after the object")
	}
	return nil
}

func pathIDs(r *http.Request, names ...string) ([]uuid.UUID, error) {
	out := make([]uuid.UUID, len(names))
	for i, n := range names {
		id, err := uuid.Parse(r.PathValue(n))
		if err != nil {
			return nil, badRequest("%s must be a UUID", n)
		}
		out[i] = id
	}
	return out, nil
}

// optionalID parses an ID the client chose, if any.
func optionalID(s string) (uuid.UUID, error) {
	if s == "" {
		return uuid.Nil, nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, badRequest("id must be a UUID")
	}
	return id, nil
}

func created(isNew bool) int {
	if isNew {
		return http.StatusCreated
	}
	return http.StatusOK
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) (int, any, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.Fleet.Store.Ping(ctx); err != nil {
		return 0, nil, &apiError{http.StatusServiceUnavailable, "internal", "the database does not answer"}
	}
	return http.StatusOK, map[string]bool{"ok": true}, nil
}

func spec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.Write(api.Spec)
}

func planIn(p api.Plan) fleet.Entitlements {
	return fleet.Entitlements{Symbols: p.Symbols, MaxAgents: p.MaxAgents, MinDecisionIntervalHours: p.MinDecisionIntervalHours,
		MaxEventDecisionsPerDay: p.MaxEventDecisionsPerDay, Live: p.Live, Models: p.Models}
}

func tenantOut(t pg.Tenant) (api.Tenant, error) {
	out := api.Tenant{ID: t.ID.String(), Name: t.Name, Status: t.Status}
	return out, json.Unmarshal(t.Entitlements, &out.Plan)
}

func (s *Server) createTenant(w http.ResponseWriter, r *http.Request) (int, any, error) {
	var in api.NewTenant
	if err := decode(w, r, &in); err != nil {
		return 0, nil, err
	}
	id, err := optionalID(in.ID)
	if err != nil {
		return 0, nil, err
	}
	t, isNew, err := s.Fleet.AddTenant(r.Context(), id, in.Name, planIn(in.Plan))
	if err != nil {
		return 0, nil, err
	}
	out, err := tenantOut(t)
	return created(isNew), out, err
}

func (s *Server) getTenant(w http.ResponseWriter, r *http.Request) (int, any, error) {
	ids, err := pathIDs(r, "tenant")
	if err != nil {
		return 0, nil, err
	}
	t, err := s.Fleet.Store.Tenant(r.Context(), ids[0])
	if err != nil {
		return 0, nil, err
	}
	out, err := tenantOut(t)
	return http.StatusOK, out, err
}

func (s *Server) setPlan(w http.ResponseWriter, r *http.Request) (int, any, error) {
	ids, err := pathIDs(r, "tenant")
	if err != nil {
		return 0, nil, err
	}
	var p api.Plan
	if err := decode(w, r, &p); err != nil {
		return 0, nil, err
	}
	stopped, err := s.Fleet.SetEntitlements(r.Context(), ids[0], planIn(p))
	if err != nil {
		return 0, nil, err
	}
	out := api.PlanChange{Plan: p, Stopped: []api.Stopped{}}
	for id, why := range stopped {
		out.Stopped = append(out.Stopped, api.Stopped{AgentID: id.String(), Reason: why})
	}
	slices.SortFunc(out.Stopped, func(a, b api.Stopped) int { return strings.Compare(a.AgentID, b.AgentID) })
	return http.StatusOK, out, nil
}

func keyOut(c pg.Credential) api.Key {
	return api.Key{ID: c.ID.String(), Kind: c.Kind, Label: c.Label, Hint: c.Hint, CreatedAt: c.CreatedAt.UTC(), Revoked: c.Revoked}
}

func (s *Server) addKey(w http.ResponseWriter, r *http.Request) (int, any, error) {
	ids, err := pathIDs(r, "tenant")
	if err != nil {
		return 0, nil, err
	}
	var in api.NewKey
	if err := decode(w, r, &in); err != nil {
		return 0, nil, err
	}
	defer clear(in.Secret)
	id, err := optionalID(in.ID)
	if err != nil {
		return 0, nil, err
	}
	if _, err := s.Fleet.Store.Tenant(r.Context(), ids[0]); err != nil {
		return 0, nil, err
	}
	c, isNew, err := s.Fleet.AddKey(r.Context(), ids[0], fleet.NewKey{ID: id, Kind: in.Kind, Label: in.Label, Mode: in.Mode, Secret: in.Secret})
	if err != nil {
		return 0, nil, err
	}
	return created(isNew), keyOut(c), nil
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) (int, any, error) {
	ids, err := pathIDs(r, "tenant")
	if err != nil {
		return 0, nil, err
	}
	if _, err := s.Fleet.Store.Tenant(r.Context(), ids[0]); err != nil {
		return 0, nil, err
	}
	keys, err := s.Fleet.Store.Credentials(r.Context(), ids[0])
	out := api.KeyList{Keys: []api.Key{}}
	for _, c := range keys {
		out.Keys = append(out.Keys, keyOut(c))
	}
	return http.StatusOK, out, err
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) (int, any, error) {
	ids, err := pathIDs(r, "tenant", "key")
	if err != nil {
		return 0, nil, err
	}
	stopped, err := s.Fleet.RevokeKey(r.Context(), ids[0], ids[1])
	out := api.Revoked{Stopped: []api.Stopped{}}
	for _, id := range stopped {
		out.Stopped = append(out.Stopped, api.Stopped{AgentID: id.String(), Reason: "its key was revoked"})
	}
	return http.StatusOK, out, err
}

func (s *Server) closeTenant(w http.ResponseWriter, r *http.Request) (int, any, error) {
	ids, err := pathIDs(r, "tenant")
	if err != nil {
		return 0, nil, err
	}
	stopped, err := s.Fleet.CloseTenant(r.Context(), ids[0])
	out := api.Closed{Stopped: []api.Stopped{}}
	for _, id := range stopped {
		out.Stopped = append(out.Stopped, api.Stopped{AgentID: id.String(), Reason: "its account was closed"})
	}
	return http.StatusOK, out, err
}

// agentOut describes an agent with its keys and whether it runs now.
func (s *Server) agentOut(ctx context.Context, row pg.AgentRow) (api.Agent, error) {
	attached, err := s.Fleet.Store.Attached(ctx, row.Tenant, row.ID)
	out := api.Agent{ID: row.ID.String(), Name: row.Name, Mode: row.Mode, Status: row.Status, Version: row.Version,
		Running: s.Agents.Running(row.Tenant, row.ID), Keys: map[string]string{}}
	for kind, id := range attached {
		out.Keys[kind] = id.String()
	}
	return out, err
}

func (s *Server) agentReply(ctx context.Context, tenant, id uuid.UUID) (int, any, error) {
	row, err := s.Fleet.Store.Agent(ctx, tenant, id)
	if err != nil {
		return 0, nil, err
	}
	out, err := s.agentOut(ctx, row)
	return http.StatusOK, out, err
}

func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) (int, any, error) {
	ids, err := pathIDs(r, "tenant")
	if err != nil {
		return 0, nil, err
	}
	var in api.NewAgent
	if err := decode(w, r, &in); err != nil {
		return 0, nil, err
	}
	id, err := optionalID(in.ID)
	if err != nil {
		return 0, nil, err
	}
	var keys []uuid.UUID
	for _, k := range in.Keys {
		kid, err := uuid.Parse(k)
		if err != nil {
			return 0, nil, badRequest("keys must be UUIDs")
		}
		keys = append(keys, kid)
	}
	row, isNew, err := s.Fleet.AddAgent(r.Context(), ids[0], fleet.NewAgent{ID: id, Name: in.Name, Mode: in.Mode, Config: in.Config, Keys: keys, By: "api"})
	if err != nil {
		return 0, nil, err
	}
	out, err := s.agentOut(r.Context(), row)
	return created(isNew), out, err
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) (int, any, error) {
	ids, err := pathIDs(r, "tenant")
	if err != nil {
		return 0, nil, err
	}
	if _, err := s.Fleet.Store.Tenant(r.Context(), ids[0]); err != nil {
		return 0, nil, err
	}
	rows, err := s.Fleet.Store.Agents(r.Context(), ids[0])
	if err != nil {
		return 0, nil, err
	}
	out := api.AgentList{Agents: []api.Agent{}}
	for _, row := range rows {
		a, err := s.agentOut(r.Context(), row)
		if err != nil {
			return 0, nil, err
		}
		out.Agents = append(out.Agents, a)
	}
	return http.StatusOK, out, nil
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request) (int, any, error) {
	ids, err := pathIDs(r, "tenant", "agent")
	if err != nil {
		return 0, nil, err
	}
	return s.agentReply(r.Context(), ids[0], ids[1])
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) (int, any, error) {
	ids, err := pathIDs(r, "tenant", "agent")
	if err != nil {
		return 0, nil, err
	}
	row, err := s.Fleet.Store.Agent(r.Context(), ids[0], ids[1])
	if err != nil {
		return 0, nil, err
	}
	version := row.Version
	if v := r.URL.Query().Get("version"); v != "" {
		if version, err = strconv.Atoi(v); err != nil || version < 1 {
			return 0, nil, badRequest("version must be a positive number")
		}
	}
	raw, err := s.Fleet.Store.Version(r.Context(), ids[0], ids[1], version)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, api.Config{Version: version, Config: raw}, nil
}

func (s *Server) setConfig(w http.ResponseWriter, r *http.Request) (int, any, error) {
	ids, err := pathIDs(r, "tenant", "agent")
	if err != nil {
		return 0, nil, err
	}
	var raw json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&raw); err != nil || !strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		return 0, nil, badRequest("the body must be the agent's config as a JSON object")
	}
	version, err := s.Fleet.SetConfig(r.Context(), ids[0], ids[1], raw, "api")
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, api.Config{Version: version, Config: raw}, nil
}

func (s *Server) attachKey(w http.ResponseWriter, r *http.Request) (int, any, error) {
	ids, err := pathIDs(r, "tenant", "agent", "key")
	if err != nil {
		return 0, nil, err
	}
	if err := s.Fleet.AttachKey(r.Context(), ids[0], ids[1], ids[2]); err != nil {
		return 0, nil, err
	}
	return s.agentReply(r.Context(), ids[0], ids[1])
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) (int, any, error) {
	ids, err := pathIDs(r, "tenant", "agent")
	if err != nil {
		return 0, nil, err
	}
	if err := s.Fleet.Start(r.Context(), ids[0], ids[1]); err != nil {
		return 0, nil, err
	}
	return s.agentReply(r.Context(), ids[0], ids[1])
}

func (s *Server) stop(w http.ResponseWriter, r *http.Request) (int, any, error) {
	ids, err := pathIDs(r, "tenant", "agent")
	if err != nil {
		return 0, nil, err
	}
	if err := s.Fleet.Stop(r.Context(), ids[0], ids[1]); err != nil {
		return 0, nil, err
	}
	return s.agentReply(r.Context(), ids[0], ids[1])
}

func statusOut(row pg.AgentRow, running bool, r agent.Report) api.Status {
	out := api.Status{AgentID: row.ID.String(), Name: row.Name, Mode: row.Mode, Running: running, Trading: api.TradingOn, StopReason: r.Halted,
		Capital: r.Capital.String(), Equity: r.Equity.String(), BuyAndHold: r.BuyAndHold.String(), Cash: r.Cash.String(), Fees: r.FeesUSD.String(),
		Positions: []api.Position{}, Trades: r.Trades, PendingOrders: r.Pending, GradedCalls: r.Calls, RightCalls: r.Hits, At: r.Now, Message: r.Text()}
	switch {
	case r.Selling:
		out.Trading = api.TradingSelling
	case r.Halted != "":
		out.Trading = api.TradingStopped
	}
	for _, p := range r.Positions {
		out.Positions = append(out.Positions, api.Position{Symbol: p.Symbol, Coin: p.Coin, Qty: p.Qty.String(), Value: p.Value.String(), Weight: p.Weight})
	}
	if d := r.Last; d != nil {
		out.LastDecision = &api.Decision{ID: d.ID, At: d.Time, Kind: d.Kind, Trigger: d.Trigger, Summary: d.Summary, CostUSD: d.CostUSD, Abstained: d.Abstained, Ratings: map[string]api.Rating{}}
		for sym, sig := range d.Signals {
			out.LastDecision.Ratings[sym] = api.Rating{Rating: string(sig.Rating), Confidence: sig.Confidence, Rationale: sig.Rationale}
		}
		if r.Halted == "" {
			next := r.NextAt
			out.NextDecision = &next
		}
	}
	if b := r.Budget; b != nil {
		out.AISpend = &api.AISpend{TodayUSD: b.TodayUSD, MonthUSD: b.MonthUSD, DailyLimitUSD: b.DailyLimitUSD, MonthlyLimitUSD: b.MonthlyLimitUSD}
	}
	return out
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) (int, any, error) {
	ids, err := pathIDs(r, "tenant", "agent")
	if err != nil {
		return 0, nil, err
	}
	row, err := s.Fleet.Store.Agent(r.Context(), ids[0], ids[1])
	if err != nil {
		return 0, nil, err
	}
	running := s.Agents.Running(ids[0], ids[1])
	if running {
		qctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		rep, err := s.Agents.Status(qctx, ids[0], ids[1])
		cancel()
		if err == nil {
			return http.StatusOK, statusOut(row, true, rep), nil
		}
		// A busy or stopping agent is read from its saved account instead.
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	spec, err := s.Fleet.Settings(ctx, ids[0], ids[1])
	if err != nil {
		return 0, nil, err
	}
	if _, err := os.Stat(agent.StatePath(spec.Config)); errors.Is(err, os.ErrNotExist) {
		return 0, nil, &apiError{http.StatusConflict, "no_account", "the agent has no account yet; start it first"}
	}
	rep, err := s.Report(ctx, spec)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, statusOut(row, running, rep), nil
}

// commands maps the API's command names to the agent's, which are the
// chat's.
var commands = map[string]string{api.Decide: "decide", api.StopTrading: "stop", api.ResumeTrading: "resume", api.SellAll: "sellall"}

func (s *Server) command(w http.ResponseWriter, r *http.Request) (int, any, error) {
	ids, err := pathIDs(r, "tenant", "agent")
	if err != nil {
		return 0, nil, err
	}
	var in api.Command
	if err := decode(w, r, &in); err != nil {
		return 0, nil, err
	}
	name, ok := commands[in.Command]
	if !ok {
		return 0, nil, badRequest("command must be decide, stop_trading, resume_trading or sell_all")
	}
	if in.Via == "" {
		in.Via = "app"
	}
	if in.Via != "app" && in.Via != "telegram" {
		return 0, nil, badRequest("via must be app or telegram")
	}
	if _, err := s.Fleet.Store.Agent(r.Context(), ids[0], ids[1]); err != nil {
		return 0, nil, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	reply, err := s.Agents.Command(ctx, ids[0], ids[1], name, in.Via)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, api.CommandResult{OK: reply.OK, Message: reply.Text}, nil
}

func eventOut(e audit.Event) api.Event {
	out := api.Event{Seq: e.Seq, ID: e.ID.String(), Type: e.Type, At: e.At, Payload: e.Body, Hash: hex.EncodeToString(e.Hash)}
	if len(e.Prev) > 0 {
		out.PrevHash = hex.EncodeToString(e.Prev)
	}
	return out
}

// after reads where an event read starts: Last-Event-ID, then ?after.
func after(r *http.Request) (int64, error) {
	v := r.Header.Get("Last-Event-ID")
	if v == "" {
		v = r.URL.Query().Get("after")
	}
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, badRequest("after must be a sequence number")
	}
	return n, nil
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) (int, any, error) {
	ids, err := pathIDs(r, "tenant", "agent")
	if err != nil {
		return 0, nil, err
	}
	from, err := after(r)
	if err != nil {
		return 0, nil, err
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > 1000 {
			return 0, nil, badRequest("limit must be between 1 and 1000")
		}
	}
	types, err := eventTypes(r.URL.Query().Get("type"))
	if err != nil {
		return 0, nil, err
	}
	if _, err := s.Fleet.Store.Agent(r.Context(), ids[0], ids[1]); err != nil {
		return 0, nil, err
	}
	events, err := s.Fleet.Store.Events(r.Context(), ids[0], ids[1], from, limit, types)
	out := api.EventList{Events: []api.Event{}}
	for _, e := range events {
		out.Events = append(out.Events, eventOut(e))
	}
	return http.StatusOK, out, err
}

var eventTypePattern = regexp.MustCompile(`^[a-z]+(\.[a-z_]+)?$`)

// eventTypes reads the type filter: event types, comma-separated.
func eventTypes(v string) ([]string, error) {
	if v == "" {
		return nil, nil
	}
	var out []string
	for _, t := range strings.Split(v, ",") {
		t = strings.TrimSpace(t)
		if !eventTypePattern.MatchString(t) {
			return nil, badRequest("type must list event types, comma-separated")
		}
		out = append(out, t)
	}
	if len(out) > 20 {
		return nil, badRequest("type lists at most 20 event types")
	}
	return out, nil
}

// stream sends the agent's events as server-sent events: what is stored
// after the starting point, then each new event as the shipper stores it.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	ids, err := pathIDs(r, "tenant", "agent")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	seq, err := after(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if _, err := s.Fleet.Store.Agent(r.Context(), ids[0], ids[1]); err != nil {
		s.fail(w, r, err)
		return
	}
	wake, done := s.hub.subscribe(ids[1])
	defer done()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	rc.Flush()
	const page = 500
	for {
		events, err := s.Fleet.Store.Events(r.Context(), ids[0], ids[1], seq, page, nil)
		if err != nil {
			if r.Context().Err() == nil {
				s.Logf("event stream: %v", err)
			}
			return
		}
		for _, e := range events {
			raw, _ := json.Marshal(eventOut(e))
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Type, raw)
			seq = e.Seq
		}
		if len(events) > 0 {
			if rc.Flush() != nil {
				return
			}
		}
		if len(events) == page {
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

// hub wakes an agent's streams when its events are stored.
type hub struct {
	mu   sync.Mutex
	subs map[uuid.UUID]map[chan struct{}]bool
}

func (h *hub) subscribe(agent uuid.UUID) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs == nil {
		h.subs = map[uuid.UUID]map[chan struct{}]bool{}
	}
	if h.subs[agent] == nil {
		h.subs[agent] = map[chan struct{}]bool{}
	}
	h.subs[agent][ch] = true
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.subs[agent], ch)
		if len(h.subs[agent]) == 0 {
			delete(h.subs, agent)
		}
	}
}

func (h *hub) wake(agent uuid.UUID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[agent] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
