package p1s

import "testing"

func TestJobName(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]any
		want   any
	}{
		{"prefers subtask_name", map[string]any{"subtask_name": "benchy.gcode", "gcode_file": "raw.gcode"}, "benchy.gcode"},
		{"falls back to gcode_file when subtask_name empty", map[string]any{"subtask_name": "", "gcode_file": "raw.gcode"}, "raw.gcode"},
		{"falls back to gcode_file when subtask_name missing", map[string]any{"gcode_file": "raw.gcode"}, "raw.gcode"},
		{"nil when neither present", map[string]any{}, nil},
	}
	for _, c := range cases {
		got := JobName(c.fields)
		if got != c.want {
			t.Errorf("%s: JobName() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestChamberLight(t *testing.T) {
	on := func(b bool) *bool { return &b }
	cases := []struct {
		name   string
		fields map[string]any
		want   *bool
	}{
		{"on", map[string]any{"lights_report": []any{
			map[string]any{"node": "chamber_light", "mode": "on"}}}, on(true)},
		{"off", map[string]any{"lights_report": []any{
			map[string]any{"node": "chamber_light", "mode": "off"}}}, on(false)},
		{"picks the chamber out of several lights", map[string]any{"lights_report": []any{
			map[string]any{"node": "work_light", "mode": "on"},
			map[string]any{"node": "chamber_light", "mode": "off"}}}, on(false)},
		{"unknown before the printer has reported", map[string]any{}, nil},
		{"unknown when no chamber entry", map[string]any{"lights_report": []any{
			map[string]any{"node": "work_light", "mode": "on"}}}, nil},
		{"unknown when the report is not a list", map[string]any{"lights_report": "on"}, nil},
		{"skips entries that are not objects", map[string]any{"lights_report": []any{
			"nonsense", map[string]any{"node": "chamber_light", "mode": "on"}}}, on(true)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ChamberLight(c.fields)
			switch {
			case c.want == nil && got != nil:
				t.Errorf("ChamberLight() = %v, want unknown", *got)
			case c.want != nil && got == nil:
				t.Errorf("ChamberLight() = unknown, want %v", *c.want)
			case c.want != nil && *got != *c.want:
				t.Errorf("ChamberLight() = %v, want %v", *got, *c.want)
			}
		})
	}
}
