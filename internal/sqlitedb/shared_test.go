// Package sqlitedb_test checks that the app's stores really do coexist in one
// database. It lives outside the package so it can import the stores that
// import sqlitedb.
package sqlitedb_test

import (
	"crypto/ecdh"
	"crypto/rand"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"

	"github.com/brhelwig/bambu-util/internal/history"
	"github.com/brhelwig/bambu-util/internal/push"
	"github.com/brhelwig/bambu-util/internal/sqlitedb"
)

func shared(t *testing.T, path string) (*sql.DB, *history.Store, *push.Store) {
	t.Helper()
	db, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	frames, err := history.New(db)
	if err != nil {
		t.Fatalf("history store: %v", err)
	}
	subs, err := push.New(db)
	if err != nil {
		t.Fatalf("push store: %v", err)
	}
	return db, frames, subs
}

func testSubscription(t *testing.T, endpoint string) push.Subscription {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatalf("generate auth: %v", err)
	}
	return push.Subscription{Endpoint: endpoint, P256dh: key.PublicKey().Bytes(), Auth: auth}
}

func TestBothStoresLiveInOneDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bambu-util.db")
	db, frames, subs := shared(t, path)

	if err := frames.InsertFrame(1000, []byte("jpeg")); err != nil {
		t.Fatalf("insert frame: %v", err)
	}
	if err := subs.Save(testSubscription(t, "https://push.example.net/a"), 1000); err != nil {
		t.Fatalf("save subscription: %v", err)
	}
	key, err := subs.Key()
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	want := map[string]bool{"frames": true, "jobs": true, "subscriptions": true, "server_key": true}
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[name] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("table %s is missing from the shared database", name)
		}
	}

	// Reopening the same file must find both stores' data, and above all the
	// same identity — a new one silently unsubscribes every phone.
	db.Close()
	_, frames2, subs2 := shared(t, path)
	if _, newest, err := frames2.Range(); err != nil || newest == nil || *newest != 1000 {
		t.Errorf("frame did not survive reopening: newest=%v err=%v", newest, err)
	}
	if n, err := subs2.Count(); err != nil || n != 1 {
		t.Errorf("subscription did not survive reopening: %d (err %v)", n, err)
	}
	key2, err := subs2.Key()
	if err != nil {
		t.Fatalf("key after reopening: %v", err)
	}
	if key2.Public() != key.Public() {
		t.Errorf("identity changed across a reopen:\n got %s\nwant %s", key2.Public(), key.Public())
	}
}

// Recording writes a frame a second and the pruner deletes in bulk, both while
// someone may be turning notifications on. Sharing one handle puts those in a
// queue; this fails if that ever stops being true.
func TestWritesFromBothStoresInterleave(t *testing.T) {
	_, frames, subs := shared(t, filepath.Join(t.TempDir(), "bambu-util.db"))

	if _, err := frames.OpenJob("benchy.gcode", 0); err != nil {
		t.Fatalf("open job: %v", err)
	}
	jpeg := make([]byte, 4096)
	var wg sync.WaitGroup
	errs := make(chan error, 300)

	wg.Add(3)
	go func() {
		defer wg.Done()
		for ts := range 100 {
			if err := frames.InsertFrame(int64(ts), jpeg); err != nil {
				errs <- err
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range 50 {
			if err := frames.Prune(50, history.DefaultKeptJobs); err != nil {
				errs <- err
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := range 50 {
			sub := testSubscription(t, "https://push.example.net/"+string(rune('a'+i%26)))
			if err := subs.Save(sub, int64(i)); err != nil {
				errs <- err
			}
		}
	}()
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("a write failed while the other store was busy: %v", err)
	}
	if n, err := subs.Count(); err != nil || n == 0 {
		t.Errorf("subscriptions = %d (err %v), want some to have survived", n, err)
	}
}

// A store handed a database it does not own must not close it.
func TestClosingASharedStoreLeavesTheDatabaseUp(t *testing.T) {
	_, frames, subs := shared(t, filepath.Join(t.TempDir(), "bambu-util.db"))
	if err := subs.Close(); err != nil {
		t.Fatalf("close push store: %v", err)
	}
	if err := frames.InsertFrame(1, []byte("jpeg")); err != nil {
		t.Errorf("recording stopped after the other store was closed: %v", err)
	}
	if err := frames.Close(); err != nil {
		t.Fatalf("close history store: %v", err)
	}
	if _, err := subs.Count(); err != nil {
		t.Errorf("subscriptions unreadable after the other store was closed: %v", err)
	}
}

// A store that opened its own database still closes it.
func TestAStoreThatOwnsItsDatabaseClosesIt(t *testing.T) {
	store, err := push.Open(filepath.Join(t.TempDir(), "push.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := store.Count(); err == nil {
		t.Error("the database is still usable after Close")
	}
}
