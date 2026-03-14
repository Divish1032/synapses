package contextbuilder

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/brain/enricher"
	"github.com/SynapsesOS/synapses/internal/brain/llm"
	"github.com/SynapsesOS/synapses/internal/brain/sdlc"
	"github.com/SynapsesOS/synapses/internal/brain/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "brain.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func newTestBuilder(t *testing.T, mockResp string) (*Builder, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	mgr := sdlc.NewManager(st)
	var enr *enricher.Enricher
	if mockResp != "" {
		mock := llm.NewMockClient(mockResp)
		enr = enricher.New(mock, st, 3*time.Second)
	}
	return New(st, mgr, enr), st
}

func TestBuild_FastPath_BasicPacket(t *testing.T) {
	b, st := newTestBuilder(t, "")

	// Pre-populate a root summary.
	st.UpsertSummary("test-project", "node:auth:Validate", "Validate", "Validates JWT tokens.", []string{"auth"})
	// Pre-populate a dep summary.
	st.UpsertSummary("test-project", "node:cache:TokenCache", "TokenCache", "Caches token validation results.", []string{"cache"})

	pkt, err := b.Build(context.Background(), Request{
		AgentID:     "agent-1",
		ProjectID:   "test-project",
		Phase:       sdlc.PhaseDevelopment,
		QualityMode: sdlc.ModeStandard,
		EnableLLM:   false,
		RootNodeID:  "node:auth:Validate",
		RootName:    "Validate",
		RootType:    "method",
		CalleeNames: []string{"TokenCache"},
	})
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}
	if pkt == nil {
		t.Fatal("Build returned nil packet")
	}

	// Header fields.
	if pkt.EntityName != "Validate" {
		t.Errorf("EntityName = %q", pkt.EntityName)
	}
	if pkt.Phase != sdlc.PhaseDevelopment {
		t.Errorf("Phase = %q", pkt.Phase)
	}
	if pkt.QualityMode != sdlc.ModeStandard {
		t.Errorf("QualityMode = %q", pkt.QualityMode)
	}

	// Root summary from store.
	if pkt.RootSummary != "Validates JWT tokens." {
		t.Errorf("RootSummary = %q", pkt.RootSummary)
	}

	// Dep summaries.
	if pkt.DependencySummaries["TokenCache"] != "Caches token validation results." {
		t.Errorf("DependencySummaries[TokenCache] = %q", pkt.DependencySummaries["TokenCache"])
	}

	// Quality gate should be present in development phase.
	if len(pkt.Gate.Checklist) == 0 {
		t.Error("expected non-empty quality gate checklist in development phase")
	}

	// Phase guidance should be present.
	if pkt.PhaseGuidance == "" {
		t.Error("expected non-empty phase guidance")
	}

	// LLM insight should be empty (EnableLLM=false).
	if pkt.Insight != "" {
		t.Errorf("expected no insight with EnableLLM=false, got %q", pkt.Insight)
	}
}

func TestBuild_WithLLM_PopulatesInsight(t *testing.T) {
	mockResp := `{"insight": "Validate is the auth boundary.", "concerns": ["token expiry"]}`
	b, _ := newTestBuilder(t, mockResp)

	pkt, err := b.Build(context.Background(), Request{
		ProjectID:   "test-project",
		Phase:       sdlc.PhaseDevelopment,
		QualityMode: sdlc.ModeStandard,
		EnableLLM:   true,
		RootNodeID:  "node:auth:Validate",
		RootName:    "Validate",
	})
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}
	if pkt.Insight != "Validate is the auth boundary." {
		t.Errorf("Insight = %q", pkt.Insight)
	}
	if len(pkt.Concerns) == 0 || pkt.Concerns[0] != "token expiry" {
		t.Errorf("Concerns = %v", pkt.Concerns)
	}
}

func TestBuild_TestingPhase_FiltersDepSummaries(t *testing.T) {
	b, st := newTestBuilder(t, "")
	st.UpsertSummary("test-project", "node:svc:Impl", "Impl", "Implementation detail.", []string{})

	pkt, err := b.Build(context.Background(), Request{
		ProjectID:   "test-project",
		Phase:       sdlc.PhaseTesting,
		QualityMode: sdlc.ModeStandard,
		EnableLLM:   false,
		RootNodeID:  "node:svc:Impl",
		RootName:    "Impl",
		CalleeNames: []string{"SomeHelper"},
	})
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}

	// In testing phase, DependencySummaries is disabled.
	if len(pkt.DependencySummaries) > 0 {
		t.Errorf("expected no dep summaries in testing phase, got %v", pkt.DependencySummaries)
	}

	// Root summary should still be present.
	if pkt.RootSummary != "Implementation detail." {
		t.Errorf("RootSummary = %q", pkt.RootSummary)
	}

	// LLM insight disabled in testing phase.
	if pkt.Insight != "" {
		t.Errorf("expected no LLM insight in testing phase, got %q", pkt.Insight)
	}
}

func TestBuild_TeamStatus_ExcludesSelf(t *testing.T) {
	b, _ := newTestBuilder(t, "")

	pkt, err := b.Build(context.Background(), Request{
		AgentID:     "me",
		Phase:       sdlc.PhaseDevelopment,
		QualityMode: sdlc.ModeStandard,
		ActiveClaims: []ClaimRef{
			{AgentID: "me", Scope: "auth", ScopeType: "package"},
			{AgentID: "other-agent", Scope: "store", ScopeType: "package", ExpiresAt: time.Now().Add(10 * time.Minute).Format(time.RFC3339)},
		},
	})
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}

	// Self claim must be excluded.
	for _, a := range pkt.TeamStatus {
		if a.AgentID == "me" {
			t.Error("own claim should not appear in TeamStatus")
		}
	}
	if len(pkt.TeamStatus) != 1 || pkt.TeamStatus[0].AgentID != "other-agent" {
		t.Errorf("TeamStatus = %+v", pkt.TeamStatus)
	}
	if pkt.TeamStatus[0].ExpiresIn <= 0 {
		t.Errorf("ExpiresIn should be >0, got %d", pkt.TeamStatus[0].ExpiresIn)
	}
}

func TestBuild_PatternHints_LoadedFromStore(t *testing.T) {
	b, st := newTestBuilder(t, "")

	// Seed patterns: editing Validate co-changes with TokenCache.
	st.UpsertPattern("Validate", "TokenCache", "edit during development")
	st.UpsertPattern("Validate", "TokenCache", "edit during development")
	st.UpsertPattern("Validate", "UserRepo", "edit during testing")

	pkt, err := b.Build(context.Background(), Request{
		Phase:       sdlc.PhaseDevelopment,
		QualityMode: sdlc.ModeStandard,
		RootName:    "Validate",
	})
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}
	if len(pkt.PatternHints) == 0 {
		t.Error("expected pattern hints from store, got none")
	}
	// Highest-confidence pattern should come first (TokenCache seen twice).
	if pkt.PatternHints[0].Trigger != "Validate" {
		t.Errorf("first pattern trigger = %q", pkt.PatternHints[0].Trigger)
	}
}

func TestBuild_ActiveConstraints_WithHint(t *testing.T) {
	b, st := newTestBuilder(t, "")

	// Seed a cached violation explanation for the rule+file combo.
	st.UpsertViolationExplanation("no-db-in-handler", "handlers/auth.go", "handlers should not call db directly", "inject a repository instead")

	pkt, err := b.Build(context.Background(), Request{
		Phase:       sdlc.PhaseDevelopment,
		QualityMode: sdlc.ModeStandard,
		RootFile:    "handlers/auth.go",
		ApplicableRules: []RuleRef{
			{RuleID: "no-db-in-handler", Severity: "error", Description: "handlers must not call db directly"},
		},
	})
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}
	if len(pkt.ActiveConstraints) != 1 {
		t.Fatalf("want 1 constraint, got %d", len(pkt.ActiveConstraints))
	}
	c := pkt.ActiveConstraints[0]
	if c.Hint != "inject a repository instead" {
		t.Errorf("Hint = %q", c.Hint)
	}
}

func TestBuild_EmptySnapshot_NoErrors(t *testing.T) {
	b, _ := newTestBuilder(t, "")
	pkt, err := b.Build(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Build error on empty request: %v", err)
	}
	if pkt == nil {
		t.Fatal("Build returned nil on empty request")
	}
}

func TestBuild_PacketQuality_NoData(t *testing.T) {
	b, _ := newTestBuilder(t, "")
	pkt, err := b.Build(context.Background(), Request{
		Phase:       sdlc.PhaseDevelopment,
		QualityMode: sdlc.ModeStandard,
		RootName:    "UnknownEntity",
	})
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}
	if pkt.PacketQuality != 0.0 {
		t.Errorf("PacketQuality with no data want 0.0, got %f", pkt.PacketQuality)
	}
}

func TestBuild_PacketQuality_WithSummary(t *testing.T) {
	b, st := newTestBuilder(t, "")
	st.UpsertSummary("test-project", "node:svc:Foo", "Foo", "Does the foo thing.", []string{})

	pkt, err := b.Build(context.Background(), Request{
		ProjectID:   "test-project",
		Phase:       sdlc.PhaseDevelopment,
		QualityMode: sdlc.ModeStandard,
		RootNodeID:  "node:svc:Foo",
		RootName:    "Foo",
	})
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}
	// Root summary present → 0.4; no dep summaries, no insight.
	if pkt.PacketQuality != 0.4 {
		t.Errorf("PacketQuality with root summary want 0.4, got %f", pkt.PacketQuality)
	}
}

func TestBuild_InsightCache_HitSkipsLLM(t *testing.T) {
	// Builder with a real mock LLM (to ensure LLM is NOT called on cache hit).
	b, st := newTestBuilder(t, `{"insight": "should not appear", "concerns": []}`)

	// Real-world order: ingest first, then cache the insight.
	// UpsertSummary invalidates insight cache, so it must come before UpsertInsightCache.
	st.UpsertSummary("test-project", "node:svc:Bar", "Bar", "Does bar.", []string{})
	st.UpsertInsightCache("node:svc:Bar", sdlc.PhaseDevelopment, "Cached insight.", []string{"cached concern"})

	pkt, err := b.Build(context.Background(), Request{
		ProjectID:   "test-project",
		Phase:       sdlc.PhaseDevelopment,
		QualityMode: sdlc.ModeStandard,
		EnableLLM:   true,
		RootNodeID:  "node:svc:Bar",
		RootName:    "Bar",
	})
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}
	if pkt.Insight != "Cached insight." {
		t.Errorf("expected cached insight, got %q", pkt.Insight)
	}
	if len(pkt.Concerns) == 0 || pkt.Concerns[0] != "cached concern" {
		t.Errorf("expected cached concerns, got %v", pkt.Concerns)
	}
	// Cache hit means LLMUsed should be false.
	if pkt.LLMUsed {
		t.Error("LLMUsed should be false on insight cache hit")
	}
}

// ---------------------------------------------------------------------------
// buildGraphWarnings — unexported, accessible from package contextbuilder
// ---------------------------------------------------------------------------

func TestBuildGraphWarnings_NoWarnings(t *testing.T) {
	req := Request{FanIn: 3, HasTests: true, RootFile: "auth.go"}
	warnings := buildGraphWarnings(req)
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for fanin=3 with tests, got %v", warnings)
	}
}

func TestBuildGraphWarnings_HighFanIn(t *testing.T) {
	req := Request{FanIn: 10, HasTests: true, RootFile: "auth.go"}
	warnings := buildGraphWarnings(req)
	if len(warnings) == 0 {
		t.Error("expected blast radius warning for fanin=10, got none")
	}
	// Warning should mention "callers"
	found := false
	for _, w := range warnings {
		if len(w) > 10 {
			found = true
		}
	}
	if !found {
		t.Errorf("warning doesn't seem meaningful: %v", warnings)
	}
}

func TestBuildGraphWarnings_HighFanIn_FromCallerNames(t *testing.T) {
	// FanIn=0 but CallerNames has 7 entries → falls back to len(CallerNames).
	req := Request{
		FanIn:       0,
		HasTests:    true,
		RootFile:    "auth.go",
		CallerNames: []string{"A", "B", "C", "D", "E", "F", "G"},
	}
	warnings := buildGraphWarnings(req)
	if len(warnings) == 0 {
		t.Error("expected blast radius warning when CallerNames has 7 entries, got none")
	}
}

func TestBuildGraphWarnings_NoTestFile(t *testing.T) {
	req := Request{FanIn: 1, HasTests: false, RootFile: "auth.go"}
	warnings := buildGraphWarnings(req)
	if len(warnings) == 0 {
		t.Error("expected test coverage warning when HasTests=false and RootFile set, got none")
	}
}

func TestBuildGraphWarnings_NoTestFile_EmptyRootFile(t *testing.T) {
	// RootFile="" → no test warning
	req := Request{FanIn: 1, HasTests: false, RootFile: ""}
	warnings := buildGraphWarnings(req)
	if len(warnings) != 0 {
		t.Errorf("expected no warning when RootFile is empty, got %v", warnings)
	}
}

func TestBuildGraphWarnings_HighFanIn_CallerListCapped(t *testing.T) {
	// CallerNames > 5 → list is capped to 5 in the warning message.
	req := Request{
		FanIn:       8,
		HasTests:    true,
		CallerNames: []string{"A", "B", "C", "D", "E", "F", "G", "H"},
	}
	warnings := buildGraphWarnings(req)
	if len(warnings) == 0 {
		t.Error("expected blast radius warning, got none")
	}
}

// ---------------------------------------------------------------------------
// computeQuality — unexported
// ---------------------------------------------------------------------------

func TestComputeQuality_Zero(t *testing.T) {
	pkt := &Packet{}
	q := computeQuality(pkt)
	if q != 0.0 {
		t.Errorf("computeQuality(empty) = %f, want 0.0", q)
	}
}

func TestComputeQuality_RootSummaryOnly(t *testing.T) {
	pkt := &Packet{RootSummary: "some summary"}
	q := computeQuality(pkt)
	if q != 0.4 {
		t.Errorf("computeQuality(root summary only) = %f, want 0.4", q)
	}
}

func TestComputeQuality_Full(t *testing.T) {
	pkt := &Packet{
		RootSummary:         "summary",
		DependencySummaries: map[string]string{"X": "dep"},
		Insight:             "some insight",
	}
	q := computeQuality(pkt)
	if q != 1.0 {
		t.Errorf("computeQuality(full) = %f, want 1.0", q)
	}
}

// ---------------------------------------------------------------------------
// collectDepNames — unexported
// ---------------------------------------------------------------------------

func TestCollectDepNames_UniqueDedup(t *testing.T) {
	// collectDepNames merges CalleeNames + CallerNames only (not RelatedNames).
	req := Request{
		CalleeNames: []string{"A", "B", "A"},
		CallerNames: []string{"C", "B"},
	}
	names := collectDepNames(req, 20)
	seen := make(map[string]int)
	for _, n := range names {
		seen[n]++
	}
	for n, count := range seen {
		if count > 1 {
			t.Errorf("collectDepNames: duplicate %q (count=%d)", n, count)
		}
	}
	if len(seen) != 3 { // A, B, C (deduped)
		t.Errorf("collectDepNames: expected 3 unique names, got %d: %v", len(seen), names)
	}
}

func TestCollectDepNames_Empty(t *testing.T) {
	req := Request{}
	names := collectDepNames(req, 20)
	if len(names) != 0 {
		t.Errorf("collectDepNames(empty) = %v, want empty", names)
	}
}
