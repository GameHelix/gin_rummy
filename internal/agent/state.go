package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/candylario/trader/internal/analyst"
	"github.com/candylario/trader/internal/config"
	"github.com/candylario/trader/internal/news"
	"github.com/candylario/trader/internal/num"
	"github.com/candylario/trader/internal/store"
)

const stateVersion = "crypto-agent-v1"

// Ledger is the agent's share of the exchange account: the capital it was
// given and what it bought with it. Fees paid in an asset it does not hold,
// such as BNB, reduce equity through OffBookFees.
type Ledger struct {
	Cash        num.Dec            `json:"cash"`
	Holdings    map[string]num.Dec `json:"holdings"`
	FeesUSD     num.Dec            `json:"fees_usd"`
	OffBookFees num.Dec            `json:"off_book_fees_usd"`
}

func (l Ledger) Equity(prices map[string]num.Dec) (num.Dec, error) {
	total := l.Cash.Sub(l.OffBookFees)
	for symbol, qty := range l.Holdings {
		if qty.IsZero() {
			continue
		}
		p, ok := prices[symbol]
		if !ok || p.Sign() <= 0 {
			return num.Dec{}, fmt.Errorf("no price to value %s", symbol)
		}
		total = total.Add(qty.Mul(p))
	}
	return total, nil
}

type Trade struct {
	Time     time.Time `json:"time"`
	Decision int       `json:"decision,omitempty"`
	Symbol   string    `json:"symbol"`
	Side     string    `json:"side"`
	Qty      num.Dec   `json:"qty"`
	QuoteQty num.Dec   `json:"quote_qty"`
	Price    num.Dec   `json:"avg_price"`
	FeeUSD   num.Dec   `json:"fee_usd"`
	OrderID  int64     `json:"order_id"`
	ClientID string    `json:"client_id"`
}

// Pending is an order sent, or about to be sent, whose result is not yet in
// the ledger. It is recorded before sending so a crash can never repeat it.
type Pending struct {
	ClientID string    `json:"client_id"`
	Symbol   string    `json:"symbol"`
	Side     string    `json:"side"`
	Qty      num.Dec   `json:"qty"`
	QuoteQty num.Dec   `json:"quote_qty"`
	Decision int       `json:"decision,omitempty"`
	SentAt   time.Time `json:"sent_at"`
}

type Point struct {
	Time      time.Time `json:"time"`
	Equity    num.Dec   `json:"equity"`
	Benchmark num.Dec   `json:"benchmark"`
}

type Decision struct {
	ID        int                       `json:"id"`
	Time      time.Time                 `json:"time"`
	Kind      string                    `json:"kind,omitempty"`
	Trigger   string                    `json:"trigger"`
	Summary   string                    `json:"summary"`
	Signals   map[string]analyst.Signal `json:"signals"`
	Prices    map[string]num.Dec        `json:"prices"`
	CostUSD   float64                   `json:"cost_usd"`
	Abstained bool                      `json:"abstained,omitempty"`
}

// OpenCall is a rating waiting to be graded at Due, with the lessons it
// relied on, which are graded with it.
type OpenCall struct {
	Decision int            `json:"decision"`
	Symbol   string         `json:"symbol"`
	Rating   analyst.Rating `json:"rating"`
	Entry    num.Dec        `json:"entry_price"`
	Due      time.Time      `json:"due"`
	Lessons  []string       `json:"lessons,omitempty"`
}

type ScoredCall struct {
	OpenCall
	Exit   num.Dec `json:"exit_price"`
	Return float64 `json:"return"`
	Hit    bool    `json:"hit"`
}

// State is the account's single source of truth, rewritten atomically.
type State struct {
	Version     string       `json:"schema_version"`
	TenantID    string       `json:"tenant_id"`
	AgentID     string       `json:"agent_id"`
	Mode        string       `json:"mode"`
	Name        string       `json:"name"`
	Symbols     []string     `json:"symbols"`
	CreatedAt   time.Time    `json:"created_at"`
	Nonce       string       `json:"order_nonce"`
	OrderSeq    int          `json:"order_seq"`
	Capital     num.Dec      `json:"capital"`
	RiskBase    num.Dec      `json:"drawdown_base"`
	TakerFee    num.Dec      `json:"taker_fee"`
	Ledger      Ledger       `json:"ledger"`
	Benchmark   Ledger       `json:"buy_and_hold"`
	Halted      string       `json:"halted,omitempty"`
	HaltKind    string       `json:"halt_kind,omitempty"`
	Liquidate   bool         `json:"selling_everything,omitempty"`
	DrawdownHit bool         `json:"drawdown_hit,omitempty"`
	Day         string       `json:"day"`
	DayEquity   num.Dec      `json:"day_start_equity"`
	NextID      int          `json:"next_decision_id"`
	Decisions   []Decision   `json:"decisions"`
	Pending     []Pending    `json:"pending_orders"`
	Trades      []Trade      `json:"trades"`
	History     []Point      `json:"history"`
	Open        []OpenCall   `json:"open_calls"`
	Scored      []ScoredCall `json:"scored_calls"`
	SeenNews    []string     `json:"seen_news"`
	NewsPrimed  bool         `json:"news_primed"`
	NewsQueue   []news.Item  `json:"news_queue,omitempty"`
	RetryAt     time.Time    `json:"retry_at,omitzero"`
	Reviews     []int        `json:"review_queue,omitempty"`
	ReviewAt    time.Time    `json:"review_retry_at,omitzero"`
	ReviewTries int          `json:"review_tries,omitempty"`
	// TelegramOffset is from the agent's own bot, since removed; it stays so
	// older state files still load, and is dropped on the next save.
	TelegramOffset int64 `json:"telegram_offset,omitempty"`
}

const (
	haltManual   = "manual"
	haltDaily    = "daily"
	haltDrawdown = "drawdown"
	haltMismatch = "mismatch"
	haltKey      = "key"
)

var haltRank = map[string]int{haltDaily: 1, haltKey: 2, haltMismatch: 2, haltDrawdown: 3, haltManual: 4}

func (st *State) Last() *Decision {
	if len(st.Decisions) == 0 {
		return nil
	}
	return &st.Decisions[len(st.Decisions)-1]
}

func StatePath(c config.Config) string { return filepath.Join(c.Dir(), "state.json") }

func LoadState(c config.Config) (State, error) {
	var st State
	if err := store.ReadJSON(StatePath(c), &st); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st, fmt.Errorf("no %s account at %s; run `trader init` first", c.Mode, StatePath(c))
		}
		return st, err
	}
	if st.Version != stateVersion {
		return st, fmt.Errorf("unsupported state version %q", st.Version)
	}
	if st.Mode != c.Mode || st.Name != c.Name || !slices.Equal(st.Symbols, c.Symbols) {
		return st, errors.New("config name, mode or symbols differ from the initialised account; use a new data_dir for a different setup")
	}
	// A negative balance still loads, so the agent can start, stop trading
	// and report it rather than refuse to run.
	for _, l := range []*Ledger{&st.Ledger, &st.Benchmark} {
		if l.Holdings == nil {
			l.Holdings = map[string]num.Dec{}
		}
		for s := range l.Holdings {
			if !slices.Contains(c.Symbols, s) {
				return st, fmt.Errorf("%s: holding of unknown symbol %s", StatePath(c), s)
			}
		}
	}
	if st.Nonce == "" {
		return st, fmt.Errorf("%s: missing order nonce", StatePath(c))
	}
	st.TelegramOffset = 0
	return st, nil
}

func saveState(c config.Config, st State) error { return store.WriteJSON(StatePath(c), st) }

// keep trims a log to its newest n entries.
func keep[T any](list []T, n int) []T {
	if len(list) > n {
		return append([]T(nil), list[len(list)-n:]...)
	}
	return list
}
