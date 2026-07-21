package p1s

import "fmt"

// States in which it is safe to move the bed or change temperatures — same
// set as the original Python TUI. Anything else (RUNNING, PREPARE, PAUSE,
// unknown) blocks all actions.
var idleStates = map[string]bool{"IDLE": true, "FINISH": true, "FAILED": true}

func GcodeState(fields map[string]any) string {
	if v, ok := fields["gcode_state"].(string); ok {
		return v
	}
	return "unknown"
}

func ActionAllowed(connected bool, gcodeState string) error {
	if !connected {
		return fmt.Errorf("not connected to printer")
	}
	if !idleStates[gcodeState] {
		return fmt.Errorf("printer state is %s", gcodeState)
	}
	return nil
}
