// Package activity records what the app did and what was done to it — commands
// sent to the printer, what the printer reported back, and notifications sent
// out — so a command can be seen leaving and being acknowledged rather than the
// page merely claiming it was sent.
//
// It is kept in the database, because the log is most wanted after something
// went wrong and the app was restarted, which is exactly when a log held in
// memory has nothing to show. What bounds it is a size rather than a count of
// entries, since one entry ranges from a few bytes to a whole printer state.
package activity

import (
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/brhelwig/bambu-util/internal/sqlitedb"
)

// Times are stored in milliseconds rather than the whole seconds the camera
// history uses: a command and the report answering it routinely land in the
// same second, and the order of exactly that is what the log is read for.
const schema = `
CREATE TABLE IF NOT EXISTS activity (
  id      INTEGER PRIMARY KEY,
  at      INTEGER NOT NULL,
  kind    TEXT NOT NULL,
  summary TEXT NOT NULL,
  payload TEXT NOT NULL DEFAULT '',
  acked   INTEGER,
  error   TEXT NOT NULL DEFAULT ''
);
`

// What kind of thing an entry records.
const (
	Command      = "command"      // sent to the printer
	Report       = "report"       // received from the printer
	Notification = "notification" // sent to subscribed devices
)

// Entry is one thing that happened. Acked is nil for anything not yet
// confirmed, which for a command means the printer has not answered — the
// difference this whole thing exists to show.
type Entry struct {
	ID      int64      `json:"id"`
	At      time.Time  `json:"at"`
	Kind    string     `json:"kind"`
	Summary string     `json:"summary"`
	Payload string     `json:"payload,omitempty"`
	Acked   *time.Time `json:"acked,omitempty"`
	Error   string     `json:"error,omitempty"`
}

// maxPayload caps how much of one payload is kept. The printer's first report
// after connecting is its entire state and dwarfs everything else; keeping all
// of it would spend much of the budget on one entry.
const maxPayload = 4096

// rowOverhead stands for the columns whose size does not vary — the id, the two
// timestamps and the kind. Without it a flood of tiny entries would count as
// costing almost nothing while still filling the disk.
const rowOverhead = 64

// lowWater is how far under the limit a trim cuts. Trimming exactly to the
// limit would mean a delete on every insert once the log is full; going under
// lets one trim cover many entries.
const lowWater = 0.9

// sizeExpr is what one stored row costs. octet_length rather than length,
// because length counts characters and the printer does not only send ASCII —
// a character count would quietly undercount the budget.
var sizeExpr = fmt.Sprintf(
	"octet_length(summary) + octet_length(payload) + octet_length(error) + %d", rowOverhead)

// Log records what happened, in the database, bounded by a size in bytes.
type Log struct {
	db    *sql.DB
	owned bool
	limit func() int64

	mu    sync.Mutex
	bytes int64 // what the stored entries currently come to
	now   func() time.Time
}

// Open makes a log over a database of its own at path, which Close then closes.
// The app shares one database across stores and calls New instead.
func Open(path string, limit func() int64) (*Log, error) {
	db, err := sqlitedb.Open(path)
	if err != nil {
		return nil, err
	}
	l, err := New(db, limit)
	if err != nil {
		db.Close()
		return nil, err
	}
	l.owned = true
	return l, nil
}

// New returns a log over db, creating its table if needed. limit is the budget
// in bytes, read on every write rather than held, so lowering it on the
// settings page takes effect on the next entry instead of at the next restart.
// The caller keeps ownership of db.
func New(db *sql.DB, limit func() int64) (*Log, error) {
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	l := &Log{db: db, limit: limit, now: time.Now}
	// What is already stored is counted once here; every write keeps the figure
	// up to date after that, so the budget costs no scan per entry.
	if err := db.QueryRow(`SELECT COALESCE(SUM(` + sizeExpr + `), 0) FROM activity`).Scan(&l.bytes); err != nil {
		return nil, err
	}
	return l, nil
}

// Close closes the database, unless it belongs to whoever passed it in —
// closing a shared handle would take every other store down with it.
func (a *Log) Close() error {
	if !a.owned {
		return nil
	}
	return a.db.Close()
}

// Record adds an entry and returns it, so a command can be marked acknowledged
// later. A nil log records nothing, so nothing has to check before calling.
//
// A database that will not take the entry costs the entry, not the command: the
// printer must do what it was told whether or not the log kept a note of it.
func (a *Log) Record(kind, summary, payload string) *Entry {
	if a == nil {
		return nil
	}
	if len(payload) > maxPayload {
		payload = payload[:maxPayload] + "… (truncated)"
	}
	entry := &Entry{At: a.now(), Kind: kind, Summary: summary, Payload: payload}

	a.mu.Lock()
	defer a.mu.Unlock()
	res, err := a.db.Exec(`INSERT INTO activity (at, kind, summary, payload) VALUES (?, ?, ?, ?)`,
		entry.At.UnixMilli(), entry.Kind, entry.Summary, entry.Payload)
	if err != nil {
		log.Printf("activity: recording %s %q: %v", kind, summary, err)
		return nil
	}
	if entry.ID, err = res.LastInsertId(); err != nil {
		log.Printf("activity: recording %s %q: %v", kind, summary, err)
		return nil
	}
	a.bytes += int64(len(entry.Summary)+len(entry.Payload)) + rowOverhead
	a.trim()
	return entry
}

// Acknowledge marks when the printer's broker confirmed a message, or why it
// did not. A nil entry is ignored, so callers need not check.
//
// The answer can arrive half a minute after the command, by which time trimming
// may have taken the row; the update then changes nothing, which is right.
func (a *Log) Acknowledge(entry *Entry, at time.Time, err error) {
	if a == nil || entry == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	if err == nil {
		// The acknowledged time is a fixed-size column, already covered by the
		// per-row overhead, so nothing is added to the total.
		if _, dbErr := a.db.Exec(`UPDATE activity SET acked = ? WHERE id = ?`, at.UnixMilli(), entry.ID); dbErr != nil {
			log.Printf("activity: acknowledging %d: %v", entry.ID, dbErr)
		}
		return
	}

	res, dbErr := a.db.Exec(`UPDATE activity SET error = ? WHERE id = ?`, err.Error(), entry.ID)
	if dbErr != nil {
		log.Printf("activity: acknowledging %d: %v", entry.ID, dbErr)
		return
	}
	// Only what actually landed is charged. An entry trimmed away while the
	// broker was still deciding updates nothing, and charging for it anyway
	// would push the running total above what is stored and never come back.
	changed, dbErr := res.RowsAffected()
	if dbErr != nil {
		log.Printf("activity: acknowledging %d: %v", entry.ID, dbErr)
		return
	}
	if changed > 0 {
		a.bytes += int64(len(err.Error()))
		a.trim()
	}
}

// trim deletes the oldest entries until the log is back under its limit. The
// caller holds the lock.
func (a *Log) trim() {
	limit := a.limit()
	if limit <= 0 || a.bytes <= limit {
		return
	}
	target := int64(float64(limit) * lowWater)

	// Ids only ever rise and only the oldest ever go, so how far to delete is a
	// walk from the oldest end until enough has been freed.
	rows, err := a.db.Query(`SELECT id, ` + sizeExpr + ` FROM activity ORDER BY id ASC`)
	if err != nil {
		log.Printf("activity: trimming: %v", err)
		return
	}
	var threshold, freed int64
	for rows.Next() {
		var id, size int64
		if err := rows.Scan(&id, &size); err != nil {
			rows.Close()
			log.Printf("activity: trimming: %v", err)
			return
		}
		threshold, freed = id, freed+size
		if a.bytes-freed <= target {
			break
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Printf("activity: trimming: %v", err)
		return
	}
	if freed == 0 {
		return
	}
	if _, err := a.db.Exec(`DELETE FROM activity WHERE id <= ?`, threshold); err != nil {
		log.Printf("activity: trimming: %v", err)
		return
	}
	a.bytes -= freed
}

// Entries returns at most limit entries, newest first, which is the order they
// are looked for in. What is kept is bounded by a size, which is far more than
// a page should draw at once, so what is served is bounded here instead.
func (a *Log) Entries(limit int) []Entry {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	rows, err := a.db.Query(`
		SELECT id, at, kind, summary, payload, acked, error
		FROM activity ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		log.Printf("activity: reading: %v", err)
		return nil
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var e Entry
		var at int64
		var acked sql.NullInt64
		if err := rows.Scan(&e.ID, &at, &e.Kind, &e.Summary, &e.Payload, &acked, &e.Error); err != nil {
			log.Printf("activity: reading: %v", err)
			return nil
		}
		e.At = time.UnixMilli(at)
		if acked.Valid {
			t := time.UnixMilli(acked.Int64)
			e.Acked = &t
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		log.Printf("activity: reading: %v", err)
		return nil
	}
	return out
}
