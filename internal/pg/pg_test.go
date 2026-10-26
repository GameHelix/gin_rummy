package pg

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/candylario/trader/internal/audit"
	"github.com/candylario/trader/internal/testkit"
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

func chain(t *testing.T, dir string, a Agent, n int) []audit.Event {
	t.Helper()
	l, err := audit.Open(dir, a.Tenant, a.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var out []audit.Event
	for i := 0; i < n; i++ {
		e, err := l.Append("decision.started", map[string]any{"n": i, "symbol": "BTCUSDT"})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func TestShipsTheChainOnceAndRejectsForks(t *testing.T) {
	url := testDB(t)
	ctx := context.Background()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := Agent{Tenant: uuid.New(), ID: uuid.New(), Name: "btc-eth", Mode: "testnet"}
	dir := t.TempDir()
	events := chain(t, dir, a, 5)
	if n, err := s.shipOnce(ctx, dir, a); err != nil || n != 5 {
		t.Fatalf("ship: %d %v", n, err)
	}
	if n, err := s.shipOnce(ctx, dir, a); err != nil || n != 0 {
		t.Fatalf("a second pass must store nothing: %d %v", n, err)
	}
	seq, hash, err := s.Head(ctx, a.Tenant, a.ID)
	if err != nil || seq != 5 || string(hash) != string(events[4].Hash) {
		t.Fatalf("head %d %v", seq, err)
	}
	forged := events[4]
	forged.Seq, forged.Prev, forged.Body = 6, events[3].Hash, []byte(`{"n":99}`)
	forged.Hash = forged.Sum()
	if _, err := s.Store(ctx, a, []audit.Event{forged}); !errors.Is(err, ErrChain) {
		t.Fatalf("a fork must be refused: %v", err)
	}
	edited := events[4]
	edited.Seq, edited.Prev = 6, events[4].Hash
	if _, err := s.Store(ctx, a, []audit.Event{edited}); !errors.Is(err, ErrChain) {
		t.Fatalf("an event whose hash does not match must be refused: %v", err)
	}
}

func TestEventsAreAppendOnlyAndTenantsIsolated(t *testing.T) {
	url := testDB(t)
	ctx := context.Background()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := Agent{Tenant: uuid.New(), ID: uuid.New(), Name: "a", Mode: "testnet"}
	b := Agent{Tenant: uuid.New(), ID: uuid.New(), Name: "b", Mode: "live"}
	for _, ag := range []Agent{a, b} {
		dir := t.TempDir()
		chain(t, dir, ag, 2)
		if _, err := s.shipOnce(ctx, dir, ag); err != nil {
			t.Fatal(err)
		}
	}
	count := func(tenant uuid.UUID) int {
		var n int
		err := s.inTenant(ctx, tenant, func(tx pgx.Tx) error { return tx.QueryRow(ctx, "select count(*) from events").Scan(&n) })
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count(a.Tenant) != 2 || count(b.Tenant) != 2 {
		t.Fatal("each tenant must see exactly its own events")
	}
	var none int
	pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error { return tx.QueryRow(ctx, "select count(*) from events").Scan(&none) })
	if none != 0 {
		t.Fatal("without a tenant no events are visible")
	}
	for _, sql := range []string{"update events set type = 'x'", "delete from events", "truncate events"} {
		err := s.inTenant(ctx, a.Tenant, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, sql); return err })
		if err == nil {
			t.Fatalf("%q must fail", sql)
		}
	}
	// Even the schema owner cannot rewrite history.
	owner, _ := pgx.Connect(ctx, url)
	defer owner.Close(ctx)
	if _, err := owner.Exec(ctx, "update events set type = 'x'"); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("the owner must not edit events: %v", err)
	}
}

func TestEventsReadBackInOrderAndAnnounceThemselves(t *testing.T) {
	url := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := Agent{Tenant: uuid.New(), ID: uuid.New(), Name: "btc-eth", Mode: "testnet"}
	woke := make(chan uuid.UUID, 4)
	listening := make(chan error, 1)
	go func() { listening <- s.Listen(ctx, func(id uuid.UUID) { woke <- id }) }()
	dir := t.TempDir()
	written := chain(t, dir, a, 5)
	// The listener may not be subscribed yet; ship until it hears one.
	for deadline := time.Now().Add(5 * time.Second); ; {
		if _, err := s.Store(ctx, a, written); err != nil {
			t.Fatal(err)
		}
		select {
		case id := <-woke:
			if id != a.ID {
				t.Fatalf("woke for %s", id)
			}
		case <-time.After(200 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("no notification")
			}
			written = append(written, chain(t, dir, a, 1)...)
			continue
		}
		break
	}
	got, err := s.Events(ctx, a.Tenant, a.ID, 2, 2, nil)
	if err != nil || len(got) != 2 || got[0].Seq != 3 || got[1].Seq != 4 {
		t.Fatalf("events after 2: %+v %v", got, err)
	}
	if string(got[0].Body) != string(written[2].Body) || got[0].Sum() == nil || string(got[0].Sum()) != string(got[0].Hash) {
		t.Fatalf("an event must read back exactly as hashed: %s", got[0].Body)
	}
	if other, _ := s.Events(ctx, uuid.New(), a.ID, 0, 10, nil); len(other) != 0 {
		t.Fatal("another tenant must not read the events")
	}
	cancel()
	if err := <-listening; err == nil {
		t.Fatal("Listen returns when its context ends")
	}
}

func TestShipperCatchesUpInTheBackground(t *testing.T) {
	url := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := Agent{Tenant: uuid.New(), ID: uuid.New(), Name: "btc-eth", Mode: "testnet"}
	dir := t.TempDir()
	chain(t, dir, a, 3)
	go Ship(ctx, s, dir, func() (Agent, bool) { return a, true }, 10*time.Millisecond, t.Logf)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if seq, _, _ := s.Head(ctx, a.Tenant, a.ID); seq == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the shipper never caught up")
		}
	}
}
