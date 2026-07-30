package web

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/brhelwig/bambu-util/internal/activity"
	"github.com/brhelwig/bambu-util/internal/p1s"
	"github.com/brhelwig/bambu-util/internal/settings"
)

func fixedClock(t *time.Time) func() time.Time {
	return func() time.Time { return *t }
}

func TestAutoOffFiresAfterWindow(t *testing.T) {
	now := time.Unix(1000, 0)
	a := newAutoOff(nil, testSettings)
	a.now = fixedClock(&now)

	a.setBed(60)
	if bed, _ := a.remaining(); bed != int(settings.Defaults.BedOffAfter.Seconds()) {
		t.Fatalf("remaining = %d, want %d", bed, int(settings.Defaults.BedOffAfter.Seconds()))
	}
	if bed, _ := a.due(); bed {
		t.Fatal("fired before the window elapsed")
	}

	now = now.Add(settings.Defaults.BedOffAfter + time.Second)
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
	a := newAutoOff(nil, testSettings)
	a.now = fixedClock(&now)

	a.setNozzle(220)
	now = now.Add(10 * time.Minute) // partway through the 15m window
	if _, nozzle := a.remaining(); nozzle != int((5 * time.Minute).Seconds()) {
		t.Fatalf("remaining = %d, want %d", nozzle, int((5 * time.Minute).Seconds()))
	}
	a.setNozzle(250) // adjusting resets the full window
	if _, nozzle := a.remaining(); nozzle != int(settings.Defaults.NozzleOffAfter.Seconds()) {
		t.Fatalf("remaining after reset = %d, want %d", nozzle, int(settings.Defaults.NozzleOffAfter.Seconds()))
	}
}

func TestAutoOffCancelledByZero(t *testing.T) {
	now := time.Unix(0, 0)
	a := newAutoOff(nil, testSettings)
	a.now = fixedClock(&now)

	a.setBed(90)
	a.setBed(0) // manual heater-off cancels the timer
	if bed, _ := a.remaining(); bed != -1 {
		t.Fatalf("remaining = %d after off, want -1", bed)
	}
	now = now.Add(settings.Defaults.BedOffAfter + time.Hour)
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
	if !ok || bedOff > settings.Defaults.BedOffAfter.Seconds() || bedOff < settings.Defaults.BedOffAfter.Seconds()-60 {
		t.Fatalf("bedOffIn = %v, want ~%v", s["bedOffIn"], settings.Defaults.BedOffAfter.Seconds())
	}
}

// autoOffServer builds a server whose heater deadlines have already passed, so
// the next poll would shut them down if the printer state allowed it.
func autoOffServer(t *testing.T, connected bool, state string) (*Server, *fakeCommander) {
	t.Helper()
	cache := p1s.NewStateCache()
	cache.SetConnected(connected)
	cache.Merge(map[string]any{"gcode_state": state})
	cmd := &fakeCommander{}
	s := NewServer(cache, cmd, openTestStore(), openTestNotifier(), nil, testSettings, nil, testPrinter(), activity.New(50))

	now := time.Unix(1000, 0)
	s.autoOff.now = fixedClock(&now)
	s.autoOff.setBed(60)
	s.autoOff.setNozzle(220)
	now = now.Add(settings.Defaults.BedOffAfter + time.Hour)
	return s, cmd
}

// Cutting the heaters mid-print ruins the print, so the shut-off must not act
// while the printer is running one.
func TestAutoOffDoesNotCutHeatersWhilePrinting(t *testing.T) {
	for _, state := range []string{"RUNNING", "PAUSE", "PREPARE"} {
		t.Run(state, func(t *testing.T) {
			s, cmd := autoOffServer(t, true, state)
			s.pollAutoOff()
			if len(cmd.calls) != 0 {
				t.Fatalf("commanded the printer during %s: %v", state, cmd.calls)
			}
		})
	}
}

// Skipping must not consume the deadline: once the print ends the heaters still
// have to go off, or a skipped shut-off is a shut-off lost for good.
func TestAutoOffStillFiresOnceThePrintIsOver(t *testing.T) {
	cache := p1s.NewStateCache()
	cache.SetConnected(true)
	cache.Merge(map[string]any{"gcode_state": "RUNNING"})
	cmd := &fakeCommander{}
	s := NewServer(cache, cmd, openTestStore(), openTestNotifier(), nil, testSettings, nil, testPrinter(), activity.New(50))

	now := time.Unix(1000, 0)
	s.autoOff.now = fixedClock(&now)
	s.autoOff.setBed(60)
	s.autoOff.setNozzle(220)
	now = now.Add(settings.Defaults.BedOffAfter + time.Hour)

	for range 3 {
		s.pollAutoOff()
	}
	if len(cmd.calls) != 0 {
		t.Fatalf("commanded the printer mid-print: %v", cmd.calls)
	}
	if bed, nozzle := s.autoOff.remaining(); bed != 0 || nozzle != 0 {
		t.Fatalf("deadlines were consumed while printing: bed=%d nozzle=%d", bed, nozzle)
	}

	cache.Merge(map[string]any{"gcode_state": "FINISH"})
	s.pollAutoOff()
	if len(cmd.bedTemps) != 1 || cmd.bedTemps[0] != 0 {
		t.Errorf("bed was not shut off once idle: %v", cmd.bedTemps)
	}
	if len(cmd.nozzleTemps) != 1 || cmd.nozzleTemps[0] != 0 {
		t.Errorf("nozzle was not shut off once idle: %v", cmd.nozzleTemps)
	}
}

// A dropped connection must not consume the deadline either — the command
// cannot be delivered, so it has to stay pending.
func TestAutoOffWaitsForTheConnectionToComeBack(t *testing.T) {
	s, cmd := autoOffServer(t, false, "IDLE")
	s.pollAutoOff()
	if len(cmd.calls) != 0 {
		t.Fatalf("commanded a disconnected printer: %v", cmd.calls)
	}
	s.cache.SetConnected(true)
	s.pollAutoOff()
	if len(cmd.bedTemps) != 1 || len(cmd.nozzleTemps) != 1 {
		t.Errorf("did not shut down after reconnecting: bed=%v nozzle=%v", cmd.bedTemps, cmd.nozzleTemps)
	}
}

func TestAutoOffFiresWhenIdle(t *testing.T) {
	for _, state := range []string{"IDLE", "FINISH", "FAILED"} {
		t.Run(state, func(t *testing.T) {
			s, cmd := autoOffServer(t, true, state)
			s.pollAutoOff()
			if len(cmd.bedTemps) != 1 || cmd.bedTemps[0] != 0 {
				t.Errorf("bed = %v, want one shut-off", cmd.bedTemps)
			}
			if len(cmd.nozzleTemps) != 1 || cmd.nozzleTemps[0] != 0 {
				t.Errorf("nozzle = %v, want one shut-off", cmd.nozzleTemps)
			}
		})
	}
}
