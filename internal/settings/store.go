// Package settings holds the app's configuration, so changing it is an edit on
// the page rather than a redeployment.
package settings

import (
	"database/sql"
	"fmt"
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
	KeyBedOffAfter    = "bed-off-after"
	KeyNozzleOffAfter = "nozzle-off-after"
	KeyLampOffAfter   = "lamp-off-after"
)

// Values is one complete set of settings.
type Values struct {
	Retention      time.Duration
	BedOffAfter    time.Duration
	NozzleOffAfter time.Duration
	LampOffAfter   time.Duration
}

// Defaults are what an unconfigured app runs with. They are also the floor a
// bad stored value falls back to, so a hand-edited row cannot stop the app.
var Defaults = Values{
	Retention:      24 * time.Hour,
	BedOffAfter:    24 * time.Hour,
	NozzleOffAfter: 15 * time.Minute,
	LampOffAfter:   8 * time.Hour,
}

// Bounds on each setting. A recording window of a year would fill the disk; a
// shut-off window of a year is not a safety shut-off at all.
var limits = map[string]struct{ min, max time.Duration }{
	KeyRetention:      {time.Hour, 30 * 24 * time.Hour},
	KeyBedOffAfter:    {time.Minute, 7 * 24 * time.Hour},
	KeyNozzleOffAfter: {time.Minute, 7 * 24 * time.Hour},
	KeyLampOffAfter:   {time.Minute, 7 * 24 * time.Hour},
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

// SetDuration stores one duration setting.
func (s *Store) SetDuration(name string, d time.Duration) error {
	limit, ok := limits[name]
	if !ok {
		return fmt.Errorf("settings: unknown setting %q", name)
	}
	if d < limit.min || d > limit.max {
		return fmt.Errorf("settings: %s must be between %s and %s", name, limit.min, limit.max)
	}
	if _, err := s.db.Exec(`
		INSERT INTO settings (name, value) VALUES (?, ?)
		ON CONFLICT(name) DO UPDATE SET value = excluded.value`, name, d.String()); err != nil {
		return err
	}
	return s.reload()
}

// reload reads every setting back into memory. A value that cannot be read
// falls back to its default rather than stopping the app: settings are edited
// by hand often enough that one bad row should not take the printer offline.
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
	v.Retention = duration(stored, KeyRetention, Defaults.Retention)
	v.BedOffAfter = duration(stored, KeyBedOffAfter, Defaults.BedOffAfter)
	v.NozzleOffAfter = duration(stored, KeyNozzleOffAfter, Defaults.NozzleOffAfter)
	v.LampOffAfter = duration(stored, KeyLampOffAfter, Defaults.LampOffAfter)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.values = v
	return nil
}

func duration(stored map[string]string, name string, fallback time.Duration) time.Duration {
	raw, ok := stored[name]
	if !ok {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	if limit, ok := limits[name]; ok && (d < limit.min || d > limit.max) {
		return fallback
	}
	return d
}
