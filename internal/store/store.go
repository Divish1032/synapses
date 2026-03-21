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
	"github.com/SynapsesOS/synapses/internal/logutil"
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

// graphSchema contains tables for the code-domain graph: nodes, edges,
// call sites, file modification times, and node embeddings. Code-mode
// projects open this database; knowledge-mode projects do not.
const graphSchema = `
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

-- FTS5 full-text search index over node names, signatures, and doc comments.
CREATE VIRTUAL TABLE IF NOT EXISTS nodes_fts USING fts5(
    node_id   UNINDEXED,
    name,
    split_name,
    signature,
    doc,
    tokenize = "unicode61 remove_diacritics 2"
);

CREATE INDEX IF NOT EXISTS idx_nodes_file      ON nodes(file);
CREATE INDEX IF NOT EXISTS idx_nodes_name      ON nodes(name);
CREATE INDEX IF NOT EXISTS idx_edges_from      ON edges(from_id);
CREATE INDEX IF NOT EXISTS idx_edges_to        ON edges(to_id);
CREATE INDEX IF NOT EXISTS idx_call_sites_caller ON call_sites(caller_id);
CREATE INDEX IF NOT EXISTS idx_embeddings_node ON node_embeddings(node_id);
CREATE INDEX IF NOT EXISTS idx_nodes_type_pkg  ON nodes(type, package);
CREATE INDEX IF NOT EXISTS idx_edges_to_type   ON edges(to_id, type);
CREATE INDEX IF NOT EXISTS idx_edges_type_to   ON edges(type, to_id);
CREATE INDEX IF NOT EXISTS idx_nodes_pkg       ON nodes(package);
`

// knowledgeSchema contains tables for universal knowledge: memories, episodes,
// sessions, events, messages, tasks, annotations, rules, gaps, and all agent
// coordination state. Knowledge-mode projects open ONLY this database.
const knowledgeSchema = `
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

CREATE TABLE IF NOT EXISTS session_state (
    id               TEXT NOT NULL,
    task_id          TEXT NOT NULL,
    agent_id         TEXT NOT NULL DEFAULT '',
    approach         TEXT NOT NULL DEFAULT '',
    files_modified   TEXT NOT NULL DEFAULT '[]',
    completed_steps  TEXT NOT NULL DEFAULT '[]',
    remaining_steps  TEXT NOT NULL DEFAULT '[]',
    blockers         TEXT NOT NULL DEFAULT '[]',
    decisions        TEXT NOT NULL DEFAULT '[]',
    context_snapshot TEXT NOT NULL DEFAULT '',
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    PRIMARY KEY (task_id)
);

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

CREATE TABLE IF NOT EXISTS agents (
    id        TEXT PRIMARY KEY,
    last_seen TEXT NOT NULL,
    metadata  TEXT NOT NULL DEFAULT '{}'
);

CREATE TABLE IF NOT EXISTS agent_context (
    agent_id       TEXT PRIMARY KEY,
    last_event_seq INTEGER NOT NULL DEFAULT 0,
    identity_hash  TEXT    NOT NULL DEFAULT '',
    last_session   TEXT    NOT NULL DEFAULT '',
    task_seq       INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS events (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    type       TEXT NOT NULL,
    agent_id   TEXT NOT NULL DEFAULT '',
    payload    TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS web_cache (
    url        TEXT PRIMARY KEY,
    content    TEXT NOT NULL,
    fetched_at TEXT NOT NULL,
    ttl_hours  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS annotations (
    id         TEXT PRIMARY KEY,
    node_id    TEXT NOT NULL,
    agent_id   TEXT NOT NULL DEFAULT '',
    note       TEXT NOT NULL,
    created_at TEXT NOT NULL,
    source     TEXT NOT NULL DEFAULT 'agent'
);

-- DEPRECATED: preserved for existing databases.
CREATE TABLE IF NOT EXISTS work_claims (
    agent_id   TEXT NOT NULL,
    scope      TEXT NOT NULL,
    scope_type TEXT NOT NULL DEFAULT 'file',
    claimed_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    PRIMARY KEY (agent_id, scope)
);

CREATE TABLE IF NOT EXISTS proposals (
    id             TEXT PRIMARY KEY,
    agent_id       TEXT NOT NULL,
    title          TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    affected_nodes TEXT NOT NULL DEFAULT '[]',
    status         TEXT NOT NULL DEFAULT 'open',
    vote_threshold INTEGER NOT NULL DEFAULT 2,
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS proposal_votes (
    proposal_id TEXT NOT NULL,
    agent_id    TEXT NOT NULL,
    vote        TEXT NOT NULL,
    rationale   TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    PRIMARY KEY (proposal_id, agent_id)
);

CREATE TABLE IF NOT EXISTS tool_calls (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    tool_name   TEXT    NOT NULL,
    agent_id    TEXT    NOT NULL DEFAULT '',
    session_id  TEXT    NOT NULL DEFAULT '',
    entity      TEXT    NOT NULL DEFAULT '',
    duration_ms INTEGER NOT NULL DEFAULT 0,
    success     INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE TABLE IF NOT EXISTS sessions (
    id                TEXT    PRIMARY KEY,
    agent_id          TEXT    NOT NULL,
    project_id        TEXT    NOT NULL,
    mcp_session_id    TEXT    NOT NULL DEFAULT '',
    intent            TEXT    NOT NULL DEFAULT '',
    started_at        INTEGER NOT NULL,
    last_seen_at      INTEGER NOT NULL,
    ended_at          INTEGER,
    end_reason        TEXT    NOT NULL DEFAULT '',
    outcome           TEXT    NOT NULL DEFAULT '',
    summary           TEXT    NOT NULL DEFAULT '',
    tool_calls        INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS session_tasks (
    session_id TEXT    NOT NULL,
    task_id    TEXT    NOT NULL,
    action     TEXT    NOT NULL,
    at         INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS agent_messages (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    id         TEXT    NOT NULL UNIQUE,
    from_agent TEXT    NOT NULL,
    to_agent   TEXT,
    topic      TEXT    NOT NULL,
    payload    TEXT    NOT NULL DEFAULT '{}',
    project_id TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    read_at    INTEGER
);

CREATE TABLE IF NOT EXISTS episodes (
    id              TEXT PRIMARY KEY,
    agent_id        TEXT NOT NULL,
    project_id      TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    episode_type    TEXT NOT NULL DEFAULT 'decision',
    outcome         TEXT NOT NULL DEFAULT 'unknown',
    trigger         TEXT NOT NULL DEFAULT '',
    decision        TEXT NOT NULL,
    rationale       TEXT NOT NULL DEFAULT '',
    affected_files  TEXT NOT NULL DEFAULT '[]',
    affected_nodes  TEXT NOT NULL DEFAULT '[]',
    tags            TEXT NOT NULL DEFAULT '[]',
    importance      REAL NOT NULL DEFAULT 0.5,
    promoted_rule   TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS quality_gaps (
    id          TEXT PRIMARY KEY,
    node_id     TEXT NOT NULL,
    gap_id      TEXT NOT NULL,
    description TEXT NOT NULL,
    severity    TEXT NOT NULL DEFAULT 'medium',
    status      TEXT NOT NULL DEFAULT 'open',
    found_by    TEXT NOT NULL DEFAULT '',
    found_at    TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    fix_notes   TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS memories (
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
    source           TEXT NOT NULL DEFAULT 'manual',
    importance       TEXT NOT NULL DEFAULT '1.0'
);

-- FTS5 for episodes.
CREATE VIRTUAL TABLE IF NOT EXISTS episodes_fts USING fts5(
    decision, rationale, trigger, tags,
    content='episodes', content_rowid='rowid'
);
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

CREATE TABLE IF NOT EXISTS cross_project_deps (
    from_entity          TEXT NOT NULL,
    to_project           TEXT NOT NULL,
    to_entity            TEXT NOT NULL,
    to_file              TEXT NOT NULL,
    verified_commit      TEXT NOT NULL,
    verified_at          TEXT NOT NULL,
    detection_tier       TEXT NOT NULL DEFAULT 'tier1',
    verified_signature   TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (from_entity, to_project, to_entity)
);

CREATE INDEX IF NOT EXISTS idx_tasks_status        ON tasks(status);
CREATE INDEX IF NOT EXISTS idx_tasks_plan_id       ON tasks(plan_id);
CREATE INDEX IF NOT EXISTS idx_dynamic_rules_id    ON dynamic_rules(id);
CREATE INDEX IF NOT EXISTS idx_vlog_rule           ON violation_log(rule_id);
CREATE INDEX IF NOT EXISTS idx_vlog_last_seen      ON violation_log(last_seen);
CREATE INDEX IF NOT EXISTS idx_agent_context_id    ON agent_context(agent_id);
CREATE INDEX IF NOT EXISTS idx_events_created      ON events(created_at);
CREATE INDEX IF NOT EXISTS idx_events_type         ON events(type);
CREATE INDEX IF NOT EXISTS idx_web_cache_fetched   ON web_cache(fetched_at);
CREATE INDEX IF NOT EXISTS idx_annotations_node    ON annotations(node_id);
CREATE INDEX IF NOT EXISTS idx_work_claims_scope   ON work_claims(scope);
CREATE INDEX IF NOT EXISTS idx_work_claims_expires ON work_claims(expires_at);
CREATE INDEX IF NOT EXISTS idx_proposals_status    ON proposals(status);
CREATE INDEX IF NOT EXISTS idx_proposals_agent     ON proposals(agent_id);
CREATE INDEX IF NOT EXISTS idx_pvotes_proposal     ON proposal_votes(proposal_id);
CREATE INDEX IF NOT EXISTS idx_tool_calls_tool     ON tool_calls(tool_name);
CREATE INDEX IF NOT EXISTS idx_tool_calls_ts       ON tool_calls(created_at);
CREATE INDEX IF NOT EXISTS idx_tool_calls_session  ON tool_calls(session_id) WHERE session_id != '';
CREATE INDEX IF NOT EXISTS idx_sessions_agent      ON sessions(agent_id);
CREATE INDEX IF NOT EXISTS idx_sessions_project    ON sessions(project_id);
CREATE INDEX IF NOT EXISTS idx_sessions_active     ON sessions(ended_at) WHERE ended_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_sessions_mcp        ON sessions(mcp_session_id, agent_id, project_id);
CREATE INDEX IF NOT EXISTS idx_session_tasks_session ON session_tasks(session_id);
CREATE INDEX IF NOT EXISTS idx_session_tasks_task  ON session_tasks(task_id);
CREATE INDEX IF NOT EXISTS idx_messages_to_agent   ON agent_messages(to_agent, read_at);
CREATE INDEX IF NOT EXISTS idx_messages_created    ON agent_messages(created_at);
CREATE INDEX IF NOT EXISTS idx_messages_seq        ON agent_messages(seq);
CREATE INDEX IF NOT EXISTS idx_episodes_agent      ON episodes(agent_id, created_at);
CREATE INDEX IF NOT EXISTS idx_episodes_project    ON episodes(project_id, created_at);
CREATE INDEX IF NOT EXISTS idx_episodes_type       ON episodes(episode_type, outcome);
CREATE INDEX IF NOT EXISTS idx_quality_gaps_node   ON quality_gaps(node_id);
CREATE INDEX IF NOT EXISTS idx_quality_gaps_status ON quality_gaps(status);
CREATE INDEX IF NOT EXISTS idx_memories_tier       ON memories(tier);
CREATE INDEX IF NOT EXISTS idx_memories_entity     ON memories(entity_id) WHERE entity_id != '';
CREATE INDEX IF NOT EXISTS idx_memories_agent      ON memories(agent_id) WHERE agent_id != '';
CREATE INDEX IF NOT EXISTS idx_memories_expires    ON memories(expires_at);
CREATE INDEX IF NOT EXISTS idx_cross_deps_project  ON cross_project_deps(to_project);
CREATE INDEX IF NOT EXISTS idx_cross_deps_file     ON cross_project_deps(to_project, to_file);
`

// Store wraps two SQLite databases — one for the code graph (nodes, edges,
// call sites, file hashes) and one for universal knowledge (memories, episodes,
// sessions, events, messages, tasks, annotations, rules, gaps). Knowledge-mode
// projects open only the knowledgeDB; code-mode projects open both.
//
// # Cross-DB Consistency Model
//
// graph.db and knowledge.db have NO cross-database transactional guarantee.
// SQLite does not support transactions spanning two separate database files, and
// Synapses deliberately avoids ATTACH DATABASE to prevent WAL-locking
// interactions between the two journals.
//
// Cross-references that span the two databases:
//
// Hard references (node_id is the primary lookup key — orphans cause stale results):
//
//   - annotations.node_id    → graphDB nodes.id
//   - memory_anchors.node_id → graphDB nodes.id  (triggers staleness tracking on node change)
//   - quality_gaps.node_id   → graphDB nodes.id  (gaps for a node persist after the node is renamed/deleted)
//
// Soft references (node IDs stored as JSON in TEXT columns — informational only, not cleaned up):
//
//   - episodes.affected_nodes  — historical record; stale IDs do not affect episode retrieval
//   - proposals.affected_nodes — captures nodes at proposal-creation time; may reference absent nodes
//
// These hard references may briefly point to non-existent node IDs during a reindex:
// the file watcher deletes stale graph nodes and re-inserts them as parsing
// completes, while knowledge records referencing those IDs persist in knowledgeDB.
// This is an intentional eventual-consistency window, not a data-loss event.
//
// The fail-open design makes this safe:
//
//   - Dangling anchor or annotation references are silently skipped when the
//     referenced node is absent — callers receive fewer results, not an error.
//   - PruneStaleData (runs daily) reconciles orphaned annotations AND quality gaps
//     by cross-checking knowledgeDB against the current graphDB node set.
//   - The staleness-tracking pipeline re-links anchors when the underlying
//     node is re-added by the watcher after reindex completes.
//
// Future improvement: a startup reconciliation pass in Open() that checks all
// hard-reference node_ids against graphDB and flags orphans immediately,
// rather than waiting for the next daily prune cycle.
type Store struct {
	graphDB     *rwDB // code-domain: nodes, edges, meta, file_hashes, call_sites, node_embeddings
	knowledgeDB *rwDB // universal: memories, episodes, sessions, events, tasks, agents, ...

	// lastPruneMu guards all prune timestamps to prevent redundant concurrent prunes.
	lastPruneMu        sync.Mutex
	lastPruneAt        time.Time // tool_calls prune (hourly debounce)
	lastSessionPruneAt time.Time // sessions prune (daily debounce)
	lastPruneStaleAt   time.Time // PruneStaleData (daily debounce)

	// semanticDedupFunc embeds text on-the-fly for semantic dedup in prepareMemory.
	// When set: Jaccard in [0.5, 0.85) triggers a cosine similarity check against
	// the candidate's stored embedding. When nil: inconclusive range falls through
	// (no semantic dedup — same as pre-Sprint-11 behavior).
	semanticDedupFunc func(text string) ([]float32, error)
}

// SetSemanticDedupFunc sets the embedding function used for semantic dedup
// in prepareMemory. When Jaccard similarity is inconclusive (0.5–0.85), the
// function embeds the new content and compares against the candidate's stored
// embedding. Pass nil to disable semantic dedup (default).
func (s *Store) SetSemanticDedupFunc(fn func(text string) ([]float32, error)) {
	s.semanticDedupFunc = fn
}

// CacheDir returns the canonical directory where synapses stores all project
// index databases: ~/.synapses/cache/
//
// Using the home directory (rather than os.UserCacheDir which resolves to
// ~/Library/Caches on macOS, ~/.cache on Linux, %LocalAppData% on Windows)
// gives a single, discoverable, cross-platform path that is not subject to
// OS or tool-driven cache eviction.
func CacheDir() (string, error) {
	// Allow tests to override the cache directory so they don't pollute the
	// user's ~/.synapses/cache with temp-path DBs that persist after cleanup.
	if override := os.Getenv("SYNAPSES_CACHE_DIR"); override != "" {
		return override, nil
	}
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

// Open opens (or creates) both the graph and knowledge SQLite databases at
// the given path and applies schema migrations. The graph database lives at
// path; the knowledge database lives at KnowledgePath(path).
func Open(path string) (*Store, error) {
	graphDB, err := openSQLiteDB(path)
	if err != nil {
		return nil, fmt.Errorf("open graph db: %w", err)
	}
	// Detect graph.db corruption early. Graph data is fully regenerable from
	// source, so the safe recovery is to delete the corrupt file and start
	// fresh — the caller will perform a full re-index automatically.
	if checkErr := runQuickCheck(graphDB); checkErr != nil {
		logutil.Error("synapses: store: graph.db corrupt (%v) — deleting and re-indexing from source\n", checkErr)
		graphDB.Close()
		if recoverErr := recoverGraphDB(path); recoverErr != nil {
			return nil, recoverErr
		}
		graphDB, err = openSQLiteDB(path)
		if err != nil {
			return nil, fmt.Errorf("open fresh graph db after corruption recovery: %w", err)
		}
	}
	if _, err := graphDB.Exec(graphSchema); err != nil {
		graphDB.Close()
		return nil, fmt.Errorf("apply graph schema: %w", err)
	}

	kPath := KnowledgePath(path)
	knowledgeDB, err := openSQLiteDB(kPath)
	if err != nil {
		graphDB.Close()
		return nil, fmt.Errorf("open knowledge db: %w", err)
	}
	// Detect knowledge.db corruption. Knowledge data (memories, tasks) is NOT
	// regenerable, so we back up the corrupt file and start with a fresh empty
	// database rather than refusing to start. The daemon continues in a
	// degraded-but-functional state.
	if checkErr := runQuickCheck(knowledgeDB); checkErr != nil {
		logutil.Error("synapses: store: knowledge.db corrupt (%v) — backing up to knowledge.db.corrupt and starting fresh\n", checkErr)
		knowledgeDB.Close()
		knowledgeDB, err = recoverKnowledgeDB(kPath)
		if err != nil {
			graphDB.Close()
			return nil, fmt.Errorf("recover corrupt knowledge db: %w", err)
		}
	}
	if _, err := knowledgeDB.Exec(knowledgeSchema); err != nil {
		graphDB.Close()
		knowledgeDB.Close()
		return nil, fmt.Errorf("apply knowledge schema: %w", err)
	}
	// ── Graph migrations ─────────────────────────────────────────────────
	// "duplicate column name" errors are safe to ignore — they mean the column
	// was already created by CREATE TABLE (fresh DB) or a previous migration run.
	for _, m := range []string{
		`ALTER TABLE nodes ADD COLUMN doc TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE nodes ADD COLUMN signature TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE nodes ADD COLUMN line_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE nodes ADD COLUMN stable_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE nodes ADD COLUMN provenance TEXT NOT NULL DEFAULT 'user-authored'`,
		`ALTER TABLE nodes ADD COLUMN domain TEXT NOT NULL DEFAULT 'code'`,
		`ALTER TABLE nodes ADD COLUMN prev_signature TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE node_embeddings ADD COLUMN content_hash TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := graphDB.Exec(m); err != nil && !isDupColumnErr(err) {
			graphDB.Close()
			knowledgeDB.Close()
			return nil, fmt.Errorf("migrate graph schema: %w", err)
		}
	}

	// ── Knowledge migrations ─────────────────────────────────────────────
	for _, m := range []string{
		`ALTER TABLE plans ADD COLUMN created_by TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE plans ADD COLUMN completed_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE tasks ADD COLUMN assigned_to TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE tasks ADD COLUMN last_updated_by TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE tasks ADD COLUMN depends_on TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE tasks ADD COLUMN start_commit TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE tasks ADD COLUMN commits TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE annotations ADD COLUMN source TEXT NOT NULL DEFAULT 'agent'`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_annotations_system_dedup ON annotations(node_id, note) WHERE source='system'`,
		`ALTER TABLE annotations ADD COLUMN stale INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE agents ADD COLUMN current_task_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN current_task_title TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN current_focus TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN current_scope TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN project_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN focus_file  TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN focus_since TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agents ADD COLUMN intent      TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE dynamic_rules ADD COLUMN rule_type TEXT NOT NULL DEFAULT 'structural'`,
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
		`CREATE TABLE IF NOT EXISTS memory_anchors (
			memory_id  TEXT NOT NULL,
			node_id    TEXT NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY (memory_id, node_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_anchors_node ON memory_anchors(node_id)`,
		`ALTER TABLE memories ADD COLUMN stale INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE memories ADD COLUMN stale_reason TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE memories ADD COLUMN surfaced_at TEXT DEFAULT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_memories_stale ON memories(stale) WHERE stale = 1`,
		`ALTER TABLE memories ADD COLUMN staled_at TEXT NOT NULL DEFAULT ''`,
		`CREATE TABLE IF NOT EXISTS memory_surfaced (
			memory_id TEXT NOT NULL,
			agent_id  TEXT NOT NULL,
			surfaced_at TEXT NOT NULL,
			PRIMARY KEY (memory_id, agent_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_surfaced_agent ON memory_surfaced(agent_id)`,
		`ALTER TABLE tool_calls ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_tool_calls_session ON tool_calls(session_id) WHERE session_id != ''`,
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
		`ALTER TABLE cross_project_deps ADD COLUMN verified_signature TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN last_branch TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sessions ADD COLUMN state TEXT NOT NULL DEFAULT 'active'`,
		`ALTER TABLE sessions ADD COLUMN parent_session_id TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_state ON sessions(state, agent_id, project_id)`,
		// Sprint 6.7: context outcome instrumentation for Sprint 11 feedback loop.
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
		`CREATE INDEX IF NOT EXISTS idx_cd_session ON context_deliveries(session_id) WHERE session_id != ''`,
		`CREATE INDEX IF NOT EXISTS idx_cd_entity  ON context_deliveries(entity, agent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_cd_created ON context_deliveries(created_at)`,
		// Sprint 6.8: vector storage for memory embeddings (quad-retrieval foundation).
		`CREATE TABLE IF NOT EXISTS memory_embeddings (
			memory_id    TEXT PRIMARY KEY,
			model        TEXT NOT NULL DEFAULT '',
			embedding    BLOB NOT NULL,
			content_hash TEXT NOT NULL DEFAULT '',
			stale        INTEGER NOT NULL DEFAULT 0,
			embedded_at  INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memembed_stale ON memory_embeddings(stale) WHERE stale = 1`,
		// Sprint 10.1: memory versioning — preserve historical snapshots on dedup overwrite.
		`CREATE TABLE IF NOT EXISTS memory_versions (
			id              TEXT PRIMARY KEY,
			memory_id       TEXT NOT NULL,
			version         INTEGER NOT NULL DEFAULT 1,
			content         TEXT NOT NULL,
			superseded_by   TEXT NOT NULL DEFAULT '',
			created_at      TEXT NOT NULL,
			superseded_at   TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memver_memory ON memory_versions(memory_id, version)`,
		// Sprint 10.2: knowledge decay scoring — importance field on memories.
		`ALTER TABLE memories ADD COLUMN importance TEXT NOT NULL DEFAULT '1.0'`,
		// Sprint 11.5: ACT-R frequency-weighted decay — access counter on memories.
		`ALTER TABLE memories ADD COLUMN access_count INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := knowledgeDB.Exec(m); err != nil && !isDupColumnErr(err) {
			graphDB.Close()
			knowledgeDB.Close()
			return nil, fmt.Errorf("migrate knowledge schema: %w", err)
		}
	}

	// Fix historical rows: sessions with ended_at already set must be 'closed'.
	_, _ = knowledgeDB.Exec(`UPDATE sessions SET state = 'closed' WHERE ended_at IS NOT NULL AND state = 'active'`)

	// Migrate data from legacy single-DB if this is the first split.
	// Only runs when knowledgeDB is empty (fresh) and graphDB has legacy knowledge tables.
	var knowledgeRowCount int
	_ = knowledgeDB.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&knowledgeRowCount)
	if knowledgeRowCount == 0 {
		// Check if legacy DB has knowledge tables with data.
		var legacyHasMemories int
		legacyHasErr := graphDB.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='memories'`,
		).Scan(&legacyHasMemories)
		if legacyHasErr == nil && legacyHasMemories > 0 {
			if err := migrateKnowledgeFromLegacy(knowledgeDB, path); err != nil {
				logutil.Warn("synapses: store: legacy migration: %v\n", err)
			}
		}
	}

	// Schema init complete. Create reader/writer pool split.
	// Writer: MaxOpenConns=1 (serializes writes — SQLite allows one writer).
	// Reader: MaxOpenConns=8 (concurrent reads — WAL mode allows unlimited readers).
	// This replaces the old MaxOpenConns(2) which only allowed 1 reader + 1 writer.
	graphRW, err := newRWDB(path, graphDB, 8)
	if err != nil {
		graphDB.Close()
		knowledgeDB.Close()
		return nil, fmt.Errorf("create graph reader pool: %w", err)
	}
	knowledgeRW, err := newRWDB(KnowledgePath(path), knowledgeDB, 8)
	if err != nil {
		graphRW.Close()
		knowledgeDB.Close() // newRWDB doesn't take ownership on failure
		return nil, fmt.Errorf("create knowledge reader pool: %w", err)
	}

	st := &Store{graphDB: graphRW, knowledgeDB: knowledgeRW}

	// Rebuild FTS index for existing databases where nodes_fts is empty but
	// the nodes table already has data.
	var ftsCount, nodeCount int
	_ = graphDB.QueryRow(`SELECT count(*) FROM nodes_fts`).Scan(&ftsCount)
	_ = graphDB.QueryRow(`SELECT count(*) FROM nodes`).Scan(&nodeCount)
	if ftsCount == 0 && nodeCount > 0 {
		_ = st.rebuildFTS()
	}

	// Backfill memories_fts for upgraded databases.
	var memFtsCount, memCount int
	_ = knowledgeDB.QueryRow(`SELECT count(*) FROM memories_fts`).Scan(&memFtsCount)
	_ = knowledgeDB.QueryRow(`SELECT count(*) FROM memories`).Scan(&memCount)
	if memFtsCount == 0 && memCount > 0 {
		_, _ = knowledgeDB.Exec(`INSERT INTO memories_fts(memories_fts) VALUES ('rebuild')`)
	}

	// One-time migration: normalize stored embeddings to unit length so cosine
	// similarity reduces to a single dot product (Sprint 11.3). Idempotent —
	// normalizing an already-normalized vector is a no-op within float32 precision.
	st.normalizeStoredEmbeddings()

	if os.Getenv("SYNAPSES_QUERY_STATS") == "1" {
		st.CollectQueryStats(os.Stderr)
	}

	return st, nil
}

// openSQLiteDB opens a single SQLite DB with WAL mode and busy timeout.
//
// Pragmas are embedded in the DSN via `_pragma=` rather than run as explicit
// EXEC calls. This is critical for correctness with pooled connections: every
// connection opened by the database/sql pool inherits the settings, not just
// the first. Without DSN-level pragmas, lazily-opened connections get default
// settings (no busy_timeout) and fail immediately with SQLITE_BUSY on write
// contention instead of waiting the configured 5 s.
func openSQLiteDB(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create parent dir for %s: %w", path, err)
	}
	// _pragma=journal_mode(WAL)       — enable WAL on every connection open.
	// _pragma=busy_timeout(5000)      — wait up to 5 s on write contention.
	// _pragma=synchronous(NORMAL)     — safe with WAL; 2x write throughput vs FULL.
	// _pragma=cache_size(-65536)      — 64 MB page cache per connection (vs 2 MB default).
	// _pragma=mmap_size(268435456)    — 256 MB memory-mapped I/O; OS shares pages across connections.
	// _pragma=temp_store(MEMORY)      — temp tables/indices in memory, not disk.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)&_pragma=cache_size(-65536)" +
		"&_pragma=mmap_size(268435456)&_pragma=temp_store(MEMORY)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db %s: %w", path, err)
	}
	// Start with 1 connection during schema initialization. modernc.org/sqlite
	// can deadlock when two connections race to initialize the same schema
	// (both calling _sqlite3InitOne while one holds a schema write-lock).
	// After store.Open() completes all migrations, this becomes the writer pool
	// (MaxOpenConns=1) and a separate reader pool (MaxOpenConns=8) is opened.
	db.SetMaxOpenConns(1)
	return db, nil
}

// isDupColumnErr returns true if the error is a benign "duplicate column" error.
func isDupColumnErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "duplicate column") || strings.Contains(msg, "already has a column")
}

// runQuickCheck executes PRAGMA quick_check(1) and returns an error if the
// database is corrupt. Returns nil if the DB is healthy ("ok").
func runQuickCheck(db *sql.DB) error {
	rows, err := db.Query("PRAGMA quick_check(1)")
	if err != nil {
		return fmt.Errorf("pragma quick_check failed: %w", err)
	}
	defer rows.Close()
	var result string
	if rows.Next() {
		if scanErr := rows.Scan(&result); scanErr != nil {
			return fmt.Errorf("scan quick_check result: %w", scanErr)
		}
		if result != "ok" {
			return fmt.Errorf("%s", result)
		}
	}
	return rows.Err()
}

// recoverGraphDB handles a corrupt graph.db: deletes the file and its WAL/SHM
// sidecars, then verifies the deletion succeeded before the caller reopens.
// Graph data is fully regenerable from source — a fresh empty DB is safe.
// Returns an error if the file could not be removed (requires manual intervention).
func recoverGraphDB(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(path + suffix)
	}
	// Verify the main file was actually removed. If it still exists, the
	// caller would reopen the same corrupt file and get confusing schema errors.
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("could not remove corrupt graph.db at %s — delete the file manually and restart", path)
	}
	return nil
}

// recoverKnowledgeDB handles a corrupt knowledge.db:
//  1. Renames to knowledge.db.corrupt to preserve data for manual recovery
//  2. Falls back to deletion if rename fails (permissions, etc.)
//  3. Always cleans up WAL/SHM sidecars — a leftover -wal file would be
//     replayed by SQLite against the fresh empty DB, corrupting it on first open
//  4. Opens and returns a fresh empty DB at kPath
func recoverKnowledgeDB(kPath string) (*sql.DB, error) {
	// Rename the corrupt file so it can be inspected later.
	if err := os.Rename(kPath, kPath+".corrupt"); err != nil {
		// Rename failed — fall back to deletion so we can create a fresh file.
		// Log the rename error so the user knows the backup was not preserved.
		logutil.Warn("synapses: store: could not back up corrupt knowledge.db (%v) — deleting it\n", err)
		_ = os.Remove(kPath)
	}
	// Always remove WAL and SHM sidecars. A leftover -wal file from the corrupt
	// DB would be replayed by SQLite's WAL recovery against the new empty file.
	_ = os.Remove(kPath + "-wal")
	_ = os.Remove(kPath + "-shm")
	return openSQLiteDB(kPath)
}

// openReadOnlySQLiteDB opens a single SQLite DB in query-only mode using
// DSN-level pragmas so every connection opened by the pool inherits them —
// not just the first. This mirrors the approach used by openSQLiteDB for the
// primary databases (see its comment about MaxOpenConns and DSN pragmas).
func openReadOnlySQLiteDB(path string) (*sql.DB, error) {
	// _pragma=query_only(true): prevents accidental writes on any pool connection.
	// _pragma=busy_timeout(5000): wait up to 5 s on write contention instead of
	// failing immediately with SQLITE_BUSY — critical for federation stores that
	// share a WAL file with the primary MCP server.
	// Performance pragmas: 64 MB page cache, 256 MB mmap, temp tables in memory.
	dsn := path + "?_pragma=query_only(true)&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)&_pragma=cache_size(-65536)" +
		"&_pragma=mmap_size(268435456)&_pragma=temp_store(MEMORY)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	// Ping to force the first connection open and confirm the DSN pragmas are accepted.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// OpenReadOnly opens an existing SQLite store at path in query-only mode.
// It does NOT run schema migrations or FTS rebuilds, making it safe to call
// concurrently with a running MCP server.
func OpenReadOnly(path string) (*Store, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, fmt.Errorf("no index at %s — run 'synapses index' first", path)
	}
	graphDB, err := openReadOnlySQLiteDB(path)
	if err != nil {
		return nil, fmt.Errorf("open graph db (ro) %s: %w", path, err)
	}

	kPath := KnowledgePath(path)
	var knowledgeDB *sql.DB
	if _, err := os.Stat(kPath); err == nil {
		knowledgeDB, err = openReadOnlySQLiteDB(kPath)
		if err != nil {
			graphDB.Close()
			return nil, fmt.Errorf("open knowledge db (ro) %s: %w", kPath, err)
		}
	} else {
		// Knowledge DB doesn't exist yet (sibling not fully initialized).
		// Open an in-memory DB with knowledge schema so knowledge queries
		// return empty results instead of panicking on nil dereference.
		// This is critical: federation code calls FindEpisodesByNodeID,
		// RecallEpisodes, SearchMemories etc. on read-only sibling stores.
		knowledgeDB, err = sql.Open("sqlite", ":memory:")
		if err != nil {
			graphDB.Close()
			return nil, fmt.Errorf("open in-memory knowledge db: %w", err)
		}
		knowledgeDB.SetMaxOpenConns(1)
		_, _ = knowledgeDB.Exec(knowledgeSchema)
	}
	return &Store{graphDB: wrapSingleDB(graphDB), knowledgeDB: wrapSingleDB(knowledgeDB)}, nil
}

// rebuildFTS repopulates the nodes_fts table from the current nodes table.
// Called once on Open() for existing databases and after SaveGraph().
func (s *Store) rebuildFTS() error {
	tx, err := s.graphDB.Begin()
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
	err := s.graphDB.QueryRowContext(ctx,
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
	rows, err := s.graphDB.QueryContext(ctx,
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
	FromEntity        string `json:"from_entity"`
	ToProject         string `json:"to_project"`
	ToEntity          string `json:"to_entity"`
	ToFile            string `json:"to_file"`
	VerifiedCommit    string `json:"verified_commit"`
	VerifiedAt        string `json:"verified_at"`
	DetectionTier     string `json:"detection_tier"`
	VerifiedSignature string `json:"verified_signature"` // entity signature at verification time; used for fallback comparison
}

// crossDepCols is the column list for cross_project_deps queries.
const crossDepCols = `from_entity, to_project, to_entity, to_file, verified_commit, verified_at, detection_tier, verified_signature`

func scanCrossDep(scanner interface{ Scan(...interface{}) error }) (CrossProjectDep, error) {
	var d CrossProjectDep
	err := scanner.Scan(&d.FromEntity, &d.ToProject, &d.ToEntity, &d.ToFile,
		&d.VerifiedCommit, &d.VerifiedAt, &d.DetectionTier, &d.VerifiedSignature)
	return d, err
}

// GetCrossProjectDeps returns all cross-project dependencies for a local entity.
func (s *Store) GetCrossProjectDeps(fromEntity string) ([]CrossProjectDep, error) {
	rows, err := s.knowledgeDB.Query(
		`SELECT `+crossDepCols+` FROM cross_project_deps WHERE from_entity = ?`,
		fromEntity,
	)
	if err != nil {
		return nil, fmt.Errorf("get cross-project deps: %w", err)
	}
	defer rows.Close()

	var deps []CrossProjectDep
	for rows.Next() {
		d, err := scanCrossDep(rows)
		if err != nil {
			return nil, err
		}
		deps = append(deps, d)
	}
	return deps, rows.Err()
}

// GetCrossProjectDepsByProject returns all deps targeting a specific sibling project.
func (s *Store) GetCrossProjectDepsByProject(project string) ([]CrossProjectDep, error) {
	rows, err := s.knowledgeDB.Query(
		`SELECT `+crossDepCols+` FROM cross_project_deps WHERE to_project = ?`,
		project,
	)
	if err != nil {
		return nil, fmt.Errorf("get cross-project deps by project: %w", err)
	}
	defer rows.Close()

	var deps []CrossProjectDep
	for rows.Next() {
		d, err := scanCrossDep(rows)
		if err != nil {
			return nil, err
		}
		deps = append(deps, d)
	}
	return deps, rows.Err()
}

// UpsertCrossProjectDep inserts or updates a cross-project dependency.
func (s *Store) UpsertCrossProjectDep(dep CrossProjectDep) error {
	_, err := s.knowledgeDB.Exec(
		`INSERT INTO cross_project_deps (from_entity, to_project, to_entity, to_file, verified_commit, verified_at, detection_tier, verified_signature)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (from_entity, to_project, to_entity)
		 DO UPDATE SET to_file=excluded.to_file, verified_commit=excluded.verified_commit, verified_at=excluded.verified_at, detection_tier=excluded.detection_tier, verified_signature=excluded.verified_signature`,
		dep.FromEntity, dep.ToProject, dep.ToEntity, dep.ToFile, dep.VerifiedCommit, dep.VerifiedAt, dep.DetectionTier, dep.VerifiedSignature,
	)
	if err != nil {
		return fmt.Errorf("upsert cross-project dep: %w", err)
	}
	return nil
}

// DeleteCrossProjectDeps removes all cross-project deps for a local entity.
func (s *Store) DeleteCrossProjectDeps(fromEntity string) error {
	_, err := s.knowledgeDB.Exec(`DELETE FROM cross_project_deps WHERE from_entity = ?`, fromEntity)
	if err != nil {
		return fmt.Errorf("delete cross-project deps: %w", err)
	}
	return nil
}

// UpdateVerifiedCommit updates the verified_commit for a dependency after
// confirming the sibling entity hasn't drifted.
func (s *Store) UpdateVerifiedCommit(toProject, toEntity, newCommit string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.knowledgeDB.Exec(
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

	rows, err := s.graphDB.Query(ftsSQL, q, limit)
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
		pat := "%" + escapeLike(w) + "%"
		conds[i] = "(name LIKE ? ESCAPE '\\' OR doc LIKE ? ESCAPE '\\')"
		args = append(args, pat, pat)
	}
	args = append(args, limit)
	rows, err := s.graphDB.Query(fmt.Sprintf(`
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
// Each word becomes a quoted phrase prefix ("word"*) joined by OR so that
// "traverse BFS ego" finds any node matching ANY of the three tokens.
// Quoting prevents FTS5 keywords (NOT, AND, OR, NEAR) from being interpreted
// as operators when they appear in user-supplied input.
// Empty result means the query had no usable terms.
func sanitizeFTSQuery(q string) string {
	// Strip FTS5 special characters to prevent syntax errors.
	// Include backslash to prevent escape sequence injection.
	replacer := strings.NewReplacer(`"`, " ", `'`, " ", `(`, " ", `)`, " ", `:`, " ", `*`, " ", `.`, " ", `-`, " ", `/`, " ", `\`, " ")
	q = strings.TrimSpace(replacer.Replace(q))
	words := strings.Fields(q)
	if len(words) == 0 {
		return ""
	}
	terms := make([]string, len(words))
	for i, w := range words {
		// Quote each term so FTS5 treats it as a literal phrase, not an operator
		// keyword. "word"* is phrase-prefix syntax: matches any document
		// containing a token starting with the given characters.
		// Short words (≤2 chars) skip prefix to avoid broad noise matches.
		if len(w) > 2 {
			terms[i] = `"` + w + `"*`
		} else {
			terms[i] = `"` + w + `"`
		}
	}
	return strings.Join(terms, " OR ")
}

// Close releases all database connections (both reader and writer pools).
func (s *Store) Close() error {
	var firstErr error
	if s.graphDB != nil {
		if err := s.graphDB.Close(); err != nil {
			firstErr = err
		}
	}
	if s.knowledgeDB != nil {
		if err := s.knowledgeDB.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// GraphDB returns the underlying graph writer database connection. Used by
// callers that need direct *sql.DB access (e.g., tests, federation probes).
func (s *Store) GraphDB() *sql.DB { return s.graphDB.Writer() }

// KnowledgeDB returns the underlying knowledge writer database connection.
func (s *Store) KnowledgeDB() *sql.DB { return s.knowledgeDB.Writer() }

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
		rows, err := s.graphDB.Query(`EXPLAIN QUERY PLAN ` + p.sql)
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
// Safe to call concurrently — a built-in 23-hour debounce ensures at most one
// prune runs per day regardless of how many goroutines invoke it.
// Intended to be called at startup and then on a daily timer.
func (s *Store) PruneStaleData(retentionDays int) {
	s.lastPruneMu.Lock()
	if time.Since(s.lastPruneStaleAt) < 23*time.Hour {
		s.lastPruneMu.Unlock()
		return // already pruned recently; skip
	}
	s.lastPruneStaleAt = time.Now()
	s.lastPruneMu.Unlock()

	cutoff := time.Now().AddDate(0, 0, -retentionDays).Format(time.RFC3339)
	cutoffUnix := time.Now().AddDate(0, 0, -retentionDays).Unix()

	pruneExec := func(query string, args ...interface{}) {
		if _, err := s.knowledgeDB.Exec(query, args...); err != nil {
			logutil.Debug("synapses: store: prune exec failed (%q): %v\n", query[:min(len(query), 60)], err)
		}
	}

	// tool_calls: one row per MCP tool invocation — can reach millions.
	pruneExec(`DELETE FROM tool_calls WHERE created_at < ?`, cutoff)

	// agent_messages: created_at is stored as Unix INTEGER (see SendMessage).
	pruneExec(`DELETE FROM agent_messages WHERE created_at < ?`, cutoffUnix)

	// events: coordination/observability stream — pruned to retention window.
	pruneExec(`DELETE FROM events WHERE created_at < ?`, cutoff)

	// episodes: stored as Unix seconds (INTEGER).
	pruneExec(`DELETE FROM episodes WHERE created_at < ?`, cutoffUnix)

	// memories: honour their own expires_at field; capture count for lifecycle event.
	memNow := time.Now().UTC().Format(time.RFC3339)
	if res, execErr := s.knowledgeDB.Exec(`DELETE FROM memories WHERE expires_at != '' AND expires_at < ?`, memNow); execErr == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			_ = s.AppendEvent("knowledge_expired", "system", fmt.Sprintf(`{"count":%d}`, n))
		}
	} else {
		logutil.Debug("synapses: store: prune expired memories: %v\n", execErr)
	}
	pruneExec(`DELETE FROM memories WHERE tier = 'session_log' AND created_at < ?`, cutoff)

	// context_deliveries: instrumentation data for Sprint 11 feedback loop.
	// Rows older than retention window have been analyzed and have no further value.
	pruneExec(`DELETE FROM context_deliveries WHERE created_at < ?`, cutoffUnix)

	// proposals: resolved proposals have no further value after retention period.
	pruneExec(`DELETE FROM proposals WHERE status IN ('accepted','rejected','withdrawn') AND updated_at < ?`, cutoff)
	pruneExec(`DELETE FROM proposal_votes WHERE proposal_id NOT IN (SELECT id FROM proposals)`)

	// Cross-DB reconciliation for hard node_id references: annotations and quality_gaps
	// in knowledgeDB reference node IDs from graphDB, but there is no cross-database
	// transaction guaranteeing consistency (see the Store type comment for the full
	// eventual-consistency model). This daily pass collects current node IDs from
	// graphDB and removes knowledgeDB records that reference absent nodes.
	// The read and delete are not atomic — a node added between the two steps could
	// be briefly affected — but since the prune runs daily, any node absent at prune
	// time has been gone for 23+ hours and is permanently deleted, not a reindex transient.
	if s.graphDB != nil {
		nodeIDs := make(map[string]struct{})
		if rows, err := s.graphDB.Query(`SELECT id FROM nodes`); err == nil {
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					nodeIDs[id] = struct{}{}
				}
			}
			rows.Close()
		}
		if len(nodeIDs) > 0 {
			// Delete stale annotations whose node no longer exists in the graph.
			// Only annotations already flagged stale=1 are candidates — un-flagged
			// annotations are preserved even during transient reindex windows.
			if annRows, err := s.knowledgeDB.Query(`SELECT id, node_id FROM annotations WHERE stale=1`); err == nil {
				var toDelete []string
				for annRows.Next() {
					var annID, nodeID string
					if annRows.Scan(&annID, &nodeID) == nil {
						if _, exists := nodeIDs[nodeID]; !exists {
							toDelete = append(toDelete, annID)
						}
					}
				}
				annRows.Close()
				for _, id := range toDelete {
					pruneExec(`DELETE FROM annotations WHERE id = ?`, id)
				}
			}

			// Delete quality gaps whose node no longer exists in the graph.
			// A quality gap for a deleted or renamed node is misleading — the code
			// it described no longer exists at that identity. Unlike annotations,
			// quality_gaps has no stale flag: any gap referencing an absent node is
			// by definition stale. The prune runs daily, so transient reindex windows
			// (seconds) are not a concern — if the node is still absent after 23+ hours,
			// it is permanently gone.
			if gapRows, err := s.knowledgeDB.Query(`SELECT id, node_id FROM quality_gaps WHERE status = 'open'`); err == nil {
				var toDelete []string
				for gapRows.Next() {
					var gapID, nodeID string
					if gapRows.Scan(&gapID, &nodeID) == nil {
						if _, exists := nodeIDs[nodeID]; !exists {
							toDelete = append(toDelete, gapID)
						}
					}
				}
				gapRows.Close()
				for _, id := range toDelete {
					pruneExec(`DELETE FROM quality_gaps WHERE id = ?`, id)
				}
			}
		}
	}

	// SQLite housekeeping for both databases.
	pruneExec(`PRAGMA optimize`)
	if s.graphDB != nil {
		if _, err := s.graphDB.Exec(`PRAGMA optimize`); err != nil {
			logutil.Debug("synapses: store: prune graphDB PRAGMA optimize: %v\n", err)
		}
	}
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
	_, err := s.knowledgeDB.Exec(
		`INSERT OR REPLACE INTO web_cache(url, content, fetched_at, ttl_hours)
		 VALUES (?, ?, ?, ?)`,
		url, content, time.Now().UTC().Format(time.RFC3339), ttlHours,
	)
	return err
}

// GetWebCache returns the cached entry for url, or (nil, false) if missing or expired.
func (s *Store) GetWebCache(url string) (*WebCacheEntry, bool) {
	row := s.knowledgeDB.QueryRow(
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
	_, err := s.knowledgeDB.Exec(`DELETE FROM web_cache WHERE url LIKE ?`, prefix+"%")
	return err
}

// PruneExpiredWebCache removes all entries whose TTL has elapsed.
func (s *Store) PruneExpiredWebCache() error {
	_, err := s.knowledgeDB.Exec(
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
	if fanRows, err := s.graphDB.Query(`SELECT to_id, COUNT(*) FROM edges WHERE type='CALLS' GROUP BY to_id`); err == nil {
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
	if sigRows, err := s.graphDB.Query(`SELECT id, signature FROM nodes WHERE signature != ''`); err == nil {
		defer sigRows.Close()
		for sigRows.Next() {
			var nid, sig string
			if sigRows.Scan(&nid, &sig) == nil {
				oldSigs[nid] = sig
			}
		}
	}

	tx, err := s.graphDB.Begin()
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
		if fanRows, err := s.graphDB.Query(`SELECT to_id, COUNT(*) FROM edges WHERE type='CALLS' GROUP BY to_id`); err == nil {
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

// SaveGraphDelta persists only the nodes and edges for changedFile,
// replacing any existing data for that file in a single transaction.
// This reduces write amplification from O(total graph) to O(changed file)
// — roughly 95% fewer writes for a typical single-file edit.
//
// Incoming edges to nodes that were deleted from changedFile are also removed
// (to prevent unbounded accumulation over long sessions). Incoming edges to
// nodes that still exist in changedFile are preserved — their from_id is in
// another file and unaffected by this delta.
//
// If changedFile is empty, falls back to the full SaveGraph.
func (s *Store) SaveGraphDelta(changedFile string, g *graph.Graph) error {
	if changedFile == "" {
		return s.SaveGraph(g)
	}

	primaryPrefix := g.RepoID() + "::"

	// Get new nodes for changedFile from the in-memory graph.
	allNewNodes := g.NodesForFile(changedFile)
	newNodes := make([]*graph.Node, 0, len(allNewNodes))
	newNodeIDSet := make(map[string]bool, len(allNewNodes))
	for _, n := range allNewNodes {
		if strings.HasPrefix(string(n.ID), primaryPrefix) {
			newNodes = append(newNodes, n)
			newNodeIDSet[string(n.ID)] = true
		}
	}

	// Query old node IDs and signatures for changedFile from the DB.
	// Must be done before the transaction so the subquery-based DELETEs
	// can use the nodes table while it still holds the old rows.
	oldNodeIDs := make([]string, 0)
	oldSigs := make(map[string]string)
	if rows, err := s.graphDB.Query(`SELECT id, signature FROM nodes WHERE file = ?`, changedFile); err == nil {
		defer rows.Close()
		for rows.Next() {
			var nid, sig string
			if rows.Scan(&nid, &sig) == nil {
				oldNodeIDs = append(oldNodeIDs, nid)
				if sig != "" {
					oldSigs[nid] = sig
				}
			}
		}
	}

	// Snapshot CALLS fan-in for changedFile nodes (for stale annotation detection).
	oldFanIn := make(map[string]int)
	if fanRows, err := s.graphDB.Query(`
		SELECT e.to_id, COUNT(*)
		FROM edges e
		WHERE e.type = 'CALLS'
		  AND e.to_id IN (SELECT id FROM nodes WHERE file = ?)
		GROUP BY e.to_id
	`, changedFile); err == nil {
		defer fanRows.Close()
		for fanRows.Next() {
			var nid string
			var cnt int
			if fanRows.Scan(&nid, &cnt) == nil {
				oldFanIn[nid] = cnt
			}
		}
	}

	// Collect outgoing edges for new changedFile nodes from the in-memory graph.
	// OutEdgesForFile is O(total_nodes + file_out_edges), avoiding the O(all_edges)
	// scan that AllEdges() + filter would require.
	var newEdges []*graph.Edge
	if len(newNodes) > 0 {
		newEdges = g.OutEdgesForFile(changedFile)
	}

	tx, err := s.graphDB.Begin()
	if err != nil {
		return fmt.Errorf("delta: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Delete FTS entries for old changedFile nodes in batches.
	// The subquery approach is avoided for FTS5 virtual tables;
	// batched IN clauses are more reliable across SQLite FTS5 implementations.
	const ftsDeleteBatch = 900
	for i := 0; i < len(oldNodeIDs); i += ftsDeleteBatch {
		batch := oldNodeIDs[i:min(i+ftsDeleteBatch, len(oldNodeIDs))]
		ph := strings.Repeat("?,", len(batch)-1) + "?"
		args := make([]interface{}, len(batch))
		for j, id := range batch {
			args[j] = id
		}
		if _, err := tx.Exec(`DELETE FROM nodes_fts WHERE node_id IN (`+ph+`)`, args...); err != nil {
			return fmt.Errorf("delta: clear fts: %w", err)
		}
	}

	// Delete outgoing edges from old changedFile nodes. The subquery is evaluated
	// before the nodes DELETE below, so it still finds the old node IDs.
	if _, err := tx.Exec(
		`DELETE FROM edges WHERE from_id IN (SELECT id FROM nodes WHERE file = ?)`, changedFile,
	); err != nil {
		return fmt.Errorf("delta: delete outgoing edges: %w", err)
	}

	// Delete incoming edges to nodes that were REMOVED from changedFile (not just
	// changed). Without this, edges from other files pointing to deleted functions
	// accumulate as dangling rows indefinitely — unlike full SaveGraph which wipes
	// everything. Identified by: old node IDs that are absent from the new graph.
	//
	// Batched IN clause (900 items/batch) avoids SQLITE_LIMIT_VARIABLE_NUMBER.
	// For typical file edits (<100 node deletions), this is a single fast query.
	var deletedNodeIDs []string
	for _, id := range oldNodeIDs {
		if !newNodeIDSet[id] {
			deletedNodeIDs = append(deletedNodeIDs, id)
		}
	}
	const incomingDeleteBatch = 900
	for i := 0; i < len(deletedNodeIDs); i += incomingDeleteBatch {
		batch := deletedNodeIDs[i:min(i+incomingDeleteBatch, len(deletedNodeIDs))]
		ph := strings.Repeat("?,", len(batch)-1) + "?"
		args := make([]interface{}, len(batch))
		for j, id := range batch {
			args[j] = id
		}
		if _, err := tx.Exec(`DELETE FROM edges WHERE to_id IN (`+ph+`)`, args...); err != nil {
			return fmt.Errorf("delta: delete incoming edges to removed nodes: %w", err)
		}
	}

	// Delete old nodes for changedFile.
	if _, err := tx.Exec(`DELETE FROM nodes WHERE file = ?`, changedFile); err != nil {
		return fmt.Errorf("delta: delete nodes: %w", err)
	}

	// Insert new nodes.
	nodeStmt, err := tx.Prepare(`
		INSERT INTO nodes (id, type, name, package, file, line, exported, metadata, doc, signature, line_count, stable_id, provenance, domain, prev_signature)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("delta: prepare node stmt: %w", err)
	}
	defer nodeStmt.Close()

	for _, n := range newNodes {
		doc := n.Metadata["doc"]
		sig := n.Metadata["signature"]
		lineCount := 0
		if lc, err := strconv.Atoi(n.Metadata["line_count"]); err == nil {
			lineCount = lc
		}
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
		prov := string(n.Provenance)
		if prov == "" {
			prov = "user-authored"
		}
		domain := string(n.Domain)
		if domain == "" {
			domain = "code"
		}
		// prev_signature: set to old sig when it changed, '' when new or unchanged.
		prevSig := ""
		if oldSig, existed := oldSigs[string(n.ID)]; existed && oldSig != sig {
			prevSig = oldSig
		}
		if _, err := nodeStmt.Exec(
			string(n.ID), string(n.Type),
			n.Name, n.Package, n.File, n.Line,
			exported, string(meta), doc, sig, lineCount, n.StableID, prov, domain, prevSig,
		); err != nil {
			return fmt.Errorf("delta: insert node %s: %w", n.ID, err)
		}
	}

	// Insert FTS entries for new nodes (skip file/package nodes — not useful for search).
	ftsStmt, err := tx.Prepare(`INSERT INTO nodes_fts (node_id, name, split_name, signature, doc) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("delta: prepare fts stmt: %w", err)
	}
	defer ftsStmt.Close()
	for _, n := range newNodes {
		if n.Type == graph.NodeFile || n.Type == graph.NodePackage {
			continue
		}
		doc := n.Metadata["doc"]
		sig := n.Metadata["signature"]
		if _, err := ftsStmt.Exec(string(n.ID), n.Name, splitCamelCase(n.Name), sig, doc); err != nil {
			return fmt.Errorf("delta: insert fts node %s: %w", n.ID, err)
		}
	}

	// Insert new outgoing edges for changedFile nodes.
	edgeStmt, err := tx.Prepare(`INSERT OR IGNORE INTO edges (from_id, to_id, type) VALUES (?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("delta: prepare edge stmt: %w", err)
	}
	defer edgeStmt.Close()
	for _, e := range newEdges {
		// Skip edges originating from linked-project nodes.
		if !strings.HasPrefix(string(e.From), primaryPrefix) {
			continue
		}
		if _, err := edgeStmt.Exec(string(e.From), string(e.To), string(e.Type)); err != nil {
			return fmt.Errorf("delta: insert edge %s→%s: %w", e.From, e.To, err)
		}
	}

	// Update metadata. Count nodes/edges directly from the DB (inside the
	// transaction) so that federated/linked-project nodes — which live only in
	// memory and are not written to the primary DB — are excluded. This matches
	// what LoadGraph reads back and avoids inflated counts.
	now := time.Now().UTC().Format(time.RFC3339)
	var nodeCount, edgeCount, fileCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodeCount); err != nil {
		return fmt.Errorf("delta: count nodes: %w", err)
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM edges`).Scan(&edgeCount); err != nil {
		return fmt.Errorf("delta: count edges: %w", err)
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM nodes WHERE type = 'file'`).Scan(&fileCount); err != nil {
		return fmt.Errorf("delta: count files: %w", err)
	}
	metaKVs := [][2]string{
		{"repo_id", g.RepoID()},
		{"repo_root", g.Root()},
		{"saved_at", now},
		{"node_count", strconv.Itoa(nodeCount)},
		{"edge_count", strconv.Itoa(edgeCount)},
		{"file_count", strconv.Itoa(fileCount)},
	}
	for _, kv := range metaKVs {
		if _, err := tx.Exec(
			`INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`, kv[0], kv[1],
		); err != nil {
			return fmt.Errorf("delta: save meta %s: %w", kv[0], err)
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// GAP-3: Post-commit stale annotation detection for changedFile nodes.
	// Same logic as SaveGraph but scoped to changedFile via subquery.
	go func() {
		newFanIn := make(map[string]int)
		if fanRows, err := s.graphDB.Query(`
			SELECT e.to_id, COUNT(*)
			FROM edges e
			WHERE e.type = 'CALLS'
			  AND e.to_id IN (SELECT id FROM nodes WHERE file = ?)
			GROUP BY e.to_id
		`, changedFile); err == nil {
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
				staleIDs = append(staleIDs, nid)
				continue
			}
			delta := float64(newCnt - oldCnt)
			if delta < 0 {
				delta = -delta
			}
			if delta/float64(oldCnt) > threshold {
				staleIDs = append(staleIDs, nid)
			}
		}
		if len(staleIDs) > 0 {
			_ = s.MarkAnnotationsStale(staleIDs)
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
	metaRows, err := s.graphDB.Query(`SELECT key, value FROM meta WHERE key IN ('repo_id', 'repo_root')`)
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
	rows, err := s.graphDB.Query(`
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
		if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
			logutil.Debug("synapses: store: unmarshal metadata for node %q (%s): %v\n", id, name, err)
		}
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
	erows, err := s.graphDB.Query(`SELECT from_id, to_id, type FROM edges`)
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
	// Use suffix matching: '%' || escapedFile handles both absolute and relative paths.
	// escapeLike prevents LIKE metacharacters in user-supplied file names (e.g. from
	// verify_implementation files_written) from wildcarding unrelated nodes.
	escapedFile := escapeLike(file)
	rows, err := s.graphDB.Query(`
		SELECT id, name, type, file, line, signature, prev_signature
		FROM nodes
		WHERE (file = ? OR file LIKE '%' || ? ESCAPE '\')
		  AND prev_signature != ''
		  AND type IN ('function', 'method', 'struct', 'interface')
	`, file, escapedFile)
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
	row := s.graphDB.QueryRow(`SELECT value FROM meta WHERE key = 'saved_at'`)
	if err := row.Scan(&raw); err == sql.ErrNoRows {
		return time.Time{}, nil
	} else if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339, raw)
}

// HasTable reports whether the given table exists in either the graph or
// knowledge SQLite database. Used by federation to check if a sibling store
// has specific tables (e.g., episodes, episodes_fts).
func (s *Store) HasTable(name string) bool {
	q := `SELECT COUNT(*) FROM sqlite_master WHERE type IN ('table','view') AND name = ?`
	var n int
	if s.graphDB != nil {
		if err := s.graphDB.QueryRow(q, name).Scan(&n); err == nil && n > 0 {
			return true
		}
	}
	if s.knowledgeDB != nil {
		if err := s.knowledgeDB.QueryRow(q, name).Scan(&n); err == nil && n > 0 {
			return true
		}
	}
	return false
}

// Stat reads only the meta key-value table and returns a ProjectStat without
// loading any nodes or edges. This is the fast path used by 'synapses list'.
func (s *Store) Stat(dbPath string) (*ProjectStat, error) {
	rows, err := s.graphDB.Query(`SELECT key, value FROM meta`)
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
		// Skip knowledge DB files — they are opened as part of their graph DB.
		if strings.HasSuffix(e.Name(), "_knowledge.db") {
			continue
		}
		dbPath := filepath.Join(dir, e.Name())
		// Use OpenReadOnly so ScanAll can run while the daemon holds the database.
		// Open() runs DDL migrations (requires exclusive lock) and would deadlock
		// against a live daemon. OpenReadOnly() is sufficient — Stat() only reads.
		st, err := OpenReadOnly(dbPath)
		if err != nil {
			logutil.Warn("synapses: skipping corrupt db %s: %v\n", e.Name(), err)
			continue
		}
		stat, err := st.Stat(dbPath)
		st.Close()
		if err != nil {
			logutil.Warn("synapses: skipping corrupt db %s: %v\n", e.Name(), err)
			continue
		}
		if stat == nil {
			continue // empty / uninitialised DB — not an error
		}
		// Skip stale entries: path no longer exists on disk (e.g. test temp dirs
		// that were indexed and then cleaned up by the OS or test framework).
		if stat.RepoRoot == "" {
			continue
		}
		if _, err := os.Stat(stat.RepoRoot); os.IsNotExist(err) {
			continue
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
	rows, err := s.graphDB.Query(`SELECT path, mod_time FROM file_hashes`)
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
	tx, err := s.graphDB.Begin()
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
	_, err := s.graphDB.Exec(
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
	tx, err := s.graphDB.Begin()
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
	rows, err := s.graphDB.Query(`SELECT caller_id, caller_file, pkg_alias, func_name FROM call_sites`)
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
	tx, err := s.graphDB.Begin()
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
	_, err := s.knowledgeDB.Exec(`
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
	rows, err := s.knowledgeDB.Query(`
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
	tx, err := s.knowledgeDB.Begin()
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
	rows, err := s.knowledgeDB.Query(
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
		rows, err = s.knowledgeDB.Query(`
            SELECT id, rule_id, severity, from_node, to_node, edge_type, first_seen, last_seen, occurrences
            FROM violation_log WHERE rule_id = ? ORDER BY last_seen DESC LIMIT ?`,
			ruleID, limit)
	} else {
		rows, err = s.knowledgeDB.Query(`
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
	_, err := s.knowledgeDB.Exec(`
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
	// escapeLike guards against LIKE metacharacters in user-supplied file names.
	escapedFile := escapeLike(f.File)
	fileWhere := func(extra string) string {
		return base + ` WHERE (node_id LIKE ? ESCAPE '\' OR node_id LIKE ? ESCAPE '\')` + extra + ` ORDER BY` + severityOrder
	}

	switch {
	// Compound: NodeID + Severity (most specific — must come before single-field cases).
	case f.NodeID != "" && f.Severity != "" && status != "all":
		rows, err = s.knowledgeDB.Query(base+` WHERE node_id = ? AND severity = ? AND status = ? ORDER BY`+severityOrder, f.NodeID, f.Severity, status)
	case f.NodeID != "" && f.Severity != "":
		rows, err = s.knowledgeDB.Query(base+` WHERE node_id = ? AND severity = ? ORDER BY`+severityOrder, f.NodeID, f.Severity)
	// Compound: File + Severity.
	case f.File != "" && f.Severity != "" && status != "all":
		rows, err = s.knowledgeDB.Query(fileWhere(` AND severity = ? AND status = ?`), "%/"+escapedFile+"::%", "%::"+escapedFile+"::%", f.Severity, status)
	case f.File != "" && f.Severity != "":
		rows, err = s.knowledgeDB.Query(fileWhere(` AND severity = ?`), "%/"+escapedFile+"::%", "%::"+escapedFile+"::%", f.Severity)
	// Single-field cases.
	case f.NodeID != "" && status != "all":
		rows, err = s.knowledgeDB.Query(base+` WHERE node_id = ? AND status = ? ORDER BY`+severityOrder, f.NodeID, status)
	case f.NodeID != "":
		rows, err = s.knowledgeDB.Query(base+` WHERE node_id = ? ORDER BY`+severityOrder, f.NodeID)
	case f.File != "" && status != "all":
		rows, err = s.knowledgeDB.Query(fileWhere(` AND status = ?`), "%/"+escapedFile+"::%", "%::"+escapedFile+"::%", status)
	case f.File != "":
		rows, err = s.knowledgeDB.Query(fileWhere(``), "%/"+escapedFile+"::%", "%::"+escapedFile+"::%")
	case f.Severity != "" && status != "all":
		rows, err = s.knowledgeDB.Query(base+` WHERE severity = ? AND status = ? ORDER BY`+severityOrder, f.Severity, status)
	case status != "all":
		rows, err = s.knowledgeDB.Query(base+` WHERE status = ? ORDER BY`+severityOrder, status)
	default:
		rows, err = s.knowledgeDB.Query(base + ` ORDER BY` + severityOrder)
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
	row := s.knowledgeDB.QueryRow(
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
		_, err := s.knowledgeDB.Exec(`
			INSERT INTO agents (id, last_seen) VALUES (?, ?)
			ON CONFLICT(id) DO UPDATE SET last_seen = excluded.last_seen`,
			id, now,
		)
		return err
	}
	_, err := s.knowledgeDB.Exec(`
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
	_, err := s.knowledgeDB.Exec(
		`UPDATE agents SET current_task_id = '', current_task_title = '' WHERE id = ?`,
		agentID,
	)
	return err
}

// GetAgents returns all known agents ordered by last_seen descending.
// Presence is computed from last_seen: active (≤5min), idle (5–15min), inactive (>15min).
func (s *Store) GetAgents() ([]Agent, error) {
	rows, err := s.knowledgeDB.Query(`
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

// CountActiveAgents returns the number of agents (excluding agentID) seen
// within the last 15 minutes.
func (s *Store) CountActiveAgents(excludeAgentID string) (int, error) {
	cutoff := time.Now().UTC().Add(-15 * time.Minute).Format(time.RFC3339)
	var n int
	err := s.knowledgeDB.QueryRow(`
		SELECT COUNT(*) FROM agents
		WHERE last_seen > ? AND id != ?`,
		cutoff, excludeAgentID,
	).Scan(&n)
	return n, err
}

// CountIndexedFiles returns the number of files currently tracked in the index.
func (s *Store) CountIndexedFiles() (int, error) {
	var n int
	err := s.graphDB.QueryRow(`SELECT COUNT(*) FROM file_hashes`).Scan(&n)
	return n, err
}


// ── Agent Context Profile ────────────────────────────────────────────────────

// GetAgentContext retrieves the context profile for the given agent.
// Returns nil if no profile exists yet (first session).
func (s *Store) GetAgentContext(agentID string) (*AgentContext, error) {
	var ac AgentContext
	err := s.knowledgeDB.QueryRow(
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
	_, err := s.knowledgeDB.Exec(`
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

	tx, err := s.knowledgeDB.Begin()
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

	rows, err := s.knowledgeDB.Query(query, args...)
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
		_ = s.knowledgeDB.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM events`).Scan(&latestSeq)
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
	_, err := s.knowledgeDB.Exec(
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
	result, err := s.knowledgeDB.Exec(
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
	_, err := s.knowledgeDB.Exec(
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
	rows, err := s.knowledgeDB.Query(
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
	_, err := s.knowledgeDB.Exec(
		`UPDATE annotations SET stale=1 WHERE node_id IN (`+placeholders+`) AND stale=0`,
		args...,
	)
	return err
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
	_, _ = s.knowledgeDB.Exec(
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
	rows, err := s.knowledgeDB.Query(`
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
	_, err := s.graphDB.Exec(
		`INSERT OR REPLACE INTO meta (key, value) VALUES ('graph_snapshot', ?)`,
		blob,
	)
	return err
}

// LoadIndexSnapshot returns the raw BLOB previously saved by SaveIndexSnapshot,
// or (nil, nil) if no snapshot exists.
func (s *Store) LoadIndexSnapshot() ([]byte, error) {
	var blob []byte
	err := s.graphDB.QueryRow(
		`SELECT value FROM meta WHERE key = 'graph_snapshot'`,
	).Scan(&blob)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return blob, err
}
