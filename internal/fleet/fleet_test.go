package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/google/uuid"

	"github.com/wickwise/trader/internal/agent"
	"github.com/wickwise/trader/internal/analyst"
	"github.com/wickwise/trader/internal/binance"
	"github.com/wickwise/trader/internal/pg"
	"github.com/wickwise/trader/internal/testkit"
	"github.com/wickwise/trader/internal/vault"
)

// service returns a fleet service on a fresh, migrated database, or skips.
func service(t *testing.T) *Service {
	t.Helper()
	url := testkit.Database(t)
	ctx := context.Background()
	if err := pg.Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	store, err := pg.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	master, _ := vault.NewMasterKey()
	v, _ := vault.New(master, 1)
	return &Service{Store: store, Vault: v, DataDir: t.TempDir(), Model: "test-model"}
}

var plan = Entitlements{Symbols: []string{"BTCUSDT", "ETHUSDT"}, MaxAgents: 2, MinDecisionIntervalHours: 1, MaxEventDecisionsPerDay: 8}

func agentConfig(t *testing.T, edit func(m map[string]any)) []byte {
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

func TestTierLimitsAndKeysAreEnforced(t *testing.T) {
	s := service(t)
	ctx := context.Background()
	tn, _, err := s.AddTenant(ctx, uuid.Nil, "alice", Entitlements{Symbols: []string{"BTCUSDT"}, MaxAgents: 1, MinDecisionIntervalHours: 4, MaxEventDecisionsPerDay: 8})
	if err != nil {
		t.Fatal(err)
	}
	tenant := tn.ID
	btcOnly := func(m map[string]any) { m["symbols"] = []string{"BTCUSDT"} }
	for name, tc := range map[string]struct {
		mode string
		edit func(map[string]any)
	}{
		"a coin outside the plan":   {"testnet", nil},
		"live without live trading": {"live", btcOnly},
		"deciding too often":        {"testnet", func(m map[string]any) { btcOnly(m); m["decision_interval_hours"] = 1 }},
		"an unknown field":          {"testnet", func(m map[string]any) { btcOnly(m); m["leverage"] = 10 }},
	} {
		_, _, err := s.AddAgent(ctx, tenant, NewAgent{Name: name, Mode: tc.mode, Config: agentConfig(t, tc.edit), By: "test"})
		if refused := new(Refused); !errors.As(err, &refused) {
			t.Fatalf("%s must be refused: %v", name, err)
		}
	}
	id := uuid.Must(uuid.NewV7())
	row, created, err := s.AddAgent(ctx, tenant, NewAgent{ID: id, Name: "btc", Mode: "testnet", Config: agentConfig(t, btcOnly), By: "test"})
	if err != nil || !created || row.ID != id || row.Version != 1 {
		t.Fatalf("add: %+v %v %v", row, created, err)
	}
	if again, created, err := s.AddAgent(ctx, tenant, NewAgent{ID: id, Name: "btc", Mode: "testnet", Config: agentConfig(t, btcOnly), By: "test"}); err != nil || created || again.ID != id {
		t.Fatalf("a retry returns the same agent: %+v %v %v", again, created, err)
	}
	if _, _, err := s.AddAgent(ctx, tenant, NewAgent{Name: "second", Mode: "testnet", Config: agentConfig(t, btcOnly), By: "test"}); err == nil {
		t.Fatal("the plan allows one agent")
	}
	if err := s.Start(ctx, tenant, id); err == nil {
		t.Fatal("an agent cannot start without its keys")
	}
	for kind, raw := range map[string]string{KindAnthropic: `{"key":"k","secret":"s"}`, KindBinance: `{"key":"k"}`, KindTelegram: `{"token":"t"}`} {
		if _, _, err := s.AddKey(ctx, tenant, NewKey{Kind: kind, Label: "bad", Mode: "testnet", Secret: []byte(raw)}); err == nil {
			t.Fatalf("a malformed %s key must be refused", kind)
		}
	}
	if _, _, err := s.AddKey(ctx, tenant, NewKey{Kind: KindBinance, Secret: []byte(`{"key":"k","secret":"s"}`)}); err == nil {
		t.Fatal("an exchange key must say where it trades")
	}
	keyID := uuid.Must(uuid.NewV7())
	c, created, err := s.AddKey(ctx, tenant, NewKey{ID: keyID, Kind: KindBinance, Label: "main", Mode: "testnet", Secret: []byte(`{"key":"ABCDEFGHwxyz","secret":"top-secret"}`)})
	if err != nil || !created || c.Hint != "…wxyz" || c.CreatedAt.IsZero() || strings.Contains(string(c.Sealed.Ciphertext), "top-secret") {
		t.Fatalf("stored key: %+v %v", c, err)
	}
	if again, created, err := s.AddKey(ctx, tenant, NewKey{ID: keyID, Kind: KindBinance, Mode: "testnet", Secret: []byte(`{"key":"other","secret":"other"}`)}); err != nil || created || again.Hint != "…wxyz" {
		t.Fatalf("a retry returns the key stored first: %+v %v", again, err)
	}
	s.CheckKey = func(ctx context.Context, kind, mode string, k Keys) error { return errors.New("invalid API key") }
	if _, _, err := s.AddKey(ctx, tenant, NewKey{Kind: KindAnthropic, Secret: []byte(`{"key":"sk-bad"}`)}); err == nil || !strings.Contains(err.Error(), "invalid API key") {
		t.Fatalf("a key that does not work is refused: %v", err)
	}
	if again, created, err := s.AddTenant(ctx, tenant, "other", plan); err != nil || created || again.Name != "alice" {
		t.Fatalf("a retried tenant is returned unchanged: %+v %v", again, err)
	}
	if _, _, err := s.AddTenant(ctx, uuid.Nil, "bad", Entitlements{Symbols: []string{"btc/usdt"}}); err == nil {
		t.Fatal("a malformed plan is refused")
	}
}

func TestAPlanDowngradeStopsTheAgentsItNoLongerAllows(t *testing.T) {
	s := service(t)
	ctx := context.Background()
	tn, _, _ := s.AddTenant(ctx, uuid.Nil, "alice", plan)
	tenant := tn.ID
	model, _, _ := s.AddKey(ctx, tenant, NewKey{Kind: KindAnthropic, Label: "main", Secret: []byte(`{"key":"sk"}`)})
	var agents []uuid.UUID
	for i, name := range []string{"first", "second"} {
		ex, _, _ := s.AddKey(ctx, tenant, NewKey{Kind: KindBinance, Label: name, Mode: "testnet", Secret: []byte(fmt.Sprintf(`{"key":"k%d","secret":"s"}`, i))})
		row, _, err := s.AddAgent(ctx, tenant, NewAgent{Name: name, Mode: "testnet", Config: agentConfig(t, nil), Keys: []uuid.UUID{model.ID, ex.ID}, By: "test"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Start(ctx, tenant, row.ID); err != nil {
			t.Fatal(err)
		}
		agents = append(agents, row.ID)
	}
	fewer := plan
	fewer.MaxAgents = 1
	stopped, err := s.SetEntitlements(ctx, tenant, fewer)
	if err != nil || len(stopped) != 1 || stopped[agents[1]] == "" {
		t.Fatalf("the newest agent over the limit stops: %v %v", stopped, err)
	}
	if err := s.Start(ctx, tenant, agents[1]); err == nil {
		t.Fatal("an agent over the limit cannot start again")
	}
	btcOnly := fewer
	btcOnly.Symbols = []string{"BTCUSDT"}
	if stopped, _ := s.SetEntitlements(ctx, tenant, btcOnly); !strings.Contains(stopped[agents[0]], "ETHUSDT") {
		t.Fatalf("an agent trading a coin the plan dropped stops: %v", stopped)
	}
	if run, _ := s.Store.Runnable(ctx); len(run) != 0 {
		t.Fatalf("nothing may run: %+v", run)
	}
}

type tenantRig struct {
	ex     *testkit.Binance
	exURL  string
	api    *testkit.API
	apiURL string
}

func TestTwoTenantsTradeSideBySideWithTheirOwnKeys(t *testing.T) {
	s := service(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := func() time.Time { return testkit.At(t, "2026-10-25 12:00") }
	rigs := map[uuid.UUID]*tenantRig{}
	var builds atomic.Int32
	for _, name := range []string{"alice", "bob"} {
		r := &tenantRig{ex: testkit.NewBinance(now), api: &testkit.API{WantKey: "sk-" + name}}
		r.ex.APIKey, r.ex.APISecret = "key-"+name, "secret-"+name
		r.api.Script = []func(string) (int, any){testkit.SubmitStep(map[string]string{"BTCUSDT": "strong_buy", "ETHUSDT": "hold"}, testkit.CitePrices)}
		r.exURL, r.apiURL = r.ex.Server(t).URL, r.api.Server(t).URL
		tn, _, err := s.AddTenant(ctx, uuid.Nil, name, plan)
		if err != nil {
			t.Fatal(err)
		}
		tenant := tn.ID
		row, _, err := s.AddAgent(ctx, tenant, NewAgent{Name: "btc-eth", Mode: "testnet", Config: agentConfig(t, nil), By: "test"})
		if err != nil {
			t.Fatal(err)
		}
		agentID := row.ID
		for kind, raw := range map[string]string{KindAnthropic: `{"key":"sk-` + name + `"}`, KindBinance: `{"key":"key-` + name + `","secret":"secret-` + name + `"}`} {
			c, _, err := s.AddKey(ctx, tenant, NewKey{Kind: kind, Label: "main", Mode: "testnet", Secret: []byte(raw)})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.AttachKey(ctx, tenant, agentID, c.ID); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Start(ctx, tenant, agentID); err != nil {
			t.Fatal(err)
		}
		rigs[agentID] = r
	}
	sup := &Supervisor{Service: s, Every: 50 * time.Millisecond, Logf: t.Logf, Build: func(spec Spec) (*agent.Engine, error) {
		builds.Add(1)
		r := rigs[spec.Agent]
		return &agent.Engine{
			Config:   spec.Config,
			Exchange: &binance.Client{HTTP: http.DefaultClient, BaseURL: r.exURL, DataURL: r.exURL, Key: spec.Keys.BinanceKey, Secret: spec.Keys.BinanceSecret, Now: now},
			Analyst: &analyst.Analyst{Config: spec.Config, Client: anthropic.NewClient(option.WithAPIKey(spec.Keys.Anthropic), option.WithBaseURL(r.apiURL)),
				Budget: &analyst.Budget{Path: filepath.Join(spec.Config.DataDir, "budget.json"), DailyUSD: spec.Config.MaxDailyLLMUSD, MonthlyUSD: spec.Config.MaxMonthlyLLMUSD, Now: now},
				Sleep:  func(context.Context, time.Duration) error { return nil }},
			Now: now,
		}, nil
	}}
	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		ready := 0
		for _, r := range rigs {
			if len(r.ex.OrderList()) == 1 {
				ready++
			}
		}
		if ready == len(rigs) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("both agents should have bought BTC with their own keys")
		}
	}
	var mu sync.Mutex
	seen := map[string]bool{}
	for id, r := range rigs {
		o := r.ex.OrderList()[0]
		mu.Lock()
		if seen[o.ClientID] || o.Symbol != "BTCUSDT" {
			t.Fatalf("unexpected order %+v", o)
		}
		seen[o.ClientID] = true
		mu.Unlock()
		if _, err := os.Stat(filepath.Join(s.DataDir, "agents", id.String(), "testnet", "state.json")); err != nil {
			t.Fatalf("each agent keeps its own account: %v", err)
		}
	}
	cancel()
	<-done
}

func TestNewVersionsAndKeysRestartAgentsAndRevokedKeysStopThem(t *testing.T) {
	s := service(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := func() time.Time { return testkit.At(t, "2026-10-25 12:00") }
	ex := testkit.NewBinance(now)
	api := &testkit.API{Script: []func(string) (int, any){testkit.SubmitStep(map[string]string{"BTCUSDT": "hold", "ETHUSDT": "hold"}, testkit.CitePrices)}}
	exURL, apiURL := ex.Server(t).URL, api.Server(t).URL
	tn, _, _ := s.AddTenant(ctx, uuid.Nil, "alice", plan)
	tenant := tn.ID
	row, _, err := s.AddAgent(ctx, tenant, NewAgent{Name: "btc-eth", Mode: "testnet", Config: agentConfig(t, nil), By: "test"})
	if err != nil {
		t.Fatal(err)
	}
	agentID := row.ID
	keys := map[string]uuid.UUID{}
	for kind, raw := range map[string]string{KindAnthropic: `{"key":"sk"}`, KindBinance: `{"key":"` + testkit.Key + `","secret":"` + testkit.Secret + `"}`} {
		c, _, _ := s.AddKey(ctx, tenant, NewKey{Kind: kind, Label: "main", Mode: "testnet", Secret: []byte(raw)})
		s.AttachKey(ctx, tenant, agentID, c.ID)
		keys[kind] = c.ID
	}
	s.Start(ctx, tenant, agentID)
	var versions []int
	var mu sync.Mutex
	sup := &Supervisor{Service: s, Every: 50 * time.Millisecond, Logf: t.Logf, Build: func(spec Spec) (*agent.Engine, error) {
		mu.Lock()
		versions = append(versions, spec.Version)
		mu.Unlock()
		return &agent.Engine{Config: spec.Config, Now: now,
			Exchange: &binance.Client{HTTP: http.DefaultClient, BaseURL: exURL, DataURL: exURL, Key: spec.Keys.BinanceKey, Secret: spec.Keys.BinanceSecret, Now: now},
			Analyst: &analyst.Analyst{Config: spec.Config, Client: anthropic.NewClient(option.WithAPIKey("sk"), option.WithBaseURL(apiURL)),
				Budget: &analyst.Budget{Path: filepath.Join(spec.Config.DataDir, "budget.json"), DailyUSD: 5, MonthlyUSD: 50, Now: now}}}, nil
	}}
	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	got := func() []int { mu.Lock(); defer mu.Unlock(); return append([]int(nil), versions...) }
	waitFor("the first start", func() bool { return len(got()) == 1 })
	if _, err := s.SetConfig(ctx, tenant, agentID, agentConfig(t, func(m map[string]any) { m["capital_usd"] = 200 }), "test"); err != nil {
		t.Fatal(err)
	}
	waitFor("a restart on version 2", func() bool { v := got(); return len(v) == 2 && v[1] == 2 })
	bot, _, _ := s.AddKey(ctx, tenant, NewKey{Kind: KindTelegram, Label: "main", Secret: []byte(`{"token":"t","chat_id":1}`)})
	s.AttachKey(ctx, tenant, agentID, bot.ID)
	waitFor("a restart with the new key", func() bool { return len(got()) == 3 })
	if stopped, err := s.RevokeKey(ctx, tenant, keys[KindAnthropic]); err != nil || len(stopped) != 1 {
		t.Fatalf("revoking the model key stops its agent: %v %v", stopped, err)
	}
	time.Sleep(300 * time.Millisecond)
	if len(got()) != 3 {
		t.Fatal("an agent without its model key must not run")
	}
	if row, _ := s.Store.Agent(ctx, tenant, agentID); row.Status != pg.StatusStopped {
		t.Fatalf("status %q", row.Status)
	}
	cancel()
	<-done
}
