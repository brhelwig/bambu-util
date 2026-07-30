package activity

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRecordsWhatHappenedInOrder(t *testing.T) {
	l := New(10)
	l.Record(Command, "pause", `{"print":{"command":"pause"}}`)
	l.Record(Report, "report", `{"print":{"gcode_state":"PAUSE"}}`)
	l.Record(Notification, "Print finished → 1 of 1 devices", "benchy.gcode")

	got := l.Entries()
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

// This is for looking at what just happened, so old entries have to go rather
// than the log growing without limit.
func TestOnlyTheMostRecentAreKept(t *testing.T) {
	l := New(3)
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		l.Record(Command, name, "")
	}
	got := l.Entries()
	if len(got) != 3 {
		t.Fatalf("kept %d entries, want 3", len(got))
	}
	if got[0].Summary != "c" || got[2].Summary != "e" {
		t.Errorf("kept %s..%s, want the newest three", got[0].Summary, got[2].Summary)
	}
}

// The printer's first report after connecting is its entire state, and would
// otherwise push out the recent traffic this exists to show.
func TestAHugePayloadIsCutDown(t *testing.T) {
	l := New(5)
	l.Record(Report, "report", strings.Repeat("x", maxPayload*3))
	got := l.Entries()[0]
	if len(got.Payload) > maxPayload+32 {
		t.Errorf("kept %d bytes, want it cut to about %d", len(got.Payload), maxPayload)
	}
	if !strings.HasSuffix(got.Payload, "(truncated)") {
		t.Error("a cut payload does not say it was cut")
	}
}

// The difference between sent and acknowledged is the whole point.
func TestACommandStartsUnacknowledged(t *testing.T) {
	l := New(5)
	entry := l.Record(Command, "stop", "")
	if l.Entries()[0].Acked != nil {
		t.Fatal("a command was acknowledged before the printer answered")
	}

	at := time.Unix(1_700_000_000, 0)
	l.Acknowledge(entry, at, nil)
	got := l.Entries()[0]
	if got.Acked == nil || !got.Acked.Equal(at) {
		t.Errorf("acknowledged at %v, want %v", got.Acked, at)
	}
	if got.Error != "" {
		t.Errorf("a successful command carries an error: %q", got.Error)
	}
}

func TestACommandThatWasNeverAcknowledgedSaysWhy(t *testing.T) {
	l := New(5)
	entry := l.Record(Command, "stop", "")
	l.Acknowledge(entry, time.Time{}, errors.New("no acknowledgement"))
	got := l.Entries()[0]
	if got.Acked != nil {
		t.Error("a failed command reads as acknowledged")
	}
	if got.Error != "no acknowledgement" {
		t.Errorf("error = %q", got.Error)
	}
}

// Reading the log must not race with a command being acknowledged behind it.
func TestEntriesAreACopy(t *testing.T) {
	l := New(5)
	entry := l.Record(Command, "stop", "")
	before := l.Entries()
	l.Acknowledge(entry, time.Unix(1, 0), nil)
	if before[0].Acked != nil {
		t.Error("an already-read entry changed underneath the reader")
	}
}

// Nothing should have to check whether logging is switched on before doing it.
func TestANilLogIsHarmless(t *testing.T) {
	var l *Log
	entry := l.Record(Command, "stop", "")
	l.Acknowledge(entry, time.Now(), nil)
	if got := l.Entries(); got != nil {
		t.Errorf("Entries = %v, want nothing", got)
	}
}
