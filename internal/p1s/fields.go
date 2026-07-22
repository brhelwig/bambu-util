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

// ChamberLightOn reports whether the chamber LED is on, read from the printer's
// "lights_report" array (entries like {"node":"chamber_light","mode":"on"}).
// Defensive: any unexpected shape reads as off.
func ChamberLightOn(fields map[string]any) bool {
	arr, ok := fields["lights_report"].([]any)
	if !ok {
		return false
	}
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if m["node"] == "chamber_light" {
			return m["mode"] == "on"
		}
	}
	return false
}
