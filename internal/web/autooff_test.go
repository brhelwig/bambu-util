package web

import (
	"encoding/json"
	"testing"
	"time"
)

func fixedClock(t *time.Time) func() time.Time {
	return func() time.Time { return *t }
}

func TestAutoOffFiresAfterWindow(t *testing.T) {
	now := time.Unix(1000, 0)
	a := newAutoOff()
	a.now = fixedClock(&now)

	a.setBed(60)
	if bed, _ := a.remaining(); bed != int(BedOffAfter.Seconds()) {
		t.Fatalf("remaining = %d, want %d", bed, int(BedOffAfter.Seconds()))
	}
	if bed, _ := a.due(); bed {
		t.Fatal("fired before the window elapsed")
	}

	now = now.Add(BedOffAfter + time.Second)
	bed, _ := a.due()
	if !bed {
		t.Fatal("did not fire after the window elapsed")
	}
	// Fires exactly once.
	if bed, _ := a.due(); bed {
		t.Fatal("fired twice")
	}
	if bed, _ := a.remaining(); bed != -1 {
		t.Fatalf("remaining = %d after firing, want -1", bed)
	}
}

func TestAutoOffResetsOnAdjust(t *testing.T) {
	now := time.Unix(0, 0)
	a := newAutoOff()
	a.now = fixedClock(&now)

	a.setNozzle(220)
	now = now.Add(10 * time.Minute) // partway through the 15m window
	if _, nozzle := a.remaining(); nozzle != int((5 * time.Minute).Seconds()) {
		t.Fatalf("remaining = %d, want %d", nozzle, int((5 * time.Minute).Seconds()))
	}
	a.setNozzle(250) // adjusting resets the full window
	if _, nozzle := a.remaining(); nozzle != int(NozzleOffAfter.Seconds()) {
		t.Fatalf("remaining after reset = %d, want %d", nozzle, int(NozzleOffAfter.Seconds()))
	}
}

func TestAutoOffCancelledByZero(t *testing.T) {
	now := time.Unix(0, 0)
	a := newAutoOff()
	a.now = fixedClock(&now)

	a.setBed(90)
	a.setBed(0) // manual heater-off cancels the timer
	if bed, _ := a.remaining(); bed != -1 {
		t.Fatalf("remaining = %d after off, want -1", bed)
	}
	now = now.Add(BedOffAfter + time.Hour)
	if bed, _ := a.due(); bed {
		t.Fatal("cancelled timer still fired")
	}
}

func TestStatusExposesAutoOffCountdown(t *testing.T) {
	ts, _ := newTestServer(true, "IDLE")
	defer ts.Close()

	// No timer armed yet → null.
	var s map[string]any
	resp, _ := ts.Client().Get(ts.URL + "/api/status")
	json.NewDecoder(resp.Body).Decode(&s)
	if s["bedOffIn"] != nil {
		t.Fatalf("bedOffIn = %v, want nil before any set", s["bedOffIn"])
	}

	// Arm the bed timer, then the countdown should be present and near 24h.
	ts.Client().Post(ts.URL+"/api/actions/set-bed-temp?temp=60", "", nil)
	resp2, _ := ts.Client().Get(ts.URL + "/api/status")
	json.NewDecoder(resp2.Body).Decode(&s)
	bedOff, ok := s["bedOffIn"].(float64)
	if !ok || bedOff > BedOffAfter.Seconds() || bedOff < BedOffAfter.Seconds()-60 {
		t.Fatalf("bedOffIn = %v, want ~%v", s["bedOffIn"], BedOffAfter.Seconds())
	}
}
