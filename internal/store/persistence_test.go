package store_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── SaveCallSites / LoadCallSites ─────────────────────────────────────────────

func TestSaveAndLoadCallSites_RoundTrip(t *testing.T) {
	st := openTestStore(t)

	sites := []graph.CallSite{
		{CallerID: "pkg/auth/auth.go:Login", CallerFile: "pkg/auth/auth.go", PkgAlias: "", FuncName: "bcrypt.Hash"},
		{CallerID: "pkg/api/handler.go:Handle", CallerFile: "pkg/api/handler.go", PkgAlias: "auth", FuncName: "Login"},
	}

	if err := st.SaveCallSites(sites); err != nil {
		t.Fatalf("SaveCallSites: %v", err)
	}

	loaded, err := st.LoadCallSites()
	if err != nil {
		t.Fatalf("LoadCallSites: %v", err)
	}
	if len(loaded) != 2 {
		t.Errorf("expected 2 call sites, got %d", len(loaded))
	}
}

func TestSaveCallSites_Replaces(t *testing.T) {
	st := openTestStore(t)

	// Save 3 sites.
	first := []graph.CallSite{
		{CallerID: "a", CallerFile: "a.go", FuncName: "F1"},
		{CallerID: "b", CallerFile: "b.go", FuncName: "F2"},
		{CallerID: "c", CallerFile: "c.go", FuncName: "F3"},
	}
	_ = st.SaveCallSites(first)

	// Save 1 site — should replace, not append.
	second := []graph.CallSite{
		{CallerID: "x", CallerFile: "x.go", FuncName: "FX"},
	}
	_ = st.SaveCallSites(second)

	loaded, _ := st.LoadCallSites()
	if len(loaded) != 1 {
		t.Errorf("expected 1 call site after replace, got %d", len(loaded))
	}
}

func TestLoadCallSites_Empty(t *testing.T) {
	st := openTestStore(t)

	sites, err := st.LoadCallSites()
	if err != nil {
		t.Fatalf("LoadCallSites empty: %v", err)
	}
	if sites != nil && len(sites) != 0 {
		t.Errorf("expected nil/empty, got %d", len(sites))
	}
}

// ── LoadFileMtimes / SaveFileMtimes / UpsertFileMtime ─────────────────────────

func TestSaveAndLoadFileMtimes_RoundTrip(t *testing.T) {
	st := openTestStore(t)

	mtimes := map[string]int64{
		"/project/main.go": 1700000001,
		"/project/auth.go": 1700000002,
	}

	if err := st.SaveFileMtimes(mtimes); err != nil {
		t.Fatalf("SaveFileMtimes: %v", err)
	}

	loaded, err := st.LoadFileMtimes()
	if err != nil {
		t.Fatalf("LoadFileMtimes: %v", err)
	}
	for path, mtime := range mtimes {
		got, ok := loaded[path]
		if !ok {
			t.Errorf("missing path %q", path)
		} else if got != mtime {
			t.Errorf("mtime mismatch for %q: got %d, want %d", path, got, mtime)
		}
	}
}

func TestSaveFileMtimes_Replaces(t *testing.T) {
	st := openTestStore(t)

	_ = st.SaveFileMtimes(map[string]int64{"/a.go": 1, "/b.go": 2})
	_ = st.SaveFileMtimes(map[string]int64{"/c.go": 3})

	loaded, _ := st.LoadFileMtimes()
	if len(loaded) != 1 {
		t.Errorf("expected 1 entry after replace, got %d", len(loaded))
	}
	if loaded["/c.go"] != 3 {
		t.Errorf("expected /c.go=3, got %d", loaded["/c.go"])
	}
}

func TestUpsertFileMtime_InsertsAndUpdates(t *testing.T) {
	st := openTestStore(t)

	if err := st.UpsertFileMtime("/project/file.go", 1000); err != nil {
		t.Fatalf("UpsertFileMtime insert: %v", err)
	}

	if err := st.UpsertFileMtime("/project/file.go", 2000); err != nil {
		t.Fatalf("UpsertFileMtime update: %v", err)
	}

	loaded, _ := st.LoadFileMtimes()
	if loaded["/project/file.go"] != 2000 {
		t.Errorf("expected mtime=2000, got %d", loaded["/project/file.go"])
	}
}

// ── UpsertDynamicRule / LoadDynamicRules ──────────────────────────────────────

func TestUpsertAndLoadDynamicRules_RoundTrip(t *testing.T) {
	st := openTestStore(t)

	rule := config.Rule{
		ID:          "no-cmd-imports-internal",
		Description: "cmd must not import internal",
		Severity:    "error",
		ForbiddenEdge: config.ForbiddenEdge{
			FromFilePattern: "cmd/.*",
			ToFilePattern:   "internal/.*",
			EdgeType:        "IMPORTS",
		},
	}

	if err := st.UpsertDynamicRule(rule); err != nil {
		t.Fatalf("UpsertDynamicRule: %v", err)
	}

	rules, err := st.LoadDynamicRules()
	if err != nil {
		t.Fatalf("LoadDynamicRules: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	if rules[0].ID != rule.ID {
		t.Errorf("expected rule ID %q, got %q", rule.ID, rules[0].ID)
	}
	if rules[0].Description != rule.Description {
		t.Errorf("expected description %q, got %q", rule.Description, rules[0].Description)
	}
}

func TestUpsertDynamicRule_UpdatesExisting(t *testing.T) {
	st := openTestStore(t)

	rule := config.Rule{ID: "rule-1", Description: "old", Severity: "warning"}
	_ = st.UpsertDynamicRule(rule)

	rule.Description = "updated"
	rule.Severity = "error"
	_ = st.UpsertDynamicRule(rule)

	rules, _ := st.LoadDynamicRules()
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule (upserted), got %d", len(rules))
	}
	if rules[0].Description != "updated" {
		t.Errorf("expected updated description, got %q", rules[0].Description)
	}
}

// ── SaveIndexSnapshot / LoadIndexSnapshot ─────────────────────────────────────

func TestSaveAndLoadIndexSnapshot_RoundTrip(t *testing.T) {
	st := openTestStore(t)

	payload := []byte("fake-zstd-compressed-graph-data")

	if err := st.SaveIndexSnapshot(payload); err != nil {
		t.Fatalf("SaveIndexSnapshot: %v", err)
	}

	loaded, err := st.LoadIndexSnapshot()
	if err != nil {
		t.Fatalf("LoadIndexSnapshot: %v", err)
	}
	if string(loaded) != string(payload) {
		t.Errorf("snapshot mismatch: got %q, want %q", loaded, payload)
	}
}

func TestLoadIndexSnapshot_Missing(t *testing.T) {
	st := openTestStore(t)

	blob, err := st.LoadIndexSnapshot()
	if err != nil {
		t.Fatalf("LoadIndexSnapshot empty: %v", err)
	}
	if blob != nil {
		t.Errorf("expected nil snapshot when none saved, got %d bytes", len(blob))
	}
}

// ── RecordToolCall / ToolUsageStats ───────────────────────────────────────────

func TestRecordToolCall_AndToolUsageStats(t *testing.T) {
	st := openTestStore(t)

	// Record some tool calls.
	st.RecordToolCall("get_context", "agent-1", "AuthService", 10, true)
	st.RecordToolCall("get_context", "agent-1", "LoginHandler", 20, true)
	st.RecordToolCall("find_entity", "agent-1", "", 5, false) // 1 error

	stats, err := st.ToolUsageStats(7, 10)
	if err != nil {
		t.Fatalf("ToolUsageStats: %v", err)
	}
	if len(stats) == 0 {
		t.Fatal("expected at least 1 stat")
	}

	for _, s := range stats {
		if s.ToolName == "get_context" {
			if s.CallCount != 2 {
				t.Errorf("get_context: expected 2 calls, got %d", s.CallCount)
			}
			if s.ErrorRate != 0 {
				t.Errorf("get_context: expected 0 error rate, got %f", s.ErrorRate)
			}
		}
		if s.ToolName == "find_entity" {
			if s.ErrorRate != 1.0 {
				t.Errorf("find_entity: expected error_rate=1.0, got %f", s.ErrorRate)
			}
		}
	}
}

func TestToolUsageStats_EmptyDB(t *testing.T) {
	st := openTestStore(t)

	stats, err := st.ToolUsageStats(7, 10)
	if err != nil {
		t.Fatalf("ToolUsageStats empty: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("expected 0 stats on empty DB, got %d", len(stats))
	}
}

// ── PruneStaleData ────────────────────────────────────────────────────────────

func TestPruneStaleData_DoesNotCrash(t *testing.T) {
	st := openTestStore(t)

	// Record some data first.
	st.RecordToolCall("get_context", "a", "", 5, true)
	_, _ = st.SendMessage("a", "b", "ping", `{}`, "")

	// Should not panic or error.
	st.PruneStaleData(30)
}

// ── CountIndexedFiles ─────────────────────────────────────────────────────────

// ── OF-H1: Domain field persistence ──────────────────────────────────────────

// TestDomainField_RoundTrip verifies that Node.Domain survives a SaveGraph/LoadGraph cycle.
func TestDomainField_RoundTrip(t *testing.T) {
	st := openTestStore(t)
	g := graph.New("testrepo")

	nodeID := g.MakeNodeID("api/openapi.yaml", "GetUsers")
	g.AddNode(&graph.Node{
		ID:     nodeID,
		Type:   graph.NodeType("endpoint"),
		Name:   "GetUsers",
		File:   "api/openapi.yaml",
		Line:   10,
		Domain: graph.DomainAPI,
	})

	if err := st.SaveGraph(g); err != nil {
		t.Fatalf("SaveGraph: %v", err)
	}

	loaded, err := st.LoadGraph()
	if err != nil {
		t.Fatalf("LoadGraph: %v", err)
	}
	n := loaded.GetNode(nodeID)
	if n == nil {
		t.Fatal("node not found after LoadGraph")
	}
	if n.Domain != graph.DomainAPI {
		t.Errorf("expected domain %q, got %q", graph.DomainAPI, n.Domain)
	}
}

// TestDomainField_DefaultsToCode verifies that nodes without an explicit domain
// load as "code" (the default set by the migration and SaveGraph normalisation).
func TestDomainField_DefaultsToCode(t *testing.T) {
	st := openTestStore(t)
	g := graph.New("testrepo")

	nodeID := g.MakeNodeID("main.go", "main")
	g.AddNode(&graph.Node{
		ID:   nodeID,
		Type: graph.NodeFunction,
		Name: "main",
		File: "main.go",
		Line: 1,
		// Domain intentionally omitted — should default to "code".
	})

	if err := st.SaveGraph(g); err != nil {
		t.Fatalf("SaveGraph: %v", err)
	}

	loaded, err := st.LoadGraph()
	if err != nil {
		t.Fatalf("LoadGraph: %v", err)
	}
	n := loaded.GetNode(nodeID)
	if n == nil {
		t.Fatal("node not found after LoadGraph")
	}
	// Domain "code" is stored and loaded correctly.
	if n.Domain != graph.DomainCode {
		t.Errorf("expected domain %q, got %q", graph.DomainCode, n.Domain)
	}
}

// TestDomainField_AllDomains verifies that all defined DomainType constants
// round-trip correctly through SaveGraph/LoadGraph.
func TestDomainField_AllDomains(t *testing.T) {
	domains := []graph.DomainType{
		graph.DomainCode,
		graph.DomainInfra,
		graph.DomainAPI,
		graph.DomainDocs,
		graph.DomainIssues,
		graph.DomainCustom,
	}

	for _, dom := range domains {
		dom := dom
		t.Run(string(dom), func(t *testing.T) {
			st := openTestStore(t)
			g := graph.New("testrepo")

			nodeID := g.MakeNodeID("file.txt", string(dom)+"-node")
			g.AddNode(&graph.Node{
				ID:     nodeID,
				Type:   graph.NodeFunction,
				Name:   string(dom) + "-node",
				File:   "file.txt",
				Line:   1,
				Domain: dom,
			})

			if err := st.SaveGraph(g); err != nil {
				t.Fatalf("SaveGraph: %v", err)
			}
			loaded, err := st.LoadGraph()
			if err != nil {
				t.Fatalf("LoadGraph: %v", err)
			}
			n := loaded.GetNode(nodeID)
			if n == nil {
				t.Fatalf("node not found for domain %q", dom)
			}
			if n.Domain != dom {
				t.Errorf("domain mismatch: got %q, want %q", n.Domain, dom)
			}
		})
	}
}

// ── CountIndexedFiles ─────────────────────────────────────────────────────────

func TestCountIndexedFiles_AfterSaveFileMtimes(t *testing.T) {
	st := openTestStore(t)

	n, err := st.CountIndexedFiles()
	if err != nil {
		t.Fatalf("CountIndexedFiles empty: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 before any files, got %d", n)
	}

	_ = st.SaveFileMtimes(map[string]int64{"/a.go": 1, "/b.go": 2, "/c.go": 3})

	n2, err := st.CountIndexedFiles()
	if err != nil {
		t.Fatalf("CountIndexedFiles after save: %v", err)
	}
	if n2 != 3 {
		t.Errorf("expected 3 indexed files, got %d", n2)
	}
}

// ── R32: Compound indexes (CollectQueryStats) ─────────────────────────────────

// TestR32_AllHotQueriesUseIndexes verifies that every R32 compound index is
// present in the schema and is actually chosen by the SQLite query planner for
// the representative hot queries it was designed to accelerate.
//
// CollectQueryStats runs EXPLAIN QUERY PLAN on each probe query and classifies
// the result as an index hit or full scan. A full scan here means either an
// index is missing (CREATE INDEX was not applied) or the query is written in a
// way that prevents index use — both are bugs.
func TestR32_AllHotQueriesUseIndexes(t *testing.T) {
	st := openTestStore(t)

	stats := st.CollectQueryStats()

	if stats.FullScans > 0 {
		t.Errorf("R32: %d hot queries use full table scans — expected 0 (missing indexes?)", stats.FullScans)
	}
	if stats.IndexHits != 4 {
		t.Errorf("R32: expected 4 index hits, got %d (some compound indexes may be missing)", stats.IndexHits)
	}
}

// TestR32_IndexesIdempotentOnUpgrade verifies that opening the same database
// twice does not fail due to duplicate index errors. All R32 indexes use
// CREATE INDEX IF NOT EXISTS, so re-applying them is always safe.
func TestR32_IndexesIdempotentOnUpgrade(t *testing.T) {
	path := t.TempDir() + "/upgrade.db"

	// First open: creates schema + R32 indexes.
	st1, err := openTestStoreAtPath(t, path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	st1.Close()

	// Second open: must not fail with "index already exists" or any other error.
	st2, err := openTestStoreAtPath(t, path)
	if err != nil {
		t.Fatalf("second Open (idempotency check): %v", err)
	}
	defer st2.Close()

	// Indexes must still work after re-open.
	stats := st2.CollectQueryStats()
	if stats.FullScans > 0 {
		t.Errorf("R32 after re-open: %d full scans (indexes lost on upgrade path?)", stats.FullScans)
	}
}
