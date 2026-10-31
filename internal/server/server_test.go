package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/candylario/trader/api"
	"github.com/candylario/trader/internal/fleet"
	"github.com/candylario/trader/internal/pg"
	"github.com/candylario/trader/internal/radar"
	"github.com/candylario/trader/internal/testkit"
	"github.com/candylario/trader/internal/vault"
)

const (
	token          = "test-token-0123456789abcdef0123456789"
	notifierBearer = "notifier-token-0123456789abcdef01234"
)

// newClient runs the engine API on a fresh database and returns a client
// with the full token.
func newClient(t *testing.T) *api.Client {
	url := testkit.Database(t)
	ctx, cancel := context.WithCancel(context.Background())
	if err := pg.Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	store, err := pg.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	master, _ := vault.NewMasterKey()
	v, _ := vault.New(master)
	srv := &Server{Fleet: &fleet.Service{Store: store, Vault: v}, Radar: &radar.Log{}, Token: token, NotifierToken: notifierBearer, Commit: "test", Logf: t.Logf}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); srv.Serve(ctx, l) }()
	t.Cleanup(func() { cancel(); wg.Wait(); store.Close() })
	return &api.Client{BaseURL: "http://" + l.Addr().String(), Token: token}
}

func code(err error) string {
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

func TestTenantsThroughTheAPI(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	tid := uuid.Must(uuid.NewV7()).String()
	tn, err := c.CreateTenant(ctx, api.NewTenant{ID: tid, Name: "alice@example.com"})
	if err != nil || tn.ID != tid || tn.Name != "alice@example.com" || tn.Status != "active" {
		t.Fatalf("create: %+v %v", tn, err)
	}
	if again, err := c.CreateTenant(ctx, api.NewTenant{ID: tid, Name: "mallory"}); err != nil || again.Name != "alice@example.com" {
		t.Fatalf("a retry returns the first tenant: %+v %v", again, err)
	}
	if got, err := c.Tenant(ctx, tid); err != nil || got != tn {
		t.Fatalf("get: %+v %v", got, err)
	}
	closed, err := c.CloseTenant(ctx, tid)
	if err != nil || closed.Status != "closed" || closed.Name != "" {
		t.Fatalf("close: %+v %v", closed, err)
	}
	if again, err := c.CloseTenant(ctx, tid); err != nil || again != closed {
		t.Fatalf("closing twice: %+v %v", again, err)
	}
	if _, err := c.CloseTenant(ctx, uuid.Must(uuid.NewV7()).String()); code(err) != "not_found" {
		t.Fatalf("an unknown tenant: %v", err)
	}
	resp, err := http.Get(c.BaseURL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var h api.Health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil || resp.StatusCode != http.StatusOK || !h.OK || h.Commit != "test" {
		t.Fatalf("health: %d %+v %v", resp.StatusCode, h, err)
	}
}

func TestBadRequestsGetClearErrors(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	if _, err := c.Tenant(ctx, "not-a-uuid"); code(err) != "bad_request" {
		t.Fatalf("bad ID: %v", err)
	}
	if _, err := c.Tenant(ctx, uuid.NewString()); code(err) != "not_found" {
		t.Fatalf("unknown tenant: %v", err)
	}
	if _, err := c.CreateTenant(ctx, api.NewTenant{Name: " "}); code(err) != "refused" {
		t.Fatalf("a tenant without a name: %v", err)
	}
	resp, err := http.Post(c.BaseURL+"/v1/tenants", "application/json", strings.NewReader(`{"name":"x","admin":true}`))
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %v %v", resp.Status, err)
	}
	req, _ := http.NewRequest(http.MethodPost, c.BaseURL+"/v1/tenants", strings.NewReader(`{"name":"x","admin":true}`))
	req.Header.Set("Authorization", "Bearer "+token)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown fields are refused: %v %v", resp.Status, err)
	}
	req, _ = http.NewRequest(http.MethodGet, c.BaseURL+"/v1/tenants/"+uuid.NewString()+"/agents", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("agents are gone: %v %v", resp.Status, err)
	}
}

// TestTheSpecMatchesTheCode keeps openapi.yaml honest: every route is in it
// and every path in it is served, and each schema lists exactly the fields
// of its Go type.
func TestTheSpecMatchesTheCode(t *testing.T) {
	lines := strings.Split(string(api.Spec), "\n")
	inSpec := map[string]bool{}
	method := regexp.MustCompile(`^    (get|post|put|delete|patch):`)
	path := ""
	for _, line := range lines {
		if line == "components:" {
			break
		}
		if strings.HasPrefix(line, "  /") {
			path = strings.TrimSuffix(strings.TrimSpace(line), ":")
		} else if m := method.FindStringSubmatch(line); m != nil {
			inSpec[strings.ToUpper(m[1])+" "+path] = true
		}
	}
	served := map[string]bool{}
	for _, rt := range (&Server{}).routes() {
		served[rt.pattern] = true
		if !inSpec[rt.pattern] {
			t.Errorf("%s is served but not in the spec", rt.pattern)
		}
	}
	for p := range inSpec {
		if !served[p] {
			t.Errorf("%s is in the spec but not served", p)
		}
	}

	types := map[string]any{
		"NewTenant": api.NewTenant{}, "Tenant": api.Tenant{}, "Error": api.Error{}, "Health": api.Health{},
		"RadarAlert": api.RadarAlert{}, "RadarAlertList": api.RadarAlertList{}, "Wallet": api.Wallet{}, "WalletAccount": api.WalletAccount{},
		"PerpPosition": api.PerpPosition{}, "WalletApprovals": api.WalletApprovals{}, "Signature": api.Signature{}, "SignedAction": api.SignedAction{}, "WalletApproval": api.WalletApproval{},
		"NewTrade": api.NewTrade{}, "Trade": api.Trade{}, "TradeSuggestion": api.TradeSuggestion{}, "Momentum": api.Momentum{}, "CloseTrade": api.CloseTrade{}, "ClosedTrade": api.ClosedTrade{},
	}
	for name, v := range types {
		var want []string
		rt := reflect.TypeOf(v)
		for i := range rt.NumField() {
			if tag := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]; tag != "-" {
				want = append(want, tag)
			}
		}
		got := schemaFields(lines, name)
		slices.Sort(want)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("schema %s has %v, the type has %v", name, got, want)
		}
	}
}

// schemaFields lists the properties of a schema in the spec.
func schemaFields(lines []string, name string) []string {
	var out []string
	in, props := false, false
	lines = lines[slices.Index(lines, "  schemas:"):]
	for _, line := range lines {
		switch {
		case line == "    "+name+":":
			in = true
		case in && strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "     "):
			return out
		case in && line == "      properties:":
			props = true
		case in && props && strings.HasPrefix(line, "        ") && !strings.HasPrefix(line, "         "):
			out = append(out, strings.SplitN(strings.TrimSpace(line), ":", 2)[0])
		case in && strings.HasPrefix(line, "      ") && !strings.HasPrefix(line, "       "):
			props = false
		}
	}
	return out
}

func TestTheNotifiersTokenOpensOnlyItsRoutes(t *testing.T) {
	c, ctx := newClient(t), context.Background()
	tn, err := c.CreateTenant(ctx, api.NewTenant{Name: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body, bearer string) (int, string) {
		req, _ := http.NewRequestWithContext(ctx, method, c.BaseURL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			Error api.Error `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out.Error.Code
	}
	if status, _ := call(http.MethodGet, "/v1/radar/alerts", "", notifierBearer); status != http.StatusOK {
		t.Fatalf("the notifier reads the radar: %d", status)
	}
	trade := `{"coin":"ETH","side":"long","usd":25,"leverage":1,"stop_loss_pct":0.1}`
	if status, code := call(http.MethodPost, "/v1/tenants/"+tn.ID+"/trades", trade, notifierBearer); status != http.StatusUnprocessableEntity || code != "refused" {
		t.Fatalf("the notifier places trades (refused here: no wallet): %d %s", status, code)
	}
	if status, code := call(http.MethodPost, "/v1/tenants/"+tn.ID+"/trades/close", `{"coin":"ETH"}`, notifierBearer); status != http.StatusUnprocessableEntity || code != "refused" {
		t.Fatalf("the notifier closes trades (refused here: no wallet): %d %s", status, code)
	}
	for _, rt := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/tenants/" + tn.ID, ""},
		{http.MethodDelete, "/v1/tenants/" + tn.ID, ""},
		{http.MethodPost, "/v1/tenants", `{"name":"mallory"}`},
		{http.MethodPost, "/v1/tenants/" + tn.ID + "/wallet", ""},
		{http.MethodGet, "/v1/tenants/" + tn.ID + "/wallet", ""},
		{http.MethodGet, "/v1/tenants/" + tn.ID + "/wallet/approvals?user=0x0000000000000000000000000000000000000001&mode=testnet", ""},
	} {
		if status, code := call(rt.method, rt.path, rt.body, notifierBearer); status != http.StatusForbidden || code != "forbidden" {
			t.Errorf("the notifier's token must not reach %s %s: %d %s", rt.method, rt.path, status, code)
		}
	}
	if status, code := call(http.MethodGet, "/v1/radar/alerts", "", "wrong"); status != http.StatusUnauthorized || code != "unauthorized" {
		t.Fatalf("a wrong token is unauthorized: %d %s", status, code)
	}
}
