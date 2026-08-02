// Package history stores recorded camera frames and print-job boundaries in
// SQLite, so the app can serve a scrollback buffer and per-job timelapses.
package history

import (
	"database/sql"
	"errors"
	"math"

	"github.com/brhelwig/bambu-util/internal/capacity"
	"github.com/brhelwig/bambu-util/internal/sqlitedb"
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
	db    *sql.DB
	owned bool
}

// Open makes a store over a database of its own at path, which Close then
// closes. The app shares one database across stores and calls New instead.
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

// New returns a store over db, creating its tables if needed. The caller keeps
// ownership of db.
func New(db *sql.DB) (*Store, error) {
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the database, unless it belongs to whoever passed it in —
// closing a shared handle would take every other store down with it.
func (s *Store) Close() error {
	if !s.owned {
		return nil
	}
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

// DefaultKeptJobs is how many finished prints keep their footage past the
// cutoff when nothing says otherwise, so a recent timelapse stays watchable
// after the rolling buffer has moved on. The running value is a setting.
const DefaultKeptJobs = 5

// ThinInterval is the spacing, in seconds, that kept footage is reduced to once
// it ages past the cutoff. Keeping five whole prints at the recording rate would
// add gigabytes; timelapse playback runs at 60x or faster, so a frame every 10s
// is still more footage than the playback can show.
const ThinInterval = 10

// MaxOpenJobSpan bounds, in seconds, how far back from the cutoff the
// in-progress print's footage is protected. The print's own row is what protects
// it, and that row only closes once the printer reports a finished state — a
// printer that drops off the network mid-print leaves RUNNING as the last thing
// it said, so the row can stay open indefinitely. Without this bound that one row
// would exempt everything from its start onward from retention, permanently. Set
// well past any real print length, so it only ever catches the stuck case.
const MaxOpenJobSpan = 48 * 60 * 60

// Prune enforces the retention policy. Frames older than cutoff are deleted
// unless they belong to a kept print — the job still in progress, or one of the
// keptJobs most recently finished — whose footage is instead thinned to one
// frame per ThinInterval. Job rows go only once they are both older than cutoff
// and no longer among the kept ones.
func (s *Store) Prune(cutoff int64, keptJobs int) error {
	kept, err := s.keptWindows(cutoff, keptJobs)
	if err != nil {
		return err
	}
	if err := s.deleteUnkeptFrames(cutoff, kept); err != nil {
		return err
	}
	for _, w := range kept {
		if err := s.thinWindow(cutoff, w); err != nil {
			return err
		}
	}
	_, err = s.db.Exec(`
		DELETE FROM jobs
		WHERE end_ts IS NOT NULL AND end_ts < ?
		  AND id NOT IN (
		    SELECT id FROM jobs WHERE end_ts IS NOT NULL
		    ORDER BY start_ts DESC, id DESC LIMIT ?
		  )`, cutoff, keptJobs)
	return err
}

// window is a span of time whose frames survive the cutoff. end is nil for a
// print still in progress, meaning the span has no upper bound yet.
type window struct {
	start int64
	end   *int64
}

func (s *Store) keptWindows(cutoff int64, keptJobs int) ([]window, error) {
	rows, err := s.db.Query(`
		SELECT start_ts, end_ts FROM (
		  SELECT start_ts, end_ts FROM jobs WHERE end_ts IS NULL
		  ORDER BY start_ts DESC, id DESC LIMIT 1
		)
		UNION ALL
		SELECT start_ts, end_ts FROM (
		  SELECT start_ts, end_ts FROM jobs WHERE end_ts IS NOT NULL
		  ORDER BY start_ts DESC, id DESC LIMIT ?
		)`, keptJobs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []window
	for rows.Next() {
		var w window
		var end sql.NullInt64
		if err := rows.Scan(&w.start, &end); err != nil {
			return nil, err
		}
		if end.Valid {
			v := end.Int64
			w.end = &v
		} else if floor := cutoff - MaxOpenJobSpan; w.start < floor {
			w.start = floor // see MaxOpenJobSpan
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// deleteUnkeptFrames removes pre-cutoff frames that fall inside none of the
// kept windows.
func (s *Store) deleteUnkeptFrames(cutoff int64, kept []window) error {
	q := `DELETE FROM frames WHERE ts < ?`
	args := []any{cutoff}
	for _, w := range kept {
		if w.end == nil {
			q += ` AND ts < ?`
			args = append(args, w.start)
			continue
		}
		q += ` AND NOT (ts >= ? AND ts <= ?)`
		args = append(args, w.start, *w.end)
	}
	_, err := s.db.Exec(q, args...)
	return err
}

// thinWindow reduces the pre-cutoff part of one kept window to a single frame
// per ThinInterval, keeping the earliest frame in each interval. Frames newer
// than cutoff are left at the full recording rate — they are still part of the
// live scrollback buffer.
func (s *Store) thinWindow(cutoff int64, w window) error {
	end := cutoff - 1
	if w.end != nil && *w.end < end {
		end = *w.end
	}
	if end < w.start {
		return nil
	}
	_, err := s.db.Exec(`
		DELETE FROM frames
		WHERE ts >= ? AND ts <= ?
		  AND id NOT IN (
		    SELECT MIN(id) FROM frames
		    WHERE ts >= ? AND ts <= ?
		    GROUP BY ts / ?
		  )`, w.start, end, w.start, end, ThinInterval)
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

// ActiveJob returns the print still in progress — the newest row with no end
// time — or nil when none is open.
func (s *Store) ActiveJob() (*Job, error) {
	row := s.db.QueryRow(`SELECT id, name, start_ts FROM jobs WHERE end_ts IS NULL ORDER BY start_ts DESC, id DESC LIMIT 1`)
	var j Job
	if err := row.Scan(&j.ID, &j.Name, &j.Start); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &j, nil
}

// CloseJobAtLastFrame closes job id at the last frame recorded inside it, or at
// fallback when it has no surviving footage. The last frame is the right end
// time in both cases that matter: for a print that has just stopped, recording
// ran up to now anyway; for a row stranded by a process that exited mid-print,
// the footage stops where the print did, which is far more honest than the
// moment we happened to notice.
func (s *Store) CloseJobAtLastFrame(id, fallback int64) error {
	var start int64
	if err := s.db.QueryRow(`SELECT start_ts FROM jobs WHERE id = ?`, id).Scan(&start); err != nil {
		return err
	}
	var last sql.NullInt64
	// Bound the search at the next print's start so one row can't claim the
	// footage of everything recorded after it.
	row := s.db.QueryRow(`
		SELECT MAX(ts) FROM frames
		WHERE ts >= ?
		  AND ts < COALESCE((SELECT MIN(start_ts) FROM jobs WHERE start_ts > ?), ?)`,
		start, start, int64(math.MaxInt64))
	if err := row.Scan(&last); err != nil {
		return err
	}
	end := fallback
	if last.Valid && last.Int64 >= start {
		end = last.Int64
	}
	return s.CloseJob(id, end)
}

// CloseOrphanJobs closes every job row left open except the newest, reporting
// how many it closed. Only one print runs at a time, so additional open rows are
// wreckage from a process that exited mid-print back when a restart opened a
// second row instead of adopting the first.
func (s *Store) CloseOrphanJobs() (int, error) {
	rows, err := s.db.Query(`SELECT id, start_ts FROM jobs WHERE end_ts IS NULL ORDER BY start_ts DESC, id DESC`)
	if err != nil {
		return 0, err
	}
	type orphan struct{ id, start int64 }
	var orphans []orphan
	for rows.Next() {
		var o orphan
		if err := rows.Scan(&o.id, &o.start); err != nil {
			rows.Close()
			return 0, err
		}
		orphans = append(orphans, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(orphans) < 2 {
		return 0, nil // only the genuinely-running row, if any
	}

	for _, o := range orphans[1:] { // [0] is the newest: leave it open
		if err := s.CloseJobAtLastFrame(o.id, o.start); err != nil {
			return 0, err
		}
	}
	return len(orphans) - 1, nil
}

// RecentJobs returns every job row currently stored, newest-started first. Rows
// sharing a start second are broken by insertion order, so "newest" is never
// ambiguous — two prints can start in the same second if one is restarted
// immediately.
// Prune keeps this bounded to the prints whose footage is still retained.
func (s *Store) RecentJobs() ([]Job, error) {
	rows, err := s.db.Query(`SELECT id, name, start_ts, end_ts FROM jobs ORDER BY start_ts DESC, id DESC`)
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

// Name identifies this source to the size cap.
func (s *Store) Name() string { return "camera frames" }

// Oldest returns the oldest stored frames for the size cap, oldest first. The
// timestamp is given in milliseconds so frames and event-log entries, which are
// recorded at different precision, can be put in one order.
//
// The order is by id rather than by timestamp, so that it matches how
// DeleteThrough cuts. Frames are recorded as they arrive, so the two agree;
// ordering by one and deleting by the other would not survive them disagreeing.
func (s *Store) Oldest(n int) ([]capacity.Item, error) {
	rows, err := s.db.Query(`
		SELECT id, ts, octet_length(jpeg) FROM frames ORDER BY id ASC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []capacity.Item
	for rows.Next() {
		var item capacity.Item
		if err := rows.Scan(&item.ID, &item.When, &item.Bytes); err != nil {
			return nil, err
		}
		item.When *= 1000
		out = append(out, item)
	}
	return out, rows.Err()
}

// DeleteThrough removes every frame up to and including id. The size cap is
// absolute, so this pays no attention to which print a frame belongs to: unlike
// Prune, it will take footage that KeptJobs protects.
func (s *Store) DeleteThrough(id int64) error {
	_, err := s.db.Exec(`DELETE FROM frames WHERE id <= ?`, id)
	return err
}
