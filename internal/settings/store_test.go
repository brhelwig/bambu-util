package settings

import (
	"path/filepath"
	"strings"
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

func TestAnUnconfiguredStoreReportsTheDefaults(t *testing.T) {
	if got := openTest(t).Values(); got != Defaults {
		t.Errorf("values = %+v, want %+v", got, Defaults)
	}
}

func TestSetAndReadBack(t *testing.T) {
	store := openTest(t)
	if err := store.SetDuration(KeyRetention, 48*time.Hour); err != nil {
		t.Fatalf("SetDuration: %v", err)
	}
	if got := store.Values().Retention; got != 48*time.Hour {
		t.Errorf("retention = %s, want 48h", got)
	}
	// Setting one leaves the rest alone.
	if got := store.Values().BedOffAfter; got != Defaults.BedOffAfter {
		t.Errorf("bed shut-off = %s, want the default %s", got, Defaults.BedOffAfter)
	}
}

func TestEverySettingCanBeChanged(t *testing.T) {
	store := openTest(t)
	for _, name := range []string{KeyRetention, KeyBedOffAfter, KeyNozzleOffAfter, KeyLampOffAfter} {
		if err := store.SetDuration(name, 2*time.Hour); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	v := store.Values()
	for name, got := range map[string]time.Duration{
		KeyRetention:      v.Retention,
		KeyBedOffAfter:    v.BedOffAfter,
		KeyNozzleOffAfter: v.NozzleOffAfter,
		KeyLampOffAfter:   v.LampOffAfter,
	} {
		if got != 2*time.Hour {
			t.Errorf("%s = %s, want 2h", name, got)
		}
	}
}

func TestSettingsSurviveReopeningTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.db")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := first.SetDuration(KeyLampOffAfter, 90*time.Minute); err != nil {
		t.Fatalf("SetDuration: %v", err)
	}
	first.Close()

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
	if got := second.Values().LampOffAfter; got != 90*time.Minute {
		t.Errorf("after reopening, lamp delay = %s, want 90m", got)
	}
}

func TestAnUnknownSettingIsRefused(t *testing.T) {
	err := openTest(t).SetDuration("chamber-temperature", time.Hour)
	if err == nil {
		t.Fatal("an unknown setting was accepted")
	}
	if !strings.Contains(err.Error(), "chamber-temperature") {
		t.Errorf("error does not name the setting: %v", err)
	}
}

// A shut-off window of a year is not a safety shut-off, and a recording window
// of a year fills the disk.
func TestValuesOutsideTheBoundsAreRefused(t *testing.T) {
	store := openTest(t)
	cases := map[string]time.Duration{
		"retention far too long":  365 * 24 * time.Hour,
		"retention far too short": time.Second,
		"shut-off far too long":   365 * 24 * time.Hour,
	}
	for name, d := range cases {
		key := KeyRetention
		if strings.HasPrefix(name, "shut-off") {
			key = KeyBedOffAfter
		}
		if err := store.SetDuration(key, d); err == nil {
			t.Errorf("%s (%s) was accepted", name, d)
		}
	}
	if got := store.Values(); got != Defaults {
		t.Errorf("a refused write changed the values: %+v", got)
	}
}

// The settings table is the obvious thing to poke at by hand. One bad row
// should cost that setting, not the printer.
func TestAValueThatCannotBeReadFallsBackToItsDefault(t *testing.T) {
	store := openTest(t)
	for _, bad := range []string{"not a duration", "", "999999h"} {
		if _, err := store.db.Exec(
			`INSERT INTO settings (name, value) VALUES (?, ?)
			 ON CONFLICT(name) DO UPDATE SET value = excluded.value`, KeyRetention, bad); err != nil {
			t.Fatalf("write %q: %v", bad, err)
		}
		if err := store.reload(); err != nil {
			t.Fatalf("reload after %q: %v", bad, err)
		}
		if got := store.Values().Retention; got != Defaults.Retention {
			t.Errorf("stored %q gave retention %s, want the default %s", bad, got, Defaults.Retention)
		}
	}
}
