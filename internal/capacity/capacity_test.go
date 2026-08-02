package capacity

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brhelwig/bambu-util/internal/sqlitedb"
)

// blobs is a stand-in source over a table of its own, so the rule can be tested
// without the stores that use it in the app.
type blobs struct {
	db    *sql.DB
	table string
}

func newBlobs(t *testing.T, db *sql.DB, table string) *blobs {
	t.Helper()
	if _, err := db.Exec(`CREATE TABLE ` + table + ` (id INTEGER PRIMARY KEY, at INTEGER NOT NULL, body BLOB)`); err != nil {
		t.Fatalf("create %s: %v", table, err)
	}
	return &blobs{db: db, table: table}
}

func (b *blobs) Name() string { return b.table }

func (b *blobs) add(t *testing.T, at int64, size int) {
	t.Helper()
	if _, err := b.db.Exec(`INSERT INTO `+b.table+` (at, body) VALUES (?, ?)`, at, make([]byte, size)); err != nil {
		t.Fatalf("insert into %s: %v", b.table, err)
	}
}

func (b *blobs) Oldest(n int) ([]Item, error) {
	rows, err := b.db.Query(`SELECT id, at, octet_length(body) FROM `+b.table+` ORDER BY at ASC, id ASC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var item Item
		if err := rows.Scan(&item.ID, &item.When, &item.Bytes); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (b *blobs) DeleteThrough(id int64) error {
	_, err := b.db.Exec(`DELETE FROM `+b.table+` WHERE id <= ?`, id)
	return err
}

func (b *blobs) times(t *testing.T) []int64 {
	t.Helper()
	rows, err := b.db.Query(`SELECT at FROM ` + b.table + ` ORDER BY at ASC`)
	if err != nil {
		t.Fatalf("read %s: %v", b.table, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var at int64
		if err := rows.Scan(&at); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, at)
	}
	return out
}

func openTest(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func sizeOf(t *testing.T, e *Enforcer) int64 {
	t.Helper()
	got, err := e.size()
	if err != nil {
		t.Fatalf("size: %v", err)
	}
	return got
}

// The assumption the whole feature rests on: deleting rows does not shrink a
// SQLite file, and returning the pages is a separate step.
func TestDeletingAloneDoesNotShrinkTheFile(t *testing.T) {
	db := openTest(t)
	source := newBlobs(t, db, "frames")
	e := New(db, func() int64 { return 0 }, source)

	for i := range 300 {
		source.add(t, int64(i), 8000)
	}
	before := sizeOf(t, e)

	if err := source.DeleteThrough(200); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if after := sizeOf(t, e); after != before {
		t.Errorf("deleting changed the file size on its own: %d -> %d", before, after)
	}
	if err := e.reclaim(); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if after := sizeOf(t, e); after >= before {
		t.Errorf("reclaiming did not shrink the file: %d -> %d", before, after)
	}
}

func TestTheFileIsBroughtUnderTheLimit(t *testing.T) {
	db := openTest(t)
	source := newBlobs(t, db, "frames")
	limit := int64(0)
	e := New(db, func() int64 { return limit }, source)

	for i := range 600 {
		source.add(t, int64(i), 8000)
	}
	before := sizeOf(t, e)
	limit = before / 2
	if err := e.Once(); err != nil {
		t.Fatalf("Once: %v", err)
	}
	after := sizeOf(t, e)
	if after > limit {
		t.Errorf("file is %d bytes, over its %d limit", after, limit)
	}
	// The newest must survive: this deletes history, not everything.
	left := source.times(t)
	if len(left) == 0 {
		t.Fatal("the cap emptied the table")
	}
	if left[len(left)-1] != 599 {
		t.Errorf("newest kept is %d, want the last one added", left[len(left)-1])
	}
}

// Which table an item is in must not matter — only how old it is.
func TestTheOldestGoFirstWhicheverSourceTheyAreIn(t *testing.T) {
	db := openTest(t)
	frames := newBlobs(t, db, "frames")
	entries := newBlobs(t, db, "activity")
	limit := int64(0)
	e := New(db, func() int64 { return limit }, frames, entries)

	// Interleaved in time: even moments are frames, odd ones are entries.
	for i := range 400 {
		if i%2 == 0 {
			frames.add(t, int64(i), 8000)
		} else {
			entries.add(t, int64(i), 8000)
		}
	}
	limit = sizeOf(t, e) / 2
	if err := e.Once(); err != nil {
		t.Fatalf("Once: %v", err)
	}

	// Both tables must have lost their oldest, and whatever is left in either
	// must be newer than everything deleted from the other.
	frameTimes, entryTimes := frames.times(t), entries.times(t)
	if len(frameTimes) == 0 || len(entryTimes) == 0 {
		t.Fatalf("one source was emptied entirely: %d frames, %d entries", len(frameTimes), len(entryTimes))
	}
	if frameTimes[0] < 100 || entryTimes[0] < 100 {
		t.Errorf("oldest left are frame %d and entry %d, want both well past the start",
			frameTimes[0], entryTimes[0])
	}
}

func TestADatabaseUnderItsLimitIsLeftAlone(t *testing.T) {
	db := openTest(t)
	source := newBlobs(t, db, "frames")
	e := New(db, func() int64 { return 1 << 30 }, source)

	for i := range 100 {
		source.add(t, int64(i), 8000)
	}
	before := sizeOf(t, e)
	if err := e.Once(); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if after := sizeOf(t, e); after != before {
		t.Errorf("file changed though it was under its limit: %d -> %d", before, after)
	}
	if got := len(source.times(t)); got != 100 {
		t.Errorf("%d rows left, want all 100", got)
	}
}

func TestNoLimitMeansNothingIsTouched(t *testing.T) {
	db := openTest(t)
	source := newBlobs(t, db, "frames")
	e := New(db, func() int64 { return 0 }, source)

	for i := range 200 {
		source.add(t, int64(i), 8000)
	}
	before := sizeOf(t, e)
	if err := e.Once(); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if after := sizeOf(t, e); after != before {
		t.Errorf("the file was compacted with the cap switched off: %d -> %d", before, after)
	}
	if got := len(source.times(t)); got != 200 {
		t.Errorf("%d rows left, want all 200", got)
	}
}

// Nothing left to delete must end the pass rather than spin.
func TestItStopsWhenThereIsNothingLeftToDelete(t *testing.T) {
	db := openTest(t)
	source := newBlobs(t, db, "frames")
	e := New(db, func() int64 { return 1 }, source)

	source.add(t, 1, 8000)
	if err := e.Once(); err != nil {
		t.Fatalf("Once: %v", err)
	}
}

// A database made before the cap existed cannot return space until it is
// converted, and the conversion must not cost it write-ahead logging.
func TestAnOlderDatabaseIsConvertedOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	// The connection string the app used before this feature: no auto-vacuum.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(wal)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	source := newBlobs(t, db, "frames")
	limit := int64(0)
	e := New(db, func() int64 { return limit }, source)
	for i := range 400 {
		source.add(t, int64(i), 8000)
	}

	var mode int
	if err := db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		t.Fatalf("read mode: %v", err)
	}
	if mode != 0 {
		t.Fatalf("this database should start unconverted, got mode %d", mode)
	}

	limit = sizeOf(t, e) / 2
	if err := e.Once(); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if err := db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		t.Fatalf("read mode: %v", err)
	}
	if mode != 2 {
		t.Errorf("auto-vacuum mode = %d, want 2 after conversion", mode)
	}
	var journal string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatalf("read journal mode: %v", err)
	}
	if journal != "wal" {
		t.Errorf("journal mode = %q, want wal to survive the conversion", journal)
	}
	if after := sizeOf(t, e); after > limit {
		t.Errorf("file is %d bytes, over its %d limit after conversion", after, limit)
	}
}

// A database the app made itself needs no conversion at all.
func TestAFreshDatabaseNeedsNoConversion(t *testing.T) {
	db := openTest(t)
	var mode int
	if err := db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		t.Fatalf("read mode: %v", err)
	}
	if mode != 2 {
		t.Errorf("auto-vacuum mode = %d, want 2 straight from sqlitedb.Open", mode)
	}
}

// One pass must not run a huge database all the way down while everything else
// waits; it comes down over several.
func TestOnePassDoesABoundedAmountOfWork(t *testing.T) {
	db := openTest(t)
	source := newBlobs(t, db, "frames")
	limit := int64(0)
	e := New(db, func() int64 { return limit }, source)

	// Far more items than one pass may consider: maxRounds passes of batch each.
	for range maxRounds*batch + 5000 {
		source.add(t, 1, 8)
	}
	limit = 4096 // below any achievable size, so the cap can never be satisfied
	if err := e.Once(); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if got := len(source.times(t)); got == 0 {
		t.Error("one pass deleted everything rather than stopping at its bound")
	}
}

// The file on disk must really be what the pragma-based measurement says.
func TestTheMeasurementMatchesTheFileOnDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	db, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	source := newBlobs(t, db, "frames")
	e := New(db, func() int64 { return 0 }, source)
	for i := range 300 {
		source.add(t, int64(i), 8000)
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := sizeOf(t, e); got != info.Size() {
		t.Errorf("measured %d bytes, file on disk is %d", got, info.Size())
	}
}

func TestSourcesAreNamedInErrors(t *testing.T) {
	db := openTest(t)
	source := newBlobs(t, db, "frames")
	if got := source.Name(); !strings.Contains(got, "frames") {
		t.Errorf("Name = %q", got)
	}
}
