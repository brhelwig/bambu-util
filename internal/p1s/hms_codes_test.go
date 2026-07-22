package p1s

import "testing"

func TestFormatHMSCode(t *testing.T) {
	got := FormatHMSCode(0x03008000, 0x00030002)
	want := "0300-8000-0003-0002"
	if got != want {
		t.Fatalf("FormatHMSCode() = %q, want %q", got, want)
	}
}

func TestHMSMessage(t *testing.T) {
	msg, ok := HMSMessage("0300-8000-0003-0002")
	if !ok || msg == "" {
		t.Fatalf("expected known message for seeded code, got %q ok=%v", msg, ok)
	}
	_, ok = HMSMessage("FFFF-FFFF-FFFF-FFFF")
	if ok {
		t.Fatal("expected unknown code to report ok=false")
	}
}

func TestHMSErrors(t *testing.T) {
	fields := map[string]any{
		"hms": []any{
			map[string]any{"attr": float64(0x03008000), "code": float64(0x00030002)},
			map[string]any{"attr": float64(0xAAAAAAAA), "code": float64(0xBBBBBBBB)},
		},
	}
	got := HMSErrors(fields)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(got), got)
	}
	if got[0].Code != "0300-8000-0003-0002" || got[0].Message == "" {
		t.Errorf("entry 0 = %+v", got[0])
	}
	if got[1].Code != "AAAA-AAAA-BBBB-BBBB" || got[1].Message != got[1].Code {
		t.Errorf("entry 1 (unknown code) should fall back to raw code as message: %+v", got[1])
	}
}

func TestHMSErrorsDefensive(t *testing.T) {
	cases := []map[string]any{
		{},
		{"hms": "not-an-array"},
		{"hms": []any{"not-a-map"}},
		{"hms": []any{map[string]any{"attr": "not-a-number", "code": float64(1)}}},
	}
	for _, fields := range cases {
		if got := HMSErrors(fields); len(got) != 0 {
			t.Errorf("fields=%v: got %+v, want empty", fields, got)
		}
	}
}
