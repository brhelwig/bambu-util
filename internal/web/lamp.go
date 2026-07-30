package web

import (
	"sync"
	"time"

	"github.com/brhelwig/bambu-util/internal/deadlines"
)

// LampInactiveOffAfter is how long the chamber lamp stays on after the
// printer goes inactive before automation forces it off. See
// Server.EnforceLampAutomation.
const LampInactiveOffAfter = 8 * time.Hour

// lampAuto decides the chamber lamp's automated state from whether the
// printer is "active" (a job running, or the bed/nozzle commanded hot).
// The moment it becomes active, the lamp is forced on once — a manual
// toggle-off afterward, during the same active stretch, sticks; automation
// won't fight it again until the next inactive->active transition. The
// moment it becomes inactive, an 8h countdown arms; when it elapses, the
// lamp is forced off exactly once — same "fires once" idiom as autoOff's
// heater safety shutoff.
type lampAuto struct {
	mu          sync.Mutex
	now         func() time.Time
	timers      timers
	hasObserved bool // false until the first poll — see poll's "first" handling
	wasActive   bool
	offAt       time.Time // zero = no pending forced-off
}

// newLampAuto resumes a pending forced-off from before the last restart, so an
// update part-way through the 8h grace period does not start it over.
func newLampAuto(store timerStore) *lampAuto {
	l := &lampAuto{now: time.Now, timers: timers{store: store}}
	if at, ok := l.timers.load()[deadlines.LampOff]; ok {
		l.offAt = at
		// A pending forced-off means the printer was idle when it was armed,
		// which is also what the first poll would have concluded. Saying so
		// here keeps that poll from re-arming and losing the elapsed time.
		l.hasObserved = true
	}
	return l
}

// poll reports what the lamp should do this tick. forceOn is true exactly
// once, on the inactive->active transition. forceOff is true exactly once,
// the tick the 8h grace period elapses.
//
// The very first call is always treated as a transition — whichever state
// it observes, active or inactive — so a process restart mid-print forces
// the lamp on immediately instead of waiting for the next real transition,
// and a restart while idle arms the off-countdown immediately instead of
// assuming the lamp is already correctly off.
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
		l.offAt = l.now().Add(LampInactiveOffAfter)
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
