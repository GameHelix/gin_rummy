package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/ismatsamadov/trader/internal/analyst"
	"github.com/ismatsamadov/trader/internal/broker"
	"github.com/ismatsamadov/trader/internal/config"
	"github.com/ismatsamadov/trader/internal/market"
	"github.com/ismatsamadov/trader/internal/store"
)

const stateVersion = "agent-state-v1"

type EquityPoint struct {
	Session         string       `json:"session"`
	AgentEquity     broker.Money `json:"agent_equity_cents"`
	AgentCash       broker.Money `json:"agent_cash_cents"`
	AgentInvested   broker.Money `json:"agent_invested_cents"`
	BenchmarkEquity broker.Money `json:"benchmark_equity_cents"`
	MarketClose     float64      `json:"market_close,omitempty"`
}

type Decision struct {
	Session   string                    `json:"session"`
	DecidedAt string                    `json:"decided_at"`
	Summary   string                    `json:"summary"`
	Signals   map[string]analyst.Signal `json:"signals"`
	Plan      []PlanLine                `json:"plan"`
	CostUSD   float64                   `json:"estimated_cost_usd"`
	Abstained bool                      `json:"abstained"`
	Note      string                    `json:"note,omitempty"`
}

// State is the paper account's single source of truth. It is rewritten
// atomically once per cycle; per-cycle details live under cycles/<session>/.
type State struct {
	Version          string                  `json:"schema_version"`
	Name             string                  `json:"name"`
	Symbols          []string                `json:"symbols"`
	CreatedAt        string                  `json:"created_at"`
	InitialCash      broker.Money            `json:"initial_cash_cents"`
	LastSession      string                  `json:"last_session"`
	Agent            broker.Account          `json:"agent"`
	Benchmark        broker.Account          `json:"benchmark"`
	BenchmarkStarted bool                    `json:"benchmark_started"`
	Pending          []broker.Order          `json:"pending_orders"`
	BenchmarkPending []broker.Order          `json:"benchmark_pending_orders"`
	LastCloses       map[string]broker.Money `json:"last_closes_cents"`
	History          []EquityPoint           `json:"history"`
	OpenSignals      []OpenSignal            `json:"open_signals"`
	Scored           []ScoredSignal          `json:"scored_signals"`
	LastDecision     *Decision               `json:"last_decision,omitempty"`
	MissedDecisions  []string                `json:"missed_decisions,omitempty"`
	// DecisionPending is a session whose accounting is saved but whose
	// decision is not yet made or marked missed.
	DecisionPending string `json:"decision_pending,omitempty"`
}

func StatePath(c config.Config) string { return filepath.Join(c.DataDir, "state.json") }

func InitState(c config.Config, now time.Time) (State, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	unlock, err := store.LockFile(ctx, filepath.Join(c.DataDir, "agent.lock"))
	if err != nil {
		return State{}, fmt.Errorf("another trader process is using %s: %w", c.DataDir, err)
	}
	defer unlock()
	path := StatePath(c)
	switch _, err := os.Stat(path); {
	case err == nil:
		return State{}, fmt.Errorf("%s already exists; the paper account is already initialised", path)
	case !errors.Is(err, os.ErrNotExist):
		return State{}, err
	}
	cash := broker.Cents(c.InitialCash)
	st := State{Version: stateVersion, Name: c.Name, Symbols: c.Symbols, CreatedAt: now.UTC().Format(time.RFC3339), InitialCash: cash, Agent: broker.NewAccount(cash), Benchmark: broker.NewAccount(cash), LastCloses: map[string]broker.Money{}}
	return st, store.WriteJSON(path, st)
}

func LoadState(c config.Config) (State, error) {
	var st State
	if err := store.ReadJSON(StatePath(c), &st); err != nil {
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
	for _, acct := range []*broker.Account{&st.Agent, &st.Benchmark} {
		if acct.Positions == nil {
			acct.Positions = map[string]int64{}
		}
	}
	if st.LastCloses == nil {
		st.LastCloses = map[string]broker.Money{}
	}
	if err := st.validate(); err != nil {
		return st, fmt.Errorf("%s: %w", StatePath(c), err)
	}
	return st, nil
}

// validate rejects state that would value or trade incorrectly.
func (st State) validate() error {
	for name, acct := range map[string]broker.Account{"agent": st.Agent, "benchmark": st.Benchmark} {
		if acct.Cash < 0 || acct.Receivable < 0 {
			return fmt.Errorf("%s account has negative cash or receivables", name)
		}
		for sym, qty := range acct.Positions {
			if qty < 0 {
				return fmt.Errorf("%s account holds negative %s", name, sym)
			}
			if qty > 0 && st.LastCloses[sym] <= 0 {
				return fmt.Errorf("%s account holds %s without a closing price", name, sym)
			}
		}
	}
	for _, o := range append(append([]broker.Order{}, st.Pending...), st.BenchmarkPending...) {
		if o.Quantity <= 0 || (o.Side != "buy" && o.Side != "sell") {
			return fmt.Errorf("invalid pending order %q", o.ID)
		}
	}
	return nil
}

// Agent runs one decision cycle per exchange session against live data.
type Agent struct {
	Config  config.Config
	Market  market.PriceSource
	Analyst *analyst.Analyst
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
	Status          string             `json:"status"`
	Session         string             `json:"session"`
	Processed       []string           `json:"processed_sessions,omitempty"`
	Fills           []broker.Fill      `json:"fills,omitempty"`
	BenchmarkFills  []broker.Fill      `json:"benchmark_fills,omitempty"`
	Rejections      []broker.Rejection `json:"rejections,omitempty"`
	CorporateEvents []string           `json:"corporate_events,omitempty"`
	Scored          []ScoredSignal     `json:"scored_signals,omitempty"`
	Decision        *Decision          `json:"decision,omitempty"`
	Orders          []broker.Order     `json:"orders,omitempty"`
	Equity          *EquityPoint       `json:"equity,omitempty"`
	Notes           []string           `json:"notes,omitempty"`
	Briefing        *analyst.Briefing  `json:"briefing,omitempty"`
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

// RunCycle brings the account up to the latest closed session, saves that
// accounting, then makes one decision if orders can still reach the next open.
// A failed decision is retried on the next run without redoing the accounting.
// A dry run fetches live data and builds the briefing without calling the
// model or saving anything.
func (a *Agent) RunCycle(ctx context.Context, dryRun bool) (CycleReport, error) {
	c := a.Config
	lockCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	unlock, err := store.LockFile(lockCtx, filepath.Join(c.DataDir, "agent.lock"))
	cancel()
	if err != nil {
		return CycleReport{}, fmt.Errorf("another trader process is using %s: %w", c.DataDir, err)
	}
	defer unlock()
	st, err := LoadState(c)
	if err != nil {
		return CycleReport{}, &analyst.PermanentError{Err: err}
	}
	start := a.now()
	session, err := market.LatestClosedSession(start, c.CloseBuffer())
	if err != nil {
		return CycleReport{}, &analyst.PermanentError{Err: err}
	}
	dir := filepath.Join(c.DataDir, "cycles", session.Date)
	rep := CycleReport{Session: session.Date}
	accounting := st.LastSession == "" || session.Date > st.LastSession
	if !accounting {
		if st.DecisionPending != session.Date {
			rep.Status = StatusUpToDate
			return rep, nil
		}
		if err := store.ReadJSON(filepath.Join(dir, "cycle.json"), &rep); err != nil && !errors.Is(err, os.ErrNotExist) {
			return rep, err
		}
	}
	sessions := []market.Session{session}
	if accounting && st.LastSession != "" {
		if sessions, err = market.SessionsAfter(st.LastSession, session.Date); err != nil {
			return rep, err
		}
	}
	series, err := a.loadMarket(ctx, dir, sessions, fetchStart(start, st.LastSession), dryRun)
	if err != nil {
		return rep, err
	}
	if accounting {
		if st.DecisionPending != "" {
			st.MissedDecisions = append(st.MissedDecisions, st.DecisionPending)
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
		st.DecisionPending = session.Date
		if !dryRun {
			if err := a.save(dir, rep, st); err != nil {
				return rep, err
			}
		}
	}
	next, err := market.NextSessionAfter(session.Date)
	if err != nil {
		return rep, &analyst.PermanentError{Err: err}
	}
	deadline := next.OpenTime().Add(-5 * time.Minute)
	if start.After(deadline) {
		return a.missed(dir, rep, st, "too late to place orders for the "+next.Date+" open; holding positions", dryRun)
	}
	if !st.BenchmarkStarted && !dryRun {
		if st.BenchmarkPending, err = BenchmarkOrders(c, session.Date, st.Benchmark, st.LastCloses); err != nil {
			return rep, err
		}
		st.BenchmarkStarted = true
	}
	briefing, bars, err := a.briefing(session, start, series)
	if err != nil {
		return rep, err
	}
	if dryRun {
		rep.Status, rep.Briefing = StatusDryRun, &briefing
		return rep, nil
	}
	if a.Analyst == nil {
		return rep, &analyst.PermanentError{Err: errors.New("no analyst configured")}
	}
	win, winErr := signalWindow(c, session.Date)
	// The timeout counts from the agent's clock, which tests replace.
	actx, cancelAnalysis := context.WithTimeout(ctx, deadline.Sub(a.now()))
	result, err := a.Analyst.Analyze(actx, analyst.AnalystInput{Briefing: briefing, Bars: bars, Cutoff: start, Dir: dir})
	cancelAnalysis()
	done := a.now()
	switch {
	case errors.Is(err, analyst.ErrBudgetExceeded):
		return a.missed(dir, rep, st, err.Error()+"; holding positions", false)
	case (err == nil && done.After(deadline)) || (errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil):
		return a.missed(dir, rep, st, "analysis finished after the "+next.Date+" order deadline; holding positions", false)
	case err != nil:
		return rep, err
	}
	orders, plan, err := PlanOrders(c, session.Date, result.Signals, st.Agent, st.LastCloses)
	if err != nil {
		return rep, err
	}
	st.Pending = orders
	if !result.Abstained {
		if winErr != nil {
			rep.Notes = append(rep.Notes, "ratings not scored: "+winErr.Error())
		} else {
			st.OpenSignals = append(st.OpenSignals, openSignals(c, win, session.Date, result.Signals)...)
		}
	}
	st.LastDecision = &Decision{Session: session.Date, DecidedAt: done.UTC().Format(time.RFC3339), Summary: result.Summary, Signals: result.Signals, Plan: plan, CostUSD: result.CostUSD, Abstained: result.Abstained, Note: result.Reason}
	st.DecisionPending = ""
	rep.Status, rep.Decision, rep.Orders = StatusDecided, st.LastDecision, orders
	return rep, a.save(dir, rep, st)
}

// missed records that no orders will reach the next open and holds positions.
func (a *Agent) missed(dir string, rep CycleReport, st State, note string, dryRun bool) (CycleReport, error) {
	rep.Status = StatusMissed
	rep.Notes = append(rep.Notes, note)
	if dryRun {
		return rep, nil
	}
	st.MissedDecisions = append(st.MissedDecisions, rep.Session)
	st.DecisionPending = ""
	return rep, a.save(dir, rep, st)
}

func (a *Agent) save(dir string, rep CycleReport, st State) error {
	if err := store.WriteJSON(filepath.Join(dir, "cycle.json"), rep); err != nil {
		return err
	}
	return store.WriteJSON(StatePath(a.Config), st)
}

// fetchStart covers a year of history for indicators, reaching further back
// when catching up after a long outage.
func fetchStart(now time.Time, lastSession string) time.Time {
	from := now.AddDate(-1, 0, -35)
	if last, err := time.Parse(time.DateOnly, lastSession); err == nil && last.AddDate(0, 0, -150).Before(from) {
		from = last.AddDate(0, 0, -150)
	}
	return from
}

// dividendPayDays approximates the gap from ex-date to payment; the price
// source publishes ex-dates only.
const dividendPayDays = 21

func (a *Agent) processSession(st *State, rep *CycleReport, s market.Session, series map[string]market.Series) error {
	c := a.Config
	// The vendor restates history in today's share units. Convert each
	// session back to the units in force then, matching the ledger's shares.
	factor := map[string]float64{}
	opens, closes := map[string]float64{}, map[string]broker.Money{}
	for _, sym := range c.AllSymbols() {
		factor[sym] = series[sym].NativeFactor(s.Date)
	}
	for _, sym := range c.Symbols {
		bar, _ := series[sym].BarOn(s.Date)
		opens[sym], closes[sym] = bar.Open*factor[sym], broker.Cents(bar.Close*factor[sym])
	}
	for _, acct := range []*broker.Account{&st.Agent, &st.Benchmark} {
		if _, err := acct.SettleDividends(s.Date); err != nil {
			return err
		}
	}
	// Corporate actions take effect at the open, before any order fills.
	for _, sym := range c.Symbols {
		if sp, ok := series[sym].SplitOn(s.Date); ok {
			for _, acct := range []*broker.Account{&st.Agent, &st.Benchmark} {
				if _, err := acct.ApplySplit(sym, sp.Numerator, sp.Denominator, broker.Cents(opens[sym])); err != nil {
					return err
				}
			}
			var err error
			if st.Pending, err = broker.AdjustOrdersForSplit(st.Pending, sym, sp.Numerator, sp.Denominator); err != nil {
				return err
			}
			if st.BenchmarkPending, err = broker.AdjustOrdersForSplit(st.BenchmarkPending, sym, sp.Numerator, sp.Denominator); err != nil {
				return err
			}
			rep.CorporateEvents = append(rep.CorporateEvents, fmt.Sprintf("%s %s split %g-for-%g", s.Date, sym, sp.Numerator, sp.Denominator))
		}
		if amount := series[sym].DividendOn(s.Date) * factor[sym]; amount > 0 {
			d, _ := time.Parse(time.DateOnly, s.Date)
			payOn := d.AddDate(0, 0, dividendPayDays).Format(time.DateOnly)
			for _, acct := range []*broker.Account{&st.Agent, &st.Benchmark} {
				if _, err := acct.ApplyDividend(sym, amount, payOn); err != nil {
					return err
				}
			}
			rep.CorporateEvents = append(rep.CorporateEvents, fmt.Sprintf("%s %s dividend $%.4f per share, paid %s", s.Date, sym, amount, payOn))
		}
	}
	fills, rejections, err := broker.ExecuteAtOpen(c, s.Date, st.Pending, opens, &st.Agent)
	if err != nil {
		return err
	}
	benchFills, _, err := broker.ExecuteAtOpen(c, s.Date, st.BenchmarkPending, opens, &st.Benchmark)
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
			point.MarketClose = round(bar.Close*factor[c.MarketSymbol], 4)
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
func (a *Agent) loadMarket(ctx context.Context, dir string, sessions []market.Session, from time.Time, dryRun bool) (map[string]market.Series, error) {
	symbols := a.Config.AllSymbols()
	series := map[string]market.Series{}
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
		if series[sym], err = market.ParseYahoo(raw, sym); err != nil {
			return nil, err
		}
	}
	if !archived {
		raws := map[string][]byte{}
		for _, sym := range symbols {
			raw, err := a.Market.Fetch(ctx, sym, from)
			if err != nil {
				return nil, err
			}
			if series[sym], err = market.ParseYahoo(raw, sym); err != nil {
				return nil, err
			}
			raws[sym] = raw
		}
		if err := market.RequireBars(series, symbols, sessions); err != nil {
			return nil, err
		}
		if !dryRun {
			for sym, raw := range raws {
				if err := store.AtomicWrite(filepath.Join(dir, "market", sym+".json"), raw); err != nil {
					return nil, err
				}
			}
		}
	}
	return series, market.RequireBars(series, symbols, sessions)
}

func (a *Agent) briefing(session market.Session, now time.Time, series map[string]market.Series) (analyst.Briefing, map[string][]market.Bar, error) {
	c := a.Config
	bars := map[string][]market.Bar{}
	for _, sym := range c.AllSymbols() {
		bars[sym] = series[sym].BarsThrough(session.Date)
	}
	b := analyst.Briefing{Session: session.Date, DecisionTime: now.In(market.NewYork).Format(time.RFC3339), HorizonSessions: c.HorizonSessions}
	for _, sym := range c.Symbols {
		ind, err := market.ComputeIndicators(sym, analyst.PriceEvidenceID(c, sym), bars[sym])
		if err != nil {
			return b, nil, err
		}
		b.Universe = append(b.Universe, ind)
	}
	if c.MarketSymbol != "" {
		ind, err := market.ComputeIndicators(c.MarketSymbol, analyst.PriceEvidenceID(c, c.MarketSymbol), bars[c.MarketSymbol])
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
		var perm *analyst.PermanentError
		switch {
		case err != nil && ctx.Err() != nil:
			return err
		case errors.As(err, &perm):
			next, nerr := market.NextCycleTime(a.now(), a.Config.CloseBuffer())
			if nerr != nil {
				return err // calendar exhausted: there is no next close to wait for
			}
			wake = next
			a.logf("%s cycle failed and needs attention (next attempt %s): %v", a.now().Format(time.RFC3339), wake.In(market.NewYork).Format("Mon Jan 2 15:04 MST"), err)
		case err != nil:
			wake = a.now().Add(10 * time.Minute)
			a.logf("%s cycle failed, retrying at %s: %v", a.now().Format(time.RFC3339), wake.In(market.NewYork).Format("Jan 2 15:04 MST"), err)
		default:
			if wake, err = market.NextCycleTime(a.now(), a.Config.CloseBuffer()); err != nil {
				return &analyst.PermanentError{Err: err}
			}
			a.logf("%s %s %s · next %s", a.now().UTC().Format("2006-01-02 15:04Z"), rep.Session, rep.Status, wake.In(market.NewYork).Format("Mon Jan 2 15:04 MST"))
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

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
