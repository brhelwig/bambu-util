// Package history stores recorded camera frames and print-job boundaries in
// SQLite, so the app can serve a scrollback buffer and per-job timelapses.
package history

import (
	"database/sql"
	"errors"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS frames (
  id  INTEGER PRIMARY KEY,
  ts  INTEGER NOT NULL,
  jpeg BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS frames_ts ON frames(ts);

CREATE TABLE IF NOT EXISTS jobs (
  id       INTEGER PRIMARY KEY,
  name     TEXT NOT NULL,
  start_ts INTEGER NOT NULL,
  end_ts   INTEGER
);
`

// Store persists camera frames and job boundaries in a SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path. Use
// ":memory:" for a throwaway in-process database, e.g. in tests.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// modernc.org/sqlite serializes writes at the connection level; capping
	// the pool at one connection avoids "database is locked" errors under
	// concurrent access and gives :memory: a single, consistent database
	// instead of a fresh one per connection.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// InsertFrame records one camera frame at the given unix-second timestamp.
func (s *Store) InsertFrame(ts int64, jpeg []byte) error {
	_, err := s.db.Exec(`INSERT INTO frames (ts, jpeg) VALUES (?, ?)`, ts, jpeg)
	return err
}

// ErrNoFrame is returned by FrameAtOrAfter when no frame exists at or after
// the requested timestamp.
var ErrNoFrame = errors.New("history: no frame at or after ts")

// FrameAtOrAfter returns the stored frame with the smallest timestamp that
// is >= ts, along with that timestamp. Returns ErrNoFrame if none exists.
func (s *Store) FrameAtOrAfter(ts int64) (jpeg []byte, gotTs int64, err error) {
	row := s.db.QueryRow(`SELECT ts, jpeg FROM frames WHERE ts >= ? ORDER BY ts ASC LIMIT 1`, ts)
	if err := row.Scan(&gotTs, &jpeg); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, ErrNoFrame
		}
		return nil, 0, err
	}
	return jpeg, gotTs, nil
}
