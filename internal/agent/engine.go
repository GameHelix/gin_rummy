// Package agent runs the trading loop: it watches prices and news, asks the
// analyst for ratings on a schedule or when something happens, trades on
// Binance within fixed limits, and reports to Telegram.
package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ismatsamadov/trader/internal/analyst"
	"github.com/ismatsamadov/trader/internal/audit"
	"github.com/ismatsamadov/trader/internal/binance"
	"github.com/ismatsamadov/trader/internal/config"
	"github.com/ismatsamadov/trader/internal/news"
	"github.com/ismatsamadov/trader/internal/num"
	"github.com/ismatsamadov/trader/internal/store"
	"github.com/ismatsamadov/trader/internal/telegram"
)

type Engine struct {
	Config   config.Config
	Exchange *binance.Client
	Analyst  *analyst.Analyst
	News     news.Source
	Bot      *telegram.Bot
	Now      func() time.Time
	Log      io.Writer
	// TenantID and AgentID name an agent the fleet manages; a standalone
	// agent creates its own.
	TenantID uuid.UUID
	AgentID  uuid.UUID

	st        State
	events    *audit.Log
	rules     map[string]binance.Symbol
	prices    map[string]num.Dec
	live      map[string]num.Dec
	moves     map[string]float64
	headlines []news.Item
	newsAt    time.Time
	saved     []byte
	alerted   map[string]time.Time
	inbox     chan inbound
	exit      chan struct{}
	mailOnce  sync.Once
	later     []inbound
	job       *job
	results   chan result
	keyOK     bool
	keyAt     time.Time
	sellFails map[string]int
	logMu     sync.Mutex
}

// inbound is a user command, from Telegram or the engine API.
type inbound struct {
	name     string
	source   string
	sent     time.Time
	received time.Time
	offset   int64      // Telegram's next update offset
	reply    chan Reply // where an engine API caller waits
}

const (
	sourceTelegram = "telegram"
	sourceAPI      = "api"
)

// Reply answers a user command.
type Reply struct {
	OK     bool    // the command took effect
	Text   string  // the answer, as Telegram shows it
	Report *Report // for status
}

// ErrNotRunning means the agent's loop is not running, so it cannot take
// commands.
var ErrNotRunning = errors.New("the agent is not running")

// mailbox is where commands wait for the loop. It exists before the loop
// starts, so commands can queue while the agent opens its account.
func (e *Engine) mailbox() chan inbound {
	e.mailOnce.Do(func() {
		e.inbox, e.exit = make(chan inbound, 16), make(chan struct{})
	})
	return e.inbox
}

// Command runs a command from the engine API in the agent's loop, as a
// Telegram command runs, and returns the answer. A command that changes
// something is recorded and announced on Telegram too. Names are those of
// the Telegram commands: decide, stop, resume and sellall.
func (e *Engine) Command(ctx context.Context, name string) (Reply, error) {
	return e.ask(ctx, inbound{name: name, source: sourceAPI, sent: e.now(), received: e.now()})
}

// Status reports the account as the running loop sees it, without
// recording or announcing anything.
func (e *Engine) Status(ctx context.Context) (Report, error) {
	r, err := e.ask(ctx, inbound{name: "status", source: sourceAPI, sent: e.now(), received: e.now()})
	if err != nil {
		return Report{}, err
	}
	if r.Report == nil {
		return Report{}, errors.New(r.Text)
	}
	return *r.Report, nil
}

func (e *Engine) ask(ctx context.Context, in inbound) (Reply, error) {
	in.reply = make(chan Reply, 1)
	inbox := e.mailbox()
	select {
	case inbox <- in:
	case <-e.exit:
		return Reply{}, ErrNotRunning
	case <-ctx.Done():
		return Reply{}, ctx.Err()
	}
	select {
	case r := <-in.reply:
		return r, nil
	case <-e.exit:
		// The loop may have answered just before it stopped.
		select {
		case r := <-in.reply:
			return r, nil
		default:
			return Reply{}, ErrNotRunning
		}
	case <-ctx.Done():
		return Reply{}, ctx.Err()
	}
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}

func (e *Engine) logf(format string, args ...any) {
	if e.Log == nil {
		return
	}
	e.logMu.Lock()
	defer e.logMu.Unlock()
	fmt.Fprintf(e.Log, "%s %s\n", time.Now().UTC().Format("2006-01-02 15:04:05Z"), fmt.Sprintf(format, args...))
}

// notify sends text to Telegram and the log. On testnet every message starts
// with 🧪 so it is never mistaken for a real trade.
func (e *Engine) notify(ctx context.Context, text string) {
	if e.Config.Mode != config.Live {
		text = "🧪 " + text
	}
	e.logf("%s", strings.ReplaceAll(text, "\n", " | "))
	if e.Bot == nil {
		return
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := e.Bot.Send(sctx, text); err != nil {
		e.logf("telegram: %v", err)
	}
}

// alert is notify at most once per half hour for each kind of problem.
func (e *Engine) alert(ctx context.Context, kind, text string) {
	if e.alerted == nil {
		e.alerted = map[string]time.Time{}
	}
	if last, ok := e.alerted[kind]; ok && e.now().Sub(last) < 30*time.Minute {
		e.logf("%s", text)
		return
	}
	e.alerted[kind] = e.now()
	e.emit(evAlert, alertRaised{Kind: kind, Message: text})
	e.notify(ctx, text)
}

// save writes the state when it has changed since the last write.
func (e *Engine) save() error {
	raw, err := json.MarshalIndent(e.st, "", "  ")
	if err != nil {
		return err
	}
	if bytes.Equal(raw, e.saved) {
		return nil
	}
	if err := store.AtomicWrite(StatePath(e.Config), append(raw, '\n')); err != nil {
		return err
	}
	e.saved = raw
	return nil
}

func (e *Engine) saveOrAlert(ctx context.Context) {
	if err := e.save(); err != nil {
		e.alert(ctx, "save", "⚠️ Could not save state: "+err.Error())
	}
}

func (e *Engine) lock(ctx context.Context) (func(), error) {
	lctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	unlock, err := store.LockFile(lctx, filepath.Join(e.Config.Dir(), "agent.lock"))
	if err != nil {
		return nil, fmt.Errorf("another trader process is using %s (is the daemon running?): %w", e.Config.Dir(), err)
	}
	return unlock, nil
}

// connect syncs the clock and loads the trading rules.
func (e *Engine) connect(ctx context.Context) error {
	if err := e.Exchange.SyncTime(ctx); err != nil {
		return fmt.Errorf("Binance unreachable: %w", err)
	}
	rules, err := e.Exchange.Symbols(ctx, e.Config.Symbols)
	if err != nil {
		return err
	}
	for s, r := range rules {
		if !r.Trading || !r.QuoteMarket || r.StepSize.Sign() <= 0 {
			return fmt.Errorf("%s cannot be traded with market orders right now", s)
		}
	}
	e.rules = rules
	return nil
}

var (
	errKeyWithdraws = errors.New("this API key can withdraw funds; create one with withdrawals disabled")
	errKeyNoSpot    = errors.New("this API key cannot trade spot; enable spot trading for it")
)

// checkKey requires a live key that can trade spot and cannot withdraw. A
// missing answer about withdrawals counts as a key that can.
func (e *Engine) checkKey(ctx context.Context) (binance.Permissions, error) {
	p, err := e.Exchange.Permissions(ctx)
	switch {
	case err != nil:
		return p, fmt.Errorf("could not read the API key's permissions: %w", err)
	case p.EnableWithdrawals == nil || *p.EnableWithdrawals:
		return p, errKeyWithdraws
	case p.EnableSpotTrading != nil && !*p.EnableSpotTrading:
		return p, errKeyNoSpot
	}
	return p, nil
}

// verifyKey checks the live key. A key that can withdraw or cannot trade
// stops trading; a failed lookup only holds orders back until a later check
// succeeds.
func (e *Engine) verifyKey(ctx context.Context) {
	e.keyAt = e.now()
	p, err := e.checkKey(ctx)
	switch {
	case errors.Is(err, errKeyWithdraws) || errors.Is(err, errKeyNoSpot):
		e.keyOK = false
		e.halt(ctx, haltKey, err.Error())
	case err != nil:
		e.keyOK = false
		e.alert(ctx, "key", "⚠️ "+err.Error()+". No orders until the check works; retrying every 10 minutes.")
	default:
		e.keyOK = true
		if e.st.HaltKind == haltKey {
			e.clearHalt(haltKey)
			e.notify(ctx, "▶️ The API key checks out again; trading resumed.")
		}
		if !p.IPRestrict {
			e.alert(ctx, "ip", "⚠️ This API key has no IP restriction. If it leaks it can trade this account; restrict it to your IP on Binance.")
		}
	}
}

// Init creates the account with capital_usd of the exchange's quote balance
// and a buy-and-hold twin that invests the same capital once.
func (e *Engine) Init(ctx context.Context) (State, error) {
	c := e.Config
	if _, err := os.Stat(StatePath(c)); err == nil {
		return State{}, fmt.Errorf("%s already exists; the account is already initialised", StatePath(c))
	}
	unlock, err := e.lock(ctx)
	if err != nil {
		return State{}, err
	}
	defer unlock()
	if _, err := os.Stat(StatePath(c)); err == nil {
		return State{}, fmt.Errorf("%s already exists; the account is already initialised", StatePath(c))
	} else if !errors.Is(err, os.ErrNotExist) {
		return State{}, err
	}
	if err := e.connect(ctx); err != nil {
		return State{}, err
	}
	if c.Mode == config.Live {
		if _, err := e.checkKey(ctx); err != nil {
			return State{}, err
		}
	}
	acct, err := e.Exchange.Account(ctx)
	if err != nil {
		return State{}, err
	}
	capital, err := num.FromFloat(c.CapitalUSD)
	if err != nil {
		return State{}, err
	}
	if !acct.CanTrade {
		return State{}, errors.New("this Binance account cannot trade")
	}
	if free := acct.Balances[c.QuoteAsset].Free; free.Less(capital) {
		return State{}, fmt.Errorf("Binance has %s %s free; capital_usd needs %s", free, c.QuoteAsset, capital)
	}
	prices, err := e.Exchange.Prices(ctx, c.Symbols)
	if err != nil {
		return State{}, err
	}
	nonce, err := rand.Int(rand.Reader, big.NewInt(36*36*36*36*36))
	if err != nil {
		return State{}, err
	}
	now := e.now()
	tenant, agent := e.TenantID, e.AgentID
	if tenant == uuid.Nil || agent == uuid.Nil {
		tenant, agent = uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	}
	st := State{
		Version: stateVersion, TenantID: tenant.String(), AgentID: agent.String(), Mode: c.Mode, Name: c.Name, Symbols: c.Symbols, CreatedAt: now, Nonce: nonce.Text(36),
		Capital: capital, RiskBase: capital, TakerFee: e.feeRate(acct.Taker), Ledger: Ledger{Cash: capital, Holdings: map[string]num.Dec{}},
		Benchmark: buyAndHold(c, capital, prices, e.feeRate(acct.Taker)), Day: now.Format(time.DateOnly), DayEquity: capital, NextID: 1,
	}
	log, err := audit.Open(EventsDir(c), tenant, agent, e.Now)
	if err != nil {
		return State{}, err
	}
	defer log.Close()
	if _, err := log.Append(evAccountCreated, accountCreated{Mode: c.Mode, Name: c.Name, Symbols: c.Symbols, Quote: c.QuoteAsset, Capital: capital}); err != nil {
		return State{}, err
	}
	return st, saveState(c, st)
}

// EventsDir is where an account's audit log lives.
func EventsDir(c config.Config) string { return filepath.Join(c.Dir(), "events") }

// openEvents opens the audit log, giving an account from before the log
// existed its tenant and agent IDs first.
func (e *Engine) openEvents() error {
	if e.st.TenantID == "" || e.st.AgentID == "" {
		e.st.TenantID, e.st.AgentID = uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String()
		if e.TenantID != uuid.Nil && e.AgentID != uuid.Nil {
			e.st.TenantID, e.st.AgentID = e.TenantID.String(), e.AgentID.String()
		}
	}
	if e.AgentID != uuid.Nil && (e.st.AgentID != e.AgentID.String() || e.st.TenantID != e.TenantID.String()) {
		return fmt.Errorf("%s holds another agent's account", e.Config.Dir())
	}
	tenant, err := uuid.Parse(e.st.TenantID)
	if err != nil {
		return err
	}
	agent, err := uuid.Parse(e.st.AgentID)
	if err != nil {
		return err
	}
	if e.events != nil {
		e.events.Close()
	}
	e.events, err = audit.Open(EventsDir(e.Config), tenant, agent, e.Now)
	if err != nil {
		return err
	}
	if e.Analyst != nil {
		e.Analyst.Observe = func(c analyst.ModelCall) { e.emit(evModelCall, c) }
	}
	return nil
}

// simulatedFee is Binance's standard spot fee. The testnet charges none, so
// testnet fills pay it on paper to keep their profit and loss realistic.
var simulatedFee = num.MustParse("0.001")

func (e *Engine) feeRate(taker num.Dec) num.Dec {
	if e.Config.Mode != config.Live && taker.IsZero() {
		return simulatedFee
	}
	return taker
}

func buyAndHold(c config.Config, capital num.Dec, prices map[string]num.Dec, fee num.Dec) Ledger {
	weight, _ := num.FromFloat(math.Min(c.MaxPositionWeight, c.MaxInvestedWeight/float64(len(c.Symbols))))
	b := Ledger{Cash: capital, Holdings: map[string]num.Dec{}}
	for _, s := range c.Symbols {
		spend := capital.Mul(weight)
		b.Holdings[s] = spend.Div(prices[s]).Mul(num.Int(1).Sub(fee))
		b.Cash = b.Cash.Sub(spend)
	}
	return b
}

// open loads the account and makes it safe to trade: it settles orders left
// unresolved by a crash and checks the exchange holds what the ledger says.
func (e *Engine) open(ctx context.Context) error {
	st, err := LoadState(e.Config)
	if err != nil {
		return err
	}
	e.st, e.saved = st, nil
	if err := e.openEvents(); err != nil {
		return fmt.Errorf("audit log: %w", err)
	}
	e.mailbox()
	if e.results == nil {
		e.results = make(chan result, 1)
	}
	if err := e.connect(ctx); err != nil {
		return err
	}
	if e.Config.Mode == config.Live {
		e.verifyKey(ctx)
	}
	if len(e.st.Pending) > 0 {
		e.recover(ctx)
	}
	e.checkLedger(ctx)
	e.reconcile(ctx)
	if err := e.emit(evAgentStarted, agentStarted{Mode: e.Config.Mode, Config: fingerprint(e.Config), Model: fingerprint(e.Config.Model), Cash: e.st.Ledger.Cash, Holdings: e.st.Ledger.Holdings, Halted: e.st.Halted}); err != nil {
		return fmt.Errorf("audit log: %w", err)
	}
	return e.save()
}

// Run trades until ctx is cancelled. Model calls run in the background, so
// prices, loss limits, sell-offs and commands keep being handled meanwhile.
func (e *Engine) Run(ctx context.Context) error {
	e.mailbox()
	defer close(e.exit)
	unlock, err := e.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := e.open(ctx); err != nil {
		return err
	}
	if e.Bot != nil {
		go e.poll(ctx, e.st.TelegramOffset)
	}
	e.notify(ctx, e.startText())
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			e.emit(evAgentStopped, agentStopped{Cash: e.st.Ledger.Cash, Holdings: e.st.Ledger.Holdings, Halted: e.st.Halted})
			e.notify(ctx, "⏹ Stopped.")
			return e.save()
		case in := <-e.inbox:
			e.handle(ctx, in)
		case r := <-e.results:
			e.finish(ctx, r)
		case <-timer.C:
			e.tick(ctx)
			timer.Reset(time.Duration(e.Config.PollSeconds) * time.Second)
		}
		e.handleLater(ctx)
		e.saveOrAlert(ctx)
	}
}

// DecideOnce makes one decision now, for the command line.
func (e *Engine) DecideOnce(ctx context.Context) (*Decision, error) {
	unlock, err := e.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := e.open(ctx); err != nil {
		return nil, err
	}
	if err := e.watch(ctx); err != nil {
		return nil, err
	}
	if !e.guard(ctx) {
		e.saveOrAlert(ctx)
		return nil, fmt.Errorf("trading is stopped: %s", e.st.Halted)
	}
	e.readNews(ctx, e.now())
	if err := e.startDecision(ctx, kindManual, "requested from the command line"); err != nil {
		return nil, err
	}
	select {
	case r := <-e.results:
		err = e.finish(ctx, r)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if serr := e.save(); err == nil {
		err = serr
	}
	if err != nil {
		return nil, err
	}
	return e.st.Last(), nil
}

// Preview builds the briefing from live data without calling the model,
// trading or saving anything. It needs no API keys.
func (e *Engine) Preview(ctx context.Context) (analyst.Briefing, error) {
	st, err := LoadState(e.Config)
	if _, statErr := os.Stat(StatePath(e.Config)); errors.Is(statErr, os.ErrNotExist) {
		capital, _ := num.FromFloat(e.Config.CapitalUSD)
		st, err = State{Capital: capital, Ledger: Ledger{Cash: capital, Holdings: map[string]num.Dec{}}, Benchmark: Ledger{Cash: capital, Holdings: map[string]num.Dec{}}, NextID: 1}, nil
	}
	if err != nil {
		return analyst.Briefing{}, err
	}
	e.st = st
	if err := e.watch(ctx); err != nil {
		return analyst.Briefing{}, err
	}
	e.readNews(ctx, e.now())
	in, err := e.snapshot(ctx, e.st.NextID, "preview", e.now(), "")
	return in.Briefing, err
}

func (e *Engine) poll(ctx context.Context, offset int64) {
	for ctx.Err() == nil {
		cmds, next, err := e.Bot.Poll(ctx, offset, 50*time.Second)
		if err != nil {
			if ctx.Err() == nil {
				e.logf("telegram: %v", err)
				select {
				case <-ctx.Done():
				case <-time.After(5 * time.Second):
				}
			}
			continue
		}
		offset = next
		for _, c := range cmds {
			select {
			case e.inbox <- inbound{name: c.Name, source: sourceTelegram, sent: c.Sent, received: e.now(), offset: next}:
			case <-ctx.Done():
				return
			}
		}
	}
}

const (
	stopReason    = "you stopped trading; /resume to trade again"
	sellAllReason = "you asked to sell everything; /resume to trade again"
)

// stale reports a command that changes something but was already old when
// it arrived, because it was sent while the agent was offline. A stop is
// honoured regardless, and questions such as /status are simply answered.
func stale(in inbound) bool {
	switch in.name {
	case "sellall", "decide", "resume":
		return in.received.Sub(in.sent) > 10*time.Minute
	}
	return false
}

// checkInbox runs before every order. A stop or sell-all takes effect at
// once, so no further order goes out; every command is then handled after
// the current work, never in the middle of it.
func (e *Engine) checkInbox() {
	for {
		select {
		case in := <-e.inbox:
			if !stale(in) {
				switch in.name {
				case "stop":
					e.st.Halted, e.st.HaltKind = stopReason, haltManual
					e.emit(evHalted, halted{haltManual, stopReason})
				case "sellall":
					e.st.Halted, e.st.HaltKind, e.st.Liquidate = sellAllReason, haltManual, true
					e.emit(evHalted, halted{haltManual, sellAllReason})
				}
			}
			e.later = append(e.later, in)
		default:
			return
		}
	}
}

func (e *Engine) handleLater(ctx context.Context) {
	for len(e.later) > 0 {
		in := e.later[0]
		e.later = e.later[1:]
		e.handle(ctx, in)
	}
}

const helpText = `Commands:
/status – account, holdings and the last decision
/decide – make a decision now (costs a model call)
/stop – stop trading, keep what is held
/resume – trade again
/sellall – sell everything and stop`

func (e *Engine) handle(ctx context.Context, in inbound) {
	if in.source == sourceAPI && in.name == "status" {
		// A read for the engine API: answered, not recorded or announced.
		if r, err := e.report(ctx); err != nil {
			in.reply <- Reply{Text: "Status unavailable: " + err.Error()}
		} else {
			in.reply <- Reply{OK: true, Text: r.Text(), Report: &r}
		}
		return
	}
	defer e.saveOrAlert(ctx)
	if in.source == sourceTelegram {
		e.st.TelegramOffset = max(e.st.TelegramOffset, in.offset)
	}
	e.emit(evCommand, commandReceived{Name: in.name, Source: in.source, Sent: in.sent, Received: in.received, Ignored: stale(in)})
	// answer replies in the chat a Telegram command came from. An engine API
	// command is answered to its caller, and on Telegram too when it took
	// effect, so the chat hears of every stop and resume.
	answer := func(ok bool, text string) {
		if in.reply != nil {
			in.reply <- Reply{OK: ok, Text: text}
			in.reply = nil
		}
		if in.source == sourceTelegram || ok {
			e.notify(ctx, text)
		}
	}
	if stale(in) {
		answer(false, fmt.Sprintf("⚠️ Ignored /%s: it was sent at %s while I was offline.", in.name, in.sent.Format("Jan 2 15:04 UTC")))
		return
	}
	switch in.name {
	case "status":
		answer(true, e.statusText(ctx))
	case "stop":
		if e.st.Halted != stopReason {
			e.emit(evHalted, halted{haltManual, stopReason})
		}
		e.st.Halted, e.st.HaltKind = stopReason, haltManual
		answer(true, "🛑 Trading stopped. Holdings are kept; /resume to trade again.")
	case "sellall":
		if e.st.Halted != sellAllReason {
			e.emit(evHalted, halted{haltManual, sellAllReason})
		}
		e.st.Halted, e.st.HaltKind, e.st.Liquidate = sellAllReason, haltManual, true
		answer(true, "🛑 Selling everything. Trading stays stopped until /resume.")
		e.sellAll(ctx)
	case "resume":
		if ok, text := e.resume(ctx); text != "" {
			answer(ok, text)
		} else if in.reply != nil {
			// Trading stopped again at once, and that was announced.
			in.reply <- Reply{Text: "🛑 Still stopped: " + e.st.Halted}
		}
	case "decide":
		switch {
		case e.job != nil:
			answer(false, "⏳ A decision is already running.")
		case e.st.Halted != "" || e.st.Liquidate:
			answer(false, "🛑 Trading is stopped ("+e.st.Halted+"). Send /resume first.")
		default:
			trigger := "you asked with /decide"
			if in.source == sourceAPI {
				trigger = "you asked in the app"
			}
			if err := e.startDecision(ctx, kindManual, trigger); err != nil {
				answer(false, "⚠️ Decision failed: "+untrusted(err.Error()))
				return
			}
			answer(true, "⏳ Thinking. The decision follows in a minute or two.")
		}
	default:
		answer(false, helpText)
	}
}

// halt stops trading unless a stronger stop is already in place: a manual
// stop outranks a drawdown, which outranks a broken key or balance, which
// outranks the daily loss limit.
func (e *Engine) halt(ctx context.Context, kind, reason string) {
	if e.st.Halted == reason || (e.st.Halted != "" && haltRank[kind] < haltRank[e.st.HaltKind]) {
		return
	}
	e.st.Halted, e.st.HaltKind = reason, kind
	e.emit(evHalted, halted{kind, reason})
	e.notify(ctx, "🛑 Trading stopped: "+reason)
}

// clearHalt ends an automatic stop of the given kind only.
func (e *Engine) clearHalt(kind string) {
	if e.st.HaltKind == kind {
		e.st.Halted, e.st.HaltKind = "", ""
		e.emit(evResumed, resumed{By: "automatic"})
	}
}

// resume is /resume. Every stop ends, but the loss limits keep their
// baselines unless one fired: after a daily stop the day counts from now,
// and after a drawdown both limits do. A key or balance that is still wrong
// stops trading again at once. It returns whether trading is on and what to
// say, or no text when a new stop was already announced.
func (e *Engine) resume(ctx context.Context) (bool, string) {
	if e.Config.Mode == config.Live {
		if _, err := e.checkKey(ctx); err != nil {
			e.st.Halted, e.st.HaltKind = err.Error(), haltKey
			return false, "🛑 Still stopped: " + err.Error()
		}
		e.keyOK = true
	}
	if equity, err := e.st.Ledger.Equity(e.prices); err == nil {
		if e.st.HaltKind == haltDaily || e.st.DrawdownHit {
			e.st.DayEquity = equity
		}
		if e.st.DrawdownHit {
			e.st.RiskBase = equity
		}
	}
	e.st.Halted, e.st.HaltKind, e.st.Liquidate, e.st.DrawdownHit = "", "", false, false
	e.sellFails = nil
	e.emit(evResumed, resumed{By: "user"})
	e.reconcile(ctx)
	if e.st.Halted != "" {
		return false, ""
	}
	return true, "▶️ Trading resumed."
}
