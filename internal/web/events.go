package web

import (
	"sync"
	"time"

	"github.com/brhelwig/bambu-util/internal/deadlines"
	"github.com/brhelwig/bambu-util/internal/p1s"
	"github.com/brhelwig/bambu-util/internal/push"
)

// Notification tags. A second notification carrying the same tag replaces the
// first on the phone rather than stacking beneath it.
const (
	tagJob   = "job"
	tagError = "error"
	tagBed   = "bed"
)

// printEvents turns the printer's reported state into the handful of changes
// worth interrupting someone for.
//
// Every decision is a transition, never a level, so nothing repeats itself: a
// print that stays finished is announced once. The first observation after
// start-up only records where things stand — announcing a print that finished
// hours ago because this process has just met the printer would be noise, and
// worse, indistinguishable from the real thing.
type printEvents struct {
	mu       sync.Mutex
	now      func() time.Time
	observed bool
	wasBusy  bool
	timers   timers
	hmsSeen  map[string]bool
	onSince  time.Time // zero while the bed is off, or a print is running
}

// newPrintEvents resumes the bed reminder clock from before the last restart,
// so a bed on for eight hours across an update is still reported as eight, not
// counted again from zero.
func newPrintEvents(store timerStore) *printEvents {
	e := &printEvents{now: time.Now, timers: timers{store: store}, hmsSeen: map[string]bool{}}
	pending := e.timers.load()
	e.onSince = pending[deadlines.BedOnSince]
	return e
}

// poll reports what should be sent this tick. A disconnected printer reports
// nothing rather than its last known state, so a dropped link cannot look like
// a print ending.
func (e *printEvents) poll(connected bool, gs, jobName string, bedTarget float64, hms []p1s.HMSEntry) []push.Notification {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !connected {
		return nil
	}

	busy := p1s.JobActive(gs)
	first := !e.observed
	e.observed = true

	var out []push.Notification
	if !first {
		out = append(out, e.jobChange(busy, gs, jobName)...)
	}
	out = append(out, e.newErrors(hms, first)...)
	e.trackBedOn(busy, bedTarget)
	e.wasBusy = busy
	return out
}

func (e *printEvents) jobChange(busy bool, gs, jobName string) []push.Notification {
	name := jobName
	if name == "" {
		name = "The print"
	}
	switch {
	case busy && !e.wasBusy:
		return []push.Notification{{Title: "Print started", Body: name, Tag: tagJob, Kind: push.KindPrintStarted}}
	case !busy && e.wasBusy && gs == "FINISH":
		return []push.Notification{{Title: "Print finished", Body: name, Tag: tagJob, Kind: push.KindPrintFinished}}
	case !busy && e.wasBusy && gs == "FAILED":
		// The printer reports the same state whether it gave up or someone
		// pressed Stop, so the wording must not claim to know which.
		return []push.Notification{{Title: "Print ended without finishing", Body: name, Tag: tagJob, Kind: push.KindPrintEnded}}
	}
	return nil
}

// newErrors announces alerts that were not raised a moment ago. Filament runout
// arrives this way rather than as a field of its own, so it needs no separate
// handling. On the first observation the alerts already standing are recorded
// silently — they are not news.
func (e *printEvents) newErrors(hms []p1s.HMSEntry, first bool) []push.Notification {
	current := make(map[string]bool, len(hms))
	var out []push.Notification
	for _, entry := range hms {
		current[entry.Code] = true
		if !e.hmsSeen[entry.Code] && !first {
			out = append(out, push.Notification{Title: "Printer error", Body: entry.Message, Tag: tagError, Kind: push.KindPrinterError})
		}
	}
	// Forgetting alerts that have cleared is what lets the same fault announce
	// itself again if it comes back.
	e.hmsSeen = current
	return out
}

// trackBedOn keeps note of when the bed came on with no print running. The
// reminders themselves are per device — each subscription asks for its own
// interval — so this only records the stretch they are measured from.
func (e *printEvents) trackBedOn(busy bool, bedTarget float64) {
	if bedTarget <= 0 || busy {
		if !e.onSince.IsZero() {
			e.onSince = time.Time{}
			e.timers.clear(deadlines.BedOnSince)
		}
		return
	}
	if e.onSince.IsZero() {
		// At start-up the bed may already have been on for hours, which cannot
		// be known — so the clock starts now and a reminder comes late rather
		// than invented.
		e.onSince = e.now()
		e.timers.set(deadlines.BedOnSince, e.onSince)
	}
}

// bedOnSince reports when the bed came on with no print running, or the zero
// time if it is not.
func (e *printEvents) bedOnSince() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.onSince
}
