package p1s

import (
	"context"
	"errors"
	"github.com/brhelwig/bambu-util/internal/activity"
	"testing"
	"time"
)

// openTestLog gives a budget far above anything a test records, so trimming
// never interferes with what is being checked.
func openTestLog() *activity.Log {
	log, err := activity.Open(":memory:", func() int64 { return 1 << 20 })
	if err != nil {
		panic(err)
	}
	return log
}

func TestAnUnconfiguredLinkRefusesToStream(t *testing.T) {
	l := NewLink(NewStateCache(), openTestLog())
	err := l.Stream(context.Background(), func([]byte) {})
	if !errors.Is(err, ErrUnconfigured) {
		t.Errorf("Stream = %v, want ErrUnconfigured so the camera loop keeps asking", err)
	}
}

// Commands sent with no printer must go nowhere rather than panic. The guards
// refuse them first, but a printer removed between the guard and the send would
// otherwise reach here.
func TestAnUnconfiguredLinkSwallowsCommands(t *testing.T) {
	l := NewLink(NewStateCache(), openTestLog())
	l.LowerBed()
	l.Home()
	l.Extrude()
	l.UnloadFilament()
	l.PausePrint()
	l.ResumePrint()
	l.StopPrint()
	l.SetBedTemp(60)
	l.SetNozzleTemp(220)
	l.SetChamberLight(true)
	l.SetAmsFilament(0, 1, "GFA00", "FFFFFFFF", "PLA", 190, 230)
	if got := l.Describe(); got != "no printer configured" {
		t.Errorf("Describe = %q", got)
	}
}

// Pointing the app at another printer must not leave the previous one's
// readings on screen.
func TestConfiguringADifferentPrinterForgetsTheOldOne(t *testing.T) {
	cache := NewStateCache()
	cache.Merge(map[string]any{"gcode_state": "RUNNING", "bed_temper": 60.0})
	cache.SetConnected(true)

	l := NewLink(cache, openTestLog())
	l.Configure(Config{IP: "127.0.0.1", Serial: "A", AccessCode: "x"})
	defer l.Stop()

	fields, connected := cache.Snapshot()
	if len(fields) != 0 {
		t.Errorf("the previous printer's readings survived: %v", fields)
	}
	if connected {
		t.Error("reported connected before the new printer has said anything")
	}
	if got := l.Config().IP; got != "127.0.0.1" {
		t.Errorf("Config().IP = %q", got)
	}
}

// A camera attempt against the old printer has to be dropped, or the recording
// keeps following a printer nobody is pointed at any more.
func TestConfiguringInterruptsTheCameraAttempt(t *testing.T) {
	l := NewLink(NewStateCache(), openTestLog())
	// An address nothing answers on, so the dial blocks until it is cancelled.
	l.Configure(Config{IP: "192.0.2.1", Serial: "A", AccessCode: "x"})
	defer l.Stop()

	done := make(chan error, 1)
	go func() { done <- l.Stream(context.Background(), func([]byte) {}) }()
	time.Sleep(50 * time.Millisecond)

	l.Configure(Config{IP: "192.0.2.2", Serial: "B", AccessCode: "y"})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the camera attempt against the old printer was never dropped")
	}
}

func TestConfigIsOnlyCompleteWithEveryPart(t *testing.T) {
	full := Config{IP: "192.0.2.10", Serial: "01P00A", AccessCode: "x"}
	if !full.Complete() {
		t.Error("a full config reads as incomplete")
	}
	for name, c := range map[string]Config{
		"no address": {Serial: "01P00A", AccessCode: "x"},
		"no serial":  {IP: "192.0.2.10", AccessCode: "x"},
		"no code":    {IP: "192.0.2.10", Serial: "01P00A"},
		"empty":      {},
	} {
		if c.Complete() {
			t.Errorf("%s reads as complete", name)
		}
	}
}
