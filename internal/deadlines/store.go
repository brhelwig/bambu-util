// Package deadlines persists the app's pending timers. They are held in memory
// while the process runs; without this a restart cancels them silently, and
// since the deployment restarts to pick up a new image, that is every update.
package deadlines

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/brhelwig/bambu-util/internal/sqlitedb"
)

const schema = `
CREATE TABLE IF NOT EXISTS deadlines (
  name TEXT PRIMARY KEY,
  at   INTEGER NOT NULL
);
`

// Names of the timers kept here. They are stored rather than derived so a
// restart resumes the same countdown instead of starting a new one.
const (
	BedOff     = "bed-off"
	NozzleOff  = "nozzle-off"
	LampOff    = "lamp-off"
	BedOnSince = "bed-on-since"
	BedOnNext  = "bed-on-next"
)

// Store holds the pending timers.
type Store struct {
	db    *sql.DB
	owned bool
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

// New returns a store over db, creating its table if needed.
func New(db *sql.DB) (*Store, error) {
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// Set records when a timer comes due, replacing any earlier setting for it.
func (s *Store) Set(name string, at time.Time) error {
	if name == "" {
		return fmt.Errorf("deadlines: no name")
	}
	_, err := s.db.Exec(`
		INSERT INTO deadlines (name, at) VALUES (?, ?)
		ON CONFLICT(name) DO UPDATE SET at = excluded.at`, name, at.Unix())
	return err
}

// Clear forgets a timer. Clearing one that is not set is not an error.
func (s *Store) Clear(name string) error {
	_, err := s.db.Exec(`DELETE FROM deadlines WHERE name = ?`, name)
	return err
}

// All returns every timer still pending.
func (s *Store) All() (map[string]time.Time, error) {
	rows, err := s.db.Query(`SELECT name, at FROM deadlines`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var name string
		var at int64
		if err := rows.Scan(&name, &at); err != nil {
			return nil, err
		}
		out[name] = time.Unix(at, 0)
	}
	return out, rows.Err()
}
