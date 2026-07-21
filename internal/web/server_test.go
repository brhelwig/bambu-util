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

func (f *fakeCommander) LowerBed()        { f.calls = append(f.calls, "lower-bed") }
func (f *fakeCommander) Home()            { f.calls = append(f.calls, "home") }
func (f *fakeCommander) SetBedTemp(t int) { f.calls = append(f.calls, "bed-temp") }

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
