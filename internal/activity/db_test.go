package activity

import (
	"database/sql"
	"testing"

	"github.com/brhelwig/bambu-util/internal/sqlitedb"
)

// openDB opens a database at path (":memory:" for a throwaway one) that is
// closed when the test ends.
func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
