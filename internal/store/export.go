package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"time"
)

// KnowledgeExport is the top-level envelope for a portable knowledge snapshot.
// It captures all durable, agent-generated knowledge for a project — ready for
// backup or migration. Graph nodes/edges are intentionally excluded (regenerable
// from source); transient tables (tool_calls, web_cache, sessions) are excluded.
type KnowledgeExport struct {
	// Schema version — bump when fields are added to enable future importers.
	Version    string `json:"version"`
	ExportedAt string `json:"exported_at"`
	// ProjectID is the FNV hash of the project root path used by this daemon.
	// It is machine-specific (depends on the absolute path). Include it for
	// identification/correlation, not as a stable cross-machine identifier.
	ProjectID string `json:"project_id"`

	// TTL note: expires_at values are preserved as-is from the DB for audit
	// fidelity. Importers should reset expires_at on re-import if they want
	// memories to remain active — past-dated entries will be pruned on the
	// next PruneStaleData run.
	TTLNote string `json:"ttl_note"`

	// Core knowledge (all slices are always non-null, even when empty).
	Memories         []Memory            `json:"memories"`
	MemoryVersions   []ExportedMemVer    `json:"memory_versions"`
	MemoryAnchors    []ExportedMemAnchor `json:"memory_anchors"`
	MemoryEmbeddings []ExportedMemEmbed  `json:"memory_embeddings"`

	Episodes     []Episode      `json:"episodes"`
	DynamicRules []ExportedRule `json:"dynamic_rules"`
	Annotations  []Annotation   `json:"annotations"`
	QualityGaps  []QualityGap   `json:"quality_gaps"`

	// Summary counts for quick inspection.
	Summary ExportSummary `json:"summary"`
}

// ExportSummary provides quick stats without parsing the full arrays.
type ExportSummary struct {
	MemoryCount          int `json:"memory_count"`
	MemoryVersionCount   int `json:"memory_version_count"`
	MemoryAnchorCount    int `json:"memory_anchor_count"`
	MemoryEmbeddingCount int `json:"memory_embedding_count"`
	EpisodeCount         int `json:"episode_count"`
	DynamicRuleCount     int `json:"dynamic_rule_count"`
	AnnotationCount      int `json:"annotation_count"`
	QualityGapCount      int `json:"quality_gap_count"`
}

// ExportedMemVer is a historical snapshot preserved when remember() deduplicates.
// Mirrors the memory_versions table fields.
type ExportedMemVer struct {
	ID           string `json:"id"`
	MemoryID     string `json:"memory_id"`
	Version      int    `json:"version"`
	Content      string `json:"content"`
	SupersededBy string `json:"superseded_by,omitempty"`
	CreatedAt    string `json:"created_at"`
	SupersededAt string `json:"superseded_at"`
}

// ExportedMemAnchor represents a memory→node staleness anchor.
type ExportedMemAnchor struct {
	MemoryID  string `json:"memory_id"`
	NodeID    string `json:"node_id"`
	CreatedAt string `json:"created_at"`
}

// ExportedMemEmbed holds a memory's embedding vector encoded as base64 for
// portability. The BLOB (normalized little-endian float32 array) is preserved
// verbatim so embeddings can be re-imported without re-computation.
// Stale embeddings (content changed since embedding was computed) are flagged
// — importers may choose to re-embed these rather than re-import them.
type ExportedMemEmbed struct {
	MemoryID     string `json:"memory_id"`
	Model        string `json:"model"`
	EmbeddingB64 string `json:"embedding_b64"` // base64-encoded normalized float32 BLOB
	ContentHash  string `json:"content_hash"`
	EmbeddedAt   int64  `json:"embedded_at"` // Unix seconds
	Stale        bool   `json:"stale,omitempty"`
}

// ExportedRule captures a dynamic architectural rule in a portable form.
// Mirrors the dynamic_rules table without internal DB columns.
type ExportedRule struct {
	ID              string `json:"id"`
	Description     string `json:"description"`
	Severity        string `json:"severity"`
	FromFilePattern string `json:"from_file_pattern,omitempty"`
	ToFilePattern   string `json:"to_file_pattern,omitempty"`
	FromType        string `json:"from_type,omitempty"`
	ToType          string `json:"to_type,omitempty"`
	EdgeType        string `json:"edge_type,omitempty"`
	ToNamePattern   string `json:"to_name_pattern,omitempty"`
	RuleType        string `json:"rule_type,omitempty"`
	PathPattern     string `json:"path_pattern,omitempty"` // comma-separated EdgeType list
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

// ExportKnowledge serializes all durable knowledge to an atomic, consistent
// snapshot. All 8 queries run inside a single DEFERRED read transaction so
// concurrent writes do not produce partially-visible state (e.g. an anchor
// row that references a memory not yet visible in the memories query).
//
// Intentionally excluded: graph nodes/edges (regenerable), file_hashes
// (ephemeral), tool_calls (analytics), web_cache (transient), sessions,
// agent_messages, events (operational logs).
//
// All slice fields in the returned KnowledgeExport are non-nil even when
// empty, so JSON consumers always receive arrays rather than null.
func (s *Store) ExportKnowledge(projectID string) (*KnowledgeExport, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	// Open a DEFERRED read transaction. All 8 export queries run inside it so
	// the snapshot is consistent — no write committed between queries can
	// partially appear in the result.
	tx, err := s.knowledgeDB.BeginTx(context.Background(), nil)
	if err != nil {
		return nil, fmt.Errorf("export begin tx: %w", err)
	}
	// Always roll back — this is a read-only transaction, so rollback is
	// equivalent to commit (no writes to discard).
	defer tx.Rollback() //nolint:errcheck

	memories, err := exportMemoriesTx(tx)
	if err != nil {
		return nil, fmt.Errorf("export memories: %w", err)
	}

	versions, err := exportMemoryVersionsTx(tx)
	if err != nil {
		return nil, fmt.Errorf("export memory versions: %w", err)
	}

	anchors, err := exportMemoryAnchorsTx(tx)
	if err != nil {
		return nil, fmt.Errorf("export memory anchors: %w", err)
	}

	embeds, err := exportMemoryEmbeddingsTx(tx)
	if err != nil {
		return nil, fmt.Errorf("export memory embeddings: %w", err)
	}

	episodes, err := exportEpisodesTx(tx)
	if err != nil {
		return nil, fmt.Errorf("export episodes: %w", err)
	}

	rules, err := exportDynamicRulesTx(tx)
	if err != nil {
		return nil, fmt.Errorf("export dynamic rules: %w", err)
	}

	annotations, err := exportAnnotationsTx(tx)
	if err != nil {
		return nil, fmt.Errorf("export annotations: %w", err)
	}

	gaps, err := exportQualityGapsTx(tx)
	if err != nil {
		return nil, fmt.Errorf("export quality gaps: %w", err)
	}

	exp := &KnowledgeExport{
		Version:    "1",
		ExportedAt: now,
		ProjectID:  projectID,
		TTLNote: "expires_at values are preserved from the database for audit fidelity. " +
			"Importers must reset expires_at on re-import to prevent immediate expiry on the next prune cycle.",
		Memories:         memories,
		MemoryVersions:   versions,
		MemoryAnchors:    anchors,
		MemoryEmbeddings: embeds,
		Episodes:         episodes,
		DynamicRules:     rules,
		Annotations:      annotations,
		QualityGaps:      gaps,
		Summary: ExportSummary{
			MemoryCount:          len(memories),
			MemoryVersionCount:   len(versions),
			MemoryAnchorCount:    len(anchors),
			MemoryEmbeddingCount: len(embeds),
			EpisodeCount:         len(episodes),
			DynamicRuleCount:     len(rules),
			AnnotationCount:      len(annotations),
			QualityGapCount:      len(gaps),
		},
	}
	return exp, nil
}

// exportMemoriesTx returns all memories (including stale/expired) up to the
// row cap. Runs within the caller's transaction for snapshot consistency.
func exportMemoriesTx(tx *sql.Tx) ([]Memory, error) {
	rows, err := tx.Query(`
		SELECT id, tier, content, entity_id, agent_id, task_id, tags,
		       created_at, expires_at, last_accessed_at, source,
		       importance, access_count
		FROM memories
		ORDER BY created_at
		LIMIT ?`, DefaultMaxMemoryRows)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanMemories(rows)
	if out == nil {
		out = []Memory{}
	}
	return out, err
}

// exportMemoryVersionsTx returns all historical memory version snapshots.
func exportMemoryVersionsTx(tx *sql.Tx) ([]ExportedMemVer, error) {
	rows, err := tx.Query(`
		SELECT id, memory_id, version, content, superseded_by, created_at, superseded_at
		FROM memory_versions
		ORDER BY memory_id, version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ExportedMemVer, 0)
	for rows.Next() {
		var v ExportedMemVer
		if err := rows.Scan(
			&v.ID, &v.MemoryID, &v.Version, &v.Content,
			&v.SupersededBy, &v.CreatedAt, &v.SupersededAt,
		); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// exportMemoryAnchorsTx returns all memory→node staleness anchors.
func exportMemoryAnchorsTx(tx *sql.Tx) ([]ExportedMemAnchor, error) {
	rows, err := tx.Query(`
		SELECT memory_id, node_id, created_at
		FROM memory_anchors
		ORDER BY memory_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ExportedMemAnchor, 0)
	for rows.Next() {
		var a ExportedMemAnchor
		if err := rows.Scan(&a.MemoryID, &a.NodeID, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// exportMemoryEmbeddingsTx returns all memory embedding vectors as base64-encoded
// BLOBs. Includes the stale flag so importers can skip re-importing stale vectors.
func exportMemoryEmbeddingsTx(tx *sql.Tx) ([]ExportedMemEmbed, error) {
	rows, err := tx.Query(`
		SELECT memory_id, model, embedding, content_hash, embedded_at, stale
		FROM memory_embeddings
		ORDER BY memory_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ExportedMemEmbed, 0)
	for rows.Next() {
		var e ExportedMemEmbed
		var blob []byte
		var staleInt int
		if err := rows.Scan(&e.MemoryID, &e.Model, &blob, &e.ContentHash, &e.EmbeddedAt, &staleInt); err != nil {
			return nil, err
		}
		e.EmbeddingB64 = base64.StdEncoding.EncodeToString(blob)
		e.Stale = staleInt != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

// exportEpisodesTx returns all episodes ordered by creation time.
func exportEpisodesTx(tx *sql.Tx) ([]Episode, error) {
	rows, err := tx.Query(`
		SELECT id, agent_id, project_id, created_at, episode_type, outcome,
		       trigger, decision, rationale, affected_files, affected_nodes,
		       tags, importance, promoted_rule
		FROM episodes
		ORDER BY created_at
		LIMIT ?`, DefaultMaxEpisodeRows)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanEpisodes(rows)
	if out == nil {
		out = []Episode{}
	}
	return out, err
}

// exportDynamicRulesTx returns all dynamic rules including rule_type/path_pattern.
func exportDynamicRulesTx(tx *sql.Tx) ([]ExportedRule, error) {
	rows, err := tx.Query(`
		SELECT id, description, severity,
		       from_file_pattern, to_file_pattern, from_type, to_type,
		       edge_type, to_name_pattern, rule_type, path_pattern,
		       created_at, updated_at
		FROM dynamic_rules
		ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ExportedRule, 0)
	for rows.Next() {
		var r ExportedRule
		if err := rows.Scan(
			&r.ID, &r.Description, &r.Severity,
			&r.FromFilePattern, &r.ToFilePattern, &r.FromType, &r.ToType,
			&r.EdgeType, &r.ToNamePattern, &r.RuleType, &r.PathPattern,
			&r.CreatedAt, &r.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// exportAnnotationsTx returns all annotations across all nodes.
func exportAnnotationsTx(tx *sql.Tx) ([]Annotation, error) {
	rows, err := tx.Query(`
		SELECT id, node_id, agent_id, note, created_at, source, stale
		FROM annotations
		ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Annotation, 0)
	for rows.Next() {
		var a Annotation
		var staleInt int
		if err := rows.Scan(
			&a.ID, &a.NodeID, &a.AgentID, &a.Note, &a.CreatedAt, &a.Source, &staleInt,
		); err != nil {
			return nil, err
		}
		a.Stale = staleInt != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

// exportQualityGapsTx returns ALL quality gaps regardless of status, bypassing
// the 1000-row cap that GetGaps applies for normal UI queries. Export must be
// complete — silently capping at 1000 would produce an incomplete backup.
func exportQualityGapsTx(tx *sql.Tx) ([]QualityGap, error) {
	rows, err := tx.Query(`
		SELECT id, node_id, gap_id, description, severity, status,
		       found_by, found_at, updated_at, fix_notes
		FROM quality_gaps
		ORDER BY
		    CASE severity
		        WHEN 'critical' THEN 4
		        WHEN 'high'     THEN 3
		        WHEN 'medium'   THEN 2
		        WHEN 'low'      THEN 1
		        ELSE 0
		    END DESC,
		    updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]QualityGap, 0)
	for rows.Next() {
		var g QualityGap
		if err := rows.Scan(
			&g.ID, &g.NodeID, &g.GapID, &g.Description,
			&g.Severity, &g.Status, &g.FoundBy, &g.FoundAt, &g.UpdatedAt, &g.FixNotes,
		); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
