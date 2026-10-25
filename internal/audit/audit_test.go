package audit

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func clock(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestAppendReopenAndVerify(t *testing.T) {
	dir := t.TempDir()
	tenant, agent := uuid.New(), uuid.New()
	now := time.Date(2026, 10, 25, 12, 0, 0, 123456789, time.UTC)
	l, err := Open(dir, tenant, agent, clock(now))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := l.Append("decision.started", map[string]any{"n": i, "note": "<b>&"}); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()
	l, err = Open(dir, tenant, agent, clock(now.Add(25*time.Hour))) // next UTC day: a new file
	if err != nil {
		t.Fatal(err)
	}
	e, err := l.Append("order.intent", map[string]string{"symbol": "BTCUSDT"})
	if err != nil || e.Seq != 4 || e.At.Nanosecond()%1000 != 0 {
		t.Fatalf("seq %d, err %v, at %v", e.Seq, err, e.At)
	}
	last, err := Verify(dir)
	if err != nil || last.Seq != 4 {
		t.Fatalf("verify: %v %d", err, last.Seq)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl")); len(files) != 2 {
		t.Fatalf("expected a file per day, got %v", files)
	}
	got, _ := ReadAfter(dir, 2, 10)
	if len(got) != 2 || got[0].Seq != 3 || got[1].Seq != 4 {
		t.Fatalf("read after 2: %+v", got)
	}
}

func TestTamperingIsDetected(t *testing.T) {
	dir := t.TempDir()
	l, _ := Open(dir, uuid.New(), uuid.New(), nil)
	l.Append("a", map[string]int{"amount": 10})
	l.Append("b", map[string]int{"amount": 20})
	l.Close()
	path := filepath.Join(dir, "000000000001.jsonl")
	raw, _ := os.ReadFile(path)
	os.WriteFile(path, []byte(string(raw[:len(raw)-1])[:0]+replace(string(raw), `"amount":10`, `"amount":99`)), 0o600)
	if _, err := Verify(dir); err == nil {
		t.Fatal("an edited event must break the chain")
	}
}

func replace(s, old, new string) string {
	for i := 0; i+len(old) <= len(s); i++ {
		if s[i:i+len(old)] == old {
			return s[:i] + new + s[i+len(old):]
		}
	}
	return s
}

func TestHalfWrittenLineIsDropped(t *testing.T) {
	dir := t.TempDir()
	tenant, agent := uuid.New(), uuid.New()
	l, _ := Open(dir, tenant, agent, nil)
	l.Append("a", 1)
	l.Close()
	f, _ := os.OpenFile(filepath.Join(dir, "000000000001.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"id":"broken`)
	f.Close()
	l, err := Open(dir, tenant, agent, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e, err := l.Append("b", 2); err != nil || e.Seq != 2 {
		t.Fatalf("append after repair: %+v %v", e, err)
	}
	if _, err := Verify(dir); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentAppendsStayInOrder(t *testing.T) {
	dir := t.TempDir()
	l, _ := Open(dir, uuid.New(), uuid.New(), nil)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); l.Append("x", i) }()
	}
	wg.Wait()
	if last, err := Verify(dir); err != nil || last.Seq != 50 {
		t.Fatalf("%v %d", err, last.Seq)
	}
}

func TestAnotherAgentsLogIsRefused(t *testing.T) {
	dir := t.TempDir()
	l, _ := Open(dir, uuid.New(), uuid.New(), nil)
	l.Append("a", 1)
	l.Close()
	if _, err := Open(dir, uuid.New(), uuid.New(), nil); err == nil {
		t.Fatal("a log must belong to one agent")
	}
}
