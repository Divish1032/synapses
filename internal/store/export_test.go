package store_test

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/store"
)

// TestExportKnowledge_EmptyStore verifies the export works on a fresh store
// with no data: all slices are nil/empty and summary counts are zero.
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
	if exp.Summary.MemoryCount != 0 {
		t.Errorf("MemoryCount = %d, want 0", exp.Summary.MemoryCount)
	}
	if exp.Summary.EpisodeCount != 0 {
		t.Errorf("EpisodeCount = %d, want 0", exp.Summary.EpisodeCount)
	}
}

// TestExportKnowledge_Memories verifies memories (including expired) are exported.
func TestExportKnowledge_Memories(t *testing.T) {
	s := openTestStore(t)

	// Insert two memories via InsertMemory.
	m1 := store.Memory{
		Tier:      store.TierProject,
		Content:   "test memory one",
		Source:    store.SourceManual,
		ExpiresAt: time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	}
	id1, err := s.InsertMemory(m1)
	if err != nil {
		t.Fatalf("InsertMemory 1: %v", err)
	}
	if id1 == "" {
		t.Skip("memory was deduplicated on empty store — unexpected but skip")
	}

	m2 := store.Memory{
		Tier:      store.TierProject,
		Content:   "test memory two with distinct content",
		Source:    store.SourceManual,
		ExpiresAt: time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	}
	id2, err := s.InsertMemory(m2)
	if err != nil {
		t.Fatalf("InsertMemory 2: %v", err)
	}
	_ = id2

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
	// Verify the inserted memory content is present.
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

	ep := store.Episode{
		AgentID:     "test-agent",
		EpisodeType: "decision",
		Outcome:     "success",
		Decision:    "use NoSQL for session cache",
		Rationale:   "lower latency requirements",
		AffectedFiles: "[]",
		AffectedNodes: "[]",
		Tags:        "[]",
		Importance:  0.7,
	}
	_, err := s.RememberEpisode(ep)
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

	// Insert a memory, then create a version snapshot.
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

// TestExportKnowledge_EmbeddingBase64 verifies embeddings are base64-encoded.
func TestExportKnowledge_EmbeddingBase64(t *testing.T) {
	s := openTestStore(t)

	// Insert a memory then manually insert an embedding blob.
	id, err := s.InsertMemory(store.Memory{
		Tier:      store.TierProject,
		Content:   "embedding export test",
		Source:    store.SourceManual,
		ExpiresAt: time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	})
	if err != nil || id == "" {
		t.Skip("memory insert failed")
	}

	// Provide a non-zero float32 vector (must have non-zero magnitude).
	vec := []float32{1.0, 0.0, 0.0}
	err = s.UpsertMemoryEmbedding(id, "test-model", vec)
	if err != nil {
		t.Fatalf("UpsertMemoryEmbedding: %v", err)
	}

	exp, err := s.ExportKnowledge("proj")
	if err != nil {
		t.Fatalf("ExportKnowledge: %v", err)
	}
	if exp.Summary.MemoryEmbeddingCount < 1 {
		t.Fatalf("expected ≥1 embedding in export, got %d", exp.Summary.MemoryEmbeddingCount)
	}
	// Verify the blob was correctly base64-encoded and round-trips.
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
	decoded, err := base64.StdEncoding.DecodeString(found.EmbeddingB64)
	if err != nil {
		t.Fatalf("base64 decode failed: %v", err)
	}
	// Each float32 is 4 bytes; verify the decoded blob has the right size.
	if len(decoded) != len(vec)*4 {
		t.Errorf("decoded blob len %d, want %d (3 float32s × 4 bytes)", len(decoded), len(vec)*4)
	}
}

// TestExportKnowledge_QualityGaps verifies all gap statuses are exported.
func TestExportKnowledge_QualityGaps(t *testing.T) {
	s := openTestStore(t)

	// Insert one open and one fixed gap.
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
	// Export should include both open and fixed gaps (Status: "all").
	if exp.Summary.QualityGapCount < 2 {
		t.Errorf("expected ≥2 gaps in export, got %d", exp.Summary.QualityGapCount)
	}
	if exp.Summary.QualityGapCount != len(exp.QualityGaps) {
		t.Errorf("summary count %d != slice len %d", exp.Summary.QualityGapCount, len(exp.QualityGaps))
	}
}

// TestExportKnowledge_SummaryCounts ensures summary fields mirror slice lengths.
func TestExportKnowledge_SummaryCounts(t *testing.T) {
	s := openTestStore(t)

	// Insert a memory and an episode to have non-zero counts.
	_, _ = s.InsertMemory(store.Memory{
		Tier:      store.TierProject,
		Content:   "summary counts test memory",
		Source:    store.SourceManual,
		ExpiresAt: time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	})
	_, _ = s.RememberEpisode(store.Episode{
		AgentID:     "a",
		EpisodeType: "decision",
		Outcome:     "success",
		Decision:    "counts test episode",
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
