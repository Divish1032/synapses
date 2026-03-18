// Package store provides SQLite-backed persistence for the code graph.
// The graph is parsed once from source, saved here, and loaded in <1s on
// subsequent starts. Only files that changed (by mtime) are re-parsed.
package store

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver; no CGo required

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
)

// ProjectStat holds the lightweight per-project metadata that can be read
// without loading the full graph. It is populated from the meta key-value table.
type ProjectStat struct {
	RepoID    string
	RepoRoot  string
	SavedAt   time.Time
	NodeCount int
	EdgeCount int
	FileCount int
	DBPath    string
}

const schema = `
CREATE TABLE IF NOT EXISTS nodes (
    id         TEXT PRIMARY KEY,
    type       TEXT NOT NULL,
    name       TEXT NOT NULL,
    package    TEXT NOT NULL DEFAULT '',
    file       TEXT NOT NULL DEFAULT '',
    line       INTEGER NOT NULL DEFAULT 0,
    exported   INTEGER NOT NULL DEFAULT 0,
    metadata   TEXT NOT NULL DEFAULT '{}',
    doc        TEXT NOT NULL DEFAULT '',
    signature  TEXT NOT NULL DEFAULT '',
    line_count INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS edges (
    from_id TEXT NOT NULL,
    to_id   TEXT NOT NULL,
    type    TEXT NOT NULL,
    PRIMARY KEY (from_id, to_id, type)
);

CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Agent Task Memory tables: persist plans and actionable tasks across LLM sessions.
-- This is the foundation for Synapses as an inter-agent communication layer —
-- future sessions call get_pending_tasks() to resume exactly where they left off.
CREATE TABLE IF NOT EXISTS plans (
    id          TEXT PRIMARY KEY,
    title       TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS tasks (
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
);

-- Session state table: captures exact work state so future LLM sessions can
-- resume from the precise point where the previous session stopped.
-- Unlike task.notes (append-only audit trail), session_state is a single mutable
-- row per task that always reflects the latest working state.
CREATE TABLE IF NOT EXISTS session_state (
    id               TEXT NOT NULL,
    task_id          TEXT NOT NULL,          -- one state per task (UNIQUE enforced as PK)
    agent_id         TEXT NOT NULL DEFAULT '',
    approach         TEXT NOT NULL DEFAULT '',      -- current strategy being taken
    files_modified   TEXT NOT NULL DEFAULT '[]',   -- JSON array of file paths
    completed_steps  TEXT NOT NULL DEFAULT '[]',   -- JSON array of step descriptions
    remaining_steps  TEXT NOT NULL DEFAULT '[]',   -- JSON array of step descriptions
    blockers         TEXT NOT NULL DEFAULT '[]',   -- JSON array of blocker descriptions
    decisions        TEXT NOT NULL DEFAULT '[]',   -- JSON array of decision records
    context_snapshot TEXT NOT NULL DEFAULT '',     -- free-form context dump for resumption
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    PRIMARY KEY (task_id)                    -- one row per task; upsert on task_id
);

-- File modification-time table for incremental reindex.
-- Stores path → mtime (Unix nanoseconds) for every file that was successfully
-- parsed during the last full index. Used by smartReindex to skip unchanged files.
CREATE TABLE IF NOT EXISTS file_hashes (
    path     TEXT PRIMARY KEY,
    mod_time INTEGER NOT NULL
);

-- Unresolved call sites collected during parsing. Persisted so that cross-project
-- CALLS edges can be re-resolved on every start after linked project graphs are
-- merged via MergeFrom. Truncated and replaced on each full index.
CREATE TABLE IF NOT EXISTS call_sites (
    caller_id   TEXT NOT NULL,
    caller_file TEXT NOT NULL,
    pkg_alias   TEXT NOT NULL DEFAULT '',
    func_name   TEXT NOT NULL
);

-- Dynamic rules table: stores architectural constraints added/modified by AI
-- agents at runtime. Uses CREATE TABLE IF NOT EXISTS so existing databases
-- pick this up automatically the next time Open() runs the schema — no explicit
-- migration needed.
CREATE TABLE IF NOT EXISTS dynamic_rules (
    id                TEXT PRIMARY KEY,
    description       TEXT NOT NULL DEFAULT '',
    severity          TEXT NOT NULL DEFAULT 'warning',
    from_file_pattern TEXT NOT NULL DEFAULT '',
    to_file_pattern   TEXT NOT NULL DEFAULT '',
    from_type         TEXT NOT NULL DEFAULT '',
    to_type           TEXT NOT NULL DEFAULT '',
    edge_type         TEXT NOT NULL DEFAULT '',
    to_name_pattern   TEXT NOT NULL DEFAULT '',
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL
);

-- Violation audit log: records every violation detected by get_violations.
-- Same violation (rule+from+to+edge) is deduped by a stable SHA-1 ID;
-- re-detection updates last_seen and increments occurrences.
CREATE TABLE IF NOT EXISTS violation_log (
    id          TEXT PRIMARY KEY,
    rule_id     TEXT NOT NULL,
    severity    TEXT NOT NULL,
    from_node   TEXT NOT NULL,
    to_node     TEXT NOT NULL,
    edge_type   TEXT NOT NULL,
    first_seen  TEXT NOT NULL,
    last_seen   TEXT NOT NULL,
    occurrences INTEGER NOT NULL DEFAULT 1
);

CREATE INDEX IF NOT EXISTS idx_nodes_file      ON nodes(file);
CREATE INDEX IF NOT EXISTS idx_nodes_name      ON nodes(name);
CREATE INDEX IF NOT EXISTS idx_edges_from      ON edges(from_id);
CREATE INDEX IF NOT EXISTS idx_edges_to        ON edges(to_id);
CREATE INDEX IF NOT EXISTS idx_tasks_status    ON tasks(status);
CREATE INDEX IF NOT EXISTS idx_tasks_plan_id   ON tasks(plan_id);
CREATE INDEX IF NOT EXISTS idx_call_sites_caller ON call_sites(caller_id);
CREATE INDEX IF NOT EXISTS idx_dynamic_rules_id  ON dynamic_rules(id);
CREATE INDEX IF NOT EXISTS idx_vlog_rule       ON violation_log(rule_id);
CREATE INDEX IF NOT EXISTS idx_vlog_last_seen  ON violation_log(last_seen);

-- R32: Compound indexes for sub-linear access at federation scale (30k+ nodes).
-- All use IF NOT EXISTS — safe to apply to existing databases without migration.
--
-- idx_nodes_type_pkg: type-filtered package queries ("all functions in package X").
--   Used by future get_repo_map and explain_codebase tools.
-- idx_edges_to_type:  inbound edge lookup by type ("who CALLS X?", "what IMPLEMENTS X?").
--   Compound supersedes the single-column idx_edges_to for type-filtered lookups.
-- idx_edges_type_to:  covering index for edge type aggregation ("COUNT(*) WHERE type='CALLS'
--   GROUP BY to_id" in SaveGraph). Covering = both columns in the index, no table fetch.
-- idx_nodes_pkg:      package-only queries ("all nodes in package X", package-local recency).
CREATE INDEX IF NOT EXISTS idx_nodes_type_pkg ON nodes(type, package);
CREATE INDEX IF NOT EXISTS idx_edges_to_type  ON edges(to_id, type);
CREATE INDEX IF NOT EXISTS idx_edges_type_to  ON edges(type, to_id);
CREATE INDEX IF NOT EXISTS idx_nodes_pkg      ON nodes(package);

-- Agent registry: tracks which agents have interacted with Synapses.
-- Self-declared identity (no auth); upserted on every call with agent_id.
CREATE TABLE IF NOT EXISTS agents (
    id        TEXT PRIMARY KEY,
    last_seen TEXT NOT NULL,
    metadata  TEXT NOT NULL DEFAULT '{}'
);

-- Agent context profile: tracks what each agent already knows so session_init
-- can skip unchanged sections and avoid redundant token delivery.
-- identity_hash is SHA-1 of the serialized ProjectIdentity; when it matches
-- the current hash, project_identity is omitted from session_init responses.
CREATE TABLE IF NOT EXISTS agent_context (
    agent_id       TEXT PRIMARY KEY,
    last_event_seq INTEGER NOT NULL DEFAULT 0,
    identity_hash  TEXT    NOT NULL DEFAULT '',
    last_session   TEXT    NOT NULL DEFAULT '',
    task_seq       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_agent_context_id ON agent_context(agent_id);

-- Pull-based event log: agents poll this table to discover what happened since
-- their last check. Append-only; rows older than 24 hours are pruned automatically.
-- Sequence number (seq) acts as a cursor — agents pass since_seq on each call.
CREATE TABLE IF NOT EXISTS events (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    type       TEXT NOT NULL,
    agent_id   TEXT NOT NULL DEFAULT '',
    payload    TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_created ON events(created_at);
CREATE INDEX IF NOT EXISTS idx_events_type    ON events(type);

-- Web documentation cache: stores fetched HTML-stripped content from pkg.go.dev
-- and arbitrary URLs. Go package docs are keyed by "pkg:<importPath>@<version>"
-- with ttl_hours=0 (never expire — valid until go.mod bumps the version).
-- Arbitrary URL cache uses ttl_hours>0 (e.g., 24h). Expiry check:
--   ttl_hours=0 OR datetime(fetched_at, '+' || ttl_hours || ' hours') > datetime('now')
CREATE TABLE IF NOT EXISTS web_cache (
    url        TEXT PRIMARY KEY,
    content    TEXT NOT NULL,
    fetched_at TEXT NOT NULL,
    ttl_hours  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_web_cache_fetched ON web_cache(fetched_at);

-- Shared annotations: agents can attach notes to graph nodes that are visible
-- to all other agents via get_context and find_entity responses.
CREATE TABLE IF NOT EXISTS annotations (
    id         TEXT PRIMARY KEY,
    node_id    TEXT NOT NULL,
    agent_id   TEXT NOT NULL DEFAULT '',
    note       TEXT NOT NULL,
    created_at TEXT NOT NULL,
    source     TEXT NOT NULL DEFAULT 'agent'
);
CREATE INDEX IF NOT EXISTS idx_annotations_node ON annotations(node_id);

-- Work claims: agents declare which files/packages/directories they are actively
-- working on so other agents can detect conflicts before starting overlapping work.
-- Claims expire automatically (TTL set by caller). Pruned on read.
CREATE TABLE IF NOT EXISTS work_claims (
    agent_id   TEXT NOT NULL,
    scope      TEXT NOT NULL,
    scope_type TEXT NOT NULL DEFAULT 'file',
    claimed_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    PRIMARY KEY (agent_id, scope)
);
CREATE INDEX IF NOT EXISTS idx_work_claims_scope   ON work_claims(scope);
CREATE INDEX IF NOT EXISTS idx_work_claims_expires ON work_claims(expires_at);

-- FTS5 full-text search index over node names, signatures, and doc comments.
-- Enables semantic_search queries like "find code that handles authentication"
-- without knowing exact function names. Uses the unicode61 tokenizer; camelCase
-- names are also indexed as space-split words (split_name column) so a query
-- for "carve" finds "CarveEgoGraph".
CREATE VIRTUAL TABLE IF NOT EXISTS nodes_fts USING fts5(
    node_id   UNINDEXED,
    name,
    split_name,
    signature,
    doc,
    tokenize = "unicode61 remove_diacritics 2"
);

-- Agent consensus protocol: agents can propose architectural changes and vote
-- on each other's proposals. A proposal is resolved when approve or reject votes
-- reach the threshold. Moves Synapses from a coordination layer to a governance layer.
CREATE TABLE IF NOT EXISTS proposals (
    id             TEXT PRIMARY KEY,
    agent_id       TEXT NOT NULL,
    title          TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    affected_nodes TEXT NOT NULL DEFAULT '[]',  -- JSON array of node IDs
    status         TEXT NOT NULL DEFAULT 'open', -- open | accepted | rejected | withdrawn
    vote_threshold INTEGER NOT NULL DEFAULT 2,   -- votes needed to decide
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_proposals_status    ON proposals(status);
CREATE INDEX IF NOT EXISTS idx_proposals_agent     ON proposals(agent_id);

CREATE TABLE IF NOT EXISTS proposal_votes (
    proposal_id TEXT NOT NULL,
    agent_id    TEXT NOT NULL,
    vote        TEXT NOT NULL,  -- approve | reject | abstain
    rationale   TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    PRIMARY KEY (proposal_id, agent_id)
);
CREATE INDEX IF NOT EXISTS idx_pvotes_proposal ON proposal_votes(proposal_id);

CREATE TABLE IF NOT EXISTS tool_calls (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    tool_name   TEXT    NOT NULL,
    agent_id    TEXT    NOT NULL DEFAULT '',
    session_id  TEXT    NOT NULL DEFAULT '',  -- Synapses session UUID; '' for pre-init calls
    entity      TEXT    NOT NULL DEFAULT '',
    duration_ms INTEGER NOT NULL DEFAULT 0,
    success     INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_tool_calls_tool    ON tool_calls(tool_name);
CREATE INDEX IF NOT EXISTS idx_tool_calls_ts      ON tool_calls(created_at);
CREATE INDEX IF NOT EXISTS idx_tool_calls_session ON tool_calls(session_id) WHERE session_id != '';

-- Session lifecycle tracking (Layer 1 of Session Intelligence).
-- A session starts on session_init() and ends on end_session() or after 30 min inactivity.
-- last_seen_at is updated on every tool call (heartbeat piggyback) so stale detection
-- needs no background goroutine — just check: last_seen_at < now - 30 minutes.
CREATE TABLE IF NOT EXISTS sessions (
    id              TEXT    PRIMARY KEY,  -- Synapses-generated UUID
    agent_id        TEXT    NOT NULL,
    project_id      TEXT    NOT NULL,
    mcp_session_id  TEXT    NOT NULL DEFAULT '', -- MCP transport connection ID; "stdio" for stdio mode
    intent          TEXT    NOT NULL DEFAULT '', -- declared at session_init
    started_at      INTEGER NOT NULL,     -- Unix epoch seconds (UTC)
    last_seen_at    INTEGER NOT NULL,     -- Unix epoch seconds; updated on every tool call
    ended_at        INTEGER,              -- Unix epoch seconds; NULL = not cleanly closed
    end_reason      TEXT    NOT NULL DEFAULT '', -- clean | timeout | reconciled | superseded
    outcome         TEXT    NOT NULL DEFAULT '', -- success | failure | partial | unknown
    summary         TEXT    NOT NULL DEFAULT '', -- from end_session or reconciliation
    tool_calls      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_sessions_agent   ON sessions(agent_id);
CREATE INDEX IF NOT EXISTS idx_sessions_project ON sessions(project_id);
CREATE INDEX IF NOT EXISTS idx_sessions_active  ON sessions(ended_at) WHERE ended_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_sessions_mcp     ON sessions(mcp_session_id, agent_id, project_id);

-- Links sessions to the tasks they created, claimed, or completed.
-- Enables orphaned task detection: tasks with action 'created' or 'claimed'
-- in a stale session but no corresponding 'completed' action.
CREATE TABLE IF NOT EXISTS session_tasks (
    session_id TEXT    NOT NULL,
    task_id    TEXT    NOT NULL,
    action     TEXT    NOT NULL,  -- created | claimed | completed | abandoned
    at         INTEGER NOT NULL   -- Unix epoch seconds (UTC)
);
CREATE INDEX IF NOT EXISTS idx_session_tasks_session ON session_tasks(session_id);
CREATE INDEX IF NOT EXISTS idx_session_tasks_task    ON session_tasks(task_id);

-- Agent Message Bus: direct and broadcast messaging between agents.
-- Enables inter-agent communication without requiring persistent HTTP endpoints.
-- Agents are ephemeral LLM sessions; SQLite acts as the message broker.
-- to_agent NULL = broadcast (all agents receive it).
-- seq is the cursor for get_messages polling (same pattern as events table).
CREATE TABLE IF NOT EXISTS agent_messages (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    id         TEXT    NOT NULL UNIQUE,
    from_agent TEXT    NOT NULL,
    to_agent   TEXT,                        -- NULL = broadcast to all agents
    topic      TEXT    NOT NULL,            -- e.g. "api_changed", "task_blocked"
    payload    TEXT    NOT NULL DEFAULT '{}', -- arbitrary JSON
    project_id TEXT    NOT NULL DEFAULT '', -- repo context (empty = global)
    created_at INTEGER NOT NULL,            -- Unix seconds
    read_at    INTEGER                      -- NULL = unread; set by mark_read
);
CREATE INDEX IF NOT EXISTS idx_messages_to_agent ON agent_messages(to_agent, read_at);
CREATE INDEX IF NOT EXISTS idx_messages_created  ON agent_messages(created_at);
CREATE INDEX IF NOT EXISTS idx_messages_seq      ON agent_messages(seq);

-- Episodic memory: agents record decisions and failures so future sessions can
-- recall them. episode_type distinguishes decisions from failures; outcome tracks
-- whether the approach worked. promoted_rule links a failure to the dynamic_rule
-- it eventually spawned (closes the failure→pattern→constraint feedback loop).
CREATE TABLE IF NOT EXISTS episodes (
    id              TEXT PRIMARY KEY,
    agent_id        TEXT NOT NULL,
    project_id      TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,            -- Unix seconds
    episode_type    TEXT NOT NULL DEFAULT 'decision', -- 'decision' | 'failure' | 'pattern' | 'rule_proposal'
    outcome         TEXT NOT NULL DEFAULT 'unknown',  -- 'success' | 'failure' | 'partial' | 'unknown'
    trigger         TEXT NOT NULL DEFAULT '',    -- what prompted this episode
    decision        TEXT NOT NULL,               -- the decision or observation (concise)
    rationale       TEXT NOT NULL DEFAULT '',    -- why (1-3 sentences)
    affected_files  TEXT NOT NULL DEFAULT '[]',  -- JSON array of file paths
    affected_nodes  TEXT NOT NULL DEFAULT '[]',  -- JSON array of graph node IDs
    tags            TEXT NOT NULL DEFAULT '[]',  -- JSON array e.g. ["auth", "breaking"]
    importance      REAL NOT NULL DEFAULT 0.5,   -- 0.0-1.0; reserved for future decay/pruning
    promoted_rule   TEXT NOT NULL DEFAULT ''     -- rule_id if promoted to a dynamic_rule
);
CREATE INDEX IF NOT EXISTS idx_episodes_agent   ON episodes(agent_id, created_at);
CREATE INDEX IF NOT EXISTS idx_episodes_project ON episodes(project_id, created_at);
CREATE INDEX IF NOT EXISTS idx_episodes_type    ON episodes(episode_type, outcome);

-- R32: Quality Intelligence — agent-discovered quality gaps on code entities.
-- Unlike architecture violations (deterministic rule checks), quality gaps are
-- agent-asserted findings discovered through reasoning about a specific entity.
-- They persist across sessions, have a severity/status lifecycle, and surface
-- in get_violations() and get_context() so future agents never re-discover them.
CREATE TABLE IF NOT EXISTS quality_gaps (
    id          TEXT PRIMARY KEY,            -- "{node_id}:{gap_id}" stable slug
    node_id     TEXT NOT NULL,
    gap_id      TEXT NOT NULL,               -- human slug: "dist-relative-path"
    description TEXT NOT NULL,
    severity    TEXT NOT NULL DEFAULT 'medium',  -- low | medium | high | critical
    status      TEXT NOT NULL DEFAULT 'open',    -- open | fixed | wontfix
    found_by    TEXT NOT NULL DEFAULT '',
    found_at    TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    fix_notes   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_quality_gaps_node   ON quality_gaps(node_id);
CREATE INDEX IF NOT EXISTS idx_quality_gaps_status ON quality_gaps(status);

-- Vector embeddings for graph nodes. Stored separately from the nodes table
-- to keep the main table lean — embeddings are optional and can be regenerated.
-- embedding is a little-endian float32 BLOB; indexed_at is Unix seconds.
CREATE TABLE IF NOT EXISTS node_embeddings (
    node_id      TEXT PRIMARY KEY,
    model        TEXT NOT NULL DEFAULT '',
    embedding    BLOB NOT NULL,
    content_hash TEXT NOT NULL DEFAULT '',
    indexed_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_embeddings_node ON node_embeddings(node_id);

-- Unified memories table: 3-tier memory model (session_log, entity, project).
-- Graph-attached memories that survive across sessions and agents.
CREATE TABLE IF NOT EXISTS memories (
    id               TEXT PRIMARY KEY,
    tier             TEXT NOT NULL,          -- 'session_log' | 'entity' | 'project'
    content          TEXT NOT NULL,
    entity_id        TEXT DEFAULT '',        -- node_id for entity tier, empty otherwise
    agent_id         TEXT DEFAULT '',
    task_id          TEXT DEFAULT '',
    tags             TEXT NOT NULL DEFAULT '[]', -- JSON array
    created_at       TEXT NOT NULL,
    expires_at       TEXT NOT NULL,
    last_accessed_at TEXT NOT NULL,
    source           TEXT NOT NULL DEFAULT 'manual' -- 'auto' | 'manual' | 'extracted'
);
CREATE INDEX IF NOT EXISTS idx_memories_tier      ON memories(tier);
CREATE INDEX IF NOT EXISTS idx_memories_entity    ON memories(entity_id) WHERE entity_id != '';
CREATE INDEX IF NOT EXISTS idx_memories_agent     ON memories(agent_id) WHERE agent_id != '';
CREATE INDEX IF NOT EXISTS idx_memories_expires   ON memories(expires_at);

-- FTS5 over episode content for check_plan_safety and recall().
-- content='episodes' makes this an external-content FTS5 table: FTS stores its
-- own index but reads content from the episodes table on demand.
-- Triggers below keep the FTS index atomically in sync on every INSERT/UPDATE/DELETE.
CREATE VIRTUAL TABLE IF NOT EXISTS episodes_fts USING fts5(
    decision,
    rationale,
    trigger,
    tags,
    content='episodes',
    content_rowid='rowid'
);

-- Keep episodes_fts in sync automatically. Without these triggers the FTS index
-- goes stale whenever episodes are inserted or deleted, making recall() and
-- check_plan_safety() silently return no results.
CREATE TRIGGER IF NOT EXISTS episodes_ai AFTER INSERT ON episodes BEGIN
    INSERT INTO episodes_fts(rowid, decision, rationale, trigger, tags)
    VALUES (new.rowid, new.decision, new.rationale, new.trigger, new.tags);
END;

CREATE TRIGGER IF NOT EXISTS episodes_ad AFTER DELETE ON episodes BEGIN
    INSERT INTO episodes_fts(episodes_fts, rowid, decision, rationale, trigger, tags)
    VALUES ('delete', old.rowid, old.decision, old.rationale, old.trigger, old.tags);
END;

CREATE TRIGGER IF NOT EXISTS episodes_au AFTER UPDATE ON episodes BEGIN
    INSERT INTO episodes_fts(episodes_fts, rowid, decision, rationale, trigger, tags)
    VALUES ('delete', old.rowid, old.decision, old.rationale, old.trigger, old.tags);
    INSERT INTO episodes_fts(rowid, decision, rationale, trigger, tags)
    VALUES (new.rowid, new.decision, new.rationale, new.trigger, new.tags);
END;

-- Cross-project dependency tracking: records which local entities depend on
-- entities in federated sibling projects. Populated at index time by the
-- federation tracker (Tier 1: deterministic, Tier 2: brain LLM).
-- Used by session_init for git-based drift detection.
CREATE TABLE IF NOT EXISTS cross_project_deps (
    from_entity     TEXT NOT NULL,
    to_project      TEXT NOT NULL,
    to_entity       TEXT NOT NULL,
    to_file         TEXT NOT NULL,
    verified_commit TEXT NOT NULL,
    verified_at     TEXT NOT NULL,
    detection_tier  TEXT NOT NULL DEFAULT 'tier1',
    PRIMARY KEY (from_entity, to_project, to_entity)
);
CREATE INDEX IF NOT EXISTS idx_cross_deps_project ON cross_project_deps(to_project);
CREATE INDEX IF NOT EXISTS idx_cross_deps_file    ON cross_project_deps(to_project, to_file);
`

// Store wraps a SQLite database and provides graph serialisation.
type Store struct {
	db *sql.DB

	// lastPruneMu guards lastPruneAt to prevent redundant concurrent prunes.
	lastPruneMu sync.Mutex
	lastPruneAt time.Time
}

// CacheDir returns the canonical directory where synapses stores all project
// index databases: ~/.synapses/cache/
//
// Using the home directory (rather than os.UserCacheDir which resolves to
// ~/Library/Caches on macOS, ~/.cache on Linux, %LocalAppData% on Windows)
// gives a single, discoverable, cross-platform path that is not subject to
// OS or tool-driven cache eviction.
func CacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home dir: %w", err)
	}
	return filepath.Join(home, ".synapses", "cache"), nil
}

// DefaultPath returns the canonical DB path for a repository root.
// The file lives at ~/.synapses/cache/<reponame>_<hash>.db
func DefaultPath(repoRoot string) (string, error) {
	dir, err := CacheDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}
	// Use a safe filename derived from the absolute repo path.
	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", fmt.Errorf("abs path: %w", err)
	}
	safe := filepath.Base(abs) + "_" + hashPath(abs)
	return filepath.Join(dir, safe+".db"), nil
}

// Open opens (or creates) the SQLite store at the given path and applies
// the schema migrations.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db %s: %w", path, err)
	}
	db.SetMaxOpenConns(1) // SQLite is single-writer
	// WAL journal mode: allows concurrent readers alongside the single writer,
	// so `synapses query` can run while the MCP server holds the connection.
	// Safe to call on existing DBs — SQLite applies it transparently.
	if _, err := db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		// Non-fatal: old DBs without WAL still work, just without concurrent reads.
		fmt.Fprintf(os.Stderr, "synapses: store: enable WAL: %v\n", err)
	}
	// busy_timeout: under heavy watcher load (rapid file changes) the write
	// connection can hit SQLITE_BUSY. Waiting up to 5 s before failing avoids
	// spurious errors without stalling the daemon noticeably.
	if _, err := db.Exec("PRAGMA busy_timeout=5000;"); err != nil {
		fmt.Fprintf(os.Stderr, "synapses: store: set busy_timeout: %v\n", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	// Migrate existing databases: promote metadata fields to first-class columns.
	// "duplicate column name" errors are safe to ignore — they mean the column
	// was already created by CREATE TABLE (fresh DB) or a previous migration run.
	for _, m := range []string{
		`ALTER TABLE nodes ADD COLUMN doc TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE nodes ADD COLUMN signature TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE nodes ADD COLUMN line_count INTEGER NOT NULL DEFAULT 0`,
		// v1.0.4: Agent identity columns
		`ALTER TABLE plans ADD COLUMN created_by TEXT NOT NULL DEFAULT ''`,
		// F11: Smart Task Lifecycle — plan auto-completion timestamp (unix seconds, 0=active).
		`ALTER TABLE plans ADD COLUMN completed_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE tasks ADD COLUMN assigned_to TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE tasks ADD COLUMN last_updated_by TEXT NOT NULL DEFAULT ''`,
		// v1.0.4: Task dependencies
		`ALTER TABLE tasks ADD COLUMN depends_on TEXT NOT NULL DEFAULT '[]'`,
		// v0.2.0: Stable cross-project node identity (survives file renames).
		`ALTER TABLE nodes ADD COLUMN stable_id TEXT NOT NULL DEFAULT ''`,
		// B1: Reflective Synthesis — distinguish agent vs system-generated annotations.
		`ALTER TABLE annotations ADD COLUMN source TEXT NOT NULL DEFAULT 'agent'`,
		// R5: Unified memories table — 3-tier memory model for session continuity.
		`CREATE TABLE IF NOT EXISTS memories (
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
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_tier    ON memories(tier)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_entity  ON memories(entity_id) WHERE entity_id != ''`,
		`CREATE INDEX IF NOT EXISTS idx_memories_agent   ON memories(agent_id) WHERE agent_id != ''`,
		`CREATE INDEX IF NOT EXISTS idx_memories_expires ON memories(expires_at)`,
		// FTS5 over unified memory content so recall() can search across all tiers.
		// content='memories' makes this an external-content table: FTS stores its own
		// index but reads content from the memories table on demand. Triggers below
		// keep the FTS index atomically in sync on every INSERT/UPDATE/DELETE.
		`CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
			content,
			content='memories',
			content_rowid='rowid'
		)`,
		`CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
			INSERT INTO memories_fts(rowid, content) VALUES (new.rowid, new.content);
		END`,
		`CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN
			INSERT INTO memories_fts(memories_fts, rowid, content)
			VALUES ('delete', old.rowid, old.content);
		END`,
		`CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories BEGIN
			INSERT INTO memories_fts(memories_fts, rowid, content)
			VALUES ('delete', old.rowid, old.content);
			INSERT INTO memories_fts(rowid, content) VALUES (new.rowid, new.content);
		END`,
		// Note: memories_fts backfill is handled after migration loop (conditional,
		// like nodes_fts) rather than here, to avoid rebuilding on every startup.
		// B11: Content-hash invalidation — detect stale embeddings when code changes.
		`ALTER TABLE node_embeddings ADD COLUMN content_hash TEXT NOT NULL DEFAULT ''`,
		// Dedup system annotations: prevent identical auditor notes from accumulating
		// on the same node when update_task(done) is called more than once.
		// Partial index (WHERE source='system') leaves agent annotations unrestricted.
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_annotations_system_dedup ON annotations(node_id, note) WHERE source='system'`,
		// GAP-3: Annotation staleness — marks annotations written against a node
		// whose call graph has changed significantly (fan-in delta >20% or node removed).
		`ALTER TABLE annotations ADD COLUMN stale INTEGER NOT NULL DEFAULT 0`,
		// Cross-agent awareness: track what each agent is currently doing so
		// peers can see activity in real time via session_init and get_agents.
		`ALTER TABLE agents ADD COLUMN current_task_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN current_task_title TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN current_focus TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN current_scope TEXT NOT NULL DEFAULT ''`,
		// project_id marks agents synced from federated peer projects.
		// Local agents have project_id = ''. Used for cross-project agent visibility.
		`ALTER TABLE agents ADD COLUMN project_id TEXT NOT NULL DEFAULT ''`,
		// Context Efficiency Layer: agent context profile for incremental session_init.
		`CREATE TABLE IF NOT EXISTS agent_context (
			agent_id       TEXT PRIMARY KEY,
			last_event_seq INTEGER NOT NULL DEFAULT 0,
			identity_hash  TEXT    NOT NULL DEFAULT '',
			last_session   TEXT    NOT NULL DEFAULT '',
			task_seq       INTEGER NOT NULL DEFAULT 0
		)`,
		// Agent behavioral rules: distinguish code-graph (structural) rules from
		// conversation-level (agent) constraints. Existing rows default to 'structural'.
		`ALTER TABLE dynamic_rules ADD COLUMN rule_type TEXT NOT NULL DEFAULT 'structural'`,
		// R28: Provenance labels — trust tier for each node (user-authored | generated | vendored | external).
		// Existing rows default to 'user-authored' (safe: old code has no generated/vendored nodes tagged).
		`ALTER TABLE nodes ADD COLUMN provenance TEXT NOT NULL DEFAULT 'user-authored'`,
		// OF-H1: Generic entity schema — domain field enables non-code nodes (infra, api, docs, issues).
		// Existing rows default to 'code' (zero regression: all current nodes are code entities).
		`ALTER TABLE nodes ADD COLUMN domain TEXT NOT NULL DEFAULT 'code'`,
		// R20: Signature change tracking — prev_signature stores the prior signature of an exported
		// entity so verify_implementation can diff actual changes instead of reporting all callers.
		// Set to the old signature when SaveGraph detects a change; '' when unchanged or new node.
		`ALTER TABLE nodes ADD COLUMN prev_signature TEXT NOT NULL DEFAULT ''`,
		// B29: Inter-agent communication — richer focus tracking for peer awareness.
		`ALTER TABLE agents ADD COLUMN focus_file  TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN focus_since TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN intent      TEXT NOT NULL DEFAULT ''`,
		// B29: Watched symbols — track what each agent has examined via get_context (30-min TTL).
		// Used to compute dependency_alerts in session_init: when another agent edits a symbol
		// this agent was recently examining, that's worth surfacing as a Tier 2 signal.
		`CREATE TABLE IF NOT EXISTS agent_watched_symbols (
			agent_id    TEXT NOT NULL,
			entity_id   TEXT NOT NULL,
			entity_name TEXT NOT NULL,
			entity_file TEXT NOT NULL,
			watched_at  TEXT NOT NULL,
			PRIMARY KEY (agent_id, entity_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_watched_symbols_entity ON agent_watched_symbols(entity_id)`,
	`CREATE INDEX IF NOT EXISTS idx_watched_symbols_agent  ON agent_watched_symbols(agent_id)`,
	`CREATE INDEX IF NOT EXISTS idx_events_agent           ON events(agent_id)`,
		// AM-1: Memory anchors — links memories to graph node IDs for cascade invalidation.
		`CREATE TABLE IF NOT EXISTS memory_anchors (
			memory_id  TEXT NOT NULL,
			node_id    TEXT NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY (memory_id, node_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_anchors_node ON memory_anchors(node_id)`,
		// AM-2: Cascade invalidation — stale flag set when anchor nodes are removed or structurally changed.
		// stale_reason describes why (e.g. "anchor node removed"). surfaced_at tracks when first shown to agent (AM-3).
		`ALTER TABLE memories ADD COLUMN stale INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE memories ADD COLUMN stale_reason TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE memories ADD COLUMN surfaced_at TEXT DEFAULT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_memories_stale ON memories(stale) WHERE stale = 1`,
		// AM-3: staled_at records WHEN a memory was invalidated (distinct from created_at).
		// Without this, ordering by created_at gives misleading "invalidated_at" values.
		`ALTER TABLE memories ADD COLUMN staled_at TEXT NOT NULL DEFAULT ''`,
		// AM-3: Per-agent surfacing log. Each agent gets their own surfacing record
		// so multi-agent setups don't lose invalidation signals when one agent runs first.
		`CREATE TABLE IF NOT EXISTS memory_surfaced (
			memory_id TEXT NOT NULL,
			agent_id  TEXT NOT NULL,
			surfaced_at TEXT NOT NULL,
			PRIMARY KEY (memory_id, agent_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_surfaced_agent ON memory_surfaced(agent_id)`,
		// Session Intelligence (Layer 1): session lifecycle + tool call audit tables.
		// Added for existing databases that were created before this schema was introduced.
		`ALTER TABLE tool_calls ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_tool_calls_session ON tool_calls(session_id) WHERE session_id != ''`,
		// Initial sessions/session_tasks tables (TEXT timestamps — superseded below).
		`CREATE TABLE IF NOT EXISTS sessions (
			id           TEXT PRIMARY KEY,
			agent_id     TEXT NOT NULL,
			project_id   TEXT NOT NULL,
			intent       TEXT NOT NULL DEFAULT '',
			started_at   TEXT NOT NULL,
			last_seen_at TEXT NOT NULL,
			ended_at     TEXT,
			end_reason   TEXT NOT NULL DEFAULT '',
			outcome      TEXT NOT NULL DEFAULT '',
			summary      TEXT NOT NULL DEFAULT '',
			tool_calls   INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_agent   ON sessions(agent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_project ON sessions(project_id)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_active  ON sessions(ended_at) WHERE ended_at IS NULL`,
		`CREATE TABLE IF NOT EXISTS session_tasks (
			session_id TEXT NOT NULL,
			task_id    TEXT NOT NULL,
			action     TEXT NOT NULL,
			at         TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_session_tasks_session ON session_tasks(session_id)`,
		`CREATE INDEX IF NOT EXISTS idx_session_tasks_task    ON session_tasks(task_id)`,
		// Session Intelligence schema upgrade: migrate timestamps from TEXT (RFC3339)
		// to INTEGER (Unix epoch). Sessions are ephemeral — dropped and recreated safely.
		// Any rows in the old TEXT-schema tables are discarded (sessions don't persist
		// meaningful agent state; memories and tasks are unaffected).
		`DROP TABLE IF EXISTS session_tasks`,
		`DROP TABLE IF EXISTS sessions`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id              TEXT    PRIMARY KEY,
			agent_id        TEXT    NOT NULL,
			project_id      TEXT    NOT NULL,
			mcp_session_id  TEXT    NOT NULL DEFAULT '',
			intent          TEXT    NOT NULL DEFAULT '',
			started_at      INTEGER NOT NULL,
			last_seen_at    INTEGER NOT NULL,
			ended_at        INTEGER,
			end_reason      TEXT    NOT NULL DEFAULT '',
			outcome         TEXT    NOT NULL DEFAULT '',
			summary         TEXT    NOT NULL DEFAULT '',
			tool_calls      INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_agent      ON sessions(agent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_project    ON sessions(project_id)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_active     ON sessions(ended_at) WHERE ended_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_mcp        ON sessions(mcp_session_id, agent_id, project_id)`,
		`CREATE TABLE IF NOT EXISTS session_tasks (
			session_id TEXT    NOT NULL,
			task_id    TEXT    NOT NULL,
			action     TEXT    NOT NULL,
			at         INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_session_tasks_session ON session_tasks(session_id)`,
		`CREATE INDEX IF NOT EXISTS idx_session_tasks_task    ON session_tasks(task_id)`,
		// RX2: Cross-project federation — dependency tracking table.
		`CREATE TABLE IF NOT EXISTS cross_project_deps (
			from_entity     TEXT NOT NULL,
			to_project      TEXT NOT NULL,
			to_entity       TEXT NOT NULL,
			to_file         TEXT NOT NULL,
			verified_commit TEXT NOT NULL,
			verified_at     TEXT NOT NULL,
			detection_tier  TEXT NOT NULL DEFAULT 'tier1',
			PRIMARY KEY (from_entity, to_project, to_entity)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_cross_deps_project ON cross_project_deps(to_project)`,
		`CREATE INDEX IF NOT EXISTS idx_cross_deps_file    ON cross_project_deps(to_project, to_file)`,
	} {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") && !strings.Contains(err.Error(), "already has a column") {
			db.Close()
			return nil, fmt.Errorf("migrate schema: %w", err)
		}
	}
	st := &Store{db: db}

	// Rebuild FTS index for existing databases where nodes_fts is empty but
	// the nodes table already has data. This happens when upgrading from a
	// version without FTS support — the user gets search without re-indexing.
	var ftsCount, nodeCount int
	_ = db.QueryRow(`SELECT count(*) FROM nodes_fts`).Scan(&ftsCount)
	_ = db.QueryRow(`SELECT count(*) FROM nodes`).Scan(&nodeCount)
	if ftsCount == 0 && nodeCount > 0 {
		_ = st.rebuildFTS() // best-effort; non-fatal
	}

	// Backfill memories_fts for existing databases upgraded from before the
	// FTS table was created. Only runs when memories exist but FTS is empty,
	// not on every startup.
	var memFtsCount, memCount int
	_ = db.QueryRow(`SELECT count(*) FROM memories_fts`).Scan(&memFtsCount)
	_ = db.QueryRow(`SELECT count(*) FROM memories`).Scan(&memCount)
	if memFtsCount == 0 && memCount > 0 {
		_, _ = db.Exec(`INSERT INTO memories_fts(memories_fts) VALUES ('rebuild')`) // best-effort
	}

	// R32: Emit query plan stats when SYNAPSES_QUERY_STATS=1 is set. Non-fatal.
	if os.Getenv("SYNAPSES_QUERY_STATS") == "1" {
		st.CollectQueryStats(os.Stderr)
	}

	return st, nil
}

// OpenReadOnly opens an existing SQLite store at path in query-only mode.
// It does NOT run schema migrations or FTS rebuilds, making it safe to call
// concurrently with a running MCP server. When the server has WAL mode enabled
// (set in Open), multiple concurrent readers work without contention.
// Returns an error if the file does not exist.
func OpenReadOnly(path string) (*Store, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, fmt.Errorf("no index at %s — run 'synapses index' first", path)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db (ro) %s: %w", path, err)
	}
	db.SetMaxOpenConns(4)
	// query_only prevents accidental writes; busy_timeout lets us wait if the
	// server is mid-commit rather than failing immediately.
	if _, err := db.Exec("PRAGMA query_only=true; PRAGMA busy_timeout=5000;"); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure read-only: %w", err)
	}
	return &Store{db: db}, nil
}

// rebuildFTS repopulates the nodes_fts table from the current nodes table.
// Called once on Open() for existing databases and after SaveGraph().
func (s *Store) rebuildFTS() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.Exec(`DELETE FROM nodes_fts`); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT id, name, signature, doc FROM nodes`)
	if err != nil {
		return err
	}
	defer rows.Close()

	stmt, err := tx.Prepare(`INSERT INTO nodes_fts (node_id, name, split_name, signature, doc) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for rows.Next() {
		var id, name, sig, doc string
		if err := rows.Scan(&id, &name, &sig, &doc); err != nil {
			return err
		}
		if _, err := stmt.Exec(id, name, splitCamelCase(name), sig, doc); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return tx.Commit()
}

// splitCamelCase inserts spaces before each uppercase letter that follows a
// lowercase letter, making camelCase names tokenizable as individual words.
// E.g. "CarveEgoGraph" → "Carve Ego Graph", "handleGetContext" → "handle Get Context".
func splitCamelCase(s string) string {
	if len(s) == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// escapeLike escapes SQLite LIKE wildcards (% and _) in a literal string
// so it can be used safely in a LIKE pattern. Uses \ as the escape char,
// which requires ESCAPE '\' in the LIKE clause.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// NodeExistsByName reports whether a node with the given name exists in the store.
// The match is case-insensitive and also matches qualified names: searching
// "Close" will match a node named "Store.Close" (suffix after the last dot).
// This mirrors graph.Graph.FindByName behaviour.
func (s *Store) NodeExistsByName(name string) (bool, error) {
	return s.NodeExistsByNameCtx(context.Background(), name)
}

// NodeExistsByNameCtx is the context-aware variant of NodeExistsByName.
// The context is threaded into the SQL query — if the context expires,
// the query is cancelled.
func (s *Store) NodeExistsByNameCtx(ctx context.Context, name string) (bool, error) {
	escaped := escapeLike(name)
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM nodes
		 WHERE name = ? COLLATE NOCASE
		    OR name LIKE ? ESCAPE '\' COLLATE NOCASE`,
		name, "%."+escaped,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("node exists check: %w", err)
	}
	return count > 0, nil
}

// FindNodesByName returns lightweight node references matching the given name
// (case-insensitive). Also matches qualified names: searching "Close" finds
// both "Close" and "Store.Close". This mirrors graph.Graph.FindByName behaviour.
func (s *Store) FindNodesByName(name string, limit int) ([]SearchResult, error) {
	return s.FindNodesByNameCtx(context.Background(), name, limit)
}

// FindNodesByNameCtx is the context-aware variant of FindNodesByName.
func (s *Store) FindNodesByNameCtx(ctx context.Context, name string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	escaped := escapeLike(name)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, signature, doc FROM nodes
		 WHERE name = ? COLLATE NOCASE
		    OR name LIKE ? ESCAPE '\' COLLATE NOCASE
		 LIMIT ?`,
		name, "%."+escaped, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("find nodes by name: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.ID, &r.Name, &r.Signature, &r.Doc); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// CrossProjectDep represents a stored dependency on an entity in a sibling project.
type CrossProjectDep struct {
	FromEntity    string `json:"from_entity"`
	ToProject     string `json:"to_project"`
	ToEntity      string `json:"to_entity"`
	ToFile        string `json:"to_file"`
	VerifiedCommit string `json:"verified_commit"`
	VerifiedAt    string `json:"verified_at"`
	DetectionTier string `json:"detection_tier"`
}

// GetCrossProjectDeps returns all cross-project dependencies for a local entity.
func (s *Store) GetCrossProjectDeps(fromEntity string) ([]CrossProjectDep, error) {
	rows, err := s.db.Query(
		`SELECT from_entity, to_project, to_entity, to_file, verified_commit, verified_at, detection_tier
		 FROM cross_project_deps WHERE from_entity = ?`,
		fromEntity,
	)
	if err != nil {
		return nil, fmt.Errorf("get cross-project deps: %w", err)
	}
	defer rows.Close()

	var deps []CrossProjectDep
	for rows.Next() {
		var d CrossProjectDep
		if err := rows.Scan(&d.FromEntity, &d.ToProject, &d.ToEntity, &d.ToFile, &d.VerifiedCommit, &d.VerifiedAt, &d.DetectionTier); err != nil {
			return nil, err
		}
		deps = append(deps, d)
	}
	return deps, rows.Err()
}

// GetCrossProjectDepsByProject returns all deps targeting a specific sibling project.
func (s *Store) GetCrossProjectDepsByProject(project string) ([]CrossProjectDep, error) {
	rows, err := s.db.Query(
		`SELECT from_entity, to_project, to_entity, to_file, verified_commit, verified_at, detection_tier
		 FROM cross_project_deps WHERE to_project = ?`,
		project,
	)
	if err != nil {
		return nil, fmt.Errorf("get cross-project deps by project: %w", err)
	}
	defer rows.Close()

	var deps []CrossProjectDep
	for rows.Next() {
		var d CrossProjectDep
		if err := rows.Scan(&d.FromEntity, &d.ToProject, &d.ToEntity, &d.ToFile, &d.VerifiedCommit, &d.VerifiedAt, &d.DetectionTier); err != nil {
			return nil, err
		}
		deps = append(deps, d)
	}
	return deps, rows.Err()
}

// UpsertCrossProjectDep inserts or updates a cross-project dependency.
func (s *Store) UpsertCrossProjectDep(dep CrossProjectDep) error {
	_, err := s.db.Exec(
		`INSERT INTO cross_project_deps (from_entity, to_project, to_entity, to_file, verified_commit, verified_at, detection_tier)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (from_entity, to_project, to_entity)
		 DO UPDATE SET to_file=excluded.to_file, verified_commit=excluded.verified_commit, verified_at=excluded.verified_at, detection_tier=excluded.detection_tier`,
		dep.FromEntity, dep.ToProject, dep.ToEntity, dep.ToFile, dep.VerifiedCommit, dep.VerifiedAt, dep.DetectionTier,
	)
	if err != nil {
		return fmt.Errorf("upsert cross-project dep: %w", err)
	}
	return nil
}

// DeleteCrossProjectDeps removes all cross-project deps for a local entity.
func (s *Store) DeleteCrossProjectDeps(fromEntity string) error {
	_, err := s.db.Exec(`DELETE FROM cross_project_deps WHERE from_entity = ?`, fromEntity)
	if err != nil {
		return fmt.Errorf("delete cross-project deps: %w", err)
	}
	return nil
}

// UpdateVerifiedCommit updates the verified_commit for a dependency after
// confirming the sibling entity hasn't drifted.
func (s *Store) UpdateVerifiedCommit(toProject, toEntity, newCommit string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(
		`UPDATE cross_project_deps SET verified_commit = ?, verified_at = ?
		 WHERE to_project = ? AND to_entity = ?`,
		newCommit, now, toProject, toEntity,
	)
	if err != nil {
		return fmt.Errorf("update verified commit: %w", err)
	}
	return nil
}

// SearchResult is a node that matched a semantic_search query,
// annotated with its BM25 relevance rank.
type SearchResult struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Signature string  `json:"signature,omitempty"`
	Doc       string  `json:"doc,omitempty"`
	Score     float64 `json:"score"` // higher = more relevant (normalised from BM25)
}

// SemanticSearch queries the FTS5 index using BM25 ranking. Returns up to
// limit results ordered by relevance. Column weights: name=10, split_name=8,
// signature=5, doc=2 — exact name matches rank highest.
// The query is sanitized to avoid FTS5 syntax errors; on failure a LIKE
// fallback is used.
func (s *Store) SemanticSearch(query string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	q := sanitizeFTSQuery(query)
	if q == "" {
		return nil, nil
	}

	// bm25() returns negative values: more negative = better match.
	// We invert the sign so Score is positive and higher = better.
	const ftsSQL = `
        SELECT node_id, name, signature, doc, -bm25(nodes_fts, 0, 10, 8, 5, 2) AS score
        FROM nodes_fts
        WHERE nodes_fts MATCH ?
        ORDER BY score DESC
        LIMIT ?`

	rows, err := s.db.Query(ftsSQL, q, limit)
	if err != nil {
		// FTS5 syntax error — fall back to LIKE search on name.
		return s.likeSearch(query, limit)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.ID, &r.Name, &r.Signature, &r.Doc, &r.Score); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// If FTS returned nothing, try LIKE as a last resort.
	if len(results) == 0 {
		return s.likeSearch(query, limit)
	}
	return results, nil
}

// likeSearch is a fallback when FTS5 returns no results or encounters a syntax
// error. It splits the query into words and matches each word independently
// using LIKE, joining the per-word conditions with OR so that a multi-word
// concept query like "traverse BFS ego" finds nodes matching ANY of the words.
func (s *Store) likeSearch(query string, limit int) ([]SearchResult, error) {
	words := strings.Fields(query)
	if len(words) == 0 {
		return nil, nil
	}
	conds := make([]string, len(words))
	args := make([]interface{}, 0, len(words)*2+1)
	for i, w := range words {
		pat := "%" + strings.ReplaceAll(w, "%", "\\%") + "%"
		conds[i] = "(name LIKE ? OR doc LIKE ?)"
		args = append(args, pat, pat)
	}
	args = append(args, limit)
	rows, err := s.db.Query(fmt.Sprintf(`
        SELECT id, name, signature, doc
        FROM nodes
        WHERE %s
        ORDER BY length(name) ASC
        LIMIT ?`, strings.Join(conds, " OR ")), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.ID, &r.Name, &r.Signature, &r.Doc); err != nil {
			return nil, err
		}
		r.Score = 1.0 // flat score for LIKE results
		results = append(results, r)
	}
	return results, rows.Err()
}

// sanitizeFTSQuery converts a raw user query into a safe FTS5 MATCH expression.
// Each word becomes an unquoted prefix term (word*) joined by OR so that
// "traverse BFS ego" finds any node matching ANY of the three tokens.
// Unquoted prefix syntax is simpler and more portable than "word"* phrase syntax.
// Empty result means the query had no usable terms.
func sanitizeFTSQuery(q string) string {
	// Strip FTS5 special characters to prevent syntax errors.
	replacer := strings.NewReplacer(`"`, " ", `'`, " ", `(`, " ", `)`, " ", `:`, " ", `*`, " ", `.`, " ", `-`, " ", `/`, " ")
	q = strings.TrimSpace(replacer.Replace(q))
	words := strings.Fields(q)
	if len(words) == 0 {
		return ""
	}
	terms := make([]string, len(words))
	for i, w := range words {
		// Prefix match (*) so "carver" finds "carve", "carving", "CarveEgoGraph".
		// Short words (≤2 chars) skip prefix to avoid broad noise matches.
		if len(w) > 2 {
			terms[i] = w + "*"
		} else {
			terms[i] = w
		}
	}
	return strings.Join(terms, " OR ")
}

// Close releases the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// QueryStats reports index coverage for a set of representative hot-path queries.
// It runs EXPLAIN QUERY PLAN on each query and classifies each step as either an
// index hit ("SEARCH USING INDEX") or a full scan ("SCAN").
//
// Only active when the environment variable SYNAPSES_QUERY_STATS=1 is set.
// Has no effect on normal query execution — purely observational.
//
// Example output logged to stderr:
//
//	synapses: query_stats: edges(to_id,type) SEARCH USING INDEX idx_edges_to_type [hit]
//	synapses: query_stats: edges(type,to_id) SEARCH USING INDEX idx_edges_type_to [hit]
//	synapses: query_stats: nodes(type,package) SEARCH USING INDEX idx_nodes_type_pkg [hit]
//	synapses: query_stats: nodes(package) SEARCH USING INDEX idx_nodes_pkg [hit]
//	synapses: query_stats: summary — 4 index hits, 0 full scans
type QueryStats struct {
	IndexHits int
	FullScans int
}

// CollectQueryStats runs EXPLAIN QUERY PLAN on the four R32 hot queries and
// returns a QueryStats summarising how many use an index vs full scan.
// Call once at startup for observability; does not affect query execution.
// Pass os.Stderr for production output or io.Discard to suppress output in tests.
func (s *Store) CollectQueryStats(w io.Writer) QueryStats {
	type probe struct {
		label string
		sql   string
	}
	probes := []probe{
		{
			label: "edges(to_id,type)",
			sql:   `SELECT from_id FROM edges WHERE to_id = 'x' AND type = 'CALLS' LIMIT 1`,
		},
		{
			label: "edges(type,to_id) aggregate",
			sql:   `SELECT to_id, COUNT(*) FROM edges WHERE type = 'CALLS' GROUP BY to_id`,
		},
		{
			label: "nodes(type,package)",
			sql:   `SELECT id FROM nodes WHERE type = 'function' AND package = 'x' LIMIT 1`,
		},
		{
			label: "nodes(package)",
			sql:   `SELECT id FROM nodes WHERE package = 'x' LIMIT 1`,
		},
	}

	var stats QueryStats
	for _, p := range probes {
		rows, err := s.db.Query(`EXPLAIN QUERY PLAN ` + p.sql)
		if err != nil {
			continue
		}
		usesIndex := false
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				continue
			}
			if strings.Contains(detail, "USING INDEX") || strings.Contains(detail, "USING COVERING INDEX") {
				usesIndex = true
				fmt.Fprintf(w, "synapses: query_stats: %s — %s [hit]\n", p.label, detail)
			}
		}
		rows.Close()
		if usesIndex {
			stats.IndexHits++
		} else {
			stats.FullScans++
			fmt.Fprintf(w, "synapses: query_stats: %s — FULL SCAN (no index used)\n", p.label)
		}
	}
	fmt.Fprintf(w, "synapses: query_stats: summary — %d index hits, %d full scans\n",
		stats.IndexHits, stats.FullScans)
	return stats
}

// PruneStaleData removes old rows from tables that grow unbounded over time.
// retentionDays controls the cutoff; rows older than that are deleted.
// Safe to call concurrently (each DELETE is a separate implicit transaction).
// Intended to be called once on startup in a background goroutine.
func (s *Store) PruneStaleData(retentionDays int) {
	cutoff := time.Now().AddDate(0, 0, -retentionDays).Format(time.RFC3339)
	cutoffUnix := time.Now().AddDate(0, 0, -retentionDays).Unix()

	// tool_calls: one row per MCP tool invocation — can reach millions.
	s.db.Exec(`DELETE FROM tool_calls WHERE created_at < ?`, cutoff)

	// agent_messages: no built-in TTL.
	s.db.Exec(`DELETE FROM agent_messages WHERE created_at < ?`, cutoff)

	// events: coordination/observability stream — pruned to retention window.
	s.db.Exec(`DELETE FROM events WHERE created_at < ?`, cutoff)

	// episodes: stored as Unix seconds (INTEGER).
	s.db.Exec(`DELETE FROM episodes WHERE created_at < ?`, cutoffUnix)

	// memories: honour their own expires_at field; also remove session_log entries
	// older than the retention window regardless of their expires_at.
	s.db.Exec(`DELETE FROM memories WHERE expires_at != '' AND expires_at < ?`, time.Now().UTC().Format(time.RFC3339))
	s.db.Exec(`DELETE FROM memories WHERE tier = 'session_log' AND created_at < ?`, cutoff)

	// proposals: resolved proposals have no further value after retention period.
	s.db.Exec(`DELETE FROM proposals WHERE status IN ('accepted','rejected','withdrawn') AND updated_at < ?`, cutoff)
	s.db.Exec(`DELETE FROM proposal_votes WHERE proposal_id NOT IN (SELECT id FROM proposals)`)

	// Stale annotations for nodes that no longer exist — actively misleading,
	// safe to remove once the retention window has passed.
	s.db.Exec(`DELETE FROM annotations WHERE stale=1 AND node_id NOT IN (SELECT id FROM nodes)`)

	// SQLite housekeeping.
	s.db.Exec(`PRAGMA optimize`)
}

// ── Web cache ──────────────────────────────────────────────────────────────

// WebCacheEntry holds a single cached web document.
type WebCacheEntry struct {
	URL       string
	Content   string
	FetchedAt time.Time
	TTLHours  int
}

// UpsertWebCache inserts or replaces a web cache entry.
// ttlHours=0 means never expire (used for version-pinned package docs).
func (s *Store) UpsertWebCache(url, content string, ttlHours int) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO web_cache(url, content, fetched_at, ttl_hours)
		 VALUES (?, ?, ?, ?)`,
		url, content, time.Now().UTC().Format(time.RFC3339), ttlHours,
	)
	return err
}

// GetWebCache returns the cached entry for url, or (nil, false) if missing or expired.
func (s *Store) GetWebCache(url string) (*WebCacheEntry, bool) {
	row := s.db.QueryRow(
		`SELECT url, content, fetched_at, ttl_hours FROM web_cache WHERE url = ?
		 AND (ttl_hours = 0
		      OR datetime(fetched_at, '+' || ttl_hours || ' hours') > datetime('now'))`,
		url,
	)
	var e WebCacheEntry
	var fetchedAt string
	if err := row.Scan(&e.URL, &e.Content, &fetchedAt, &e.TTLHours); err != nil {
		return nil, false
	}
	e.FetchedAt, _ = time.Parse(time.RFC3339, fetchedAt)
	return &e, true
}

// DeleteWebCachePrefix removes all cache entries whose URL starts with prefix.
// Used to invalidate version-pinned entries when go.mod bumps a package version.
func (s *Store) DeleteWebCachePrefix(prefix string) error {
	_, err := s.db.Exec(`DELETE FROM web_cache WHERE url LIKE ?`, prefix+"%")
	return err
}

// PruneExpiredWebCache removes all entries whose TTL has elapsed.
func (s *Store) PruneExpiredWebCache() error {
	_, err := s.db.Exec(
		`DELETE FROM web_cache
		 WHERE ttl_hours > 0
		   AND datetime(fetched_at, '+' || ttl_hours || ' hours') <= datetime('now')`,
	)
	return err
}

// SaveGraph persists all nodes and edges of g, replacing any existing data.
// A metadata record stores the repo ID and the save timestamp.
func (s *Store) SaveGraph(g *graph.Graph) error {
	// GAP-3: Snapshot CALLS fan-in counts before the wipe so we can detect nodes
	// whose call structure changed significantly and mark their annotations stale.
	oldFanIn := make(map[string]int)
	if fanRows, err := s.db.Query(`SELECT to_id, COUNT(*) FROM edges WHERE type='CALLS' GROUP BY to_id`); err == nil {
		defer fanRows.Close()
		for fanRows.Next() {
			var nid string
			var cnt int
			if fanRows.Scan(&nid, &cnt) == nil {
				oldFanIn[nid] = cnt
			}
		}
	}

	// R20/FIX-R20A: Snapshot current signatures before the wipe so we can detect
	// which entity signatures actually changed. Both exported and unexported
	// entities are captured — test files call unexported functions directly and
	// break compilation when their signature changes. Only non-empty signatures
	// are captured — new nodes (not in this map) are treated as additions, not changes.
	oldSigs := make(map[string]string)
	if sigRows, err := s.db.Query(`SELECT id, signature FROM nodes WHERE signature != ''`); err == nil {
		defer sigRows.Close()
		for sigRows.Next() {
			var nid, sig string
			if sigRows.Scan(&nid, &sig) == nil {
				oldSigs[nid] = sig
			}
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Wipe existing data for a clean replace.
	if _, err := tx.Exec(`DELETE FROM nodes`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM edges`); err != nil {
		return err
	}

	// Insert nodes in batches.
	nodeStmt, err := tx.Prepare(`
        INSERT INTO nodes (id, type, name, package, file, line, exported, metadata, doc, signature, line_count, stable_id, provenance, domain, prev_signature)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `)
	if err != nil {
		return fmt.Errorf("prepare node stmt: %w", err)
	}
	defer nodeStmt.Close()

	// Only persist primary-project nodes; federated nodes from linked projects
	// are reloaded at startup by MergeFrom and must not pollute this store.
	primaryPrefix := g.RepoID() + "::"

	// Insert nodes; count file nodes in the same pass (avoids a second full scan).
	fileCount := 0
	for _, n := range g.AllNodes() {
		if !strings.HasPrefix(string(n.ID), primaryPrefix) {
			continue // skip linked-project node
		}

		// Promote doc/signature/line_count to first-class columns.
		doc := n.Metadata["doc"]
		sig := n.Metadata["signature"]
		lineCount := 0
		if lc, err := strconv.Atoi(n.Metadata["line_count"]); err == nil {
			lineCount = lc
		}
		// Remaining metadata (without promoted fields) goes into the JSON blob.
		remaining := make(map[string]string, len(n.Metadata))
		for k, v := range n.Metadata {
			if k != "doc" && k != "signature" && k != "line_count" {
				remaining[k] = v
			}
		}
		meta, _ := json.Marshal(remaining)

		exported := 0
		if n.Exported {
			exported = 1
		}
		if n.Type == graph.NodeFile {
			fileCount++
		}
		prov := string(n.Provenance)
		if prov == "" {
			prov = "user-authored"
		}
		domain := string(n.Domain)
		if domain == "" {
			domain = "code"
		}
		// R20: compute prev_signature — set to old signature when it changed,
		// '' when the node is new or the signature is unchanged.
		prevSig := ""
		if oldSig, existed := oldSigs[string(n.ID)]; existed && oldSig != sig {
			prevSig = oldSig
		}

		if _, err := nodeStmt.Exec(
			string(n.ID), string(n.Type),
			n.Name, n.Package, n.File, n.Line,
			exported, string(meta), doc, sig, lineCount, n.StableID, prov, domain, prevSig,
		); err != nil {
			return fmt.Errorf("insert node %s: %w", n.ID, err)
		}
	}

	// Rebuild FTS index inside the same transaction so search is always consistent
	// with the nodes table.
	if _, err := tx.Exec(`DELETE FROM nodes_fts`); err != nil {
		return fmt.Errorf("clear fts: %w", err)
	}
	ftsStmt, err := tx.Prepare(`INSERT INTO nodes_fts (node_id, name, split_name, signature, doc) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare fts stmt: %w", err)
	}
	defer ftsStmt.Close()
	for _, n := range g.AllNodes() {
		if !strings.HasPrefix(string(n.ID), primaryPrefix) {
			continue
		}
		if n.Type == graph.NodeFile || n.Type == graph.NodePackage {
			continue // not useful for search
		}
		doc := n.Metadata["doc"]
		sig := n.Metadata["signature"]
		if _, err := ftsStmt.Exec(string(n.ID), n.Name, splitCamelCase(n.Name), sig, doc); err != nil {
			return fmt.Errorf("insert fts node %s: %w", n.ID, err)
		}
	}

	// Insert edges.
	edgeStmt, err := tx.Prepare(`
        INSERT OR IGNORE INTO edges (from_id, to_id, type)
        VALUES (?, ?, ?)
    `)
	if err != nil {
		return fmt.Errorf("prepare edge stmt: %w", err)
	}
	defer edgeStmt.Close()

	for _, e := range g.AllEdges() {
		// Skip edges originating from linked-project nodes.
		if !strings.HasPrefix(string(e.From), primaryPrefix) {
			continue
		}
		if _, err := edgeStmt.Exec(string(e.From), string(e.To), string(e.Type)); err != nil {
			return fmt.Errorf("insert edge %s→%s: %w", e.From, e.To, err)
		}
	}

	// Persist metadata (lightweight stats so 'list' never needs to load the full graph).
	now := time.Now().UTC().Format(time.RFC3339)
	metaRows := [][2]string{
		{"repo_id", g.RepoID()},
		{"repo_root", g.Root()},
		{"saved_at", now},
		{"node_count", strconv.Itoa(g.NodeCount())},
		{"edge_count", strconv.Itoa(g.EdgeCount())},
		{"file_count", strconv.Itoa(fileCount)},
	}
	for _, kv := range metaRows {
		if _, err := tx.Exec(
			`INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`, kv[0], kv[1],
		); err != nil {
			return fmt.Errorf("save meta %s: %w", kv[0], err)
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// GAP-3: After the new graph is committed, compute new fan-in and mark
	// annotations stale where the call structure changed by >20% or node removed.
	go func() {
		newFanIn := make(map[string]int)
		if fanRows, err := s.db.Query(`SELECT to_id, COUNT(*) FROM edges WHERE type='CALLS' GROUP BY to_id`); err == nil {
			defer fanRows.Close()
			for fanRows.Next() {
				var nid string
				var cnt int
				if fanRows.Scan(&nid, &cnt) == nil {
					newFanIn[nid] = cnt
				}
			}
		}
		var staleIDs []string
		const threshold = 0.20
		for nid, oldCnt := range oldFanIn {
			if oldCnt == 0 {
				continue
			}
			newCnt, exists := newFanIn[nid]
			if !exists {
				// Node removed entirely — its annotations are definitely stale.
				staleIDs = append(staleIDs, nid)
				continue
			}
			delta := float64(newCnt-oldCnt)
			if delta < 0 {
				delta = -delta
			}
			if delta/float64(oldCnt) > threshold {
				staleIDs = append(staleIDs, nid)
			}
		}
		if len(staleIDs) > 0 {
			_ = s.MarkAnnotationsStale(staleIDs)
			// AM-2: cascade stale flag to any memories anchored to these structurally-changed nodes.
			// Gap-4 fix: also stale entity-tier memories written with entity_id but no anchor_nodes.
			// Both calls are fire-and-forget — failures are non-fatal; stale memories will be
			// re-detected on next session via AM-3.
			_ = s.MarkAnchoredMemoriesStale(staleIDs, "anchor node structural change (fanin delta >20%)")
			_ = s.MarkEntityMemoriesStaleForNodes(staleIDs, "entity node structural change (fanin delta >20%)")
		}
	}()

	return nil
}

// LoadGraph reads the persisted graph from the store and returns it.
// Returns (nil, nil) if the store is empty (first run).
func (s *Store) LoadGraph() (*graph.Graph, error) {
	// Read lightweight meta (repo_id, repo_root) in one pass.
	metaRows, err := s.db.Query(`SELECT key, value FROM meta WHERE key IN ('repo_id', 'repo_root')`)
	if err != nil {
		return nil, fmt.Errorf("read meta: %w", err)
	}
	kv := make(map[string]string, 2)
	for metaRows.Next() {
		var k, v string
		if err := metaRows.Scan(&k, &v); err != nil {
			metaRows.Close()
			return nil, err
		}
		kv[k] = v
	}
	metaRows.Close()
	if err := metaRows.Err(); err != nil {
		return nil, err
	}

	repoID := kv["repo_id"]
	if repoID == "" {
		return nil, nil // nothing persisted yet
	}

	g := graph.New(repoID)
	if root := kv["repo_root"]; root != "" {
		g.SetRoot(root)
	}

	// Load nodes.
	rows, err := s.db.Query(`
        SELECT id, type, name, package, file, line, exported, metadata, doc, signature, line_count, stable_id, provenance, domain FROM nodes
    `)
	if err != nil {
		return nil, fmt.Errorf("query nodes: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id, typ, name, pkg, file string
			line, exported           int
			metaJSON, doc, sig       string
			lineCount                int
			stableID                 string
			provenance               string
			domain                   string
		)
		if err := rows.Scan(&id, &typ, &name, &pkg, &file, &line, &exported, &metaJSON, &doc, &sig, &lineCount, &stableID, &provenance, &domain); err != nil {
			return nil, fmt.Errorf("scan node: %w", err)
		}
		var meta map[string]string
		_ = json.Unmarshal([]byte(metaJSON), &meta)
		if meta == nil {
			meta = make(map[string]string)
		}
		// Restore promoted fields back into the metadata map.
		if doc != "" {
			meta["doc"] = doc
		}
		if sig != "" {
			meta["signature"] = sig
		}
		if lineCount > 0 {
			meta["line_count"] = strconv.Itoa(lineCount)
		}

		g.AddNode(&graph.Node{
			ID:         graph.NodeID(id),
			Type:       graph.NodeType(typ),
			Name:       name,
			Package:    pkg,
			File:       file,
			Line:       line,
			Exported:   exported != 0,
			Metadata:   meta,
			StableID:   stableID,
			Provenance: graph.ProvenanceType(provenance),
			Domain:     graph.DomainType(domain),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate nodes: %w", err)
	}

	// Load edges.
	erows, err := s.db.Query(`SELECT from_id, to_id, type FROM edges`)
	if err != nil {
		return nil, fmt.Errorf("query edges: %w", err)
	}
	defer erows.Close()

	for erows.Next() {
		var fromID, toID, typ string
		if err := erows.Scan(&fromID, &toID, &typ); err != nil {
			return nil, fmt.Errorf("scan edge: %w", err)
		}
		g.AddEdge(&graph.Edge{
			From: graph.NodeID(fromID),
			To:   graph.NodeID(toID),
			Type: graph.EdgeType(typ),
		})
	}
	if err := erows.Err(); err != nil {
		return nil, fmt.Errorf("iterate edges: %w", err)
	}

	return g, nil
}

// SignatureChange records an exported entity whose signature changed in the last SaveGraph.
type SignatureChange struct {
	NodeID    string // graph node ID
	Name      string
	NodeType  string
	File      string
	Line      int
	OldSig    string // signature before the last SaveGraph
	NewSig    string // current signature
}

// GetSignatureChanges returns exported entities in the given file whose signature
// changed during the last SaveGraph call. The file argument is matched as a suffix
// (same semantics as Graph.FindByFile) so callers may pass either a relative or an
// absolute path. Returns an empty slice — not an error — when nothing changed.
func (s *Store) GetSignatureChanges(file string) ([]SignatureChange, error) {
	// Use suffix matching: '%' || file handles both absolute and relative paths.
	rows, err := s.db.Query(`
		SELECT id, name, type, file, line, signature, prev_signature
		FROM nodes
		WHERE (file = ? OR file LIKE '%' || ?)
		  AND prev_signature != ''
		  AND type IN ('function', 'method', 'struct', 'interface')
	`, file, file)
	if err != nil {
		return nil, fmt.Errorf("store.GetSignatureChanges: %w", err)
	}
	defer rows.Close()

	var changes []SignatureChange
	for rows.Next() {
		var c SignatureChange
		if err := rows.Scan(&c.NodeID, &c.Name, &c.NodeType, &c.File, &c.Line, &c.NewSig, &c.OldSig); err != nil {
			return nil, fmt.Errorf("store.GetSignatureChanges scan: %w", err)
		}
		changes = append(changes, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.GetSignatureChanges iterate: %w", err)
	}
	return changes, nil
}

// SavedAt returns the timestamp of the last SaveGraph call, or zero if absent.
func (s *Store) SavedAt() (time.Time, error) {
	var raw string
	row := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'saved_at'`)
	if err := row.Scan(&raw); err == sql.ErrNoRows {
		return time.Time{}, nil
	} else if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339, raw)
}

// Stat reads only the meta key-value table and returns a ProjectStat without
// loading any nodes or edges. This is the fast path used by 'synapses list'.
func (s *Store) Stat(dbPath string) (*ProjectStat, error) {
	rows, err := s.db.Query(`SELECT key, value FROM meta`)
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
		return nil, nil // empty / uninitialised store
	}

	stat := &ProjectStat{
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

// ScanAll discovers every project that has been indexed by scanning the
// synapses cache directory for *.db files and reading their meta tables.
// Results are sorted by SavedAt descending (most recent first).
func ScanAll() ([]ProjectStat, error) {
	dir, err := CacheDir()
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // nothing indexed yet
		}
		return nil, fmt.Errorf("read cache dir: %w", err)
	}

	var stats []ProjectStat
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".db" {
			continue
		}
		dbPath := filepath.Join(dir, e.Name())
		st, err := Open(dbPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "synapses: skipping corrupt db %s: %v\n", e.Name(), err)
			continue
		}
		stat, err := st.Stat(dbPath)
		st.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "synapses: skipping corrupt db %s: %v\n", e.Name(), err)
			continue
		}
		if stat == nil {
			continue // empty / uninitialised DB — not an error
		}
		stats = append(stats, *stat)
	}

	sort.Slice(stats, func(i, j int) bool {
		return stats[i].SavedAt.After(stats[j].SavedAt)
	})
	return stats, nil
}

// LoadFileMtimes returns the stored path→mtime (UnixNano) map from the last
// successful index. Returns an empty map (not nil) if no data is stored yet.
func (s *Store) LoadFileMtimes() (map[string]int64, error) {
	rows, err := s.db.Query(`SELECT path, mod_time FROM file_hashes`)
	if err != nil {
		return nil, fmt.Errorf("query file_hashes: %w", err)
	}
	defer rows.Close()

	m := make(map[string]int64)
	for rows.Next() {
		var path string
		var mtime int64
		if err := rows.Scan(&path, &mtime); err != nil {
			return nil, fmt.Errorf("scan file_hashes: %w", err)
		}
		m[path] = mtime
	}
	return m, rows.Err()
}

// SaveFileMtimes replaces the stored file-mtime table with the provided map.
// m maps absolute file path → mtime in UnixNano.
func (s *Store) SaveFileMtimes(m map[string]int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.Exec(`DELETE FROM file_hashes`); err != nil {
		return fmt.Errorf("clear file_hashes: %w", err)
	}

	stmt, err := tx.Prepare(`INSERT INTO file_hashes (path, mod_time) VALUES (?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare file_hashes stmt: %w", err)
	}
	defer stmt.Close()

	for path, mtime := range m {
		if _, err := stmt.Exec(path, mtime); err != nil {
			return fmt.Errorf("insert file_hash %s: %w", path, err)
		}
	}
	return tx.Commit()
}

// UpsertFileMtime updates (or inserts) the stored mtime for a single file.
// Used by the watcher after a hot-reload to keep file_hashes current without
// rewriting the entire table (which would require loading all other entries).
func (s *Store) UpsertFileMtime(path string, mtime int64) error {
	_, err := s.db.Exec(
		`INSERT INTO file_hashes (path, mod_time) VALUES (?, ?)
         ON CONFLICT(path) DO UPDATE SET mod_time = excluded.mod_time`,
		path, mtime,
	)
	return err
}

// SaveCallSites replaces the persisted call-site table with sites.
// Called after every full parse so cross-project CALLS can be re-resolved
// on subsequent starts once linked project graphs have been merged.
func (s *Store) SaveCallSites(sites []graph.CallSite) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.Exec(`DELETE FROM call_sites`); err != nil {
		return fmt.Errorf("clear call_sites: %w", err)
	}

	stmt, err := tx.Prepare(`INSERT INTO call_sites (caller_id, caller_file, pkg_alias, func_name) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare call_sites stmt: %w", err)
	}
	defer stmt.Close()

	for _, cs := range sites {
		if _, err := stmt.Exec(string(cs.CallerID), cs.CallerFile, cs.PkgAlias, cs.FuncName); err != nil {
			return fmt.Errorf("insert call_site: %w", err)
		}
	}
	return tx.Commit()
}

// LoadCallSites returns all persisted call sites from the last full index.
// Returns nil (not an error) if the table is empty.
func (s *Store) LoadCallSites() ([]graph.CallSite, error) {
	rows, err := s.db.Query(`SELECT caller_id, caller_file, pkg_alias, func_name FROM call_sites`)
	if err != nil {
		return nil, fmt.Errorf("query call_sites: %w", err)
	}
	defer rows.Close()

	var sites []graph.CallSite
	for rows.Next() {
		var cs graph.CallSite
		var callerID string
		if err := rows.Scan(&callerID, &cs.CallerFile, &cs.PkgAlias, &cs.FuncName); err != nil {
			return nil, fmt.Errorf("scan call_site: %w", err)
		}
		cs.CallerID = graph.NodeID(callerID)
		sites = append(sites, cs)
	}
	return sites, rows.Err()
}

// UpdateCallSitesForFile atomically replaces the persisted call sites whose
// caller_file matches file with newSites. This is used by the watcher after
// an incremental re-parse so the stored call-site table stays consistent with
// the live graph without a full table replacement.
func (s *Store) UpdateCallSitesForFile(file string, newSites []graph.CallSite) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.Exec(`DELETE FROM call_sites WHERE caller_file = ?`, file); err != nil {
		return fmt.Errorf("delete call_sites for %s: %w", file, err)
	}

	if len(newSites) > 0 {
		stmt, err := tx.Prepare(`INSERT INTO call_sites (caller_id, caller_file, pkg_alias, func_name) VALUES (?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("prepare call_sites stmt: %w", err)
		}
		defer stmt.Close()

		for _, cs := range newSites {
			if _, err := stmt.Exec(string(cs.CallerID), cs.CallerFile, cs.PkgAlias, cs.FuncName); err != nil {
				return fmt.Errorf("insert call_site: %w", err)
			}
		}
	}
	return tx.Commit()
}

// UpsertDynamicRule persists a dynamic architectural rule to the store.
// If a rule with the same ID already exists it is fully replaced; otherwise
// a new row is inserted. The rule takes effect in-memory immediately — see
// Server.handleUpsertRule for the in-memory update that accompanies this call.
func (s *Store) UpsertDynamicRule(r config.Rule) error {
	now := time.Now().UTC().Format(time.RFC3339)
	ruleType := r.RuleType
	if ruleType == "" {
		ruleType = "structural"
	}
	_, err := s.db.Exec(`
        INSERT INTO dynamic_rules
            (id, description, severity, from_file_pattern, to_file_pattern,
             from_type, to_type, edge_type, to_name_pattern, rule_type, created_at, updated_at)
        VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
        ON CONFLICT(id) DO UPDATE SET
            description=excluded.description, severity=excluded.severity,
            from_file_pattern=excluded.from_file_pattern,
            to_file_pattern=excluded.to_file_pattern,
            from_type=excluded.from_type, to_type=excluded.to_type,
            edge_type=excluded.edge_type,
            to_name_pattern=excluded.to_name_pattern,
            rule_type=excluded.rule_type,
            updated_at=excluded.updated_at`,
		r.ID, r.Description, r.Severity,
		r.ForbiddenEdge.FromFilePattern, r.ForbiddenEdge.ToFilePattern,
		string(r.ForbiddenEdge.FromType), string(r.ForbiddenEdge.ToType),
		string(r.ForbiddenEdge.EdgeType), r.ForbiddenEdge.ToNamePattern,
		ruleType, now, now,
	)
	return err
}

// LoadDynamicRules returns all dynamic rules persisted in the store, ordered
// by creation time. Called at server startup to restore rules from previous
// sessions so agents don't need to re-declare them after a restart.
func (s *Store) LoadDynamicRules() ([]config.Rule, error) {
	rows, err := s.db.Query(`
        SELECT id, description, severity,
            from_file_pattern, to_file_pattern, from_type, to_type,
            edge_type, to_name_pattern, rule_type
        FROM dynamic_rules
        ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("query dynamic_rules: %w", err)
	}
	defer rows.Close()

	var rules []config.Rule
	for rows.Next() {
		var r config.Rule
		var fromType, toType, edgeType string
		if err := rows.Scan(
			&r.ID, &r.Description, &r.Severity,
			&r.ForbiddenEdge.FromFilePattern, &r.ForbiddenEdge.ToFilePattern,
			&fromType, &toType, &edgeType,
			&r.ForbiddenEdge.ToNamePattern, &r.RuleType,
		); err != nil {
			return nil, fmt.Errorf("scan dynamic_rule: %w", err)
		}
		r.ForbiddenEdge.FromType = graph.NodeType(fromType)
		r.ForbiddenEdge.ToType = graph.NodeType(toType)
		r.ForbiddenEdge.EdgeType = graph.EdgeType(edgeType)
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

// hashPath produces a short, filesystem-safe hash of a path string.
// This is not cryptographic — it exists solely to avoid filename collisions.
func hashPath(path string) string {
	h := uint32(2166136261) // FNV-1a
	for _, c := range []byte(path) {
		h ^= uint32(c)
		h *= 16777619
	}
	return fmt.Sprintf("%08x", h)
}

// ViolationLogEntry is a single entry in the violation audit log.
type ViolationLogEntry struct {
	ID          string `json:"id"`
	RuleID      string `json:"rule_id"`
	Severity    string `json:"severity"`
	FromNode    string `json:"from_node"`
	ToNode      string `json:"to_node"`
	EdgeType    string `json:"edge_type"`
	FirstSeen   string `json:"first_seen"`
	LastSeen    string `json:"last_seen"`
	Occurrences int    `json:"occurrences"`
}

// ViolationID returns a stable SHA-1-derived ID for a violation so that
// re-detecting the same violation updates the existing row rather than
// inserting a duplicate. Exported so callers (e.g. the watcher) can compute
// IDs to compare against ViolationIDsForFile results.
func ViolationID(ruleID, fromNode, toNode, edgeType string) string {
	h := sha1.Sum([]byte(ruleID + "\x00" + fromNode + "\x00" + toNode + "\x00" + edgeType))
	return fmt.Sprintf("%x", h[:8]) // 16 hex chars — compact and collision-resistant
}

// violationID is the package-internal alias kept for backward compatibility.
func violationID(ruleID, fromNode, toNode, edgeType string) string {
	return ViolationID(ruleID, fromNode, toNode, edgeType)
}

// LogViolations upserts a batch of violations into the audit log.
// Re-detecting the same violation (same rule+from+to+edge) updates last_seen
// and increments occurrences instead of creating a duplicate row.
func (s *Store) LogViolations(vs []config.Violation) error {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(`
        INSERT INTO violation_log (id, rule_id, severity, from_node, to_node, edge_type, first_seen, last_seen, occurrences)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1)
        ON CONFLICT(id) DO UPDATE SET
            last_seen   = excluded.last_seen,
            occurrences = occurrences + 1
    `)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, v := range vs {
		id := violationID(v.RuleID, string(v.FromNode), string(v.ToNode), string(v.EdgeType))
		if _, err := stmt.Exec(id, v.RuleID, v.Severity,
			string(v.FromNode), string(v.ToNode), string(v.EdgeType),
			now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ViolationIDsForFile returns the set of stable violation IDs already recorded
// in the log whose from_node or to_node contains the given file path as a
// substring. Used by the watcher to distinguish newly-detected violations
// (which should trigger an event) from pre-existing ones (which should not).
func (s *Store) ViolationIDsForFile(file string) (map[string]struct{}, error) {
	pattern := "%" + file + "%"
	rows, err := s.db.Query(
		`SELECT id FROM violation_log WHERE from_node LIKE ? OR to_node LIKE ?`,
		pattern, pattern,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = struct{}{}
	}
	return ids, rows.Err()
}

// GetViolationLog returns up to limit entries from the violation audit log,
// ordered by last_seen descending (most recent first).
// If ruleID is non-empty, only violations for that rule are returned.
func (s *Store) GetViolationLog(ruleID string, limit int) ([]ViolationLogEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	var rows *sql.Rows
	var err error
	if ruleID != "" {
		rows, err = s.db.Query(`
            SELECT id, rule_id, severity, from_node, to_node, edge_type, first_seen, last_seen, occurrences
            FROM violation_log WHERE rule_id = ? ORDER BY last_seen DESC LIMIT ?`,
			ruleID, limit)
	} else {
		rows, err = s.db.Query(`
            SELECT id, rule_id, severity, from_node, to_node, edge_type, first_seen, last_seen, occurrences
            FROM violation_log ORDER BY last_seen DESC LIMIT ?`,
			limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []ViolationLogEntry
	for rows.Next() {
		var e ViolationLogEntry
		if err := rows.Scan(&e.ID, &e.RuleID, &e.Severity,
			&e.FromNode, &e.ToNode, &e.EdgeType,
			&e.FirstSeen, &e.LastSeen, &e.Occurrences); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// ── Quality Intelligence (R32) ────────────────────────────────────────────────

// QualityGap is an agent-discovered quality finding on a specific code entity.
// Unlike architecture violations (deterministic rule checks), quality gaps are
// asserted through reasoning — "I examined this function and found this edge case."
type QualityGap struct {
	ID          string `json:"id"`          // "{node_id}:{gap_id}"
	NodeID      string `json:"node_id"`
	GapID       string `json:"gap_id"`      // slug: "dist-relative-path"
	Description string `json:"description"`
	Severity    string `json:"severity"`    // low | medium | high | critical
	Status      string `json:"status"`      // open | fixed | wontfix
	FoundBy     string `json:"found_by,omitempty"`
	FoundAt     string `json:"found_at"`
	UpdatedAt   string `json:"updated_at"`
	FixNotes    string `json:"fix_notes,omitempty"`
}

// GapFilter controls which quality gaps are returned by GetGaps.
type GapFilter struct {
	NodeID   string // filter by exact node ID
	File     string // filter by source file (matches any node in that file)
	Severity string // filter by severity ("low" | "medium" | "high" | "critical")
	Status   string // filter by status; default "open" when empty
}

// UpsertGap creates or updates a quality gap. The primary key is
// "{nodeID}:{gapID}" — re-calling with the same pair updates the record.
func (s *Store) UpsertGap(g QualityGap) (QualityGap, error) {
	if g.NodeID == "" || g.GapID == "" {
		return QualityGap{}, fmt.Errorf("node_id and gap_id are required")
	}
	g.ID = g.NodeID + ":" + g.GapID
	if g.Severity == "" {
		g.Severity = "medium"
	}
	if g.Status == "" {
		g.Status = "open"
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if g.FoundAt == "" {
		g.FoundAt = now
	}
	g.UpdatedAt = now
	_, err := s.db.Exec(`
		INSERT INTO quality_gaps
			(id, node_id, gap_id, description, severity, status, found_by, found_at, updated_at, fix_notes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			description = excluded.description,
			severity    = excluded.severity,
			status      = excluded.status,
			updated_at  = excluded.updated_at,
			fix_notes   = excluded.fix_notes`,
		g.ID, g.NodeID, g.GapID, g.Description, g.Severity, g.Status,
		g.FoundBy, g.FoundAt, g.UpdatedAt, g.FixNotes,
	)
	if err != nil {
		return QualityGap{}, fmt.Errorf("upsert quality gap: %w", err)
	}
	return g, nil
}

// GetGaps returns quality gaps matching the filter. When filter.Status is
// empty it defaults to "open". Pass status="all" to return every status.
func (s *Store) GetGaps(f GapFilter) ([]QualityGap, error) {
	status := f.Status
	if status == "" {
		status = "open"
	}

	var (
		rows *sql.Rows
		err  error
	)
	base := `SELECT id, node_id, gap_id, description, severity, status,
	                found_by, found_at, updated_at, fix_notes
	         FROM quality_gaps`

	// severityOrder sorts critical→high→medium→low semantically.
	// TEXT columns sort lexicographically in SQLite, so ORDER BY severity DESC
	// would yield medium→low→high→critical (wrong). The CASE expression maps
	// each value to an integer weight so DESC gives the intended order.
	const severityOrder = ` CASE severity WHEN 'critical' THEN 4 WHEN 'high' THEN 3 WHEN 'medium' THEN 2 WHEN 'low' THEN 1 ELSE 0 END DESC, updated_at DESC`

	// filePattern builds two LIKE patterns to match node_id by file basename.
	// node_id format: "{repoID}::{filePath}::{name}"
	//   - "%/auth.go::%" matches  "repo::pkg/auth.go::Func" (path component)
	//   - "%::auth.go::%" matches "repo::auth.go::Func"     (root-level file)
	// Using two patterns with OR avoids the substring false-positive of the old
	// single "%auth.go%" pattern (which matched "unauth.go" as well).
	fileWhere := func(extra string) string {
		return base + ` WHERE (node_id LIKE ? OR node_id LIKE ?)` + extra + ` ORDER BY` + severityOrder
	}

	switch {
	// Compound: NodeID + Severity (most specific — must come before single-field cases).
	case f.NodeID != "" && f.Severity != "" && status != "all":
		rows, err = s.db.Query(base+` WHERE node_id = ? AND severity = ? AND status = ? ORDER BY`+severityOrder, f.NodeID, f.Severity, status)
	case f.NodeID != "" && f.Severity != "":
		rows, err = s.db.Query(base+` WHERE node_id = ? AND severity = ? ORDER BY`+severityOrder, f.NodeID, f.Severity)
	// Compound: File + Severity.
	case f.File != "" && f.Severity != "" && status != "all":
		rows, err = s.db.Query(fileWhere(` AND severity = ? AND status = ?`), "%/"+f.File+"::%", "%::"+f.File+"::%", f.Severity, status)
	case f.File != "" && f.Severity != "":
		rows, err = s.db.Query(fileWhere(` AND severity = ?`), "%/"+f.File+"::%", "%::"+f.File+"::%", f.Severity)
	// Single-field cases.
	case f.NodeID != "" && status != "all":
		rows, err = s.db.Query(base+` WHERE node_id = ? AND status = ? ORDER BY`+severityOrder, f.NodeID, status)
	case f.NodeID != "":
		rows, err = s.db.Query(base+` WHERE node_id = ? ORDER BY`+severityOrder, f.NodeID)
	case f.File != "" && status != "all":
		rows, err = s.db.Query(fileWhere(` AND status = ?`), "%/"+f.File+"::%", "%::"+f.File+"::%", status)
	case f.File != "":
		rows, err = s.db.Query(fileWhere(``), "%/"+f.File+"::%", "%::"+f.File+"::%")
	case f.Severity != "" && status != "all":
		rows, err = s.db.Query(base+` WHERE severity = ? AND status = ? ORDER BY`+severityOrder, f.Severity, status)
	case status != "all":
		rows, err = s.db.Query(base+` WHERE status = ? ORDER BY`+severityOrder, status)
	default:
		rows, err = s.db.Query(base + ` ORDER BY` + severityOrder)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QualityGap
	for rows.Next() {
		var g QualityGap
		if err := rows.Scan(&g.ID, &g.NodeID, &g.GapID, &g.Description,
			&g.Severity, &g.Status, &g.FoundBy, &g.FoundAt, &g.UpdatedAt, &g.FixNotes); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// CountOpenGaps returns the number of open quality gaps matching the given
// node IDs. Used by session_init to surface gap counts for recently-worked files.
func (s *Store) CountOpenGaps(nodeIDs []string) (int, error) {
	if len(nodeIDs) == 0 {
		return 0, nil
	}
	placeholders := make([]string, len(nodeIDs))
	args := make([]interface{}, len(nodeIDs))
	for i, id := range nodeIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	row := s.db.QueryRow(
		`SELECT COUNT(*) FROM quality_gaps WHERE status = 'open' AND node_id IN (`+
			strings.Join(placeholders, ",")+`)`,
		args...)
	var n int
	return n, row.Scan(&n)
}

// Agent is a registered agent that has interacted with Synapses.
type Agent struct {
	ID               string `json:"id"`
	LastSeen         string `json:"last_seen"`
	Metadata         string `json:"metadata"`
	CurrentTaskID    string `json:"current_task_id,omitempty"`
	CurrentTaskTitle string `json:"current_task_title,omitempty"`
	CurrentFocus     string `json:"current_focus,omitempty"`
	// B29: richer focus fields.
	CurrentFocusFile  string `json:"current_focus_file,omitempty"`
	CurrentFocusSince string `json:"current_focus_since,omitempty"`
	Intent            string `json:"intent,omitempty"`
	// ProjectID is non-empty only for remote agents synced from federated peers.
	// Local agents always have ProjectID = "".
	ProjectID string `json:"project_id,omitempty"`
	// Presence is computed from LastSeen: active (≤5min), idle (5–15min), inactive (>15min).
	Presence string `json:"presence,omitempty"`
}

// AgentActivity carries optional activity fields for UpsertAgent.
// Only non-empty fields overwrite existing values (partial update semantics).
type AgentActivity struct {
	TaskID    string
	TaskTitle string
	Focus     string
	// B29: richer focus fields.
	FocusFile  string
	FocusSince string // RFC3339; DB only applies this when Focus changes entity name.
	Intent     string
}

// AgentSummary is the compact view of a peer agent returned in session_init's
// agent_awareness section. Includes active claims so the caller can avoid conflicts.
type AgentSummary struct {
	ID       string   `json:"id"`
	Presence string   `json:"presence"`
	Project  string   `json:"project,omitempty"` // non-empty for remote (federated) agents
	Task     string   `json:"task,omitempty"`
	Focus    string   `json:"focus,omitempty"`
	// B29: richer focus fields surfaced to peers.
	FocusFile    string `json:"focus_file,omitempty"`
	FocusAgeSecs int    `json:"focus_age_seconds,omitempty"` // how long on current entity
	Intent       string `json:"intent,omitempty"`
	Scopes       []string `json:"scopes,omitempty"` // for local agents: from work_claims; for remote: from peer sync
}

// DependencyAlert is emitted when a peer agent recently changed a file containing
// a symbol the calling agent was actively examining (via get_context).
type DependencyAlert struct {
	PeerAgentID string `json:"peer_agent_id"`
	EntityName  string `json:"entity_name"`
	EntityFile  string `json:"entity_file"`
	ChangedAt   string `json:"changed_at"` // RFC3339
}

// classifyPresence derives active/idle/inactive from a RFC3339 last_seen timestamp.
func classifyPresence(lastSeen string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, lastSeen)
	if err != nil {
		return "inactive"
	}
	switch elapsed := now.Sub(t); {
	case elapsed <= 8*time.Minute:
		return "active"
	case elapsed <= 15*time.Minute:
		return "idle"
	default:
		return "inactive"
	}
}

// AgentContext tracks what an agent already knows so session_init can deliver
// incremental updates instead of repeating the full project identity every time.
type AgentContext struct {
	AgentID      string `json:"agent_id"`
	LastEventSeq int64  `json:"last_event_seq"` // last event seq the agent received
	IdentityHash string `json:"identity_hash"`  // SHA-1 of last ProjectIdentity sent
	LastSession  string `json:"last_session"`   // RFC3339 timestamp of last session_init
	TaskSeq      int64  `json:"task_seq"`       // sequence marker for task change detection
}

// UpsertAgent records that an agent was seen. activity is optional — when nil,
// only last_seen is touched (fast path). When non-nil, non-empty fields replace
// existing values; empty fields leave existing values untouched.
func (s *Store) UpsertAgent(id string, activity *AgentActivity) error {
	now := time.Now().UTC().Format(time.RFC3339)
	if activity == nil {
		_, err := s.db.Exec(`
			INSERT INTO agents (id, last_seen) VALUES (?, ?)
			ON CONFLICT(id) DO UPDATE SET last_seen = excluded.last_seen`,
			id, now,
		)
		return err
	}
	_, err := s.db.Exec(`
		INSERT INTO agents (id, last_seen, current_task_id, current_task_title, current_focus, focus_file, focus_since, intent)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			last_seen          = excluded.last_seen,
			current_task_id    = CASE WHEN excluded.current_task_id    != '' THEN excluded.current_task_id    ELSE agents.current_task_id    END,
			current_task_title = CASE WHEN excluded.current_task_title != '' THEN excluded.current_task_title ELSE agents.current_task_title END,
			current_focus      = CASE WHEN excluded.current_focus      != '' THEN excluded.current_focus      ELSE agents.current_focus      END,
			focus_file         = CASE WHEN excluded.focus_file         != '' THEN excluded.focus_file         ELSE agents.focus_file         END,
			focus_since        = CASE
				WHEN excluded.current_focus != '' AND excluded.current_focus != agents.current_focus
				THEN excluded.focus_since
				ELSE agents.focus_since
			END,
			intent             = CASE WHEN excluded.intent != '' THEN excluded.intent ELSE agents.intent END`,
		id, now,
		activity.TaskID, activity.TaskTitle, activity.Focus,
		activity.FocusFile, activity.FocusSince, activity.Intent,
	)
	return err
}

// ClearAgentTask zeroes the current task fields for the given agent.
// Call when a task transitions to done/cancelled.
func (s *Store) ClearAgentTask(agentID string) error {
	_, err := s.db.Exec(
		`UPDATE agents SET current_task_id = '', current_task_title = '' WHERE id = ?`,
		agentID,
	)
	return err
}

// ClearAgentScope zeroes the current_scope field for the given agent.
// Kept for compatibility; the column is not surfaced in API responses —
// use work_claims (local) or metadata (remote) for scope visibility.
func (s *Store) ClearAgentScope(agentID string) error {
	_, err := s.db.Exec(
		`UPDATE agents SET current_scope = '' WHERE id = ?`,
		agentID,
	)
	return err
}

// UpsertRemoteAgent registers or refreshes an agent synced from a federated peer.
// projectID is the peer's repo name (e.g. "backend-repo"). scopes are the peer's
// active work claims at the time of sync — stored in metadata as JSON for display.
// Remote agents age out naturally via the same presence thresholds as local agents.
func (s *Store) UpsertRemoteAgent(id, projectID string, activity *AgentActivity, scopes []string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	scopesJSON := "[]"
	if len(scopes) > 0 {
		if b, err := json.Marshal(scopes); err == nil {
			scopesJSON = string(b)
		}
	}
	// Store scopes in metadata JSON: {"scopes":["internal/auth","cmd/server"]}
	meta := `{"scopes":` + scopesJSON + `}`
	task, focus := "", ""
	if activity != nil {
		task = activity.TaskTitle
		focus = activity.Focus
	}
	_, err := s.db.Exec(`
		INSERT INTO agents (id, last_seen, metadata, current_task_title, current_focus, project_id)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			last_seen          = excluded.last_seen,
			metadata           = excluded.metadata,
			current_task_title = excluded.current_task_title,
			current_focus      = excluded.current_focus,
			project_id         = excluded.project_id`,
		id, now, meta, task, focus, projectID,
	)
	return err
}

// GetAgents returns all known agents ordered by last_seen descending.
// Presence is computed from last_seen: active (≤5min), idle (5–15min), inactive (>15min).
func (s *Store) GetAgents() ([]Agent, error) {
	rows, err := s.db.Query(`
		SELECT id, last_seen, metadata,
		       current_task_id, current_task_title, current_focus, project_id,
		       focus_file, focus_since, intent
		FROM agents ORDER BY last_seen DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	now := time.Now().UTC()
	var agents []Agent
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.ID, &a.LastSeen, &a.Metadata,
			&a.CurrentTaskID, &a.CurrentTaskTitle, &a.CurrentFocus, &a.ProjectID,
			&a.CurrentFocusFile, &a.CurrentFocusSince, &a.Intent); err != nil {
			return nil, err
		}
		a.Presence = classifyPresence(a.LastSeen, now)
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

// GetActiveAgents returns a compact summary of agents seen within the last 15
// minutes (active or idle), excluding the caller. Active claims are attached so
// the caller can immediately spot scope conflicts. Capped at 10 peers.
// For local agents scopes come from work_claims; for remote (project_id != "")
// they are read from the metadata JSON stored during peer sync.
func (s *Store) GetActiveAgents(excludeAgentID string) ([]AgentSummary, error) {
	cutoff := time.Now().UTC().Add(-15 * time.Minute).Format(time.RFC3339)
	now := time.Now().UTC()
	expiry := now.Format(time.RFC3339)

	// Include agents seen in the last 15 minutes OR holding a non-expired work
	// claim. The second condition covers LLMs in long thinking loops: if they
	// claimed a scope before going silent, they are still in-flight.
	rows, err := s.db.Query(`
		SELECT id, last_seen, current_task_title, current_focus, project_id, metadata,
		       focus_file, focus_since, intent
		FROM agents
		WHERE (last_seen > ? OR id IN (
			SELECT DISTINCT agent_id FROM work_claims WHERE expires_at > ? AND agent_id != ?
		)) AND id != ?
		ORDER BY last_seen DESC
		LIMIT 10`, cutoff, expiry, excludeAgentID, excludeAgentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var peers []AgentSummary
	for rows.Next() {
		var p AgentSummary
		var lastSeen, meta, focusSince string
		if err := rows.Scan(&p.ID, &lastSeen, &p.Task, &p.Focus, &p.Project, &meta,
			&p.FocusFile, &focusSince, &p.Intent); err != nil {
			return nil, err
		}
		p.Presence = classifyPresence(lastSeen, now)
		// Compute how long the agent has been on its current entity.
		if focusSince != "" {
			if t, err := time.Parse(time.RFC3339, focusSince); err == nil {
				p.FocusAgeSecs = int(now.Sub(t).Seconds())
			}
		}
		// Remote agents carry their scopes in metadata JSON: {"scopes":[...]}.
		if p.Project != "" && meta != "" && meta != "{}" {
			var m struct {
				Scopes []string `json:"scopes"`
			}
			if json.Unmarshal([]byte(meta), &m) == nil && len(m.Scopes) > 0 {
				p.Scopes = m.Scopes
			}
		}
		peers = append(peers, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(peers) == 0 {
		return nil, nil
	}

	// Attach active local claims keyed by agent_id in a single indexed query.
	claimRows, err := s.db.Query(`
		SELECT agent_id, scope FROM work_claims
		WHERE expires_at > ? AND agent_id != ?`,
		expiry, excludeAgentID)
	if err == nil {
		defer claimRows.Close()
		claimMap := make(map[string][]string)
		for claimRows.Next() {
			var aid, scope string
			if claimRows.Scan(&aid, &scope) == nil {
				claimMap[aid] = append(claimMap[aid], scope)
			}
		}
		for i := range peers {
			// Only override scopes for local agents (remote scopes come from metadata).
			if peers[i].Project == "" {
				if scopes, ok := claimMap[peers[i].ID]; ok {
					peers[i].Scopes = scopes
					// Claim-based presence override: an agent holding a live claim is
					// provably in-flight regardless of how long ago it last called a tool.
					// This covers LLMs in long thinking loops between tool calls.
					if peers[i].Presence != "active" {
						peers[i].Presence = "active"
					}
				}
			}
		}
	}
	return peers, nil
}

// CountActiveAgents returns the number of agents (excluding agentID) that are
// either seen within the last 15 minutes OR hold a non-expired work claim.
// Cheaper than GetActiveAgents — a single COUNT query with no claims join.
func (s *Store) CountActiveAgents(excludeAgentID string) (int, error) {
	cutoff := time.Now().UTC().Add(-15 * time.Minute).Format(time.RFC3339)
	expiry := time.Now().UTC().Format(time.RFC3339)
	var n int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM agents
		WHERE (last_seen > ? OR id IN (
			SELECT DISTINCT agent_id FROM work_claims WHERE expires_at > ? AND agent_id != ?
		)) AND id != ?`,
		cutoff, expiry, excludeAgentID, excludeAgentID,
	).Scan(&n)
	return n, err
}

// CountIndexedFiles returns the number of files currently tracked in the index.
func (s *Store) CountIndexedFiles() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM file_hashes`).Scan(&n)
	return n, err
}

// ── B29: Watched Symbols ──────────────────────────────────────────────────────

// WatchSymbol records that agentID recently examined the given entity via get_context.
// Subsequent calls refresh watched_at (resetting the 30-minute TTL).
// Old entries for this agent older than 30 minutes are pruned inline.
// Non-fatal: errors are silently discarded to keep the hot path unaffected.
func (s *Store) WatchSymbol(agentID, entityID, entityName, entityFile string) {
	now := time.Now().UTC().Format(time.RFC3339)
	cutoff := time.Now().UTC().Add(-30 * time.Minute).Format(time.RFC3339)
	_, _ = s.db.Exec(`
		INSERT INTO agent_watched_symbols (agent_id, entity_id, entity_name, entity_file, watched_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(agent_id, entity_id) DO UPDATE SET watched_at = excluded.watched_at`,
		agentID, entityID, entityName, entityFile, now,
	)
	_, _ = s.db.Exec(
		`DELETE FROM agent_watched_symbols WHERE agent_id = ? AND watched_at < ?`,
		agentID, cutoff,
	)
}

// GetDependencyAlerts returns alerts for symbols this agent has watched (via get_context)
// that were recently changed in a scope claimed by another agent.
// Returns nil when there are no alerts.
//
// Design note: file_change events are emitted by the file watcher with agent_id=""
// (no agent attribution). Rather than relying on the event's agent_id, we join against
// work_claims to find which peer agent currently holds a claim over the changed file's
// directory. This correctly attributes watcher-detected changes to the agent who declared
// intent to work in that scope — the only reliable source of file-to-agent mapping.
//
// Tier 2 signal: only surfaces when a peer's claimed scope overlaps a file you were examining.
func (s *Store) GetDependencyAlerts(agentID string) ([]DependencyAlert, error) {
	now := time.Now().UTC()
	eventCutoff := now.Add(-60 * time.Minute).Format(time.RFC3339)
	watchCutoff := now.Add(-30 * time.Minute).Format(time.RFC3339)
	claimsExpiry := now.Format(time.RFC3339)
	rows, err := s.db.Query(`
		SELECT wc.agent_id, aws.entity_name, aws.entity_file, e.created_at
		FROM events e
		JOIN agent_watched_symbols aws
		     ON json_extract(e.payload, '$.file') = aws.entity_file
		JOIN work_claims wc
		     ON (aws.entity_file = wc.scope
		         OR aws.entity_file LIKE wc.scope || '/%'
		         OR wc.scope LIKE aws.entity_file || '/%')
		WHERE aws.agent_id  = ?
		  AND wc.agent_id   != ?
		  AND wc.expires_at  > ?
		  AND e.type         = 'file_change'
		  AND e.created_at   > ?
		  AND aws.watched_at > ?
		ORDER BY e.created_at DESC
		LIMIT 20`,
		agentID, agentID, claimsExpiry, eventCutoff, watchCutoff,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	seen := make(map[string]struct{})
	var alerts []DependencyAlert
	for rows.Next() {
		var a DependencyAlert
		if err := rows.Scan(&a.PeerAgentID, &a.EntityName, &a.EntityFile, &a.ChangedAt); err != nil {
			return nil, err
		}
		key := a.PeerAgentID + "|" + a.EntityFile
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		alerts = append(alerts, a)
	}
	return alerts, rows.Err()
}

// ── Agent Context Profile ────────────────────────────────────────────────────

// GetAgentContext retrieves the context profile for the given agent.
// Returns nil if no profile exists yet (first session).
func (s *Store) GetAgentContext(agentID string) (*AgentContext, error) {
	var ac AgentContext
	err := s.db.QueryRow(
		`SELECT agent_id, last_event_seq, identity_hash, last_session, task_seq
		 FROM agent_context WHERE agent_id = ?`, agentID,
	).Scan(&ac.AgentID, &ac.LastEventSeq, &ac.IdentityHash, &ac.LastSession, &ac.TaskSeq)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ac, nil
}

// UpsertAgentContext creates or updates the context profile for an agent.
// Called after session_init to record what the agent has received.
func (s *Store) UpsertAgentContext(ac *AgentContext) error {
	_, err := s.db.Exec(`
		INSERT INTO agent_context (agent_id, last_event_seq, identity_hash, last_session, task_seq)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(agent_id) DO UPDATE SET
			last_event_seq = excluded.last_event_seq,
			identity_hash  = excluded.identity_hash,
			last_session   = excluded.last_session,
			task_seq       = excluded.task_seq`,
		ac.AgentID, ac.LastEventSeq, ac.IdentityHash, ac.LastSession, ac.TaskSeq,
	)
	return err
}

// ── Event Log ────────────────────────────────────────────────────────────────

// Event is a single entry in the pull-based event log.
type Event struct {
	Seq       int64  `json:"seq"`
	Type      string `json:"type"`
	AgentID   string `json:"agent_id,omitempty"`
	Payload   string `json:"payload"`
	CreatedAt string `json:"created_at"`
}

// AppendEvent writes a new event to the log and prunes entries older than 24h.
// Non-fatal: errors are silently ignored by callers to avoid disrupting hot paths.
func (s *Store) AppendEvent(typ, agentID, payload string) error {
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339)
	cutoff := now.Add(-24 * time.Hour).Format(time.RFC3339)

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		`INSERT INTO events (type, agent_id, payload, created_at) VALUES (?, ?, ?, ?)`,
		typ, agentID, payload, nowStr,
	); err != nil {
		return err
	}
	// Prune old events (bounded table).
	if _, err := tx.Exec(`DELETE FROM events WHERE created_at < ?`, cutoff); err != nil {
		return err
	}
	// Prune globally expired watched symbols. WatchSymbol only prunes per-agent inline,
	// so agents who stopped connecting would leave orphaned rows forever without this.
	watchCutoff := now.Add(-30 * time.Minute).Format(time.RFC3339)
	if _, err := tx.Exec(`DELETE FROM agent_watched_symbols WHERE watched_at < ?`, watchCutoff); err != nil {
		return err
	}
	return tx.Commit()
}

// GetEvents returns up to limit events with seq > sinceSeq, optionally filtered
// by event type and/or agent ID. Returns the latest seq seen so the caller can
// use it as a cursor. Pass agentIDFilter="" to disable agent filtering.
func (s *Store) GetEvents(sinceSeq int64, types []string, agentIDFilter string, limit int) ([]Event, int64, error) {
	if limit <= 0 {
		limit = 100
	}

	query := `SELECT seq, type, agent_id, payload, created_at FROM events WHERE seq > ?`
	args := []interface{}{sinceSeq}

	if len(types) > 0 {
		placeholders := strings.Repeat("?,", len(types))
		placeholders = placeholders[:len(placeholders)-1]
		query += ` AND type IN (` + placeholders + `)`
		for _, t := range types {
			args = append(args, t)
		}
	}
	if agentIDFilter != "" {
		query += ` AND agent_id = ?`
		args = append(args, agentIDFilter)
	}
	query += ` ORDER BY seq ASC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var events []Event
	var latestSeq int64
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.Seq, &e.Type, &e.AgentID, &e.Payload, &e.CreatedAt); err != nil {
			return nil, 0, err
		}
		events = append(events, e)
		if e.Seq > latestSeq {
			latestSeq = e.Seq
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if latestSeq == 0 {
		// Return the current max seq so the caller always has a valid cursor.
		_ = s.db.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM events`).Scan(&latestSeq)
	}
	return events, latestSeq, nil
}

// ── Annotations ──────────────────────────────────────────────────────────────

// Annotation is a note attached to a graph node by an agent.
type Annotation struct {
	ID        string `json:"id"`
	NodeID    string `json:"node_id"`
	AgentID   string `json:"agent_id,omitempty"`
	Note      string `json:"note"`
	CreatedAt string `json:"created_at"`
	// Source distinguishes manually-added agent notes ("agent") from
	// system-generated retrospective notes ("system"). Defaults to "agent".
	Source string `json:"source,omitempty"`
	// Stale is true when the node's call-graph changed significantly (fan-in delta
	// >20% or node removed) since the annotation was written. Treat stale
	// annotations as hints, not facts — they may describe outdated structure.
	Stale bool `json:"stale,omitempty"`
}

// AddAnnotation attaches a note to a graph node. Source is set to "agent".
func (s *Store) AddAnnotation(nodeID, agentID, note string) (string, error) {
	id := fmt.Sprintf("%x", time.Now().UnixNano())
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(
		`INSERT INTO annotations (id, node_id, agent_id, note, created_at, source) VALUES (?, ?, ?, ?, ?, 'agent')`,
		id, nodeID, agentID, note, now,
	)
	return id, err
}

// AddAnnotationIfNew attaches a note to a graph node only when no annotation
// from the same agentID with the same note content already exists within the
// last dedupeWindow seconds. Returns the new annotation ID and true on insert,
// or ("", false, nil) when deduplication suppresses the write.
func (s *Store) AddAnnotationIfNew(nodeID, agentID, note string, dedupeWindow time.Duration) (string, bool, error) {
	id := fmt.Sprintf("%x", time.Now().UnixNano())
	now := time.Now().UTC().Format(time.RFC3339)
	windowSec := fmt.Sprintf("-%d seconds", int(dedupeWindow.Seconds()))
	result, err := s.db.Exec(
		`INSERT INTO annotations (id, node_id, agent_id, note, created_at, source)
		 SELECT ?, ?, ?, ?, ?, 'agent'
		 WHERE NOT EXISTS (
		     SELECT 1 FROM annotations
		     WHERE node_id = ? AND agent_id = ?
		       AND note = ?
		       AND created_at > datetime('now', ?)
		 )`,
		id, nodeID, agentID, note, now,
		nodeID, agentID, note, windowSec,
	)
	if err != nil {
		return "", false, err
	}
	rows, _ := result.RowsAffected()
	return id, rows > 0, nil
}

// AddSystemAnnotation attaches a system-generated retrospective note to a graph
// node. Unlike AddAnnotation it sets source='system' so callers can distinguish
// automated notes from agent-authored ones. Used by the Reflective Synthesis
// auditor that runs when a task is marked done.
func (s *Store) AddSystemAnnotation(nodeID, note string) (string, error) {
	id := fmt.Sprintf("%x", time.Now().UnixNano())
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO annotations (id, node_id, agent_id, note, created_at, source) VALUES (?, ?, '', ?, ?, 'system')`,
		id, nodeID, note, now,
	)
	return id, err
}

// GetAnnotationsForNodes returns all annotations for the given node IDs,
// keyed by node ID. Returns an empty map if none exist.
func (s *Store) GetAnnotationsForNodes(nodeIDs []string) (map[string][]Annotation, error) {
	if len(nodeIDs) == 0 {
		return map[string][]Annotation{}, nil
	}
	placeholders := strings.Repeat("?,", len(nodeIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]interface{}, len(nodeIDs))
	for i, id := range nodeIDs {
		args[i] = id
	}
	rows, err := s.db.Query(
		`SELECT id, node_id, agent_id, note, created_at, source, stale FROM annotations WHERE node_id IN (`+placeholders+`) ORDER BY created_at ASC`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string][]Annotation)
	for rows.Next() {
		var a Annotation
		var staleInt int
		if err := rows.Scan(&a.ID, &a.NodeID, &a.AgentID, &a.Note, &a.CreatedAt, &a.Source, &staleInt); err != nil {
			return nil, err
		}
		a.Stale = staleInt != 0
		result[a.NodeID] = append(result[a.NodeID], a)
	}
	return result, rows.Err()
}

// MarkAnnotationsStale marks all annotations on the given node IDs as stale.
// Called when a node's call-graph changes significantly (fan-in delta >20%)
// or when a node is removed, so agents see a warning in get_context.
func (s *Store) MarkAnnotationsStale(nodeIDs []string) error {
	if len(nodeIDs) == 0 {
		return nil
	}
	placeholders := strings.Repeat("?,", len(nodeIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]interface{}, len(nodeIDs))
	for i, id := range nodeIDs {
		args[i] = id
	}
	_, err := s.db.Exec(
		`UPDATE annotations SET stale=1 WHERE node_id IN (`+placeholders+`) AND stale=0`,
		args...,
	)
	return err
}

// ─── Work Claims ──────────────────────────────────────────────────────────────

// WorkClaim describes one agent's active lock on a scope of work.
type WorkClaim struct {
	AgentID   string    `json:"agent_id"`
	Scope     string    `json:"scope"`
	ScopeType string    `json:"scope_type"`
	ClaimedAt time.Time `json:"claimed_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// pruneExpiredClaims removes all claims whose expires_at is in the past.
func (s *Store) pruneExpiredClaims() {
	_, _ = s.db.Exec(`DELETE FROM work_claims WHERE expires_at < ?`,
		time.Now().UTC().Format(time.RFC3339))
}

// ClaimWork upserts a claim for agentID over scope. ttlMinutes must be > 0.
// Returns any active claims by OTHER agents that conflict with the same scope
// (exact match or directory-prefix overlap). Caller should inspect conflicts
// before starting work if coordination matters.
func (s *Store) ClaimWork(agentID, scope, scopeType string, ttlMinutes int) ([]WorkClaim, error) {
	s.pruneExpiredClaims()
	if ttlMinutes <= 0 {
		ttlMinutes = 30
	}
	now := time.Now().UTC()
	exp := now.Add(time.Duration(ttlMinutes) * time.Minute)
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO work_claims (agent_id, scope, scope_type, claimed_at, expires_at) VALUES (?,?,?,?,?)`,
		agentID, scope, scopeType, now.Format(time.RFC3339), exp.Format(time.RFC3339),
	)
	if err != nil {
		return nil, err
	}

	// Find conflicts: other agents claiming the same scope exactly, or whose
	// scope is a directory-prefix of (or is prefixed by) the new claim's scope.
	rows, err := s.db.Query(
		`SELECT agent_id, scope, scope_type, claimed_at, expires_at
         FROM work_claims
         WHERE agent_id != ? AND expires_at > ?
           AND (scope = ? OR scope LIKE ? OR ? LIKE scope || '/%')`,
		agentID, now.Format(time.RFC3339),
		scope,
		scope+"/%", // other agent claims a sub-path of scope
		scope,      // other agent claims a parent directory of scope
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanWorkClaims(rows)
}

// GetConflicts returns all work claims by other agents that overlap with any
// scope the given agent currently holds. Expired claims are pruned first.
func (s *Store) GetConflicts(agentID string) ([]WorkClaim, error) {
	s.pruneExpiredClaims()
	now := time.Now().UTC().Format(time.RFC3339)

	// Get the agent's own active scopes.
	myRows, err := s.db.Query(
		`SELECT scope FROM work_claims WHERE agent_id = ? AND expires_at > ?`,
		agentID, now,
	)
	if err != nil {
		return nil, err
	}
	var myScopes []string
	for myRows.Next() {
		var sc string
		if err := myRows.Scan(&sc); err != nil {
			myRows.Close()
			return nil, err
		}
		myScopes = append(myScopes, sc)
	}
	myRows.Close()

	if len(myScopes) == 0 {
		return nil, nil
	}

	// For each of the agent's scopes, find other agents' conflicting claims.
	seen := make(map[string]struct{})
	var conflicts []WorkClaim
	for _, sc := range myScopes {
		rows, err := s.db.Query(
			`SELECT agent_id, scope, scope_type, claimed_at, expires_at
             FROM work_claims
             WHERE agent_id != ? AND expires_at > ?
               AND (scope = ? OR scope LIKE ? OR ? LIKE scope || '/%')`,
			agentID, now,
			sc,
			sc+"/%",
			sc,
		)
		if err != nil {
			return nil, err
		}
		cls, err := scanWorkClaims(rows)
		if err != nil {
			return nil, err
		}
		for _, c := range cls {
			key := c.AgentID + "|" + c.Scope
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			conflicts = append(conflicts, c)
		}
	}
	return conflicts, nil
}

// ReleaseClaims removes all active claims for the given agent. Call this when
// work is complete to immediately free scopes for other agents.
func (s *Store) ReleaseClaims(agentID string) error {
	_, err := s.db.Exec(`DELETE FROM work_claims WHERE agent_id = ?`, agentID)
	return err
}

// GetMyClaims returns all non-expired claims for the given agent.
func (s *Store) GetMyClaims(agentID string) ([]WorkClaim, error) {
	s.pruneExpiredClaims()
	rows, err := s.db.Query(
		`SELECT agent_id, scope, scope_type, claimed_at, expires_at
         FROM work_claims WHERE agent_id = ? AND expires_at > ?
         ORDER BY claimed_at DESC`,
		agentID, time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		return nil, err
	}
	return scanWorkClaims(rows)
}

// GetAllClaims returns every non-expired work claim across all agents.
// Used by the peer API /claims endpoint to expose active work state to peers.
func (s *Store) GetAllClaims() ([]WorkClaim, error) {
	s.pruneExpiredClaims()
	rows, err := s.db.Query(
		`SELECT agent_id, scope, scope_type, claimed_at, expires_at
         FROM work_claims WHERE expires_at > ?
         ORDER BY claimed_at DESC`,
		time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		return nil, err
	}
	return scanWorkClaims(rows)
}

func scanWorkClaims(rows *sql.Rows) ([]WorkClaim, error) {
	defer rows.Close()
	var claims []WorkClaim
	for rows.Next() {
		var c WorkClaim
		var claimedStr, expiresStr string
		if err := rows.Scan(&c.AgentID, &c.Scope, &c.ScopeType, &claimedStr, &expiresStr); err != nil {
			return nil, err
		}
		c.ClaimedAt, _ = time.Parse(time.RFC3339, claimedStr)
		c.ExpiresAt, _ = time.Parse(time.RFC3339, expiresStr)
		claims = append(claims, c)
	}
	return claims, rows.Err()
}

// ── Tool Call Observability ───────────────────────────────────────────────────

// ToolUsageStat summarises call patterns for one MCP tool over a time window.
type ToolUsageStat struct {
	ToolName  string  `json:"tool_name"`
	CallCount int     `json:"call_count"`
	AvgMs     float64 `json:"avg_ms"`
	ErrorRate float64 `json:"error_rate"` // 0.0–1.0 fraction of calls that errored
}

// RecordToolCall inserts one row into the tool_calls table. All errors are
// silently discarded — observability must never block the hot path.
// sessionID is the Synapses session UUID from CreateSession; empty for pre-init calls.
func (s *Store) RecordToolCall(toolName, agentID, sessionID, entity string, durationMs int64, success bool) {
	succ := 1
	if !success {
		succ = 0
	}
	_, _ = s.db.Exec(
		`INSERT INTO tool_calls(tool_name,agent_id,session_id,entity,duration_ms,success) VALUES(?,?,?,?,?,?)`,
		toolName, agentID, sessionID, entity, durationMs, succ,
	)
}

// ToolUsageStats returns the top-N tools by call count over the last `days` days.
func (s *Store) ToolUsageStats(days, limit int) ([]ToolUsageStat, error) {
	if limit <= 0 {
		limit = 10
	}
	since := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02T15:04:05Z")
	rows, err := s.db.Query(`
        SELECT tool_name,
               COUNT(*) AS calls,
               AVG(duration_ms) AS avg_ms,
               1.0 - AVG(success) AS error_rate
        FROM tool_calls
        WHERE created_at >= ?
        GROUP BY tool_name
        ORDER BY calls DESC
        LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stats []ToolUsageStat
	for rows.Next() {
		var s ToolUsageStat
		if err := rows.Scan(&s.ToolName, &s.CallCount, &s.AvgMs, &s.ErrorRate); err != nil {
			return nil, err
		}
		stats = append(stats, s)
	}
	return stats, rows.Err()
}

// ---------------------------------------------------------------------------
// GraphIndex snapshot persistence
//
// The snapshot is stored as a binary BLOB in the meta table under the key
// "graph_snapshot". On a warm boot where file_hashes are unchanged, the watcher
// can call LoadIndexSnapshot to restore the columnar index in <200ms instead of
// re-parsing every source file.
// ---------------------------------------------------------------------------

// SaveIndexSnapshot persists a zstd-compressed GraphIndex BLOB to the meta table.
func (s *Store) SaveIndexSnapshot(blob []byte) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO meta (key, value) VALUES ('graph_snapshot', ?)`,
		blob,
	)
	return err
}

// LoadIndexSnapshot returns the raw BLOB previously saved by SaveIndexSnapshot,
// or (nil, nil) if no snapshot exists.
func (s *Store) LoadIndexSnapshot() ([]byte, error) {
	var blob []byte
	err := s.db.QueryRow(
		`SELECT value FROM meta WHERE key = 'graph_snapshot'`,
	).Scan(&blob)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return blob, err
}
