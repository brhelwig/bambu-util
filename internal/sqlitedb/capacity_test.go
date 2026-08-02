// This file checks the size cap against the app's real stores rather than
// stand-ins, so the rule is exercised over the tables it actually governs. It
// lives here for the same reason the other shared test does: outside the
// packages, so it may import them all.
package sqlitedb_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/brhelwig/bambu-util/internal/activity"
	"github.com/brhelwig/bambu-util/internal/capacity"
	"github.com/brhelwig/bambu-util/internal/history"
	"github.com/brhelwig/bambu-util/internal/sqlitedb"
)

// The cap has to hold the file down across both stores at once, taking whatever
// is oldest — a camera frame or an event-log entry — without either store
// knowing about the other.
func TestTheCapHoldsTheRealStoresToASize(t *testing.T) {
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "bambu-util.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	frames, err := history.New(db)
	if err != nil {
		t.Fatalf("history store: %v", err)
	}
	events, err := activity.New(db, func() int64 { return 1 << 30 })
	if err != nil {
		t.Fatalf("event log: %v", err)
	}

	// A print's worth of footage, with log entries interleaved through it.
	jpeg := make([]byte, 8000)
	for i := range 400 {
		if err := frames.InsertFrame(int64(i), jpeg); err != nil {
			t.Fatalf("insert frame: %v", err)
		}
		if i%10 == 0 {
			events.Record(activity.Report, "report", string(make([]byte, 2000)))
		}
	}
	// A print row covering all of it. The cap is absolute, so this must not
	// protect the footage the way retention would.
	if _, err := frames.OpenJob("benchy.gcode", 0); err != nil {
		t.Fatalf("open job: %v", err)
	}

	limit := int64(0)
	e := capacity.New(db, func() int64 { return limit }, frames, events)

	before, err := sizeOf(db)
	if err != nil {
		t.Fatalf("size: %v", err)
	}
	limit = before / 2
	if err := e.Once(); err != nil {
		t.Fatalf("Once: %v", err)
	}
	after, err := sizeOf(db)
	if err != nil {
		t.Fatalf("size: %v", err)
	}
	if after > limit {
		t.Errorf("file is %d bytes, over its %d limit", after, limit)
	}

	// Both stores gave something up, and the newest of each survived.
	oldest, newest, err := frames.Range()
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	if oldest == nil || newest == nil {
		t.Fatal("the cap deleted every frame")
	}
	if *oldest == 0 {
		t.Error("the oldest frame survived a pass that had to free space")
	}
	if *newest != 399 {
		t.Errorf("newest frame is %d, want the last one recorded", *newest)
	}
	if got := events.Entries(1); len(got) == 0 {
		t.Error("the cap deleted every event-log entry")
	}
}

func sizeOf(db *sql.DB) (int64, error) {
	var pages, pageSize int64
	if err := db.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		return 0, err
	}
	if err := db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, err
	}
	return pages * pageSize, nil
}
