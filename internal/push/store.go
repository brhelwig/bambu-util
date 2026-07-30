package push

import (
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS subscriptions (
  id         INTEGER PRIMARY KEY,
  endpoint   TEXT NOT NULL UNIQUE,
  p256dh     BLOB NOT NULL,
  auth       BLOB NOT NULL,
  created_ts INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS server_key (
  id  INTEGER PRIMARY KEY CHECK (id = 1),
  der BLOB NOT NULL
);
`

// Subscription is one browser's address for push messages, as handed over when
// the user turns notifications on.
type Subscription struct {
	Endpoint string
	P256dh   []byte // the browser's public key
	Auth     []byte // the shared secret mixed into the encryption
}

// Store holds subscriptions and this server's identity.
//
// It keeps its own database file rather than sharing the camera history's: that
// one takes a frame every second and is pruned constantly, and its writes are
// serialized on a single connection. Subscriptions are written a few times a
// year and must not queue behind a prune.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path. Use ":memory:" for a
// throwaway in-process database, e.g. in tests.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// Same reasoning as the history store: this driver serializes writes per
	// connection, and a single connection also gives ":memory:" one consistent
	// database instead of a fresh one per connection.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Key returns this server's identity, generating and storing one the first time
// it is asked. The key is persistent because browsers bind their subscription
// to it: a new key silently stops every existing phone from receiving.
func (s *Store) Key() (*Key, error) {
	var der []byte
	err := s.db.QueryRow(`SELECT der FROM server_key WHERE id = 1`).Scan(&der)
	switch {
	case err == nil:
		return ParseKey(der)
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}

	key, err := NewKey()
	if err != nil {
		return nil, err
	}
	der, err = key.Marshal()
	if err != nil {
		return nil, err
	}
	// Two callers racing on first start would otherwise each store a key and
	// the loser's subscriptions would be signed by a key nobody kept.
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO server_key (id, der) VALUES (1, ?)`, der); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(`SELECT der FROM server_key WHERE id = 1`).Scan(&der); err != nil {
		return nil, err
	}
	return ParseKey(der)
}

// Save records a subscription, replacing any earlier one for the same endpoint.
// A browser that re-subscribes reports the same endpoint with fresh keys.
func (s *Store) Save(sub Subscription, ts int64) error {
	if sub.Endpoint == "" {
		return fmt.Errorf("push: subscription has no endpoint")
	}
	if len(sub.P256dh) != keyLength {
		return fmt.Errorf("push: subscription key is %d bytes, want %d", len(sub.P256dh), keyLength)
	}
	if len(sub.Auth) == 0 {
		return fmt.Errorf("push: subscription has no auth secret")
	}
	_, err := s.db.Exec(`
		INSERT INTO subscriptions (endpoint, p256dh, auth, created_ts) VALUES (?, ?, ?, ?)
		ON CONFLICT(endpoint) DO UPDATE SET p256dh = excluded.p256dh, auth = excluded.auth`,
		sub.Endpoint, sub.P256dh, sub.Auth, ts)
	return err
}

// Delete forgets one subscription. Deleting one that is not there is not an
// error: both the user turning notifications off and the push service reporting
// a dead endpoint can race.
func (s *Store) Delete(endpoint string) error {
	_, err := s.db.Exec(`DELETE FROM subscriptions WHERE endpoint = ?`, endpoint)
	return err
}

// All returns every subscription.
func (s *Store) All() ([]Subscription, error) {
	rows, err := s.db.Query(`SELECT endpoint, p256dh, auth FROM subscriptions ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subscription
	for rows.Next() {
		var sub Subscription
		if err := rows.Scan(&sub.Endpoint, &sub.P256dh, &sub.Auth); err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// Count reports how many subscriptions are stored.
func (s *Store) Count() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM subscriptions`).Scan(&n)
	return n, err
}
