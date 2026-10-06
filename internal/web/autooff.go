package web

import (
	"sync"
	"time"

	"github.com/brhelwig/bambu-util/internal/deadlines"
)

// autoOff turns the bed and nozzle off a set time after they were last set
// through this app. Server.pollAutoOff enforces it, and only while the printer
// is idle. Setting a heater again, including to off, resets its timer.
type autoOff struct {
	mu       sync.Mutex
	now      func() time.Time
	settings current
	timers   timers
	bedAt    time.Time // zero = inactive
	nozAt    time.Time
}

// newAutoOff resumes the countdowns pending when the process last stopped. One
// that fell due while it was down fires on the first poll.
func newAutoOff(store timerStore, cur current) *autoOff {
	a := &autoOff{now: time.Now, settings: cur, timers: timers{store: store}}
	pending := a.timers.load()
	a.bedAt = pending[deadlines.BedOff]
	a.nozAt = pending[deadlines.NozzleOff]
	return a
}

func (a *autoOff) setBed(temp int) {
	a.set(&a.bedAt, deadlines.BedOff, temp, a.settings().BedOffAfter)
}

func (a *autoOff) setNozzle(temp int) {
	a.set(&a.nozAt, deadlines.NozzleOff, temp, a.settings().NozzleOffAfter)
}

func (a *autoOff) set(at *time.Time, name string, temp int, window time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if temp > 0 {
		*at = a.now().Add(window)
	} else {
		*at = time.Time{}
	}
	a.timers.set(name, *at)
}

// due reports which heaters have reached their deadline, clearing them so each
// fires exactly once.
func (a *autoOff) due() (bed, nozzle bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t := a.now()
	if !a.bedAt.IsZero() && !t.Before(a.bedAt) {
		bed = true
		a.bedAt = time.Time{}
		a.timers.clear(deadlines.BedOff)
	}
	if !a.nozAt.IsZero() && !t.Before(a.nozAt) {
		nozzle = true
		a.nozAt = time.Time{}
		a.timers.clear(deadlines.NozzleOff)
	}
	return
}

// remaining returns whole seconds until each auto-off, or -1 when inactive.
func (a *autoOff) remaining() (bed, nozzle int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t := a.now()
	return secsUntil(t, a.bedAt), secsUntil(t, a.nozAt)
}

func secsUntil(now, at time.Time) int {
	if at.IsZero() {
		return -1
	}
	s := int(at.Sub(now).Round(time.Second).Seconds())
	if s < 0 {
		return 0
	}
	return s
}
