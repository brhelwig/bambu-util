// Package auth requires a login before anything but the printer's own health
// check can be reached, using OpenID Connect against whatever provider the app
// is pointed at.
//
// Who may log in is the provider's business, not this app's: a valid login for
// its client gets in. Pocket ID, the provider this was built against, restricts
// a client to chosen user groups, which is one place to manage access rather
// than two that can disagree.
package auth

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"time"

	"github.com/brhelwig/bambu-util/internal/sqlitedb"
)

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
  id       TEXT PRIMARY KEY,
  subject  TEXT NOT NULL,
  name     TEXT NOT NULL DEFAULT '',
  created  INTEGER NOT NULL,
  expires  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_expires ON sessions(expires);

CREATE TABLE IF NOT EXISTS pending_logins (
  state    TEXT PRIMARY KEY,
  verifier TEXT NOT NULL,
  nonce    TEXT NOT NULL,
  next     TEXT NOT NULL DEFAULT '',
  expires  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS pending_logins_expires ON pending_logins(expires);
`

// Store holds who is logged in, and the logins that are part-way through.
type Store struct {
	db    *sql.DB
	owned bool
}

// OpenStore makes a store over a database of its own at path, which Close then
// closes. The app shares one database across stores and calls NewStore instead.
func OpenStore(path string) (*Store, error) {
	db, err := sqlitedb.Open(path)
	if err != nil {
		return nil, err
	}
	store, err := NewStore(db)
	if err != nil {
		db.Close()
		return nil, err
	}
	store.owned = true
	return store, nil
}

// NewStore returns a store over db, creating its tables if needed. The caller
// keeps ownership of db.
func NewStore(db *sql.DB) (*Store, error) {
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

// Session is one logged-in browser.
type Session struct {
	ID      string
	Subject string
	Name    string
	Expires time.Time
}

// token returns a value long enough that guessing one is not worth attempting.
func token() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// CreateSession opens a session for the person the provider vouched for, and
// returns the value the cookie carries.
func (s *Store) CreateSession(subject, name string, now, expires time.Time) (string, error) {
	id, err := token()
	if err != nil {
		return "", err
	}
	_, err = s.db.Exec(`INSERT INTO sessions (id, subject, name, created, expires) VALUES (?, ?, ?, ?, ?)`,
		id, subject, name, now.Unix(), expires.Unix())
	if err != nil {
		return "", err
	}
	return id, nil
}

// ErrNoSession is returned when a cookie names no session that is still good.
var ErrNoSession = errors.New("auth: no live session")

// Session returns the session the cookie names, or ErrNoSession when there is
// none or it has lapsed. A lapsed row is left for the sweep rather than deleted
// here, so a read stays a read.
func (s *Store) Session(id string, now time.Time) (*Session, error) {
	row := s.db.QueryRow(`SELECT id, subject, name, expires FROM sessions WHERE id = ?`, id)
	var got Session
	var expires int64
	if err := row.Scan(&got.ID, &got.Subject, &got.Name, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNoSession
		}
		return nil, err
	}
	got.Expires = time.Unix(expires, 0)
	if !got.Expires.After(now) {
		return nil, ErrNoSession
	}
	return &got, nil
}

// Extend pushes a session's lapse time out, so a phone in daily use is not
// asked to log in again on a schedule.
func (s *Store) Extend(id string, expires time.Time) error {
	_, err := s.db.Exec(`UPDATE sessions SET expires = ? WHERE id = ?`, expires.Unix(), id)
	return err
}

// EndSession drops one session, which is what logging out does.
func (s *Store) EndSession(id string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// Pending is a login that has been started but not yet come back.
type Pending struct {
	Verifier string
	Nonce    string
	Next     string
}

// StartLogin records what the callback will need to check when the provider
// sends the browser back.
func (s *Store) StartLogin(state, verifier, nonce, next string, expires time.Time) error {
	_, err := s.db.Exec(`INSERT INTO pending_logins (state, verifier, nonce, next, expires) VALUES (?, ?, ?, ?, ?)`,
		state, verifier, nonce, next, expires.Unix())
	return err
}

// ErrNoLogin is returned when a callback carries a state nothing is waiting on.
var ErrNoLogin = errors.New("auth: no login is waiting on that state")

// TakeLogin returns what was stored for state and deletes it in the same
// breath, so a callback cannot be replayed with the same state twice. An
// expired one is refused and removed just the same.
func (s *Store) TakeLogin(state string, now time.Time) (*Pending, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var got Pending
	var expires int64
	row := tx.QueryRow(`SELECT verifier, nonce, next, expires FROM pending_logins WHERE state = ?`, state)
	if err := row.Scan(&got.Verifier, &got.Nonce, &got.Next, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNoLogin
		}
		return nil, err
	}
	if _, err := tx.Exec(`DELETE FROM pending_logins WHERE state = ?`, state); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if time.Unix(expires, 0).Before(now) {
		return nil, ErrNoLogin
	}
	return &got, nil
}

// Sweep removes sessions and part-way logins that have lapsed.
func (s *Store) Sweep(now time.Time) error {
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE expires <= ?`, now.Unix()); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM pending_logins WHERE expires <= ?`, now.Unix())
	return err
}
