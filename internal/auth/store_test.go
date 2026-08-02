package auth

import (
	"errors"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// A state is good once. The browser is also checked for the cookie it was given,
// but this is the gate that does not depend on the browser having behaved.
func TestAStateCanOnlyBeSpentOnce(t *testing.T) {
	store := openTestStore(t)
	now := time.Unix(1_700_000_000, 0)
	if err := store.StartLogin("the-state", "v", "n", "/", now.Add(loginWindow)); err != nil {
		t.Fatalf("StartLogin: %v", err)
	}

	got, err := store.TakeLogin("the-state", now)
	if err != nil {
		t.Fatalf("TakeLogin: %v", err)
	}
	if got.Verifier != "v" || got.Nonce != "n" {
		t.Errorf("got %+v, want what was stored", got)
	}
	if _, err := store.TakeLogin("the-state", now); !errors.Is(err, ErrNoLogin) {
		t.Errorf("spending the same state twice gave %v, want ErrNoLogin", err)
	}
}

func TestALapsedLoginIsRefused(t *testing.T) {
	store := openTestStore(t)
	now := time.Unix(1_700_000_000, 0)
	if err := store.StartLogin("the-state", "v", "n", "/", now.Add(loginWindow)); err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if _, err := store.TakeLogin("the-state", now.Add(2*loginWindow)); !errors.Is(err, ErrNoLogin) {
		t.Errorf("a lapsed login gave %v, want ErrNoLogin", err)
	}
}

func TestASessionOutlivesNothingButItsExpiry(t *testing.T) {
	store := openTestStore(t)
	now := time.Unix(1_700_000_000, 0)
	id, err := store.CreateSession("user-1", "Ada", now, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := store.Session(id, now)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if got.Subject != "user-1" || got.Name != "Ada" {
		t.Errorf("session = %+v, want who was vouched for", got)
	}
	if _, err := store.Session(id, now.Add(2*time.Hour)); !errors.Is(err, ErrNoSession) {
		t.Errorf("a lapsed session was accepted: %v", err)
	}
	if _, err := store.Session("never-issued", now); !errors.Is(err, ErrNoSession) {
		t.Errorf("an unknown session was accepted: %v", err)
	}
}

func TestSweepingClearsWhatHasLapsed(t *testing.T) {
	store := openTestStore(t)
	now := time.Unix(1_700_000_000, 0)
	if _, err := store.CreateSession("user-1", "Ada", now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.StartLogin("the-state", "v", "n", "/", now.Add(loginWindow)); err != nil {
		t.Fatal(err)
	}
	if err := store.Sweep(now.Add(2 * time.Hour)); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	for _, table := range []string{"sessions", "pending_logins"} {
		var left int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&left); err != nil {
			t.Fatal(err)
		}
		if left != 0 {
			t.Errorf("%s still holds %d lapsed rows", table, left)
		}
	}
}
