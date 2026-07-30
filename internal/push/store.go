package push

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/brhelwig/bambu-util/internal/sqlitedb"
)

const schema = `
CREATE TABLE IF NOT EXISTS subscriptions (
  id              INTEGER PRIMARY KEY,
  endpoint        TEXT NOT NULL UNIQUE,
  p256dh          BLOB NOT NULL,
  auth            BLOB NOT NULL,
  created_ts      INTEGER NOT NULL,
  kinds           TEXT NOT NULL DEFAULT '',
  bed_interval    INTEGER NOT NULL DEFAULT 0,
  bed_reminded_ts INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS server_key (
  id  INTEGER PRIMARY KEY CHECK (id = 1),
  der BLOB NOT NULL
);
`

// Subscription is one browser's address for push messages, as handed over when
// the user turns notifications on, plus what that device asked to be told
// about. Preferences live here rather than in the settings because two devices
// should be able to want different things.
type Subscription struct {
	Endpoint string
	P256dh   []byte // the browser's public key
	Auth     []byte // the shared secret mixed into the encryption

	// Kinds this device wants. Empty means every kind, which is what a device
	// that has never chosen gets.
	Kinds []string
	// BedInterval is how often to repeat the bed reminder while the bed is on
	// with no print running. Zero means never.
	BedInterval time.Duration
	// BedRemindedAt is when this device was last reminded, so each device keeps
	// its own place in its own schedule.
	BedRemindedAt time.Time
}

// Wants reports whether this device asked to be told about a kind. A device
// that has chosen nothing is told about everything, which is what turning
// notifications on and never opening the settings should mean.
func (s Subscription) Wants(kind string) bool {
	if len(s.Kinds) == 0 {
		return true
	}
	return slices.Contains(s.Kinds, kind)
}

// Store holds subscriptions and this server's identity.
type Store struct {
	db    *sql.DB
	owned bool
}

// Open makes a store over a database of its own at path, which Close then
// closes. The app shares one database across stores and calls New instead.
func Open(path string) (*Store, error) {
	db, err := sqlitedb.Open(path)
	if err != nil {
		return nil, err
	}
	store, err := New(db)
	if err != nil {
		db.Close()
		return nil, err
	}
	store.owned = true
	return store, nil
}

// New returns a store over db, creating its tables if needed. The caller keeps
// ownership of db.
func New(db *sql.DB) (*Store, error) {
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the database, unless it belongs to whoever passed it in —
// closing a shared handle would take every other store down with it.
func (s *Store) Close() error {
	if !s.owned {
		return nil
	}
	return s.db.Close()
}

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
	// Re-subscribing refreshes the keys and leaves the preferences alone: the
	// page re-sends its subscription on every load, and that must not undo what
	// the user chose.
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
	rows, err := s.db.Query(`
		SELECT endpoint, p256dh, auth, kinds, bed_interval, bed_reminded_ts
		FROM subscriptions ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subscription
	for rows.Next() {
		var sub Subscription
		var kinds string
		var interval, reminded int64
		if err := rows.Scan(&sub.Endpoint, &sub.P256dh, &sub.Auth, &kinds, &interval, &reminded); err != nil {
			return nil, err
		}
		if kinds != "" {
			sub.Kinds = strings.Split(kinds, ",")
		}
		sub.BedInterval = time.Duration(interval) * time.Second
		if reminded > 0 {
			sub.BedRemindedAt = time.Unix(reminded, 0)
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// Find returns one subscription, or false if it is not stored.
func (s *Store) Find(endpoint string) (Subscription, bool, error) {
	all, err := s.All()
	if err != nil {
		return Subscription{}, false, err
	}
	for _, sub := range all {
		if sub.Endpoint == endpoint {
			return sub, true, nil
		}
	}
	return Subscription{}, false, nil
}

// SetPreferences records what one device wants to be told about.
func (s *Store) SetPreferences(endpoint string, kinds []string, bedInterval time.Duration) error {
	for _, kind := range kinds {
		if !slices.Contains(Kinds, kind) {
			return fmt.Errorf("push: unknown notification %q", kind)
		}
	}
	if bedInterval < 0 {
		return fmt.Errorf("push: reminder interval cannot be negative")
	}
	res, err := s.db.Exec(`UPDATE subscriptions SET kinds = ?, bed_interval = ? WHERE endpoint = ?`,
		strings.Join(kinds, ","), int64(bedInterval.Seconds()), endpoint)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("push: no such subscription")
	}
	return nil
}

// MarkBedReminded records when a device was last reminded, so its schedule is
// its own.
func (s *Store) MarkBedReminded(endpoint string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE subscriptions SET bed_reminded_ts = ? WHERE endpoint = ?`,
		at.Unix(), endpoint)
	return err
}

// ClearBedReminders forgets where every device had got to, for when the bed
// goes off and the next stretch should start counting again.
func (s *Store) ClearBedReminders() error {
	_, err := s.db.Exec(`UPDATE subscriptions SET bed_reminded_ts = 0 WHERE bed_reminded_ts != 0`)
	return err
}

// Count reports how many subscriptions are stored.
func (s *Store) Count() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM subscriptions`).Scan(&n)
	return n, err
}
