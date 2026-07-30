package web

import (
	"sync"
	"time"

	"github.com/brhelwig/bambu-util/internal/deadlines"
)

// Heaters left on unattended waste power and are a mild fire risk, so the bed
// and nozzle are shut off automatically some time after they were last set
// through this app. Enforcement is server-side (see Server.pollAutoOff) so it
// still fires when no browser is open, and it waits for the printer to be idle
// so it can never cut the heat out from under a print. Adjusting a heater —
// including turning it off — resets its timer.
const (
	BedOffAfter    = 24 * time.Hour
	NozzleOffAfter = 15 * time.Minute
)

type autoOff struct {
	mu     sync.Mutex
	now    func() time.Time
	timers timers
	bedAt  time.Time // zero = inactive
	nozAt  time.Time
}

// newAutoOff resumes whatever countdowns were pending when the process last
// stopped. One that came due while it was down is left in the past, so it fires
// on the first poll rather than being written off as stale — a shut-off missed
// because of a restart is the whole reason these are stored.
func newAutoOff(store timerStore) *autoOff {
	a := &autoOff{now: time.Now, timers: timers{store: store}}
	pending := a.timers.load()
	a.bedAt = pending[deadlines.BedOff]
	a.nozAt = pending[deadlines.NozzleOff]
	return a
}

func (a *autoOff) setBed(temp int) { a.set(&a.bedAt, deadlines.BedOff, temp, BedOffAfter) }
func (a *autoOff) setNozzle(temp int) {
	a.set(&a.nozAt, deadlines.NozzleOff, temp, NozzleOffAfter)
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
