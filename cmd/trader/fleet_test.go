package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/candylario/trader/internal/testkit"
	"github.com/candylario/trader/internal/vault"
)

// fleetEnv points DATABASE_URL at a fresh database and sets a master key,
// or skips the test.
func fleetEnv(t *testing.T) {
	t.Helper()
	master, _ := vault.NewMasterKey()
	t.Setenv("DATABASE_URL", testkit.Database(t))
	t.Setenv("ENGINE_MASTER_KEY", master)
}

var idPattern = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

func TestImportMovesTheStandaloneAgentIntoTheEngine(t *testing.T) {
	cfg, e, _ := setup(t)
	fleetEnv(t)
	ctx := context.Background()
	if err := run(ctx, []string{"init", "--config", cfg}, &bytes.Buffer{}, e); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run(ctx, []string{"import", "--config", cfg}, &out, e); err != nil {
		t.Fatalf("import: %v\n%s", err, out.String())
	}
	ids := idPattern.FindAllString(out.String(), -1)
	if len(ids) != 2 || !strings.Contains(out.String(), "Imported cli-test") {
		t.Fatalf("output: %s", out.String())
	}
	data := filepath.Join(filepath.Dir(cfg), "data")
	if _, err := os.Stat(filepath.Join(data, "agents", ids[0], "testnet", "state.json")); err != nil {
		t.Fatalf("the account must move under its agent ID: %v", err)
	}
	out.Reset()
	if err := run(ctx, []string{"agent", "list", "--tenant", ids[1], "--config", cfg}, &out, e); err != nil || !strings.Contains(out.String(), "active") {
		t.Fatalf("list: %v %s", err, out.String())
	}
	out.Reset()
	if err := run(ctx, []string{"agent", "status", "--tenant", ids[1], "--agent", ids[0], "--config", cfg}, &out, e); err != nil || !strings.Contains(out.String(), "100") {
		t.Fatalf("status: %v %s", err, out.String())
	}
}

func TestTenantKeyAndAgentCommands(t *testing.T) {
	cfg, e, _ := setup(t)
	fleetEnv(t)
	ctx := context.Background()
	dir := filepath.Dir(cfg)
	plan := filepath.Join(dir, "plan.json")
	os.WriteFile(plan, []byte(`{"symbols":["BTCUSDT","ETHUSDT"],"max_agents":2,"min_decision_interval_hours":1,"max_event_decisions_per_day":8}`), 0o600)
	var out bytes.Buffer
	if err := run(ctx, []string{"tenant", "add", "--name", "bob", "--plan", plan, "--config", cfg}, &out, e); err != nil {
		t.Fatal(err)
	}
	tenant := idPattern.FindString(out.String())
	var keys []string
	for kind, secret := range map[string]string{"anthropic": `{"key":"test-key"}`, "binance": `{"key":"` + testkit.Key + `","secret":"` + testkit.Secret + `"}`} {
		out.Reset()
		e.In = strings.NewReader(secret)
		if err := run(ctx, []string{"key", "add", "--tenant", tenant, "--kind", kind, "--config", cfg}, &out, e); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "test-key") || strings.Contains(out.String(), testkit.Secret) {
			t.Fatalf("a key must never be printed: %s", out.String())
		}
		keys = append(keys, idPattern.FindString(out.String()))
	}
	out.Reset()
	if err := run(ctx, []string{"agent", "add", "--tenant", tenant, "--name", "btc-eth", "--keys", strings.Join(keys, ","), "--config", cfg}, &out, e); err != nil {
		t.Fatal(err)
	}
	agentID := idPattern.FindString(out.String())
	if err := run(ctx, []string{"agent", "start", "--tenant", tenant, "--agent", agentID, "--config", cfg}, &bytes.Buffer{}, e); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	run(ctx, []string{"agent", "list", "--tenant", tenant, "--config", cfg}, &out, e)
	if !strings.Contains(out.String(), "btc-eth") || !strings.Contains(out.String(), "active") {
		t.Fatalf("list: %s", out.String())
	}
	out.Reset()
	if err := run(ctx, []string{"key", "revoke", "--tenant", tenant, "--key", keys[1], "--config", cfg}, &out, e); err != nil || !strings.Contains(out.String(), "stopped agent "+agentID) {
		t.Fatalf("revoke: %v %s", err, out.String())
	}
	out.Reset()
	run(ctx, []string{"key", "list", "--tenant", tenant, "--config", cfg}, &out, e)
	if strings.Count(out.String(), "\n") != 2 || !strings.Contains(out.String(), "revoked") || strings.Contains(out.String(), testkit.Secret) {
		t.Fatalf("key list: %s", out.String())
	}
	os.WriteFile(plan, []byte(`{"symbols":["BTCUSDT"],"max_agents":1,"min_decision_interval_hours":4,"max_event_decisions_per_day":8}`), 0o600)
	out.Reset()
	if err := run(ctx, []string{"tenant", "plan", "--tenant", tenant, "--plan", plan, "--config", cfg}, &out, e); err != nil || !strings.Contains(out.String(), "plan updated") {
		t.Fatalf("plan: %v %s", err, out.String())
	}
}
