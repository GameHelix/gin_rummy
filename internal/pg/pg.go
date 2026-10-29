// Package pg is the engine's Postgres store. It receives each agent's audit
// log from the local disk, checks the hash chain, and keeps it append-only
// and isolated per tenant.
package pg

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/candylario/trader/internal/audit"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store runs every query as trader_app, so row-level security and the
// append-only rules apply even when the connecting user owns the schema.
type Store struct {
	pool *pgxpool.Pool
	// months caches the event partitions known to exist; every agent's
	// shipper uses it.
	mu     sync.Mutex
	months map[string]bool
}

func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, errors.New("invalid DATABASE_URL")
	}
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "set role trader_app")
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Store{pool: pool, months: map[string]bool{}}, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Migrate applies the embedded migrations in order, once each, as the
// connecting user. Concurrent engines wait for one another.
func Migrate(ctx context.Context, url string) error {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "select pg_advisory_lock(hashtext('trader-migrations'))"); err != nil {
		return err
	}
	defer conn.Exec(context.WithoutCancel(ctx), "select pg_advisory_unlock(hashtext('trader-migrations'))")
	if _, err := conn.Exec(ctx, "create table if not exists schema_migrations (version int primary key, applied_at timestamptz not null default now())"); err != nil {
		return err
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		version, err := strconv.Atoi(strings.SplitN(strings.TrimPrefix(name, "migrations/"), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: bad name", name)
		}
		var done bool
		if err := conn.QueryRow(ctx, "select exists (select from schema_migrations where version = $1)", version).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "insert into schema_migrations (version) values ($1)", version)
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}

// Agent names the agent whose events are shipped.
type Agent struct {
	Tenant uuid.UUID
	ID     uuid.UUID
	Name   string
	Mode   string
}

// inTenant runs fn in a transaction limited to one tenant's rows.
func (s *Store) inTenant(ctx context.Context, tenant uuid.UUID, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "select set_config('app.tenant_id', $1, true)", tenant.String()); err != nil {
			return err
		}
		return fn(tx)
	})
}

// Head returns the sequence number and hash of the agent's last stored
// event, or zero for none.
func (s *Store) Head(ctx context.Context, tenant, agent uuid.UUID) (int64, []byte, error) {
	var seq int64
	var hash []byte
	err := s.inTenant(ctx, tenant, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, "select seq, hash from streams where agent_id = $1", agent).Scan(&seq, &hash)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	return seq, hash, err
}

// ErrChain means an event does not continue the stored chain: a gap, a fork
// or an edited event. Nothing from the batch is stored.
var ErrChain = errors.New("event does not continue the stored chain")

// Store saves events that continue the agent's chain, skipping any already
// stored, all in one transaction.
func (s *Store) Store(ctx context.Context, a Agent, events []audit.Event) (int, error) {
	stored := 0
	err := s.inTenant(ctx, a.Tenant, func(tx pgx.Tx) error {
		stored = 0
		if _, err := tx.Exec(ctx, "insert into tenants (id, name) values ($1, $2) on conflict (id) do nothing", a.Tenant, "default"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "insert into agents (id, tenant_id, name, mode) values ($1, $2, $3, $4) on conflict (id) do nothing", a.ID, a.Tenant, a.Name, a.Mode); err != nil {
			return err
		}
		var seq int64
		var hash []byte
		err := tx.QueryRow(ctx, "select seq, hash from streams where agent_id = $1 for update", a.ID).Scan(&seq, &hash)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		for _, e := range events {
			if e.Seq <= seq {
				continue
			}
			if e.Tenant != a.Tenant || e.Agent != a.ID || !e.Follows(seq, hash) {
				return fmt.Errorf("%w: event %d after %d", ErrChain, e.Seq, seq)
			}
			if !s.hasMonth(e.At) {
				if _, err := tx.Exec(ctx, "select ensure_event_partition($1)", e.At.UTC()); err != nil {
					return err
				}
			}
			_, err := tx.Exec(ctx, `insert into events (id, tenant_id, agent_id, seq, type, at, body, prev_hash, hash)
				values ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, e.ID, e.Tenant, e.Agent, e.Seq, e.Type, e.At, string(e.Body), e.Prev, e.Hash)
			if err != nil {
				return err
			}
			seq, hash = e.Seq, e.Hash
			stored++
		}
		if stored == 0 {
			return nil
		}
		_, err = tx.Exec(ctx, `insert into streams (agent_id, tenant_id, seq, hash, updated_at) values ($1, $2, $3, $4, now())
			on conflict (agent_id) do update set seq = excluded.seq, hash = excluded.hash, updated_at = excluded.updated_at`, a.ID, a.Tenant, seq, hash)
		if err != nil {
			return err
		}
		// Delivered on commit, to whoever streams this agent's events.
		_, err = tx.Exec(ctx, "select pg_notify($1, $2)", eventsChannel, a.ID.String())
		return err
	})
	if err == nil {
		s.mu.Lock()
		for _, e := range events {
			s.months[e.At.UTC().Format("2006-01")] = true
		}
		s.mu.Unlock()
	}
	return stored, err
}

func (s *Store) hasMonth(at time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.months[at.UTC().Format("2006-01")]
}

const eventsChannel = "agent_events"

// Events returns up to limit of the agent's stored events after seq, in
// order: all of them, or only those of the given types.
func (s *Store) Events(ctx context.Context, tenant, agent uuid.UUID, after int64, limit int, types []string) ([]audit.Event, error) {
	return s.events(ctx, tenant, agent, `select id, seq, type, at, body::text, prev_hash, hash from events
		where agent_id = $1 and seq > $2 and ($4::text[] is null or type = any($4)) order by seq limit $3`, after, limit, types)
}

// LastEvents is Events read from the other end: up to limit of the
// agent's stored events between after and before, the last of them, still
// in order.
func (s *Store) LastEvents(ctx context.Context, tenant, agent uuid.UUID, after, before int64, limit int, types []string) ([]audit.Event, error) {
	return s.events(ctx, tenant, agent, `select * from (select id, seq, type, at, body::text, prev_hash, hash from events
		where agent_id = $1 and seq > $2 and seq < $5 and ($4::text[] is null or type = any($4)) order by seq desc limit $3) newest order by seq`,
		after, limit, types, before)
}

// events runs a query for an agent's events; the agent is its $1.
func (s *Store) events(ctx context.Context, tenant, agent uuid.UUID, query string, args ...any) ([]audit.Event, error) {
	var out []audit.Event
	err := s.inTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, append([]any{agent}, args...)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e := audit.Event{Tenant: tenant, Agent: agent}
			var body string
			if err := rows.Scan(&e.ID, &e.Seq, &e.Type, &e.At, &body, &e.Prev, &e.Hash); err != nil {
				return err
			}
			e.At, e.Body = e.At.UTC(), json.RawMessage(body)
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// Listen calls fn with an agent's ID whenever new events of that agent are
// stored, until ctx ends or the connection fails.
func (s *Store) Listen(ctx context.Context, fn func(agent uuid.UUID)) error {
	pc, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	conn := pc.Hijack()
	defer conn.Close(context.WithoutCancel(ctx))
	if _, err := conn.Exec(ctx, "listen "+eventsChannel); err != nil {
		return err
	}
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		if id, err := uuid.Parse(n.Payload); err == nil {
			fn(id)
		}
	}
}

// Ship copies an agent's local audit log to Postgres every interval until
// ctx ends. The local log stays the record of truth; after an outage the
// database catches up from where it stopped.
func Ship(ctx context.Context, s *Store, dir string, a func() (Agent, bool), every time.Duration, logf func(string, ...any)) {
	for ctx.Err() == nil {
		wait := every
		if agent, ok := a(); ok {
			n, err := s.shipOnce(ctx, dir, agent)
			switch {
			case err != nil && ctx.Err() == nil:
				logf("database: %v", err)
				wait = time.Minute
			case n == batch:
				wait = 0
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
	}
	// One last pass, so the events of a clean stop are stored too.
	if agent, ok := a(); ok {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := s.shipOnce(fctx, dir, agent); err != nil {
			logf("database: %v", err)
		}
	}
}

const batch = 500

func (s *Store) shipOnce(ctx context.Context, dir string, a Agent) (int, error) {
	seq, hash, err := s.Head(ctx, a.Tenant, a.ID)
	if err != nil {
		return 0, err
	}
	events, err := audit.ReadAfter(dir, seq, batch)
	if err != nil || len(events) == 0 {
		return 0, err
	}
	if seq > 0 && !bytes.Equal(events[0].Prev, hash) {
		return 0, fmt.Errorf("%w: the local log disagrees with the database at event %d", ErrChain, seq)
	}
	return s.Store(ctx, a, events)
}
