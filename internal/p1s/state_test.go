package p1s

import (
	"encoding/json"
	"strconv"
	"testing"
)

func TestMergeAccumulatesAndOverwrites(t *testing.T) {
	c := NewStateCache()
	c.Merge(map[string]any{"gcode_state": "IDLE", "bed_temper": 20.0})
	c.Merge(map[string]any{"bed_temper": 55.5})
	fields, _ := c.Snapshot()
	if fields["gcode_state"] != "IDLE" || fields["bed_temper"] != 55.5 {
		t.Fatalf("bad merge: %v", fields)
	}
}

func TestMergePreservesNestedSiblings(t *testing.T) {
	c := NewStateCache()
	// Full report: ams carries the tray array plus tray_now (loaded tray).
	c.Merge(map[string]any{"ams": map[string]any{
		"tray_now": "0",
		"ams":      []any{map[string]any{"id": "0"}},
	}})
	// Partial delta: ams array again, but no tray_now this time.
	c.Merge(map[string]any{"ams": map[string]any{
		"ams": []any{map[string]any{"id": "0"}},
	}})
	fields, _ := c.Snapshot()
	ams, ok := fields["ams"].(map[string]any)
	if !ok || ams["tray_now"] != "0" {
		t.Fatalf("tray_now clobbered by partial ams report: %v", fields["ams"])
	}
}

func TestMergeDeepOverwritesLeaf(t *testing.T) {
	c := NewStateCache()
	c.Merge(map[string]any{"ams": map[string]any{"tray_now": "0"}})
	c.Merge(map[string]any{"ams": map[string]any{"tray_now": "3"}})
	fields, _ := c.Snapshot()
	ams := fields["ams"].(map[string]any)
	if ams["tray_now"] != "3" {
		t.Fatalf("nested leaf not overwritten: %v", ams)
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

// A snapshot must not share nested maps with the cache: Merge writes into them
// in place, and a handler encoding a snapshot reads them without the lock.
func TestSnapshotIsIndependentOfLaterMerges(t *testing.T) {
	c := NewStateCache()
	c.Merge(map[string]any{"ams": map[string]any{"tray_now": "1"}})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			c.Merge(map[string]any{"ams": map[string]any{"tray_now": strconv.Itoa(i)}})
		}
	}()
	for i := 0; i < 1000; i++ {
		fields, _ := c.Snapshot()
		if _, err := json.Marshal(fields); err != nil {
			t.Fatal(err)
		}
	}
	<-done

	snap, _ := c.Snapshot()
	c.Merge(map[string]any{"ams": map[string]any{"tray_now": "changed"}})
	if got := snap["ams"].(map[string]any)["tray_now"]; got == "changed" {
		t.Fatal("a later Merge changed an earlier snapshot")
	}
}
