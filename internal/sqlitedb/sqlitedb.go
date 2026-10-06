// Package sqlitedb opens the app's SQLite database.
package sqlitedb

import (
	"database/sql"

	_ "modernc.org/sqlite"
)

// busy_timeout makes a writer wait for the lock instead of failing with
// "database is locked". WAL lets reads continue during a write (ignored for
// ":memory:"). Incremental auto-vacuum lets the size cap return freed space a
// little at a time; it only applies to a new database, so internal/capacity
// converts older ones.
const pragmas = "?_pragma=busy_timeout(5000)&_pragma=journal_mode(wal)" +
	"&_pragma=auto_vacuum(incremental)"

// Open returns a handle for every store to share. It is limited to one
// connection, which is enough for this app and makes ":memory:" work (a second
// connection would get a separate empty database).
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+pragmas)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}
