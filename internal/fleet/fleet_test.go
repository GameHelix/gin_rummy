package fleet

import (
	"context"
	"encoding/json"
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
	"github.com/jackc/pgx/v5"

	"github.com/ismatsamadov/trader/internal/agent"
	"github.com/ismatsamadov/trader/internal/analyst"
	"github.com/ismatsamadov/trader/internal/binance"
	"github.com/ismatsamadov/trader/internal/pg"
	"github.com/ismatsamadov/trader/internal/testkit"
	"github.com/ismatsamadov/trader/internal/vault"
)

// service returns a fleet service on a fresh, migrated database, or skips.
func service(t *testing.T) *Service {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("set TEST_DATABASE_URL to run fleet tests")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	name := "fleet_test_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := admin.Exec(ctx, "create database "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c, err := pgx.Connect(context.Background(), base); err == nil {
			c.Exec(context.Background(), "drop database "+name+" with (force)")
			c.Close(context.Background())
		}
	})
	i := strings.LastIndex(base, "/")
	url := base[:i+1] + name
	if q := strings.Index(base[i:], "?"); q >= 0 {
		url += base[i+q:]
	}
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
	tenant, err := s.AddTenant(ctx, "alice", Entitlements{Symbols: []string{"BTCUSDT"}, MaxAgents: 1, MinDecisionIntervalHours: 4, MaxEventDecisionsPerDay: 8})
	if err != nil {
		t.Fatal(err)
	}
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
		if _, err := s.AddAgent(ctx, tenant, name, tc.mode, agentConfig(t, tc.edit), "test"); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
	agentID, err := s.AddAgent(ctx, tenant, "btc", "testnet", agentConfig(t, btcOnly), "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddAgent(ctx, tenant, "second", "testnet", agentConfig(t, btcOnly), "test"); err == nil {
		t.Fatal("the plan allows one agent")
	}
	if err := s.Start(ctx, tenant, agentID); err == nil {
		t.Fatal("an agent cannot start without its keys")
	}
	for kind, raw := range map[string]string{KindAnthropic: `{"key":"k","secret":"s"}`, KindBinance: `{"key":"k"}`, KindTelegram: `{"token":"t"}`} {
		if _, err := s.AddKey(ctx, tenant, kind, "bad", []byte(raw)); err == nil {
			t.Fatalf("a malformed %s key must be refused", kind)
		}
	}
	c, err := s.AddKey(ctx, tenant, KindBinance, "main", []byte(`{"key":"ABCDEFGHwxyz","secret":"top-secret"}`))
	if err != nil || c.Hint != "…wxyz" || strings.Contains(string(c.Sealed.Ciphertext), "top-secret") {
		t.Fatalf("stored key: %+v %v", c, err)
	}
}

func TestAPlanDowngradeStopsTheAgentsItNoLongerAllows(t *testing.T) {
	s := service(t)
	ctx := context.Background()
	tenant, _ := s.AddTenant(ctx, "alice", plan)
	model, _ := s.AddKey(ctx, tenant, KindAnthropic, "main", []byte(`{"key":"sk"}`))
	var agents []uuid.UUID
	for i, name := range []string{"first", "second"} {
		id, err := s.AddAgent(ctx, tenant, name, "testnet", agentConfig(t, nil), "test")
		if err != nil {
			t.Fatal(err)
		}
		ex, _ := s.AddKey(ctx, tenant, KindBinance, name, []byte(fmt.Sprintf(`{"key":"k%d","secret":"s"}`, i)))
		s.AttachKey(ctx, tenant, id, model.ID)
		s.AttachKey(ctx, tenant, id, ex.ID)
		if err := s.Start(ctx, tenant, id); err != nil {
			t.Fatal(err)
		}
		agents = append(agents, id)
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
		tenant, err := s.AddTenant(ctx, name, plan)
		if err != nil {
			t.Fatal(err)
		}
		agentID, err := s.AddAgent(ctx, tenant, "btc-eth", "testnet", agentConfig(t, nil), "test")
		if err != nil {
			t.Fatal(err)
		}
		for kind, raw := range map[string]string{KindAnthropic: `{"key":"sk-` + name + `"}`, KindBinance: `{"key":"key-` + name + `","secret":"secret-` + name + `"}`} {
			c, err := s.AddKey(ctx, tenant, kind, "main", []byte(raw))
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
	tenant, _ := s.AddTenant(ctx, "alice", plan)
	agentID, err := s.AddAgent(ctx, tenant, "btc-eth", "testnet", agentConfig(t, nil), "test")
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]uuid.UUID{}
	for kind, raw := range map[string]string{KindAnthropic: `{"key":"sk"}`, KindBinance: `{"key":"` + testkit.Key + `","secret":"` + testkit.Secret + `"}`} {
		c, _ := s.AddKey(ctx, tenant, kind, "main", []byte(raw))
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
	bot, _ := s.AddKey(ctx, tenant, KindTelegram, "main", []byte(`{"token":"t","chat_id":1}`))
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
