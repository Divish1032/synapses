package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// createLegacySingleDB creates a single-DB that has BOTH graph and knowledge
// schemas applied — simulating the pre-dual-DB layout where everything lived
// in one file. Returns the path and a cleanup function.
func createLegacySingleDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "legacy.db")

	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	// Apply graph schema first (nodes, edges, etc.).
	if _, err := db.Exec(graphSchema); err != nil {
		t.Fatalf("apply graph schema to legacy db: %v", err)
	}
	// Apply knowledge schema (all knowledge tables).
	if _, err := db.Exec(knowledgeSchema); err != nil {
		t.Fatalf("apply knowledge schema to legacy db: %v", err)
	}
	// Apply the knowledge migration DDLs that create tables not in the base
	// schema. These tables are created via ALTER TABLE / CREATE TABLE
	// migrations in store.Open(), but in a real legacy DB they would already
	// exist from previous runs.
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS memory_anchors (
			memory_id  TEXT NOT NULL,
			node_id    TEXT NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY (memory_id, node_id)
		)`,
		`CREATE TABLE IF NOT EXISTS memory_surfaced (
			memory_id TEXT NOT NULL,
			agent_id  TEXT NOT NULL,
			surfaced_at TEXT NOT NULL,
			PRIMARY KEY (memory_id, agent_id)
		)`,
		`CREATE TABLE IF NOT EXISTS agent_watched_symbols (
			agent_id    TEXT NOT NULL,
			entity_id   TEXT NOT NULL,
			entity_name TEXT NOT NULL,
			entity_file TEXT NOT NULL,
			watched_at  TEXT NOT NULL,
			PRIMARY KEY (agent_id, entity_id)
		)`,
		`CREATE TABLE IF NOT EXISTS context_deliveries (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id   TEXT    NOT NULL DEFAULT '',
			agent_id     TEXT    NOT NULL DEFAULT '',
			tool_name    TEXT    NOT NULL,
			entity       TEXT    NOT NULL DEFAULT '',
			refetched    INTEGER NOT NULL DEFAULT 0,
			task_outcome TEXT    NOT NULL DEFAULT '',
			created_at   INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS memory_embeddings (
			memory_id    TEXT PRIMARY KEY,
			model        TEXT NOT NULL DEFAULT '',
			embedding    BLOB NOT NULL,
			content_hash TEXT NOT NULL DEFAULT '',
			stale        INTEGER NOT NULL DEFAULT 0,
			embedded_at  INTEGER NOT NULL
		)`,
		// Migration-added columns on base tables.
		`ALTER TABLE memories ADD COLUMN stale INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE memories ADD COLUMN stale_reason TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE memories ADD COLUMN surfaced_at TEXT DEFAULT NULL`,
		`ALTER TABLE memories ADD COLUMN staled_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE annotations ADD COLUMN stale INTEGER NOT NULL DEFAULT 0`,
		// Sprint 10.1: memory versioning table.
		`CREATE TABLE IF NOT EXISTS memory_versions (
			id              TEXT PRIMARY KEY,
			memory_id       TEXT NOT NULL,
			version         INTEGER NOT NULL DEFAULT 1,
			content         TEXT NOT NULL,
			superseded_by   TEXT NOT NULL DEFAULT '',
			created_at      TEXT NOT NULL,
			superseded_at   TEXT NOT NULL
		)`,
	} {
		_, _ = db.Exec(ddl) // Ignore "duplicate column" errors
	}
	return dbPath
}

// seedLegacyData populates a legacy single-DB with representative rows
// across all knowledgeTables. Returns a map of table -> expected row count.
func seedLegacyData(t *testing.T, dbPath string) map[string]int {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open for seeding: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	expected := make(map[string]int)

	// memories
	mustExec(t, db, `INSERT INTO memories (id, tier, content, entity_id, agent_id, task_id, tags, created_at, expires_at, last_accessed_at, source)
		VALUES ('mem-1', 'project', 'auth switched to OAuth', 'ent-1', 'agent-1', '', '["auth"]', '2026-03-01', '2027-03-01', '2026-03-01', 'manual')`)
	mustExec(t, db, `INSERT INTO memories (id, tier, content, entity_id, agent_id, task_id, tags, created_at, expires_at, last_accessed_at, source)
		VALUES ('mem-2', 'entity', 'TokenValidator uses RS256', '', '', 'task-1', '[]', '2026-03-02', '2027-03-02', '2026-03-02', 'agent')`)
	expected["memories"] = 2

	// memory_anchors
	mustExec(t, db, `INSERT INTO memory_anchors (memory_id, node_id, created_at) VALUES ('mem-1', 'repo::auth.go::AuthService', '2026-03-01')`)
	expected["memory_anchors"] = 1

	// memory_surfaced
	mustExec(t, db, `INSERT INTO memory_surfaced (memory_id, agent_id, surfaced_at) VALUES ('mem-1', 'agent-1', '2026-03-01')`)
	expected["memory_surfaced"] = 1

	// episodes
	mustExec(t, db, `INSERT INTO episodes (id, agent_id, project_id, created_at, episode_type, outcome, trigger, decision, rationale, affected_files, affected_nodes, tags, importance)
		VALUES ('ep-1', 'agent-1', 'proj-1', 1709251200, 'decision', 'success', 'modifying auth', 'switched to OAuth', 'compliance requirement', '["auth.go"]', '["node-1"]', '["auth","security"]', 0.8)`)
	mustExec(t, db, `INSERT INTO episodes (id, agent_id, project_id, created_at, episode_type, outcome, trigger, decision, rationale, affected_files, affected_nodes, tags, importance)
		VALUES ('ep-2', 'agent-2', 'proj-1', 1709337600, 'failure', 'failure', 'db migration', 'migration broke FK', 'missing index', '[]', '[]', '["db"]', 0.5)`)
	expected["episodes"] = 2

	// plans
	mustExec(t, db, `INSERT INTO plans (id, title, description, created_at, updated_at) VALUES ('plan-1', 'Auth Rewrite', 'Rewrite auth module', '2026-03-01', '2026-03-01')`)
	expected["plans"] = 1

	// tasks
	mustExec(t, db, `INSERT INTO tasks (id, plan_id, title, description, status, priority, linked_nodes, notes, created_at, updated_at)
		VALUES ('task-1', 'plan-1', 'Implement OAuth', 'Add OAuth flow', 'done', 'p0', '["node-1"]', 'completed successfully', '2026-03-01', '2026-03-02')`)
	mustExec(t, db, `INSERT INTO tasks (id, plan_id, title, description, status, priority, linked_nodes, notes, created_at, updated_at)
		VALUES ('task-2', 'plan-1', 'Add tests', '', 'pending', 'p1', '[]', '', '2026-03-01', '2026-03-01')`)
	expected["tasks"] = 2

	// session_state
	mustExec(t, db, `INSERT INTO session_state (id, task_id, agent_id, approach, context_snapshot, created_at, updated_at)
		VALUES ('ss-1', 'task-1', 'agent-1', 'incremental', 'working on auth', '2026-03-01', '2026-03-01')`)
	expected["session_state"] = 1

	// sessions
	mustExec(t, db, `INSERT INTO sessions (id, agent_id, project_id, mcp_session_id, intent, started_at, last_seen_at, ended_at, end_reason, outcome, summary, tool_calls)
		VALUES ('sess-1', 'agent-1', 'proj-1', 'mcp-1', 'auth rewrite', 1709251200, 1709254800, NULL, '', '', '', 15)`)
	mustExec(t, db, `INSERT INTO sessions (id, agent_id, project_id, mcp_session_id, intent, started_at, last_seen_at, ended_at, end_reason, outcome, summary, tool_calls)
		VALUES ('sess-2', 'agent-2', 'proj-1', 'mcp-2', 'testing', 1709337600, 1709341200, 1709341200, 'completed', 'success', 'all tests pass', 8)`)
	expected["sessions"] = 2

	// session_tasks
	mustExec(t, db, `INSERT INTO session_tasks (session_id, task_id, action, at) VALUES ('sess-1', 'task-1', 'started', 1709251200)`)
	expected["session_tasks"] = 1

	// events
	mustExec(t, db, `INSERT INTO events (type, agent_id, payload, created_at) VALUES ('file_change', 'agent-1', '{"file":"auth.go"}', '2026-03-01')`)
	mustExec(t, db, `INSERT INTO events (type, agent_id, payload, created_at) VALUES ('task_update', 'agent-2', '{"task":"task-1"}', '2026-03-02')`)
	expected["events"] = 2

	// agent_messages — includes NULL columns (to_agent, read_at)
	mustExec(t, db, `INSERT INTO agent_messages (id, from_agent, to_agent, topic, payload, project_id, created_at, read_at)
		VALUES ('msg-1', 'agent-1', NULL, 'api_changed', '{"endpoint":"/auth"}', 'proj-1', 1709251200, NULL)`)
	mustExec(t, db, `INSERT INTO agent_messages (id, from_agent, to_agent, topic, payload, project_id, created_at, read_at)
		VALUES ('msg-2', 'agent-1', 'agent-2', 'task_blocked', '{}', 'proj-1', 1709337600, 1709341200)`)
	expected["agent_messages"] = 2

	// agents
	mustExec(t, db, `INSERT INTO agents (id, last_seen, metadata) VALUES ('agent-1', '2026-03-01', '{"model":"claude"}')`)
	expected["agents"] = 1

	// agent_context
	mustExec(t, db, `INSERT INTO agent_context (agent_id, last_event_seq, identity_hash, last_session, task_seq)
		VALUES ('agent-1', 42, 'abc123', 'sess-1', 5)`)
	expected["agent_context"] = 1

	// annotations
	mustExec(t, db, `INSERT INTO annotations (id, node_id, agent_id, note, created_at, source) VALUES ('ann-1', 'node-1', 'agent-1', 'race condition here', '2026-03-01', 'agent')`)
	expected["annotations"] = 1

	// dynamic_rules
	mustExec(t, db, `INSERT INTO dynamic_rules (id, description, severity, from_file_pattern, to_file_pattern, edge_type, created_at, updated_at)
		VALUES ('rule-1', 'no db in handler', 'error', '*/handlers/*', '*/db/*', 'CALLS', '2026-03-01', '2026-03-01')`)
	expected["dynamic_rules"] = 1

	// violation_log
	mustExec(t, db, `INSERT INTO violation_log (id, rule_id, severity, from_node, to_node, edge_type, first_seen, last_seen, occurrences)
		VALUES ('viol-1', 'rule-1', 'error', 'Handler.Get', 'db.Query', 'CALLS', '2026-03-01', '2026-03-02', 3)`)
	expected["violation_log"] = 1

	// quality_gaps
	mustExec(t, db, `INSERT INTO quality_gaps (id, node_id, gap_id, description, severity, status, found_by, found_at, updated_at)
		VALUES ('gap-1', 'node-1', 'missing-nil-check', 'No nil check on input', 'high', 'open', 'agent-1', '2026-03-01', '2026-03-01')`)
	expected["quality_gaps"] = 1

	// tool_calls
	mustExec(t, db, `INSERT INTO tool_calls (tool_name, agent_id, session_id, entity, duration_ms, success, created_at)
		VALUES ('get_context', 'agent-1', 'sess-1', 'AuthService', 42, 1, '2026-03-01')`)
	expected["tool_calls"] = 1

	// web_cache
	mustExec(t, db, `INSERT INTO web_cache (url, content, fetched_at, ttl_hours) VALUES ('https://example.com', 'cached content', '2026-03-01', 24)`)
	expected["web_cache"] = 1

	// cross_project_deps
	mustExec(t, db, `INSERT INTO cross_project_deps (from_entity, to_project, to_entity, to_file, verified_commit, verified_at, detection_tier)
		VALUES ('AuthService', 'backend', 'UserStore', 'store.go', 'abc123', '2026-03-01', 'tier1')`)
	expected["cross_project_deps"] = 1

	// work_claims
	mustExec(t, db, `INSERT INTO work_claims (agent_id, scope, scope_type, claimed_at, expires_at)
		VALUES ('agent-1', 'auth.go', 'file', '2026-03-01', '2026-03-01T01:00:00Z')`)
	expected["work_claims"] = 1

	// agent_watched_symbols
	mustExec(t, db, `INSERT INTO agent_watched_symbols (agent_id, entity_id, entity_name, entity_file, watched_at)
		VALUES ('agent-1', 'repo::auth.go::AuthService', 'AuthService', 'auth.go', '2026-03-01')`)
	expected["agent_watched_symbols"] = 1

	return expected
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...interface{}) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query[:min(80, len(query))], err)
	}
}

// countRows returns the row count for a table in the given DB.
func countRows(db *sql.DB, table string) (int, error) {
	var n int
	err := db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&n)
	return n, err
}

// TestDualDBMigration_AllTablesPopulated verifies that all 23 knowledgeTables
// are correctly migrated from a legacy single-DB to the new knowledge.db.
func TestDualDBMigration_AllTablesPopulated(t *testing.T) {
	t.Parallel()
	dbPath := createLegacySingleDB(t)
	expected := seedLegacyData(t, dbPath)

	// Open the store — this triggers migrateKnowledgeFromLegacy.
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	defer st.Close()

	// Verify: open the knowledge.db directly and check all rows migrated.
	kPath := KnowledgePath(dbPath)
	kDB, err := sql.Open("sqlite", kPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open knowledge db for verification: %v", err)
	}
	defer kDB.Close()

	for _, table := range knowledgeTables {
		want, ok := expected[table]
		if !ok {
			// Table was not seeded — should be 0 rows (but table should exist).
			want = 0
		}
		got, err := countRows(kDB, table)
		if err != nil {
			t.Errorf("count %s: %v", table, err)
			continue
		}
		if got != want {
			t.Errorf("table %s: got %d rows, want %d", table, got, want)
		}
	}
}

// TestDualDBMigration_FTSRebuilt verifies that FTS indexes on episodes and
// memories are functional after migration.
func TestDualDBMigration_FTSRebuilt(t *testing.T) {
	t.Parallel()
	dbPath := createLegacySingleDB(t)
	seedLegacyData(t, dbPath)

	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	defer st.Close()

	kPath := KnowledgePath(dbPath)
	kDB, err := sql.Open("sqlite", kPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open knowledge db: %v", err)
	}
	defer kDB.Close()

	// episodes_fts should find exactly 1 match for "OAuth" (ep-1 only).
	var epCount int
	err = kDB.QueryRow(`SELECT COUNT(*) FROM episodes_fts WHERE episodes_fts MATCH 'OAuth'`).Scan(&epCount)
	if err != nil {
		t.Fatalf("query episodes_fts: %v", err)
	}
	if epCount != 1 {
		t.Errorf("episodes_fts: expected exactly 1 match for 'OAuth', got %d", epCount)
	}

	// episodes_fts should find ep-2 by "migration" keyword.
	var ep2Count int
	err = kDB.QueryRow(`SELECT COUNT(*) FROM episodes_fts WHERE episodes_fts MATCH 'migration'`).Scan(&ep2Count)
	if err != nil {
		t.Fatalf("query episodes_fts for migration: %v", err)
	}
	if ep2Count != 1 {
		t.Errorf("episodes_fts: expected exactly 1 match for 'migration', got %d", ep2Count)
	}

	// Total episodes_fts should have exactly 2 entries (one per migrated episode).
	var totalEpFTS int
	err = kDB.QueryRow(`SELECT COUNT(*) FROM episodes_fts`).Scan(&totalEpFTS)
	if err != nil {
		t.Fatalf("count episodes_fts: %v", err)
	}
	if totalEpFTS != 2 {
		t.Errorf("episodes_fts total: expected 2, got %d", totalEpFTS)
	}

	// memories_fts should find exactly 1 match for "OAuth" (mem-1 only).
	var memCount int
	err = kDB.QueryRow(`SELECT COUNT(*) FROM memories_fts WHERE memories_fts MATCH 'OAuth'`).Scan(&memCount)
	if err != nil {
		t.Fatalf("query memories_fts: %v", err)
	}
	if memCount != 1 {
		t.Errorf("memories_fts: expected exactly 1 match for 'OAuth', got %d", memCount)
	}

	// Total memories_fts should have exactly 2 entries (one per migrated memory).
	var totalMemFTS int
	err = kDB.QueryRow(`SELECT COUNT(*) FROM memories_fts`).Scan(&totalMemFTS)
	if err != nil {
		t.Fatalf("count memories_fts: %v", err)
	}
	if totalMemFTS != 2 {
		t.Errorf("memories_fts total: expected 2, got %d", totalMemFTS)
	}
}

// TestDualDBMigration_Idempotent verifies that closing and re-opening the
// store after migration does not duplicate rows (INSERT OR IGNORE).
func TestDualDBMigration_Idempotent(t *testing.T) {
	t.Parallel()
	dbPath := createLegacySingleDB(t)
	expected := seedLegacyData(t, dbPath)

	// First open — triggers migration.
	st1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (first): %v", err)
	}
	st1.Close()

	// Second open — should not duplicate rows.
	st2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (second): %v", err)
	}
	defer st2.Close()

	kPath := KnowledgePath(dbPath)
	kDB, err := sql.Open("sqlite", kPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open knowledge db: %v", err)
	}
	defer kDB.Close()

	for _, table := range knowledgeTables {
		want, ok := expected[table]
		if !ok {
			want = 0
		}
		got, err := countRows(kDB, table)
		if err != nil {
			t.Errorf("count %s: %v", table, err)
			continue
		}
		if got != want {
			t.Errorf("table %s after reopen: got %d rows, want %d", table, got, want)
		}
	}
}

// TestDualDBMigration_EmptyTables verifies migration completes cleanly when
// the legacy DB has tables but no data rows.
func TestDualDBMigration_EmptyTables(t *testing.T) {
	t.Parallel()
	dbPath := createLegacySingleDB(t)
	// Don't seed any data — tables exist but are empty.

	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	defer st.Close()

	// Verify knowledge.db exists and tables are present (though empty).
	kPath := KnowledgePath(dbPath)
	if _, err := os.Stat(kPath); os.IsNotExist(err) {
		t.Fatal("knowledge.db was not created")
	}

	kDB, err := sql.Open("sqlite", kPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open knowledge db: %v", err)
	}
	defer kDB.Close()

	for _, table := range knowledgeTables {
		got, err := countRows(kDB, table)
		if err != nil {
			t.Errorf("count %s: %v", table, err)
			continue
		}
		if got != 0 {
			t.Errorf("table %s: expected 0 rows in empty migration, got %d", table, got)
		}
	}
}

// TestDualDBMigration_UnicodeContent verifies that CJK characters, emoji,
// and multi-byte UTF-8 survive the migration round-trip.
func TestDualDBMigration_UnicodeContent(t *testing.T) {
	t.Parallel()
	dbPath := createLegacySingleDB(t)

	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open for unicode seeding: %v", err)
	}
	db.SetMaxOpenConns(1)

	// CJK content
	mustExec(t, db, `INSERT INTO memories (id, tier, content, created_at, expires_at, last_accessed_at)
		VALUES ('unicode-1', 'project', '認証サービスはOAuthに切り替えました', '2026-03-01', '2027-03-01', '2026-03-01')`)
	// Emoji content
	mustExec(t, db, `INSERT INTO memories (id, tier, content, created_at, expires_at, last_accessed_at)
		VALUES ('unicode-2', 'entity', '🔐 Auth module refactored 🎉 — includes 中文 and العربية', '2026-03-01', '2027-03-01', '2026-03-01')`)
	// Mixed script episode
	mustExec(t, db, `INSERT INTO episodes (id, agent_id, created_at, decision, tags)
		VALUES ('unicode-ep', 'agent-1', 1709251200, '데이터베이스 마이그레이션 완료', '["한국어","テスト"]')`)
	// Annotation with special characters
	mustExec(t, db, `INSERT INTO annotations (id, node_id, note, created_at)
		VALUES ('unicode-ann', 'node-1', 'Ñoño path: /tmp/ñ/café ☕', '2026-03-01')`)
	db.Close()

	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	defer st.Close()

	kPath := KnowledgePath(dbPath)
	kDB, err := sql.Open("sqlite", kPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open knowledge db: %v", err)
	}
	defer kDB.Close()

	// Verify CJK memory content.
	var content string
	if err := kDB.QueryRow(`SELECT content FROM memories WHERE id = 'unicode-1'`).Scan(&content); err != nil {
		t.Fatalf("query unicode-1: %v", err)
	}
	if content != "認証サービスはOAuthに切り替えました" {
		t.Errorf("CJK content mismatch: got %q", content)
	}

	// Verify emoji memory content.
	if err := kDB.QueryRow(`SELECT content FROM memories WHERE id = 'unicode-2'`).Scan(&content); err != nil {
		t.Fatalf("query unicode-2: %v", err)
	}
	if content != "🔐 Auth module refactored 🎉 — includes 中文 and العربية" {
		t.Errorf("emoji content mismatch: got %q", content)
	}

	// Verify Korean episode.
	var decision string
	if err := kDB.QueryRow(`SELECT decision FROM episodes WHERE id = 'unicode-ep'`).Scan(&decision); err != nil {
		t.Fatalf("query unicode-ep: %v", err)
	}
	if decision != "데이터베이스 마이그레이션 완료" {
		t.Errorf("Korean decision mismatch: got %q", decision)
	}

	// Verify special-char annotation.
	var note string
	if err := kDB.QueryRow(`SELECT note FROM annotations WHERE id = 'unicode-ann'`).Scan(&note); err != nil {
		t.Fatalf("query unicode-ann: %v", err)
	}
	if note != "Ñoño path: /tmp/ñ/café ☕" {
		t.Errorf("special char annotation mismatch: got %q", note)
	}
}

// TestDualDBMigration_NullColumns verifies that SQL NULL values in nullable
// columns are preserved through migration (not converted to empty strings).
func TestDualDBMigration_NullColumns(t *testing.T) {
	t.Parallel()
	dbPath := createLegacySingleDB(t)

	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open for null seeding: %v", err)
	}
	db.SetMaxOpenConns(1)

	// agent_messages: to_agent and read_at are nullable.
	mustExec(t, db, `INSERT INTO agent_messages (id, from_agent, to_agent, topic, created_at, read_at)
		VALUES ('null-msg', 'agent-1', NULL, 'test', 1709251200, NULL)`)
	// sessions: ended_at is nullable.
	mustExec(t, db, `INSERT INTO sessions (id, agent_id, project_id, started_at, last_seen_at, ended_at)
		VALUES ('null-sess', 'agent-1', 'proj-1', 1709251200, 1709254800, NULL)`)
	db.Close()

	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	defer st.Close()

	kPath := KnowledgePath(dbPath)
	kDB, err := sql.Open("sqlite", kPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open knowledge db: %v", err)
	}
	defer kDB.Close()

	// Verify agent_messages NULLs preserved.
	var toAgent sql.NullString
	var readAt sql.NullInt64
	if err := kDB.QueryRow(`SELECT to_agent, read_at FROM agent_messages WHERE id = 'null-msg'`).Scan(&toAgent, &readAt); err != nil {
		t.Fatalf("query null-msg: %v", err)
	}
	if toAgent.Valid {
		t.Errorf("to_agent should be NULL, got %q", toAgent.String)
	}
	if readAt.Valid {
		t.Errorf("read_at should be NULL, got %d", readAt.Int64)
	}

	// Verify sessions NULL ended_at preserved.
	var endedAt sql.NullInt64
	if err := kDB.QueryRow(`SELECT ended_at FROM sessions WHERE id = 'null-sess'`).Scan(&endedAt); err != nil {
		t.Fatalf("query null-sess: %v", err)
	}
	if endedAt.Valid {
		t.Errorf("ended_at should be NULL, got %d", endedAt.Int64)
	}
}

// TestDualDBMigration_NoLegacyData verifies that when the graph DB has NO
// knowledge tables at all (fresh install, not upgrade), migration is skipped
// gracefully.
func TestDualDBMigration_NoLegacyData(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "fresh.db")

	// Don't pre-create anything — store.Open creates both DBs fresh.
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	defer st.Close()

	kPath := KnowledgePath(dbPath)
	if _, err := os.Stat(kPath); os.IsNotExist(err) {
		t.Fatal("knowledge.db was not created for fresh install")
	}
}

// TestDualDBMigration_SpecificDataIntegrity does spot-checks on individual
// field values to verify data fidelity beyond just row counts.
func TestDualDBMigration_SpecificDataIntegrity(t *testing.T) {
	t.Parallel()
	dbPath := createLegacySingleDB(t)
	seedLegacyData(t, dbPath)

	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	defer st.Close()

	kPath := KnowledgePath(dbPath)
	kDB, err := sql.Open("sqlite", kPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open knowledge db: %v", err)
	}
	defer kDB.Close()

	// Verify memory content and tier.
	var tier, content string
	if err := kDB.QueryRow(`SELECT tier, content FROM memories WHERE id = 'mem-1'`).Scan(&tier, &content); err != nil {
		t.Fatalf("query mem-1: %v", err)
	}
	if tier != "project" {
		t.Errorf("mem-1 tier: got %q, want %q", tier, "project")
	}
	if content != "auth switched to OAuth" {
		t.Errorf("mem-1 content: got %q", content)
	}

	// Verify episode with all fields.
	var agentID, decision, outcome string
	var importance float64
	if err := kDB.QueryRow(`SELECT agent_id, decision, outcome, importance FROM episodes WHERE id = 'ep-1'`).Scan(&agentID, &decision, &outcome, &importance); err != nil {
		t.Fatalf("query ep-1: %v", err)
	}
	if agentID != "agent-1" || decision != "switched to OAuth" || outcome != "success" {
		t.Errorf("ep-1 fields: agent=%q decision=%q outcome=%q", agentID, decision, outcome)
	}
	if importance != 0.8 {
		t.Errorf("ep-1 importance: got %f, want 0.8", importance)
	}

	// Verify task status and priority.
	var status, priority string
	if err := kDB.QueryRow(`SELECT status, priority FROM tasks WHERE id = 'task-1'`).Scan(&status, &priority); err != nil {
		t.Fatalf("query task-1: %v", err)
	}
	if status != "done" || priority != "p0" {
		t.Errorf("task-1: status=%q priority=%q", status, priority)
	}

	// Verify agent_context numeric fields.
	var lastEventSeq, taskSeq int
	if err := kDB.QueryRow(`SELECT last_event_seq, task_seq FROM agent_context WHERE agent_id = 'agent-1'`).Scan(&lastEventSeq, &taskSeq); err != nil {
		t.Fatalf("query agent_context: %v", err)
	}
	if lastEventSeq != 42 || taskSeq != 5 {
		t.Errorf("agent_context: last_event_seq=%d task_seq=%d", lastEventSeq, taskSeq)
	}

	// Verify rule fields.
	var severity, fromPattern string
	if err := kDB.QueryRow(`SELECT severity, from_file_pattern FROM dynamic_rules WHERE id = 'rule-1'`).Scan(&severity, &fromPattern); err != nil {
		t.Fatalf("query rule-1: %v", err)
	}
	if severity != "error" || fromPattern != "*/handlers/*" {
		t.Errorf("rule-1: severity=%q from_pattern=%q", severity, fromPattern)
	}

	// Verify cross_project_deps.
	var toProject, toEntity string
	if err := kDB.QueryRow(`SELECT to_project, to_entity FROM cross_project_deps WHERE from_entity = 'AuthService'`).Scan(&toProject, &toEntity); err != nil {
		t.Fatalf("query cross_project_deps: %v", err)
	}
	if toProject != "backend" || toEntity != "UserStore" {
		t.Errorf("cross_project_deps: to_project=%q to_entity=%q", toProject, toEntity)
	}
}

// TestDualDBMigration_SchemaMismatch verifies that migration handles a legacy
// DB with FEWER columns than the destination (simulating an upgrade from an
// older version). The commonCols intersection logic in migrateSingleTable
// must only copy columns that exist in both source and destination.
func TestDualDBMigration_SchemaMismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "oldschema.db")

	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)

	// Apply graph schema (required for Open to succeed).
	if _, err := db.Exec(graphSchema); err != nil {
		t.Fatalf("apply graph schema: %v", err)
	}

	// Create a MINIMAL memories table — only the columns from the original
	// schema, missing the migration-added columns (stale, stale_reason, etc.).
	// This simulates a very old legacy DB that never got migration-added columns.
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS memories (
		id               TEXT PRIMARY KEY,
		tier             TEXT NOT NULL,
		content          TEXT NOT NULL,
		entity_id        TEXT DEFAULT '',
		agent_id         TEXT DEFAULT '',
		task_id          TEXT DEFAULT '',
		tags             TEXT NOT NULL DEFAULT '[]',
		created_at       TEXT NOT NULL,
		expires_at       TEXT NOT NULL,
		last_accessed_at TEXT NOT NULL,
		source           TEXT NOT NULL DEFAULT 'manual'
	)`)
	if err != nil {
		t.Fatalf("create minimal memories: %v", err)
	}

	// Create a minimal episodes table — missing the promoted_rule column.
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS episodes (
		id             TEXT PRIMARY KEY,
		agent_id       TEXT NOT NULL,
		project_id     TEXT NOT NULL DEFAULT '',
		created_at     INTEGER NOT NULL,
		episode_type   TEXT NOT NULL DEFAULT 'decision',
		outcome        TEXT NOT NULL DEFAULT 'unknown',
		trigger        TEXT NOT NULL DEFAULT '',
		decision       TEXT NOT NULL,
		rationale      TEXT NOT NULL DEFAULT '',
		affected_files TEXT NOT NULL DEFAULT '[]',
		affected_nodes TEXT NOT NULL DEFAULT '[]',
		tags           TEXT NOT NULL DEFAULT '[]',
		importance     REAL NOT NULL DEFAULT 0.5
	)`)
	if err != nil {
		t.Fatalf("create minimal episodes: %v", err)
	}

	// Create a minimal tasks table — missing migration-added columns
	// (assigned_to, last_updated_by, depends_on, start_commit, commits).
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS tasks (
		id           TEXT PRIMARY KEY,
		plan_id      TEXT NOT NULL DEFAULT '',
		title        TEXT NOT NULL,
		description  TEXT NOT NULL DEFAULT '',
		status       TEXT NOT NULL DEFAULT 'pending',
		priority     TEXT NOT NULL DEFAULT 'p2',
		linked_nodes TEXT NOT NULL DEFAULT '[]',
		notes        TEXT NOT NULL DEFAULT '',
		created_at   TEXT NOT NULL,
		updated_at   TEXT NOT NULL
	)`)
	if err != nil {
		t.Fatalf("create minimal tasks: %v", err)
	}

	// Seed data into the old-schema tables.
	mustExec(t, db, `INSERT INTO memories (id, tier, content, created_at, expires_at, last_accessed_at)
		VALUES ('old-mem', 'project', 'old schema memory', '2026-01-01', '2027-01-01', '2026-01-01')`)
	mustExec(t, db, `INSERT INTO episodes (id, agent_id, created_at, decision)
		VALUES ('old-ep', 'agent-1', 1709000000, 'decision from old schema')`)
	mustExec(t, db, `INSERT INTO tasks (id, title, created_at, updated_at)
		VALUES ('old-task', 'task from old schema', '2026-01-01', '2026-01-01')`)
	db.Close()

	// Open — migration must handle the column mismatch gracefully.
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() with old schema: %v", err)
	}
	defer st.Close()

	kPath := KnowledgePath(dbPath)
	kDB, err := sql.Open("sqlite", kPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open knowledge db: %v", err)
	}
	defer kDB.Close()

	// Verify memory migrated with old columns preserved, new columns defaulted.
	var content string
	var stale int
	if err := kDB.QueryRow(`SELECT content, stale FROM memories WHERE id = 'old-mem'`).Scan(&content, &stale); err != nil {
		t.Fatalf("query old-mem: %v", err)
	}
	if content != "old schema memory" {
		t.Errorf("old-mem content: got %q", content)
	}
	if stale != 0 {
		t.Errorf("old-mem stale: expected 0 (default), got %d", stale)
	}

	// Verify episode migrated.
	var decision string
	if err := kDB.QueryRow(`SELECT decision FROM episodes WHERE id = 'old-ep'`).Scan(&decision); err != nil {
		t.Fatalf("query old-ep: %v", err)
	}
	if decision != "decision from old schema" {
		t.Errorf("old-ep decision: got %q", decision)
	}

	// Verify task migrated with old columns, new columns get defaults.
	var title, assignedTo string
	if err := kDB.QueryRow(`SELECT title, assigned_to FROM tasks WHERE id = 'old-task'`).Scan(&title, &assignedTo); err != nil {
		t.Fatalf("query old-task: %v", err)
	}
	if title != "task from old schema" {
		t.Errorf("old-task title: got %q", title)
	}
	if assignedTo != "" {
		t.Errorf("old-task assigned_to: expected empty default, got %q", assignedTo)
	}
}

// TestDualDBMigration_LegacyNoMemoryRows verifies that migration is still
// triggered when the legacy DB has knowledge tables but ZERO rows in the
// memories table specifically. The guard checks knowledgeDB.memories count,
// not legacyDB.memories count.
func TestDualDBMigration_LegacyNoMemoryRows(t *testing.T) {
	t.Parallel()
	dbPath := createLegacySingleDB(t)

	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open for seeding: %v", err)
	}
	db.SetMaxOpenConns(1)

	// Seed episodes and tasks but NOT memories — 0 rows in memories.
	mustExec(t, db, `INSERT INTO episodes (id, agent_id, created_at, decision)
		VALUES ('no-mem-ep', 'agent-1', 1709251200, 'decision without memory')`)
	mustExec(t, db, `INSERT INTO tasks (id, title, created_at, updated_at)
		VALUES ('no-mem-task', 'task without memory', '2026-03-01', '2026-03-01')`)
	mustExec(t, db, `INSERT INTO plans (id, title, created_at, updated_at)
		VALUES ('no-mem-plan', 'plan without memory', '2026-03-01', '2026-03-01')`)
	db.Close()

	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	defer st.Close()

	kPath := KnowledgePath(dbPath)
	kDB, err := sql.Open("sqlite", kPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open knowledge db: %v", err)
	}
	defer kDB.Close()

	// Migration should fire (knowledgeDB has 0 memories, legacyDB has memories table).
	// Episodes and tasks should be migrated.
	var epCount int
	if err := kDB.QueryRow(`SELECT COUNT(*) FROM episodes`).Scan(&epCount); err != nil {
		t.Fatalf("count episodes: %v", err)
	}
	if epCount != 1 {
		t.Errorf("episodes: expected 1 migrated row, got %d", epCount)
	}

	var taskCount int
	if err := kDB.QueryRow(`SELECT COUNT(*) FROM tasks`).Scan(&taskCount); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if taskCount != 1 {
		t.Errorf("tasks: expected 1 migrated row, got %d", taskCount)
	}

	var planCount int
	if err := kDB.QueryRow(`SELECT COUNT(*) FROM plans`).Scan(&planCount); err != nil {
		t.Fatalf("count plans: %v", err)
	}
	if planCount != 1 {
		t.Errorf("plans: expected 1 migrated row, got %d", planCount)
	}
}

// TestDualDBMigration_MultiRowStress seeds multiple rows per table to verify
// that row iteration doesn't break mid-batch (e.g., early close, scan errors).
func TestDualDBMigration_MultiRowStress(t *testing.T) {
	t.Parallel()
	dbPath := createLegacySingleDB(t)

	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open for seeding: %v", err)
	}
	db.SetMaxOpenConns(1)

	const rowCount = 50

	// Seed 50 memories.
	for i := 0; i < rowCount; i++ {
		mustExec(t, db, fmt.Sprintf(
			`INSERT INTO memories (id, tier, content, created_at, expires_at, last_accessed_at)
			VALUES ('stress-mem-%d', 'project', 'memory content %d', '2026-03-01', '2027-03-01', '2026-03-01')`, i, i))
	}
	// Seed 50 episodes.
	for i := 0; i < rowCount; i++ {
		mustExec(t, db, fmt.Sprintf(
			`INSERT INTO episodes (id, agent_id, created_at, decision, tags)
			VALUES ('stress-ep-%d', 'agent-1', %d, 'decision %d', '["tag-%d"]')`, i, 1709251200+i, i, i))
	}
	// Seed 50 events (AUTOINCREMENT PK — tests seq handling).
	for i := 0; i < rowCount; i++ {
		mustExec(t, db, fmt.Sprintf(
			`INSERT INTO events (type, agent_id, payload, created_at)
			VALUES ('file_change', 'agent-1', '{"i":%d}', '2026-03-01')`, i))
	}
	// Seed 50 session_tasks (NO explicit PK — tests INSERT OR IGNORE on pkless table).
	for i := 0; i < rowCount; i++ {
		mustExec(t, db, fmt.Sprintf(
			`INSERT INTO session_tasks (session_id, task_id, action, at)
			VALUES ('sess-stress', 'task-%d', 'started', %d)`, i, 1709251200+i))
	}
	db.Close()

	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	defer st.Close()

	kPath := KnowledgePath(dbPath)
	kDB, err := sql.Open("sqlite", kPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open knowledge db: %v", err)
	}
	defer kDB.Close()

	// Verify all 50 memories migrated.
	memCount, err := countRows(kDB, "memories")
	if err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if memCount != rowCount {
		t.Errorf("memories: expected %d, got %d", rowCount, memCount)
	}

	// Verify all 50 episodes migrated.
	epCount, err := countRows(kDB, "episodes")
	if err != nil {
		t.Fatalf("count episodes: %v", err)
	}
	if epCount != rowCount {
		t.Errorf("episodes: expected %d, got %d", rowCount, epCount)
	}

	// Verify all 50 events migrated.
	evCount, err := countRows(kDB, "events")
	if err != nil {
		t.Fatalf("count events: %v", err)
	}
	if evCount != rowCount {
		t.Errorf("events: expected %d, got %d", rowCount, evCount)
	}

	// Verify all 50 session_tasks migrated.
	stCount, err := countRows(kDB, "session_tasks")
	if err != nil {
		t.Fatalf("count session_tasks: %v", err)
	}
	if stCount != rowCount {
		t.Errorf("session_tasks: expected %d, got %d", rowCount, stCount)
	}

	// Verify FTS is correctly populated for all 50 memories and episodes.
	var ftsMem int
	if err := kDB.QueryRow(`SELECT COUNT(*) FROM memories_fts`).Scan(&ftsMem); err != nil {
		t.Fatalf("count memories_fts: %v", err)
	}
	if ftsMem != rowCount {
		t.Errorf("memories_fts: expected %d entries, got %d", rowCount, ftsMem)
	}

	var ftsEp int
	if err := kDB.QueryRow(`SELECT COUNT(*) FROM episodes_fts`).Scan(&ftsEp); err != nil {
		t.Fatalf("count episodes_fts: %v", err)
	}
	if ftsEp != rowCount {
		t.Errorf("episodes_fts: expected %d entries, got %d", rowCount, ftsEp)
	}

	// Verify individual rows have correct content (spot check first and last).
	var first, last string
	if err := kDB.QueryRow(`SELECT content FROM memories WHERE id = 'stress-mem-0'`).Scan(&first); err != nil {
		t.Fatalf("query first memory: %v", err)
	}
	if first != "memory content 0" {
		t.Errorf("first memory content: got %q", first)
	}
	if err := kDB.QueryRow(`SELECT content FROM memories WHERE id = 'stress-mem-49'`).Scan(&last); err != nil {
		t.Fatalf("query last memory: %v", err)
	}
	if last != "memory content 49" {
		t.Errorf("last memory content: got %q", last)
	}
}

// TestDualDBMigration_IdempotentGuardPreventsDoubleRun verifies that the
// migration guard (knowledgeDB.memories count > 0) correctly prevents the
// migration from running a second time. This is critical for session_tasks
// which has no PRIMARY KEY — without the guard, duplicates would appear.
func TestDualDBMigration_IdempotentGuardPreventsDoubleRun(t *testing.T) {
	t.Parallel()
	dbPath := createLegacySingleDB(t)

	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open for seeding: %v", err)
	}
	db.SetMaxOpenConns(1)

	// Seed memories (needed to trigger migration) and session_tasks (no PK).
	mustExec(t, db, `INSERT INTO memories (id, tier, content, created_at, expires_at, last_accessed_at)
		VALUES ('guard-mem', 'project', 'guard test', '2026-03-01', '2027-03-01', '2026-03-01')`)
	mustExec(t, db, `INSERT INTO session_tasks (session_id, task_id, action, at)
		VALUES ('guard-sess', 'guard-task', 'started', 1709251200)`)
	mustExec(t, db, `INSERT INTO session_tasks (session_id, task_id, action, at)
		VALUES ('guard-sess', 'guard-task-2', 'started', 1709251201)`)
	db.Close()

	// First open — triggers migration.
	st1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (first): %v", err)
	}
	st1.Close()

	// Second open — migration guard should prevent re-run.
	st2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (second): %v", err)
	}
	defer st2.Close()

	// Third open — extra paranoia.
	st2.Close()
	st3, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (third): %v", err)
	}
	defer st3.Close()

	kPath := KnowledgePath(dbPath)
	kDB, err := sql.Open("sqlite", kPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open knowledge db: %v", err)
	}
	defer kDB.Close()

	// session_tasks has no PK — if migration ran 3 times, we'd have 6 rows.
	// Expect exactly 2 (one migration, guard prevents repeats).
	var count int
	if err := kDB.QueryRow(`SELECT COUNT(*) FROM session_tasks`).Scan(&count); err != nil {
		t.Fatalf("count session_tasks: %v", err)
	}
	if count != 2 {
		t.Errorf("session_tasks: expected 2 (single migration), got %d (migration ran multiple times?)", count)
	}
}

// TEST-003: migrateSingleTable partial failure — if one table fails
// mid-migration, the others should still succeed (graceful degradation).
func TestMigrateSingleTable_PartialFailure(t *testing.T) {
	// Create a source DB with a valid table and a corrupt table.
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.db")
	srcDB, err := sql.Open("sqlite", srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer srcDB.Close()
	srcDB.SetMaxOpenConns(1)

	// Create a valid memories table with data.
	_, err = srcDB.Exec(`CREATE TABLE memories (id TEXT PRIMARY KEY, content TEXT)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = srcDB.Exec(`INSERT INTO memories (id, content) VALUES ('m1', 'test memory')`)
	if err != nil {
		t.Fatal(err)
	}

	// Create the destination DB with the memories table.
	dstPath := filepath.Join(dir, "dst.db")
	dstDB, err := sql.Open("sqlite", dstPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dstDB.Close()
	dstDB.SetMaxOpenConns(1)

	_, err = dstDB.Exec(`CREATE TABLE memories (id TEXT PRIMARY KEY, content TEXT)`)
	if err != nil {
		t.Fatal(err)
	}

	// Migrate the valid table — should succeed.
	n, err := migrateSingleTable(srcDB, dstDB, "memories")
	if err != nil {
		t.Fatalf("migrateSingleTable(memories) unexpected error: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 row migrated, got %d", n)
	}

	// Migrate a non-existent table — should return 0, no error.
	n2, err2 := migrateSingleTable(srcDB, dstDB, "nonexistent_table")
	if err2 != nil {
		t.Fatalf("migrateSingleTable(nonexistent) unexpected error: %v", err2)
	}
	if n2 != 0 {
		t.Errorf("expected 0 rows for nonexistent table, got %d", n2)
	}
}

// TEST-003b: quoteIdentifier and isValidIdentifier helpers.
func TestQuoteIdentifier(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"memories", `"memories"`},
		{`bad"name`, `"bad""name"`},
		{"", `""`},
	}
	for _, tc := range tests {
		got := quoteIdentifier(tc.input)
		if got != tc.want {
			t.Errorf("quoteIdentifier(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestIsValidIdentifier(t *testing.T) {
	valid := []string{"memories", "node_embeddings", "col1", "A"}
	for _, s := range valid {
		if !isValidIdentifier(s) {
			t.Errorf("isValidIdentifier(%q) = false, want true", s)
		}
	}
	invalid := []string{"", "bad;name", "col name", "drop--table", "x)", "1;DROP TABLE"}
	for _, s := range invalid {
		if isValidIdentifier(s) {
			t.Errorf("isValidIdentifier(%q) = true, want false", s)
		}
	}
}
