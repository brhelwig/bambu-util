package p1s

import "testing"

func TestHandleReportMergesPrintFields(t *testing.T) {
	c := NewStateCache()
	HandleReport(c, []byte(`{"print":{"gcode_state":"RUNNING","bed_temper":55.5}}`))
	fields, _ := c.Snapshot()
	if fields["gcode_state"] != "RUNNING" || fields["bed_temper"] != 55.5 {
		t.Fatalf("merge failed: %v", fields)
	}
}

func TestHandleReportIgnoresGarbage(t *testing.T) {
	c := NewStateCache()
	HandleReport(c, []byte(`not json`))
	HandleReport(c, []byte(`{"system":{"x":1}}`))
	HandleReport(c, []byte(`{"print":"not an object"}`))
	if fields, _ := c.Snapshot(); len(fields) != 0 {
		t.Fatalf("cache should be empty, got %v", fields)
	}
}

func TestPrintCommandPayload(t *testing.T) {
	got := printCommandPayload(7, "pause")
	want := `{"print":{"command":"pause","sequence_id":"7"}}`
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
