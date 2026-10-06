package web

import (
	"log"
	"time"
)

// timerStore persists the countdowns. Write failures are logged and ignored:
// the countdown still runs, it just won't survive a restart.
type timerStore interface {
	Set(name string, at time.Time) error
	Clear(name string) error
	All() (map[string]time.Time, error)
}

// timers wraps a store so a nil one is a no-op.
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
		log.Printf("timers: recording %s: %v", name, err)
	}
}

func (t timers) clear(name string) {
	if t.store == nil {
		return
	}
	if err := t.store.Clear(name); err != nil {
		log.Printf("timers: clearing %s: %v", name, err)
	}
}

// load returns the pending timers, or nothing if they can't be read.
func (t timers) load() map[string]time.Time {
	if t.store == nil {
		return nil
	}
	pending, err := t.store.All()
	if err != nil {
		log.Printf("timers: reading pending timers: %v", err)
		return nil
	}
	return pending
}
