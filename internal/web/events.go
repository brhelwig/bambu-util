package web

import (
	"fmt"
	"sync"
	"time"

	"github.com/brhelwig/bambu-util/internal/deadlines"
	"github.com/brhelwig/bambu-util/internal/p1s"
	"github.com/brhelwig/bambu-util/internal/push"
)

// BedOnReminders are how long the bed target can be above zero with no print
// running before each reminder goes out. The last one only reaches a bed this
// app did not heat: heat set here is shut off at 24h (BedOffAfter), so by then
// the target is zero and the reminder is skipped on its own.
var BedOnReminders = []time.Duration{time.Hour, 8 * time.Hour, 16 * time.Hour, 24 * time.Hour}

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
	nextAt   time.Time // zero when no reminder is still to come
}

// newPrintEvents resumes the bed reminder clock from before the last restart,
// so a bed on for eight hours across an update is still reported as eight, not
// counted again from zero.
func newPrintEvents(store timerStore) *printEvents {
	e := &printEvents{now: time.Now, timers: timers{store: store}, hmsSeen: map[string]bool{}}
	pending := e.timers.load()
	e.onSince = pending[deadlines.BedOnSince]
	e.nextAt = pending[deadlines.BedOnNext]
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
	out = append(out, e.bedOn(busy, bedTarget)...)
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
		return []push.Notification{{Title: "Print started", Body: name, Tag: tagJob}}
	case !busy && e.wasBusy && gs == "FINISH":
		return []push.Notification{{Title: "Print finished", Body: name, Tag: tagJob}}
	case !busy && e.wasBusy && gs == "FAILED":
		// The printer reports the same state whether it gave up or someone
		// pressed Stop, so the wording must not claim to know which.
		return []push.Notification{{Title: "Print ended without finishing", Body: name, Tag: tagJob}}
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
			out = append(out, push.Notification{Title: "Printer error", Body: entry.Message, Tag: tagError})
		}
	}
	// Forgetting alerts that have cleared is what lets the same fault announce
	// itself again if it comes back.
	e.hmsSeen = current
	return out
}

// bedOn reports how long the bed target has been above zero with no print
// running. What is stored is when the bed came on and when the next reminder is
// due, rather than which reminders have already gone out, so a restart neither
// repeats them nor starts the count again.
func (e *printEvents) bedOn(busy bool, bedTarget float64) []push.Notification {
	if bedTarget <= 0 || busy {
		if !e.onSince.IsZero() || !e.nextAt.IsZero() {
			e.onSince, e.nextAt = time.Time{}, time.Time{}
			e.timers.clear(deadlines.BedOnSince)
			e.timers.clear(deadlines.BedOnNext)
		}
		return nil
	}
	if e.onSince.IsZero() {
		e.onSince = e.now()
		e.nextAt = e.onSince.Add(BedOnReminders[0])
		e.timers.set(deadlines.BedOnSince, e.onSince)
		e.timers.set(deadlines.BedOnNext, e.nextAt)
	}
	if e.nextAt.IsZero() || e.now().Before(e.nextAt) {
		return nil
	}

	// Which reminder this is comes from the elapsed time rather than from the
	// due time, so a stored value rounded to the second cannot land just short
	// of its mark and report the wrong number of hours.
	elapsed := e.now().Sub(e.onSince)
	reached := BedOnReminders[0]
	for _, after := range BedOnReminders {
		if elapsed >= after {
			reached = after
		}
	}
	e.nextAt = time.Time{}
	for _, after := range BedOnReminders {
		if after > reached {
			e.nextAt = e.onSince.Add(after)
			break
		}
	}
	if e.nextAt.IsZero() {
		e.timers.clear(deadlines.BedOnNext)
	} else {
		e.timers.set(deadlines.BedOnNext, e.nextAt)
	}
	return []push.Notification{{
		Title: fmt.Sprintf("Bed on for %s", roundHours(reached)),
		Body:  fmt.Sprintf("Holding %.0f°C.", bedTarget),
		Tag:   tagBed,
	}}
}

func roundHours(d time.Duration) string {
	h := int(d.Hours())
	if h == 1 {
		return "1 hour"
	}
	return fmt.Sprintf("%d hours", h)
}
