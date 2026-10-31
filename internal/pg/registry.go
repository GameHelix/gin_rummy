package pg

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/candylario/trader/internal/vault"
)

// ErrNotFound means the row does not exist for this tenant.
var ErrNotFound = errors.New("not found")

func found(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// ErrExists means the ID or a unique name is already taken.
var ErrExists = errors.New("already exists")

func unique(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return ErrExists
	}
	return err
}

type Tenant struct {
	ID     uuid.UUID
	Name   string
	Status string
}

// TenantClosed is the status of an account closed for good.
const TenantClosed = "closed"

// CloseTenant closes an account for good: every key is revoked and loses
// its hint and sealed secret, and the tenant keeps its id but not its name.
// Closing twice changes nothing more.
func (s *Store) CloseTenant(ctx context.Context, tenant uuid.UUID) error {
	return s.inTenant(ctx, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "update tenants set status = $2, name = '' where id = $1", tenant, TenantClosed)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		_, err = tx.Exec(ctx, `update credentials set revoked_at = coalesce(revoked_at, now()), hint = '',
			ciphertext = '', nonce = '', wrapped_key = '', key_nonce = '' where tenant_id = $1`, tenant)
		return err
	})
}

func (s *Store) CreateTenant(ctx context.Context, t Tenant) error {
	return s.inTenant(ctx, t.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "insert into tenants (id, name) values ($1, $2)", t.ID, t.Name)
		return unique(err)
	})
}

func (s *Store) Tenant(ctx context.Context, id uuid.UUID) (Tenant, error) {
	t := Tenant{ID: id}
	err := s.inTenant(ctx, id, func(tx pgx.Tx) error {
		return found(tx.QueryRow(ctx, "select name, status from tenants where id = $1", id).Scan(&t.Name, &t.Status))
	})
	return t, err
}

type Credential struct {
	ID        uuid.UUID
	Tenant    uuid.UUID
	Kind      string
	Label     string
	Hint      string
	Sealed    vault.Sealed
	CreatedAt time.Time
	Revoked   bool
}

func (s *Store) PutCredential(ctx context.Context, c Credential) error {
	return s.inTenant(ctx, c.Tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `insert into credentials (id, tenant_id, kind, label, hint, ciphertext, nonce, wrapped_key, key_nonce, master_version)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`, c.ID, c.Tenant, c.Kind, c.Label, c.Hint,
			c.Sealed.Ciphertext, c.Sealed.Nonce, c.Sealed.WrappedKey, c.Sealed.KeyNonce, c.Sealed.MasterVersion)
		return unique(err)
	})
}

const credentialColumns = "id, tenant_id, kind, label, hint, ciphertext, nonce, wrapped_key, key_nonce, master_version, created_at, revoked_at is not null"

func scanCredential(row pgx.Row, c *Credential) error {
	return found(row.Scan(&c.ID, &c.Tenant, &c.Kind, &c.Label, &c.Hint, &c.Sealed.Ciphertext, &c.Sealed.Nonce, &c.Sealed.WrappedKey, &c.Sealed.KeyNonce, &c.Sealed.MasterVersion, &c.CreatedAt, &c.Revoked))
}

func (s *Store) Credential(ctx context.Context, tenant, id uuid.UUID) (Credential, error) {
	var c Credential
	err := s.inTenant(ctx, tenant, func(tx pgx.Tx) error {
		return scanCredential(tx.QueryRow(ctx, "select "+credentialColumns+" from credentials where id = $1", id), &c)
	})
	return c, err
}

func (s *Store) Credentials(ctx context.Context, tenant uuid.UUID) ([]Credential, error) {
	var out []Credential
	err := s.inTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "select "+credentialColumns+" from credentials order by created_at")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c Credential
			if err := scanCredential(rows, &c); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// StaleKey names a key sealed under a master key version that is not the
// current one.
type StaleKey struct {
	Tenant uuid.UUID
	ID     uuid.UUID
}

// StaleCredentials lists, across tenants, the keys still in use that are
// sealed under any master key version but current.
func (s *Store) StaleCredentials(ctx context.Context, current int) ([]StaleKey, error) {
	rows, err := s.pool.Query(ctx, "select tenant_id, id from credentials_to_rewrap($1)", current)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StaleKey
	for rows.Next() {
		var k StaleKey
		if err := rows.Scan(&k.Tenant, &k.ID); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RewrapCredential stores a key's data key as sealed under another master
// key version. The secret's own ciphertext is unchanged.
func (s *Store) RewrapCredential(ctx context.Context, tenant, id uuid.UUID, sealed vault.Sealed) error {
	return s.inTenant(ctx, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "update credentials set wrapped_key = $2, key_nonce = $3, master_version = $4 where id = $1 and revoked_at is null",
			id, sealed.WrappedKey, sealed.KeyNonce, sealed.MasterVersion)
		if err == nil && tag.RowsAffected() == 0 {
			err = ErrNotFound
		}
		return err
	})
}

// ResealCredential replaces a credential's secret with one sealed anew,
// when what it holds has changed. A revoked credential is left alone.
func (s *Store) ResealCredential(ctx context.Context, tenant, id uuid.UUID, sealed vault.Sealed) error {
	return s.inTenant(ctx, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "update credentials set ciphertext = $2, nonce = $3, wrapped_key = $4, key_nonce = $5, master_version = $6 where id = $1 and revoked_at is null",
			id, sealed.Ciphertext, sealed.Nonce, sealed.WrappedKey, sealed.KeyNonce, sealed.MasterVersion)
		if err == nil && tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return err
	})
}
