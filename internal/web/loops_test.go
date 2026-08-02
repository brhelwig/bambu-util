package web

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/brhelwig/bambu-util/internal/p1s"
)

// hasObserved reads the flag behind the lock that guards it, so a test can
// watch the events loop from outside the goroutine running it.
func (e *printEvents) hasObserved() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.observed
}

func loopServer(connected bool, fields map[string]any) (*Server, *fakeCommander) {
	cache := newStateCacheWith(connected, fields)
	cmd := &fakeCommander{}
	s := NewServer(cache, cmd, openTestStore(), openTestNotifier(), nil, testSettings, nil, testPrinter(), openTestLog())
	s.tick = time.Millisecond
	return s, cmd
}

// runLoop starts one background loop, waits for done to come true, then cancels
// and requires the loop to return. Both halves matter: that it does its work at
// all, and that it stops being asked to.
func runLoop(t *testing.T, loop func(context.Context), done func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		loop(ctx)
		close(stopped)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("the loop never did its work")
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop kept running after its context was cancelled")
	}
}

// Three loops that differ only in what they poll are exactly where a
// copy-paste sends one of them to the wrong place, so each is checked to drive
// its own work and nobody else's.
func TestEachLoopDrivesItsOwnPoll(t *testing.T) {
	t.Run("auto-off shuts the bed down", func(t *testing.T) {
		s, cmd := loopServer(true, map[string]any{"gcode_state": "IDLE"})
		s.autoOff.bedAt = time.Now().Add(-time.Minute)

		runLoop(t, s.EnforceAutoOff, func() bool {
			return slices.Contains(cmd.sent(), "bed-temp")
		})

		if got := cmd.sent(); slices.Contains(got, "lamp-on") || slices.Contains(got, "lamp-off") {
			t.Errorf("the heater loop touched the lamp: %v", got)
		}
		if s.events.hasObserved() {
			t.Error("the heater loop ran the event watcher")
		}
	})

	t.Run("lamp automation turns the lamp on", func(t *testing.T) {
		s, cmd := loopServer(true, map[string]any{
			"gcode_state": "IDLE", "bed_target_temper": 60.0,
		})

		runLoop(t, s.EnforceLampAutomation, func() bool {
			return slices.Contains(cmd.sent(), "lamp-on")
		})

		if got := cmd.sent(); slices.Contains(got, "bed-temp") || slices.Contains(got, "nozzle-temp") {
			t.Errorf("the lamp loop touched a heater: %v", got)
		}
		if s.events.hasObserved() {
			t.Error("the lamp loop ran the event watcher")
		}
	})

	t.Run("event notifications watch the printer", func(t *testing.T) {
		s, cmd := loopServer(true, map[string]any{"gcode_state": "IDLE"})

		runLoop(t, s.EnforceEventNotifications, func() bool {
			return s.events.hasObserved()
		})

		if got := cmd.sent(); len(got) != 0 {
			t.Errorf("the event loop sent commands to the printer: %v", got)
		}
	})
}

// A loop that ignores cancellation outlives the process it belongs to, so the
// shared helper is checked on its own as well as through the three above.
func TestALoopStopsWhenCancelled(t *testing.T) {
	s, _ := loopServer(true, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stopped := make(chan struct{})
	go func() {
		s.every(ctx, func() {})
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("a loop given an already-cancelled context never returned")
	}
}

func TestLoopsFallBackToTheDefaultInterval(t *testing.T) {
	s, _ := loopServer(true, nil)
	s.tick = 0

	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	polls := make(chan struct{}, 1)
	s.every(ctx, func() {
		select {
		case polls <- struct{}{}:
		default:
		}
	})

	if len(polls) != 0 {
		t.Errorf("polled within %s, want the %s default", time.Since(started), defaultTick)
	}
}

func newStateCacheWith(connected bool, fields map[string]any) *p1s.StateCache {
	cache := p1s.NewStateCache()
	cache.SetConnected(connected)
	if len(fields) > 0 {
		cache.Merge(fields)
	}
	return cache
}
