// Package sqlitedb opens the app's SQLite database.
package sqlitedb

import (
	"database/sql"

	_ "modernc.org/sqlite"
)

// Open returns a handle for every store to share. Held to one handle because
// SQLite locks the file to write, and because ":memory:" is a separate empty
// database per handle.
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}
