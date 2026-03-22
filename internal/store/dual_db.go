package store

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/SynapsesOS/synapses/internal/logutil"
)

// KnowledgePath derives the knowledge DB path from the graph DB path.
// E.g. "/path/to/repo_hash.db" → "/path/to/repo_hash_knowledge.db"
func KnowledgePath(graphPath string) string {
	return strings.TrimSuffix(graphPath, ".db") + "_knowledge.db"
}

// knowledgeTables lists the tables to migrate from a legacy single-DB into
// knowledge.db. FTS virtual tables are excluded — rebuilt by triggers.
var knowledgeTables = []string{
	"memories", "memory_anchors", "memory_surfaced", "episodes",
	"plans", "tasks", "session_state",
	"sessions", "session_tasks",
	"events", "agent_messages", "agents", "agent_context",
	"annotations", "dynamic_rules", "violation_log",
	"quality_gaps",
	"tool_calls", "web_cache", "cross_project_deps",
	"work_claims", "agent_watched_symbols",
}

// migrateKnowledgeFromLegacy copies knowledge tables from the legacy single-DB
// (graphDB) into knowledgeDB. Called once when knowledge.db is freshly created.
// Uses Go-level row copying (not ATTACH) to avoid locking conflicts since
// graphDB is already open with WAL mode.
func migrateKnowledgeFromLegacy(knowledgeDB *sql.DB, graphDBPath string) error {
	// Open a separate read-only connection to the legacy DB with WAL mode
	// and performance pragmas via the shared helper.
	legacyDB, err := openReadOnlySQLiteDB(graphDBPath)
	if err != nil {
		return fmt.Errorf("open legacy db: %w", err)
	}
	defer legacyDB.Close()

	var migrationErrors []string
	for _, table := range knowledgeTables {
		n, err := migrateSingleTable(legacyDB, knowledgeDB, table)
		if err != nil {
			logutil.Error("synapses: dual_db: migrate %s: %v\n", table, err)
			migrationErrors = append(migrationErrors, fmt.Sprintf("%s: %v", table, err))
			continue
		}
		if n > 0 {
			logutil.Info("synapses: dual_db: migrated %d rows from legacy.%s\n", n, table)
		}
	}
	if len(migrationErrors) > 0 {
		return fmt.Errorf("partial migration failures: %s", strings.Join(migrationErrors, "; "))
	}
	return nil
}

// migrateSingleTable copies all rows from srcDB.<table> into dstDB.<table>.
// Returns the number of rows copied, or 0 if the table is absent/empty in src.
func migrateSingleTable(srcDB, dstDB *sql.DB, table string) (int64, error) {
	// Check table exists in source.
	var exists int
	if err := srcDB.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table,
	).Scan(&exists); err != nil || exists == 0 {
		return 0, nil
	}

	// BUG-030: sanitize table name to prevent SQL injection via crafted federation DBs.
	quotedTable := quoteIdentifier(table)

	// Get column names from source table.
	rows, err := srcDB.Query(fmt.Sprintf("PRAGMA table_info(%s)", quotedTable))
	if err != nil {
		return 0, fmt.Errorf("pragma table_info: %w", err)
	}
	var cols []string
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan column info: %w", err)
		}
		// BUG-030: validate column name characters to prevent injection.
		if !isValidIdentifier(name) {
			rows.Close()
			return 0, fmt.Errorf("invalid column name %q in table %s", name, table)
		}
		cols = append(cols, name)
	}
	rows.Close()
	if len(cols) == 0 {
		return 0, nil
	}

	// Check destination has the same columns (may be a subset if source is newer).
	dstRows, err := dstDB.Query(fmt.Sprintf("PRAGMA table_info(%s)", quotedTable))
	if err != nil {
		return 0, fmt.Errorf("dst pragma table_info: %w", err)
	}
	dstCols := make(map[string]bool)
	for dstRows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := dstRows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			dstRows.Close()
			return 0, fmt.Errorf("scan dst column info: %w", err)
		}
		dstCols[name] = true
	}
	dstRows.Close()

	// Use only columns that exist in both source and destination.
	var commonCols []string
	for _, c := range cols {
		if dstCols[c] {
			commonCols = append(commonCols, c)
		}
	}
	if len(commonCols) == 0 {
		return 0, nil
	}

	// BUG-030: quote column names in SQL to prevent injection.
	quotedCols := make([]string, len(commonCols))
	for i, c := range commonCols {
		quotedCols[i] = quoteIdentifier(c)
	}
	colList := strings.Join(quotedCols, ", ")
	placeholders := strings.Repeat("?, ", len(commonCols))
	placeholders = placeholders[:len(placeholders)-2] // trim trailing ", "

	// Read all rows from source.
	srcRows, err := srcDB.Query(fmt.Sprintf("SELECT %s FROM %s", colList, quotedTable))
	if err != nil {
		return 0, fmt.Errorf("select from source: %w", err)
	}
	defer srcRows.Close()

	// Begin transaction on destination.
	tx, err := dstDB.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	insertSQL := fmt.Sprintf("INSERT OR IGNORE INTO %s (%s) VALUES (%s)", quotedTable, colList, placeholders)
	stmt, err := tx.Prepare(insertSQL)
	if err != nil {
		return 0, fmt.Errorf("prepare insert: %w", err)
	}
	defer stmt.Close()

	var count int64
	scanDest := make([]interface{}, len(commonCols))
	scanPtrs := make([]interface{}, len(commonCols))
	for i := range scanDest {
		scanPtrs[i] = &scanDest[i]
	}

	for srcRows.Next() {
		if err := srcRows.Scan(scanPtrs...); err != nil {
			return count, fmt.Errorf("scan row: %w", err)
		}
		if _, err := stmt.Exec(scanDest...); err != nil {
			// Non-fatal per row — skip and continue.
			continue
		}
		count++
	}
	if err := srcRows.Err(); err != nil {
		return count, fmt.Errorf("iterate rows: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return count, nil
}

// quoteIdentifier wraps a SQL identifier in double quotes and escapes any
// embedded double quotes. This prevents SQL injection via crafted column or
// table names from untrusted SQLite databases (BUG-030).
func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// isValidIdentifier checks that a SQL identifier contains only safe characters.
// Rejects names with semicolons, parentheses, or other SQL metacharacters.
func isValidIdentifier(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return true
}
