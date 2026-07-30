package web

import (
	"fmt"
	"sync"
	"time"

	"github.com/brhelwig/bambu-util/internal/p1s"
	"github.com/brhelwig/bambu-util/internal/push"
)

// HotBedReminders are how long the bed can sit hot with no print running before
// each reminder goes out. The last one only reaches a bed this app did not heat:
// heat set through the app is shut off automatically at 24h (BedOffAfter), so by
// then the bed is cold and the reminder is skipped.
var HotBedReminders = []time.Duration{time.Hour, 8 * time.Hour, 16 * time.Hour, 24 * time.Hour}

// Notification tags. A second notification carrying the same tag replaces the
// first on the phone rather than stacking beneath it.
const (
	tagJob    = "job"
	tagError  = "error"
	tagHotBed = "hot-bed"
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
	hmsSeen  map[string]bool
	hotSince time.Time // zero while the bed is not sitting hot and unused
	sent     map[time.Duration]bool
}

func newPrintEvents() *printEvents {
	return &printEvents{now: time.Now, hmsSeen: map[string]bool{}, sent: map[time.Duration]bool{}}
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
	out = append(out, e.hotBed(busy, bedTarget, first)...)
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

func (e *printEvents) hotBed(busy bool, bedTarget float64, first bool) []push.Notification {
	hot := bedTarget > 0 && !busy
	if !hot {
		e.hotSince = time.Time{}
		clear(e.sent)
		return nil
	}
	if e.hotSince.IsZero() {
		// On the first observation the bed may already have been hot for hours,
		// which cannot be known — the clock starts now and the reminders are
		// late rather than invented.
		e.hotSince = e.now()
		clear(e.sent)
		if first {
			return nil
		}
	}
	elapsed := e.now().Sub(e.hotSince)
	for _, after := range HotBedReminders {
		if elapsed >= after && !e.sent[after] {
			e.sent[after] = true
			return []push.Notification{{
				Title: "Bed still hot",
				Body:  fmt.Sprintf("On for %s with no print running.", roundHours(after)),
				Tag:   tagHotBed,
			}}
		}
	}
	return nil
}

func roundHours(d time.Duration) string {
	h := int(d.Hours())
	if h == 1 {
		return "1 hour"
	}
	return fmt.Sprintf("%d hours", h)
}
