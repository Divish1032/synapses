// Package store provides SQLite-backed persistence for the code graph.
// The graph is parsed once from source, saved here, and loaded in <1s on
// subsequent starts. Only files that changed (by mtime) are re-parsed.
package store

import (
    "crypto/sha1"
    "database/sql"
    "encoding/json"
    "fmt"
    "os"
    "path/filepath"
    "sort"
    "strconv"
    "strings"
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

-- Agent registry: tracks which agents have interacted with Synapses.
-- Self-declared identity (no auth); upserted on every call with agent_id.
CREATE TABLE IF NOT EXISTS agents (
    id        TEXT PRIMARY KEY,
    last_seen TEXT NOT NULL,
    metadata  TEXT NOT NULL DEFAULT '{}'
);

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

-- Shared annotations: agents can attach notes to graph nodes that are visible
-- to all other agents via get_context and find_entity responses.
CREATE TABLE IF NOT EXISTS annotations (
    id         TEXT PRIMARY KEY,
    node_id    TEXT NOT NULL,
    agent_id   TEXT NOT NULL DEFAULT '',
    note       TEXT NOT NULL,
    created_at TEXT NOT NULL
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
    entity      TEXT    NOT NULL DEFAULT '',
    duration_ms INTEGER NOT NULL DEFAULT 0,
    success     INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_tool_calls_tool ON tool_calls(tool_name);
CREATE INDEX IF NOT EXISTS idx_tool_calls_ts   ON tool_calls(created_at);
`

// Store wraps a SQLite database and provides graph serialisation.
type Store struct {
    db *sql.DB
}

// DefaultPath returns the canonical cache path for a repository root.
// It uses a user-scoped cache directory so the project tree stays clean.
func DefaultPath(repoRoot string) (string, error) {
    cacheDir, err := os.UserCacheDir()
    if err != nil {
        return "", fmt.Errorf("locate cache dir: %w", err)
    }
    dir := filepath.Join(cacheDir, "synapses", "cache")
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
        `ALTER TABLE tasks ADD COLUMN assigned_to TEXT NOT NULL DEFAULT ''`,
        `ALTER TABLE tasks ADD COLUMN last_updated_by TEXT NOT NULL DEFAULT ''`,
        // v1.0.4: Task dependencies
        `ALTER TABLE tasks ADD COLUMN depends_on TEXT NOT NULL DEFAULT '[]'`,
        // v0.2.0: Stable cross-project node identity (survives file renames).
        `ALTER TABLE nodes ADD COLUMN stable_id TEXT NOT NULL DEFAULT ''`,
    } {
        if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
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
// error. It performs a simple case-insensitive LIKE search on name and doc.
func (s *Store) likeSearch(query string, limit int) ([]SearchResult, error) {
    pattern := "%" + strings.ReplaceAll(query, "%", "\\%") + "%"
    rows, err := s.db.Query(`
        SELECT id, name, signature, doc
        FROM nodes
        WHERE name LIKE ? OR doc LIKE ?
        ORDER BY length(name) ASC
        LIMIT ?`, pattern, pattern, limit)
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
// Each word is double-quoted and suffixed with * for prefix matching — this
// enables multi-word phrase queries like "BFS carver" to find "CarveEgoGraph"
// (split_name = "Carve Ego Graph") by matching carv* → "Carve".
// Empty result means the query had no usable terms.
func sanitizeFTSQuery(q string) string {
    // Strip FTS5 special characters.
    replacer := strings.NewReplacer(`"`, " ", `'`, " ", `(`, " ", `)`, " ", `:`, " ", `*`, " ")
    q = strings.TrimSpace(replacer.Replace(q))
    words := strings.Fields(q)
    if len(words) == 0 {
        return ""
    }
    quoted := make([]string, len(words))
    for i, w := range words {
        // Prefix match (*) so "carver" finds "carve", "carving", "CarveEgoGraph".
        // Short words (≤2 chars) skip prefix to avoid broad noise matches.
        if len(w) > 2 {
            quoted[i] = `"` + w + `"*`
        } else {
            quoted[i] = `"` + w + `"`
        }
    }
    return strings.Join(quoted, " ")
}

// Close releases the database connection.
func (s *Store) Close() error {
    return s.db.Close()
}

// SaveGraph persists all nodes and edges of g, replacing any existing data.
// A metadata record stores the repo ID and the save timestamp.
func (s *Store) SaveGraph(g *graph.Graph) error {
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
        INSERT INTO nodes (id, type, name, package, file, line, exported, metadata, doc, signature, line_count, stable_id)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
        if _, err := nodeStmt.Exec(
            string(n.ID), string(n.Type),
            n.Name, n.Package, n.File, n.Line,
            exported, string(meta), doc, sig, lineCount, n.StableID,
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

    return tx.Commit()
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
        SELECT id, type, name, package, file, line, exported, metadata, doc, signature, line_count, stable_id FROM nodes
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
        )
        if err := rows.Scan(&id, &typ, &name, &pkg, &file, &line, &exported, &metaJSON, &doc, &sig, &lineCount, &stableID); err != nil {
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
            ID:       graph.NodeID(id),
            Type:     graph.NodeType(typ),
            Name:     name,
            Package:  pkg,
            File:     file,
            Line:     line,
            Exported: exported != 0,
            Metadata: meta,
            StableID: stableID,
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
    cacheDir, err := os.UserCacheDir()
    if err != nil {
        return nil, fmt.Errorf("locate cache dir: %w", err)
    }
    dir := filepath.Join(cacheDir, "synapses", "cache")

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

// UpsertDynamicRule persists a dynamic architectural rule to the store.
// If a rule with the same ID already exists it is fully replaced; otherwise
// a new row is inserted. The rule takes effect in-memory immediately — see
// Server.handleUpsertRule for the in-memory update that accompanies this call.
func (s *Store) UpsertDynamicRule(r config.Rule) error {
    now := time.Now().UTC().Format(time.RFC3339)
    _, err := s.db.Exec(`
        INSERT INTO dynamic_rules
            (id, description, severity, from_file_pattern, to_file_pattern,
             from_type, to_type, edge_type, to_name_pattern, created_at, updated_at)
        VALUES (?,?,?,?,?,?,?,?,?,?,?)
        ON CONFLICT(id) DO UPDATE SET
            description=excluded.description, severity=excluded.severity,
            from_file_pattern=excluded.from_file_pattern,
            to_file_pattern=excluded.to_file_pattern,
            from_type=excluded.from_type, to_type=excluded.to_type,
            edge_type=excluded.edge_type,
            to_name_pattern=excluded.to_name_pattern,
            updated_at=excluded.updated_at`,
        r.ID, r.Description, r.Severity,
        r.ForbiddenEdge.FromFilePattern, r.ForbiddenEdge.ToFilePattern,
        string(r.ForbiddenEdge.FromType), string(r.ForbiddenEdge.ToType),
        string(r.ForbiddenEdge.EdgeType), r.ForbiddenEdge.ToNamePattern,
        now, now,
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
            edge_type, to_name_pattern
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
            &r.ForbiddenEdge.ToNamePattern,
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

// Agent is a registered agent that has interacted with Synapses.
type Agent struct {
    ID       string `json:"id"`
    LastSeen string `json:"last_seen"`
    Metadata string `json:"metadata"`
}

// UpsertAgent records that an agent was seen. Called as a side-effect whenever
// an MCP tool receives a non-empty agent_id parameter.
func (s *Store) UpsertAgent(id string) error {
    now := time.Now().UTC().Format(time.RFC3339)
    _, err := s.db.Exec(`
        INSERT INTO agents (id, last_seen) VALUES (?, ?)
        ON CONFLICT(id) DO UPDATE SET last_seen = excluded.last_seen`,
        id, now,
    )
    return err
}

// GetAgents returns all known agents ordered by last_seen descending.
func (s *Store) GetAgents() ([]Agent, error) {
    rows, err := s.db.Query(`SELECT id, last_seen, metadata FROM agents ORDER BY last_seen DESC`)
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var agents []Agent
    for rows.Next() {
        var a Agent
        if err := rows.Scan(&a.ID, &a.LastSeen, &a.Metadata); err != nil {
            return nil, err
        }
        agents = append(agents, a)
    }
    return agents, rows.Err()
}

// CountIndexedFiles returns the number of files currently tracked in the index.
func (s *Store) CountIndexedFiles() (int, error) {
    var n int
    err := s.db.QueryRow(`SELECT COUNT(*) FROM file_hashes`).Scan(&n)
    return n, err
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
    return tx.Commit()
}

// GetEvents returns up to limit events with seq > sinceSeq, optionally filtered
// by event type. Returns the latest seq seen so the caller can use it as a cursor.
func (s *Store) GetEvents(sinceSeq int64, types []string, limit int) ([]Event, int64, error) {
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
}

// AddAnnotation attaches a note to a graph node.
func (s *Store) AddAnnotation(nodeID, agentID, note string) (string, error) {
    id := fmt.Sprintf("%x", time.Now().UnixNano())
    now := time.Now().UTC().Format(time.RFC3339)
    _, err := s.db.Exec(
        `INSERT INTO annotations (id, node_id, agent_id, note, created_at) VALUES (?, ?, ?, ?, ?)`,
        id, nodeID, agentID, note, now,
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
        `SELECT id, node_id, agent_id, note, created_at FROM annotations WHERE node_id IN (`+placeholders+`) ORDER BY created_at ASC`,
        args...,
    )
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    result := make(map[string][]Annotation)
    for rows.Next() {
        var a Annotation
        if err := rows.Scan(&a.ID, &a.NodeID, &a.AgentID, &a.Note, &a.CreatedAt); err != nil {
            return nil, err
        }
        result[a.NodeID] = append(result[a.NodeID], a)
    }
    return result, rows.Err()
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
func (s *Store) RecordToolCall(toolName, agentID, entity string, durationMs int64, success bool) {
    succ := 1
    if !success {
        succ = 0
    }
    _, _ = s.db.Exec(
        `INSERT INTO tool_calls(tool_name,agent_id,entity,duration_ms,success) VALUES(?,?,?,?,?)`,
        toolName, agentID, entity, durationMs, succ,
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

