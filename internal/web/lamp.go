package web

import (
	"sync"
	"time"
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
	hasObserved bool      // false until the first poll — see poll's "first" handling
	wasActive   bool
	offAt       time.Time // zero = no pending forced-off
}

func newLampAuto() *lampAuto { return &lampAuto{now: time.Now} }

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
	}
	if !l.offAt.IsZero() && !l.now().Before(l.offAt) {
		l.offAt = time.Time{}
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
