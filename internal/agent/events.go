package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/ismatsamadov/trader/internal/analyst"
	"github.com/ismatsamadov/trader/internal/news"
	"github.com/ismatsamadov/trader/internal/num"
)

// The events an agent records in its audit log. This is the contract the
// engine API and the app read; add fields, never change their meaning.
const (
	evAccountCreated  = "account.created"
	evAgentStarted    = "agent.started"
	evAgentStopped    = "agent.stopped"
	evDecisionStarted = "decision.started"
	evModelCall       = "model.call"
	evDecisionDone    = "decision.finished"
	evDecisionFailed  = "decision.failed"
	evOrderIntent     = "order.intent"
	evOrderResult     = "order.result"
	evTradeBooked     = "trade.booked"
	evHalted          = "trading.stopped"
	evResumed         = "trading.resumed"
	evCommand         = "command.received"
	evEquity          = "equity.hourly"
	evCallGraded      = "call.graded"
	evNewsAlert       = "news.alert"
	evAlert           = "alert.raised"
)

type accountCreated struct {
	Mode    string   `json:"mode"`
	Name    string   `json:"name"`
	Symbols []string `json:"symbols"`
	Quote   string   `json:"quote"`
	Capital num.Dec  `json:"capital"`
}

type agentStarted struct {
	Mode     string             `json:"mode"`
	Config   string             `json:"config_fingerprint"`
	Model    string             `json:"model_fingerprint"`
	Cash     num.Dec            `json:"cash"`
	Holdings map[string]num.Dec `json:"holdings"`
	Halted   string             `json:"halted,omitempty"`
}

type agentStopped struct {
	Cash     num.Dec            `json:"cash"`
	Holdings map[string]num.Dec `json:"holdings"`
	Halted   string             `json:"halted,omitempty"`
}

// alertRaised is a problem reported to the user, such as missing prices.
type alertRaised struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

type decisionStarted struct {
	Decision int              `json:"decision"`
	Kind     string           `json:"kind"`
	Trigger  string           `json:"trigger"`
	Config   string           `json:"config_fingerprint"`
	Model    string           `json:"model_fingerprint"`
	Briefing analyst.Briefing `json:"briefing"`
}

type decisionFinished struct {
	Decision  int                       `json:"decision"`
	Signals   map[string]analyst.Signal `json:"signals"`
	Summary   string                    `json:"summary"`
	Abstained bool                      `json:"abstained,omitempty"`
	CostUSD   float64                   `json:"cost_usd"`
	Plans     []Plan                    `json:"plans"`
	Note      string                    `json:"note,omitempty"`
}

type decisionFailed struct {
	Decision int    `json:"decision,omitempty"`
	Error    string `json:"error"`
}

type orderIntent struct {
	Decision int     `json:"decision,omitempty"`
	ClientID string  `json:"client_id"`
	Symbol   string  `json:"symbol"`
	Side     string  `json:"side"`
	Qty      num.Dec `json:"qty"`
	QuoteQty num.Dec `json:"quote_qty"`
}

// Order outcomes.
const (
	outcomeFilled    = "filled"
	outcomeExpired   = "expired"
	outcomeRejected  = "rejected"
	outcomeUnknown   = "unknown"
	outcomeMissing   = "never_arrived"
	outcomeRecovered = "recovered"
)

type orderResult struct {
	ClientID    string  `json:"client_id"`
	Outcome     string  `json:"outcome"`
	OrderID     int64   `json:"order_id,omitempty"`
	Status      string  `json:"status,omitempty"`
	ExecutedQty num.Dec `json:"executed_qty"`
	QuoteQty    num.Dec `json:"quote_qty"`
	Error       string  `json:"error,omitempty"`
}

type tradeBooked struct {
	Trade   Trade   `json:"trade"`
	Cash    num.Dec `json:"cash_after"`
	Holding num.Dec `json:"holding_after"`
}

type halted struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

type resumed struct {
	By string `json:"by"`
}

type commandReceived struct {
	Name     string    `json:"name"`
	Source   string    `json:"source"`
	Sent     time.Time `json:"sent_at"`
	Received time.Time `json:"received_at"`
	Ignored  bool      `json:"ignored_as_stale,omitempty"`
}

type equityHourly struct {
	Equity    num.Dec            `json:"equity"`
	Benchmark num.Dec            `json:"buy_and_hold"`
	Cash      num.Dec            `json:"cash"`
	Prices    map[string]num.Dec `json:"prices"`
}

type newsAlert struct {
	Items []news.Item `json:"items"`
}

// emit writes an event to the audit log. Callers that are about to act on
// something, such as sending an order, stop when it fails: nothing happens
// that is not on record first.
func (e *Engine) emit(typ string, payload any) error {
	if e.events == nil {
		return nil
	}
	if _, err := e.events.Append(typ, payload); err != nil {
		e.logf("audit log: %v", err)
		return err
	}
	return nil
}

// fingerprint identifies a value without storing it, such as which model
// or configuration a decision used.
func fingerprint(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:6])
}
