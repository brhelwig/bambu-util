package activity

import (
	"sync"
	"sync/atomic"
	"time"
)

// Package activity records what the app did and what was done to it — commands
// sent to the printer, what the printer reported back, and notifications sent
// out — so a command can be seen leaving and being acknowledged rather than the
// page merely claiming it was sent.
//
// It is deliberately in memory and deliberately small: this is for looking at
// what just happened, not a history worth keeping across a restart.
type Log struct {
	mu      sync.Mutex
	entries []*Entry
	max     int
	nextID  atomic.Int64
	now     func() time.Time
}

// What kind of thing an entry records.
const (
	Command      = "command"      // sent to the printer
	Report       = "report"       // received from the printer
	Notification = "notification" // sent to subscribed devices
)

// Entry is one thing that happened. Acked is nil for anything not yet
// confirmed, which for a command means the printer has not answered — the
// difference this whole thing exists to show.
type Entry struct {
	ID      int64      `json:"id"`
	At      time.Time  `json:"at"`
	Kind    string     `json:"kind"`
	Summary string     `json:"summary"`
	Payload string     `json:"payload,omitempty"`
	Acked   *time.Time `json:"acked,omitempty"`
	Error   string     `json:"error,omitempty"`
}

// maxPayload caps how much of one payload is kept. The printer's first report
// after connecting is its entire state and dwarfs everything else; keeping all
// of it would push out the recent traffic this is for.
const maxPayload = 4096

// New returns a log holding at most max entries.
func New(max int) *Log {
	return &Log{max: max, now: time.Now}
}

// Record adds an entry and returns it, so a command can be marked acknowledged
// later. A nil log records nothing, so nothing has to check before calling.
func (a *Log) Record(kind, summary, payload string) *Entry {
	if a == nil {
		return nil
	}
	if len(payload) > maxPayload {
		payload = payload[:maxPayload] + "… (truncated)"
	}
	entry := &Entry{
		ID:      a.nextID.Add(1),
		At:      a.now(),
		Kind:    kind,
		Summary: summary,
		Payload: payload,
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, entry)
	if len(a.entries) > a.max {
		a.entries = a.entries[len(a.entries)-a.max:]
	}
	return entry
}

// Acknowledge marks when the printer's broker confirmed a message, or why it
// did not. A nil entry is ignored, so callers need not check.
func (a *Log) Acknowledge(entry *Entry, at time.Time, err error) {
	if a == nil || entry == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		entry.Error = err.Error()
		return
	}
	entry.Acked = &at
}

// Entries returns the messages held, newest last. The entries are copied, so
// reading them cannot race with a command being acknowledged.
func (a *Log) Entries() []Entry {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Entry, len(a.entries))
	for i, e := range a.entries {
		out[i] = *e
	}
	return out
}
