package fleet

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/candylario/trader/internal/pg"
	"github.com/candylario/trader/internal/testkit"
	"github.com/candylario/trader/internal/vault"
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
	v, _ := vault.New(master)
	return &Service{Store: store, Vault: v}
}

func TestATenantIsMadeOnceAndClosedForGood(t *testing.T) {
	s := service(t)
	ctx := context.Background()
	var refused *Refused
	if _, _, err := s.AddTenant(ctx, uuid.Nil, " "); !errors.As(err, &refused) {
		t.Fatalf("a tenant needs a name: %v", err)
	}
	id := uuid.Must(uuid.NewV7())
	tn, isNew, err := s.AddTenant(ctx, id, "alice@example.com")
	if err != nil || !isNew || tn.ID != id || tn.Status != "active" {
		t.Fatalf("add: %+v %v %v", tn, isNew, err)
	}
	if again, isNew, err := s.AddTenant(ctx, id, "someone else"); err != nil || isNew || again.Name != "alice@example.com" {
		t.Fatalf("a retry returns the first tenant: %+v %v %v", again, isNew, err)
	}
	w, _, err := s.Wallet(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	bob, _, _ := s.AddTenant(ctx, uuid.Nil, "bob@example.com")

	if err := s.CloseTenant(ctx, id); err != nil {
		t.Fatal(err)
	}
	if tn, _ := s.Store.Tenant(ctx, id); tn.Status != pg.TenantClosed || tn.Name != "" {
		t.Fatalf("the tenant keeps no name: %+v", tn)
	}
	if c, err := s.Store.Credential(ctx, id, w.ID); err != nil || !c.Revoked || c.Hint != "" || len(c.Sealed.Ciphertext) != 0 || len(c.Sealed.WrappedKey) != 0 {
		t.Fatalf("the wallet key is not erased: %+v %v", c, err)
	}
	if _, _, err := s.Wallet(ctx, id); !errors.As(err, &refused) {
		t.Fatalf("a closed tenant gets no new wallet: %v", err)
	}
	if _, err := s.OpenTrade(ctx, id, TradeRequest{Coin: "ETH", Side: "long", USD: 20, Leverage: 1, StopLossPct: 0.1}); !errors.As(err, &refused) {
		t.Fatalf("a closed tenant places no trades: %v", err)
	}
	if err := s.CloseTenant(ctx, id); err != nil {
		t.Fatalf("closing twice: %v", err)
	}
	if err := s.CloseTenant(ctx, uuid.Must(uuid.NewV7())); !errors.Is(err, pg.ErrNotFound) {
		t.Fatalf("an unknown tenant: %v", err)
	}
	if b, _ := s.Store.Tenant(ctx, bob.ID); b.Status != "active" || b.Name != "bob@example.com" {
		t.Fatalf("another tenant changed: %+v", b)
	}
}

func TestRotatingTheMasterKeyRewrapsStoredKeys(t *testing.T) {
	s := service(t)
	ctx := context.Background()
	k1, _ := vault.NewMasterKey()
	k2, _ := vault.NewMasterKey()
	s.Vault, _ = vault.New(k1)
	wallets := map[uuid.UUID]Wallet{}
	for _, name := range []string{"alice", "bob", "carol"} {
		tn, _, err := s.AddTenant(ctx, uuid.Nil, name)
		if err != nil {
			t.Fatal(err)
		}
		w, _, err := s.Wallet(ctx, tn.ID)
		if err != nil {
			t.Fatal(err)
		}
		if name == "carol" {
			// A closed account's key is erased and left alone.
			if err := s.CloseTenant(ctx, tn.ID); err != nil {
				t.Fatal(err)
			}
			continue
		}
		wallets[tn.ID] = w
	}
	// Both keys loaded: the rewrap moves every key in use to version 2.
	var err error
	if s.Vault, err = vault.New("2:"+k2, k1); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Rewrap(ctx); err != nil || n != 2 {
		t.Fatalf("rewrapped %d keys: %v", n, err)
	}
	if n, err := s.Rewrap(ctx); err != nil || n != 0 {
		t.Fatalf("a second rewrap finds nothing: %d %v", n, err)
	}
	// The old key dropped: the wallets still open.
	if s.Vault, err = vault.New("2:" + k2); err != nil {
		t.Fatal(err)
	}
	for tenant, w := range wallets {
		got, signer, err := s.WalletSigner(ctx, tenant)
		if err != nil || got.Agent != w.Agent || signer.Address() != w.Agent {
			t.Fatalf("wallet after the rotation: %+v %v", got, err)
		}
	}
	tn, _, _ := s.AddTenant(ctx, uuid.Nil, "dave")
	w, _, err := s.Wallet(ctx, tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := s.Store.Credential(ctx, tn.ID, w.ID); err != nil || c.Sealed.MasterVersion != 2 {
		t.Fatalf("new keys seal under the current version: %+v %v", c.Sealed.MasterVersion, err)
	}
}
