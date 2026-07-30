// Package sqlitedb opens the app's SQLite database.
package sqlitedb

import (
	"database/sql"

	_ "modernc.org/sqlite"
)

// This driver applies no busy timeout of its own, so without one a writer that
// finds the database busy gives up on the spot with "database is locked"
// instead of waiting its turn. Write-ahead logging additionally lets reads go
// on during a write; it is ignored for ":memory:", which has no file to log
// against.
const pragmas = "?_pragma=busy_timeout(5000)&_pragma=journal_mode(wal)"

// Open returns a handle for every store to share. One handle is not a
// constraint SQLite imposes — it is enough for this app's traffic, and it is
// what makes ":memory:" usable, since a second handle to that is a second empty
// database rather than the same one.
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+pragmas)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}
