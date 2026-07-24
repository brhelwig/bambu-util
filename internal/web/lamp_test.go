package web

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/brhelwig/bambu-util/internal/p1s"
)

func TestLampAutoForcesOnWhileActive(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newLampAuto()
	l.now = fixedClock(&now)

	on, off := l.poll(true)
	if !on || off {
		t.Fatalf("poll(true) = (%v, %v), want (true, false)", on, off)
	}
	if r := l.remaining(); r != -1 {
		t.Fatalf("remaining = %d while active, want -1", r)
	}
}

func TestLampAutoArmsOnTransitionToInactive(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newLampAuto()
	l.now = fixedClock(&now)

	l.poll(true)
	on, off := l.poll(false)
	if on || off {
		t.Fatalf("poll(false) right after going inactive = (%v, %v), want (false, false)", on, off)
	}
	want := int(LampInactiveOffAfter.Seconds())
	if r := l.remaining(); r != want {
		t.Fatalf("remaining = %d, want %d", r, want)
	}

	// A later tick while still inactive must not push the deadline out
	// further — it should count down, not reset.
	now = now.Add(time.Hour)
	l.poll(false)
	want -= int(time.Hour.Seconds())
	if r := l.remaining(); r != want {
		t.Fatalf("remaining after 1h = %d, want %d (deadline was re-armed)", r, want)
	}
}

func TestLampAutoFiresOnceAfterGracePeriod(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newLampAuto()
	l.now = fixedClock(&now)

	l.poll(true)
	l.poll(false)
	now = now.Add(LampInactiveOffAfter + time.Second)

	on, off := l.poll(false)
	if on || !off {
		t.Fatalf("poll after grace period = (%v, %v), want (false, true)", on, off)
	}
	on, off = l.poll(false)
	if on || off {
		t.Fatalf("poll fired twice: second call = (%v, %v), want (false, false)", on, off)
	}
	if r := l.remaining(); r != -1 {
		t.Fatalf("remaining after firing = %d, want -1", r)
	}
}

func TestLampAutoCancelledByReactivation(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newLampAuto()
	l.now = fixedClock(&now)

	l.poll(true)
	l.poll(false)
	now = now.Add(2 * time.Hour) // partway through the 8h window

	l.poll(true) // job/heater becomes active again
	if r := l.remaining(); r != -1 {
		t.Fatalf("remaining after reactivation = %d, want -1", r)
	}

	// Going inactive again arms a fresh window from *this* point, not the
	// original one.
	l.poll(false)
	want := int(LampInactiveOffAfter.Seconds())
	if r := l.remaining(); r != want {
		t.Fatalf("remaining after re-arming = %d, want %d", r, want)
	}
}

func TestPollLampForcesOnWhenJobRunning(t *testing.T) {
	cache := p1s.NewStateCache()
	cache.SetConnected(true)
	cache.Merge(map[string]any{"gcode_state": "RUNNING"})
	cmd := &fakeCommander{}
	store := openTestStore()
	defer store.Close()
	s := NewServer(cache, cmd, store)

	s.pollLamp()

	if len(cmd.calls) != 1 || cmd.calls[0] != "lamp-on" {
		t.Fatalf("calls = %v, want [lamp-on]", cmd.calls)
	}
}

func TestPollLampDoesNothingWhenDisconnected(t *testing.T) {
	cache := p1s.NewStateCache()
	cache.SetConnected(false)
	cmd := &fakeCommander{}
	store := openTestStore()
	defer store.Close()
	s := NewServer(cache, cmd, store)

	s.pollLamp()

	if len(cmd.calls) != 0 {
		t.Fatalf("calls = %v, want none", cmd.calls)
	}
}

func TestStatusExposesLampOffCountdown(t *testing.T) {
	cache := p1s.NewStateCache()
	cache.SetConnected(true)
	cache.Merge(map[string]any{"gcode_state": "IDLE"})
	cmd := &fakeCommander{}
	store := openTestStore()
	defer store.Close()
	s := NewServer(cache, cmd, store)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	var status map[string]any
	resp, _ := ts.Client().Get(ts.URL + "/api/status")
	json.NewDecoder(resp.Body).Decode(&status)
	if status["lampOffIn"] != nil {
		t.Fatalf("lampOffIn = %v before any poll, want nil", status["lampOffIn"])
	}

	s.pollLamp() // idle server: lampAuto starts wasActive=true, so this arms the 8h countdown

	resp2, _ := ts.Client().Get(ts.URL + "/api/status")
	json.NewDecoder(resp2.Body).Decode(&status)
	lampOff, ok := status["lampOffIn"].(float64)
	if !ok || lampOff > LampInactiveOffAfter.Seconds() || lampOff < LampInactiveOffAfter.Seconds()-60 {
		t.Fatalf("lampOffIn = %v, want ~%v", status["lampOffIn"], LampInactiveOffAfter.Seconds())
	}
}
