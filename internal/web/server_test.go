package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/brhelwig/bambu-util/internal/history"
	"github.com/brhelwig/bambu-util/internal/p1s"
)

type fakeCommander struct {
	calls       []string
	bedTemps    []int
	nozzleTemps []int
	loads       [][3]int // {slot, currTemp, tarTemp}
	filaments   []filamentCall
}

type filamentCall struct {
	amsID, trayID           int
	trayInfoIdx, color, typ string
	tempMin, tempMax        int
}

func (f *fakeCommander) LowerBed() { f.calls = append(f.calls, "lower-bed") }
func (f *fakeCommander) Home()     { f.calls = append(f.calls, "home") }
func (f *fakeCommander) SetBedTemp(t int) {
	f.calls = append(f.calls, "bed-temp")
	f.bedTemps = append(f.bedTemps, t)
}
func (f *fakeCommander) SetNozzleTemp(t int) {
	f.calls = append(f.calls, "nozzle-temp")
	f.nozzleTemps = append(f.nozzleTemps, t)
}
func (f *fakeCommander) Extrude()        { f.calls = append(f.calls, "extrude") }
func (f *fakeCommander) UnloadFilament() { f.calls = append(f.calls, "unload") }
func (f *fakeCommander) LoadFilament(slot, currTemp, tarTemp int) {
	f.calls = append(f.calls, "load")
	f.loads = append(f.loads, [3]int{slot, currTemp, tarTemp})
}
func (f *fakeCommander) SetAmsFilament(amsID, trayID int, trayInfoIdx, color, typ string, tempMin, tempMax int) {
	f.calls = append(f.calls, "set-filament")
	f.filaments = append(f.filaments, filamentCall{amsID, trayID, trayInfoIdx, color, typ, tempMin, tempMax})
}
func (f *fakeCommander) SetChamberLight(on bool) {
	if on {
		f.calls = append(f.calls, "lamp-on")
	} else {
		f.calls = append(f.calls, "lamp-off")
	}
}
func (f *fakeCommander) PausePrint()  { f.calls = append(f.calls, "pause") }
func (f *fakeCommander) ResumePrint() { f.calls = append(f.calls, "resume") }
func (f *fakeCommander) StopPrint()   { f.calls = append(f.calls, "stop") }

func openTestStore() *history.Store {
	store, err := history.Open(":memory:")
	if err != nil {
		panic(err)
	}
	return store
}

func buildTestServer(connected bool, state string) (*httptest.Server, *fakeCommander, *history.Store) {
	cache := p1s.NewStateCache()
	cache.SetConnected(connected)
	if state != "" {
		cache.Merge(map[string]any{"gcode_state": state, "bed_temper": 20.5})
	}
	cmd := &fakeCommander{}
	store := openTestStore()
	// Yields continuously, like a real camera, rather than once — a viewer
	// can Attach() at any point after Start (its connection lifecycle is no
	// longer tied to Attach) and still needs to see a frame promptly.
	hub := NewHub(func(ctx context.Context, yield func([]byte)) error {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				yield([]byte{0xFF, 0xD8, 0xFF, 0xD9})
			}
		}
	}, store)
	// Start is normally called once from main; tests need it running so
	// Attach()ed viewers (e.g. /camera/stream) actually receive a frame.
	// This goroutine exits when ctx is cancelled by the test binary's exit;
	// tests don't attach frequently enough for the leak to matter here.
	go hub.Start(context.Background())
	return httptest.NewServer(NewServer(cache, cmd, hub, store).Handler()), cmd, store
}

func newTestServer(connected bool, state string) (*httptest.Server, *fakeCommander) {
	ts, cmd, _ := buildTestServer(connected, state)
	return ts, cmd
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

func TestSetNozzleTempValid(t *testing.T) {
	ts, cmd := newTestServer(true, "IDLE")
	defer ts.Close()
	resp, _ := ts.Client().Post(ts.URL+"/api/actions/set-nozzle-temp?temp=200", "", nil)
	if resp.StatusCode != 200 || len(cmd.nozzleTemps) != 1 || cmd.nozzleTemps[0] != 200 {
		t.Fatalf("status %d nozzleTemps %v", resp.StatusCode, cmd.nozzleTemps)
	}
}

func TestSetNozzleTempInvalid(t *testing.T) {
	for _, temp := range []string{"abc", "999", "-5", ""} {
		ts, cmd := newTestServer(true, "IDLE")
		resp, _ := ts.Client().Post(ts.URL+"/api/actions/set-nozzle-temp?temp="+temp, "", nil)
		if resp.StatusCode != 400 || len(cmd.calls) != 0 {
			t.Fatalf("temp %q: status %d calls %v", temp, resp.StatusCode, cmd.calls)
		}
		ts.Close()
	}
}

func TestSetNozzleTempBlockedWhileRunning(t *testing.T) {
	ts, cmd := newTestServer(true, "RUNNING")
	defer ts.Close()
	resp, _ := ts.Client().Post(ts.URL+"/api/actions/set-nozzle-temp?temp=200", "", nil)
	if resp.StatusCode != 409 || len(cmd.calls) != 0 {
		t.Fatalf("status %d calls %v, want 409 and no call", resp.StatusCode, cmd.calls)
	}
}

func TestSetBedTempValid(t *testing.T) {
	ts, cmd := newTestServer(true, "IDLE")
	defer ts.Close()
	resp, _ := ts.Client().Post(ts.URL+"/api/actions/set-bed-temp?temp=55", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if len(cmd.bedTemps) != 1 || cmd.bedTemps[0] != 55 {
		t.Fatalf("bedTemps = %v, want [55]", cmd.bedTemps)
	}
}

func TestSetBedTempOff(t *testing.T) {
	ts, cmd := newTestServer(true, "IDLE")
	defer ts.Close()
	resp, _ := ts.Client().Post(ts.URL+"/api/actions/set-bed-temp?temp=0", "", nil)
	if resp.StatusCode != 200 || len(cmd.bedTemps) != 1 || cmd.bedTemps[0] != 0 {
		t.Fatalf("status %d bedTemps %v", resp.StatusCode, cmd.bedTemps)
	}
}

func TestSetBedTempInvalid(t *testing.T) {
	for _, temp := range []string{"abc", "999", "-5", ""} {
		ts, cmd := newTestServer(true, "IDLE")
		resp, _ := ts.Client().Post(ts.URL+"/api/actions/set-bed-temp?temp="+temp, "", nil)
		if resp.StatusCode != 400 || len(cmd.calls) != 0 {
			t.Fatalf("temp %q: status %d calls %v, want 400 and no call", temp, resp.StatusCode, cmd.calls)
		}
		ts.Close()
	}
}

func TestSetBedTempBlockedWhileRunning(t *testing.T) {
	ts, cmd := newTestServer(true, "RUNNING")
	defer ts.Close()
	resp, _ := ts.Client().Post(ts.URL+"/api/actions/set-bed-temp?temp=55", "", nil)
	if resp.StatusCode != 409 || len(cmd.calls) != 0 {
		t.Fatalf("status %d calls %v, want 409 and no call", resp.StatusCode, cmd.calls)
	}
}

func newTestServerWithFields(fields map[string]any) (*httptest.Server, *fakeCommander) {
	cache := p1s.NewStateCache()
	cache.SetConnected(true)
	cache.Merge(fields)
	cmd := &fakeCommander{}
	store := openTestStore()
	hub := NewHub(func(ctx context.Context, yield func([]byte)) error { <-ctx.Done(); return ctx.Err() }, store)
	return httptest.NewServer(NewServer(cache, cmd, hub, store).Handler()), cmd
}

func TestExtrudeAllowedWhenHotAndIdle(t *testing.T) {
	ts, cmd := newTestServerWithFields(map[string]any{"gcode_state": "IDLE", "nozzle_temper": float64(220)})
	defer ts.Close()
	resp, _ := ts.Client().Post(ts.URL+"/api/actions/extrude", "", nil)
	if resp.StatusCode != 200 || len(cmd.calls) != 1 || cmd.calls[0] != "extrude" {
		t.Fatalf("status %d calls %v", resp.StatusCode, cmd.calls)
	}
}

func TestExtrudeBlockedWhenCold(t *testing.T) {
	ts, cmd := newTestServerWithFields(map[string]any{"gcode_state": "IDLE", "nozzle_temper": float64(30)})
	defer ts.Close()
	resp, _ := ts.Client().Post(ts.URL+"/api/actions/extrude", "", nil)
	if resp.StatusCode != 409 || len(cmd.calls) != 0 {
		t.Fatalf("status %d calls %v, want 409 and no call", resp.StatusCode, cmd.calls)
	}
}

func TestExtrudeBlockedWhenRunning(t *testing.T) {
	ts, cmd := newTestServerWithFields(map[string]any{"gcode_state": "RUNNING", "nozzle_temper": float64(220)})
	defer ts.Close()
	resp, _ := ts.Client().Post(ts.URL+"/api/actions/extrude", "", nil)
	if resp.StatusCode != 409 || len(cmd.calls) != 0 {
		t.Fatalf("status %d calls %v, want 409 and no call", resp.StatusCode, cmd.calls)
	}
}

func TestUnloadAllowedWhenIdle(t *testing.T) {
	ts, cmd := newTestServer(true, "IDLE")
	defer ts.Close()
	resp, _ := ts.Client().Post(ts.URL+"/api/actions/unload", "", nil)
	if resp.StatusCode != 200 || len(cmd.calls) != 1 || cmd.calls[0] != "unload" {
		t.Fatalf("status %d calls %v", resp.StatusCode, cmd.calls)
	}
}

func TestUnloadBlockedWhileRunning(t *testing.T) {
	ts, cmd := newTestServer(true, "RUNNING")
	defer ts.Close()
	resp, _ := ts.Client().Post(ts.URL+"/api/actions/unload", "", nil)
	if resp.StatusCode != 409 || len(cmd.calls) != 0 {
		t.Fatalf("status %d calls %v, want 409 and no call", resp.StatusCode, cmd.calls)
	}
}

func TestLampTogglesInAnyState(t *testing.T) {
	// The lamp is not idle-guarded: it works even mid-print.
	ts, cmd := newTestServer(true, "RUNNING")
	defer ts.Close()
	if resp, _ := ts.Client().Post(ts.URL+"/api/actions/lamp-on", "", nil); resp.StatusCode != 200 {
		t.Fatalf("lamp-on status %d, want 200", resp.StatusCode)
	}
	if resp, _ := ts.Client().Post(ts.URL+"/api/actions/lamp-off", "", nil); resp.StatusCode != 200 {
		t.Fatalf("lamp-off status %d, want 200", resp.StatusCode)
	}
	if len(cmd.calls) != 2 || cmd.calls[0] != "lamp-on" || cmd.calls[1] != "lamp-off" {
		t.Fatalf("calls %v", cmd.calls)
	}
}

func TestLampBlockedWhenDisconnected(t *testing.T) {
	ts, cmd := newTestServer(false, "IDLE")
	defer ts.Close()
	resp, _ := ts.Client().Post(ts.URL+"/api/actions/lamp-on", "", nil)
	if resp.StatusCode != 409 || len(cmd.calls) != 0 {
		t.Fatalf("status %d calls %v, want 409 and no call", resp.StatusCode, cmd.calls)
	}
}

func TestLoadUsesNozzleTargetTemp(t *testing.T) {
	ts, cmd := newTestServerWithFields(map[string]any{
		"gcode_state":          "IDLE",
		"nozzle_temper":        float64(212),
		"nozzle_target_temper": float64(225),
	})
	defer ts.Close()
	resp, _ := ts.Client().Post(ts.URL+"/api/actions/load?slot=2", "", nil)
	if resp.StatusCode != 200 || len(cmd.loads) != 1 || cmd.loads[0] != [3]int{2, 212, 225} {
		t.Fatalf("status %d loads %v", resp.StatusCode, cmd.loads)
	}
}

func TestLoadBlockedWithoutNozzleTemp(t *testing.T) {
	ts, cmd := newTestServerWithFields(map[string]any{"gcode_state": "IDLE", "nozzle_target_temper": float64(0)})
	defer ts.Close()
	resp, _ := ts.Client().Post(ts.URL+"/api/actions/load?slot=0", "", nil)
	if resp.StatusCode != 409 || len(cmd.calls) != 0 {
		t.Fatalf("status %d calls %v, want 409 and no call", resp.StatusCode, cmd.calls)
	}
}

func TestLoadInvalidSlot(t *testing.T) {
	for _, slot := range []string{"16", "-1", "x", ""} {
		ts, cmd := newTestServerWithFields(map[string]any{"gcode_state": "IDLE", "nozzle_target_temper": float64(225)})
		resp, _ := ts.Client().Post(ts.URL+"/api/actions/load?slot="+slot, "", nil)
		if resp.StatusCode != 400 || len(cmd.calls) != 0 {
			t.Fatalf("slot %q: status %d calls %v", slot, resp.StatusCode, cmd.calls)
		}
		ts.Close()
	}
}

func TestLoadBlockedWhileRunning(t *testing.T) {
	ts, cmd := newTestServerWithFields(map[string]any{"gcode_state": "RUNNING", "nozzle_target_temper": float64(225)})
	defer ts.Close()
	resp, _ := ts.Client().Post(ts.URL+"/api/actions/load?slot=0", "", nil)
	if resp.StatusCode != 409 || len(cmd.calls) != 0 {
		t.Fatalf("status %d calls %v, want 409 and no call", resp.StatusCode, cmd.calls)
	}
}

func setFilamentURL(base string, q url.Values) string {
	return base + "/api/actions/set-filament?" + q.Encode()
}

func validFilamentQuery() url.Values {
	q := url.Values{}
	q.Set("ams_id", "0")
	q.Set("tray_id", "2")
	q.Set("tray_color", "FF6B35FF")
	q.Set("tray_type", "PETG")
	q.Set("nozzle_temp_min", "220")
	q.Set("nozzle_temp_max", "260")
	q.Set("tray_info_idx", "Pe352488")
	return q
}

func TestSetFilamentValid(t *testing.T) {
	ts, cmd := newTestServerWithFields(map[string]any{"gcode_state": "IDLE"})
	defer ts.Close()
	resp, _ := ts.Client().Post(setFilamentURL(ts.URL, validFilamentQuery()), "", nil)
	want := filamentCall{0, 2, "Pe352488", "FF6B35FF", "PETG", 220, 260}
	if resp.StatusCode != 200 || len(cmd.filaments) != 1 || cmd.filaments[0] != want {
		t.Fatalf("status %d filaments %v", resp.StatusCode, cmd.filaments)
	}
}

func TestSetFilamentNormalizesShortColor(t *testing.T) {
	ts, cmd := newTestServerWithFields(map[string]any{"gcode_state": "IDLE"})
	defer ts.Close()
	q := validFilamentQuery()
	q.Set("tray_color", "ff6b35") // 6-hex, lowercase → RRGGBBAA uppercase
	resp, _ := ts.Client().Post(setFilamentURL(ts.URL, q), "", nil)
	if resp.StatusCode != 200 || len(cmd.filaments) != 1 || cmd.filaments[0].color != "FF6B35FF" {
		t.Fatalf("status %d filaments %v", resp.StatusCode, cmd.filaments)
	}
}

func TestSetFilamentInvalid(t *testing.T) {
	bad := map[string]string{
		"ams_id":          "4",   // > max unit index
		"tray_id":         "4",   // > 3
		"tray_color":      "xyz", // not hex
		"tray_type":       "",    // empty
		"nozzle_temp_min": "999", // out of range
	}
	for field, val := range bad {
		ts, cmd := newTestServerWithFields(map[string]any{"gcode_state": "IDLE"})
		q := validFilamentQuery()
		q.Set(field, val)
		resp, _ := ts.Client().Post(setFilamentURL(ts.URL, q), "", nil)
		if resp.StatusCode != 400 || len(cmd.calls) != 0 {
			t.Fatalf("bad %s=%q: status %d calls %v", field, val, resp.StatusCode, cmd.calls)
		}
		ts.Close()
	}
}

func TestSetFilamentRejectsMinAboveMax(t *testing.T) {
	ts, cmd := newTestServerWithFields(map[string]any{"gcode_state": "IDLE"})
	defer ts.Close()
	q := validFilamentQuery()
	q.Set("nozzle_temp_min", "260")
	q.Set("nozzle_temp_max", "220")
	resp, _ := ts.Client().Post(setFilamentURL(ts.URL, q), "", nil)
	if resp.StatusCode != 400 || len(cmd.calls) != 0 {
		t.Fatalf("status %d calls %v, want 400 and no call", resp.StatusCode, cmd.calls)
	}
}

func TestSetFilamentBlockedWhileRunning(t *testing.T) {
	ts, cmd := newTestServerWithFields(map[string]any{"gcode_state": "RUNNING"})
	defer ts.Close()
	resp, _ := ts.Client().Post(setFilamentURL(ts.URL, validFilamentQuery()), "", nil)
	if resp.StatusCode != 409 || len(cmd.calls) != 0 {
		t.Fatalf("status %d calls %v, want 409 and no call", resp.StatusCode, cmd.calls)
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
	store := openTestStore()
	hub := NewHub(func(ctx context.Context, yield func([]byte)) error { <-ctx.Done(); return ctx.Err() }, store)
	ts := httptest.NewServer(NewServer(cache, cmd, hub, store).Handler())
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
	store := openTestStore()
	hub := NewHub(func(ctx context.Context, yield func([]byte)) error { <-ctx.Done(); return ctx.Err() }, store)
	ts := httptest.NewServer(NewServer(cache, cmd, hub, store).Handler())
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

func TestHistoryRangeEmpty(t *testing.T) {
	ts, _, _ := buildTestServer(true, "IDLE")
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/camera/history/range")
	if err != nil {
		t.Fatal(err)
	}
	var r map[string]any
	json.NewDecoder(resp.Body).Decode(&r)
	if r["oldest"] != nil || r["newest"] != nil {
		t.Fatalf("want null range on an empty store, got %v", r)
	}
}

func TestHistoryRangeAndFrame(t *testing.T) {
	ts, _, store := buildTestServer(true, "IDLE")
	defer ts.Close()
	store.InsertFrame(100, []byte{0xFF, 0xD8, 0xFF, 0xD9})
	store.InsertFrame(200, []byte{0xFF, 0xD8, 0x01, 0xFF, 0xD9})

	resp, _ := ts.Client().Get(ts.URL + "/camera/history/range")
	var r map[string]any
	json.NewDecoder(resp.Body).Decode(&r)
	if r["oldest"] != float64(100) || r["newest"] != float64(200) {
		t.Fatalf("bad range: %v", r)
	}

	resp2, _ := ts.Client().Get(ts.URL + "/camera/history/frame?ts=150")
	body, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != 200 || !bytes.Equal(body, []byte{0xFF, 0xD8, 0x01, 0xFF, 0xD9}) {
		t.Fatalf("frame at ts=150: status %d body %x, want the ts=200 frame", resp2.StatusCode, body)
	}
	if ct := resp2.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("Content-Type = %q, want image/jpeg", ct)
	}
}

func TestHistoryFrameNotFound(t *testing.T) {
	ts, _, _ := buildTestServer(true, "IDLE")
	defer ts.Close()

	resp, _ := ts.Client().Get(ts.URL + "/camera/history/frame?ts=999")
	if resp.StatusCode != 404 {
		t.Fatalf("status %d, want 404", resp.StatusCode)
	}
}

func TestHistoryFrameInvalidTs(t *testing.T) {
	ts, _, _ := buildTestServer(true, "IDLE")
	defer ts.Close()

	resp, _ := ts.Client().Get(ts.URL + "/camera/history/frame?ts=notanumber")
	if resp.StatusCode != 400 {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
}

func TestHistoryJobs(t *testing.T) {
	ts, _, store := buildTestServer(true, "IDLE")
	defer ts.Close()
	id, _ := store.OpenJob("benchy.3mf", 100)
	store.CloseJob(id, 200)
	store.OpenJob("ongoing.3mf", 300)

	resp, _ := ts.Client().Get(ts.URL + "/camera/history/jobs")
	var jobs []map[string]any
	json.NewDecoder(resp.Body).Decode(&jobs)
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2", len(jobs))
	}
}
