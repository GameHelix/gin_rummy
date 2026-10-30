package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/google/uuid"

	"github.com/candylario/trader/api"
	"github.com/candylario/trader/internal/agent"
	"github.com/candylario/trader/internal/analyst"
	"github.com/candylario/trader/internal/binance"
	"github.com/candylario/trader/internal/fleet"
	"github.com/candylario/trader/internal/pg"
	"github.com/candylario/trader/internal/testkit"
	"github.com/candylario/trader/internal/vault"
)

const (
	token          = "test-token-0123456789abcdef0123456789"
	notifierBearer = "notifier-token-0123456789abcdef01234"
)

type rig struct {
	t      *testing.T
	ex     *testkit.Binance
	client *api.Client
	sup    *fleet.Supervisor
	mu     sync.Mutex
	clock  time.Time
}

// newRig runs an engine with its API against fake Binance and model
// servers and a fresh database.
func newRig(t *testing.T) *rig {
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
	r := &rig{t: t, clock: testkit.At(t, "2026-10-25 12:00")}
	now := r.now
	r.ex = testkit.NewBinance(now)
	model := &testkit.API{WantKey: "sk-alice", Script: []func(string) (int, any){testkit.SubmitStep(map[string]string{"BTCUSDT": "strong_buy", "ETHUSDT": "hold"}, testkit.CitePrices)}}
	exURL, modelURL := r.ex.Server(t).URL, model.Server(t).URL
	svc := &fleet.Service{Store: store, Vault: v, DataDir: t.TempDir(), Model: "test-model",
		CheckKey: func(ctx context.Context, kind, mode string, k fleet.Keys) error {
			if kind == fleet.KindAnthropic && k.Anthropic != "sk-alice" {
				return errors.New("the Anthropic key is not valid")
			}
			return nil
		}}
	build := func(spec fleet.Spec) *agent.Engine {
		c := spec.Config
		e := &agent.Engine{Config: c, Now: now,
			Exchange: &binance.Client{HTTP: http.DefaultClient, BaseURL: exURL, DataURL: exURL, Key: spec.Keys.BinanceKey, Secret: spec.Keys.BinanceSecret, Now: now},
			Analyst: &analyst.Analyst{Config: c, Client: anthropic.NewClient(option.WithAPIKey(spec.Keys.Anthropic), option.WithBaseURL(modelURL)),
				Sleep:  func(context.Context, time.Duration) error { return nil },
				Budget: &analyst.Budget{Path: filepath.Join(c.DataDir, "budget.json"), DailyUSD: c.MaxDailyLLMUSD, MonthlyUSD: c.MaxMonthlyLLMUSD, Now: now}}}
		return e
	}
	sup := &fleet.Supervisor{Service: svc, Every: 50 * time.Millisecond, Logf: t.Logf, Now: now,
		Build: func(spec fleet.Spec) (*agent.Engine, error) { return build(spec), nil },
		Ship: func(ctx context.Context, spec fleet.Spec) {
			who := pg.Agent{Tenant: spec.Tenant, ID: spec.Agent, Name: spec.Name, Mode: spec.Config.Mode}
			pg.Ship(ctx, store, agent.EventsDir(spec.Config), func() (pg.Agent, bool) { return who, true }, 50*time.Millisecond, t.Logf)
		}}
	r.sup = sup
	srv := &Server{Fleet: svc, Agents: sup, Token: token, NotifierToken: notifierBearer, Commit: "test", Logf: t.Logf,
		Report: func(ctx context.Context, spec fleet.Spec) (agent.Report, error) { return build(spec).Report(ctx) }}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); srv.Serve(ctx, l) }()
	go func() { defer wg.Done(); sup.Run(ctx) }()
	t.Cleanup(func() { cancel(); wg.Wait(); store.Close() })
	r.client = &api.Client{BaseURL: "http://" + l.Addr().String(), Token: token}
	return r
}

func (r *rig) waitFor(what string, ok func() bool) {
	r.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func code(err error) string {
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

func agentConfig(t *testing.T, edit func(map[string]any)) json.RawMessage {
	raw, _ := json.Marshal(testkit.Config("ignored"))
	var m map[string]any
	json.Unmarshal(raw, &m)
	delete(m, "data_dir")
	delete(m, "model")
	if edit != nil {
		edit(m)
	}
	out, _ := json.Marshal(m)
	return out
}

var plan = api.Plan{Symbols: []string{"BTCUSDT", "ETHUSDT"}, MaxAgents: 2, MinDecisionIntervalHours: 1, MaxEventDecisionsPerDay: 8}

// TestTheAppRunsAnAgentThroughTheAPI does through the API all that the
// chat can, and what only the app can: tenants, plans, keys, agents,
// configs and the audit log.
func TestTheAppRunsAnAgentThroughTheAPI(t *testing.T) {
	r := newRig(t)
	c := r.client
	ctx := context.Background()

	if _, err := (&api.Client{BaseURL: c.BaseURL, Token: "wrong"}).Tenant(ctx, uuid.NewString()); code(err) != "unauthorized" {
		t.Fatalf("a wrong token is refused: %v", err)
	}
	tid := uuid.Must(uuid.NewV7()).String()
	tn, err := c.CreateTenant(ctx, api.NewTenant{ID: tid, Name: "alice", Plan: plan})
	if err != nil || tn.ID != tid || tn.Status != "active" || tn.Plan.MaxAgents != 2 {
		t.Fatalf("tenant: %+v %v", tn, err)
	}
	if again, err := c.CreateTenant(ctx, api.NewTenant{ID: tid, Name: "alice", Plan: plan}); err != nil || again.ID != tid {
		t.Fatalf("a retried create returns the same tenant: %+v %v", again, err)
	}

	if _, err := c.AddKey(ctx, tid, api.NewKey{Kind: api.KindAnthropic, Secret: json.RawMessage(`{"key":"sk-wrong"}`)}); code(err) != "refused" || !strings.Contains(err.Error(), "not valid") {
		t.Fatalf("a key that does not work is refused: %v", err)
	}
	var keys []string
	for _, k := range []api.NewKey{
		{Kind: api.KindAnthropic, Label: "model", Secret: json.RawMessage(`{"key":"sk-alice"}`)},
		{Kind: api.KindBinance, Label: "testnet", Mode: "testnet", Secret: json.RawMessage(`{"key":"` + testkit.Key + `","secret":"` + testkit.Secret + `"}`)},
	} {
		got, err := c.AddKey(ctx, tid, k)
		if err != nil || got.Kind != k.Kind || got.Hint == "" {
			t.Fatalf("key %s: %+v %v", k.Kind, got, err)
		}
		keys = append(keys, got.ID)
	}
	listed, err := c.Keys(ctx, tid)
	if raw, _ := json.Marshal(listed); err != nil || len(listed) != 2 || strings.Contains(string(raw), testkit.Secret) || strings.Contains(string(raw), "sk-alice") {
		t.Fatalf("keys are listed without their secrets: %s %v", raw, err)
	}

	aid := uuid.Must(uuid.NewV7()).String()
	ag, err := c.CreateAgent(ctx, tid, api.NewAgent{ID: aid, Name: "btc-eth", Mode: "testnet", Config: agentConfig(t, nil), Keys: keys})
	if err != nil || ag.Status != "stopped" || ag.Version != 1 || len(ag.Keys) != 2 || ag.Running {
		t.Fatalf("agent: %+v %v", ag, err)
	}
	if list, err := c.Agents(ctx, tid); err != nil || len(list) != 1 || list[0].ID != aid {
		t.Fatalf("agents: %+v %v", list, err)
	}
	if _, err := c.Status(ctx, tid, aid); code(err) != "no_account" {
		t.Fatalf("a new agent has no account yet: %v", err)
	}
	if _, err := c.Command(ctx, tid, aid, api.Command{Command: api.Decide}); code(err) != "not_running" {
		t.Fatalf("a stopped agent takes no commands: %v", err)
	}

	var mu sync.Mutex
	var seen []api.Event
	events := func() []api.Event {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
	types := func() []string {
		var out []string
		for _, e := range events() {
			out = append(out, e.Type)
		}
		return out
	}
	said := func(substr string) bool {
		for _, e := range events() {
			if e.Type == "notice" && strings.Contains(string(e.Payload), substr) {
				return true
			}
		}
		return false
	}
	sctx, stopStream := context.WithCancel(ctx)
	defer stopStream()
	go c.Stream(sctx, tid, aid, 0, func(e api.Event) error {
		mu.Lock()
		defer mu.Unlock()
		if len(seen) > 0 && e.Seq != seen[len(seen)-1].Seq+1 {
			t.Errorf("event %d after %d", e.Seq, seen[len(seen)-1].Seq)
		}
		seen = append(seen, e)
		return nil
	})

	if ag, err := c.Start(ctx, tid, aid); err != nil || ag.Status != "active" {
		t.Fatalf("start: %+v %v", ag, err)
	}
	r.waitFor("the first trade", func() bool { return len(r.ex.OrderList()) == 1 })
	r.waitFor("the trade on the stream", func() bool { return slices.Contains(types(), "trade.booked") })
	st, err := c.Status(ctx, tid, aid)
	if err != nil || !st.Running || st.Trading != api.TradingOn || st.Trades != 1 || st.LastDecision == nil || st.LastDecision.Ratings["BTCUSDT"].Rating != "strong_buy" || st.NextDecision == nil || !strings.Contains(st.Message, "📊 Status") {
		t.Fatalf("status: %+v %v", st, err)
	}

	for _, step := range []struct {
		command string
		ok      bool
		said    string
	}{
		{api.StopTrading, true, "Trading stopped"},
		{api.Decide, false, "Send /resume first"},
		{api.ResumeTrading, true, "Trading resumed"},
		{api.SellAll, true, "Selling everything"},
	} {
		got, err := c.Command(ctx, tid, aid, api.Command{Command: step.command, Via: "telegram"})
		if err != nil || got.OK != step.ok || !strings.Contains(got.Message, step.said) {
			t.Fatalf("%s: %+v %v", step.command, got, err)
		}
	}
	r.waitFor("the sell-off", func() bool { return len(r.ex.OrderList()) == 2 })
	r.waitFor("the announcements", func() bool { return said("Trading stopped") && said("Selling everything") })
	if said("Send /resume first") {
		t.Fatal("a refused command is not announced")
	}
	if _, err := c.Command(ctx, tid, aid, api.Command{Command: "leverage_up"}); code(err) != "bad_request" {
		t.Fatalf("unknown commands are refused: %v", err)
	}

	cfg, err := c.SetConfig(ctx, tid, aid, agentConfig(t, func(m map[string]any) { m["decision_interval_hours"] = 6 }))
	if err != nil || cfg.Version != 2 {
		t.Fatalf("config: %+v %v", cfg, err)
	}
	if _, err := c.SetConfig(ctx, tid, aid, agentConfig(t, func(m map[string]any) { m["symbols"] = []string{"SOLUSDT"} })); code(err) != "refused" {
		t.Fatalf("a config outside the plan is refused: %v", err)
	}
	r.waitFor("a restart on version 2", func() bool { return strings.Count(strings.Join(types(), " "), "agent.started") == 2 })
	if old, err := c.Config(ctx, tid, aid, 1); err != nil || old.Version != 1 || strings.Contains(string(old.Config), `"decision_interval_hours": 6`) {
		t.Fatalf("version 1 is kept as it was: %+v %v", old, err)
	}

	if ag, err := c.Stop(ctx, tid, aid); err != nil || ag.Status != "stopped" {
		t.Fatalf("stop: %+v %v", ag, err)
	}
	r.waitFor("the agent to stop", func() bool { a, _ := c.Agent(ctx, tid, aid); return !a.Running })
	if _, err := c.Start(ctx, tid, aid); err != nil {
		t.Fatal(err)
	}
	r.waitFor("the agent to run again", func() bool { a, _ := c.Agent(ctx, tid, aid); return a.Running })

	page, err := c.Events(ctx, tid, aid, 2, 3)
	if err != nil || len(page) != 3 || page[0].Seq != 3 || page[0].PrevHash == "" || page[0].Hash == "" {
		t.Fatalf("events after 2: %+v %v", page, err)
	}
	req, _ := http.NewRequest(http.MethodGet, c.BaseURL+"/v1/tenants/"+tid+"/agents/"+aid+"/events?before=6&limit=2", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	var last api.EventList
	if resp, err := http.DefaultClient.Do(req); err != nil || json.NewDecoder(resp.Body).Decode(&last) != nil || len(last.Events) != 2 || last.Events[0].Seq != 4 || last.Events[1].Seq != 5 {
		t.Fatalf("the two events before 6: %+v %v", last, err)
	}
	var commands []string
	for _, e := range events() {
		var p struct{ Name, Source, Via string }
		if e.Type == "command.received" && json.Unmarshal(e.Payload, &p) == nil && p.Source == "api" && p.Via == "telegram" {
			commands = append(commands, p.Name)
		}
	}
	if strings.Join(commands, " ") != "stop decide resume sellall" {
		t.Fatalf("every command is on record: %v", commands)
	}

	bob, _ := c.CreateTenant(ctx, api.NewTenant{Name: "bob", Plan: plan})
	if _, err := c.Agent(ctx, bob.ID, aid); code(err) != "not_found" {
		t.Fatalf("another tenant cannot see the agent: %v", err)
	}
	if _, err := c.Command(ctx, bob.ID, aid, api.Command{Command: api.SellAll}); code(err) != "not_found" {
		t.Fatalf("another tenant cannot command the agent: %v", err)
	}
	if _, err := c.Events(ctx, bob.ID, aid, 0, 10); code(err) != "not_found" {
		t.Fatalf("another tenant cannot read the agent's events: %v", err)
	}
	if _, err := c.RevokeKey(ctx, bob.ID, keys[0]); code(err) != "not_found" {
		t.Fatalf("another tenant cannot revoke the key: %v", err)
	}

	change, err := c.SetPlan(ctx, tid, api.Plan{Symbols: []string{"BTCUSDT"}, MaxAgents: 2, MinDecisionIntervalHours: 1, MaxEventDecisionsPerDay: 8})
	if err != nil || len(change.Stopped) != 1 || change.Stopped[0].AgentID != aid || !strings.Contains(change.Stopped[0].Reason, "ETHUSDT") {
		t.Fatalf("a downgrade stops the agent: %+v %v", change, err)
	}
	r.waitFor("the agent to stop", func() bool { a, _ := c.Agent(ctx, tid, aid); return !a.Running })
	st, err = c.Status(ctx, tid, aid)
	if err != nil || st.Running || st.Trading != api.TradingStopped || st.Trades != 2 {
		t.Fatalf("a stopped agent's account is still there: %+v %v", st, err)
	}
	if _, err := c.Start(ctx, tid, aid); code(err) != "refused" {
		t.Fatalf("the plan no longer allows the agent: %v", err)
	}
	revoked, err := c.RevokeKey(ctx, tid, keys[1])
	if err != nil || len(revoked.Stopped) != 1 {
		t.Fatalf("revoke: %+v %v", revoked, err)
	}
	if a, _ := c.Agent(ctx, tid, aid); a.Keys["binance"] != "" {
		t.Fatalf("a revoked key is detached: %+v", a)
	}
	fresh, err := c.AddKey(ctx, tid, api.NewKey{Kind: api.KindBinance, Mode: "testnet", Secret: json.RawMessage(`{"key":"k2","secret":"s2"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if a, err := c.AttachKey(ctx, tid, aid, fresh.ID); err != nil || a.Keys["binance"] != fresh.ID {
		t.Fatalf("attach: %+v %v", a, err)
	}
	if _, err := c.AttachKey(ctx, tid, aid, keys[1]); code(err) != "refused" {
		t.Fatalf("a revoked key cannot be attached: %v", err)
	}
}

func TestBadRequestsGetClearErrors(t *testing.T) {
	r := newRig(t)
	c := r.client
	ctx := context.Background()
	if _, err := c.Tenant(ctx, "not-a-uuid"); code(err) != "bad_request" {
		t.Fatalf("bad ID: %v", err)
	}
	if _, err := c.Tenant(ctx, uuid.NewString()); code(err) != "not_found" {
		t.Fatalf("unknown tenant: %v", err)
	}
	if _, err := c.CreateTenant(ctx, api.NewTenant{Name: "x", Plan: api.Plan{MaxAgents: -1}}); code(err) != "refused" {
		t.Fatalf("a bad plan: %v", err)
	}
	resp, err := http.Post(c.BaseURL+"/v1/tenants", "application/json", strings.NewReader(`{"name":"x","plan":{},"admin":true}`))
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %v %v", resp.Status, err)
	}
	req, _ := http.NewRequest(http.MethodPost, c.BaseURL+"/v1/tenants", strings.NewReader(`{"name":"x","plan":{},"admin":true}`))
	req.Header.Set("Authorization", "Bearer "+token)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown fields are refused: %v %v", resp.Status, err)
	}
	req, _ = http.NewRequest(http.MethodGet, c.BaseURL+"/v1/tenants/"+uuid.NewString()+"/agents/"+uuid.NewString()+"/events?before=0", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("before must be a sequence number: %v %v", resp.Status, err)
	}
	if resp, err := http.Get(c.BaseURL + "/v1/health"); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("health: %v", err)
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
		"Plan": api.Plan{}, "NewTenant": api.NewTenant{}, "Tenant": api.Tenant{}, "Stopped": api.Stopped{}, "PlanChange": api.PlanChange{},
		"NewKey": api.NewKey{}, "Key": api.Key{}, "KeyList": api.KeyList{}, "Revoked": api.Revoked{}, "NewAgent": api.NewAgent{},
		"Agent": api.Agent{}, "AgentList": api.AgentList{}, "Config": api.Config{}, "Command": api.Command{}, "CommandResult": api.CommandResult{},
		"Position": api.Position{}, "Rating": api.Rating{}, "Decision": api.Decision{}, "AISpend": api.AISpend{}, "Status": api.Status{},
		"Event": api.Event{}, "EventList": api.EventList{}, "Error": api.Error{}, "Health": api.Health{}, "AgentDown": api.AgentDown{},
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

func TestClosingATenantThroughTheAPI(t *testing.T) {
	r := newRig(t)
	c, ctx := r.client, context.Background()
	tid := uuid.Must(uuid.NewV7()).String()
	if _, err := c.CreateTenant(ctx, api.NewTenant{ID: tid, Name: "alice@example.com", Plan: plan}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddKey(ctx, tid, api.NewKey{Kind: api.KindAnthropic, Label: "model", Secret: json.RawMessage(`{"key":"sk-alice"}`)}); err != nil {
		t.Fatal(err)
	}
	if out, err := c.CloseTenant(ctx, tid); err != nil || out.Stopped == nil {
		t.Fatalf("close: %+v %v", out, err)
	}
	if tn, err := c.Tenant(ctx, tid); err != nil || tn.Status != "closed" || tn.Name != "" {
		t.Fatalf("closed tenant: %+v %v", tn, err)
	}
	if keys, err := c.Keys(ctx, tid); err != nil || len(keys) != 1 || !keys[0].Revoked || keys[0].Hint != "" {
		t.Fatalf("keys of a closed tenant: %+v %v", keys, err)
	}
	if _, err := c.AddKey(ctx, tid, api.NewKey{Kind: api.KindAnthropic, Secret: json.RawMessage(`{"key":"sk-alice"}`)}); code(err) != "refused" {
		t.Fatalf("a closed tenant takes no keys: %v", err)
	}
	if _, err := c.CloseTenant(ctx, uuid.Must(uuid.NewV7()).String()); code(err) != "not_found" {
		t.Fatalf("an unknown tenant: %v", err)
	}
}

// now is the rig's clock; advance moves it.
func (r *rig) now() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.clock }

func (r *rig) advance(d time.Duration) { r.mu.Lock(); r.clock = r.clock.Add(d); r.mu.Unlock() }

// health reads the engine's health check without a token.
func (r *rig) health() (int, api.Health) {
	r.t.Helper()
	resp, err := http.Get(r.client.BaseURL + "/v1/health")
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	var h api.Health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		r.t.Fatal(err)
	}
	return resp.StatusCode, h
}

func TestHealthReportsAnAgentThatCannotRun(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	tn, err := r.client.CreateTenant(ctx, api.NewTenant{Name: "alice", Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	ak, err := r.client.AddKey(ctx, tn.ID, api.NewKey{Kind: api.KindAnthropic, Secret: json.RawMessage(`{"key":"sk-alice"}`)})
	if err != nil {
		t.Fatal(err)
	}
	// A Binance key the venue rejects: the agent can never open its account.
	bk, err := r.client.AddKey(ctx, tn.ID, api.NewKey{Kind: api.KindBinance, Mode: "testnet", Secret: json.RawMessage(`{"key":"wrong","secret":"wrong"}`)})
	if err != nil {
		t.Fatal(err)
	}
	ag, err := r.client.CreateAgent(ctx, tn.ID, api.NewAgent{Name: "broken", Mode: "testnet", Config: agentConfig(t, nil), Keys: []string{ak.ID, bk.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.client.Start(ctx, tn.ID, ag.ID); err != nil {
		t.Fatal(err)
	}
	if status, h := r.health(); status != http.StatusOK || !h.OK || h.Commit != "test" || len(h.Down) != 0 {
		t.Fatalf("healthy at first: %d %+v", status, h)
	}
	r.waitFor("the agent to fail", func() bool { return len(r.sup.Down(0)) == 1 })
	if status, h := r.health(); status != http.StatusOK || !h.OK {
		t.Fatalf("a fresh failure is not reported yet: %d %+v", status, h)
	}
	r.advance(downFor)
	status, h := r.health()
	if status != http.StatusServiceUnavailable || h.OK || len(h.Down) != 1 || h.Down[0].AgentID != ag.ID || h.Down[0].TenantID != tn.ID || !strings.Contains(h.Down[0].Error, "Binance") || !h.Down[0].Since.Equal(r.now().Add(-downFor)) {
		t.Fatalf("after ten minutes the agent is reported: %d %+v", status, h)
	}
	if _, err := r.client.Stop(ctx, tn.ID, ag.ID); err != nil {
		t.Fatal(err)
	}
	r.waitFor("health to recover once the agent is stopped", func() bool { status, _ := r.health(); return status == http.StatusOK })
}

func TestTheNotifiersTokenOpensOnlyItsRoutes(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	tn, err := r.client.CreateTenant(ctx, api.NewTenant{Name: "alice", Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	ag, err := r.client.CreateAgent(ctx, tn.ID, api.NewAgent{Name: "btc-eth", Mode: "testnet", Config: agentConfig(t, nil)})
	if err != nil {
		t.Fatal(err)
	}
	notifier := &api.Client{BaseURL: r.client.BaseURL, Token: notifierBearer}
	if agents, err := notifier.Agents(ctx, tn.ID); err != nil || len(agents) != 1 {
		t.Fatalf("the notifier lists agents: %v %v", agents, err)
	}
	if _, err := notifier.Status(ctx, tn.ID, ag.ID); code(err) != "no_account" {
		t.Fatalf("the notifier reads status: %v", err)
	}
	if _, err := notifier.Command(ctx, tn.ID, ag.ID, api.Command{Command: api.Decide, Via: "telegram"}); code(err) != "not_running" {
		t.Fatalf("the notifier sends commands: %v", err)
	}
	sctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if err := notifier.Stream(sctx, tn.ID, ag.ID, 0, func(api.Event) error { return nil }); code(err) != "" {
		t.Fatalf("the notifier streams events: %v", err)
	}
	for what, call := range map[string]func() error{
		"tenants": func() error { _, err := notifier.Tenant(ctx, tn.ID); return err },
		"plans":   func() error { _, err := notifier.SetPlan(ctx, tn.ID, plan); return err },
		"keys":    func() error { _, err := notifier.Keys(ctx, tn.ID); return err },
		"starts":  func() error { _, err := notifier.Start(ctx, tn.ID, ag.ID); return err },
		"stops":   func() error { _, err := notifier.Stop(ctx, tn.ID, ag.ID); return err },
		"configs": func() error { _, err := notifier.Config(ctx, tn.ID, ag.ID, 0); return err },
		"events":  func() error { _, err := notifier.Events(ctx, tn.ID, ag.ID, 0, 10); return err },
		"closing": func() error { _, err := notifier.CloseTenant(ctx, tn.ID); return err },
		"new tenants": func() error {
			_, err := notifier.CreateTenant(ctx, api.NewTenant{Name: "mallory", Plan: plan})
			return err
		},
	} {
		if err := call(); code(err) != "forbidden" {
			t.Errorf("the notifier's token must not reach %s: %v", what, err)
		}
	}
	if _, err := (&api.Client{BaseURL: r.client.BaseURL, Token: "wrong"}).Agents(ctx, tn.ID); code(err) != "unauthorized" {
		t.Fatalf("a wrong token is unauthorized: %v", err)
	}
}
