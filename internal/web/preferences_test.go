package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/brhelwig/bambu-util/internal/activity"
	"github.com/brhelwig/bambu-util/internal/p1s"
	"github.com/brhelwig/bambu-util/internal/push"
)

// serverWithNotifier returns a running server and the notifier behind it, so a
// test can subscribe a device without going through the browser's half of the
// handshake.
func serverWithNotifier(t *testing.T) (*httptest.Server, *push.Sender) {
	t.Helper()
	notify := openTestNotifier()
	srv := NewServer(p1s.NewStateCache(), &fakeCommander{}, openTestStore(), notify,
		nil, testSettings, nil, testPrinter(), activity.New(50))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, notify
}

func subscribeDevice(t *testing.T, notify *push.Sender, endpoint string) {
	t.Helper()
	sub := push.Subscription{Endpoint: endpoint, P256dh: make([]byte, 65), Auth: make([]byte, 16)}
	if err := notify.Subscribe(sub, 100); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
}

func TestPushPreferencesNeedsAnEndpoint(t *testing.T) {
	ts, _ := serverWithNotifier(t)
	resp, err := ts.Client().Get(ts.URL + "/api/push/preferences")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400 without an endpoint", resp.StatusCode)
	}
}

func TestPushPreferencesOfAnUnknownDevice(t *testing.T) {
	ts, _ := serverWithNotifier(t)
	resp, err := ts.Client().Get(ts.URL + "/api/push/preferences?endpoint=https://push.example/never")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status %d, want 404 for a device that never subscribed", resp.StatusCode)
	}
}

// A device that has chosen nothing is told about everything, so the page has to
// be handed the full set — handing it an empty list would draw every switch off
// and misreport what the device actually gets.
func TestADeviceThatHasChosenNothingIsToldItGetsEverything(t *testing.T) {
	ts, notify := serverWithNotifier(t)
	endpoint := "https://push.example/a"
	subscribeDevice(t, notify, endpoint)

	resp, err := ts.Client().Get(ts.URL + "/api/push/preferences?endpoint=" + endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}

	var got struct {
		Available   []string `json:"available"`
		Kinds       []string `json:"kinds"`
		BedInterval int      `json:"bedInterval"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Kinds) != len(push.Kinds) {
		t.Errorf("kinds = %v, want all of %v", got.Kinds, push.Kinds)
	}
	if len(got.Available) != len(push.Kinds) {
		t.Errorf("available = %v, want %v", got.Available, push.Kinds)
	}
	if got.BedInterval != 0 {
		t.Errorf("bed interval = %d, want 0 for a device that has not asked", got.BedInterval)
	}
}

func TestSetPushPreferencesRoundTrips(t *testing.T) {
	ts, notify := serverWithNotifier(t)
	endpoint := "https://push.example/a"
	subscribeDevice(t, notify, endpoint)

	body := `{"endpoint":"` + endpoint + `","kinds":["printer-error"],"bedInterval":7200}`
	resp, err := ts.Client().Post(ts.URL+"/api/push/preferences", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d, want 204", resp.StatusCode)
	}

	sub, ok, err := notify.Preferences(endpoint)
	if err != nil || !ok {
		t.Fatalf("found = %v (err %v), want the device still there", ok, err)
	}
	if len(sub.Kinds) != 1 || sub.Kinds[0] != push.KindPrinterError {
		t.Errorf("kinds = %v, want just the error kind", sub.Kinds)
	}
	if sub.BedInterval != 2*time.Hour {
		t.Errorf("bed interval = %s, want 2h", sub.BedInterval)
	}
}

// Records what happens today, which is not what it should be: a device that
// switches every notification off is stored the same way as one that has never
// chosen, and Subscription.Wants reads an empty set as "everything". So the
// switches all come back on, and the device keeps receiving all five kinds.
// Pinned here so that fixing it visibly changes this test rather than passing
// unnoticed.
func TestSwitchingEverythingOffCurrentlyLeavesEverythingOn(t *testing.T) {
	ts, notify := serverWithNotifier(t)
	endpoint := "https://push.example/a"
	subscribeDevice(t, notify, endpoint)

	body := `{"endpoint":"` + endpoint + `","kinds":[],"bedInterval":0}`
	resp, err := ts.Client().Post(ts.URL+"/api/push/preferences", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d, want 204", resp.StatusCode)
	}

	got, err := ts.Client().Get(ts.URL + "/api/push/preferences?endpoint=" + endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()
	var prefs struct {
		Kinds []string `json:"kinds"`
	}
	json.NewDecoder(got.Body).Decode(&prefs)
	if len(prefs.Kinds) != len(push.Kinds) {
		t.Errorf("kinds = %v, want the current (wrong) behaviour of reporting all of %v", prefs.Kinds, push.Kinds)
	}

	sub, ok, err := notify.Preferences(endpoint)
	if err != nil || !ok {
		t.Fatalf("found = %v (err %v)", ok, err)
	}
	for _, kind := range push.Kinds {
		if !sub.Wants(kind) {
			t.Errorf("Wants(%q) = false; if this fails the bug is fixed and this test should assert the opposite", kind)
		}
	}
}

func TestSetPushPreferencesRejectsWhatItShould(t *testing.T) {
	ts, _ := serverWithNotifier(t)
	for name, body := range map[string]string{
		"not json":      `{`,
		"no endpoint":   `{"kinds":["printer-error"]}`,
		"empty request": `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := ts.Client().Post(ts.URL+"/api/push/preferences", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status %d, want 400", resp.StatusCode)
			}
		})
	}
}

// The log is read to find out what just happened, so the most recent thing has
// to be at the top.
func TestEventsAreReportedNewestFirst(t *testing.T) {
	log := activity.New(50)
	srv := NewServer(p1s.NewStateCache(), &fakeCommander{}, openTestStore(), openTestNotifier(),
		nil, testSettings, nil, testPrinter(), log)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	log.Record(activity.Command, "first", "{}")
	log.Record(activity.Command, "second", "{}")
	log.Record(activity.Command, "third", "{}")

	resp, err := ts.Client().Get(ts.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		Events []struct {
			Summary string `json:"summary"`
		} `json:"events"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 3 {
		t.Fatalf("got %d events, want 3", len(got.Events))
	}
	for i, want := range []string{"third", "second", "first"} {
		if got.Events[i].Summary != want {
			t.Errorf("event %d = %q, want %q", i, got.Events[i].Summary, want)
		}
	}
}

func TestEventsWhenNothingHasHappened(t *testing.T) {
	ts, _ := serverWithNotifier(t)
	resp, err := ts.Client().Get(ts.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	var got struct {
		Events []any `json:"events"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 0 {
		t.Errorf("got %d events, want none", len(got.Events))
	}
}
