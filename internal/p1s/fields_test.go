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
