package web

import (
	"sync"
	"time"
)

// Heaters left on unattended waste power and are a mild fire risk, so the bed
// and nozzle are shut off automatically some time after they were last set
// through this app. Enforcement is server-side (see Server.EnforceAutoOff) so
// it still fires when no browser is open. Adjusting a heater — including
// turning it off — resets its timer.
const (
	BedOffAfter    = 24 * time.Hour
	NozzleOffAfter = 15 * time.Minute
)

type autoOff struct {
	mu    sync.Mutex
	now   func() time.Time
	bedAt time.Time // zero = inactive
	nozAt time.Time
}

func newAutoOff() *autoOff { return &autoOff{now: time.Now} }

func (a *autoOff) setBed(temp int)    { a.set(&a.bedAt, temp, BedOffAfter) }
func (a *autoOff) setNozzle(temp int) { a.set(&a.nozAt, temp, NozzleOffAfter) }

func (a *autoOff) set(at *time.Time, temp int, window time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if temp > 0 {
		*at = a.now().Add(window)
	} else {
		*at = time.Time{}
	}
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
	}
	if !a.nozAt.IsZero() && !t.Before(a.nozAt) {
		nozzle = true
		a.nozAt = time.Time{}
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
