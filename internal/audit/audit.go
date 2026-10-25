// Package audit is an agent's append-only, hash-chained event log on local
// disk. The agent writes an event before it acts; a shipper later copies the
// log to Postgres. Every event's hash covers the one before it, so an edited,
// reordered or missing entry is detectable.
package audit

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type Event struct {
	ID     uuid.UUID       `json:"id"`
	Tenant uuid.UUID       `json:"tenant"`
	Agent  uuid.UUID       `json:"agent"`
	Seq    int64           `json:"seq"`
	Type   string          `json:"type"`
	At     time.Time       `json:"at"`
	Body   json.RawMessage `json:"body"`
	Prev   []byte          `json:"prev,omitempty"`
	Hash   []byte          `json:"hash"`
}

// Sum is the event's hash: SHA-256 over the previous hash and every field,
// each length-prefixed so no two events can encode alike.
func (e Event) Sum() []byte {
	h := sha256.New()
	field := func(b []byte) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	var seq [8]byte
	binary.BigEndian.PutUint64(seq[:], uint64(e.Seq))
	field(e.Prev)
	field(e.ID[:])
	field(e.Tenant[:])
	field(e.Agent[:])
	field(seq[:])
	field([]byte(e.Type))
	field([]byte(e.At.UTC().Format(time.RFC3339Nano)))
	field(e.Body)
	return h.Sum(nil)
}

// Follows reports whether e correctly continues a chain whose last event had
// seq and hash.
func (e Event) Follows(seq int64, hash []byte) bool {
	return e.Seq == seq+1 && bytes.Equal(e.Prev, hash) && bytes.Equal(e.Hash, e.Sum())
}

// Log appends events for one agent. Files are named by the first sequence
// number they hold and roll over each UTC day.
type Log struct {
	mu      sync.Mutex
	dir     string
	tenant  uuid.UUID
	agent   uuid.UUID
	now     func() time.Time
	seq     int64
	hash    []byte
	file    *os.File
	fileDay string
}

// Open continues the log in dir, dropping a final line a crash left half
// written: it was never acknowledged, so nothing relied on it.
func Open(dir string, tenant, agent uuid.UUID, now func() time.Time) (*Log, error) {
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	l := &Log{dir: dir, tenant: tenant, agent: agent, now: now}
	files, err := segments(dir)
	if err != nil {
		return nil, err
	}
	for i := len(files) - 1; i >= 0 && l.seq == 0; i-- {
		events, err := readSegment(files[i].path, true)
		if err != nil {
			return nil, err
		}
		if len(events) == 0 {
			if err := os.Remove(files[i].path); err != nil {
				return nil, err
			}
			continue
		}
		last := events[len(events)-1]
		if last.Tenant != tenant || last.Agent != agent {
			return nil, fmt.Errorf("%s belongs to another agent", dir)
		}
		l.seq, l.hash, l.fileDay = last.Seq, last.Hash, events[0].At.UTC().Format(time.DateOnly)
		f, err := os.OpenFile(files[i].path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		l.file = f
	}
	return l, nil
}

// Append records one event and returns once it is on disk.
func (l *Log) Append(typ string, payload any) (Event, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return Event{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	id, err := uuid.NewV7()
	if err != nil {
		return Event{}, err
	}
	e := Event{ID: id, Tenant: l.tenant, Agent: l.agent, Seq: l.seq + 1, Type: typ, At: l.now().UTC().Truncate(time.Microsecond), Body: body, Prev: l.hash}
	e.Hash = e.Sum()
	line, err := json.Marshal(e)
	if err != nil {
		return Event{}, err
	}
	if day := e.At.Format(time.DateOnly); l.file == nil || day != l.fileDay {
		if l.file != nil {
			l.file.Close()
		}
		f, err := os.OpenFile(filepath.Join(l.dir, fmt.Sprintf("%012d.jsonl", e.Seq)), os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return Event{}, err
		}
		l.file, l.fileDay = f, day
	}
	if _, err := l.file.Write(append(line, '\n')); err != nil {
		return Event{}, err
	}
	if err := l.file.Sync(); err != nil {
		return Event{}, err
	}
	l.seq, l.hash = e.Seq, e.Hash
	return e, nil
}

func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

type segment struct {
	path  string
	first int64
}

func segments(dir string) ([]segment, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []segment
	for _, en := range entries {
		name := en.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		first, err := strconv.ParseInt(strings.TrimSuffix(name, ".jsonl"), 10, 64)
		if err != nil {
			continue
		}
		out = append(out, segment{filepath.Join(dir, name), first})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].first < out[j].first })
	return out, nil
}

// readSegment parses a file. With repair, an unterminated final line is cut
// off the file; otherwise it is ignored.
func readSegment(path string, repair bool) ([]Event, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	complete := raw
	if i := bytes.LastIndexByte(raw, '\n'); i < len(raw)-1 {
		complete = raw[:i+1]
		if repair {
			if err := os.Truncate(path, int64(len(complete))); err != nil {
				return nil, err
			}
		}
	}
	var out []Event
	sc := bufio.NewScanner(bytes.NewReader(complete))
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("%s: corrupt event: %w", path, err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// ReadAfter returns up to limit events with a sequence number above seq, in
// order.
func ReadAfter(dir string, seq int64, limit int) ([]Event, error) {
	files, err := segments(dir)
	if err != nil {
		return nil, err
	}
	start := 0
	for i, f := range files {
		if f.first <= seq+1 {
			start = i
		}
	}
	var out []Event
	for _, f := range files[start:] {
		events, err := readSegment(f.path, false)
		if err != nil {
			return nil, err
		}
		for _, e := range events {
			if e.Seq > seq {
				out = append(out, e)
				if len(out) == limit {
					return out, nil
				}
			}
		}
	}
	return out, nil
}

// Verify checks the whole chain in dir and returns its last event.
func Verify(dir string) (Event, error) {
	var last Event
	for {
		batch, err := ReadAfter(dir, last.Seq, 1000)
		if err != nil {
			return last, err
		}
		if len(batch) == 0 {
			return last, nil
		}
		for _, e := range batch {
			if !e.Follows(last.Seq, last.Hash) {
				return last, fmt.Errorf("event %d does not follow event %d", e.Seq, last.Seq)
			}
			last = e
		}
	}
}

var _ io.Closer = (*Log)(nil)
