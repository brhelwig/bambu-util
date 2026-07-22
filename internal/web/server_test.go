package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/brhelwig/bambu-util/internal/p1s"
)

type fakeCommander struct{ calls []string }

func (f *fakeCommander) LowerBed()           { f.calls = append(f.calls, "lower-bed") }
func (f *fakeCommander) Home()               { f.calls = append(f.calls, "home") }
func (f *fakeCommander) SetBedTemp(t int)    { f.calls = append(f.calls, "bed-temp") }
func (f *fakeCommander) SetNozzleTemp(t int) { f.calls = append(f.calls, "nozzle-temp") }
func (f *fakeCommander) PausePrint()         { f.calls = append(f.calls, "pause") }
func (f *fakeCommander) ResumePrint()        { f.calls = append(f.calls, "resume") }
func (f *fakeCommander) StopPrint()          { f.calls = append(f.calls, "stop") }

func newTestServer(connected bool, state string) (*httptest.Server, *fakeCommander) {
	cache := p1s.NewStateCache()
	cache.SetConnected(connected)
	if state != "" {
		cache.Merge(map[string]any{"gcode_state": state, "bed_temper": 20.5})
	}
	cmd := &fakeCommander{}
	hub := NewHub(func(ctx context.Context, yield func([]byte)) error {
		yield([]byte{0xFF, 0xD8, 0xFF, 0xD9})
		<-ctx.Done()
		return ctx.Err()
	})
	return httptest.NewServer(NewServer(cache, cmd, hub).Handler()), cmd
}

func TestStatusEndpoint(t *testing.T) {
	ts, _ := newTestServer(true, "IDLE")
	defer ts.Close()
	resp, err := ts.Client().Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	json.NewDecoder(resp.Body).Decode(&s)
	if s["connected"] != true || s["gcodeState"] != "IDLE" || s["actionsAllowed"] != true || s["bedTemp"] != 20.5 {
		t.Fatalf("bad status: %v", s)
	}
}

func TestActionBlockedWhenNotIdle(t *testing.T) {
	ts, cmd := newTestServer(true, "RUNNING")
	defer ts.Close()
	resp, _ := ts.Client().Post(ts.URL+"/api/actions/lower-bed", "", nil)
	if resp.StatusCode != 409 {
		t.Fatalf("status %d, want 409", resp.StatusCode)
	}
	if len(cmd.calls) != 0 {
		t.Fatal("command sent despite guard")
	}
}

func TestActionBlockedWhenDisconnected(t *testing.T) {
	ts, cmd := newTestServer(false, "IDLE")
	defer ts.Close()
	resp, _ := ts.Client().Post(ts.URL+"/api/actions/home", "", nil)
	if resp.StatusCode != 409 {
		t.Fatalf("status %d, want 409", resp.StatusCode)
	}
	if len(cmd.calls) != 0 {
		t.Fatal("command sent despite guard")
	}
}

func TestActionAllowedWhenIdle(t *testing.T) {
	ts, cmd := newTestServer(true, "IDLE")
	defer ts.Close()
	resp, _ := ts.Client().Post(ts.URL+"/api/actions/lower-bed", "", nil)
	if resp.StatusCode != 200 || len(cmd.calls) != 1 || cmd.calls[0] != "lower-bed" {
		t.Fatalf("status %d calls %v", resp.StatusCode, cmd.calls)
	}
}

func TestUnknownAction(t *testing.T) {
	ts, _ := newTestServer(true, "IDLE")
	defer ts.Close()
	resp, _ := ts.Client().Post(ts.URL+"/api/actions/explode", "", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("status %d, want 404", resp.StatusCode)
	}
}

func TestHealthz(t *testing.T) {
	ts, _ := newTestServer(false, "")
	defer ts.Close()
	resp, _ := ts.Client().Get(ts.URL + "/healthz")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
}

func TestCameraStreamContentType(t *testing.T) {
	ts, _ := newTestServer(true, "IDLE")
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/camera/stream", nil)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "multipart/x-mixed-replace") {
		t.Fatalf("content-type: %s", resp.Header.Get("Content-Type"))
	}
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "image/jpeg") {
		t.Fatalf("first part header missing: %q", buf[:n])
	}
}

func TestPrintActionPauseWhileRunning(t *testing.T) {
	ts, cmd := newTestServer(true, "RUNNING")
	defer ts.Close()
	resp, _ := ts.Client().Post(ts.URL+"/api/actions/pause", "", nil)
	if resp.StatusCode != 200 || len(cmd.calls) != 1 || cmd.calls[0] != "pause" {
		t.Fatalf("status %d calls %v", resp.StatusCode, cmd.calls)
	}
}

func TestPrintActionPauseBlockedWhileIdle(t *testing.T) {
	ts, cmd := newTestServer(true, "IDLE")
	defer ts.Close()
	resp, _ := ts.Client().Post(ts.URL+"/api/actions/pause", "", nil)
	if resp.StatusCode != 409 || len(cmd.calls) != 0 {
		t.Fatalf("status %d calls %v", resp.StatusCode, cmd.calls)
	}
}

func TestPrintActionResumeOnlyWhilePaused(t *testing.T) {
	ts, cmd := newTestServer(true, "PAUSE")
	defer ts.Close()
	resp, _ := ts.Client().Post(ts.URL+"/api/actions/resume", "", nil)
	if resp.StatusCode != 200 || len(cmd.calls) != 1 || cmd.calls[0] != "resume" {
		t.Fatalf("status %d calls %v", resp.StatusCode, cmd.calls)
	}
	ts2, cmd2 := newTestServer(true, "RUNNING")
	defer ts2.Close()
	resp2, _ := ts2.Client().Post(ts2.URL+"/api/actions/resume", "", nil)
	if resp2.StatusCode != 409 || len(cmd2.calls) != 0 {
		t.Fatalf("status %d calls %v", resp2.StatusCode, cmd2.calls)
	}
}

func TestPrintActionStopWhileRunningOrPaused(t *testing.T) {
	for _, state := range []string{"RUNNING", "PAUSE"} {
		ts, cmd := newTestServer(true, state)
		resp, _ := ts.Client().Post(ts.URL+"/api/actions/stop", "", nil)
		if resp.StatusCode != 200 || len(cmd.calls) != 1 || cmd.calls[0] != "stop" {
			t.Fatalf("state %s: status %d calls %v", state, resp.StatusCode, cmd.calls)
		}
		ts.Close()
	}
}

func TestStatusIncludesPrintActions(t *testing.T) {
	ts, _ := newTestServer(true, "RUNNING")
	defer ts.Close()
	resp, _ := ts.Client().Get(ts.URL + "/api/status")
	var s struct {
		PrintActions map[string]bool `json:"printActions"`
	}
	json.NewDecoder(resp.Body).Decode(&s)
	want := map[string]bool{"pause": true, "resume": false, "stop": true}
	for k, v := range want {
		if s.PrintActions[k] != v {
			t.Fatalf("printActions[%s] = %v, want %v (all: %v)", k, s.PrintActions[k], v, s.PrintActions)
		}
	}
}

func TestNozzleHeatActionAllowedWhenIdle(t *testing.T) {
	ts, cmd := newTestServer(true, "IDLE")
	defer ts.Close()
	resp, _ := ts.Client().Post(ts.URL+"/api/actions/nozzle-heat", "", nil)
	if resp.StatusCode != 200 || len(cmd.calls) != 1 || cmd.calls[0] != "nozzle-temp" {
		t.Fatalf("status %d calls %v", resp.StatusCode, cmd.calls)
	}
}

func TestNozzleHeatOffAllowedWhenIdle(t *testing.T) {
	ts, cmd := newTestServer(true, "IDLE")
	defer ts.Close()
	resp, _ := ts.Client().Post(ts.URL+"/api/actions/nozzle-heat-off", "", nil)
	if resp.StatusCode != 200 || len(cmd.calls) != 1 || cmd.calls[0] != "nozzle-temp" {
		t.Fatalf("status %d calls %v", resp.StatusCode, cmd.calls)
	}
}

func TestStatusIncludesJobFields(t *testing.T) {
	cache := p1s.NewStateCache()
	cache.SetConnected(true)
	cache.Merge(map[string]any{
		"gcode_state":       "RUNNING",
		"subtask_name":      "benchy.gcode",
		"layer_num":         float64(42),
		"total_layer_num":   float64(120),
		"mc_remaining_time": float64(37),
		"chamber_temper":    float64(28.4),
		"wifi_signal":       "-45dBm",
		"cooling_fan_speed": float64(15),
		"big_fan1_speed":    float64(0),
		"big_fan2_speed":    float64(8),
		"ams":               map[string]any{"ams": []any{}},
	})
	cmd := &fakeCommander{}
	hub := NewHub(func(ctx context.Context, yield func([]byte)) error { <-ctx.Done(); return ctx.Err() })
	ts := httptest.NewServer(NewServer(cache, cmd, hub).Handler())
	defer ts.Close()

	resp, _ := ts.Client().Get(ts.URL + "/api/status")
	var s map[string]any
	json.NewDecoder(resp.Body).Decode(&s)

	if s["jobName"] != "benchy.gcode" {
		t.Errorf("jobName = %v", s["jobName"])
	}
	if s["layerNum"] != float64(42) || s["totalLayerNum"] != float64(120) {
		t.Errorf("layerNum/totalLayerNum = %v/%v", s["layerNum"], s["totalLayerNum"])
	}
	if s["remainingMinutes"] != float64(37) {
		t.Errorf("remainingMinutes = %v", s["remainingMinutes"])
	}
	if s["chamberTemp"] != 28.4 {
		t.Errorf("chamberTemp = %v", s["chamberTemp"])
	}
	if s["wifiSignal"] != "-45dBm" {
		t.Errorf("wifiSignal = %v", s["wifiSignal"])
	}
	fans, ok := s["fans"].(map[string]any)
	if !ok || fans["cooling"] != float64(15) || fans["aux"] != float64(0) || fans["chamber"] != float64(8) {
		t.Errorf("fans = %v", s["fans"])
	}
	if _, ok := s["ams"]; !ok {
		t.Error("expected ams key present")
	}
	if _, ok := s["hms"]; !ok {
		t.Error("expected hms key present")
	}
}

func TestStatusHMSPopulated(t *testing.T) {
	cache := p1s.NewStateCache()
	cache.SetConnected(true)
	cache.Merge(map[string]any{
		"gcode_state": "IDLE",
		"hms": []any{
			map[string]any{"attr": float64(0x03008000), "code": float64(0x00030002)},
		},
	})
	cmd := &fakeCommander{}
	hub := NewHub(func(ctx context.Context, yield func([]byte)) error { <-ctx.Done(); return ctx.Err() })
	ts := httptest.NewServer(NewServer(cache, cmd, hub).Handler())
	defer ts.Close()

	resp, _ := ts.Client().Get(ts.URL + "/api/status")
	var s struct {
		HMS []p1s.HMSEntry `json:"hms"`
	}
	json.NewDecoder(resp.Body).Decode(&s)
	if len(s.HMS) != 1 || s.HMS[0].Code != "0300-8000-0003-0002" {
		t.Fatalf("hms = %+v", s.HMS)
	}
}
