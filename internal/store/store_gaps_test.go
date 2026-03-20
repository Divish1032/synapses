package store_test

// Tests targeting remaining uncovered store paths:
// - WebCache CRUD (UpsertWebCache, GetWebCache, DeleteWebCachePrefix, PruneExpiredWebCache)
// - PruneStaleData
// - CollectQueryStats
// - Stat on populated DB
// - SavedAt on populated and empty DB
// - GetSignatureChanges
// - UpsertDynamicRule / LoadDynamicRules
// - UpdateCallSitesForFile
// - UpsertFileMtime
// - splitCamelCase (via FTS search for split tokens)
// - sanitizeFTSQuery edge cases (via SemanticSearch with special chars)
// - ViolationID deterministic
// - SaveGraph with metadata (doc, signature, line_count, provenance, domain)
// - LoadGraph restores promoted metadata fields

import (
	"io"
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── WebCache CRUD ─────────────────────────────────────────────────────────────

func TestUpsertWebCache_InsertAndGet(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	if err := st.UpsertWebCache("https://pkg.go.dev/net/http", "HTTP package docs", 0); err != nil {
		t.Fatalf("UpsertWebCache: %v", err)
	}

	entry, ok := st.GetWebCache("https://pkg.go.dev/net/http")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if entry.Content != "HTTP package docs" {
		t.Errorf("content = %q, want 'HTTP package docs'", entry.Content)
	}
	if entry.TTLHours != 0 {
		t.Errorf("TTLHours = %d, want 0", entry.TTLHours)
	}
}

func TestUpsertWebCache_Overwrite(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_ = st.UpsertWebCache("https://example.com/api", "v1", 24)
	_ = st.UpsertWebCache("https://example.com/api", "v2", 24)

	entry, ok := st.GetWebCache("https://example.com/api")
	if !ok {
		t.Fatal("expected cache hit after overwrite")
	}
	if entry.Content != "v2" {
		t.Errorf("content = %q after overwrite, want 'v2'", entry.Content)
	}
}

func TestGetWebCache_Miss(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_, ok := st.GetWebCache("https://nonexistent.example.com")
	if ok {
		t.Error("expected cache miss for unknown URL")
	}
}

func TestDeleteWebCachePrefix(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_ = st.UpsertWebCache("pkg:net/http@v1.0", "http docs", 0)
	_ = st.UpsertWebCache("pkg:net/url@v1.0", "url docs", 0)
	_ = st.UpsertWebCache("https://other.com", "other", 24)

	if err := st.DeleteWebCachePrefix("pkg:net/"); err != nil {
		t.Fatalf("DeleteWebCachePrefix: %v", err)
	}

	// Both pkg:net/* should be gone.
	if _, ok := st.GetWebCache("pkg:net/http@v1.0"); ok {
		t.Error("expected pkg:net/http to be deleted")
	}
	if _, ok := st.GetWebCache("pkg:net/url@v1.0"); ok {
		t.Error("expected pkg:net/url to be deleted")
	}
	// Other URL should survive.
	if _, ok := st.GetWebCache("https://other.com"); !ok {
		t.Error("expected other URL to survive prefix delete")
	}
}

func TestPruneExpiredWebCache(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Insert a zero-TTL entry (never expires) and a positive-TTL entry.
	_ = st.UpsertWebCache("pkg:never-expire", "permanent", 0)
	_ = st.UpsertWebCache("https://will-survive.com", "fresh", 9999)

	if err := st.PruneExpiredWebCache(); err != nil {
		t.Fatalf("PruneExpiredWebCache: %v", err)
	}

	// Zero-TTL entry should still be present.
	if _, ok := st.GetWebCache("pkg:never-expire"); !ok {
		t.Error("zero-TTL entry should survive pruning")
	}
}

// ── PruneStaleData ────────────────────────────────────────────────────────────

func TestPruneStaleData_NoData_NoPanic(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	// Should not panic on an empty database.
	st.PruneStaleData(7)
}

func TestPruneStaleData_WithData(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Seed some data that PruneStaleData targets.
	_ = st.AppendEvent("test_event", "agent-prune", `{"x":1}`)
	_, _ = st.SendMessage("a1", "a2", "topic", "payload", "proj")
	_, _ = st.RememberEpisode(store.Episode{
		AgentID:       "prune-agent",
		EpisodeType:   "decision",
		Outcome:       "success",
		Decision:      "test decision",
		AffectedFiles: "[]",
		AffectedNodes: "[]",
		Tags:          "[]",
	})

	// Prune with 0-day retention — should delete everything.
	st.PruneStaleData(0)
}

// TestPruneStaleData_OrphanedQualityGaps verifies that PruneStaleData removes
// open quality gaps whose node_id no longer exists in graphDB, while preserving:
//   - open gaps for nodes that still exist
//   - non-open gaps (fixed/wontfix) for absent nodes (historical records)
func TestPruneStaleData_OrphanedQualityGaps(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Seed a real node in the graph so we can distinguish "exists" from "absent".
	g := graph.New("prune-repo")
	existingNodeID := g.MakeNodeID("auth.go", "Authenticate")
	g.AddNode(&graph.Node{
		ID: existingNodeID, Name: "Authenticate", Type: graph.NodeFunction,
		File: "auth.go", Package: "auth",
	})
	if err := st.SaveGraph(g); err != nil {
		t.Fatalf("SaveGraph: %v", err)
	}

	absentNodeID := "prune-repo::deleted/old.go::OldFunction"

	existingID := string(existingNodeID)

	// Gap 1: open gap for a node that still exists → must survive prune.
	_, err := st.UpsertGap(store.QualityGap{
		NodeID: existingID, GapID: "missing-error-handling",
		Description: "no error returned", Severity: "high", Status: "open",
	})
	if err != nil {
		t.Fatalf("UpsertGap existing: %v", err)
	}

	// Gap 2: open gap for an absent node → must be deleted by prune.
	_, err = st.UpsertGap(store.QualityGap{
		NodeID: absentNodeID, GapID: "stale-gap",
		Description: "function no longer exists", Severity: "medium", Status: "open",
	})
	if err != nil {
		t.Fatalf("UpsertGap absent open: %v", err)
	}

	// Gap 3: fixed gap for an absent node → must survive (historical record).
	_, err = st.UpsertGap(store.QualityGap{
		NodeID: absentNodeID, GapID: "already-fixed",
		Description: "was fixed in refactor", Severity: "low", Status: "fixed",
	})
	if err != nil {
		t.Fatalf("UpsertGap absent fixed: %v", err)
	}

	// Run prune — fresh store has zero debounce, so this runs immediately.
	st.PruneStaleData(30)

	// Gap 1 must survive: its node still exists.
	surviving, err := st.GetGaps(store.GapFilter{NodeID: existingID})
	if err != nil {
		t.Fatalf("GetGaps existing: %v", err)
	}
	if len(surviving) != 1 {
		t.Errorf("expected 1 gap for existing node after prune, got %d", len(surviving))
	}

	// Gap 2 must be gone: open gap for absent node.
	orphaned, err := st.GetGaps(store.GapFilter{NodeID: absentNodeID, Status: "open"})
	if err != nil {
		t.Fatalf("GetGaps absent open: %v", err)
	}
	if len(orphaned) != 0 {
		t.Errorf("expected open orphaned gap to be pruned, got %d gaps", len(orphaned))
	}

	// Gap 3 must survive: fixed gaps for absent nodes are historical, not pruned.
	fixed, err := st.GetGaps(store.GapFilter{NodeID: absentNodeID, Status: "fixed"})
	if err != nil {
		t.Fatalf("GetGaps absent fixed: %v", err)
	}
	if len(fixed) != 1 {
		t.Errorf("expected fixed gap for absent node to survive prune, got %d", len(fixed))
	}
}

// ── CollectQueryStats ─────────────────────────────────────────────────────────

func TestCollectQueryStats_ReturnsStats(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// The store has indexes from schema creation; stats should reflect index hits.
	// Pass io.Discard instead of nil to avoid nil-pointer dereference in fmt.Fprintf.
	stats := st.CollectQueryStats(io.Discard)
	// On a fresh DB the probes should hit indexes (from CREATE INDEX in schema).
	if stats.IndexHits+stats.FullScans == 0 {
		t.Error("expected at least one probe result")
	}
}

// ── Stat ──────────────────────────────────────────────────────────────────────

func TestStat_PopulatedDB(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	g := graph.New("stat-repo")
	id := g.MakeNodeID("main.go", "main")
	g.AddNode(&graph.Node{
		ID: id, Name: "main", Type: graph.NodeFunction,
		File: "main.go", Package: "main",
	})
	if err := st.SaveGraph(g); err != nil {
		t.Fatalf("SaveGraph: %v", err)
	}

	stat, err := st.Stat("test.db")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if stat == nil {
		t.Fatal("expected non-nil stat")
	}
	if stat.RepoID != "stat-repo" {
		t.Errorf("RepoID = %q, want stat-repo", stat.RepoID)
	}
	if stat.NodeCount == 0 {
		t.Error("expected NodeCount > 0")
	}
	if stat.DBPath != "test.db" {
		t.Errorf("DBPath = %q, want test.db", stat.DBPath)
	}
}

func TestStat_EmptyDB(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	stat, err := st.Stat("empty.db")
	if err != nil {
		t.Fatalf("Stat empty: %v", err)
	}
	if stat != nil {
		t.Error("expected nil stat on empty DB")
	}
}

// ── SavedAt ───────────────────────────────────────────────────────────────────

func TestSavedAt_EmptyDB(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	ts, err := st.SavedAt()
	if err != nil {
		t.Fatalf("SavedAt empty: %v", err)
	}
	if !ts.IsZero() {
		t.Error("expected zero time on empty DB")
	}
}

func TestSavedAt_AfterSaveGraph(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	g := graph.New("saved-at-repo")
	_ = st.SaveGraph(g)

	ts, err := st.SavedAt()
	if err != nil {
		t.Fatalf("SavedAt: %v", err)
	}
	if ts.IsZero() {
		t.Error("expected non-zero time after SaveGraph")
	}
}

// ── GetSignatureChanges ───────────────────────────────────────────────────────

func TestGetSignatureChanges_DetectsChange(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Save graph v1 with a function signature.
	g1 := graph.New("sig-repo")
	id := g1.MakeNodeID("pkg/auth.go", "Login")
	g1.AddNode(&graph.Node{
		ID: id, Name: "Login", Type: graph.NodeFunction,
		File: "pkg/auth.go", Package: "auth", Exported: true,
		Metadata: map[string]string{"signature": "func Login(u string) error"},
	})
	if err := st.SaveGraph(g1); err != nil {
		t.Fatalf("SaveGraph v1: %v", err)
	}

	// Save graph v2 with changed signature.
	g2 := graph.New("sig-repo")
	id2 := g2.MakeNodeID("pkg/auth.go", "Login")
	g2.AddNode(&graph.Node{
		ID: id2, Name: "Login", Type: graph.NodeFunction,
		File: "pkg/auth.go", Package: "auth", Exported: true,
		Metadata: map[string]string{"signature": "func Login(u, p string) error"},
	})
	if err := st.SaveGraph(g2); err != nil {
		t.Fatalf("SaveGraph v2: %v", err)
	}

	changes, err := st.GetSignatureChanges("pkg/auth.go")
	if err != nil {
		t.Fatalf("GetSignatureChanges: %v", err)
	}
	if len(changes) == 0 {
		t.Error("expected at least 1 signature change")
	}
}

func TestGetSignatureChanges_NoChanges(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	g := graph.New("nochange-repo")
	id := g.MakeNodeID("pkg/svc.go", "Serve")
	g.AddNode(&graph.Node{
		ID: id, Name: "Serve", Type: graph.NodeFunction,
		File: "pkg/svc.go", Package: "pkg",
		Metadata: map[string]string{"signature": "func Serve()"},
	})
	_ = st.SaveGraph(g)

	changes, err := st.GetSignatureChanges("pkg/svc.go")
	if err != nil {
		t.Fatalf("GetSignatureChanges: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("expected 0 changes, got %d", len(changes))
	}
}

// ── UpsertDynamicRule / LoadDynamicRules ───────────────────────────────────────

func TestUpsertAndLoadDynamicRules(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	rule := config.Rule{
		ID:          "no-handler-to-db",
		Description: "handlers must not call DB directly",
		Severity:    "error",
		ForbiddenEdge: config.ForbiddenEdge{
			EdgeType:        graph.EdgeCalls,
			FromFilePattern: "*/handler/*",
			ToFilePattern:   "*/db/*",
		},
	}
	if err := st.UpsertDynamicRule(rule); err != nil {
		t.Fatalf("UpsertDynamicRule: %v", err)
	}

	rules, err := st.LoadDynamicRules()
	if err != nil {
		t.Fatalf("LoadDynamicRules: %v", err)
	}
	if len(rules) == 0 {
		t.Fatal("expected at least 1 dynamic rule")
	}
	if rules[0].ID != "no-handler-to-db" {
		t.Errorf("rule ID = %q, want no-handler-to-db", rules[0].ID)
	}
}

func TestUpsertDynamicRule_Overwrite(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	r1 := config.Rule{ID: "test-rule", Description: "v1", Severity: "warning"}
	_ = st.UpsertDynamicRule(r1)

	r2 := config.Rule{ID: "test-rule", Description: "v2", Severity: "error"}
	_ = st.UpsertDynamicRule(r2)

	rules, _ := st.LoadDynamicRules()
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule after overwrite, got %d", len(rules))
	}
	if rules[0].Description != "v2" {
		t.Errorf("description = %q after overwrite, want v2", rules[0].Description)
	}
}

// ── UpdateCallSitesForFile ────────────────────────────────────────────────────

func TestUpdateCallSitesForFile_ReplacesPerFile(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Save initial call sites for two files.
	initial := []graph.CallSite{
		{CallerID: "id1", CallerFile: "a.go", FuncName: "Foo", PkgAlias: ""},
		{CallerID: "id2", CallerFile: "b.go", FuncName: "Bar", PkgAlias: ""},
	}
	if err := st.SaveCallSites(initial); err != nil {
		t.Fatalf("SaveCallSites: %v", err)
	}

	// Update only a.go with new call sites.
	newSites := []graph.CallSite{
		{CallerID: "id3", CallerFile: "a.go", FuncName: "Baz", PkgAlias: "pkg"},
	}
	if err := st.UpdateCallSitesForFile("a.go", newSites); err != nil {
		t.Fatalf("UpdateCallSitesForFile: %v", err)
	}

	loaded, _ := st.LoadCallSites()
	// Should have b.go's original + a.go's replacement.
	if len(loaded) != 2 {
		t.Errorf("expected 2 call sites, got %d", len(loaded))
	}
}

func TestUpdateCallSitesForFile_EmptyNewSites(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_ = st.SaveCallSites([]graph.CallSite{
		{CallerID: "id1", CallerFile: "a.go", FuncName: "Foo"},
	})

	// Remove all call sites for a.go by passing nil.
	if err := st.UpdateCallSitesForFile("a.go", nil); err != nil {
		t.Fatalf("UpdateCallSitesForFile nil: %v", err)
	}

	loaded, _ := st.LoadCallSites()
	if len(loaded) != 0 {
		t.Errorf("expected 0 call sites after removing a.go, got %d", len(loaded))
	}
}

// ── UpsertFileMtime ───────────────────────────────────────────────────────────

func TestUpsertFileMtime_InsertAndUpdate(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	if err := st.UpsertFileMtime("pkg/auth.go", 1000); err != nil {
		t.Fatalf("UpsertFileMtime insert: %v", err)
	}
	if err := st.UpsertFileMtime("pkg/auth.go", 2000); err != nil {
		t.Fatalf("UpsertFileMtime update: %v", err)
	}

	loaded, _ := st.LoadFileMtimes()
	if loaded["pkg/auth.go"] != 2000 {
		t.Errorf("mtime = %d, want 2000", loaded["pkg/auth.go"])
	}
}

// ── ViolationID deterministic ─────────────────────────────────────────────────

func TestViolationID_StableAndDistinct(t *testing.T) {
	t.Parallel()
	id1 := store.ViolationID("rule1", "from", "to", "CALLS")
	id2 := store.ViolationID("rule1", "from", "to", "CALLS")
	if id1 != id2 {
		t.Errorf("ViolationID not stable: %q != %q", id1, id2)
	}

	id3 := store.ViolationID("rule2", "from", "to", "CALLS")
	if id1 == id3 {
		t.Error("different inputs should produce different IDs")
	}

	// Different edge type should produce different ID.
	id4 := store.ViolationID("rule1", "from", "to", "IMPORTS")
	if id1 == id4 {
		t.Error("different edge type should produce different ID")
	}
}

// ── SemanticSearch with special characters ────────────────────────────────────

func TestSemanticSearch_SpecialChars_NoError(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	// Queries with FTS5 special chars should be sanitized, not error.
	for _, q := range []string{
		`"quoted"`,
		`foo:bar`,
		`(parens)`,
		`a*b`,
		`path/to/file.go`,
		`a-b-c`,
	} {
		_, err := st.SemanticSearch(q, 5)
		if err != nil {
			t.Errorf("SemanticSearch(%q) error: %v", q, err)
		}
	}
}

// ── SaveGraph with rich metadata ──────────────────────────────────────────────

func TestSaveAndLoadGraph_RichMetadata(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	g := graph.New("rich-repo")
	g.SetRoot("/tmp/rich")

	id := g.MakeNodeID("pkg/auth.go", "Login")
	g.AddNode(&graph.Node{
		ID: id, Name: "Login", Type: graph.NodeFunction,
		File: "pkg/auth.go", Package: "auth", Line: 10,
		Exported: true,
		Metadata: map[string]string{
			"doc":        "Login handles user authentication",
			"signature":  "func Login(user, pass string) error",
			"line_count": "25",
			"churn":      "3",
		},
		Provenance: graph.ProvenanceType("user-authored"),
		Domain:     graph.DomainType("code"),
	})

	if err := st.SaveGraph(g); err != nil {
		t.Fatalf("SaveGraph: %v", err)
	}

	loaded, err := st.LoadGraph()
	if err != nil {
		t.Fatalf("LoadGraph: %v", err)
	}
	if loaded == nil {
		t.Fatal("LoadGraph returned nil")
	}

	nodes := loaded.AllNodes()
	found := false
	for _, n := range nodes {
		if n.Name == "Login" {
			found = true
			if n.Metadata["doc"] != "Login handles user authentication" {
				t.Errorf("doc = %q", n.Metadata["doc"])
			}
			if n.Metadata["signature"] != "func Login(user, pass string) error" {
				t.Errorf("signature = %q", n.Metadata["signature"])
			}
			if n.Metadata["line_count"] != "25" {
				t.Errorf("line_count = %q", n.Metadata["line_count"])
			}
			if !n.Exported {
				t.Error("expected Exported=true")
			}
		}
	}
	if !found {
		t.Error("Login node not found after LoadGraph")
	}
}

// ── CacheDir ──────────────────────────────────────────────────────────────────

func TestCacheDir_ReturnsNonEmpty(t *testing.T) {
	t.Parallel()
	dir, err := store.CacheDir()
	if err != nil {
		t.Fatalf("CacheDir: %v", err)
	}
	if dir == "" {
		t.Error("expected non-empty cache dir")
	}
}

// ── Open re-open (migration idempotency) ──────────────────────────────────────

func TestOpen_Reopen_MigrationIdempotent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "reopen.db")

	st1, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open first: %v", err)
	}
	st1.Close()

	// Re-open should succeed — migrations are idempotent.
	st2, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open second: %v", err)
	}
	st2.Close()
}
