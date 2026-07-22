package p1s

import "fmt"

// hmsMessages maps a formatted HMS code (see FormatHMSCode) to a
// human-readable message. Sourced from community documentation
// (ha-bambulab/pybambu), not verified against a real printer payload.
// Starts small; add entries as real codes are observed in practice.
var hmsMessages = map[string]string{
	"0300-8000-0003-0002": "AMS filament runout",
}

// FormatHMSCode renders an attr/code pair as Bambu's dash-grouped hex
// display format, e.g. "0300-8000-0003-0002": attr and code are each
// packed as 8 hex digits, concatenated, then split into four 4-digit
// groups.
func FormatHMSCode(attr, code int64) string {
	hex := fmt.Sprintf("%08X%08X", attr, code)
	return fmt.Sprintf("%s-%s-%s-%s", hex[0:4], hex[4:8], hex[8:12], hex[12:16])
}

// HMSMessage looks up a human-readable message for a formatted HMS code.
// ok is false for any code not in the table.
func HMSMessage(code string) (string, bool) {
	msg, ok := hmsMessages[code]
	return msg, ok
}

// HMSEntry is a single translated HMS health-management-system error.
type HMSEntry struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// HMSErrors reads the raw "hms" field from a print report and translates
// each entry into a display code plus a message (falling back to the raw
// code when it isn't in the lookup table). Entries with an unexpected
// shape are skipped rather than causing an error.
func HMSErrors(fields map[string]any) []HMSEntry {
	raw, ok := fields["hms"].([]any)
	if !ok {
		return nil
	}
	var out []HMSEntry
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		attr, ok1 := m["attr"].(float64)
		code, ok2 := m["code"].(float64)
		if !ok1 || !ok2 {
			continue
		}
		display := FormatHMSCode(int64(attr), int64(code))
		msg, known := HMSMessage(display)
		if !known {
			msg = display
		}
		out = append(out, HMSEntry{Code: display, Message: msg})
	}
	return out
}
