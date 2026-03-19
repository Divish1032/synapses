package mcp

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
	"github.com/SynapsesOS/synapses/internal/watcher"
)

// ── buildPackageWorkSummary unit tests ─────────────────────────────────────

func TestBuildPackageWorkSummary_NilSummary(t *testing.T) {
	result := buildPackageWorkSummary(nil, nil)
	if result != nil {
		t.Errorf("expected nil for nil summary, got %v", result)
	}
}

func TestBuildPackageWorkSummary_EmptyFiles(t *testing.T) {
	sess := &sessionSummary{
		EntitiesExamined: []string{"SomeFunc"},
	}
	result := buildPackageWorkSummary(sess, nil)
	if result != nil {
		t.Errorf("expected nil when no files touched, got %v", result)
	}
}

func TestBuildPackageWorkSummary_GroupsByDirectory(t *testing.T) {
	sess := &sessionSummary{
		FilesTouched: []string{
			"internal/store/store.go",
			"internal/store/memories.go",
			"internal/mcp/tools.go",
		},
		EntitiesExamined: []string{},
	}
	result := buildPackageWorkSummary(sess, nil)
	if len(result) != 2 {
		t.Fatalf("expected 2 packages, got %d: %v", len(result), result)
	}
	// Sorted by package name: internal/mcp before internal/store
	if result[0].Package != "internal/mcp" {
		t.Errorf("expected first package=internal/mcp, got %q", result[0].Package)
	}
	if result[1].Package != "internal/store" {
		t.Errorf("expected second package=internal/store, got %q", result[1].Package)
	}
	if len(result[0].Files) != 1 {
		t.Errorf("internal/mcp: expected 1 file, got %d", len(result[0].Files))
	}
	if len(result[1].Files) != 2 {
		t.Errorf("internal/store: expected 2 files, got %d", len(result[1].Files))
	}
}

func TestBuildPackageWorkSummary_SortsFilesAndEntities(t *testing.T) {
	sess := &sessionSummary{
		FilesTouched:     []string{"pkg/auth/auth.go"},
		EntitiesExamined: []string{"ZFunc", "AFunc"},
	}

	g := graph.New("test")
	g.AddNode(&graph.Node{
		ID:   g.MakeNodeID("pkg/auth/auth.go", "ZFunc"),
		Name: "ZFunc",
		File: "pkg/auth/auth.go",
		Type: graph.NodeFunction,
	})
	g.AddNode(&graph.Node{
		ID:   g.MakeNodeID("pkg/auth/auth.go", "AFunc"),
		Name: "AFunc",
		File: "pkg/auth/auth.go",
		Type: graph.NodeFunction,
	})

	result := buildPackageWorkSummary(sess, g)
	if len(result) != 1 {
		t.Fatalf("expected 1 package, got %d", len(result))
	}
	pw := result[0]
	if !sort.StringsAreSorted(pw.Entities) {
		t.Errorf("entities not sorted: %v", pw.Entities)
	}
	if len(pw.Entities) != 2 {
		t.Errorf("expected 2 entities, got %d: %v", len(pw.Entities), pw.Entities)
	}
}

func TestBuildPackageWorkSummary_EntityOnlyAddedIfFileTouched(t *testing.T) {
	// Entity lives in a file that was NOT touched — should not appear.
	sess := &sessionSummary{
		FilesTouched:     []string{"internal/store/store.go"},
		EntitiesExamined: []string{"AuthLogin", "Store.Close"},
	}
	g := graph.New("test")
	g.AddNode(&graph.Node{
		ID:   g.MakeNodeID("pkg/auth/auth.go", "AuthLogin"),
		Name: "AuthLogin",
		File: "pkg/auth/auth.go", // NOT in FilesTouched
		Type: graph.NodeFunction,
	})
	g.AddNode(&graph.Node{
		ID:   g.MakeNodeID("internal/store/store.go", "Store.Close"),
		Name: "Store.Close",
		File: "internal/store/store.go", // IS in FilesTouched
		Type: graph.NodeFunction,
	})

	result := buildPackageWorkSummary(sess, g)
	if len(result) != 1 {
		t.Fatalf("expected 1 package (internal/store), got %d: %v", len(result), result)
	}
	pw := result[0]
	if pw.Package != "internal/store" {
		t.Errorf("expected package=internal/store, got %q", pw.Package)
	}
	if len(pw.Entities) != 1 || pw.Entities[0] != "Store.Close" {
		t.Errorf("expected [Store.Close], got %v", pw.Entities)
	}
}

func TestBuildPackageWorkSummary_RootFilePackage(t *testing.T) {
	// File at repo root: filepath.Dir returns "." — should map to "<root>".
	sess := &sessionSummary{
		FilesTouched: []string{"main.go"},
	}
	result := buildPackageWorkSummary(sess, nil)
	if len(result) != 1 {
		t.Fatalf("expected 1 package, got %d", len(result))
	}
	if result[0].Package != "<root>" {
		t.Errorf("expected package=<root>, got %q", result[0].Package)
	}
}

func TestBuildPackageWorkSummary_CapsAt20Packages(t *testing.T) {
	var files []string
	for i := 0; i < 30; i++ {
		files = append(files, filepath.Join("internal", "pkg"+string(rune('a'+i)), "file.go"))
	}
	sess := &sessionSummary{FilesTouched: files}
	result := buildPackageWorkSummary(sess, nil)
	if len(result) > 20 {
		t.Errorf("expected at most 20 packages, got %d", len(result))
	}
}

// ── GetLatestWorkSummary store tests ────────────────────────────────────────

func TestGetLatestWorkSummary_NilWhenNone(t *testing.T) {
	st := openMCPTestStore(t)
	m, err := st.GetLatestWorkSummary("agent-1")
	if err != nil {
		t.Fatalf("GetLatestWorkSummary: %v", err)
	}
	if m != nil {
		t.Errorf("expected nil when no work summary exists, got %+v", m)
	}
}

func TestGetLatestWorkSummary_EmptyAgentID(t *testing.T) {
	st := openMCPTestStore(t)
	m, err := st.GetLatestWorkSummary("")
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		t.Error("expected nil for empty agent ID")
	}
}

func TestGetLatestWorkSummary_ReturnsLatest(t *testing.T) {
	st := openMCPTestStore(t)

	pkgs1 := []PackageWork{{Package: "internal/store", Files: []string{"store.go"}}}
	pkgs2 := []PackageWork{{Package: "internal/mcp", Files: []string{"tools.go"}}}

	j1, _ := json.Marshal(pkgs1)
	j2, _ := json.Marshal(pkgs2)

	_, err := st.InsertMemory(store.Memory{
		Tier:    store.TierSessionLog,
		Content: string(j1),
		AgentID: "agent-1",
		Source:  store.SourceAuto,
		Tags:    `["work_summary","session_end","auto"]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.InsertMemory(store.Memory{
		Tier:    store.TierSessionLog,
		Content: string(j2),
		AgentID: "agent-1",
		Source:  store.SourceAuto,
		Tags:    `["work_summary","session_end","auto"]`,
	})
	if err != nil {
		t.Fatal(err)
	}

	m, err := st.GetLatestWorkSummary("agent-1")
	if err != nil {
		t.Fatalf("GetLatestWorkSummary: %v", err)
	}
	if m == nil {
		t.Fatal("expected a work summary, got nil")
	}
	// Should be the second one (latest = internal/mcp).
	var got []PackageWork
	if err := json.Unmarshal([]byte(m.Content), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 1 || got[0].Package != "internal/mcp" {
		t.Errorf("expected latest work summary (internal/mcp), got %v", got)
	}
}

func TestGetLatestWorkSummary_IgnoresOtherTags(t *testing.T) {
	st := openMCPTestStore(t)

	// Insert a regular session-log memory (no work_summary tag).
	_, err := st.InsertMemory(store.Memory{
		Tier:    store.TierSessionLog,
		Content: "Session by agent-1 at 2026-03-19. Files: foo.go.",
		AgentID: "agent-1",
		Source:  store.SourceAuto,
		Tags:    `["session_end","auto"]`,
	})
	if err != nil {
		t.Fatal(err)
	}

	m, err := st.GetLatestWorkSummary("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		t.Errorf("expected nil (no work_summary tag), got %+v", m)
	}
}

func TestGetLatestWorkSummary_IgnoresOtherAgents(t *testing.T) {
	st := openMCPTestStore(t)

	pkgs := []PackageWork{{Package: "internal/store", Files: []string{"store.go"}}}
	j, _ := json.Marshal(pkgs)

	_, _ = st.InsertMemory(store.Memory{
		Tier:    store.TierSessionLog,
		Content: string(j),
		AgentID: "agent-other",
		Source:  store.SourceAuto,
		Tags:    `["work_summary","session_end","auto"]`,
	})

	m, err := st.GetLatestWorkSummary("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		t.Errorf("expected nil for different agent, got %+v", m)
	}
}

// ── E2E: end_session stores work summary → session_init surfaces it ──────────

func TestRX4_E2E_WorkSummaryRoundtrip(t *testing.T) {
	srv, loginID, _ := newPopulatedServer(t)

	// Simulate: agent examined AuthLogin (which lives in pkg/auth/auth.go).
	// Emit an agent_examining event so extractSessionSummary picks it up.
	_ = srv.store.AppendEvent("agent_examining", "agent-rx4",
		`{"entity":"AuthLogin"}`)

	// Simulate a file change so the intersection logic considers the file "touched".
	// We mock this by inserting a file_change event AND by populating the
	// change source. Since newPopulatedServer doesn't attach a watcher, we
	// inject a work summary directly to test the storage + retrieval path.
	_ = loginID // used via graph in buildPackageWorkSummary

	// Directly store a work summary as end_session would.
	pkgs := []PackageWork{
		{
			Package:  "pkg/auth",
			Files:    []string{"pkg/auth/auth.go"},
			Entities: []string{"AuthLogin"},
		},
	}
	pkgJSON, _ := json.Marshal(pkgs)
	_, err := srv.store.InsertMemory(store.Memory{
		Tier:    store.TierSessionLog,
		Content: string(pkgJSON),
		AgentID: "agent-rx4",
		Source:  store.SourceAuto,
		Tags:    `["work_summary","session_end","auto"]`,
	})
	if err != nil {
		t.Fatalf("InsertMemory: %v", err)
	}

	// Now call session_init for the same agent — should surface previous_session_work.
	result, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "agent-rx4",
	}))
	m := mustResult(t, result, err)

	hasKey(t, m, "previous_session_work")
	psw, ok := m["previous_session_work"].(map[string]any)
	if !ok {
		t.Fatalf("previous_session_work is not a map: %T", m["previous_session_work"])
	}
	pkgsAny, ok := psw["packages"].([]any)
	if !ok || len(pkgsAny) == 0 {
		t.Fatal("expected at least 1 package in previous_session_work")
	}
	first, ok := pkgsAny[0].(map[string]any)
	if !ok {
		t.Fatalf("package entry is not a map: %T", pkgsAny[0])
	}
	if first["package"] != "pkg/auth" {
		t.Errorf("expected package=pkg/auth, got %v", first["package"])
	}
	if psw["note"] == "" {
		t.Error("expected non-empty note in previous_session_work")
	}
}

func TestRX4_E2E_AbsentWhenNoWorkSummary(t *testing.T) {
	srv := newTestServer(t)

	// No work summary in store — previous_session_work should be absent.
	result, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "agent-fresh",
	}))
	m := mustResult(t, result, err)
	noKey(t, m, "previous_session_work")
}

func TestRX4_E2E_AbsentInQuickMode(t *testing.T) {
	srv := newTestServer(t)

	// Insert a work summary.
	pkgs := []PackageWork{{Package: "internal/store", Files: []string{"store.go"}}}
	j, _ := json.Marshal(pkgs)
	_, _ = srv.store.InsertMemory(store.Memory{
		Tier:    store.TierSessionLog,
		Content: string(j),
		AgentID: "agent-quick",
		Source:  store.SourceAuto,
		Tags:    `["work_summary","session_end","auto"]`,
	})

	// quick mode should NOT include previous_session_work.
	result, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "agent-quick",
		"scope":    "quick",
	}))
	m := mustResult(t, result, err)
	noKey(t, m, "previous_session_work")
}

func TestRX4_HandleEndSession_StoresWorkSummary(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)

	// Wire a stub change source so extractSessionSummary sees the file as modified.
	srv.SetChangeSource(newStubChangeSource("pkg/auth/auth.go"))

	// Emit an agent_examining event so the entity is picked up.
	_ = srv.store.AppendEvent("agent_examining", "agent-es",
		`{"entity":"AuthLogin"}`)

	// Call end_session.
	result, err := srv.handleEndSession(ctx, callTool(map[string]any{
		"agent_id": "agent-es",
		"summary":  "implemented auth changes",
	}))
	if err != nil {
		t.Fatalf("handleEndSession: %v", err)
	}
	if result == nil || result.IsError {
		t.Fatalf("unexpected error result: %v", result)
	}

	// Verify work summary was stored in the memory store.
	m, err := srv.store.GetLatestWorkSummary("agent-es")
	if err != nil {
		t.Fatalf("GetLatestWorkSummary: %v", err)
	}
	if m == nil {
		t.Fatal("expected work summary to be stored after end_session")
	}
	// handleEndSession now stores a workSummaryEnvelope (not raw []PackageWork).
	var env workSummaryEnvelope
	if err := json.Unmarshal([]byte(m.Content), &env); err != nil {
		t.Fatalf("unmarshal work summary envelope: %v", err)
	}
	if env.SessionAt == 0 {
		t.Error("expected non-zero session_at in work summary envelope")
	}
	pkgs := env.Packages
	if len(pkgs) == 0 {
		t.Fatal("expected at least 1 package in work summary")
	}
	// AuthLogin lives in pkg/auth/auth.go → package should be pkg/auth.
	found := false
	for _, pw := range pkgs {
		if pw.Package == "pkg/auth" {
			found = true
			if len(pw.Files) == 0 {
				t.Error("expected files in pkg/auth package work")
			}
			break
		}
	}
	if !found {
		t.Errorf("expected pkg/auth package in work summary, got: %v", pkgs)
	}
}

// TestWorkSummaryEnvelope_PreventsDedup verifies that two consecutive
// end_session calls that touch identical files+entities both persist a work
// summary. Without the SessionAt timestamp nonce the Jaccard dedup in
// InsertMemory (threshold 0.85) would silently drop the second one.
func TestWorkSummaryEnvelope_PreventsDedup(t *testing.T) {
	st := openMCPTestStore(t)

	pkgs := []PackageWork{{Package: "internal/store", Files: []string{"store.go"}}}

	const ts1 int64 = 1710835200 // 2026-03-19 10:00:00 UTC
	const ts2 int64 = 1710849600 // 2026-03-19 14:00:00 UTC

	env1 := workSummaryEnvelope{Packages: pkgs, SessionAt: ts1}
	env2 := workSummaryEnvelope{Packages: pkgs, SessionAt: ts2}

	j1, _ := json.Marshal(env1)
	j2, _ := json.Marshal(env2)

	// Both should be stored without dedup collision.
	id1, err := st.InsertMemory(store.Memory{
		Tier: store.TierSessionLog, Content: string(j1), AgentID: "agent-1",
		Source: store.SourceAuto, Tags: `["work_summary","session_end","auto"]`,
	})
	if err != nil {
		t.Fatalf("insert 1: %v", err)
	}
	id2, err := st.InsertMemory(store.Memory{
		Tier: store.TierSessionLog, Content: string(j2), AgentID: "agent-1",
		Source: store.SourceAuto, Tags: `["work_summary","session_end","auto"]`,
	})
	if err != nil {
		t.Fatalf("insert 2: %v", err)
	}
	if id1 == id2 {
		t.Error("dedup fired: second work summary merged into first (SessionAt nonce not distinctive enough)")
	}

	// GetLatestWorkSummary should return the second one.
	m, err := st.GetLatestWorkSummary("agent-1")
	if err != nil || m == nil {
		t.Fatalf("GetLatestWorkSummary: err=%v mem=%v", err, m)
	}
	var got workSummaryEnvelope
	if json.Unmarshal([]byte(m.Content), &got) != nil || got.SessionAt != ts2 {
		t.Errorf("expected second envelope (ts2=%d), got: %v", ts2, m.Content)
	}
}

// TestGetRecentlyModifiedFiles_UsesSessionWindow verifies that a session older
// than 30 minutes produces a window wider than 30 minutes, so file changes made
// at the start of a long session are not silently dropped.
func TestGetRecentlyModifiedFiles_UsesSessionWindow(t *testing.T) {
	// Stub that records the windowMinutes it was called with.
	called := 0
	gotWindow := 0
	stub := &capturingChangeSource{onRecentChanges: func(w int) []watcher.ChangeEvent {
		called++
		gotWindow = w
		return nil
	}}

	srv := newTestServer(t)
	srv.SetChangeSource(stub)

	// Session started 90 minutes ago.
	sessionStart := time.Now().Add(-90 * time.Minute)
	srv.getRecentlyModifiedFiles(sessionStart)

	if called != 1 {
		t.Fatalf("RecentChanges called %d times, expected 1", called)
	}
	// Window must be at least 90+5 = 95 minutes (session elapsed + 5-min buffer).
	if gotWindow < 95 {
		t.Errorf("expected window >= 95 min for 90-min session, got %d", gotWindow)
	}
}

// capturingChangeSource is a test double that records the windowMinutes argument.
type capturingChangeSource struct {
	onRecentChanges func(int) []watcher.ChangeEvent
}

func (c *capturingChangeSource) RecentChanges(windowMinutes int) []watcher.ChangeEvent {
	return c.onRecentChanges(windowMinutes)
}
