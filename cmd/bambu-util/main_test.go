package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brhelwig/bambu-util/internal/auth"
	"github.com/brhelwig/bambu-util/internal/p1s"
	"github.com/brhelwig/bambu-util/internal/settings"
)

// start assembles the real program against a fresh database and serves it, the
// same way main does. Nothing is stubbed: the stores are real, the background
// loops run, and the printer link is simply left unconfigured — which is the
// state the app is in before a printer has been entered on the settings page.
func start(t *testing.T) *httptest.Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	a, err := newApp(ctx, filepath.Join(t.TempDir(), "data"), auth.Decision{Disabled: true})
	if err != nil {
		t.Fatalf("assemble the app: %v", err)
	}
	t.Cleanup(a.close)

	ts := httptest.NewServer(a.handler)
	t.Cleanup(ts.Close)
	return ts
}

func get(t *testing.T, ts *httptest.Server, path string) *http.Response {
	t.Helper()
	resp, err := ts.Client().Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// The whole assembly has to come up against an empty directory: every store
// creates its own tables, and the page is served before a printer exists.
func TestTheAppStartsWithNothingConfigured(t *testing.T) {
	ts := start(t)

	if resp := get(t, ts, "/healthz"); resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", resp.StatusCode)
	}

	resp := get(t, ts, "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/ = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content type = %q, want HTML", ct)
	}

	var status struct {
		Connected      bool `json:"connected"`
		ActionsAllowed bool `json:"actionsAllowed"`
	}
	if err := json.NewDecoder(get(t, ts, "/api/status").Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.Connected {
		t.Error("reported connected with no printer configured")
	}
	if status.ActionsAllowed {
		t.Error("allowed actions with no printer configured")
	}
}

// A database is created on first run and reopened on the next, which is what a
// restart does. Both have to work against the same directory.
func TestTheAppReopensItsDatabase(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")

	for _, run := range []string{"first", "second"} {
		ctx, cancel := context.WithCancel(context.Background())
		a, err := newApp(ctx, dir, auth.Decision{Disabled: true})
		if err != nil {
			t.Fatalf("%s run: %v", run, err)
		}
		ts := httptest.NewServer(a.handler)
		if resp, err := ts.Client().Get(ts.URL + "/healthz"); err != nil || resp.StatusCode != http.StatusOK {
			t.Errorf("%s run: /healthz failed (%v)", run, err)
		} else {
			resp.Body.Close()
		}
		ts.Close()
		cancel()
		a.close()
	}

	if _, err := os.Stat(filepath.Join(dir, "bambu-util.db")); err != nil {
		t.Errorf("no database left behind: %v", err)
	}
}

// The path a real report takes: the printer's own message shape, merged through
// the same handler the MQTT subscription calls, surfacing on the status the
// page reads. This is the wiring between the printer link and the web layer
// that only main puts together.
func TestAPrinterReportReachesTheStatusEndpoint(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a, err := newApp(ctx, filepath.Join(t.TempDir(), "data"), auth.Decision{Disabled: true})
	if err != nil {
		t.Fatalf("assemble the app: %v", err)
	}
	t.Cleanup(a.close)
	ts := httptest.NewServer(a.handler)
	t.Cleanup(ts.Close)

	a.cache.SetConnected(true)
	p1s.HandleReport(a.cache, []byte(`{"print":{
		"gcode_state":"RUNNING",
		"subtask_name":"benchy.gcode",
		"bed_temper":58.5,
		"bed_target_temper":60,
		"nozzle_temper":219.4
	}}`))

	var status struct {
		Connected      bool    `json:"connected"`
		GcodeState     string  `json:"gcodeState"`
		JobName        string  `json:"jobName"`
		BedTemp        float64 `json:"bedTemp"`
		BedTarget      float64 `json:"bedTarget"`
		ActionsAllowed bool    `json:"actionsAllowed"`
		PrintActions   struct {
			Pause  bool `json:"pause"`
			Resume bool `json:"resume"`
			Stop   bool `json:"stop"`
		} `json:"printActions"`
	}
	resp, err := ts.Client().Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}

	if !status.Connected || status.GcodeState != "RUNNING" {
		t.Errorf("connected=%v state=%q, want a running printer", status.Connected, status.GcodeState)
	}
	if status.JobName != "benchy.gcode" {
		t.Errorf("job name = %q, want the subtask name from the report", status.JobName)
	}
	if status.BedTemp != 58.5 || status.BedTarget != 60 {
		t.Errorf("bed = %v/%v, want 58.5/60 from the report", status.BedTemp, status.BedTarget)
	}
	// Mid-print the bed actions are refused and the print ones offered — the
	// guard and the reported state have to agree.
	if status.ActionsAllowed {
		t.Error("bed actions offered during a print")
	}
	if !status.PrintActions.Pause || !status.PrintActions.Stop || status.PrintActions.Resume {
		t.Errorf("print actions = %+v, want pause and stop but not resume", status.PrintActions)
	}
}

// An action refused by the guard must be refused by the assembled program, not
// only by the handler in isolation.
func TestAnActionIsRefusedWithNoPrinter(t *testing.T) {
	ts := start(t)
	resp, err := ts.Client().Post(ts.URL+"/api/actions/lower-bed", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status %d, want 409 with no printer connected", resp.StatusCode)
	}
}

// The settings page is where a printer is entered, so it has to answer with the
// defaults before one exists — and the access code is a credential that must
// never come back out.
func TestSettingsAreServedBeforeAPrinterExists(t *testing.T) {
	ts := start(t)
	resp := get(t, ts, "/api/settings")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}

	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		settings.KeyRetention, settings.KeyKeptJobs, settings.KeyBedOffAfter,
		settings.KeyNozzleOffAfter, settings.KeyLampOffAfter, settings.KeyDashboard,
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("settings response has no %q", key)
		}
	}
	for _, key := range []string{
		settings.KeyPrinterAccessCode, settings.KeyPrinterIP, settings.KeyPrinterSerial,
	} {
		if _, ok := got[key]; ok {
			t.Errorf("settings response carries %q, which belongs to the printer connection", key)
		}
	}
}

func TestJobNameString(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]any
		want   string
	}{
		{"the subtask name", map[string]any{"subtask_name": "benchy.gcode"}, "benchy.gcode"},
		{"falls back to the file", map[string]any{"gcode_file": "raw.gcode"}, "raw.gcode"},
		{"empty when nothing is reported", map[string]any{}, ""},
		{"empty when the name is not text", map[string]any{"gcode_file": 42}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := jobNameString(c.fields); got != c.want {
				t.Errorf("jobNameString() = %q, want %q", got, c.want)
			}
		})
	}
}
