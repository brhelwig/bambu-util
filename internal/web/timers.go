package web

import (
	"log"
	"time"
)

// timerStore is the persistence the in-memory timers need. A failure to write
// is logged and otherwise ignored: the countdown still runs, it just will not
// survive a restart, and refusing to arm it at all would be worse.
type timerStore interface {
	Set(name string, at time.Time) error
	Clear(name string) error
	All() (map[string]time.Time, error)
}

// timers wraps a store so a nil one — which is what tests that do not care
// about persistence pass — is simply a no-op.
type timers struct{ store timerStore }

func (t timers) set(name string, at time.Time) {
	if t.store == nil {
		return
	}
	if at.IsZero() {
		t.clear(name)
		return
	}
	if err := t.store.Set(name, at); err != nil {
		log.Printf("deadlines: recording %s: %v", name, err)
	}
}

func (t timers) clear(name string) {
	if t.store == nil {
		return
	}
	if err := t.store.Clear(name); err != nil {
		log.Printf("deadlines: clearing %s: %v", name, err)
	}
}

// load returns the pending timers, or nothing if they cannot be read. Starting
// with empty timers is the old behaviour, so a read failure loses the
// countdowns rather than the process.
func (t timers) load() map[string]time.Time {
	if t.store == nil {
		return nil
	}
	pending, err := t.store.All()
	if err != nil {
		log.Printf("deadlines: reading pending timers: %v", err)
		return nil
	}
	return pending
}
