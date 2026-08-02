package activity

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brhelwig/bambu-util/internal/sqlitedb"
)

// generous is a budget no test reaches by accident, so only the tests that are
// about trimming ever trim.
const generous = 1 << 20

func openTest(t *testing.T, limit int64) *Log {
	t.Helper()
	l, err := Open(":memory:", func() int64 { return limit })
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// newest reads the entries back oldest-first, which is how these tests read.
func newest(t *testing.T, l *Log, n int) []Entry {
	t.Helper()
	got := l.Entries(n)
	for i, j := 0, len(got)-1; i < j; i, j = i+1, j-1 {
		got[i], got[j] = got[j], got[i]
	}
	return got
}

func TestRecordsWhatHappenedInOrder(t *testing.T) {
	l := openTest(t, generous)
	l.Record(Command, "pause", `{"print":{"command":"pause"}}`)
	l.Record(Report, "report", `{"print":{"gcode_state":"PAUSE"}}`)
	l.Record(Notification, "Print finished → 1 of 1 devices", "benchy.gcode")

	got := newest(t, l, 10)
	if len(got) != 3 {
		t.Fatalf("kept %d entries, want 3", len(got))
	}
	if got[0].Kind != Command || got[1].Kind != Report || got[2].Kind != Notification {
		t.Errorf("kinds = %s/%s/%s", got[0].Kind, got[1].Kind, got[2].Kind)
	}
	if got[0].ID >= got[1].ID || got[1].ID >= got[2].ID {
		t.Error("entries are not numbered in the order they happened")
	}
}

// The whole point of the change: the log is wanted after a restart, which is
// when it used to have nothing to show.
func TestEntriesSurviveReopeningTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bambu-util.db")
	limit := func() int64 { return generous }

	first, err := Open(path, limit)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	first.Record(Command, "stop", `{"print":{"command":"stop"}}`)
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := Open(path, limit)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()

	got := second.Entries(10)
	if len(got) != 1 {
		t.Fatalf("kept %d entries across a restart, want 1", len(got))
	}
	if got[0].Summary != "stop" {
		t.Errorf("summary = %q, want stop", got[0].Summary)
	}
	if got[0].Payload != `{"print":{"command":"stop"}}` {
		t.Errorf("payload did not survive: %q", got[0].Payload)
	}
}

// A reopened log has to know what is already stored, or the budget starts again
// from zero and the log grows without bound across restarts.
func TestAReopenedLogCountsWhatIsAlreadyThere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bambu-util.db")
	limit := func() int64 { return generous }

	first, err := Open(path, limit)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for range 20 {
		first.Record(Report, "report", strings.Repeat("x", 500))
	}
	stored := first.bytes
	first.Close()

	second, err := Open(path, limit)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
	if second.bytes != stored {
		t.Errorf("counted %d bytes after reopening, want the %d already stored", second.bytes, stored)
	}
}

// A size is the bound, so what has to go is the oldest, and enough of it.
func TestTheOldestGoWhenTheBudgetIsSpent(t *testing.T) {
	const limit = 8000
	l := openTest(t, limit)
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		l.Record(Command, name, strings.Repeat("x", 900))
	}
	if l.bytes > limit {
		t.Errorf("log holds %d bytes, over its %d limit", l.bytes, limit)
	}

	got := newest(t, l, 100)
	if len(got) == 0 {
		t.Fatal("trimming emptied the log")
	}
	if got[len(got)-1].Summary != "j" {
		t.Errorf("newest = %q, want the last one recorded", got[len(got)-1].Summary)
	}
	if got[0].Summary == "a" {
		t.Error("the oldest entry survived a trim that had to free space")
	}
	// What the log believes it holds must match what is really stored, or the
	// budget drifts until it means nothing.
	if want := sum(t, l.db); l.bytes != want {
		t.Errorf("log counts %d bytes, database holds %d", l.bytes, want)
	}
}

// length() counts characters and octet_length() counts bytes. Getting this
// wrong lets a log of non-ASCII text quietly grow past its budget.
func TestAMultiByteEntryIsChargedItsBytes(t *testing.T) {
	l := openTest(t, generous)
	// Each 'é' is two bytes and one character.
	l.Record(Report, "report", strings.Repeat("é", 100))
	if got, want := sum(t, l.db), int64(200+len("report")+rowOverhead); got != want {
		t.Errorf("charged %d bytes, want %d — a character count would say %d", got, want, want-100)
	}
}

// The printer's first report after connecting is its entire state.
func TestAHugePayloadIsCutDown(t *testing.T) {
	l := openTest(t, generous)
	l.Record(Report, "report", strings.Repeat("x", maxPayload*3))
	got := l.Entries(1)[0]
	if len(got.Payload) > maxPayload+32 {
		t.Errorf("kept %d bytes, want it cut to about %d", len(got.Payload), maxPayload)
	}
	if !strings.HasSuffix(got.Payload, "(truncated)") {
		t.Error("a cut payload does not say it was cut")
	}
}

// Lowering the setting has to take hold without a restart.
func TestLoweringTheLimitShrinksTheLogOnTheNextEntry(t *testing.T) {
	limit := int64(generous)
	l, err := Open(":memory:", func() int64 { return limit })
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer l.Close()

	for range 50 {
		l.Record(Report, "report", strings.Repeat("x", 500))
	}
	before := l.bytes

	limit = 4000
	l.Record(Command, "stop", "")
	if l.bytes > limit {
		t.Errorf("log holds %d bytes after the limit dropped to %d", l.bytes, limit)
	}
	if l.bytes >= before {
		t.Error("lowering the limit did not shrink the log")
	}
}

// The difference between sent and acknowledged is the whole point.
func TestACommandStartsUnacknowledged(t *testing.T) {
	l := openTest(t, generous)
	entry := l.Record(Command, "stop", "")
	if l.Entries(1)[0].Acked != nil {
		t.Fatal("a command was acknowledged before the printer answered")
	}

	at := time.UnixMilli(1_700_000_000_000)
	l.Acknowledge(entry, at, nil)
	got := l.Entries(1)[0]
	if got.Acked == nil || !got.Acked.Equal(at) {
		t.Errorf("acknowledged at %v, want %v", got.Acked, at)
	}
	if got.Error != "" {
		t.Errorf("a successful command carries an error: %q", got.Error)
	}
}

func TestACommandThatWasNeverAcknowledgedSaysWhy(t *testing.T) {
	l := openTest(t, generous)
	entry := l.Record(Command, "stop", "")
	l.Acknowledge(entry, time.Time{}, errors.New("no acknowledgement"))
	got := l.Entries(1)[0]
	if got.Acked != nil {
		t.Error("a failed command reads as acknowledged")
	}
	if got.Error != "no acknowledgement" {
		t.Errorf("error = %q", got.Error)
	}
}

// The printer's broker can answer half a minute later, by which time the entry
// may have been trimmed away. That must not be an error.
func TestAcknowledgingAnEntryThatIsGoneIsHarmless(t *testing.T) {
	l := openTest(t, generous)
	entry := l.Record(Command, "stop", "")
	if _, err := l.db.Exec(`DELETE FROM activity`); err != nil {
		t.Fatalf("clear: %v", err)
	}
	l.Acknowledge(entry, time.Now(), nil)
	if got := l.Entries(10); len(got) != 0 {
		t.Errorf("acknowledging brought back %d entries", len(got))
	}
}

// Charging for an entry that is no longer there would push the running total
// permanently above what is stored, and the log would trim ever harder.
func TestAFailureRecordedAgainstAGoneEntryIsNotCharged(t *testing.T) {
	l := openTest(t, generous)
	entry := l.Record(Command, "stop", "")
	if _, err := l.db.Exec(`DELETE FROM activity`); err != nil {
		t.Fatalf("clear: %v", err)
	}
	l.bytes = 0 // what the delete left behind
	l.Acknowledge(entry, time.Time{}, errors.New("no acknowledgement"))
	if l.bytes != 0 {
		t.Errorf("log counts %d bytes, but nothing is stored", l.bytes)
	}
}

// The page asks for a bounded number, because the log holds far more than it
// can draw.
func TestOnlyTheNewestAreServed(t *testing.T) {
	l := openTest(t, generous)
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		l.Record(Command, name, "")
	}
	got := l.Entries(2)
	if len(got) != 2 {
		t.Fatalf("served %d entries, want 2", len(got))
	}
	if got[0].Summary != "e" || got[1].Summary != "d" {
		t.Errorf("served %s then %s, want the newest two, newest first", got[0].Summary, got[1].Summary)
	}
}

// Nothing should have to check whether logging is switched on before doing it.
func TestANilLogIsHarmless(t *testing.T) {
	var l *Log
	entry := l.Record(Command, "stop", "")
	l.Acknowledge(entry, time.Now(), nil)
	if got := l.Entries(10); got != nil {
		t.Errorf("Entries = %v, want nothing", got)
	}
}

// The log shares the app's one database rather than opening its own.
func TestItSharesTheAppDatabase(t *testing.T) {
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()

	l, err := New(db, func() int64 { return generous })
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	l.Record(Command, "stop", "")
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Closing a log it does not own must leave the database up for everything
	// else sharing it.
	if _, err := db.Exec(`SELECT 1`); err != nil {
		t.Errorf("the shared database went down with the log: %v", err)
	}
}

func sum(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var total int64
	if err := db.QueryRow(`SELECT COALESCE(SUM(` + sizeExpr + `), 0) FROM activity`).Scan(&total); err != nil {
		t.Fatalf("sum: %v", err)
	}
	return total
}
