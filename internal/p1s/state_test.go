package p1s

import "testing"

func TestMergeAccumulatesAndOverwrites(t *testing.T) {
	c := NewStateCache()
	c.Merge(map[string]any{"gcode_state": "IDLE", "bed_temper": 20.0})
	c.Merge(map[string]any{"bed_temper": 55.5})
	fields, _ := c.Snapshot()
	if fields["gcode_state"] != "IDLE" || fields["bed_temper"] != 55.5 {
		t.Fatalf("bad merge: %v", fields)
	}
}

func TestSnapshotIsACopy(t *testing.T) {
	c := NewStateCache()
	c.Merge(map[string]any{"a": 1})
	fields, _ := c.Snapshot()
	fields["a"] = 2
	again, _ := c.Snapshot()
	if again["a"] != 1 {
		t.Fatal("snapshot leaked internal map")
	}
}

func TestConnectedFlag(t *testing.T) {
	c := NewStateCache()
	if _, conn := c.Snapshot(); conn {
		t.Fatal("should start disconnected")
	}
	c.SetConnected(true)
	if _, conn := c.Snapshot(); !conn {
		t.Fatal("SetConnected(true) not reflected")
	}
}
