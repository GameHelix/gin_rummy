package pg

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/candylario/trader/internal/testkit"
	"github.com/candylario/trader/internal/vault"
)

// testDB returns a fresh, migrated database, or skips the test.
func testDB(t *testing.T) string {
	t.Helper()
	url := testkit.Database(t)
	ctx := context.Background()
	if err := Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, url); err != nil {
		t.Fatalf("migrations must be idempotent: %v", err)
	}
	return url
}

func TestTenantsAndTheirKeysStayApart(t *testing.T) {
	url := testDB(t)
	ctx := context.Background()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, b := Tenant{ID: uuid.New(), Name: "alice"}, Tenant{ID: uuid.New(), Name: "bob"}
	for _, tn := range []Tenant{a, b} {
		if err := s.CreateTenant(ctx, tn); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateTenant(ctx, a); !errors.Is(err, ErrExists) {
		t.Fatalf("tenant IDs are unique: %v", err)
	}
	master, _ := vault.NewMasterKey()
	v, _ := vault.New(master)
	id := uuid.New()
	sealed, _ := v.Seal([]byte(id.String()), []byte(`{"key":"k"}`))
	if err := s.PutCredential(ctx, Credential{ID: id, Tenant: a.ID, Kind: "hyperliquid", Label: "wallet", Hint: "0xabc", Sealed: sealed}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutCredential(ctx, Credential{ID: uuid.New(), Tenant: a.ID, Kind: "anthropic", Label: "model", Hint: "…wxyz", Sealed: sealed}); err == nil {
		t.Fatal("only wallets are stored")
	}
	c, err := s.Credential(ctx, a.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	if plain, err := v.Open([]byte(id.String()), c.Sealed); err != nil || string(plain) != `{"key":"k"}` {
		t.Fatalf("stored key: %s %v", plain, err)
	}
	if _, err := s.Credential(ctx, b.ID, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another tenant must not see the key: %v", err)
	}
	var none int
	pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error { return tx.QueryRow(ctx, "select count(*) from credentials").Scan(&none) })
	if none != 0 {
		t.Fatal("without a tenant no keys are visible")
	}

	if err := s.CloseTenant(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if tn, _ := s.Tenant(ctx, a.ID); tn.Status != TenantClosed || tn.Name != "" {
		t.Fatalf("a closed tenant keeps no name: %+v", tn)
	}
	if c, err := s.Credential(ctx, a.ID, id); err != nil || !c.Revoked || c.Hint != "" || len(c.Sealed.Ciphertext) != 0 || len(c.Sealed.WrappedKey) != 0 {
		t.Fatalf("the key is not erased: %+v %v", c, err)
	}
	if err := s.CloseTenant(ctx, a.ID); err != nil {
		t.Fatalf("closing twice: %v", err)
	}
	if err := s.CloseTenant(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown tenant: %v", err)
	}
	if tn, _ := s.Tenant(ctx, b.ID); tn.Status != "active" || tn.Name != "bob" {
		t.Fatalf("another tenant changed: %+v", tn)
	}
}

// The agent's last migration deletes the model and exchange keys and stops
// the agents, and leaves wallets and the audit log alone.
func TestTheAgentsKeysGoWithIt(t *testing.T) {
	url := testkit.Database(t)
	ctx := context.Background()
	if err := migrate(ctx, url, 6); err != nil {
		t.Fatal(err)
	}
	owner, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(ctx)
	tenant, agent := uuid.New(), uuid.New()
	keys := map[string]uuid.UUID{"anthropic": uuid.New(), "binance": uuid.New(), "hyperliquid": uuid.New()}
	if _, err := owner.Exec(ctx, "select ensure_event_partition('2026-10-30')"); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		sql  string
		args []any
	}{
		{"insert into tenants (id, name) values ($1, 'alice')", []any{tenant}},
		{"insert into agents (id, tenant_id, name, mode, status) values ($1, $2, 'btc-eth', 'testnet', 'active')", []any{agent, tenant}},
		{"insert into events (id, tenant_id, agent_id, seq, type, at, body, hash) values (gen_random_uuid(), $1, $2, 1, 'notice', '2026-10-30T00:00:00Z', '{}', '\\x00')", []any{tenant, agent}},
	}
	for _, st := range steps {
		if _, err := owner.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatal(st.sql, err)
		}
	}
	for kind, id := range keys {
		if _, err := owner.Exec(ctx, `insert into credentials (id, tenant_id, kind, label, hint, ciphertext, nonce, wrapped_key, key_nonce, master_version)
			values ($1, $2, $3, 'main', '…', '\x01', '\x01', '\x01', '\x01', 1)`, id, tenant, kind); err != nil {
			t.Fatal(err)
		}
		if kind != "hyperliquid" {
			if _, err := owner.Exec(ctx, "insert into agent_credentials (agent_id, tenant_id, kind, credential_id) values ($1, $2, $3, $4)", agent, tenant, kind, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	rows, _ := owner.Query(ctx, "select kind from credentials")
	kinds, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(kinds) != 1 || kinds[0] != "hyperliquid" {
		t.Fatalf("only the wallet is left: %v %v", kinds, err)
	}
	var attached, events int
	var status string
	owner.QueryRow(ctx, "select count(*) from agent_credentials").Scan(&attached)
	owner.QueryRow(ctx, "select status from agents where id = $1", agent).Scan(&status)
	owner.QueryRow(ctx, "select count(*) from events").Scan(&events)
	if attached != 0 || status != "stopped" || events != 1 {
		t.Fatalf("attached %d, agent %s, events %d", attached, status, events)
	}
}
