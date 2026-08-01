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
	if err := store.Set(KeyRetention, 48*3600); err != nil {
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
		if err := store.Set(name, 2*3600); err != nil {
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
	if err := first.Set(KeyLampOffAfter, 90*60); err != nil {
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
	err := openTest(t).Set("chamber-temperature", 3600)
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
	cases := map[string]int{
		"retention far too long":  365 * 24 * 3600,
		"retention far too short": 1,
		"shut-off far too long":   365 * 24 * 3600,
	}
	for name, value := range cases {
		key := KeyRetention
		if strings.HasPrefix(name, "shut-off") {
			key = KeyBedOffAfter
		}
		if err := store.Set(key, value); err == nil {
			t.Errorf("%s (%d) was accepted", name, value)
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
	for _, bad := range []string{"not a number", "", "999999999"} {
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

func TestJobRetentionIsASetting(t *testing.T) {
	store := openTest(t)
	if got := store.Values().KeptJobs; got != Defaults.KeptJobs {
		t.Errorf("kept jobs = %d, want the default %d", got, Defaults.KeptJobs)
	}
	if err := store.Set(KeyKeptJobs, 12); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := store.Values().KeptJobs; got != 12 {
		t.Errorf("kept jobs = %d, want 12", got)
	}
	// Keeping none is a real choice: retention alone then decides everything.
	if err := store.Set(KeyKeptJobs, 0); err != nil {
		t.Errorf("keeping no jobs was refused: %v", err)
	}
	if err := store.Set(KeyKeptJobs, -1); err == nil {
		t.Error("a negative count was accepted")
	}
	if err := store.Set(KeyKeptJobs, 5000); err == nil {
		t.Error("keeping every print ever made was accepted")
	}
}

// A count is not seconds, and the page needs to know which it is to label it.
func TestSecondsSaysWhichSettingsAreLengthsOfTime(t *testing.T) {
	for _, name := range []string{KeyRetention, KeyBedOffAfter, KeyNozzleOffAfter, KeyLampOffAfter} {
		if !Seconds(name) {
			t.Errorf("%s should be a length of time", name)
		}
	}
	if Seconds(KeyKeptJobs) {
		t.Error("kept jobs is a count, not a length of time")
	}
}

func TestTextReportsWhichSettingsHoldWords(t *testing.T) {
	for _, name := range []string{KeyPrinterIP, KeyPrinterSerial, KeyPrinterAccessCode, KeyDashboard} {
		if !Text(name) {
			t.Errorf("Text(%q) = false, want true", name)
		}
	}
	for _, name := range []string{KeyRetention, KeyKeptJobs, KeyBedOffAfter, "nonsense"} {
		if Text(name) {
			t.Errorf("Text(%q) = true, want false", name)
		}
	}
}

func TestSetTextStoresAndClears(t *testing.T) {
	store := openTest(t)

	if err := store.SetText(KeyPrinterIP, "192.168.1.50"); err != nil {
		t.Fatalf("SetText: %v", err)
	}
	if got := store.Values().PrinterIP; got != "192.168.1.50" {
		t.Errorf("PrinterIP = %q, want it stored", got)
	}

	// Clearing is how a printer is forgotten, so empty has to delete rather
	// than store an empty string.
	if err := store.SetText(KeyPrinterIP, ""); err != nil {
		t.Fatalf("SetText empty: %v", err)
	}
	if got := store.Values().PrinterIP; got != "" {
		t.Errorf("PrinterIP = %q, want it forgotten", got)
	}
}

func TestSetTextRejectsWhatItShould(t *testing.T) {
	store := openTest(t)

	if err := store.SetText(KeyRetention, "a while"); err == nil {
		t.Error("SetText accepted a setting that does not hold text")
	}
	if err := store.SetText(KeyPrinterIP, strings.Repeat("x", 513)); err == nil {
		t.Error("SetText accepted an over-long value")
	}
	if got := store.Values().PrinterIP; got != "" {
		t.Errorf("PrinterIP = %q, want the rejected value not stored", got)
	}
}
