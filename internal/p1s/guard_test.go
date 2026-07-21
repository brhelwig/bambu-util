package p1s

import "testing"

func TestActionAllowed(t *testing.T) {
	cases := []struct {
		connected bool
		state     string
		allowed   bool
	}{
		{false, "IDLE", false},
		{true, "RUNNING", false},
		{true, "PREPARE", false},
		{true, "PAUSE", false},
		{true, "unknown", false},
		{true, "IDLE", true},
		{true, "FINISH", true},
		{true, "FAILED", true},
	}
	for _, c := range cases {
		err := ActionAllowed(c.connected, c.state)
		if (err == nil) != c.allowed {
			t.Errorf("connected=%v state=%q: got err=%v", c.connected, c.state, err)
		}
	}
}

func TestGcodeState(t *testing.T) {
	if GcodeState(map[string]any{"gcode_state": "IDLE"}) != "IDLE" {
		t.Fatal("did not read gcode_state")
	}
	if GcodeState(map[string]any{}) != "unknown" {
		t.Fatal("missing gcode_state should read unknown")
	}
	if GcodeState(map[string]any{"gcode_state": 7}) != "unknown" {
		t.Fatal("non-string gcode_state should read unknown")
	}
}

func TestPrintActionAllowed(t *testing.T) {
	cases := []struct {
		connected bool
		state     string
		action    string
		allowed   bool
	}{
		{false, "RUNNING", "pause", false},
		{true, "RUNNING", "pause", true},
		{true, "PAUSE", "pause", false},
		{true, "IDLE", "pause", false},
		{true, "PAUSE", "resume", true},
		{true, "RUNNING", "resume", false},
		{true, "IDLE", "resume", false},
		{true, "RUNNING", "stop", true},
		{true, "PAUSE", "stop", true},
		{true, "IDLE", "stop", false},
		{true, "FINISH", "stop", false},
		{true, "RUNNING", "explode", false},
	}
	for _, c := range cases {
		err := PrintActionAllowed(c.connected, c.state, c.action)
		if (err == nil) != c.allowed {
			t.Errorf("connected=%v state=%q action=%q: got err=%v", c.connected, c.state, c.action, err)
		}
	}
}
