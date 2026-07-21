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

// PrintActionAllowed guards print-flow controls, which are only meaningful
// mid-print — the inverse of the bed-action guard.
func PrintActionAllowed(connected bool, gcodeState, action string) error {
	if !connected {
		return fmt.Errorf("not connected to printer")
	}
	switch action {
	case "pause":
		if gcodeState != "RUNNING" {
			return fmt.Errorf("can only pause while RUNNING, printer state is %s", gcodeState)
		}
	case "resume":
		if gcodeState != "PAUSE" {
			return fmt.Errorf("can only resume while PAUSE, printer state is %s", gcodeState)
		}
	case "stop":
		if gcodeState != "RUNNING" && gcodeState != "PAUSE" {
			return fmt.Errorf("can only stop while RUNNING or PAUSE, printer state is %s", gcodeState)
		}
	default:
		return fmt.Errorf("unknown print action %s", action)
	}
	return nil
}
