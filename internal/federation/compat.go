package federation

import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// rawDB is a thin wrapper around sql.DB for schema compatibility checks.
// It opens the database read-only and provides only the minimal interface
// needed for table existence checks.
type rawDB struct {
	db *sql.DB
}

// newRawDB opens a SQLite database in read-only mode without running any
// schema migrations. Used for compatibility checks before handing off to
// store.OpenReadOnly.
func newRawDB(path string) (rawDB, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return rawDB{}, fmt.Errorf("db file not found: %s", path)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return rawDB{}, fmt.Errorf("open raw db: %w", err)
	}
	if _, err := db.Exec("PRAGMA query_only=true; PRAGMA busy_timeout=2000;"); err != nil {
		db.Close()
		return rawDB{}, fmt.Errorf("set read-only: %w", err)
	}
	return rawDB{db: db}, nil
}

// QueryRow delegates to the underlying sql.DB.
func (r rawDB) QueryRow(query string, args ...interface{}) *sql.Row {
	return r.db.QueryRow(query, args...)
}

// Close closes the underlying database connection.
func (r rawDB) Close() error {
	if r.db != nil {
		return r.db.Close()
	}
	return nil
}
