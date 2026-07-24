package web

import (
	"testing"
	"time"
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
