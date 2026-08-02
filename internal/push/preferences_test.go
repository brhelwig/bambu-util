package push

import (
	"testing"
	"time"

	"github.com/brhelwig/bambu-util/internal/activity"
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

func testSender(t *testing.T) *Sender {
	t.Helper()
	sender, err := NewSender(openTestStore(t))
	if err != nil {
		t.Fatalf("new sender: %v", err)
	}
	return sender
}

func testSubscription(endpoint string) Subscription {
	return Subscription{
		Endpoint: endpoint,
		P256dh:   make([]byte, 65),
		Auth:     make([]byte, 16),
	}
}

func TestSubscribeCountAndUnsubscribe(t *testing.T) {
	s := testSender(t)

	if n, err := s.Count(); err != nil || n != 0 {
		t.Fatalf("count = %d (err %v), want 0 before anything subscribes", n, err)
	}

	if err := s.Subscribe(testSubscription("https://push.example/a"), 100); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := s.Subscribe(testSubscription("https://push.example/b"), 200); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if n, err := s.Count(); err != nil || n != 2 {
		t.Fatalf("count = %d (err %v), want 2", n, err)
	}

	if err := s.Unsubscribe("https://push.example/a"); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}
	if n, err := s.Count(); err != nil || n != 1 {
		t.Fatalf("count = %d (err %v), want 1 after one unsubscribed", n, err)
	}

	if _, ok, err := s.Preferences("https://push.example/a"); err != nil || ok {
		t.Errorf("found = %v (err %v), want the unsubscribed device gone", ok, err)
	}
}

// A device that has never chosen is recorded as wanting nothing in particular,
// which the sender reads as every kind — the two must stay distinguishable from
// a device that has chosen nothing at all.
func TestPreferencesRoundTrip(t *testing.T) {
	s := testSender(t)
	endpoint := "https://push.example/a"
	if err := s.Subscribe(testSubscription(endpoint), 100); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	sub, ok, err := s.Preferences(endpoint)
	if err != nil || !ok {
		t.Fatalf("found = %v (err %v), want the new subscription", ok, err)
	}
	if len(sub.Kinds) != 0 {
		t.Errorf("kinds = %v, want none chosen yet", sub.Kinds)
	}

	if err := s.SetPreferences(endpoint, []string{KindPrinterError}, 4*time.Hour); err != nil {
		t.Fatalf("set preferences: %v", err)
	}

	sub, ok, err = s.Preferences(endpoint)
	if err != nil || !ok {
		t.Fatalf("found = %v (err %v), want it still there", ok, err)
	}
	if len(sub.Kinds) != 1 || sub.Kinds[0] != KindPrinterError {
		t.Errorf("kinds = %v, want just %q", sub.Kinds, KindPrinterError)
	}
	if sub.BedInterval != 4*time.Hour {
		t.Errorf("bed interval = %s, want 4h", sub.BedInterval)
	}
}

func TestPreferencesOfAnUnknownDevice(t *testing.T) {
	s := testSender(t)
	if _, ok, err := s.Preferences("https://push.example/never"); err != nil || ok {
		t.Errorf("found = %v (err %v), want nothing for a device that never subscribed", ok, err)
	}
}

func TestWatchSendsNotificationsToTheLog(t *testing.T) {
	s := testSender(t)
	if s.log != nil {
		t.Fatal("a new sender should not be recording anywhere yet")
	}

	log := openTestLog()
	s.Watch(log)

	if s.log != log {
		t.Error("Watch did not attach the log it was given")
	}
}
