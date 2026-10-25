package trader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const stateVersion = "agent-state-v1"

type EquityPoint struct {
	Session         string  `json:"session"`
	AgentEquity     Money   `json:"agent_equity_cents"`
	AgentCash       Money   `json:"agent_cash_cents"`
	AgentInvested   Money   `json:"agent_invested_cents"`
	BenchmarkEquity Money   `json:"benchmark_equity_cents"`
	MarketClose     float64 `json:"market_close,omitempty"`
}

type Decision struct {
	Session   string            `json:"session"`
	DecidedAt string            `json:"decided_at"`
	Summary   string            `json:"summary"`
	Signals   map[string]Signal `json:"signals"`
	Plan      []PlanLine        `json:"plan"`
	CostUSD   float64           `json:"estimated_cost_usd"`
	Abstained bool              `json:"abstained"`
	Note      string            `json:"note,omitempty"`
}

// State is the paper account's single source of truth. It is rewritten
// atomically once per cycle; per-cycle details live under cycles/<session>/.
type State struct {
	Version          string           `json:"schema_version"`
	Name             string           `json:"name"`
	Symbols          []string         `json:"symbols"`
	CreatedAt        string           `json:"created_at"`
	InitialCash      Money            `json:"initial_cash_cents"`
	LastSession      string           `json:"last_session"`
	Agent            Account          `json:"agent"`
	Benchmark        Account          `json:"benchmark"`
	BenchmarkStarted bool             `json:"benchmark_started"`
	Pending          []Order          `json:"pending_orders"`
	BenchmarkPending []Order          `json:"benchmark_pending_orders"`
	LastCloses       map[string]Money `json:"last_closes_cents"`
	History          []EquityPoint    `json:"history"`
	OpenSignals      []OpenSignal     `json:"open_signals"`
	Scored           []ScoredSignal   `json:"scored_signals"`
	LastDecision     *Decision        `json:"last_decision,omitempty"`
	MissedDecisions  []string         `json:"missed_decisions,omitempty"`
}

func StatePath(c Config) string { return filepath.Join(c.DataDir, "state.json") }

func InitState(c Config, now time.Time) (State, error) {
	path := StatePath(c)
	if _, err := os.Stat(path); err == nil {
		return State{}, fmt.Errorf("%s already exists; the paper account is already initialised", path)
	}
	cash := Cents(c.InitialCash)
	st := State{Version: stateVersion, Name: c.Name, Symbols: c.Symbols, CreatedAt: now.UTC().Format(time.RFC3339), InitialCash: cash, Agent: NewAccount(cash), Benchmark: NewAccount(cash), LastCloses: map[string]Money{}}
	return st, WriteJSON(path, st)
}

func LoadState(c Config) (State, error) {
	var st State
	if err := ReadJSON(StatePath(c), &st); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st, fmt.Errorf("no paper account at %s; run `trader init` first", StatePath(c))
		}
		return st, err
	}
	if st.Version != stateVersion {
		return st, fmt.Errorf("unsupported state version %q", st.Version)
	}
	if st.Name != c.Name || !slices.Equal(st.Symbols, c.Symbols) {
		return st, errors.New("config name or symbols differ from the initialised account; use a new data_dir for a different universe")
	}
	for _, acct := range []*Account{&st.Agent, &st.Benchmark} {
		if acct.Positions == nil {
			acct.Positions = map[string]int64{}
		}
	}
	if st.LastCloses == nil {
		st.LastCloses = map[string]Money{}
	}
	return st, nil
}

// Agent runs one decision cycle per exchange session against live data.
type Agent struct {
	Config  Config
	Market  MarketSource
	Analyst *Analyst
	Now     func() time.Time
	Sleep   func(context.Context, time.Duration) error
	Log     io.Writer
}

const (
	StatusUpToDate = "up_to_date"
	StatusDecided  = "decided"
	StatusMissed   = "missed_decision"
	StatusDryRun   = "dry_run"
)

type CycleReport struct {
	Status          string         `json:"status"`
	Session         string         `json:"session"`
	Processed       []string       `json:"processed_sessions,omitempty"`
	Fills           []Fill         `json:"fills,omitempty"`
	BenchmarkFills  []Fill         `json:"benchmark_fills,omitempty"`
	Rejections      []Rejection    `json:"rejections,omitempty"`
	CorporateEvents []string       `json:"corporate_events,omitempty"`
	Scored          []ScoredSignal `json:"scored_signals,omitempty"`
	Decision        *Decision      `json:"decision,omitempty"`
	Orders          []Order        `json:"orders,omitempty"`
	Equity          *EquityPoint   `json:"equity,omitempty"`
	Notes           []string       `json:"notes,omitempty"`
	Briefing        *Briefing      `json:"briefing,omitempty"`
}

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Agent) logf(format string, args ...any) {
	if a.Log != nil {
		fmt.Fprintf(a.Log, format+"\n", args...)
	}
}

// RunCycle brings the account up to the latest closed session: corporate
// actions, fills at each missed open, marks at each close, then one new
// decision if orders can still reach the next open. Running it again for the
// same session is a no-op. A dry run fetches live data and builds the briefing
// without calling the model or saving anything.
func (a *Agent) RunCycle(ctx context.Context, dryRun bool) (CycleReport, error) {
	c := a.Config
	lockCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	unlock, err := lockFile(lockCtx, filepath.Join(c.DataDir, "agent.lock"))
	cancel()
	if err != nil {
		return CycleReport{}, fmt.Errorf("another trader process is using %s: %w", c.DataDir, err)
	}
	defer unlock()
	st, err := LoadState(c)
	if err != nil {
		return CycleReport{}, &PermanentError{err}
	}
	now := a.now()
	session, err := LatestClosedSession(now, c.CloseBuffer())
	if err != nil {
		return CycleReport{}, &PermanentError{err}
	}
	rep := CycleReport{Session: session.Date}
	if st.LastSession != "" && session.Date <= st.LastSession {
		rep.Status = StatusUpToDate
		return rep, nil
	}
	sessions := []Session{session}
	if st.LastSession != "" {
		if sessions, err = SessionsAfter(st.LastSession, session.Date); err != nil {
			return rep, err
		}
	}
	dir := filepath.Join(c.DataDir, "cycles", session.Date)
	series, err := a.loadMarket(ctx, dir, sessions, dryRun)
	if err != nil {
		return rep, err
	}
	for i, s := range sessions {
		if err := a.processSession(&st, &rep, s, series); err != nil {
			return rep, err
		}
		if i < len(sessions)-1 {
			st.MissedDecisions = append(st.MissedDecisions, s.Date)
			rep.Notes = append(rep.Notes, "no decision was made after "+s.Date+" (agent was not running)")
		}
	}
	if !st.BenchmarkStarted && !dryRun {
		if st.BenchmarkPending, err = BenchmarkOrders(c, session.Date, st.Benchmark, st.LastCloses); err != nil {
			return rep, err
		}
		st.BenchmarkStarted = true
	}
	next, err := NextSessionAfter(session.Date)
	if err != nil {
		return rep, &PermanentError{err}
	}
	if deadline := next.OpenTime().Add(-5 * time.Minute); now.After(deadline) {
		rep.Status = StatusMissed
		rep.Notes = append(rep.Notes, "too late to place orders for the "+next.Date+" open; holding positions")
		st.MissedDecisions = append(st.MissedDecisions, session.Date)
	} else {
		briefing, bars, err := a.briefing(session, now, series)
		if err != nil {
			return rep, err
		}
		if dryRun {
			rep.Status, rep.Briefing = StatusDryRun, &briefing
			return rep, nil
		}
		if a.Analyst == nil {
			return rep, &PermanentError{errors.New("no analyst configured")}
		}
		result, err := a.Analyst.Analyze(ctx, AnalystInput{Briefing: briefing, Bars: bars, Cutoff: now, Dir: dir})
		switch {
		case errors.Is(err, ErrBudgetExceeded):
			rep.Status = StatusMissed
			rep.Notes = append(rep.Notes, err.Error()+"; holding positions")
			st.MissedDecisions = append(st.MissedDecisions, session.Date)
		case err != nil:
			return rep, err
		default:
			orders, plan, err := PlanOrders(c, session.Date, result.Signals, st.Agent, st.LastCloses)
			if err != nil {
				return rep, err
			}
			st.Pending = orders
			if !result.Abstained {
				open, err := NewOpenSignals(c, session.Date, result.Signals)
				if err != nil {
					return rep, err
				}
				st.OpenSignals = append(st.OpenSignals, open...)
			}
			st.LastDecision = &Decision{Session: session.Date, DecidedAt: now.UTC().Format(time.RFC3339), Summary: result.Summary, Signals: result.Signals, Plan: plan, CostUSD: result.CostUSD, Abstained: result.Abstained, Note: result.Reason}
			rep.Status, rep.Decision, rep.Orders = StatusDecided, st.LastDecision, orders
		}
	}
	if err := WriteJSON(filepath.Join(dir, "cycle.json"), rep); err != nil {
		return rep, err
	}
	return rep, WriteJSON(StatePath(c), st)
}

func (a *Agent) processSession(st *State, rep *CycleReport, s Session, series map[string]Series) error {
	c := a.Config
	opens, closes := map[string]float64{}, map[string]Money{}
	for _, sym := range c.Symbols {
		bar, _ := series[sym].BarOn(s.Date)
		opens[sym], closes[sym] = bar.Open, Cents(bar.Close)
	}
	// Corporate actions take effect at the open, before any order fills.
	for _, sym := range c.Symbols {
		if sp, ok := series[sym].SplitOn(s.Date); ok {
			for _, acct := range []*Account{&st.Agent, &st.Benchmark} {
				if _, err := acct.ApplySplit(sym, sp, Cents(opens[sym])); err != nil {
					return err
				}
			}
			var err error
			if st.Pending, err = AdjustOrdersForSplit(st.Pending, sym, sp); err != nil {
				return err
			}
			if st.BenchmarkPending, err = AdjustOrdersForSplit(st.BenchmarkPending, sym, sp); err != nil {
				return err
			}
			rep.CorporateEvents = append(rep.CorporateEvents, fmt.Sprintf("%s %s split %g-for-%g", s.Date, sym, sp.Numerator, sp.Denominator))
		}
		if amount := series[sym].DividendOn(s.Date); amount > 0 {
			for _, acct := range []*Account{&st.Agent, &st.Benchmark} {
				if _, err := acct.ApplyDividend(sym, amount); err != nil {
					return err
				}
			}
			rep.CorporateEvents = append(rep.CorporateEvents, fmt.Sprintf("%s %s dividend $%.4f per share", s.Date, sym, amount))
		}
	}
	fills, rejections, err := ExecuteAtOpen(c, s.Date, st.Pending, opens, &st.Agent)
	if err != nil {
		return err
	}
	benchFills, _, err := ExecuteAtOpen(c, s.Date, st.BenchmarkPending, opens, &st.Benchmark)
	if err != nil {
		return err
	}
	st.Pending, st.BenchmarkPending = nil, nil
	rep.Fills = append(rep.Fills, fills...)
	rep.BenchmarkFills = append(rep.BenchmarkFills, benchFills...)
	rep.Rejections = append(rep.Rejections, rejections...)

	invested, err := st.Agent.Invested(closes)
	if err != nil {
		return err
	}
	equity, err := st.Agent.Equity(closes)
	if err != nil {
		return err
	}
	bench, err := st.Benchmark.Equity(closes)
	if err != nil {
		return err
	}
	point := EquityPoint{Session: s.Date, AgentEquity: equity, AgentCash: st.Agent.Cash, AgentInvested: invested, BenchmarkEquity: bench}
	if c.MarketSymbol != "" {
		if bar, ok := series[c.MarketSymbol].BarOn(s.Date); ok {
			point.MarketClose = bar.Close
		}
	}
	st.History = append(st.History, point)
	rep.Equity = &point
	st.LastCloses = closes
	keep, scored, err := ScoreThrough(c, st.OpenSignals, s.Date, series)
	if err != nil {
		return err
	}
	st.OpenSignals = keep
	st.Scored = append(st.Scored, scored...)
	rep.Scored = append(rep.Scored, scored...)
	st.LastSession = s.Date
	rep.Processed = append(rep.Processed, s.Date)
	return nil
}

// loadMarket reuses the cycle's archived payloads so a retried cycle sees the
// same prices; otherwise it downloads fresh data and archives it once every
// required bar is present.
func (a *Agent) loadMarket(ctx context.Context, dir string, sessions []Session, dryRun bool) (map[string]Series, error) {
	symbols := a.Config.AllSymbols()
	series := map[string]Series{}
	archived := true
	for _, sym := range symbols {
		raw, err := os.ReadFile(filepath.Join(dir, "market", sym+".json"))
		if errors.Is(err, os.ErrNotExist) {
			archived = false
			break
		}
		if err != nil {
			return nil, err
		}
		if series[sym], err = ParseYahoo(raw, sym); err != nil {
			return nil, err
		}
	}
	if !archived {
		raws := map[string][]byte{}
		for _, sym := range symbols {
			raw, err := a.Market.Fetch(ctx, sym)
			if err != nil {
				return nil, err
			}
			if series[sym], err = ParseYahoo(raw, sym); err != nil {
				return nil, err
			}
			raws[sym] = raw
		}
		if err := RequireBars(series, symbols, sessions); err != nil {
			return nil, err
		}
		if !dryRun {
			for sym, raw := range raws {
				if err := AtomicWrite(filepath.Join(dir, "market", sym+".json"), raw); err != nil {
					return nil, err
				}
			}
		}
	}
	return series, RequireBars(series, symbols, sessions)
}

func (a *Agent) briefing(session Session, now time.Time, series map[string]Series) (Briefing, map[string][]Bar, error) {
	c := a.Config
	bars := map[string][]Bar{}
	for _, sym := range c.AllSymbols() {
		bars[sym] = series[sym].BarsThrough(session.Date)
	}
	b := Briefing{Session: session.Date, DecisionTime: now.In(newYork).Format(time.RFC3339), HorizonSessions: c.HorizonSessions}
	for _, sym := range c.Symbols {
		ind, err := ComputeIndicators(sym, priceEvidenceID(c, sym), bars[sym])
		if err != nil {
			return b, nil, err
		}
		b.Universe = append(b.Universe, ind)
	}
	if c.MarketSymbol != "" {
		ind, err := ComputeIndicators(c.MarketSymbol, priceEvidenceID(c, c.MarketSymbol), bars[c.MarketSymbol])
		if err != nil {
			return b, nil, err
		}
		b.Market = &ind
	}
	return b, bars, nil
}

// Daemon runs a cycle after every session close until ctx is cancelled.
// Transient failures (data not yet published, provider outages) are retried
// every ten minutes. Failures that retrying cannot fix (for example an
// exhausted credit balance) are logged and retried at the next close, so a
// long-running service recovers once the cause is fixed instead of exiting.
func (a *Agent) Daemon(ctx context.Context) error {
	sleep := a.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	for {
		rep, err := a.RunCycle(ctx, false)
		var wake time.Time
		var perm *PermanentError
		switch {
		case err != nil && ctx.Err() != nil:
			return err
		case errors.As(err, &perm):
			next, nerr := NextCycleTime(a.now(), a.Config.CloseBuffer())
			if nerr != nil {
				return err // calendar exhausted: there is no next close to wait for
			}
			wake = next
			a.logf("%s cycle failed and needs attention (next attempt %s): %v", a.now().Format(time.RFC3339), wake.In(newYork).Format("Mon Jan 2 15:04 MST"), err)
		case err != nil:
			wake = a.now().Add(10 * time.Minute)
			a.logf("%s cycle failed, retrying at %s: %v", a.now().Format(time.RFC3339), wake.In(newYork).Format("Jan 2 15:04 MST"), err)
		default:
			a.logf("%s cycle for %s: %s", a.now().Format(time.RFC3339), rep.Session, rep.Status)
			if wake, err = NextCycleTime(a.now(), a.Config.CloseBuffer()); err != nil {
				return &PermanentError{err}
			}
			a.logf("next cycle at %s", wake.In(newYork).Format("Mon Jan 2 15:04 MST"))
		}
		if err := a.waitUntil(ctx, sleep, wake); err != nil {
			return err
		}
	}
}

// waitUntil naps in short steps and re-reads the wall clock after each one.
// One long timer would count only awake time, so a laptop that sleeps (or a
// paused Docker VM) could wake the daemon hours late and miss decisions.
func (a *Agent) waitUntil(ctx context.Context, sleep func(context.Context, time.Duration) error, wake time.Time) error {
	for {
		left := wake.Sub(a.now())
		if left <= 0 {
			return nil
		}
		if err := sleep(ctx, min(left, time.Minute)); err != nil {
			return err
		}
	}
}
