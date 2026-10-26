package pg

import (
	"context"
	"encoding/json"
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

// ErrKeyInUse means an exchange key already serves another agent.
var ErrKeyInUse = errors.New("that key is used by another agent")

func unique(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		if pe.ConstraintName == "agent_credentials_exclusive" {
			return ErrKeyInUse
		}
		return ErrExists
	}
	return err
}

type Tenant struct {
	ID           uuid.UUID
	Name         string
	Status       string
	Entitlements json.RawMessage
}

// TenantClosed is the status of an account closed for good.
const TenantClosed = "closed"

// CloseTenant closes an account for good: its agents stop, every key is
// revoked and loses its hint and sealed secret, and the tenant keeps its id
// and history but not its name. It returns the agents it stopped.
func (s *Store) CloseTenant(ctx context.Context, tenant uuid.UUID) ([]uuid.UUID, error) {
	var agents []uuid.UUID
	err := s.inTenant(ctx, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "update tenants set status = $2, name = '' where id = $1", tenant, TenantClosed)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, `update credentials set revoked_at = coalesce(revoked_at, now()), hint = '',
			ciphertext = '', nonce = '', wrapped_key = '', key_nonce = '' where tenant_id = $1`, tenant); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "delete from agent_credentials where tenant_id = $1", tenant); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, "update agents set status = $2 where tenant_id = $1 and status = $3 returning id", tenant, StatusStopped, StatusActive)
		if err != nil {
			return err
		}
		agents, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	})
	return agents, err
}

func (s *Store) CreateTenant(ctx context.Context, t Tenant) error {
	return s.inTenant(ctx, t.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "insert into tenants (id, name, entitlements) values ($1, $2, $3)", t.ID, t.Name, string(t.Entitlements))
		return unique(err)
	})
}

func (s *Store) Tenant(ctx context.Context, id uuid.UUID) (Tenant, error) {
	t := Tenant{ID: id}
	err := s.inTenant(ctx, id, func(tx pgx.Tx) error {
		var ent string
		err := tx.QueryRow(ctx, "select name, status, entitlements::text from tenants where id = $1", id).Scan(&t.Name, &t.Status, &ent)
		t.Entitlements = json.RawMessage(ent)
		return found(err)
	})
	return t, err
}

func (s *Store) SetEntitlements(ctx context.Context, tenant uuid.UUID, ent json.RawMessage) error {
	return s.inTenant(ctx, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "update tenants set entitlements = $2 where id = $1", tenant, string(ent))
		if err == nil && tag.RowsAffected() == 0 {
			err = ErrNotFound
		}
		return err
	})
}

type AgentRow struct {
	ID      uuid.UUID
	Tenant  uuid.UUID
	Name    string
	Mode    string
	Status  string
	Version int
}

const (
	StatusActive  = "active"
	StatusStopped = "stopped"
)

// CreateAgent stores a new, stopped agent with its first config, if given,
// and its keys by kind: all of it or nothing.
func (s *Store) CreateAgent(ctx context.Context, a AgentRow, config json.RawMessage, by string, keys map[string]uuid.UUID) error {
	return s.inTenant(ctx, a.Tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "insert into agents (id, tenant_id, name, mode) values ($1, $2, $3, $4)", a.ID, a.Tenant, a.Name, a.Mode); err != nil {
			return unique(err)
		}
		if config != nil {
			if _, err := tx.Exec(ctx, "insert into agent_versions (agent_id, tenant_id, version, config, created_by) values ($1, $2, 1, $3, $4)", a.ID, a.Tenant, string(config), by); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "update agents set version = 1 where id = $1", a.ID); err != nil {
				return err
			}
		}
		for kind, id := range keys {
			if _, err := tx.Exec(ctx, "insert into agent_credentials (agent_id, tenant_id, kind, credential_id) values ($1, $2, $3, $4)", a.ID, a.Tenant, kind, id); err != nil {
				return unique(err)
			}
		}
		return nil
	})
}

func scanAgent(row pgx.Row, a *AgentRow) error {
	return found(row.Scan(&a.ID, &a.Tenant, &a.Name, &a.Mode, &a.Status, &a.Version))
}

const agentColumns = "id, tenant_id, name, mode, status, version"

func (s *Store) Agent(ctx context.Context, tenant, id uuid.UUID) (AgentRow, error) {
	var a AgentRow
	err := s.inTenant(ctx, tenant, func(tx pgx.Tx) error {
		return scanAgent(tx.QueryRow(ctx, "select "+agentColumns+" from agents where id = $1", id), &a)
	})
	return a, err
}

func (s *Store) Agents(ctx context.Context, tenant uuid.UUID) ([]AgentRow, error) {
	var out []AgentRow
	err := s.inTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "select "+agentColumns+" from agents order by name")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a AgentRow
			if err := scanAgent(rows, &a); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

// AddVersion stores a new config as the agent's next version and makes it
// current. Earlier versions stay as they were.
func (s *Store) AddVersion(ctx context.Context, tenant, agent uuid.UUID, config json.RawMessage, by string) (int, error) {
	var version int
	err := s.inTenant(ctx, tenant, func(tx pgx.Tx) error {
		if err := found(tx.QueryRow(ctx, "select version from agents where id = $1 for update", agent).Scan(&version)); err != nil {
			return err
		}
		version++
		if _, err := tx.Exec(ctx, "insert into agent_versions (agent_id, tenant_id, version, config, created_by) values ($1, $2, $3, $4, $5)", agent, tenant, version, string(config), by); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "update agents set version = $2 where id = $1", agent, version)
		return err
	})
	return version, err
}

func (s *Store) Version(ctx context.Context, tenant, agent uuid.UUID, version int) (json.RawMessage, error) {
	var config string
	err := s.inTenant(ctx, tenant, func(tx pgx.Tx) error {
		return found(tx.QueryRow(ctx, "select config::text from agent_versions where agent_id = $1 and version = $2", agent, version).Scan(&config))
	})
	return json.RawMessage(config), err
}

func (s *Store) SetStatus(ctx context.Context, tenant, agent uuid.UUID, status string) error {
	return s.inTenant(ctx, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "update agents set status = $2 where id = $1", agent, status)
		if err == nil && tag.RowsAffected() == 0 {
			err = ErrNotFound
		}
		return err
	})
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

// RevokeCredential marks a key revoked and detaches it, returning the agents
// that used it.
func (s *Store) RevokeCredential(ctx context.Context, tenant, id uuid.UUID) ([]uuid.UUID, error) {
	var agents []uuid.UUID
	err := s.inTenant(ctx, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "update credentials set revoked_at = now() where id = $1 and revoked_at is null", id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		rows, err := tx.Query(ctx, "delete from agent_credentials where credential_id = $1 returning agent_id", id)
		if err != nil {
			return err
		}
		agents, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	})
	return agents, err
}

// Attach makes the agent use the credential for its kind, replacing any
// key of that kind it used before.
func (s *Store) Attach(ctx context.Context, tenant, agent, credential uuid.UUID, kind string) error {
	return s.inTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `insert into agent_credentials (agent_id, tenant_id, kind, credential_id) values ($1, $2, $3, $4)
			on conflict (agent_id, kind) do update set credential_id = excluded.credential_id`, agent, tenant, kind, credential)
		return unique(err)
	})
}

// Attached lists the credential each kind of key resolves to for the agent.
func (s *Store) Attached(ctx context.Context, tenant, agent uuid.UUID) (map[string]uuid.UUID, error) {
	out := map[string]uuid.UUID{}
	err := s.inTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "select kind, credential_id from agent_credentials where agent_id = $1", agent)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var kind string
			var id uuid.UUID
			if err := rows.Scan(&kind, &id); err != nil {
				return err
			}
			out[kind] = id
		}
		return rows.Err()
	})
	return out, err
}

// Runnable is an agent that should be running, at a config version and with
// a set of keys. A change to either restarts it.
type Runnable struct {
	Agent   uuid.UUID
	Tenant  uuid.UUID
	Version int
	Keys    string
}

// Runnable lists every agent, across tenants, that should be running.
func (s *Store) Runnable(ctx context.Context) ([]Runnable, error) {
	rows, err := s.pool.Query(ctx, "select agent_id, tenant_id, version, keys from runnable_agents()")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Runnable
	for rows.Next() {
		var r Runnable
		if err := rows.Scan(&r.Agent, &r.Tenant, &r.Version, &r.Keys); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
