package federation

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver

	"github.com/SynapsesOS/synapses/internal/store"
)

// rawDB is a thin wrapper around sql.DB for schema compatibility checks
// and lightweight metadata reads. It opens the database read-only and
// does NOT run schema migrations, making it safe to use concurrently
// with a running MCP server.
type rawDB struct {
	db *sql.DB
}

// newRawDB opens a SQLite database in read-only mode without running any
// schema migrations. Used for compatibility checks and stats reads in
// Status() — avoids the overhead of store.OpenReadOnly for diagnostic queries.
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

// checkTables verifies that all named tables exist in the database.
// Returns an error naming the first missing table.
func (r rawDB) checkTables(tables ...string) error {
	for _, table := range tables {
		var name string
		err := r.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name)
		if err != nil {
			return fmt.Errorf("missing table %q — incompatible store version", table)
		}
	}
	return nil
}

// readProjectStat reads lightweight project metadata from the meta table.
// Returns nil if the store has no repo_id (uninitialised). This duplicates
// the logic in store.Stat but avoids opening a full store.Store just for
// Status() diagnostics.
func (r rawDB) readProjectStat(dbPath string) (*store.ProjectStat, error) {
	rows, err := r.db.Query(`SELECT key, value FROM meta`)
	if err != nil {
		return nil, fmt.Errorf("read meta: %w", err)
	}
	defer rows.Close()

	kv := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		kv[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if kv["repo_id"] == "" {
		return nil, nil // uninitialised store
	}

	stat := &store.ProjectStat{
		RepoID:   kv["repo_id"],
		RepoRoot: kv["repo_root"],
		DBPath:   dbPath,
	}
	if t, err := time.Parse(time.RFC3339, kv["saved_at"]); err == nil {
		stat.SavedAt = t
	}
	stat.NodeCount, _ = strconv.Atoi(kv["node_count"])
	stat.EdgeCount, _ = strconv.Atoi(kv["edge_count"])
	stat.FileCount, _ = strconv.Atoi(kv["file_count"])
	return stat, nil
}

// Close closes the underlying database connection.
func (r rawDB) Close() error {
	if r.db != nil {
		return r.db.Close()
	}
	return nil
}
