package p1s

// JobName returns the active job's display name. "subtask_name" is the
// human-entered name Bambu Studio/Handy assigns; "gcode_file" (the raw
// filename) is the fallback for jobs with no subtask name, e.g. local SD
// prints started without the slicer's project metadata.
func JobName(fields map[string]any) any {
	if v, ok := fields["subtask_name"].(string); ok && v != "" {
		return v
	}
	return fields["gcode_file"]
}
