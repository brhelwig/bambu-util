// Package settings holds the app's configuration, so changing it is an edit on
// the page rather than a redeployment.
package settings

import (
	"database/sql"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/brhelwig/bambu-util/internal/sqlitedb"
)

const schema = `
CREATE TABLE IF NOT EXISTS settings (
  name  TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`

// Names of the settings, as the page sends them.
const (
	KeyRetention      = "retention"
	KeyKeptJobs       = "kept-jobs"
	KeyBedOffAfter    = "bed-off-after"
	KeyNozzleOffAfter = "nozzle-off-after"
	KeyLampOffAfter   = "lamp-off-after"
	KeyActivityLimit  = "activity-limit"
	KeyDatabaseLimit  = "database-limit"
	KeySessionLength  = "session-length"

	KeyPrinterIP         = "printer-ip"
	KeyPrinterSerial     = "printer-serial"
	KeyPrinterAccessCode = "printer-access-code"

	// KeyDashboard is the printer screen's sections, in order. Anything absent
	// is hidden.
	KeyDashboard = "dashboard"
)

// texts are the settings that hold words rather than numbers.
var texts = map[string]bool{
	KeyPrinterIP:         true,
	KeyPrinterSerial:     true,
	KeyPrinterAccessCode: true,
	KeyDashboard:         true,
}

// Text reports whether a setting holds words.
func Text(name string) bool { return texts[name] }

// Values is one complete set of settings.
//
// AccessCode is a credential. It is here because the connection needs it, and
// it must never reach the browser — see the settings endpoint, which reports
// only whether one is set.
type Values struct {
	PrinterIP     string
	PrinterSerial string
	AccessCode    string
	Dashboard     string

	Retention      time.Duration
	KeptJobs       int
	BedOffAfter    time.Duration
	NozzleOffAfter time.Duration
	LampOffAfter   time.Duration

	// ActivityLimit and DatabaseLimit are in bytes, converted from the megabytes
	// stored, because every consultation of them is a comparison against a size
	// in bytes. DatabaseLimit is zero when the cap is switched off.
	ActivityLimit int64
	DatabaseLimit int64

	// SessionLength is how long a login lasts before it has to be done again.
	SessionLength time.Duration
}

// BytesPerMB converts the stored megabytes to the bytes the log counts in.
const BytesPerMB = 1 << 20

// Defaults are what an unconfigured app runs with, and what a value that cannot
// be read falls back to.
var Defaults = Values{
	Retention:      24 * time.Hour,
	KeptJobs:       5,
	BedOffAfter:    24 * time.Hour,
	NozzleOffAfter: 15 * time.Minute,
	LampOffAfter:   8 * time.Hour,
	ActivityLimit:  64 * BytesPerMB,
	SessionLength:  14 * 24 * time.Hour,
}

// unit is how a setting's whole number should be read back.
type unit int

const (
	count unit = iota
	seconds
	megabytes
)

// Every setting is stored as a whole number: seconds for a length of time,
// megabytes for a size, a plain count otherwise. The bounds keep a value from
// being useless — a recording window of a year fills the disk, a shut-off
// window of a year is not a safety shut-off, and keeping every print ever made
// defeats the retention it is meant to work alongside.
//
// A few settings can also be switched off, which is written as zero and sits
// below their useful range rather than inside it.
type spec struct {
	unit     unit
	min, max int
	offAtNil bool
}

// allows reports whether value is one this setting will take.
func (s spec) allows(value int) bool {
	if s.offAtNil && value == 0 {
		return true
	}
	return value >= s.min && value <= s.max
}

// refuse says what the setting will take, in the units it is written in.
func (s spec) refuse(name string) error {
	if s.offAtNil {
		return fmt.Errorf("%s must be 0 to switch it off, or between %s and %s",
			name, s.show(s.min), s.show(s.max))
	}
	return fmt.Errorf("%s must be between %s and %s", name, s.show(s.min), s.show(s.max))
}

// show renders a bound the way the setting is written, so a refusal reads in
// the units the field uses rather than in raw seconds or bytes.
func (s spec) show(value int) string {
	switch s.unit {
	case seconds:
		return (time.Duration(value) * time.Second).String()
	case megabytes:
		return fmt.Sprintf("%d MB", value)
	}
	return strconv.Itoa(value)
}

// The event log's ceiling is deliberately well short of a whole disk: this runs
// on a Pi whose database also holds the camera buffer.
var specs = map[string]spec{
	KeyRetention:      {unit: seconds, min: 3600, max: 30 * 24 * 3600},
	KeyKeptJobs:       {unit: count, min: 0, max: 50},
	KeyBedOffAfter:    {unit: seconds, min: 60, max: 7 * 24 * 3600},
	KeyNozzleOffAfter: {unit: seconds, min: 60, max: 7 * 24 * 3600},
	KeyLampOffAfter:   {unit: seconds, min: 60, max: 7 * 24 * 3600},
	KeyActivityLimit:  {unit: megabytes, min: 1, max: 512},

	// A login that lasts a year is not much of a login, and one that lasts
	// minutes makes a phone on the home screen useless.
	KeySessionLength: {unit: seconds, min: 3600, max: 365 * 24 * 3600},

	// The floor is not fussiness: a cap of a few megabytes would delete almost
	// everything and rebuild the file on every pass. Off is the default, since
	// this deletes footage the other settings promised to keep.
	KeyDatabaseLimit: {unit: megabytes, min: 256, max: 64 * 1024, offAtNil: true},
}

// Store reads and writes the settings, keeping the current set in memory so the
// hot paths that consult them are not querying the database every few seconds.
type Store struct {
	db     *sql.DB
	owned  bool
	mu     sync.RWMutex
	values Values
}

// Open makes a store over a database of its own at path. The app shares one
// database across stores and calls New instead.
func Open(path string) (*Store, error) {
	db, err := sqlitedb.Open(path)
	if err != nil {
		return nil, err
	}
	store, err := New(db)
	if err != nil {
		db.Close()
		return nil, err
	}
	store.owned = true
	return store, nil
}

// Close closes the database, unless it belongs to whoever passed it in —
// closing a shared handle would take every other store down with it.
func (s *Store) Close() error {
	if !s.owned {
		return nil
	}
	return s.db.Close()
}

// New returns a store over db, creating its table if needed and reading what is
// already stored.
func New(db *sql.DB) (*Store, error) {
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	s := &Store{db: db, values: Defaults}
	if err := s.reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Values returns the current settings.
func (s *Store) Values() Values {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.values
}

// Set stores one setting: seconds for a length of time, a plain count
// otherwise.
func (s *Store) Set(name string, value int) error {
	spec, ok := specs[name]
	if !ok {
		return fmt.Errorf("settings: unknown setting %q", name)
	}
	if !spec.allows(value) {
		return spec.refuse(name)
	}
	if _, err := s.db.Exec(`
		INSERT INTO settings (name, value) VALUES (?, ?)
		ON CONFLICT(name) DO UPDATE SET value = excluded.value`, name, strconv.Itoa(value)); err != nil {
		return err
	}
	return s.reload()
}

// SetText stores one setting that holds words. An empty value clears it, which
// is how a printer is forgotten.
func (s *Store) SetText(name, value string) error {
	if !texts[name] {
		return fmt.Errorf("settings: %q does not hold text", name)
	}
	if len(value) > 512 {
		return fmt.Errorf("%s is too long", name)
	}
	if value == "" {
		if _, err := s.db.Exec(`DELETE FROM settings WHERE name = ?`, name); err != nil {
			return err
		}
		return s.reload()
	}
	if _, err := s.db.Exec(`
		INSERT INTO settings (name, value) VALUES (?, ?)
		ON CONFLICT(name) DO UPDATE SET value = excluded.value`, name, value); err != nil {
		return err
	}
	return s.reload()
}

// reload reads every setting back into memory. A value that cannot be read
// falls back to its default rather than stopping the app: the settings table is
// the obvious thing to edit by hand, and one bad row should cost that setting,
// not the printer.
func (s *Store) reload() error {
	rows, err := s.db.Query(`SELECT name, value FROM settings`)
	if err != nil {
		return err
	}
	defer rows.Close()

	stored := map[string]string{}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return err
		}
		stored[name] = value
	}
	if err := rows.Err(); err != nil {
		return err
	}

	v := Defaults
	v.Retention = readDuration(stored, KeyRetention, Defaults.Retention)
	v.BedOffAfter = readDuration(stored, KeyBedOffAfter, Defaults.BedOffAfter)
	v.NozzleOffAfter = readDuration(stored, KeyNozzleOffAfter, Defaults.NozzleOffAfter)
	v.LampOffAfter = readDuration(stored, KeyLampOffAfter, Defaults.LampOffAfter)
	v.KeptJobs = readInt(stored, KeyKeptJobs, Defaults.KeptJobs)
	v.ActivityLimit = int64(readInt(stored, KeyActivityLimit, int(Defaults.ActivityLimit/BytesPerMB))) * BytesPerMB
	v.DatabaseLimit = int64(readInt(stored, KeyDatabaseLimit, int(Defaults.DatabaseLimit/BytesPerMB))) * BytesPerMB
	v.SessionLength = readDuration(stored, KeySessionLength, Defaults.SessionLength)
	v.PrinterIP = stored[KeyPrinterIP]
	v.PrinterSerial = stored[KeyPrinterSerial]
	v.AccessCode = stored[KeyPrinterAccessCode]
	v.Dashboard = stored[KeyDashboard]

	s.mu.Lock()
	defer s.mu.Unlock()
	s.values = v
	return nil
}

func readInt(stored map[string]string, name string, fallback int) int {
	raw, ok := stored[name]
	if !ok {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	if !specs[name].allows(n) {
		return fallback
	}
	return n
}

func readDuration(stored map[string]string, name string, fallback time.Duration) time.Duration {
	secs := readInt(stored, name, int(fallback.Seconds()))
	return time.Duration(secs) * time.Second
}
