package trader

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestBudgetEnforcesDailyAndMonthlyCaps(t *testing.T) {
	now := time.Date(2026, 10, 23, 21, 0, 0, 0, time.UTC)
	b := &Budget{Path: filepath.Join(t.TempDir(), "budget.json"), DailyUSD: 1, MonthlyUSD: 1.5, Now: func() time.Time { return now }}
	ctx := context.Background()
	a, err := b.Reserve(ctx, "2026-10-23", "p1", "m", 0.6)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reserve(ctx, "2026-10-23", "p2", "m", 0.6); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("daily cap not enforced: %v", err)
	}
	b.Dispatch(ctx, a.ID)
	if err := b.Settle(ctx, a.ID, 0.2, 1000, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reserve(ctx, "2026-10-23", "p3", "m", 0.6); err != nil {
		t.Fatalf("settling at a lower cost frees the difference: %v", err)
	}
	now = now.Add(24 * time.Hour)
	if _, err := b.Reserve(ctx, "2026-10-24", "p4", "m", 0.8); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("monthly cap not enforced across days: %v", err)
	}
	now = time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC)
	if _, err := b.Reserve(ctx, "2026-11-02", "p5", "m", 0.8); err != nil {
		t.Fatalf("a new month starts a new allowance: %v", err)
	}
}

func TestReleasedAttemptsAreFreeAndUncertainOnesStayCharged(t *testing.T) {
	b := &Budget{Path: filepath.Join(t.TempDir(), "budget.json"), DailyUSD: 1, MonthlyUSD: 10}
	ctx := context.Background()
	a1, _ := b.Reserve(ctx, "s", "p1", "m", 0.4)
	b.Dispatch(ctx, a1.ID)
	b.Release(ctx, a1.ID, "HTTP 529")
	a2, _ := b.Reserve(ctx, "s", "p2", "m", 0.4)
	b.Dispatch(ctx, a2.ID)
	b.Uncertain(ctx, a2.ID, "connection reset")
	s, err := b.Summary(ctx)
	if err != nil || s.TodayUSD < 0.39 || s.TodayUSD > 0.41 || s.UnresolvedUSD < 0.39 || len(s.Open) != 1 {
		t.Fatalf("summary: %+v %v", s, err)
	}
	if err := b.Resolve(ctx, a2.ID, 0.05, "checked provider usage"); err != nil {
		t.Fatal(err)
	}
	if s, _ := b.Summary(ctx); s.UnresolvedUSD != 0 || s.TodayUSD > 0.051 {
		t.Fatalf("resolution not applied: %+v", s)
	}
	if err := b.Resolve(ctx, a2.ID, 0.05, "again"); err == nil {
		t.Fatal("a resolved attempt cannot be resolved twice")
	}
}

func TestDamagedJournalNeverResetsSpending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.json")
	b := &Budget{Path: path, DailyUSD: 1, MonthlyUSD: 1}
	for _, content := range []string{"{}", `{"attempts":[]}`, "not json"} {
		os.WriteFile(path, []byte(content), 0o600)
		if _, err := b.Reserve(context.Background(), "s", "p", "m", 0.1); err == nil || errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("journal %q must be rejected, got %v", content, err)
		}
	}
}

func TestConcurrentReservationsRespectTheCap(t *testing.T) {
	b := &Budget{Path: filepath.Join(t.TempDir(), "budget.json"), DailyUSD: 5, MonthlyUSD: 100}
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := b.Reserve(ctx, "s", string(rune('a'+i)), "m", 1); err == nil {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if granted != 5 {
		t.Fatalf("exactly five $1 reservations fit a $5 cap, got %d", granted)
	}
}
