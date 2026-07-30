package push

import (
	"bytes"
	"testing"
)

func testSub(t *testing.T, endpoint string) Subscription {
	t.Helper()
	return newBrowser(t).subscription(endpoint)
}

func TestSaveAndReadBackASubscription(t *testing.T) {
	store := openTestStore(t)
	want := testSub(t, "https://push.example.net/a")
	if err := store.Save(want, testNow.Unix()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("stored %d subscriptions, want 1", len(got))
	}
	if got[0].Endpoint != want.Endpoint ||
		!bytes.Equal(got[0].P256dh, want.P256dh) ||
		!bytes.Equal(got[0].Auth, want.Auth) {
		t.Errorf("subscription came back changed:\n got %+v\nwant %+v", got[0], want)
	}
}

// A browser that re-subscribes reports the same endpoint with fresh keys.
// Keeping both rows would send every notification twice, one of them
// undecryptable.
func TestResubscribingReplacesTheOldKeys(t *testing.T) {
	store := openTestStore(t)
	const endpoint = "https://push.example.net/a"
	first := testSub(t, endpoint)
	if err := store.Save(first, testNow.Unix()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	second := testSub(t, endpoint)
	if err := store.Save(second, testNow.Unix()+60); err != nil {
		t.Fatalf("Save again: %v", err)
	}

	got, err := store.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("stored %d subscriptions, want 1", len(got))
	}
	if !bytes.Equal(got[0].P256dh, second.P256dh) {
		t.Error("the stored key is still the old one")
	}
	if !bytes.Equal(got[0].Auth, second.Auth) {
		t.Error("the stored auth secret is still the old one")
	}
}

func TestTwoPhonesAreBothKept(t *testing.T) {
	store := openTestStore(t)
	for _, e := range []string{"https://push.example.net/a", "https://push.example.net/b"} {
		if err := store.Save(testSub(t, e), testNow.Unix()); err != nil {
			t.Fatalf("Save %s: %v", e, err)
		}
	}
	if n, err := store.Count(); err != nil || n != 2 {
		t.Errorf("Count = %d (err %v), want 2", n, err)
	}
}

func TestDelete(t *testing.T) {
	store := openTestStore(t)
	const endpoint = "https://push.example.net/a"
	if err := store.Save(testSub(t, endpoint), testNow.Unix()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Delete(endpoint); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n, _ := store.Count(); n != 0 {
		t.Errorf("Count = %d, want 0", n)
	}
	// Turning notifications off and the push service reporting a dead endpoint
	// can race, so a second delete must not be an error.
	if err := store.Delete(endpoint); err != nil {
		t.Errorf("deleting an absent subscription: %v", err)
	}
}

// A malformed subscription would fail at encryption time on every send. It is
// cheaper to refuse it at the door.
func TestSaveRejectsAnUnusableSubscription(t *testing.T) {
	store := openTestStore(t)
	good := testSub(t, "https://push.example.net/a")
	cases := map[string]Subscription{
		"no endpoint":     {Endpoint: "", P256dh: good.P256dh, Auth: good.Auth},
		"key wrong size":  {Endpoint: "https://p/a", P256dh: good.P256dh[:20], Auth: good.Auth},
		"no auth secret":  {Endpoint: "https://p/a", P256dh: good.P256dh},
		"nothing at all":  {},
		"key not present": {Endpoint: "https://p/a", Auth: good.Auth},
	}
	for name, sub := range cases {
		if err := store.Save(sub, testNow.Unix()); err == nil {
			t.Errorf("%s: was accepted", name)
		}
	}
	if n, _ := store.Count(); n != 0 {
		t.Errorf("Count = %d, want 0", n)
	}
}

func TestKeyIsGeneratedOnceAndKept(t *testing.T) {
	store := openTestStore(t)
	first, err := store.Key()
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	second, err := store.Key()
	if err != nil {
		t.Fatalf("Key again: %v", err)
	}
	if first.Public() != second.Public() {
		t.Errorf("a second call minted a new identity:\n got %s\nwant %s", second.Public(), first.Public())
	}
}
