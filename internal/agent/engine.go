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

	"github.com/ismatsamadov/trader/internal/analyst"
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
	News     news.Feeds
	Bot      *telegram.Bot
	Now      func() time.Time
	Log      io.Writer

	st        State
	rules     map[string]binance.Symbol
	prices    map[string]num.Dec
	moves     map[string]float64
	headlines []news.Item
	newsQueue []news.Item
	newsAt    time.Time
	retryAt   time.Time
	saved     []byte
	alerted   map[string]time.Time
	inbox     chan inbound
	later     []inbound
	deciding  bool
	logMu     sync.Mutex
}

type inbound struct {
	cmd      telegram.Command
	offset   int64
	received time.Time
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

// notify sends text to Telegram and the log. Testnet messages are marked so
// they are never mistaken for real trades.
func (e *Engine) notify(ctx context.Context, text string) {
	if e.Config.Mode != config.Live {
		text = "[testnet] " + text
	}
	e.logf("%s", strings.ReplaceAll(text, "\n", " · "))
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
		e.alert(ctx, "save", "Could not save state: "+err.Error())
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

// checkKey refuses live keys that can withdraw: the agent only needs to
// trade. A missing answer counts as a key that can.
func (e *Engine) checkKey(ctx context.Context) (binance.Permissions, error) {
	p, err := e.Exchange.Permissions(ctx)
	if err != nil {
		return p, fmt.Errorf("could not read the API key's permissions: %w", err)
	}
	if p.EnableWithdrawals == nil || *p.EnableWithdrawals {
		return p, errors.New("this API key can withdraw funds; create one with withdrawals disabled")
	}
	return p, nil
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
	st := State{
		Version: stateVersion, Mode: c.Mode, Name: c.Name, Symbols: c.Symbols, CreatedAt: now, Nonce: nonce.Text(36),
		Capital: capital, RiskBase: capital, Ledger: Ledger{Cash: capital, Holdings: map[string]num.Dec{}},
		Benchmark: buyAndHold(c, capital, prices, acct.Taker), Day: now.Format(time.DateOnly), DayEquity: capital, NextID: 1,
	}
	return st, saveState(c, st)
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
	if err := e.connect(ctx); err != nil {
		return err
	}
	if e.Config.Mode == config.Live {
		p, err := e.checkKey(ctx)
		switch {
		case err != nil:
			e.halt(ctx, haltKey, err.Error())
		case e.st.HaltKind == haltKey:
			e.clearHalt(haltKey)
		}
		if err == nil && !p.IPRestrict {
			e.notify(ctx, "Warning: this API key has no IP restriction. If it leaks it can trade this account; restrict it to your IP on Binance.")
		}
	}
	if len(e.st.Pending) > 0 {
		e.recover(ctx)
	}
	e.checkLedger(ctx)
	e.reconcile(ctx)
	return e.save()
}

// Run trades until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) error {
	unlock, err := e.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := e.open(ctx); err != nil {
		return err
	}
	e.inbox = make(chan inbound, 16)
	if e.Bot != nil {
		go e.poll(ctx, e.st.TelegramOffset)
	}
	e.notify(ctx, e.startText())
	for {
		e.tick(ctx)
		e.handleLater(ctx)
		e.saveOrAlert(ctx)
		timer := time.NewTimer(time.Duration(e.Config.PollSeconds) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			e.notify(ctx, "Stopped.")
			return e.save()
		case in := <-e.inbox:
			timer.Stop()
			e.handle(ctx, in)
			e.handleLater(ctx)
		case <-timer.C:
		}
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
	err = e.decide(ctx, "requested from the command line")
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
			case e.inbox <- inbound{c, next, e.now()}:
			case <-ctx.Done():
				return
			}
		}
	}
}

const (
	stopReason    = "stopped from Telegram; /resume to trade again"
	sellAllReason = "sell-all from Telegram; /resume to trade again"
)

// stale reports a command that was already old when it arrived, because it
// was sent while the agent was offline. A stop is honoured regardless.
func stale(in inbound) bool {
	return in.cmd.Name != "stop" && in.received.Sub(in.cmd.Sent) > 10*time.Minute
}

// checkInbox runs before every order. A stop or sell-all takes effect at
// once, so no further order goes out; every command is then handled after
// the current work, never in the middle of it.
func (e *Engine) checkInbox() {
	for {
		select {
		case in := <-e.inbox:
			if !stale(in) {
				switch in.cmd.Name {
				case "stop":
					e.st.Halted, e.st.HaltKind = stopReason, haltManual
				case "sellall":
					e.st.Halted, e.st.HaltKind, e.st.Liquidate = sellAllReason, haltManual, true
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

const helpText = `/status  account, positions and last decision
/decide  make a decision now
/stop    stop trading, keep positions
/resume  trade again
/sellall sell everything and stop`

func (e *Engine) handle(ctx context.Context, in inbound) {
	defer e.saveOrAlert(ctx)
	e.st.TelegramOffset = max(e.st.TelegramOffset, in.offset)
	c := in.cmd
	if stale(in) {
		e.notify(ctx, fmt.Sprintf("Ignored /%s sent at %s while I was offline.", c.Name, c.Sent.Format("Jan 2 15:04 UTC")))
		return
	}
	switch c.Name {
	case "status":
		e.notify(ctx, e.statusText(ctx))
	case "stop":
		e.st.Halted, e.st.HaltKind = stopReason, haltManual
		e.notify(ctx, "Trading stopped: "+stopReason)
	case "sellall":
		e.st.Halted, e.st.HaltKind, e.st.Liquidate = sellAllReason, haltManual, true
		e.notify(ctx, "Selling everything; trading stays stopped until /resume.")
		e.sellAll(ctx)
	case "resume":
		if e.Config.Mode == config.Live {
			if _, err := e.checkKey(ctx); err != nil {
				e.st.Halted, e.st.HaltKind = err.Error(), haltKey
				e.notify(ctx, "Still stopped: "+err.Error())
				return
			}
		}
		e.resumeAll()
		e.notify(ctx, "Trading resumed.")
	case "decide":
		switch {
		case e.deciding:
			e.notify(ctx, "A decision is already running.")
		case e.st.Halted != "" || e.st.Liquidate:
			e.notify(ctx, "Trading is stopped ("+e.st.Halted+").")
		default:
			if err := e.decide(ctx, "requested from Telegram"); err != nil {
				e.notify(ctx, "Decision failed: "+untrusted(err.Error()))
			}
		}
	default:
		e.notify(ctx, helpText)
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
	e.notify(ctx, "Trading stopped: "+reason)
}

// clearHalt ends an automatic stop of the given kind only.
func (e *Engine) clearHalt(kind string) {
	if e.st.HaltKind == kind {
		e.st.Halted, e.st.HaltKind = "", ""
	}
}

// resumeAll is /resume: every stop ends and the loss limits count from now,
// or the same losses would stop trading again at once.
func (e *Engine) resumeAll() {
	if equity, err := e.st.Ledger.Equity(e.prices); err == nil {
		e.st.DayEquity, e.st.RiskBase = equity, equity
	}
	e.st.Halted, e.st.HaltKind, e.st.Liquidate, e.st.DrawdownHit = "", "", false, false
}
