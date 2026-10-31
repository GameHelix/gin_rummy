// Package fleet holds the engine's accounts: each tenant's agent wallet on
// Hyperliquid, sealed in the vault, and the trades it places. The engine API
// and the command line call it.
package fleet

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/candylario/trader/internal/hyperliquid"
	"github.com/candylario/trader/internal/pg"
	"github.com/candylario/trader/internal/vault"
)

// Refused is an error the user can fix, such as a trade beyond the limits
// or a wallet that is not approved. Its message is safe to show them.
type Refused struct{ Err error }

func (r *Refused) Error() string { return r.Err.Error() }
func (r *Refused) Unwrap() error { return r.Err }

func refusef(format string, args ...any) error { return &Refused{fmt.Errorf(format, args...)} }

// Service manages tenants, their wallets and their trades.
type Service struct {
	Store *pg.Store
	Vault *vault.Vault
	// Exchange gives a Hyperliquid client for a wallet mode, testnet or
	// mainnet; Builder is the engine's builder address and fee, in tenths
	// of a basis point, that accounts approve.
	Exchange func(mode string) *hyperliquid.Client
	Builder  hyperliquid.Builder
	// TradeMaxUSD and TradeMaxLeverage cap every trade; zero takes the
	// defaults of $100 and 5x.
	TradeMaxUSD      float64
	TradeMaxLeverage int
}

// AddTenant creates a tenant, under the given ID or a new one. An ID that
// is taken returns that tenant unchanged, so a retried request is safe.
func (s *Service) AddTenant(ctx context.Context, id uuid.UUID, name string) (pg.Tenant, bool, error) {
	if strings.TrimSpace(name) == "" {
		return pg.Tenant{}, false, refusef("a tenant needs a name")
	}
	if id == uuid.Nil {
		id = uuid.Must(uuid.NewV7())
	}
	err := s.Store.CreateTenant(ctx, pg.Tenant{ID: id, Name: name})
	if err != nil && !errors.Is(err, pg.ErrExists) {
		return pg.Tenant{}, false, err
	}
	t, terr := s.Store.Tenant(ctx, id)
	return t, err == nil, terr
}

// CloseTenant closes an account for good: its wallet key can never be
// opened again and its name is gone. Closing twice changes nothing more.
func (s *Service) CloseTenant(ctx context.Context, tenant uuid.UUID) error {
	return s.Store.CloseTenant(ctx, tenant)
}

// open refuses changes to a closed account.
func (s *Service) open(ctx context.Context, tenant uuid.UUID) error {
	t, err := s.Store.Tenant(ctx, tenant)
	if err != nil {
		return err
	}
	if t.Status == pg.TenantClosed {
		return refusef("this account is closed")
	}
	return nil
}

func owner(tenant, credential uuid.UUID, kind string) []byte {
	return []byte(tenant.String() + "/" + credential.String() + "/" + kind)
}

// Rewrap seals every stored key's data key under the current master key,
// after a rotation, and returns how many keys it moved. A key revoked
// meanwhile is skipped. Run it with the old key still loaded; afterwards
// the engine needs only the new one.
func (s *Service) Rewrap(ctx context.Context) (int, error) {
	stale, err := s.Store.StaleCredentials(ctx, s.Vault.Version())
	if err != nil {
		return 0, err
	}
	n := 0
	for _, k := range stale {
		c, err := s.Store.Credential(ctx, k.Tenant, k.ID)
		if errors.Is(err, pg.ErrNotFound) {
			continue
		}
		if err != nil {
			return n, err
		}
		sealed, err := s.Vault.Rewrap(owner(k.Tenant, c.ID, c.Kind), c.Sealed)
		if err != nil {
			return n, fmt.Errorf("key %s of tenant %s: %w", c.ID, k.Tenant, err)
		}
		if err := s.Store.RewrapCredential(ctx, k.Tenant, c.ID, sealed); err != nil && !errors.Is(err, pg.ErrNotFound) {
			return n, err
		}
		n++
	}
	return n, nil
}
