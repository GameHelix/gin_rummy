// Package api is the engine API's contract: the types it sends and
// receives, a client for the app, and the OpenAPI document (openapi.yaml).
// It uses the standard library only, so the app can import it without the
// engine's dependencies.
package api

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"time"
)

// Spec is the OpenAPI document of this version of the API.
//
//go:embed openapi.yaml
var Spec []byte

// Health says whether the engine can work: its database answers and every
// agent that should run does. Down lists agents that have failed to run
// for ten minutes or more; the engine answers 503 while any exist. Commit
// is the source commit the engine was built from, when known.
type Health struct {
	OK     bool        `json:"ok"`
	Commit string      `json:"commit,omitempty"`
	Down   []AgentDown `json:"agents_down"`
}

// AgentDown is an agent that should run but keeps failing, with its last
// error.
type AgentDown struct {
	TenantID string    `json:"tenant_id"`
	AgentID  string    `json:"agent_id"`
	Since    time.Time `json:"since"`
	Error    string    `json:"error"`
}

// Plan is what a tenant's subscription allows. The app sets it; the engine
// enforces it on every config, start and plan change.
type Plan struct {
	Symbols                  []string `json:"symbols"`
	MaxAgents                int      `json:"max_agents"`
	MinDecisionIntervalHours float64  `json:"min_decision_interval_hours"`
	MaxEventDecisionsPerDay  int      `json:"max_event_decisions_per_day"`
	Live                     bool     `json:"live"`
	Models                   []string `json:"models,omitempty"`
}

type Tenant struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Plan   Plan   `json:"plan"`
}

// NewTenant creates a tenant. With an ID, a retried request returns the
// tenant created the first time.
type NewTenant struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
	Plan Plan   `json:"plan"`
}

// PlanChange is a tenant's new plan and the agents it stopped.
type PlanChange struct {
	Plan    Plan      `json:"plan"`
	Stopped []Stopped `json:"stopped"`
}

// Stopped is an agent the engine stopped, and why.
type Stopped struct {
	AgentID string `json:"agent_id"`
	Reason  string `json:"reason"`
}

// Key is a stored key. The secret never leaves the engine; the hint shows
// its last characters.
type Key struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Label     string    `json:"label"`
	Hint      string    `json:"hint"`
	CreatedAt time.Time `json:"created_at"`
	Revoked   bool      `json:"revoked"`
}

// Kinds of keys.
const (
	KindAnthropic = "anthropic"
	KindBinance   = "binance"
)

// NewKey stores a key in the engine's vault after trying it. Secret is
// {"key"} for Anthropic and {"key", "secret"} for Binance. Mode says where a
// Binance key trades: testnet or live. With an ID, a retried request returns
// the key stored the first time.
type NewKey struct {
	ID     string          `json:"id,omitempty"`
	Kind   string          `json:"kind"`
	Label  string          `json:"label,omitempty"`
	Mode   string          `json:"mode,omitempty"`
	Secret json.RawMessage `json:"secret"`
}

type KeyList struct {
	Keys []Key `json:"keys"`
}

// Revoked lists the agents that stopped because a key was revoked.
type Revoked struct {
	Stopped []Stopped `json:"stopped"`
}

// Closed lists the agents that closing an account stopped.
type Closed struct {
	Stopped []Stopped `json:"stopped"`
}

// Agent is a trading agent. Status is what the user asked for, active or
// stopped; Running is whether the engine runs it right now.
type Agent struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Mode    string            `json:"mode"`
	Status  string            `json:"status"`
	Running bool              `json:"running"`
	Version int               `json:"version"`
	Keys    map[string]string `json:"keys"`
}

// NewAgent creates a stopped agent with its first config and its keys.
// With an ID, a retried request returns the agent created the first time.
type NewAgent struct {
	ID     string          `json:"id,omitempty"`
	Name   string          `json:"name"`
	Mode   string          `json:"mode"`
	Config json.RawMessage `json:"config"`
	Keys   []string        `json:"keys,omitempty"`
}

type AgentList struct {
	Agents []Agent `json:"agents"`
}

// Config is one version of an agent's config.
type Config struct {
	Version int             `json:"version"`
	Config  json.RawMessage `json:"config"`
}

// Commands an agent takes, as it does from a chat.
const (
	Decide        = "decide"
	StopTrading   = "stop_trading"
	ResumeTrading = "resume_trading"
	SellAll       = "sell_all"
)

// Command is a command for a running agent. Via says where the user sent
// it, for the audit log: "app" (the default) or "telegram".
type Command struct {
	Command string `json:"command"`
	Via     string `json:"via,omitempty"`
}

// CommandResult is the agent's answer. OK is false when the agent did not
// act, for example a decision while trading is stopped.
type CommandResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// Status is an agent's account. Amounts are decimal strings in the quote
// asset. Text written by the model is untrusted: show it as plain text.
// Message is the same account as a short text, as a chat shows it.
type Status struct {
	AgentID       string     `json:"agent_id"`
	Name          string     `json:"name"`
	Mode          string     `json:"mode"`
	Running       bool       `json:"running"`
	Trading       string     `json:"trading"`
	StopReason    string     `json:"stop_reason,omitempty"`
	Capital       string     `json:"capital"`
	Equity        string     `json:"equity"`
	BuyAndHold    string     `json:"buy_and_hold"`
	Cash          string     `json:"cash"`
	Fees          string     `json:"fees"`
	Positions     []Position `json:"positions"`
	LastDecision  *Decision  `json:"last_decision,omitempty"`
	NextDecision  *time.Time `json:"next_decision,omitempty"`
	Trades        int        `json:"trades"`
	PendingOrders int        `json:"pending_orders"`
	GradedCalls   int        `json:"graded_calls"`
	RightCalls    int        `json:"right_calls"`
	AISpend       *AISpend   `json:"ai_spend,omitempty"`
	At            time.Time  `json:"at"`
	Message       string     `json:"message"`
}

// Trading states in Status.
const (
	TradingOn      = "on"
	TradingStopped = "stopped"
	TradingSelling = "selling"
)

type Position struct {
	Symbol string  `json:"symbol"`
	Coin   string  `json:"coin"`
	Qty    string  `json:"qty"`
	Value  string  `json:"value"`
	Weight float64 `json:"weight"`
}

type Decision struct {
	ID        int               `json:"id"`
	At        time.Time         `json:"at"`
	Kind      string            `json:"kind"`
	Trigger   string            `json:"trigger"`
	Summary   string            `json:"summary"`
	Ratings   map[string]Rating `json:"ratings"`
	CostUSD   float64           `json:"cost_usd"`
	Abstained bool              `json:"abstained"`
}

type Rating struct {
	Rating     string `json:"rating"`
	Confidence string `json:"confidence"`
	Rationale  string `json:"rationale"`
}

// AISpend is what the agent's model calls cost, against its own limits.
type AISpend struct {
	TodayUSD        float64 `json:"today_usd"`
	MonthUSD        float64 `json:"month_usd"`
	DailyLimitUSD   float64 `json:"daily_limit_usd"`
	MonthlyLimitUSD float64 `json:"monthly_limit_usd"`
}

// Event is one entry of an agent's audit log, recorded before the step it
// describes. Each hash covers the event and the hash before it.
type Event struct {
	Seq      int64           `json:"seq"`
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	At       time.Time       `json:"at"`
	Payload  json.RawMessage `json:"payload"`
	Hash     string          `json:"hash"`
	PrevHash string          `json:"prev_hash,omitempty"`
}

type EventList struct {
	Events []Event `json:"events"`
}

// Error is the body of every failed request, under "error". Status is the
// HTTP status; Code is one of bad_request, unauthorized, not_found,
// conflict, not_running, no_account, refused, timeout and internal.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("engine API %d %s: %s", e.Status, e.Code, e.Message)
}

// RadarAlert is one unusual move the radar raised on a Hyperliquid
// perpetual. Changes are fractions: 0.05 is +5%.
type RadarAlert struct {
	Seq          int64            `json:"seq"`
	At           time.Time        `json:"at"`
	Coin         string           `json:"coin"`
	Rule         string           `json:"rule"`
	Price        float64          `json:"price"`
	Change15m    float64          `json:"change_15m"`
	Change1h     float64          `json:"change_1h"`
	Change24h    float64          `json:"change_24h"`
	Sigma15m     float64          `json:"sigma_15m"`
	VolumeRate   float64          `json:"volume_rate"`
	Volume24hUSD float64          `json:"volume_24h_usd"`
	OIChange1h   float64          `json:"oi_change_1h"`
	Funding      float64          `json:"funding"`
	Score        float64          `json:"score"`
	Reasons      []string         `json:"reasons"`
	Text         string           `json:"text"`
	History      string           `json:"history"`
	Suggestion   *TradeSuggestion `json:"suggestion,omitempty"`
}

// TradeSuggestion is the trade the history argues for on an alert, with
// the stop it needs; absent when the record says stay out.
type TradeSuggestion struct {
	Side        string  `json:"side"`
	StopLossPct float64 `json:"stop_loss_pct"`
	Reason      string  `json:"reason"`
}

type RadarAlertList struct {
	Alerts []RadarAlert `json:"alerts"`
}

// Wallet is an account's agent wallet on Hyperliquid: made by the engine,
// approved by the account's own wallet. Account is present once approved.
type Wallet struct {
	ID            string         `json:"id"`
	AgentAddress  string         `json:"agent_address"`
	User          string         `json:"user,omitempty"`
	Mode          string         `json:"mode,omitempty"`
	AgentApproved bool           `json:"agent_approved"`
	BuilderFee    int            `json:"builder_fee"`
	Account       *WalletAccount `json:"account,omitempty"`
}

// WalletAccount is the perpetuals account as the exchange sees it.
type WalletAccount struct {
	Value        float64        `json:"value"`
	Withdrawable float64        `json:"withdrawable"`
	Positions    []PerpPosition `json:"positions"`
}

// PerpPosition is an open perpetual position; Size is negative when short.
type PerpPosition struct {
	Coin          string  `json:"coin"`
	Size          float64 `json:"size"`
	EntryPx       float64 `json:"entry_px"`
	Value         float64 `json:"value"`
	UnrealizedPnl float64 `json:"unrealized_pnl"`
	LiquidationPx float64 `json:"liquidation_px"`
	Leverage      int     `json:"leverage"`
	Cross         bool    `json:"cross"`
}

// WalletApprovals is what the account's wallet signs with
// eth_signTypedData_v4: each holds the action, its nonce and the typed
// data. The builder fee is absent when the engine charges none.
type WalletApprovals struct {
	User              string          `json:"user"`
	Mode              string          `json:"mode"`
	AgentAddress      string          `json:"agent_address"`
	ApproveAgent      json.RawMessage `json:"approve_agent"`
	ApproveBuilderFee json.RawMessage `json:"approve_builder_fee,omitempty"`
}

// Signature is an EIP-712 signature as a wallet returns it.
type Signature struct {
	R string `json:"r"`
	S string `json:"s"`
	V int    `json:"v"`
}

// SignedAction is an approval's action and nonce as the engine gave them,
// with the account wallet's signature.
type SignedAction struct {
	Action    json.RawMessage `json:"action"`
	Nonce     uint64          `json:"nonce"`
	Signature Signature       `json:"signature"`
}

// WalletApproval submits the signed approvals for an account.
type WalletApproval struct {
	User              string        `json:"user"`
	Mode              string        `json:"mode"`
	ApproveAgent      SignedAction  `json:"approve_agent"`
	ApproveBuilderFee *SignedAction `json:"approve_builder_fee,omitempty"`
}

// NewTrade opens a position: a market order with its stop-loss, and a
// take-profit when given, placed together. USD is the position's size;
// the stop and take-profit are fractions of the entry price.
type NewTrade struct {
	Coin          string  `json:"coin"`
	Side          string  `json:"side"`
	USD           float64 `json:"usd"`
	Leverage      int     `json:"leverage"`
	StopLossPct   float64 `json:"stop_loss_pct"`
	TakeProfitPct float64 `json:"take_profit_pct,omitempty"`
	SlippagePct   float64 `json:"slippage_pct,omitempty"`
}

// Trade is an opened position with its orders on the exchange.
type Trade struct {
	Coin          string  `json:"coin"`
	Side          string  `json:"side"`
	Size          float64 `json:"size"`
	EntryPx       float64 `json:"entry_px"`
	USD           float64 `json:"usd"`
	Leverage      int     `json:"leverage"`
	OID           int64   `json:"oid"`
	StopPx        float64 `json:"stop_px"`
	StopOID       int64   `json:"stop_oid"`
	TakeProfitPx  float64 `json:"take_profit_px,omitempty"`
	TakeProfitOID int64   `json:"take_profit_oid,omitempty"`
}

// CloseTrade closes the position in a coin.
type CloseTrade struct {
	Coin        string  `json:"coin"`
	SlippagePct float64 `json:"slippage_pct,omitempty"`
}

// ClosedTrade is the result of a close. Remaining is what did not fill.
type ClosedTrade struct {
	Coin           string  `json:"coin"`
	Size           float64 `json:"size"`
	AvgPx          float64 `json:"avg_px"`
	Remaining      float64 `json:"remaining"`
	StopsCancelled int     `json:"stops_cancelled"`
}
