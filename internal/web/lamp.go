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
// While active the lamp is forced on every tick (self-healing against a
// manual toggle-off). The moment it goes inactive, an 8h countdown arms;
// when it elapses, the lamp is forced off exactly once — same "fires once"
// idiom as autoOff's heater safety shutoff — after which manual control is
// unfought until the printer goes active again.
type lampAuto struct {
	mu        sync.Mutex
	now       func() time.Time
	wasActive bool      // starts true: an inactive printer at boot arms immediately
	offAt     time.Time // zero = no pending forced-off
}

func newLampAuto() *lampAuto { return &lampAuto{now: time.Now, wasActive: true} }

// poll reports what the lamp should do this tick. forceOn is true on every
// active tick. forceOff is true exactly once, the tick the 8h grace period
// elapses.
func (l *lampAuto) poll(active bool) (forceOn, forceOff bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if active {
		l.offAt = time.Time{}
		l.wasActive = true
		return true, false
	}
	if l.wasActive {
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
