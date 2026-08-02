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

// A refusal has to read in the units the field uses, not in raw seconds or
// bytes, or it describes a number nobody typed.
func TestARefusalReadsInTheSettingsOwnUnits(t *testing.T) {
	store := openTest(t)
	for _, c := range []struct{ name, want string }{
		{KeyRetention, "h"},
		{KeyKeptJobs, "50"},
		{KeyActivityLimit, "512 MB"},
	} {
		err := store.Set(c.name, 1_000_000_000)
		if err == nil {
			t.Errorf("%s: an absurd value was accepted", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s refused with %q, want it to mention %q", c.name, err, c.want)
		}
	}
}

func TestTheEventLogSizeIsASetting(t *testing.T) {
	store := openTest(t)
	if got := store.Values().ActivityLimit; got != Defaults.ActivityLimit {
		t.Errorf("event log limit = %d, want the default %d", got, Defaults.ActivityLimit)
	}
	// Stored in megabytes, read back in the bytes everything compares against.
	if err := store.Set(KeyActivityLimit, 8); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := store.Values().ActivityLimit; got != 8*BytesPerMB {
		t.Errorf("event log limit = %d, want %d", got, 8*BytesPerMB)
	}
	// A log of no size is not a log, and a whole disk is not a bound.
	if err := store.Set(KeyActivityLimit, 0); err == nil {
		t.Error("a limit of nothing was accepted")
	}
	if err := store.Set(KeyActivityLimit, 5000); err == nil {
		t.Error("a limit larger than the disk was accepted")
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

func TestTheDatabaseCapIsASettingAndIsOffByDefault(t *testing.T) {
	store := openTest(t)
	// Off by default on purpose: this deletes footage the other settings
	// promised to keep, so switching it on has to be a choice.
	if got := store.Values().DatabaseLimit; got != 0 {
		t.Errorf("database cap = %d, want 0 meaning off", got)
	}
	if err := store.Set(KeyDatabaseLimit, 1024); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := store.Values().DatabaseLimit; got != 1024*BytesPerMB {
		t.Errorf("database cap = %d, want %d", got, 1024*BytesPerMB)
	}
	// Zero is how it is switched off again, not an out-of-range value.
	if err := store.Set(KeyDatabaseLimit, 0); err != nil {
		t.Errorf("switching the cap off was refused: %v", err)
	}
	if got := store.Values().DatabaseLimit; got != 0 {
		t.Errorf("database cap = %d after switching off, want 0", got)
	}
	// Between off and the floor there is nothing useful: a cap that small would
	// delete almost everything and rebuild the file every pass.
	if err := store.Set(KeyDatabaseLimit, 16); err == nil {
		t.Error("a cap below the floor was accepted")
	}
	if err := store.Set(KeyDatabaseLimit, 100_000); err == nil {
		t.Error("a cap larger than any disk was accepted")
	}
}

// A setting that can be switched off has to say so when it refuses, or the
// message describes a range that does not include the value that works.
func TestRefusingTheDatabaseCapMentionsSwitchingItOff(t *testing.T) {
	err := openTest(t).Set(KeyDatabaseLimit, 16)
	if err == nil {
		t.Fatal("a cap below the floor was accepted")
	}
	for _, want := range []string{"0", "256 MB", "65536 MB"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
}
