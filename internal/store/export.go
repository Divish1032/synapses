package store

import (
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
	ProjectID  string `json:"project_id"`

	// Core knowledge
	Memories        []Memory           `json:"memories"`
	MemoryVersions  []ExportedMemVer   `json:"memory_versions"`
	MemoryAnchors   []ExportedMemAnchor `json:"memory_anchors"`
	MemoryEmbeddings []ExportedMemEmbed `json:"memory_embeddings"`

	Episodes    []Episode      `json:"episodes"`
	DynamicRules []ExportedRule `json:"dynamic_rules"`
	Annotations []Annotation   `json:"annotations"`
	QualityGaps []QualityGap   `json:"quality_gaps"`

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

// ExportedMemVer is a stripped-down view of MemoryVersion safe for export.
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
// portability. The original BLOB (little-endian float32 array) is preserved
// verbatim so embeddings can be re-imported without re-computation.
type ExportedMemEmbed struct {
	MemoryID    string `json:"memory_id"`
	Model       string `json:"model"`
	EmbeddingB64 string `json:"embedding_b64"` // base64-encoded float32 BLOB
	ContentHash string `json:"content_hash"`
	EmbeddedAt  int64  `json:"embedded_at"` // Unix seconds
}

// ExportedRule captures a dynamic architectural rule in a portable form.
// Mirrors the dynamic_rules table without internal DB columns.
type ExportedRule struct {
	ID                string `json:"id"`
	Description       string `json:"description"`
	Severity          string `json:"severity"`
	FromFilePattern   string `json:"from_file_pattern,omitempty"`
	ToFilePattern     string `json:"to_file_pattern,omitempty"`
	FromType          string `json:"from_type,omitempty"`
	ToType            string `json:"to_type,omitempty"`
	EdgeType          string `json:"edge_type,omitempty"`
	ToNamePattern     string `json:"to_name_pattern,omitempty"`
	RuleType          string `json:"rule_type,omitempty"`
	PathPattern       string `json:"path_pattern,omitempty"` // comma-separated EdgeType list
	CreatedAt         string `json:"created_at"`
	UpdatedAt         string `json:"updated_at"`
}

// ExportKnowledge serializes all durable knowledge to a portable snapshot.
// It runs four parallel queries plus three sequential ones — all read-only.
// Intentionally excluded: graph nodes/edges (regenerable), file_hashes
// (ephemeral), tool_calls (analytics), web_cache (transient), sessions,
// agent_messages, events (operational logs).
func (s *Store) ExportKnowledge(projectID string) (*KnowledgeExport, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	// ── 1. Memories (cap at 10 000 matching the row cap constant) ──────────
	memories, err := s.exportMemories()
	if err != nil {
		return nil, fmt.Errorf("export memories: %w", err)
	}

	// ── 2. Memory versions ────────────────────────────────────────────────
	versions, err := s.exportMemoryVersions()
	if err != nil {
		return nil, fmt.Errorf("export memory versions: %w", err)
	}

	// ── 3. Memory anchors ─────────────────────────────────────────────────
	anchors, err := s.exportMemoryAnchors()
	if err != nil {
		return nil, fmt.Errorf("export memory anchors: %w", err)
	}

	// ── 4. Memory embeddings (base64-encoded BLOBs) ───────────────────────
	embeds, err := s.exportMemoryEmbeddings()
	if err != nil {
		return nil, fmt.Errorf("export memory embeddings: %w", err)
	}

	// ── 5. Episodes ────────────────────────────────────────────────────────
	episodes, err := s.exportEpisodes()
	if err != nil {
		return nil, fmt.Errorf("export episodes: %w", err)
	}

	// ── 6. Dynamic rules (includes rule_type and path_pattern) ────────────
	rules, err := s.exportDynamicRules()
	if err != nil {
		return nil, fmt.Errorf("export dynamic rules: %w", err)
	}

	// ── 7. Annotations ────────────────────────────────────────────────────
	annotations, err := s.exportAnnotations()
	if err != nil {
		return nil, fmt.Errorf("export annotations: %w", err)
	}

	// ── 8. Quality gaps (all statuses) ────────────────────────────────────
	gaps, err := s.GetGaps(GapFilter{Status: "all"})
	if err != nil {
		return nil, fmt.Errorf("export quality gaps: %w", err)
	}

	exp := &KnowledgeExport{
		Version:          "1",
		ExportedAt:       now,
		ProjectID:        projectID,
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

// exportMemories returns all memories (including stale/expired) up to the row cap.
// Uses the same column selection as scanMemories so the standard Memory struct
// can be scanned without modification.
func (s *Store) exportMemories() ([]Memory, error) {
	rows, err := s.knowledgeDB.Query(`
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
	return scanMemories(rows)
}

// exportMemoryVersions returns all historical memory version snapshots.
func (s *Store) exportMemoryVersions() ([]ExportedMemVer, error) {
	rows, err := s.knowledgeDB.Query(`
		SELECT id, memory_id, version, content, superseded_by, created_at, superseded_at
		FROM memory_versions
		ORDER BY memory_id, version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ExportedMemVer
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

// exportMemoryAnchors returns all memory→node staleness anchors.
func (s *Store) exportMemoryAnchors() ([]ExportedMemAnchor, error) {
	rows, err := s.knowledgeDB.Query(`
		SELECT memory_id, node_id, created_at
		FROM memory_anchors
		ORDER BY memory_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ExportedMemAnchor
	for rows.Next() {
		var a ExportedMemAnchor
		if err := rows.Scan(&a.MemoryID, &a.NodeID, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// exportMemoryEmbeddings returns all memory embedding vectors as base64-encoded BLOBs.
func (s *Store) exportMemoryEmbeddings() ([]ExportedMemEmbed, error) {
	rows, err := s.knowledgeDB.Query(`
		SELECT memory_id, model, embedding, content_hash, embedded_at
		FROM memory_embeddings
		ORDER BY memory_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ExportedMemEmbed
	for rows.Next() {
		var e ExportedMemEmbed
		var blob []byte
		if err := rows.Scan(&e.MemoryID, &e.Model, &blob, &e.ContentHash, &e.EmbeddedAt); err != nil {
			return nil, err
		}
		e.EmbeddingB64 = base64.StdEncoding.EncodeToString(blob)
		out = append(out, e)
	}
	return out, rows.Err()
}

// exportEpisodes returns all episodes ordered by creation time.
func (s *Store) exportEpisodes() ([]Episode, error) {
	rows, err := s.knowledgeDB.Query(`
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
	return scanEpisodes(rows)
}

// exportDynamicRules returns all dynamic rules including rule_type/path_pattern.
func (s *Store) exportDynamicRules() ([]ExportedRule, error) {
	rows, err := s.knowledgeDB.Query(`
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

	var out []ExportedRule
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

// exportAnnotations returns all annotations across all nodes.
func (s *Store) exportAnnotations() ([]Annotation, error) {
	rows, err := s.knowledgeDB.Query(`
		SELECT id, node_id, agent_id, note, created_at, source, stale
		FROM annotations
		ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Annotation
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
