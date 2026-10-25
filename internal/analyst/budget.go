package analyst

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/ismatsamadov/trader/internal/store"
	"slices"
)

const nanosPerUSD int64 = 1_000_000_000

// ErrBudgetExceeded means a new model call would break the daily or monthly cap.
var ErrBudgetExceeded = errors.New("LLM spending limit reached")

// Attempt states. Reserved and dispatched attempts count their reservation
// against the caps; released attempts count nothing; uncertain attempts (the
// network failed after sending) stay charged until resolved.
const (
	attemptReserved   = "reserved"
	attemptDispatched = "dispatched"
	attemptSettled    = "settled"
	attemptReleased   = "released"
	attemptUncertain  = "uncertain"
	attemptResolved   = "resolved"
)

type BudgetAttempt struct {
	ID            string `json:"id"`
	Session       string `json:"session"`
	Purpose       string `json:"purpose"`
	Status        string `json:"status"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
	ReservedNanos int64  `json:"reserved_nanos"`
	SpentNanos    int64  `json:"spent_nanos"`
	InputTokens   int64  `json:"input_tokens,omitempty"`
	OutputTokens  int64  `json:"output_tokens,omitempty"`
	Note          string `json:"note,omitempty"`
}

type budgetJournal struct {
	Version  string          `json:"schema_version"`
	Attempts []BudgetAttempt `json:"attempts"`
}

// Budget is a durable spending journal shared by every process using the same
// data directory. Days and months are UTC.
type Budget struct {
	Path       string
	DailyUSD   float64
	MonthlyUSD float64
	Now        func() time.Time
}

type BudgetSummary struct {
	TodayUSD        float64         `json:"today_usd"`
	MonthUSD        float64         `json:"month_usd"`
	UnresolvedUSD   float64         `json:"unresolved_usd"`
	DailyLimitUSD   float64         `json:"daily_limit_usd"`
	MonthlyLimitUSD float64         `json:"monthly_limit_usd"`
	MonthCalls      int             `json:"month_calls"`
	Open            []BudgetAttempt `json:"open_attempts,omitempty"`
}

func usdNanos(v float64) (int64, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1e6 {
		return 0, errors.New("invalid USD amount")
	}
	return int64(math.Ceil(v * float64(nanosPerUSD))), nil
}

func (a BudgetAttempt) charged() int64 {
	switch a.Status {
	case attemptSettled, attemptResolved:
		return a.SpentNanos
	case attemptReserved, attemptDispatched, attemptUncertain:
		return a.ReservedNanos
	}
	return 0
}

func (b *Budget) now() time.Time {
	if b.Now != nil {
		return b.Now().UTC()
	}
	return time.Now().UTC()
}

func (b *Budget) load() (budgetJournal, error) {
	// Decode into a zero value: a file such as {} must fail the version check
	// rather than inherit defaults and silently erase recorded spending.
	var j budgetJournal
	if err := store.ReadJSON(b.Path, &j); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return budgetJournal{Version: "budget-v2"}, nil
		}
		return j, fmt.Errorf("budget journal: %w", err)
	}
	if j.Version != "budget-v2" {
		return j, errors.New("unsupported budget journal version")
	}
	return j, nil
}

func (b *Budget) edit(ctx context.Context, fn func(*budgetJournal) error) error {
	unlock, err := store.LockFile(ctx, b.Path+".lock")
	if err != nil {
		return err
	}
	defer unlock()
	j, err := b.load()
	if err != nil {
		return err
	}
	if err := fn(&j); err != nil {
		return err
	}
	b.compact(&j)
	return store.WriteJSON(b.Path, j)
}

// compact drops finished attempts from before last month: the caps only
// count this month, and open attempts stay until they are resolved.
func (b *Budget) compact(j *budgetJournal) {
	now := b.now()
	cutoff := time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, time.UTC)
	kept := j.Attempts[:0]
	for _, a := range j.Attempts {
		created, err := time.Parse(time.RFC3339Nano, a.CreatedAt)
		finished := a.Status == attemptSettled || a.Status == attemptReleased || a.Status == attemptResolved
		if err == nil && finished && created.Before(cutoff) {
			continue
		}
		kept = append(kept, a)
	}
	j.Attempts = kept
}

func (b *Budget) totals(j budgetJournal) (today, month int64, calls int) {
	now := b.now()
	day, mon := now.Format(time.DateOnly), now.Format("2006-01")
	for _, a := range j.Attempts {
		created, err := time.Parse(time.RFC3339Nano, a.CreatedAt)
		if err != nil {
			continue
		}
		if created.UTC().Format("2006-01") == mon {
			month += a.charged()
			if a.Status != attemptReleased {
				calls++
			}
			if created.UTC().Format(time.DateOnly) == day {
				today += a.charged()
			}
		}
	}
	return today, month, calls
}

// Reserve records an upper-bound cost before a request is sent.
func (b *Budget) Reserve(ctx context.Context, session, purpose string, usd float64) (BudgetAttempt, error) {
	var attempt BudgetAttempt
	reserve, err := usdNanos(usd)
	if err != nil {
		return attempt, err
	}
	daily, err := usdNanos(b.DailyUSD)
	if err != nil {
		return attempt, err
	}
	monthly, err := usdNanos(b.MonthlyUSD)
	if err != nil {
		return attempt, err
	}
	err = b.edit(ctx, func(j *budgetJournal) error {
		today, month, _ := b.totals(*j)
		if today+reserve > daily || month+reserve > monthly {
			return fmt.Errorf("%w (today $%.2f of $%.2f, month $%.2f of $%.2f; next call may cost up to $%.2f)", ErrBudgetExceeded, float64(today)/1e9, b.DailyUSD, float64(month)/1e9, b.MonthlyUSD, usd)
		}
		now := b.now().Format(time.RFC3339Nano)
		// The journal position makes IDs unique even for identical retries
		// within one clock tick.
		id := store.Hash([]byte(fmt.Sprintf("%s/%s/%s/%d", session, purpose, now, len(j.Attempts))))[:20]
		attempt = BudgetAttempt{ID: id, Session: session, Purpose: purpose, Status: attemptReserved, CreatedAt: now, UpdatedAt: now, ReservedNanos: reserve}
		j.Attempts = append(j.Attempts, attempt)
		return nil
	})
	return attempt, err
}

func (b *Budget) transition(ctx context.Context, id string, allowed []string, fn func(*BudgetAttempt)) error {
	return b.edit(ctx, func(j *budgetJournal) error {
		for i := range j.Attempts {
			a := &j.Attempts[i]
			if a.ID != id {
				continue
			}
			if !slices.Contains(allowed, a.Status) {
				return fmt.Errorf("budget attempt %s is %s", id, a.Status)
			}
			fn(a)
			a.UpdatedAt = b.now().Format(time.RFC3339Nano)
			return nil
		}
		return fmt.Errorf("unknown budget attempt %s", id)
	})
}

func (b *Budget) Dispatch(ctx context.Context, id string) error {
	return b.transition(ctx, id, []string{attemptReserved}, func(a *BudgetAttempt) { a.Status = attemptDispatched })
}

// Settle records the cost derived from the provider's usage report.
func (b *Budget) Settle(ctx context.Context, id string, usd float64, input, output int64) error {
	spent, err := usdNanos(usd)
	if err != nil {
		return err
	}
	return b.transition(ctx, id, []string{attemptDispatched}, func(a *BudgetAttempt) {
		a.Status, a.SpentNanos, a.ReservedNanos, a.InputTokens, a.OutputTokens = attemptSettled, spent, 0, input, output
	})
}

// Release frees a reservation for a request the provider rejected with an
// HTTP error response: it was not processed, so nothing was generated.
func (b *Budget) Release(ctx context.Context, id, note string) error {
	return b.transition(ctx, id, []string{attemptReserved, attemptDispatched}, func(a *BudgetAttempt) {
		a.Status, a.ReservedNanos, a.Note = attemptReleased, 0, note
	})
}

// Uncertain keeps the reservation charged when the outcome is unknown.
func (b *Budget) Uncertain(ctx context.Context, id, note string) error {
	return b.transition(ctx, id, []string{attemptReserved, attemptDispatched}, func(a *BudgetAttempt) {
		a.Status, a.Note = attemptUncertain, note
	})
}

// Resolve records a provider-verified cost for an uncertain or stale attempt.
func (b *Budget) Resolve(ctx context.Context, id string, usd float64, note string) error {
	if note == "" {
		return errors.New("a reconciliation note is required")
	}
	spent, err := usdNanos(usd)
	if err != nil {
		return err
	}
	return b.transition(ctx, id, []string{attemptUncertain, attemptDispatched, attemptReserved}, func(a *BudgetAttempt) {
		a.Status, a.SpentNanos, a.ReservedNanos, a.Note = attemptResolved, spent, 0, note
	})
}

func (b *Budget) Summary(ctx context.Context) (BudgetSummary, error) {
	unlock, err := store.LockFile(ctx, b.Path+".lock")
	if err != nil {
		return BudgetSummary{}, err
	}
	defer unlock()
	j, err := b.load()
	if err != nil {
		return BudgetSummary{}, err
	}
	today, month, calls := b.totals(j)
	s := BudgetSummary{TodayUSD: float64(today) / 1e9, MonthUSD: float64(month) / 1e9, DailyLimitUSD: b.DailyUSD, MonthlyLimitUSD: b.MonthlyUSD, MonthCalls: calls}
	for _, a := range j.Attempts {
		if a.Status == attemptUncertain || a.Status == attemptDispatched || a.Status == attemptReserved {
			s.UnresolvedUSD += float64(a.ReservedNanos) / 1e9
			s.Open = append(s.Open, a)
		}
	}
	return s, nil
}
