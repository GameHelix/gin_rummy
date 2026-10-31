// Package server answers the engine API that the app and the notifier
// use: tenants, each tenant's wallet and trades, and the radar's alerts.
// The contract is in package api.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/candylario/trader/api"
	"github.com/candylario/trader/internal/fleet"
	"github.com/candylario/trader/internal/pg"
	"github.com/candylario/trader/internal/radar"
)

type Server struct {
	Fleet *fleet.Service
	Token string
	// NotifierToken, when set, opens only what the notifier needs: the
	// radar and a tenant's trades. Every other route refuses it.
	NotifierToken string
	// Commit is the source commit the engine was built from, for the
	// health check.
	Commit string
	Logf   func(format string, args ...any)
	// Radar, when set, is the radar's alert log, served to the notifier
	// and the app.
	Radar *radar.Log
}

// Serve answers on the listener until ctx ends, then closes every stream.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
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

type route struct {
	pattern  string
	open     bool // answered without a token
	notifier bool // answered to the notifier's token as well
	json     func(w http.ResponseWriter, r *http.Request) (int, any, error)
	raw      http.HandlerFunc
}

func (s *Server) routes() []route {
	return []route{
		{pattern: "GET /v1/health", open: true, json: s.health},
		{pattern: "GET /v1/openapi.yaml", open: true, raw: spec},
		{pattern: "POST /v1/tenants", json: s.createTenant},
		{pattern: "GET /v1/tenants/{tenant}", json: s.getTenant},
		{pattern: "DELETE /v1/tenants/{tenant}", json: s.closeTenant},
		{pattern: "POST /v1/tenants/{tenant}/wallet", json: s.createWallet},
		{pattern: "GET /v1/tenants/{tenant}/wallet", json: s.getWallet},
		{pattern: "GET /v1/tenants/{tenant}/wallet/approvals", json: s.walletApprovals},
		{pattern: "POST /v1/tenants/{tenant}/wallet/approvals", json: s.approveWallet},
		{pattern: "POST /v1/tenants/{tenant}/trades", notifier: true, json: s.openTrade},
		{pattern: "POST /v1/tenants/{tenant}/trades/close", notifier: true, json: s.closeTrade},
		{pattern: "GET /v1/radar/alerts", notifier: true, json: s.radarAlerts},
		{pattern: "GET /v1/radar/stream", notifier: true, raw: s.radarStream},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		mux.HandleFunc(rt.pattern, func(w http.ResponseWriter, r *http.Request) {
			switch s.bearer(r) {
			case fullToken:
			case notifierToken:
				if !rt.open && !rt.notifier {
					s.fail(w, r, &apiError{http.StatusForbidden, "forbidden", "the notifier's token cannot use this route"})
					return
				}
			default:
				if !rt.open {
					s.fail(w, r, &apiError{http.StatusUnauthorized, "unauthorized", "a valid bearer token is required"})
					return
				}
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

// Which token a request carries.
type tokenKind int

const (
	noToken tokenKind = iota
	notifierToken
	fullToken
)

func (s *Server) bearer(r *http.Request) tokenKind {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	switch {
	case !ok:
		return noToken
	case same(got, s.Token):
		return fullToken
	case same(got, s.NotifierToken):
		return notifierToken
	}
	return noToken
}

// same compares a presented token with a configured one in constant time;
// an empty configured token matches nothing.
func same(got, want string) bool {
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	return want != "" && subtle.ConstantTimeCompare(a[:], b[:]) == 1
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
		ae = &apiError{http.StatusNotFound, "not_found", "no such tenant or wallet"}
	case errors.Is(err, pg.ErrExists):
		ae = &apiError{http.StatusConflict, "conflict", "that ID is already taken"}
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

// health answers 200 when the database answers, and 503 otherwise.
func (s *Server) health(w http.ResponseWriter, r *http.Request) (int, any, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.Fleet.Store.Ping(ctx); err != nil {
		return 0, nil, &apiError{http.StatusServiceUnavailable, "internal", "the database does not answer"}
	}
	return http.StatusOK, api.Health{OK: true, Commit: s.Commit}, nil
}

func spec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.Write(api.Spec)
}

func tenantOut(t pg.Tenant) api.Tenant {
	return api.Tenant{ID: t.ID.String(), Name: t.Name, Status: t.Status}
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
	t, isNew, err := s.Fleet.AddTenant(r.Context(), id, in.Name)
	if err != nil {
		return 0, nil, err
	}
	return created(isNew), tenantOut(t), nil
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
	return http.StatusOK, tenantOut(t), nil
}

func (s *Server) closeTenant(w http.ResponseWriter, r *http.Request) (int, any, error) {
	ids, err := pathIDs(r, "tenant")
	if err != nil {
		return 0, nil, err
	}
	if err := s.Fleet.CloseTenant(r.Context(), ids[0]); err != nil {
		return 0, nil, err
	}
	t, err := s.Fleet.Store.Tenant(r.Context(), ids[0])
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, tenantOut(t), nil
}
