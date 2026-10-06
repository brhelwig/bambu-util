package web

import (
	"sync"
	"time"

	"github.com/brhelwig/bambu-util/internal/deadlines"
)

// lampAuto drives the chamber lamp from whether the printer is active (a job
// running, or a heater commanded hot). Becoming active forces the lamp on once;
// becoming inactive arms a countdown that forces it off once. Manual toggles in
// between are left alone.
type lampAuto struct {
	mu          sync.Mutex
	now         func() time.Time
	settings    current
	timers      timers
	hasObserved bool // false until the first poll — see poll's "first" handling
	wasActive   bool
	offAt       time.Time // zero = no pending forced-off
}

// newLampAuto resumes a forced-off pending from before the last restart.
func newLampAuto(store timerStore, cur current) *lampAuto {
	l := &lampAuto{now: time.Now, settings: cur, timers: timers{store: store}}
	if at, ok := l.timers.load()[deadlines.LampOff]; ok {
		l.offAt = at
		// A pending forced-off means the printer was idle when it was armed,
		// which is also what the first poll would have concluded. Saying so
		// here keeps that poll from re-arming and losing the elapsed time.
		l.hasObserved = true
	}
	return l
}

// poll reports what the lamp should do this tick: forceOn on the
// inactive->active transition, forceOff the tick the off delay elapses.
//
// The first call counts as a transition either way, so a restart mid-print
// turns the lamp on and a restart while idle arms the countdown.
func (l *lampAuto) poll(active bool) (forceOn, forceOff bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	first := !l.hasObserved
	l.hasObserved = true

	if active {
		if !l.offAt.IsZero() {
			l.timers.clear(deadlines.LampOff)
		}
		l.offAt = time.Time{}
		if first || !l.wasActive {
			l.wasActive = true
			return true, false
		}
		return false, false
	}
	if first || l.wasActive {
		l.offAt = l.now().Add(l.settings().LampOffAfter)
		l.wasActive = false
		l.timers.set(deadlines.LampOff, l.offAt)
	}
	if !l.offAt.IsZero() && !l.now().Before(l.offAt) {
		l.offAt = time.Time{}
		l.timers.clear(deadlines.LampOff)
		return false, true
	}
	return false, false
}

// remaining returns whole seconds until forced-off, or -1 if none pending.
func (l *lampAuto) remaining() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return secsUntil(l.now(), l.offAt)
}
