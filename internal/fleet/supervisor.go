package fleet

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ismatsamadov/trader/internal/agent"
	"github.com/ismatsamadov/trader/internal/pg"
)

// Supervisor keeps every runnable agent running: it starts new ones, stops
// those that were stopped, and restarts an agent on a new config version or
// a change of keys. An agent that fails is retried a minute later.
type Supervisor struct {
	Service *Service
	// Build makes an engine for an agent from its config and keys.
	Build func(Spec) (*agent.Engine, error)
	// Ship, when set, runs alongside each agent, copying its audit log.
	Ship  func(ctx context.Context, spec Spec)
	Every time.Duration
	Logf  func(format string, args ...any)

	mu      sync.Mutex
	engines map[uuid.UUID]*agent.Engine
}

// engine returns the tenant's agent if it runs here now.
func (s *Supervisor) engine(tenant, id uuid.UUID) *agent.Engine {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.engines[id]; e != nil && e.TenantID == tenant {
		return e
	}
	return nil
}

// Running reports whether the agent runs in this engine now.
func (s *Supervisor) Running(tenant, id uuid.UUID) bool { return s.engine(tenant, id) != nil }

// Command sends a user command to a running agent and returns its answer.
func (s *Supervisor) Command(ctx context.Context, tenant, id uuid.UUID, name string) (agent.Reply, error) {
	e := s.engine(tenant, id)
	if e == nil {
		return agent.Reply{}, agent.ErrNotRunning
	}
	return e.Command(ctx, name)
}

// Status reports a running agent's account as its loop sees it.
func (s *Supervisor) Status(ctx context.Context, tenant, id uuid.UUID) (agent.Report, error) {
	e := s.engine(tenant, id)
	if e == nil {
		return agent.Report{}, agent.ErrNotRunning
	}
	return e.Status(ctx)
}

func (s *Supervisor) register(id uuid.UUID, e *agent.Engine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.engines == nil {
		s.engines = map[uuid.UUID]*agent.Engine{}
	}
	if e == nil {
		delete(s.engines, id)
		return
	}
	s.engines[id] = e
}

type running struct {
	want   pg.Runnable
	cancel context.CancelFunc
	done   chan struct{}
}

func (s *Supervisor) Run(ctx context.Context) error {
	agents := map[uuid.UUID]*running{}
	retryAt := map[uuid.UUID]time.Time{}
	stop := func(id uuid.UUID) {
		agents[id].cancel()
		<-agents[id].done
		delete(agents, id)
	}
	defer func() {
		for id := range agents {
			stop(id)
		}
	}()
	for {
		for id, run := range agents {
			select {
			case <-run.done:
				delete(agents, id)
				retryAt[id] = time.Now().Add(time.Minute)
			default:
			}
		}
		list, err := s.Service.Store.Runnable(ctx)
		if err != nil && ctx.Err() == nil {
			s.Logf("fleet: %v", err)
		}
		if err == nil {
			want := map[uuid.UUID]pg.Runnable{}
			for _, r := range list {
				want[r.Agent] = r
			}
			for id, run := range agents {
				if want[id] != run.want {
					stop(id)
				}
			}
			for _, r := range list {
				if _, ok := agents[r.Agent]; ok || time.Now().Before(retryAt[r.Agent]) {
					continue
				}
				agents[r.Agent] = s.start(ctx, r)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(s.Every):
		}
	}
}

func (s *Supervisor) start(ctx context.Context, r pg.Runnable) *running {
	actx, cancel := context.WithCancel(ctx)
	run := &running{want: r, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(run.done)
		if err := s.runAgent(actx, r.Tenant, r.Agent); err != nil && actx.Err() == nil {
			s.Logf("agent %s stopped: %v", r.Agent, err)
		}
	}()
	return run
}

func (s *Supervisor) runAgent(ctx context.Context, tenant, id uuid.UUID) error {
	spec, err := s.Service.Spec(ctx, tenant, id)
	if err != nil {
		return err
	}
	eng, err := s.Build(spec)
	if err != nil {
		return err
	}
	eng.TenantID, eng.AgentID = spec.Tenant, spec.Agent
	if _, err := os.Stat(agent.StatePath(spec.Config)); errors.Is(err, os.ErrNotExist) {
		if _, err := eng.Init(ctx); err != nil {
			return err
		}
	}
	s.register(id, eng)
	defer s.register(id, nil)
	var wg sync.WaitGroup
	shipCtx, stopShip := context.WithCancel(ctx)
	if s.Ship != nil {
		wg.Add(1)
		go func() { defer wg.Done(); s.Ship(shipCtx, spec) }()
	}
	err = eng.Run(ctx)
	stopShip()
	wg.Wait()
	return err
}

// Prefixed writes each line to w after prefix, so many agents can share one
// log.
func Prefixed(w io.Writer, prefix string) io.Writer {
	if w == nil {
		return nil
	}
	return writerFunc(func(p []byte) (int, error) {
		_, err := w.Write(append([]byte(prefix), p...))
		return len(p), err
	})
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
