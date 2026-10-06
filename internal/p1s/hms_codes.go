package p1s

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
)

// errorMessagesJSON is the community English message table (ha-bambulab, MIT),
// filtered to entries that apply to the P1S/P1P. It is not verified against a
// real printer payload. "hms" is keyed by FormatHMSCode output; "print_error"
// by the 8-hex-digit print_error value split into two 4-digit groups.
//
//go:embed error_messages.json
var errorMessagesJSON []byte

// hmsOverrides take precedence over the embedded table.
var hmsOverrides = map[string]string{
	"0300-8000-0003-0002": "AMS filament runout",
}

var (
	messagesOnce sync.Once
	hmsMessages  map[string]string
	printErrors  map[string]string
)

func loadMessages() {
	messagesOnce.Do(func() {
		var t struct {
			HMS        map[string]string `json:"hms"`
			PrintError map[string]string `json:"print_error"`
		}
		if err := json.Unmarshal(errorMessagesJSON, &t); err != nil {
			panic("p1s: bad error_messages.json: " + err.Error())
		}
		hmsMessages, printErrors = t.HMS, t.PrintError
		for k, v := range hmsOverrides {
			hmsMessages[k] = v
		}
	})
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
	loadMessages()
	msg, ok := hmsMessages[code]
	return msg, ok
}

// FormatPrintError renders a print_error value as two dash-grouped 4-digit
// hex words, e.g. "0300-4000".
func FormatPrintError(code int64) string {
	hex := fmt.Sprintf("%08X", code)
	return hex[0:4] + "-" + hex[4:8]
}

// PrintErrorMessage looks up a message for a formatted print_error code.
func PrintErrorMessage(code string) (string, bool) {
	loadMessages()
	msg, ok := printErrors[code]
	return msg, ok
}

// HMSEntry is a single translated printer error: an HMS entry, or the
// separate print_error value.
type HMSEntry struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// HMSErrors reads the raw "hms" field and the non-zero "print_error" value
// from a print report and translates each into a display code plus a message
// (falling back to the raw code when it isn't in the lookup table). Entries
// with an unexpected shape are skipped rather than causing an error.
func HMSErrors(fields map[string]any) []HMSEntry {
	var out []HMSEntry
	if pe, ok := fields["print_error"].(float64); ok && pe > 0 {
		display := FormatPrintError(int64(pe))
		msg, known := PrintErrorMessage(display)
		if !known {
			msg = display
		}
		out = append(out, HMSEntry{Code: display, Message: msg})
	}
	raw, _ := fields["hms"].([]any)
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
