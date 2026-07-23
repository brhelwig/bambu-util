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

// Range returns the oldest and newest frame timestamps currently stored,
// or nil, nil if the store is empty.
func (s *Store) Range() (oldest, newest *int64, err error) {
	var minTs, maxTs sql.NullInt64
	row := s.db.QueryRow(`SELECT MIN(ts), MAX(ts) FROM frames`)
	if err := row.Scan(&minTs, &maxTs); err != nil {
		return nil, nil, err
	}
	if minTs.Valid {
		v := minTs.Int64
		oldest = &v
	}
	if maxTs.Valid {
		v := maxTs.Int64
		newest = &v
	}
	return oldest, newest, nil
}

// Prune deletes frames older than cutoff, and any job row that finished
// before cutoff. A job with no end yet (still in progress) is never pruned,
// regardless of how old its start is.
func (s *Store) Prune(cutoff int64) error {
	if _, err := s.db.Exec(`DELETE FROM frames WHERE ts < ?`, cutoff); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM jobs WHERE end_ts IS NOT NULL AND end_ts < ?`, cutoff)
	return err
}

// Job is one print job's recorded time range. End is nil while the job is
// still in progress.
type Job struct {
	ID    int64
	Name  string
	Start int64
	End   *int64
}

// OpenJob records the start of a print job and returns its id.
func (s *Store) OpenJob(name string, startTs int64) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO jobs (name, start_ts, end_ts) VALUES (?, ?, NULL)`, name, startTs)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// CloseJob records the end of a print job.
func (s *Store) CloseJob(id, endTs int64) error {
	_, err := s.db.Exec(`UPDATE jobs SET end_ts = ? WHERE id = ?`, endTs, id)
	return err
}

// RecentJobs returns every job row currently stored, newest-started first.
// Prune keeps this bounded to jobs whose frames could still exist.
func (s *Store) RecentJobs() ([]Job, error) {
	rows, err := s.db.Query(`SELECT id, name, start_ts, end_ts FROM jobs ORDER BY start_ts DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var j Job
		var end sql.NullInt64
		if err := rows.Scan(&j.ID, &j.Name, &j.Start, &end); err != nil {
			return nil, err
		}
		if end.Valid {
			v := end.Int64
			j.End = &v
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}
