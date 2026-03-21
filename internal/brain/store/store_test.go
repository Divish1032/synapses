package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// openTestStore opens a fresh Store in t.TempDir() and registers t.Cleanup to close it.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "brain.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// --- Open ---

func TestOpen_Success(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	if s == nil {
		t.Fatal("expected non-nil store")
	}
}

func TestOpen_PerformancePragmas(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	for _, tc := range []struct {
		pragma, want string
	}{
		{"journal_mode", "wal"},
		{"synchronous", "1"},       // NORMAL
		{"cache_size", "-65536"},    // 64 MB
		{"mmap_size", "268435456"}, // 256 MB
		{"temp_store", "2"},        // MEMORY
	} {
		var val string
		if err := s.db.QueryRow("PRAGMA " + tc.pragma).Scan(&val); err != nil {
			t.Errorf("PRAGMA %s: %v", tc.pragma, err)
		} else if val != tc.want {
			t.Errorf("PRAGMA %s = %s, want %s", tc.pragma, val, tc.want)
		}
	}
}

func TestOpen_CreatesParentDirs(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "a", "b", "c", "brain.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open with nested dirs: %v", err)
	}
	_ = s.Close()
}

// --- UpsertSummary / GetSummary ---

func TestUpsertAndGetSummary_Found(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	if err := s.UpsertSummary("test-project", "node1", "MyFunc", "does something useful", []string{"api", "core"}); err != nil {
		t.Fatalf("UpsertSummary: %v", err)
	}

	got := s.GetSummary("test-project", "node1")
	if got != "does something useful" {
		t.Errorf("GetSummary = %q, want %q", got, "does something useful")
	}
}

func TestGetSummary_NotFound(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	got := s.GetSummary("test-project", "nonexistent")
	if got != "" {
		t.Errorf("GetSummary(missing) = %q, want \"\"", got)
	}
}

func TestUpsertSummary_Update_Idempotent(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	_ = s.UpsertSummary("test-project", "node1", "MyFunc", "original summary", []string{"a"})
	_ = s.UpsertSummary("test-project", "node1", "MyFunc", "updated summary", []string{"a", "b"})

	got := s.GetSummary("test-project", "node1")
	if got != "updated summary" {
		t.Errorf("after update: GetSummary = %q, want %q", got, "updated summary")
	}

	// Count must still be 1.
	if c := s.SummaryCount(); c != 1 {
		t.Errorf("SummaryCount = %d, want 1", c)
	}
}

func TestUpsertSummary_NilTags(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	if err := s.UpsertSummary("test-project", "node-nil", "SomeFunc", "summary text", nil); err != nil {
		t.Fatalf("UpsertSummary with nil tags: %v", err)
	}

	_, tags := s.GetSummaryWithTags("test-project", "node-nil")
	if tags == nil {
		t.Error("GetSummaryWithTags: expected non-nil slice for nil tags, got nil")
	}
	if len(tags) != 0 {
		t.Errorf("expected empty tags slice, got %v", tags)
	}
}

// --- GetSummaryWithTags ---

func TestGetSummaryWithTags_Found(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	_ = s.UpsertSummary("test-project", "node2", "OtherFunc", "another summary", []string{"x", "y"})

	summary, tags := s.GetSummaryWithTags("test-project", "node2")
	if summary != "another summary" {
		t.Errorf("summary = %q, want %q", summary, "another summary")
	}
	if len(tags) != 2 || tags[0] != "x" || tags[1] != "y" {
		t.Errorf("tags = %v, want [x y]", tags)
	}
}

func TestGetSummaryWithTags_NotFound(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	summary, tags := s.GetSummaryWithTags("test-project", "missing")
	if summary != "" {
		t.Errorf("summary = %q, want \"\"", summary)
	}
	if tags != nil {
		t.Errorf("tags = %v, want nil", tags)
	}
}

// --- GetSummaries ---

func TestGetSummaries_MultipleIDs_MissingOmitted(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	_ = s.UpsertSummary("test-project", "n1", "Func1", "summary1", nil)
	_ = s.UpsertSummary("test-project", "n2", "Func2", "summary2", nil)

	result := s.GetSummaries("test-project", []string{"n1", "n2", "n3"})

	if len(result) != 2 {
		t.Fatalf("GetSummaries len = %d, want 2", len(result))
	}
	if result["n1"] != "summary1" {
		t.Errorf("n1 = %q, want %q", result["n1"], "summary1")
	}
	if result["n2"] != "summary2" {
		t.Errorf("n2 = %q, want %q", result["n2"], "summary2")
	}
	if _, ok := result["n3"]; ok {
		t.Error("n3 should be absent from result map")
	}
}

// --- GetSummariesByName ---

func TestGetSummariesByName_Found(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	_ = s.UpsertSummary("test-project", "id-alpha", "Alpha", "alpha summary", nil)
	_ = s.UpsertSummary("test-project", "id-beta", "Beta", "beta summary", nil)

	result := s.GetSummariesByName([]string{"Alpha", "Beta"})
	if len(result) != 2 {
		t.Fatalf("GetSummariesByName len = %d, want 2", len(result))
	}
	if result["Alpha"] != "alpha summary" {
		t.Errorf("Alpha = %q", result["Alpha"])
	}
}

func TestGetSummariesByName_Missing(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	result := s.GetSummariesByName([]string{"DoesNotExist"})
	if len(result) != 0 {
		t.Errorf("expected empty map, got %v", result)
	}
}

func TestGetSummariesByName_EmptySlice(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	result := s.GetSummariesByName([]string{})
	if result == nil {
		t.Error("expected non-nil empty map for empty input")
	}
	if len(result) != 0 {
		t.Errorf("expected empty map, got %v", result)
	}
}

// --- SummaryCount ---

func TestSummaryCount(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	if c := s.SummaryCount(); c != 0 {
		t.Errorf("initial count = %d, want 0", c)
	}
	_ = s.UpsertSummary("test-project", "n1", "F1", "s1", nil)
	_ = s.UpsertSummary("test-project", "n2", "F2", "s2", nil)
	if c := s.SummaryCount(); c != 2 {
		t.Errorf("after 2 inserts count = %d, want 2", c)
	}
}

// --- AllSummaries ---

func TestAllSummaries(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	_ = s.UpsertSummary("test-project", "n1", "F1", "s1", nil)
	_ = s.UpsertSummary("test-project", "n2", "F2", "s2", nil)

	all, err := s.AllSummaries()
	if err != nil {
		t.Fatalf("AllSummaries: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("AllSummaries len = %d, want 2", len(all))
	}
}

// --- UpsertViolationExplanation / GetViolationExplanation ---

func TestViolationExplanation_HitAndMiss(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	// Miss before insert.
	_, _, ok := s.GetViolationExplanation("rule1", "file.go")
	if ok {
		t.Error("expected miss before insert")
	}

	// Insert and hit.
	if err := s.UpsertViolationExplanation("rule1", "file.go", "no imports from x", "move to pkg y"); err != nil {
		t.Fatalf("UpsertViolationExplanation: %v", err)
	}

	expl, fix, ok := s.GetViolationExplanation("rule1", "file.go")
	if !ok {
		t.Fatal("expected hit after insert")
	}
	if expl != "no imports from x" {
		t.Errorf("explanation = %q", expl)
	}
	if fix != "move to pkg y" {
		t.Errorf("fix = %q", fix)
	}
}

// --- UpsertInsightCache / GetInsightCache ---

func TestInsightCache_HitAndMiss(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	// Miss.
	_, ok := s.GetInsightCache("node-x", "development")
	if ok {
		t.Error("expected miss before insert")
	}

	// Insert and hit.
	concerns := []string{"possible race", "large allocation"}
	if err := s.UpsertInsightCache("node-x", "development", "useful insight text", concerns); err != nil {
		t.Fatalf("UpsertInsightCache: %v", err)
	}

	entry, ok := s.GetInsightCache("node-x", "development")
	if !ok {
		t.Fatal("expected hit after insert")
	}
	if entry.Insight != "useful insight text" {
		t.Errorf("Insight = %q", entry.Insight)
	}
	if len(entry.Concerns) != 2 || entry.Concerns[0] != "possible race" {
		t.Errorf("Concerns = %v", entry.Concerns)
	}
}

func TestInsightCache_NilConcerns(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	if err := s.UpsertInsightCache("node-nil", "staging", "insight", nil); err != nil {
		t.Fatalf("UpsertInsightCache nil concerns: %v", err)
	}

	entry, ok := s.GetInsightCache("node-nil", "staging")
	if !ok {
		t.Fatal("expected hit")
	}
	if entry.Concerns == nil {
		t.Error("expected non-nil concerns slice, got nil")
	}
	if len(entry.Concerns) != 0 {
		t.Errorf("expected empty concerns, got %v", entry.Concerns)
	}
}

// UpsertSummary must invalidate the insight cache for that node.
func TestUpsertSummary_InvalidatesInsightCache(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	_ = s.UpsertInsightCache("nodeA", "development", "old insight", nil)
	_, ok := s.GetInsightCache("nodeA", "development")
	if !ok {
		t.Fatal("pre-condition: cache entry should exist")
	}

	// Re-upsert the summary for nodeA — must invalidate the cache.
	_ = s.UpsertSummary("test-project", "nodeA", "FuncA", "new summary", nil)

	_, ok = s.GetInsightCache("nodeA", "development")
	if ok {
		t.Error("insight cache should have been invalidated after UpsertSummary")
	}
}

// --- UpsertSDLCConfig / GetSDLCConfig ---

func TestGetSDLCConfig_DefaultsWhenEmpty(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	cfg := s.GetSDLCConfig()
	if cfg.Phase != "development" {
		t.Errorf("default Phase = %q, want development", cfg.Phase)
	}
	if cfg.QualityMode != "standard" {
		t.Errorf("default QualityMode = %q, want standard", cfg.QualityMode)
	}
}

func TestUpsertSDLCConfig_AfterUpsert(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	if err := s.UpsertSDLCConfig("production", "strict", "agent-007"); err != nil {
		t.Fatalf("UpsertSDLCConfig: %v", err)
	}

	cfg := s.GetSDLCConfig()
	if cfg.Phase != "production" {
		t.Errorf("Phase = %q, want production", cfg.Phase)
	}
	if cfg.QualityMode != "strict" {
		t.Errorf("QualityMode = %q, want strict", cfg.QualityMode)
	}
	if cfg.UpdatedBy != "agent-007" {
		t.Errorf("UpdatedBy = %q, want agent-007", cfg.UpdatedBy)
	}
}

// --- UpsertPattern / GetPatternsForTriggers ---

func TestUpsertPattern_IncrementCount(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	if err := s.UpsertPattern("AuthHandler", "TokenStore", "auth always touches token store"); err != nil {
		t.Fatalf("first UpsertPattern: %v", err)
	}
	if err := s.UpsertPattern("AuthHandler", "TokenStore", "auth always touches token store"); err != nil {
		t.Fatalf("second UpsertPattern: %v", err)
	}

	patterns := s.GetPatternsForTriggers([]string{"AuthHandler"}, 10)
	if len(patterns) != 1 {
		t.Fatalf("expected 1 pattern, got %d", len(patterns))
	}
	if patterns[0].CoCount < 2 {
		t.Errorf("CoCount = %d, want >= 2", patterns[0].CoCount)
	}
	if patterns[0].Trigger != "AuthHandler" {
		t.Errorf("Trigger = %q", patterns[0].Trigger)
	}
	if patterns[0].CoChange != "TokenStore" {
		t.Errorf("CoChange = %q", patterns[0].CoChange)
	}
}

func TestGetPatternsForTriggers_EmptyTriggers(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	_ = s.UpsertPattern("A", "B", "reason")
	result := s.GetPatternsForTriggers([]string{}, 10)
	if result != nil {
		t.Errorf("expected nil for empty triggers, got %v", result)
	}
}

func TestGetPatternsForTriggers_ZeroLimit(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	_ = s.UpsertPattern("A", "B", "reason")
	result := s.GetPatternsForTriggers([]string{"A"}, 0)
	if result != nil {
		t.Errorf("expected nil for zero limit, got %v", result)
	}
}

func TestUpsertPattern_LongReasonTruncated(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	longReason := strings.Repeat("x", 200)
	if err := s.UpsertPattern("TrigA", "CoB", longReason); err != nil {
		t.Fatalf("UpsertPattern with long reason: %v", err)
	}

	patterns := s.GetPatternsForTriggers([]string{"TrigA"}, 5)
	if len(patterns) != 1 {
		t.Fatalf("expected 1 pattern, got %d", len(patterns))
	}
	if len(patterns[0].Reason) > 100 {
		t.Errorf("reason length = %d, want <= 100", len(patterns[0].Reason))
	}
}

// --- AllPatterns ---

func TestAllPatterns(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	_ = s.UpsertPattern("T1", "C1", "r1")
	_ = s.UpsertPattern("T2", "C2", "r2")

	all, err := s.AllPatterns()
	if err != nil {
		t.Fatalf("AllPatterns: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("AllPatterns len = %d, want 2", len(all))
	}
}

// --- LogDecision / GetRecentDecisions ---

func TestLogAndGetRecentDecisions_FilterByEntity(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	_ = s.LogDecision("agent1", "dev", "FuncA", "refactor", []string{"FuncB"}, "success", "")
	_ = s.LogDecision("agent1", "dev", "FuncC", "review", nil, "pending", "notes here")

	decisions, err := s.GetRecentDecisions("FuncA", 10)
	if err != nil {
		t.Fatalf("GetRecentDecisions: %v", err)
	}
	if len(decisions) != 1 {
		t.Fatalf("expected 1 decision for FuncA, got %d", len(decisions))
	}
	if decisions[0].EntityName != "FuncA" {
		t.Errorf("EntityName = %q, want FuncA", decisions[0].EntityName)
	}
	if decisions[0].Action != "refactor" {
		t.Errorf("Action = %q, want refactor", decisions[0].Action)
	}
	if len(decisions[0].RelatedEntities) != 1 || decisions[0].RelatedEntities[0] != "FuncB" {
		t.Errorf("RelatedEntities = %v", decisions[0].RelatedEntities)
	}
}

func TestGetRecentDecisions_AllEntities(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	_ = s.LogDecision("a1", "dev", "FuncA", "act1", nil, "ok", "")
	_ = s.LogDecision("a2", "dev", "FuncB", "act2", nil, "ok", "")
	_ = s.LogDecision("a3", "dev", "FuncC", "act3", nil, "ok", "")

	// Empty entityName = all entities.
	decisions, err := s.GetRecentDecisions("", 10)
	if err != nil {
		t.Fatalf("GetRecentDecisions all: %v", err)
	}
	if len(decisions) != 3 {
		t.Errorf("expected 3 decisions, got %d", len(decisions))
	}
}

func TestLogDecision_NilRelatedEntities(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	if err := s.LogDecision("a", "p", "Entity", "do", nil, "out", "note"); err != nil {
		t.Fatalf("LogDecision with nil related: %v", err)
	}

	decisions, _ := s.GetRecentDecisions("Entity", 1)
	if len(decisions) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(decisions))
	}
	if decisions[0].RelatedEntities == nil {
		t.Error("RelatedEntities should be non-nil empty slice, got nil")
	}
}

// --- UpsertADR / GetADR ---

func TestUpsertAndGetADR_Hit(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	adr := ADR{
		ID:          "adr-001",
		Title:       "Use SQLite for brain store",
		Status:      "accepted",
		ContextText: "We need lightweight persistent storage",
		Decision:    "Use SQLite via modernc.org/sqlite",
		LinkedFiles: []string{"internal/store/"},
	}
	if err := s.UpsertADR(adr); err != nil {
		t.Fatalf("UpsertADR: %v", err)
	}

	got, err := s.GetADR("adr-001")
	if err != nil {
		t.Fatalf("GetADR: %v", err)
	}
	if got.Title != "Use SQLite for brain store" {
		t.Errorf("Title = %q", got.Title)
	}
	if got.Status != "accepted" {
		t.Errorf("Status = %q", got.Status)
	}
	if len(got.LinkedFiles) != 1 || got.LinkedFiles[0] != "internal/store/" {
		t.Errorf("LinkedFiles = %v", got.LinkedFiles)
	}
}

func TestGetADR_Miss(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	_, err := s.GetADR("nonexistent-adr")
	if err == nil {
		t.Error("expected error for missing ADR, got nil")
	}
}

func TestUpsertADR_Update(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	adr := ADR{ID: "adr-x", Title: "Original title", Status: "proposed", Decision: "TBD"}
	_ = s.UpsertADR(adr)

	adr.Title = "Updated title"
	adr.Status = "accepted"
	adr.Decision = "Decided"
	_ = s.UpsertADR(adr)

	got, err := s.GetADR("adr-x")
	if err != nil {
		t.Fatalf("GetADR after update: %v", err)
	}
	if got.Title != "Updated title" {
		t.Errorf("Title = %q, want Updated title", got.Title)
	}
	if got.Status != "accepted" {
		t.Errorf("Status = %q, want accepted", got.Status)
	}
}

// --- AllADRs ---

func TestAllADRs(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	_ = s.UpsertADR(ADR{ID: "a1", Title: "T1", Status: "accepted", Decision: "D1"})
	_ = s.UpsertADR(ADR{ID: "a2", Title: "T2", Status: "proposed", Decision: "D2"})

	all, err := s.AllADRs()
	if err != nil {
		t.Fatalf("AllADRs: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("AllADRs len = %d, want 2", len(all))
	}
}

// --- GetADRsForFile ---

func TestGetADRsForFile_AcceptedOnly(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	// accepted — should match
	_ = s.UpsertADR(ADR{ID: "accepted-1", Title: "T", Status: "accepted", Decision: "D",
		LinkedFiles: []string{"internal/store/"}})
	// proposed — should NOT match
	_ = s.UpsertADR(ADR{ID: "proposed-1", Title: "T2", Status: "proposed", Decision: "D2",
		LinkedFiles: []string{"internal/store/"}})

	adrs, err := s.GetADRsForFile("internal/store/store.go", 10)
	if err != nil {
		t.Fatalf("GetADRsForFile: %v", err)
	}
	if len(adrs) != 1 {
		t.Errorf("expected 1 (accepted only), got %d", len(adrs))
	}
	if adrs[0].ID != "accepted-1" {
		t.Errorf("ID = %q, want accepted-1", adrs[0].ID)
	}
}

func TestGetADRsForFile_PatternMatch(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	_ = s.UpsertADR(ADR{ID: "a1", Title: "T", Status: "accepted", Decision: "D",
		LinkedFiles: []string{"internal/archivist/"}})
	_ = s.UpsertADR(ADR{ID: "a2", Title: "T", Status: "accepted", Decision: "D",
		LinkedFiles: []string{"internal/store/"}})

	adrs, err := s.GetADRsForFile("internal/archivist/archivist.go", 10)
	if err != nil {
		t.Fatalf("GetADRsForFile: %v", err)
	}
	if len(adrs) != 1 || adrs[0].ID != "a1" {
		t.Errorf("expected a1, got %v", adrs)
	}
}

func TestGetADRsForFile_LimitRespected(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	for i := 0; i < 5; i++ {
		id := string(rune('a' + i))
		_ = s.UpsertADR(ADR{ID: id, Title: "T", Status: "accepted", Decision: "D",
			LinkedFiles: []string{"shared/"}})
	}

	adrs, err := s.GetADRsForFile("shared/file.go", 2)
	if err != nil {
		t.Fatalf("GetADRsForFile: %v", err)
	}
	if len(adrs) != 2 {
		t.Errorf("expected 2 (limit), got %d", len(adrs))
	}
}

func TestGetADRsForFile_NoMatch(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	_ = s.UpsertADR(ADR{ID: "a1", Title: "T", Status: "accepted", Decision: "D",
		LinkedFiles: []string{"internal/foo/"}})

	adrs, err := s.GetADRsForFile("cmd/main.go", 10)
	if err != nil {
		t.Fatalf("GetADRsForFile: %v", err)
	}
	if len(adrs) != 0 {
		t.Errorf("expected no match, got %v", adrs)
	}
}

// --- Reset ---

func TestReset_ClearsAllTables(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	// Populate multiple tables.
	_ = s.UpsertSummary("test-project", "n1", "F", "s", nil)
	_ = s.UpsertViolationExplanation("r1", "f.go", "expl", "fix")
	_ = s.UpsertInsightCache("n1", "dev", "insight", nil)
	_ = s.UpsertSDLCConfig("production", "strict", "agent")
	_ = s.UpsertPattern("T", "C", "r")
	_ = s.LogDecision("a", "p", "E", "act", nil, "out", "")
	_ = s.UpsertADR(ADR{ID: "adr1", Title: "T", Status: "accepted", Decision: "D"})

	if err := s.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	if c := s.SummaryCount(); c != 0 {
		t.Errorf("after Reset: SummaryCount = %d, want 0", c)
	}
	_, _, ok := s.GetViolationExplanation("r1", "f.go")
	if ok {
		t.Error("after Reset: violation cache should be empty")
	}
	_, ok = s.GetInsightCache("n1", "dev")
	if ok {
		t.Error("after Reset: insight cache should be empty")
	}
	decisions, _ := s.GetRecentDecisions("", 100)
	if len(decisions) != 0 {
		t.Errorf("after Reset: decision_log len = %d, want 0", len(decisions))
	}
	adrs, _ := s.AllADRs()
	if len(adrs) != 0 {
		t.Errorf("after Reset: ADRs len = %d, want 0", len(adrs))
	}
	patterns, _ := s.AllPatterns()
	if len(patterns) != 0 {
		t.Errorf("after Reset: patterns len = %d, want 0", len(patterns))
	}
}

// --- Close ---

func TestClose(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "close_test.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
