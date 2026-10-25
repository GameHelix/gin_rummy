package pg

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ismatsamadov/trader/internal/vault"
)

func TestTenantsAgentsVersionsAndKeys(t *testing.T) {
	url := testDB(t)
	ctx := context.Background()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, b := Tenant{ID: uuid.New(), Name: "alice", Entitlements: json.RawMessage(`{"symbols":["BTCUSDT"]}`)}, Tenant{ID: uuid.New(), Name: "bob", Entitlements: json.RawMessage(`{}`)}
	for _, tn := range []Tenant{a, b} {
		if err := s.CreateTenant(ctx, tn); err != nil {
			t.Fatal(err)
		}
	}
	agent := AgentRow{ID: uuid.New(), Tenant: a.ID, Name: "btc", Mode: "testnet"}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAgent(ctx, AgentRow{ID: uuid.New(), Tenant: a.ID, Name: "btc", Mode: "live"}); err == nil {
		t.Fatal("agent names are unique per tenant")
	}
	for i, cfg := range []string{`{"capital_usd":100}`, `{"capital_usd":200}`} {
		v, err := s.AddVersion(ctx, a.ID, agent.ID, json.RawMessage(cfg), "test")
		if err != nil || v != i+1 {
			t.Fatalf("version %d: %v", v, err)
		}
	}
	if old, _ := s.Version(ctx, a.ID, agent.ID, 1); string(old) != `{"capital_usd": 100}` {
		t.Fatalf("earlier versions are kept as they were: %s", old)
	}
	if run, _ := s.Runnable(ctx); len(run) != 0 {
		t.Fatal("a stopped agent must not run")
	}
	if err := s.SetStatus(ctx, a.ID, agent.ID, StatusActive); err != nil {
		t.Fatal(err)
	}
	if run, _ := s.Runnable(ctx); len(run) != 0 {
		t.Fatal("an agent without keys must not run")
	}

	master, _ := vault.NewMasterKey()
	v, _ := vault.New(master, 1)
	id, model := uuid.New(), uuid.New()
	sealed, _ := v.Seal([]byte(id.String()), []byte(`{"key":"k","secret":"s"}`))
	for kind, cid := range map[string]uuid.UUID{"binance": id, "anthropic": model} {
		if err := s.PutCredential(ctx, Credential{ID: cid, Tenant: a.ID, Kind: kind, Label: "main", Hint: "…wxyz", Sealed: sealed}); err != nil {
			t.Fatal(err)
		}
		if err := s.Attach(ctx, a.ID, agent.ID, cid, kind); err != nil {
			t.Fatal(err)
		}
	}
	if run, err := s.Runnable(ctx); err != nil || len(run) != 1 || run[0].Agent != agent.ID || run[0].Version != 2 || run[0].Keys != model.String()+","+id.String() {
		t.Fatalf("runnable: %+v %v", run, err)
	}
	second := AgentRow{ID: uuid.New(), Tenant: a.ID, Name: "eth", Mode: "testnet"}
	s.CreateAgent(ctx, second)
	if err := s.Attach(ctx, a.ID, second.ID, id, "binance"); err == nil {
		t.Fatal("an exchange key serves one agent only")
	}
	c, err := s.Credential(ctx, a.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	if plain, err := v.Open([]byte(id.String()), c.Sealed); err != nil || string(plain) != `{"key":"k","secret":"s"}` {
		t.Fatalf("stored key: %s %v", plain, err)
	}
	if _, err := s.Credential(ctx, b.ID, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another tenant must not see the key: %v", err)
	}
	if _, err := s.Agent(ctx, b.ID, agent.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("another tenant must not see the agent")
	}
	err = s.inTenant(ctx, a.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "update agent_versions set config = '{}'")
		return err
	})
	if err == nil {
		t.Fatal("config versions must not be editable")
	}
	if keys, _ := s.Credentials(ctx, a.ID); len(keys) != 2 {
		t.Fatalf("keys: %+v", keys)
	}
	if users, err := s.RevokeCredential(ctx, a.ID, id); err != nil || len(users) != 1 || users[0] != agent.ID {
		t.Fatalf("revoke: %v %v", users, err)
	}
	if att, _ := s.Attached(ctx, a.ID, agent.ID); len(att) != 1 {
		t.Fatal("a revoked key is detached")
	}
	if run, _ := s.Runnable(ctx); len(run) != 0 {
		t.Fatal("an agent whose exchange key was revoked must not run")
	}
	if _, err := s.RevokeCredential(ctx, a.ID, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking twice: %v", err)
	}
}
