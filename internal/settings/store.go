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
}

// Defaults are what an unconfigured app runs with, and what a value that cannot
// be read falls back to.
var Defaults = Values{
	Retention:      24 * time.Hour,
	KeptJobs:       5,
	BedOffAfter:    24 * time.Hour,
	NozzleOffAfter: 15 * time.Minute,
	LampOffAfter:   8 * time.Hour,
}

// Every setting is stored as a whole number: seconds for a length of time, a
// plain count otherwise. The bounds keep a value from being useless — a
// recording window of a year fills the disk, a shut-off window of a year is not
// a safety shut-off, and keeping every print ever made defeats the retention it
// is meant to work alongside.
type spec struct {
	seconds  bool
	min, max int
}

// show renders a bound the way the setting is written, so a refusal reads in
// the units the field uses rather than in raw seconds.
func (s spec) show(value int) string {
	if !s.seconds {
		return strconv.Itoa(value)
	}
	return (time.Duration(value) * time.Second).String()
}

var specs = map[string]spec{
	KeyRetention:      {true, 3600, 30 * 24 * 3600},
	KeyKeptJobs:       {false, 0, 50},
	KeyBedOffAfter:    {true, 60, 7 * 24 * 3600},
	KeyNozzleOffAfter: {true, 60, 7 * 24 * 3600},
	KeyLampOffAfter:   {true, 60, 7 * 24 * 3600},
}

// Seconds reports whether a setting is a length of time, so the page can label
// it in the right units.
func Seconds(name string) bool { return specs[name].seconds }

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
	if value < spec.min || value > spec.max {
		return fmt.Errorf("%s must be between %s and %s", name, spec.show(spec.min), spec.show(spec.max))
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
	if spec := specs[name]; n < spec.min || n > spec.max {
		return fallback
	}
	return n
}

func readDuration(stored map[string]string, name string, fallback time.Duration) time.Duration {
	secs := readInt(stored, name, int(fallback.Seconds()))
	return time.Duration(secs) * time.Second
}
