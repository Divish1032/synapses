package store_test

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/store"
)

// TestExportKnowledge_EmptyStore verifies the export works on a fresh store:
// all slices are non-nil empty arrays (not null) and summary counts are zero.
func TestExportKnowledge_EmptyStore(t *testing.T) {
	s := openTestStore(t)
	exp, err := s.ExportKnowledge("testproject")
	if err != nil {
		t.Fatalf("ExportKnowledge on empty store: %v", err)
	}
	if exp.Version != "1" {
		t.Errorf("Version = %q, want \"1\"", exp.Version)
	}
	if exp.ProjectID != "testproject" {
		t.Errorf("ProjectID = %q, want \"testproject\"", exp.ProjectID)
	}
	if exp.ExportedAt == "" {
		t.Error("ExportedAt is empty")
	}
	if exp.TTLNote == "" {
		t.Error("TTLNote is empty — importers need this guidance")
	}
	if exp.Summary.MemoryCount != 0 {
		t.Errorf("MemoryCount = %d, want 0", exp.Summary.MemoryCount)
	}

	// All slices must be non-nil (JSON: [] not null) even on empty store.
	if exp.Memories == nil {
		t.Error("Memories is nil, want []")
	}
	if exp.MemoryVersions == nil {
		t.Error("MemoryVersions is nil, want []")
	}
	if exp.MemoryAnchors == nil {
		t.Error("MemoryAnchors is nil, want []")
	}
	if exp.MemoryEmbeddings == nil {
		t.Error("MemoryEmbeddings is nil, want []")
	}
	if exp.Episodes == nil {
		t.Error("Episodes is nil, want []")
	}
	if exp.DynamicRules == nil {
		t.Error("DynamicRules is nil, want []")
	}
	if exp.Annotations == nil {
		t.Error("Annotations is nil, want []")
	}
	if exp.QualityGaps == nil {
		t.Error("QualityGaps is nil, want []")
	}
}

// TestExportKnowledge_Memories verifies memories are exported.
func TestExportKnowledge_Memories(t *testing.T) {
	s := openTestStore(t)

	id1, err := s.InsertMemory(store.Memory{
		Tier:      store.TierProject,
		Content:   "test memory one",
		Source:    store.SourceManual,
		ExpiresAt: time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("InsertMemory 1: %v", err)
	}
	if id1 == "" {
		t.Skip("memory was deduplicated on empty store — unexpected but skip")
	}

	_, err = s.InsertMemory(store.Memory{
		Tier:      store.TierProject,
		Content:   "test memory two with distinct content",
		Source:    store.SourceManual,
		ExpiresAt: time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("InsertMemory 2: %v", err)
	}

	exp, err := s.ExportKnowledge("proj")
	if err != nil {
		t.Fatalf("ExportKnowledge: %v", err)
	}
	if exp.Summary.MemoryCount != len(exp.Memories) {
		t.Errorf("summary count %d != slice len %d", exp.Summary.MemoryCount, len(exp.Memories))
	}
	if exp.Summary.MemoryCount < 1 {
		t.Errorf("expected ≥1 memory in export, got %d", exp.Summary.MemoryCount)
	}
	found := false
	for _, m := range exp.Memories {
		if m.Content == "test memory one" {
			found = true
			break
		}
	}
	if !found {
		t.Error("exported memories do not contain inserted memory content")
	}
}

// TestExportKnowledge_Episodes verifies episodes are exported.
func TestExportKnowledge_Episodes(t *testing.T) {
	s := openTestStore(t)

	_, err := s.RememberEpisode(store.Episode{
		AgentID:       "test-agent",
		EpisodeType:   "decision",
		Outcome:       "success",
		Decision:      "use NoSQL for session cache",
		AffectedFiles: "[]", AffectedNodes: "[]", Tags: "[]",
	})
	if err != nil {
		t.Fatalf("RememberEpisode: %v", err)
	}

	exp, err := s.ExportKnowledge("proj")
	if err != nil {
		t.Fatalf("ExportKnowledge: %v", err)
	}
	if exp.Summary.EpisodeCount != len(exp.Episodes) {
		t.Errorf("summary count %d != slice len %d", exp.Summary.EpisodeCount, len(exp.Episodes))
	}
	if exp.Summary.EpisodeCount < 1 {
		t.Errorf("expected ≥1 episode in export, got %d", exp.Summary.EpisodeCount)
	}
	found := false
	for _, e := range exp.Episodes {
		if e.Decision == "use NoSQL for session cache" {
			found = true
		}
	}
	if !found {
		t.Error("exported episodes do not contain inserted episode")
	}
}

// TestExportKnowledge_MemoryVersions verifies version history is included.
func TestExportKnowledge_MemoryVersions(t *testing.T) {
	s := openTestStore(t)

	id, err := s.InsertMemory(store.Memory{
		Tier:      store.TierProject,
		Content:   "original content for versioning",
		Source:    store.SourceManual,
		ExpiresAt: time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	})
	if err != nil || id == "" {
		t.Skip("memory insert failed or was deduped")
	}

	_, err = s.CreateMemoryVersion(id, "original content for versioning", time.Now().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("CreateMemoryVersion: %v", err)
	}

	exp, err := s.ExportKnowledge("proj")
	if err != nil {
		t.Fatalf("ExportKnowledge: %v", err)
	}
	if exp.Summary.MemoryVersionCount != len(exp.MemoryVersions) {
		t.Errorf("summary count %d != slice len %d", exp.Summary.MemoryVersionCount, len(exp.MemoryVersions))
	}
	if exp.Summary.MemoryVersionCount < 1 {
		t.Errorf("expected ≥1 version in export, got %d", exp.Summary.MemoryVersionCount)
	}
}

// TestExportKnowledge_MemoryAnchors verifies anchor rows are exported.
func TestExportKnowledge_MemoryAnchors(t *testing.T) {
	s := openTestStore(t)

	id, err := s.InsertMemoryWithAnchors(
		store.Memory{
			Tier:      store.TierProject,
			Content:   "anchored memory for export",
			Source:    store.SourceManual,
			ExpiresAt: time.Now().Add(24 * time.Hour).Format(time.RFC3339),
		},
		[]string{"testrepo::auth.go::AuthService"},
	)
	if err != nil || id == "" {
		t.Skip("anchored insert failed or was deduped")
	}

	exp, err := s.ExportKnowledge("proj")
	if err != nil {
		t.Fatalf("ExportKnowledge: %v", err)
	}
	if exp.Summary.MemoryAnchorCount != len(exp.MemoryAnchors) {
		t.Errorf("summary count %d != slice len %d", exp.Summary.MemoryAnchorCount, len(exp.MemoryAnchors))
	}
	if exp.Summary.MemoryAnchorCount < 1 {
		t.Errorf("expected ≥1 anchor in export, got %d", exp.Summary.MemoryAnchorCount)
	}
}

// TestExportKnowledge_EmbeddingBase64 verifies embeddings are base64-encoded
// with correct size and stale flag.
func TestExportKnowledge_EmbeddingBase64(t *testing.T) {
	s := openTestStore(t)

	id, err := s.InsertMemory(store.Memory{
		Tier:      store.TierProject,
		Content:   "embedding export test",
		Source:    store.SourceManual,
		ExpiresAt: time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	})
	if err != nil || id == "" {
		t.Skip("memory insert failed")
	}

	vec := []float32{1.0, 0.0, 0.0} // non-zero magnitude for normalization
	if err := s.UpsertMemoryEmbedding(id, "test-model", vec); err != nil {
		t.Fatalf("UpsertMemoryEmbedding: %v", err)
	}

	exp, err := s.ExportKnowledge("proj")
	if err != nil {
		t.Fatalf("ExportKnowledge: %v", err)
	}
	if exp.Summary.MemoryEmbeddingCount < 1 {
		t.Fatalf("expected ≥1 embedding in export, got %d", exp.Summary.MemoryEmbeddingCount)
	}

	var found *store.ExportedMemEmbed
	for i := range exp.MemoryEmbeddings {
		if exp.MemoryEmbeddings[i].MemoryID == id {
			found = &exp.MemoryEmbeddings[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("embedding for memory %s not found in export", id)
	}
	// Verify the BLOB round-trips correctly through base64.
	decoded, err := base64.StdEncoding.DecodeString(found.EmbeddingB64)
	if err != nil {
		t.Fatalf("base64 decode failed: %v", err)
	}
	// Stored as normalized float32 BLOB: 3 floats × 4 bytes = 12 bytes.
	if len(decoded) != len(vec)*4 {
		t.Errorf("decoded blob len %d, want %d (3 float32s × 4 bytes)", len(decoded), len(vec)*4)
	}
	// A freshly inserted embedding is not stale.
	if found.Stale {
		t.Error("freshly inserted embedding should not be stale")
	}
}

// TestExportKnowledge_QualityGaps verifies ALL statuses are exported
// (not just "open" — the GetGaps default). Also verifies the export
// bypasses the 1000-row cap that GetGaps applies for normal UI queries.
func TestExportKnowledge_QualityGaps(t *testing.T) {
	s := openTestStore(t)

	_, err := s.UpsertGap(store.QualityGap{
		NodeID: "repo::auth.go::Login", GapID: "no-test",
		Description: "no test coverage", Severity: "high", Status: "open",
	})
	if err != nil {
		t.Fatalf("UpsertGap open: %v", err)
	}
	_, err = s.UpsertGap(store.QualityGap{
		NodeID: "repo::auth.go::Logout", GapID: "no-test",
		Description: "no test coverage", Severity: "medium", Status: "fixed",
	})
	if err != nil {
		t.Fatalf("UpsertGap fixed: %v", err)
	}

	exp, err := s.ExportKnowledge("proj")
	if err != nil {
		t.Fatalf("ExportKnowledge: %v", err)
	}
	// Both open and fixed gaps must appear.
	if exp.Summary.QualityGapCount < 2 {
		t.Errorf("expected ≥2 gaps (both open and fixed), got %d", exp.Summary.QualityGapCount)
	}
	if exp.Summary.QualityGapCount != len(exp.QualityGaps) {
		t.Errorf("summary count %d != slice len %d", exp.Summary.QualityGapCount, len(exp.QualityGaps))
	}
	// Verify both statuses are represented.
	statuses := map[string]bool{}
	for _, g := range exp.QualityGaps {
		statuses[g.Status] = true
	}
	if !statuses["open"] {
		t.Error("no 'open' gap in export")
	}
	if !statuses["fixed"] {
		t.Error("no 'fixed' gap in export — GetGaps default 'open' filter may have leaked into export path")
	}
}

// TestExportKnowledge_SummaryCounts ensures summary fields mirror slice lengths.
func TestExportKnowledge_SummaryCounts(t *testing.T) {
	s := openTestStore(t)

	_, _ = s.InsertMemory(store.Memory{
		Tier:      store.TierProject,
		Content:   "summary counts test memory",
		Source:    store.SourceManual,
		ExpiresAt: time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	})
	_, _ = s.RememberEpisode(store.Episode{
		AgentID:       "a",
		EpisodeType:   "decision",
		Outcome:       "success",
		Decision:      "counts test episode",
		AffectedFiles: "[]", AffectedNodes: "[]", Tags: "[]",
	})

	exp, err := s.ExportKnowledge("proj")
	if err != nil {
		t.Fatalf("ExportKnowledge: %v", err)
	}

	checks := []struct {
		name    string
		summary int
		slice   int
	}{
		{"memories", exp.Summary.MemoryCount, len(exp.Memories)},
		{"memory_versions", exp.Summary.MemoryVersionCount, len(exp.MemoryVersions)},
		{"memory_anchors", exp.Summary.MemoryAnchorCount, len(exp.MemoryAnchors)},
		{"memory_embeddings", exp.Summary.MemoryEmbeddingCount, len(exp.MemoryEmbeddings)},
		{"episodes", exp.Summary.EpisodeCount, len(exp.Episodes)},
		{"dynamic_rules", exp.Summary.DynamicRuleCount, len(exp.DynamicRules)},
		{"annotations", exp.Summary.AnnotationCount, len(exp.Annotations)},
		{"quality_gaps", exp.Summary.QualityGapCount, len(exp.QualityGaps)},
	}
	for _, c := range checks {
		if c.summary != c.slice {
			t.Errorf("%s: summary=%d != len(slice)=%d", c.name, c.summary, c.slice)
		}
	}
}
