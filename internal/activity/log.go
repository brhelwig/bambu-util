// Package activity is a persistent event log of commands sent to the printer
// (and whether they were acknowledged), printer reports, and notifications
// sent. It is bounded by size in bytes, since entries vary widely in size.
package activity

import (
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/brhelwig/bambu-util/internal/capacity"
	"github.com/brhelwig/bambu-util/internal/sqlitedb"
)

// Times are in milliseconds, since a command and its reply often land in the
// same second.
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

// Entry is one logged event. Acked is nil until the printer confirms it.
type Entry struct {
	ID      int64      `json:"id"`
	At      time.Time  `json:"at"`
	Kind    string     `json:"kind"`
	Summary string     `json:"summary"`
	Payload string     `json:"payload,omitempty"`
	Acked   *time.Time `json:"acked,omitempty"`
	Error   string     `json:"error,omitempty"`
}

// maxPayload caps how much of one payload is kept (the first report after
// connecting is the printer's whole state).
const maxPayload = 4096

// rowOverhead approximates the columns sizeExpr does not measure (id,
// timestamps, kind), so many tiny entries still count against the budget.
const rowOverhead = 64

// lowWater is how far under the limit a trim cuts, so a full log isn't
// trimmed on every insert.
const lowWater = 0.9

// sizeExpr is what one stored row costs, in bytes (octet_length, not the
// character count length gives).
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

// New returns a log over db, creating its table if needed. limit returns the
// budget in bytes and is read on every write. The caller keeps ownership of db.
func New(db *sql.DB, limit func() int64) (*Log, error) {
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	l := &Log{db: db, limit: limit, now: time.Now}
	// Count once; writes keep the total up to date after that.
	if err := db.QueryRow(`SELECT COALESCE(SUM(` + sizeExpr + `), 0) FROM activity`).Scan(&l.bytes); err != nil {
		return nil, err
	}
	return l, nil
}

// Close closes the database if this store opened it.
func (a *Log) Close() error {
	if !a.owned {
		return nil
	}
	return a.db.Close()
}

// Record adds an entry and returns it, so it can be acknowledged later. A nil
// log records nothing. A failed write is logged and returns nil; it never
// blocks the command being recorded.
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

// Acknowledge records when the broker confirmed a message, or the error if it
// didn't. A nil entry is ignored. The row may already have been trimmed, in
// which case nothing changes.
func (a *Log) Acknowledge(entry *Entry, at time.Time, err error) {
	if a == nil || entry == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	if err == nil {
		// Fixed-size column, covered by rowOverhead.
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
	// Only count the error if the row still existed.
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

	// Walk from the oldest until enough is freed, then delete up to that id.
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

// Entries returns at most limit entries, newest first.
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

// Name identifies this source to the size cap.
func (a *Log) Name() string { return "event log" }

// Oldest returns the oldest stored entries for the size cap, oldest first.
func (a *Log) Oldest(n int) ([]capacity.Item, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	rows, err := a.db.Query(`SELECT id, at, `+sizeExpr+` FROM activity ORDER BY id ASC LIMIT ?`, n)
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
		out = append(out, item)
	}
	return out, rows.Err()
}

// DeleteThrough removes every entry up to and including id, then recounts the
// running total.
func (a *Log) DeleteThrough(id int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, err := a.db.Exec(`DELETE FROM activity WHERE id <= ?`, id); err != nil {
		return err
	}
	return a.db.QueryRow(`SELECT COALESCE(SUM(` + sizeExpr + `), 0) FROM activity`).Scan(&a.bytes)
}
