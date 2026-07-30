package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/brhelwig/bambu-util/internal/activity"
	"github.com/brhelwig/bambu-util/internal/p1s"
	"github.com/brhelwig/bambu-util/internal/settings"
)

// settingsTestServer wires a real settings store behind the HTTP layer, so a
// write through the page is read back the way the running app would read it.
func settingsTestServer(t *testing.T) (*httptest.Server, *settings.Store) {
	t.Helper()
	config := openTestSettings(t)
	cache := p1s.NewStateCache()
	cache.SetConnected(true)
	cache.Merge(map[string]any{"gcode_state": "IDLE"})
	srv := NewServer(cache, &fakeCommander{}, openTestStore(), openTestNotifier(), nil, config.Values, config, testPrinter(), activity.New(50))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, config
}

func readSettings(t *testing.T, ts *httptest.Server) map[string]any {
	t.Helper()
	resp, err := ts.Client().Get(ts.URL + "/api/settings")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

func TestSettingsAreServedAsSeconds(t *testing.T) {
	ts, _ := settingsTestServer(t)
	got := readSettings(t, ts)
	if got := readSettings(t, ts)[settings.KeyKeptJobs]; got != float64(settings.Defaults.KeptJobs) {
		t.Errorf("kept jobs = %v, want %d", got, settings.Defaults.KeptJobs)
	}
	for name, want := range map[string]time.Duration{
		settings.KeyRetention:      settings.Defaults.Retention,
		settings.KeyBedOffAfter:    settings.Defaults.BedOffAfter,
		settings.KeyNozzleOffAfter: settings.Defaults.NozzleOffAfter,
		settings.KeyLampOffAfter:   settings.Defaults.LampOffAfter,
	} {
		if got[name] != float64(int(want.Seconds())) {
			t.Errorf("%s = %v, want %d", name, got[name], int(want.Seconds()))
		}
	}
}

func TestChangingASettingTakesEffect(t *testing.T) {
	ts, config := settingsTestServer(t)
	resp, err := ts.Client().Post(ts.URL+"/api/settings/"+settings.KeyBedOffAfter+"?value=3600", "", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d, want 204", resp.StatusCode)
	}
	if got := config.Values().BedOffAfter; got != time.Hour {
		t.Errorf("stored bed shut-off = %s, want 1h", got)
	}
	if got := readSettings(t, ts)[settings.KeyBedOffAfter]; got != float64(3600) {
		t.Errorf("served bed shut-off = %v, want 3600", got)
	}
}

// The point of these being settings is that a change works without a restart,
// so the countdown a heater is given must come from the current value.
func TestANewShutOffWindowAppliesToTheNextHeaterSet(t *testing.T) {
	ts, _ := settingsTestServer(t)
	if resp, _ := ts.Client().Post(ts.URL+"/api/settings/"+settings.KeyBedOffAfter+"?value=7200", "", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("changing the setting returned %d", resp.StatusCode)
	}
	if resp, _ := ts.Client().Post(ts.URL+"/api/actions/set-bed-temp?temp=60", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("setting the bed returned %d", resp.StatusCode)
	}

	resp, err := ts.Client().Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	defer resp.Body.Close()
	var status map[string]any
	json.NewDecoder(resp.Body).Decode(&status)
	bedOff, ok := status["bedOffIn"].(float64)
	if !ok {
		t.Fatalf("bedOffIn = %v, want a countdown", status["bedOffIn"])
	}
	if bedOff > 7200 || bedOff < 7200-60 {
		t.Errorf("countdown = %v, want about 7200 — the new window, not the old default", bedOff)
	}
}

func TestBadSettingWritesAreRefused(t *testing.T) {
	ts, config := settingsTestServer(t)
	cases := map[string]string{
		"unknown setting": "/api/settings/chamber-temperature?value=3600",
		"not a number":    "/api/settings/" + settings.KeyRetention + "?value=soon",
		"missing value":   "/api/settings/" + settings.KeyRetention,
		"far too long":    "/api/settings/" + settings.KeyRetention + "?value=999999999",
		"far too short":   "/api/settings/" + settings.KeyRetention + "?value=1",
		"negative":        "/api/settings/" + settings.KeyRetention + "?value=-3600",
		"count too high":  "/api/settings/" + settings.KeyKeptJobs + "?value=5000",
	}
	for name, path := range cases {
		resp, err := ts.Client().Post(ts.URL+path, "", nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, resp.StatusCode)
		}
	}
	if got := config.Values(); got != settings.Defaults {
		t.Errorf("a refused write changed the settings: %+v", got)
	}
}

// A server built without a writable store serves its settings but refuses to
// change them, rather than pretending the write worked.
func TestAServerWithNoWritableSettingsSaysSo(t *testing.T) {
	cache := p1s.NewStateCache()
	cache.SetConnected(true)
	srv := NewServer(cache, &fakeCommander{}, openTestStore(), openTestNotifier(), nil, testSettings, nil, testPrinter(), activity.New(50))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	if resp, _ := ts.Client().Post(ts.URL+"/api/settings/"+settings.KeyRetention+"?value=3600", "", nil); resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status %d, want 501", resp.StatusCode)
	}
	if got := readSettings(t, ts)[settings.KeyRetention]; got != float64(int(settings.Defaults.Retention.Seconds())) {
		t.Errorf("reading settings = %v, want the defaults to still be served", got)
	}
}

func printerTestServer(t *testing.T) (*httptest.Server, *settings.Store, *fakePrinter) {
	t.Helper()
	config := openTestSettings(t)
	printer := &fakePrinter{}
	cache := p1s.NewStateCache()
	srv := NewServer(cache, &fakeCommander{}, openTestStore(), openTestNotifier(), nil, config.Values, config, printer, activity.New(50))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, config, printer
}

func postPrinter(t *testing.T, ts *httptest.Server, body string) *http.Response {
	t.Helper()
	resp, err := ts.Client().Post(ts.URL+"/api/printer", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestSettingUpAPrinterConnectsToIt(t *testing.T) {
	ts, config, printer := printerTestServer(t)
	resp := postPrinter(t, ts, `{"ip":"192.0.2.10","serial":"01P00A","accessCode":"hunter2"}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d, want 204", resp.StatusCode)
	}
	if printer.calls != 1 {
		t.Errorf("the link was reconfigured %d times, want once", printer.calls)
	}
	want := p1s.Config{IP: "192.0.2.10", Serial: "01P00A", AccessCode: "hunter2"}
	if printer.conf != want {
		t.Errorf("pointed at %+v, want %+v", printer.conf, want)
	}
	// And it survives a restart, which is the whole point of storing it.
	v := config.Values()
	if v.PrinterIP != want.IP || v.PrinterSerial != want.Serial || v.AccessCode != want.AccessCode {
		t.Errorf("stored %q/%q/(code set: %v), want it all kept", v.PrinterIP, v.PrinterSerial, v.AccessCode != "")
	}
}

// The page has no authentication, so a credential it is never served cannot be
// read off it.
func TestTheAccessCodeIsNeverSentToTheBrowser(t *testing.T) {
	ts, _, _ := printerTestServer(t)
	postPrinter(t, ts, `{"ip":"192.0.2.10","serial":"01P00A","accessCode":"hunter2"}`)

	for _, path := range []string{"/api/printer", "/api/settings", "/api/status"} {
		resp, err := ts.Client().Get(ts.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(body), "hunter2") {
			t.Errorf("%s served the access code: %s", path, body)
		}
	}

	resp, err := ts.Client().Get(ts.URL + "/api/printer")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var got map[string]any
	json.NewDecoder(resp.Body).Decode(&got)
	if got["accessCodeSet"] != true {
		t.Errorf("accessCodeSet = %v, want the page told that one is set", got["accessCodeSet"])
	}
}

// The form never receives the code, so it cannot send it back. Leaving it out
// must not wipe it.
func TestSavingWithNoAccessCodeKeepsTheStoredOne(t *testing.T) {
	ts, config, printer := printerTestServer(t)
	postPrinter(t, ts, `{"ip":"192.0.2.10","serial":"01P00A","accessCode":"hunter2"}`)
	postPrinter(t, ts, `{"ip":"192.0.2.99","serial":"01P00A","accessCode":""}`)

	if got := config.Values().AccessCode; got != "hunter2" {
		t.Errorf("access code = %q, want the stored one kept", got)
	}
	if printer.conf.IP != "192.0.2.99" {
		t.Errorf("address = %q, want the new one", printer.conf.IP)
	}
}

func TestAnIncompletePrinterIsRefused(t *testing.T) {
	ts, _, printer := printerTestServer(t)
	for name, body := range map[string]string{
		"no address": `{"ip":"","serial":"01P00A","accessCode":"hunter2"}`,
		"no serial":  `{"ip":"192.0.2.10","serial":"","accessCode":"hunter2"}`,
		"no code":    `{"ip":"192.0.2.10","serial":"01P00A","accessCode":""}`,
		"nothing":    `{}`,
		"not json":   `{`,
	} {
		if resp := postPrinter(t, ts, body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, resp.StatusCode)
		}
	}
	if printer.calls != 0 {
		t.Errorf("the link was reconfigured %d times on refused input, want none", printer.calls)
	}
}

// With nothing set up the app still has to serve the page — that is where a
// printer is entered.
func TestAnUnconfiguredAppStillServesItsPage(t *testing.T) {
	ts, _, _ := printerTestServer(t)
	for _, path := range []string{"/", "/api/printer", "/api/status", "/healthz"} {
		resp, err := ts.Client().Get(ts.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d with no printer configured, want 200", path, resp.StatusCode)
		}
	}
}
