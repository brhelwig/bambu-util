package web

import (
	"crypto/ecdh"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brhelwig/bambu-util/internal/activity"
	"github.com/brhelwig/bambu-util/internal/p1s"
	"github.com/brhelwig/bambu-util/internal/push"
)

type eventsFixture struct {
	events *printEvents
	now    time.Time
}

func newEventsFixture() *eventsFixture {
	f := &eventsFixture{events: newPrintEvents(nil), now: time.Unix(1_000_000, 0)}
	f.events.now = func() time.Time { return f.now }
	return f
}

// poll with the arguments a settled, connected, idle printer would report.
func (f *eventsFixture) poll(gs, jobName string) []push.Notification {
	return f.events.poll(true, gs, jobName, 0, nil)
}

func titles(ns []push.Notification) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.Title
	}
	return out
}

func onlyTitle(t *testing.T, ns []push.Notification, want string) push.Notification {
	t.Helper()
	if len(ns) != 1 || ns[0].Title != want {
		t.Fatalf("notifications = %v, want exactly [%q]", titles(ns), want)
	}
	return ns[0]
}

func expectSilence(t *testing.T, ns []push.Notification) {
	t.Helper()
	if len(ns) != 0 {
		t.Fatalf("expected nothing, got %v", titles(ns))
	}
}

// Meeting the printer is not an event. Announcing a print that finished hours
// ago, because this process has only just started, is indistinguishable from
// the real thing.
func TestTheFirstLookNeverNotifies(t *testing.T) {
	for _, state := range []string{"RUNNING", "FINISH", "FAILED", "IDLE"} {
		t.Run(state, func(t *testing.T) {
			f := newEventsFixture()
			expectSilence(t, f.poll(state, "benchy.gcode"))
		})
	}
}

func TestPrintStartedFinishedAndEnded(t *testing.T) {
	for _, tc := range []struct{ end, title string }{
		{"FINISH", "Print finished"},
		{"FAILED", "Print ended without finishing"},
	} {
		t.Run(tc.end, func(t *testing.T) {
			f := newEventsFixture()
			expectSilence(t, f.poll("IDLE", ""))

			got := onlyTitle(t, f.poll("RUNNING", "benchy.gcode"), "Print started")
			if got.Body != "benchy.gcode" {
				t.Errorf("body = %q, want the job name", got.Body)
			}
			expectSilence(t, f.poll("RUNNING", "benchy.gcode"))

			got = onlyTitle(t, f.poll(tc.end, "benchy.gcode"), tc.title)
			if got.Body != "benchy.gcode" {
				t.Errorf("body = %q, want the job name", got.Body)
			}
			// The printer sits in the finished state; it must not keep saying so.
			expectSilence(t, f.poll(tc.end, "benchy.gcode"))
			expectSilence(t, f.poll(tc.end, "benchy.gcode"))
		})
	}
}

// The printer reports the same state whether it gave up or someone pressed
// Stop, so the wording must not claim to know which.
func TestAnEndedPrintIsNotCalledAFailure(t *testing.T) {
	f := newEventsFixture()
	f.poll("IDLE", "")
	f.poll("RUNNING", "benchy.gcode")
	got := onlyTitle(t, f.poll("FAILED", "benchy.gcode"), "Print ended without finishing")
	for _, word := range []string{"fail", "error", "crash"} {
		if strings.Contains(strings.ToLower(got.Title+got.Body), word) {
			t.Errorf("wording claims to know the cause: %q / %q", got.Title, got.Body)
		}
	}
}

// Pausing is not the end of a print, and resuming is not a new one.
func TestPausingAndResumingIsNotAJobBoundary(t *testing.T) {
	f := newEventsFixture()
	f.poll("IDLE", "")
	onlyTitle(t, f.poll("RUNNING", "benchy.gcode"), "Print started")
	expectSilence(t, f.poll("PAUSE", "benchy.gcode"))
	expectSilence(t, f.poll("RUNNING", "benchy.gcode"))
	onlyTitle(t, f.poll("FINISH", "benchy.gcode"), "Print finished")
}

func TestAPrintWithNoNameStillReads(t *testing.T) {
	f := newEventsFixture()
	f.poll("IDLE", "")
	got := onlyTitle(t, f.poll("RUNNING", ""), "Print started")
	if got.Body == "" {
		t.Error("body is empty, leaving the notification with no second line")
	}
}

// A dropped link must not look like a print ending.
func TestADisconnectedPrinterReportsNothing(t *testing.T) {
	f := newEventsFixture()
	f.poll("IDLE", "")
	onlyTitle(t, f.poll("RUNNING", "benchy.gcode"), "Print started")
	expectSilence(t, f.events.poll(false, "IDLE", "", 0, nil))
	expectSilence(t, f.events.poll(false, "FINISH", "", 0, nil))
	// Once it is back, a real change is still reported.
	onlyTitle(t, f.poll("FINISH", "benchy.gcode"), "Print finished")
}

func hms(codes ...string) []p1s.HMSEntry {
	out := make([]p1s.HMSEntry, len(codes))
	for i, c := range codes {
		out[i] = p1s.HMSEntry{Code: c, Message: "message for " + c}
	}
	return out
}

func TestOnlyNewErrorsAreAnnounced(t *testing.T) {
	f := newEventsFixture()
	expectSilence(t, f.events.poll(true, "IDLE", "", 0, nil))

	got := onlyTitle(t, f.events.poll(true, "IDLE", "", 0, hms("0300-8000-0003-0002")), "Printer error")
	if got.Body != "message for 0300-8000-0003-0002" {
		t.Errorf("body = %q, want the decoded message", got.Body)
	}
	// Still standing is not news.
	expectSilence(t, f.events.poll(true, "IDLE", "", 0, hms("0300-8000-0003-0002")))
	// A second, different fault is.
	onlyTitle(t, f.events.poll(true, "IDLE", "", 0, hms("0300-8000-0003-0002", "0500-0100-0001-0003")), "Printer error")
}

// A fault that clears and comes back is news again — otherwise a recurring
// problem is announced once and then silently repeats.
func TestAClearedErrorCanAnnounceItselfAgain(t *testing.T) {
	f := newEventsFixture()
	f.events.poll(true, "IDLE", "", 0, nil)
	onlyTitle(t, f.events.poll(true, "IDLE", "", 0, hms("0300-8000-0003-0002")), "Printer error")
	expectSilence(t, f.events.poll(true, "IDLE", "", 0, nil))
	onlyTitle(t, f.events.poll(true, "IDLE", "", 0, hms("0300-8000-0003-0002")), "Printer error")
}

// Errors already standing when the app starts are recorded, not announced.
func TestErrorsPresentAtStartupAreNotAnnounced(t *testing.T) {
	f := newEventsFixture()
	expectSilence(t, f.events.poll(true, "IDLE", "", 0, hms("0300-8000-0003-0002")))
	expectSilence(t, f.events.poll(true, "IDLE", "", 0, hms("0300-8000-0003-0002")))
}

// The reminders themselves are per device now, so all this side has to do is
// know when the bed came on with no print running.
func TestTheBedOnClockStartsAndStops(t *testing.T) {
	f := newEventsFixture()
	f.events.poll(true, "IDLE", "", 0, nil)
	if got := f.events.bedOnSince(); !got.IsZero() {
		t.Errorf("a cold bed reports a start time: %v", got)
	}

	f.events.poll(true, "IDLE", "", 60, nil)
	if got := f.events.bedOnSince(); !got.Equal(f.now) {
		t.Errorf("bed on since = %v, want %v", got, f.now)
	}

	// The clock keeps running rather than restarting on each look.
	started := f.events.bedOnSince()
	f.now = f.now.Add(3 * time.Hour)
	f.events.poll(true, "IDLE", "", 60, nil)
	if got := f.events.bedOnSince(); !got.Equal(started) {
		t.Errorf("the clock restarted: %v, want %v", got, started)
	}

	f.events.poll(true, "IDLE", "", 0, nil)
	if got := f.events.bedOnSince(); !got.IsZero() {
		t.Errorf("turning the bed off left a start time: %v", got)
	}
}

// A hot bed during a print is the printer doing its job, not something to
// measure.
func TestTheBedOnClockIgnoresAPrint(t *testing.T) {
	f := newEventsFixture()
	f.events.poll(true, "IDLE", "", 0, nil)
	f.events.poll(true, "RUNNING", "benchy.gcode", 60, nil)
	if got := f.events.bedOnSince(); !got.IsZero() {
		t.Errorf("a bed hot mid-print started the clock: %v", got)
	}

	// It starts once the print is over and the bed is still on.
	f.events.poll(true, "FINISH", "benchy.gcode", 60, nil)
	if got := f.events.bedOnSince(); got.IsZero() {
		t.Error("the clock did not start when the print ended with the bed still on")
	}
}

func TestAFinishedPrintReachesASubscribedPhone(t *testing.T) {
	var mu sync.Mutex
	var delivered int
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		if len(body) > 0 {
			delivered++
		}
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer svc.Close()

	cache := p1s.NewStateCache()
	cache.SetConnected(true)
	notifier := openTestNotifier()
	srv := NewServer(cache, &fakeCommander{}, openTestStore(), notifier, nil, testSettings, nil, testPrinter(), activity.New(50))

	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	auth := make([]byte, 16)
	rand.Read(auth)
	sub := push.Subscription{Endpoint: svc.URL + "/push/1", P256dh: key.PublicKey().Bytes(), Auth: auth}
	if err := notifier.Subscribe(sub, 0); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	cache.Merge(map[string]any{"gcode_state": "RUNNING", "subtask_name": "benchy.gcode"})
	srv.pollEvents() // the first look only records where things stand
	mu.Lock()
	if delivered != 0 {
		t.Fatalf("the first look sent %d notifications", delivered)
	}
	mu.Unlock()

	cache.Merge(map[string]any{"gcode_state": "FINISH"})
	srv.pollEvents()
	mu.Lock()
	defer mu.Unlock()
	if delivered != 1 {
		t.Errorf("the push service received %d messages, want 1", delivered)
	}
}
