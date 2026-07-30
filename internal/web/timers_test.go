package web

import (
	"fmt"
	"testing"
	"time"

	"github.com/brhelwig/bambu-util/internal/deadlines"
	"github.com/brhelwig/bambu-util/internal/p1s"
	"github.com/brhelwig/bambu-util/internal/settings"
)

func openTestTimers(t *testing.T) *deadlines.Store {
	t.Helper()
	store, err := deadlines.Open(":memory:")
	if err != nil {
		t.Fatalf("open timers: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// A restart is not a rare event here: the deployment pulls a new image by
// restarting, so anything held only in memory is lost on every update.
func TestAHeaterShutOffSurvivesARestart(t *testing.T) {
	store := openTestTimers(t)
	now := time.Unix(1_000_000, 0)

	before := newAutoOff(store, testSettings)
	before.now = fixedClock(&now)
	before.setBed(60)
	before.setNozzle(220)

	// The process stops and starts again an hour later.
	now = now.Add(time.Hour)
	after := newAutoOff(store, testSettings)
	after.now = fixedClock(&now)

	bed, nozzle := after.remaining()
	if want := int((settings.Defaults.BedOffAfter - time.Hour).Seconds()); bed != want {
		t.Errorf("bed countdown = %ds, want %ds — the hour before the restart should still count", bed, want)
	}
	// The nozzle's 15 minutes elapsed while the process was down.
	if nozzle != 0 {
		t.Errorf("nozzle countdown = %ds, want 0 — it came due while the process was down", nozzle)
	}
	if gotBed, gotNozzle := after.due(); gotBed || !gotNozzle {
		t.Errorf("due() = bed %v nozzle %v, want the nozzle to fire and the bed to wait", gotBed, gotNozzle)
	}
}

// A shut-off missed because of a restart is the whole reason these are stored,
// so one that lapsed while the process was down must still act.
func TestAShutOffThatLapsedWhileDownStillFires(t *testing.T) {
	store := openTestTimers(t)
	now := time.Unix(1_000_000, 0)

	before := newAutoOff(store, testSettings)
	before.now = fixedClock(&now)
	before.setBed(60)

	now = now.Add(settings.Defaults.BedOffAfter + 12*time.Hour) // down for a day and a half
	after := newAutoOff(store, testSettings)
	after.now = fixedClock(&now)
	if bed, _ := after.due(); !bed {
		t.Error("an overdue shut-off was written off as stale")
	}
}

func TestTurningAHeaterOffForgetsItsShutOff(t *testing.T) {
	store := openTestTimers(t)
	now := time.Unix(1_000_000, 0)

	before := newAutoOff(store, testSettings)
	before.now = fixedClock(&now)
	before.setBed(60)
	before.setBed(0)

	pending, err := store.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if at, ok := pending[deadlines.BedOff]; ok {
		t.Errorf("a shut-off is still pending at %v after the bed was turned off", at)
	}
	if bed, _ := newAutoOff(store, testSettings).remaining(); bed != -1 {
		t.Errorf("a restart resumed a shut-off that was cancelled: %ds", bed)
	}
}

func TestFiringAShutOffForgetsIt(t *testing.T) {
	store := openTestTimers(t)
	now := time.Unix(1_000_000, 0)

	a := newAutoOff(store, testSettings)
	a.now = fixedClock(&now)
	a.setBed(60)
	now = now.Add(settings.Defaults.BedOffAfter + time.Second)
	if bed, _ := a.due(); !bed {
		t.Fatal("did not fire")
	}

	if bed, _ := newAutoOff(store, testSettings).remaining(); bed != -1 {
		t.Errorf("a restart resurrected a shut-off that already fired: %ds", bed)
	}
}

func TestTheLampCountdownSurvivesARestart(t *testing.T) {
	store := openTestTimers(t)
	now := time.Unix(1_000_000, 0)

	before := newLampAuto(store, testSettings)
	before.now = fixedClock(&now)
	before.poll(true)  // active: lamp on
	before.poll(false) // idle: the countdown arms

	now = now.Add(6 * time.Hour)
	after := newLampAuto(store, testSettings)
	after.now = fixedClock(&now)
	if got, want := after.remaining(), int((settings.Defaults.LampOffAfter - 6*time.Hour).Seconds()); got != want {
		t.Errorf("lamp countdown = %ds, want %ds", got, want)
	}
	// Two more hours and it is due, rather than eight from the restart.
	now = now.Add(2*time.Hour + time.Second)
	if _, off := after.poll(false); !off {
		t.Error("the lamp countdown restarted instead of resuming")
	}
}

func TestTheBedOnClockSurvivesARestart(t *testing.T) {
	store := openTestTimers(t)
	now := time.Unix(1_000_000, 0)

	before := newPrintEvents(store)
	before.now = func() time.Time { return now }
	before.poll(true, "IDLE", "", 0, nil)
	before.poll(true, "IDLE", "", 60, nil) // the bed comes on
	started := before.bedOnSince()

	now = now.Add(90 * time.Minute)
	after := newPrintEvents(store)
	after.now = func() time.Time { return now }
	after.poll(true, "IDLE", "", 60, nil)
	if got := after.bedOnSince(); !got.Equal(started) {
		t.Errorf("after a restart the bed came on at %v, want %v — the count restarted", got, started)
	}
}

func TestTurningTheBedOffForgetsTheClock(t *testing.T) {
	store := openTestTimers(t)
	now := time.Unix(1_000_000, 0)

	e := newPrintEvents(store)
	e.now = func() time.Time { return now }
	e.poll(true, "IDLE", "", 0, nil)
	e.poll(true, "IDLE", "", 60, nil)
	e.poll(true, "IDLE", "", 0, nil) // bed off

	pending, err := store.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if at, ok := pending[deadlines.BedOnSince]; ok {
		t.Errorf("%s is still stored (%v) after the bed was turned off", deadlines.BedOnSince, at)
	}
}

// The countdowns are shown in the status card, so a restart must not blank them.
func TestTheStatusCountdownSurvivesARestart(t *testing.T) {
	store := openTestTimers(t)
	cache := p1s.NewStateCache()
	cache.SetConnected(true)
	cache.Merge(map[string]any{"gcode_state": "IDLE"})

	first := NewServer(cache, &fakeCommander{}, openTestStore(), openTestNotifier(), store, testSettings, nil, testPrinter())
	first.autoOff.setBed(60)

	second := NewServer(cache, &fakeCommander{}, openTestStore(), openTestNotifier(), store, testSettings, nil, testPrinter())
	bed, _ := second.autoOff.remaining()
	if bed <= 0 {
		t.Errorf("bed countdown after a restart = %d, want the remaining time", bed)
	}
}

// A store that cannot be written to must not stop the app: the countdown still
// runs, it just will not outlive the process.
type brokenTimers struct{}

func (brokenTimers) Set(string, time.Time) error        { return fmt.Errorf("disk on fire") }
func (brokenTimers) Clear(string) error                 { return fmt.Errorf("disk on fire") }
func (brokenTimers) All() (map[string]time.Time, error) { return nil, fmt.Errorf("disk on fire") }

func TestAFailingStoreStillLeavesTheTimersWorking(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	a := newAutoOff(brokenTimers{}, testSettings)
	a.now = fixedClock(&now)
	a.setBed(60)
	if bed, _ := a.remaining(); bed != int(settings.Defaults.BedOffAfter.Seconds()) {
		t.Errorf("countdown = %ds, want it armed despite the store failing", bed)
	}
	now = now.Add(settings.Defaults.BedOffAfter + time.Second)
	if bed, _ := a.due(); !bed {
		t.Error("the shut-off did not fire when the store was failing")
	}
}
