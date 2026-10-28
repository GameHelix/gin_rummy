package fleet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/candylario/trader/internal/agent"
	"github.com/candylario/trader/internal/pg"
)

// Supervisor keeps every runnable agent running: it starts new ones, stops
// those that were stopped, and restarts an agent on a new config version or
// a change of keys. An agent that fails is retried a minute later, and
// after longer pauses while it keeps failing; a panic ends only that
// agent's run. It knows which agents should run but do not, for the health
// check.
type Supervisor struct {
	Service *Service
	// Build makes an engine for an agent from its config and keys.
	Build func(Spec) (*agent.Engine, error)
	// Ship, when set, runs alongside each agent, copying its audit log.
	Ship  func(ctx context.Context, spec Spec)
	Every time.Duration
	Logf  func(format string, args ...any)
	// Now is the clock; time.Now unless set.
	Now func() time.Time

	mu      sync.Mutex
	engines map[uuid.UUID]*agent.Engine
	failing map[uuid.UUID]*Failure
}

// Failure is an agent that should run but does not: its last error, since
// when it fails, and how many runs in a row have failed.
type Failure struct {
	Tenant uuid.UUID
	Agent  uuid.UUID
	Since  time.Time
	Error  string
	Runs   int
}

func (s *Supervisor) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
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
func (s *Supervisor) Command(ctx context.Context, tenant, id uuid.UUID, name, via string) (agent.Reply, error) {
	e := s.engine(tenant, id)
	if e == nil {
		return agent.Reply{}, agent.ErrNotRunning
	}
	return e.Command(ctx, name, via)
}

// Status reports a running agent's account as its loop sees it.
func (s *Supervisor) Status(ctx context.Context, tenant, id uuid.UUID) (agent.Report, error) {
	e := s.engine(tenant, id)
	if e == nil {
		return agent.Report{}, agent.ErrNotRunning
	}
	return e.Status(ctx)
}

// Down lists the agents that should be running but have failed for at
// least minAge, the longest failing first.
func (s *Supervisor) Down(minAge time.Duration) []Failure {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Failure
	for _, f := range s.failing {
		if s.now().Sub(f.Since) >= minAge {
			out = append(out, *f)
		}
	}
	slices.SortFunc(out, func(a, b Failure) int { return a.Since.Compare(b.Since) })
	return out
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
	cancel context.CancelCauseFunc
	done   chan struct{}
	// err is why the run ended, when it failed; set before done closes.
	err error
}

// maxRetryWait caps the pause between runs of an agent that keeps failing.
const maxRetryWait = 15 * time.Minute

func (s *Supervisor) Run(ctx context.Context) error {
	agents := map[uuid.UUID]*running{}
	retryAt := map[uuid.UUID]time.Time{}
	stop := func(id uuid.UUID, cause error) {
		agents[id].cancel(cause)
		<-agents[id].done
		delete(agents, id)
	}
	defer func() {
		for id := range agents {
			stop(id, agent.ErrEngineStopping)
		}
	}()
	for {
		for id, run := range agents {
			select {
			case <-run.done:
				delete(agents, id)
				retryAt[id] = s.now().Add(s.ended(run))
			default:
				s.check(id)
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
				if _, wanted := want[id]; !wanted {
					// The user stopped it, a key was revoked, or the plan
					// or the account changed: an announced stop.
					stop(id, nil)
				} else if want[id] != run.want {
					stop(id, agent.ErrRestarting)
				}
			}
			for _, r := range list {
				if _, ok := agents[r.Agent]; ok || s.now().Before(retryAt[r.Agent]) {
					continue
				}
				agents[r.Agent] = s.start(ctx, r)
			}
			s.forget(want)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(s.Every):
		}
	}
}

// ended notes how a run ended and returns the pause before the next: a
// minute, doubling while runs keep failing, up to maxRetryWait.
func (s *Supervisor) ended(run *running) time.Duration {
	if run.err == nil {
		return time.Minute
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing == nil {
		s.failing = map[uuid.UUID]*Failure{}
	}
	f := s.failing[run.want.Agent]
	if f == nil {
		f = &Failure{Tenant: run.want.Tenant, Agent: run.want.Agent, Since: s.now()}
		s.failing[run.want.Agent] = f
	}
	f.Runs, f.Error = f.Runs+1, run.err.Error()
	wait := time.Minute
	for i := 1; i < f.Runs && wait < maxRetryWait; i++ {
		wait *= 2
	}
	return min(wait, maxRetryWait)
}

// check clears an agent's failure once its run has opened the account and
// entered its loop.
func (s *Supervisor) check(id uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.engines[id]; e != nil && s.failing[id] != nil {
		select {
		case <-e.Started():
			delete(s.failing, id)
		default:
		}
	}
}

// forget drops the failures of agents that should no longer run.
func (s *Supervisor) forget(want map[uuid.UUID]pg.Runnable) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.failing {
		if _, ok := want[id]; !ok {
			delete(s.failing, id)
		}
	}
}

func (s *Supervisor) start(ctx context.Context, r pg.Runnable) *running {
	// The run ends only through its own cancel, so that a shutdown reaches
	// the agent with its cause rather than the parent's plain one.
	actx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	run := &running{want: r, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(run.done)
		err := s.guard("agent "+r.Agent.String(), func() error { return s.runAgent(actx, r.Tenant, r.Agent) })
		if err != nil && actx.Err() == nil {
			s.Logf("agent %s stopped: %v", r.Agent, err)
			run.err = err
		}
	}()
	return run
}

// guard runs fn and turns a panic into an error, with the stack in the
// log, so one agent's crash ends only its own run.
func (s *Supervisor) guard(what string, fn func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
			s.Logf("%s panicked: %v\n%s", what, p, debug.Stack())
		}
	}()
	return fn()
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
		go func() {
			defer wg.Done()
			// A shipper that panics starts again a minute later; the
			// agent trades on meanwhile, and its log on disk waits.
			for {
				err := s.guard("event shipper of agent "+id.String(), func() error { s.Ship(shipCtx, spec); return nil })
				if err == nil || shipCtx.Err() != nil {
					return
				}
				select {
				case <-shipCtx.Done():
					return
				case <-time.After(time.Minute):
				}
			}
		}()
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
