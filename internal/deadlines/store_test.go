package deadlines

import (
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	store, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestSetAndReadBack(t *testing.T) {
	store := openTest(t)
	at := time.Unix(1_700_000_000, 0)
	if err := store.Set(BedOff, at); err != nil {
		t.Fatalf("Set: %v", err)
	}
	pending, err := store.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := pending[BedOff]; !got.Equal(at) {
		t.Errorf("stored %v, want %v", got, at)
	}
}

// Re-arming a timer replaces it. Two rows for one countdown would leave the
// older, wrong one to be restored.
func TestSettingATimerAgainReplacesIt(t *testing.T) {
	store := openTest(t)
	first := time.Unix(1_700_000_000, 0)
	later := first.Add(time.Hour)
	if err := store.Set(BedOff, first); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Set(BedOff, later); err != nil {
		t.Fatalf("Set again: %v", err)
	}
	pending, err := store.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("stored %d timers, want 1", len(pending))
	}
	if got := pending[BedOff]; !got.Equal(later) {
		t.Errorf("stored %v, want the newer %v", got, later)
	}
}

func TestTimersAreIndependent(t *testing.T) {
	store := openTest(t)
	bed := time.Unix(1_700_000_000, 0)
	nozzle := bed.Add(time.Minute)
	if err := store.Set(BedOff, bed); err != nil {
		t.Fatalf("Set bed: %v", err)
	}
	if err := store.Set(NozzleOff, nozzle); err != nil {
		t.Fatalf("Set nozzle: %v", err)
	}
	if err := store.Clear(BedOff); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	pending, err := store.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if _, ok := pending[BedOff]; ok {
		t.Error("the cleared timer is still there")
	}
	if got := pending[NozzleOff]; !got.Equal(nozzle) {
		t.Errorf("clearing one timer disturbed another: %v", got)
	}
}

// Clearing races with firing, so clearing one that is not set cannot be an
// error.
func TestClearingATimerThatIsNotSet(t *testing.T) {
	store := openTest(t)
	if err := store.Clear(BedOff); err != nil {
		t.Errorf("Clear: %v", err)
	}
}

func TestSetRejectsAnEmptyName(t *testing.T) {
	if err := openTest(t).Set("", time.Unix(1, 0)); err == nil {
		t.Error("a timer with no name was accepted")
	}
}

func TestAnEmptyStoreHasNoTimers(t *testing.T) {
	pending, err := openTest(t).All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("found %d timers in a new store", len(pending))
	}
}

func TestTimersSurviveReopeningTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "timers.db")
	at := time.Unix(1_700_000_000, 0)

	first, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := first.Set(LampOff, at); err != nil {
		t.Fatalf("Set: %v", err)
	}
	first.Close()

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
	pending, err := second.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := pending[LampOff]; !got.Equal(at) {
		t.Errorf("after reopening, stored %v, want %v", got, at)
	}
}
