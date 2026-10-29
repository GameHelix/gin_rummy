package radar

import "sync"

// Log keeps the latest alerts in memory, numbered, so a stream can resume
// where it left off and a page can show the recent ones. It holds the
// last thousand; the radar's alerts are news, not records.
type Log struct {
	mu      sync.Mutex
	seq     int64
	entries []Entry
	subs    map[chan struct{}]bool
}

// Entry is an alert with its number in the log.
type Entry struct {
	Seq int64
	Alert
}

const keep = 1000

// Add numbers an alert, keeps it, and wakes the streams.
func (l *Log) Add(a Alert) Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	e := Entry{Seq: l.seq, Alert: a}
	l.entries = append(l.entries, e)
	if len(l.entries) > keep {
		l.entries = l.entries[len(l.entries)-keep:]
	}
	for ch := range l.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return e
}

// After returns up to limit entries numbered above seq, oldest first.
func (l *Log) After(seq int64, limit int) []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	i := 0
	for i < len(l.entries) && l.entries[i].Seq <= seq {
		i++
	}
	out := l.entries[i:]
	if len(out) > limit {
		out = out[:limit]
	}
	return append([]Entry(nil), out...)
}

// Last returns the newest n entries, oldest first.
func (l *Log) Last(n int) []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.entries
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return append([]Entry(nil), out...)
}

// Subscribe returns a channel that gets a value when an alert is added,
// and a function to stop.
func (l *Log) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.subs == nil {
		l.subs = map[chan struct{}]bool{}
	}
	l.subs[ch] = true
	return ch, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		delete(l.subs, ch)
	}
}
