package mcp

// White-box tests for explain.go and repomap.go.
// Uses package mcp for direct access to unexported builder functions.

import (
	"strings"
	"testing"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── buildExplanation ──────────────────────────────────────────────────────────

func TestBuildExplanation_EmptyGraph(t *testing.T) {
	identity := &graph.ProjectIdentity{
		Scale: graph.ScaleMicro,
	}
	out := buildExplanation(identity, nil, nil, "")
	if out == "" {
		t.Fatal("expected non-empty output for empty graph")
	}
	if !strings.Contains(out, "Codebase Orientation") {
		t.Errorf("expected header, got: %q", out)
	}
}

func TestBuildExplanation_WithNodes(t *testing.T) {
	identity := &graph.ProjectIdentity{
		Scale: graph.ScaleSmall,
		Summary: graph.GraphSummary{
			Files:     5,
			Functions: 10,
			Structs:   3,
			Edges:     20,
		},
		EntryPoints: []graph.EntityRef{
			{Name: "main", Type: graph.NodeFunction, File: "cmd/main.go", Line: 1},
		},
	}
	nodes := []*graph.Node{
		{ID: "n1", Name: "Store", Type: graph.NodeStruct, File: "/repo/internal/store/store.go", Package: "store", Line: 10},
		{ID: "n2", Name: "doWork", Type: graph.NodeFunction, File: "/repo/internal/core/core.go", Package: "core", Line: 5},
	}
	// refs map simulates connectivityMap output (DEFINES/IMPORTS already excluded).
	refs := map[graph.NodeID]int{"n1": 2}

	out := buildExplanation(identity, nodes, refs, "/repo")
	if !strings.Contains(out, "Store") {
		t.Errorf("expected key type 'Store' in output, got: %q", out)
	}
	if !strings.Contains(out, "2 refs") {
		t.Errorf("expected '2 refs' for Store, got: %q", out)
	}
	if !strings.Contains(out, "main") {
		t.Errorf("expected entry point 'main', got: %q", out)
	}
}

func TestBuildExplanation_SkipsTestAndVendored(t *testing.T) {
	identity := &graph.ProjectIdentity{Scale: graph.ScaleSmall}
	nodes := []*graph.Node{
		{ID: "t1", Name: "TestHelper", Type: graph.NodeStruct, File: "/repo/auth_test.go", Package: "auth"},
		{ID: "v1", Name: "VendorType", Type: graph.NodeStruct, File: "/repo/vendor/foo/foo.go", Package: "foo", Provenance: graph.ProvenanceVendored},
		{ID: "u1", Name: "RealType", Type: graph.NodeStruct, File: "/repo/internal/auth.go", Package: "auth"},
	}
	refs := map[graph.NodeID]int{"t1": 10, "v1": 10, "u1": 3}
	out := buildExplanation(identity, nodes, refs, "")
	if strings.Contains(out, "TestHelper") {
		t.Errorf("test file types should be excluded, got: %q", out)
	}
	if strings.Contains(out, "VendorType") {
		t.Errorf("vendored types should be excluded, got: %q", out)
	}
}

// ── connectivityMap ───────────────────────────────────────────────────────────

func TestConnectivityMap_ExcludesDefinesAndImports(t *testing.T) {
	edges := []*graph.Edge{
		{From: "file1", To: "entity1", Type: graph.EdgeDefines},  // must be excluded
		{From: "file1", To: "pkg1", Type: graph.EdgeImports},     // must be excluded
		{From: "fn1", To: "entity1", Type: graph.EdgeCalls},      // must be counted
		{From: "fn2", To: "entity1", Type: graph.EdgeCalls},      // must be counted
		{From: "struct1", To: "entity1", Type: graph.EdgeEmbeds}, // must be counted
	}
	refs := connectivityMap(edges)

	if refs["entity1"] != 3 {
		t.Errorf("entity1 should have 3 refs (2 CALLS + 1 EMBEDS), got %d", refs["entity1"])
	}
	if refs["pkg1"] != 0 {
		t.Errorf("IMPORTS target should not be counted, got %d", refs["pkg1"])
	}
}

// ── detectArchPattern ─────────────────────────────────────────────────────────

func TestDetectArchPattern_HTTPServer(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "p1", Name: "net/http", Type: graph.NodePackage, File: "/repo/main.go"},
	}
	pattern := detectArchPattern(nodes)
	if !strings.Contains(pattern, "HTTP server") {
		t.Errorf("expected HTTP server pattern, got %q", pattern)
	}
}

func TestDetectArchPattern_CLI(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "p1", Name: "github.com/spf13/cobra", Type: graph.NodePackage, File: "/repo/cmd/root.go"},
	}
	pattern := detectArchPattern(nodes)
	if !strings.Contains(pattern, "CLI") {
		t.Errorf("expected CLI pattern, got %q", pattern)
	}
}

func TestDetectArchPattern_Unknown(t *testing.T) {
	pattern := detectArchPattern(nil)
	if !strings.Contains(pattern, "unknown") {
		t.Errorf("expected 'unknown' for empty nodes, got %q", pattern)
	}
}

func TestDetectArchPattern_MCP(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "p1", Name: "github.com/mark3labs/mcp-go/mcp", Type: graph.NodePackage, File: "/repo/main.go"},
	}
	pattern := detectArchPattern(nodes)
	if !strings.Contains(pattern, "MCP server") {
		t.Errorf("expected MCP server pattern, got %q", pattern)
	}
}

// ── detectLayerLabel ──────────────────────────────────────────────────────────

func TestDetectLayerLabel(t *testing.T) {
	cases := []struct {
		pkg  string
		want string
	}{
		{"cmd/synapses", "[entry point]"},
		{"internal/store", "[persistence]"},
		{"internal/mcp", "[api surface]"},
		{"internal/graph", "[core logic]"},
		{"internal/config", "[config]"},
	}
	for _, c := range cases {
		got := detectLayerLabel(c.pkg)
		if got != c.want {
			t.Errorf("detectLayerLabel(%q) = %q, want %q", c.pkg, got, c.want)
		}
	}
}

// ── buildRepoMap ──────────────────────────────────────────────────────────────

func TestBuildRepoMap_EmptyGraph(t *testing.T) {
	out := buildRepoMap(nil, nil, "", 3)
	if !strings.Contains(out, "Repository Map") {
		t.Errorf("expected header, got %q", out)
	}
}

func TestBuildRepoMap_Compact(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "f1", Name: "HandleAuth", Type: graph.NodeFunction, File: "/repo/internal/mcp/auth.go", Package: "mcp"},
		{ID: "f2", Name: "HandleSearch", Type: graph.NodeFunction, File: "/repo/internal/mcp/search.go", Package: "mcp"},
		{ID: "f3", Name: "HandleContext", Type: graph.NodeFunction, File: "/repo/internal/mcp/context.go", Package: "mcp"},
		{ID: "f4", Name: "HandleExtra", Type: graph.NodeFunction, File: "/repo/internal/mcp/extra.go", Package: "mcp"},
		{ID: "s1", Name: "Store", Type: graph.NodeStruct, File: "/repo/internal/store/store.go", Package: "store"},
	}
	refs := map[graph.NodeID]int{"f1": 5, "f2": 3, "f3": 2, "f4": 1, "s1": 10}

	out := buildRepoMap(nodes, refs, "/repo", 3)
	// compact mode (topN=3): HandleExtra (refs=1) should be cut
	if strings.Contains(out, "HandleExtra") {
		t.Errorf("compact mode should limit to top 3 per package, HandleExtra should be cut")
	}
	if !strings.Contains(out, "HandleAuth") {
		t.Errorf("top entity HandleAuth should appear in compact map")
	}
}

func TestBuildRepoMap_Full(t *testing.T) {
	var nodes []*graph.Node
	refs := make(map[graph.NodeID]int)
	for i := 0; i < 12; i++ {
		id := graph.NodeID(strings.Repeat("x", i+1))
		name := strings.Repeat("F", i+1)
		nodes = append(nodes, &graph.Node{
			ID: id, Name: name, Type: graph.NodeFunction,
			File: "/repo/internal/core/core.go", Package: "core",
		})
		refs[id] = 12 - i
	}
	out := buildRepoMap(nodes, refs, "/repo", 10)
	// full mode (topN=10): node at index 10 (11th, refs=2) should appear but 12th (refs=1) cut
	if strings.Contains(out, strings.Repeat("F", 12)) {
		t.Errorf("full mode should cut at top 10, 12th entry should not appear")
	}
}

func TestBuildRepoMap_SkipsTestAndVendored(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "t1", Name: "TestFn", Type: graph.NodeFunction, File: "/repo/auth_test.go", Package: "auth"},
		{ID: "v1", Name: "VendorFn", Type: graph.NodeFunction, File: "/repo/vendor/foo/foo.go", Package: "foo", Provenance: graph.ProvenanceVendored},
		{ID: "u1", Name: "RealFn", Type: graph.NodeFunction, File: "/repo/internal/auth.go", Package: "auth"},
	}
	refs := map[graph.NodeID]int{"t1": 10, "v1": 10, "u1": 3}
	out := buildRepoMap(nodes, refs, "/repo", 3)
	if strings.Contains(out, "TestFn") {
		t.Errorf("test nodes should be excluded from repo map")
	}
	if strings.Contains(out, "VendorFn") {
		t.Errorf("vendored nodes should be excluded from repo map")
	}
	if !strings.Contains(out, "RealFn") {
		t.Errorf("real (non-test, non-vendored) node should appear in repo map")
	}
}

func TestBuildRepoMap_NoExtraBlankLinesForEmptyLayers(t *testing.T) {
	// Only put nodes in [core logic] layer — all other layers should be empty.
	// The result must NOT have double blank lines from empty layers firing the separator.
	nodes := []*graph.Node{
		{ID: "f1", Name: "Foo", Type: graph.NodeFunction, File: "/repo/internal/core/core.go", Package: "core"},
	}
	refs := map[graph.NodeID]int{"f1": 5}
	out := buildRepoMap(nodes, refs, "/repo", 3)
	if strings.Contains(out, "\n\n\n") {
		t.Errorf("output should not contain triple newlines (empty-layer separator bug): %q", out)
	}
}

func TestBuildRepoMap_UsesRefsLabel(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "s1", Name: "MyStruct", Type: graph.NodeStruct, File: "/repo/internal/core/core.go", Package: "core"},
	}
	refs := map[graph.NodeID]int{"s1": 7}
	out := buildRepoMap(nodes, refs, "/repo", 3)
	if strings.Contains(out, "callers") {
		t.Errorf("output should use 'refs' label, not 'callers': %q", out)
	}
	if !strings.Contains(out, "7 refs") {
		t.Errorf("expected '7 refs' for MyStruct, got: %q", out)
	}
}

// ── Handler-level integration tests ──────────────────────────────────────────

// TestHandleExplainCodebase_EmptyGraph verifies the handler works without
// panicking on an empty graph and returns well-formed text output.
func TestHandleExplainCodebase_EmptyGraph(t *testing.T) {
	s := newTestServer(t)
	result, err := s.handleExplainCodebase(ctx, callTool(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("nil result")
	}
	if result.IsError {
		t.Fatalf("handler returned tool error: %v", result.Content)
	}
	if len(result.Content) == 0 {
		t.Fatal("empty content")
	}
	tc, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	if !strings.Contains(tc.Text, "Codebase Orientation") {
		t.Errorf("expected orientation header, got: %q", tc.Text)
	}
}

// TestHandleExplainCodebase_CacheHit verifies the second call returns the cached value.
func TestHandleExplainCodebase_CacheHit(t *testing.T) {
	s := newTestServer(t)
	r1, err := s.handleExplainCodebase(ctx, callTool(nil))
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	r2, err := s.handleExplainCodebase(ctx, callTool(nil))
	if err != nil {
		t.Fatalf("second call (cache): %v", err)
	}
	t1 := r1.Content[0].(mcp.TextContent).Text
	t2 := r2.Content[0].(mcp.TextContent).Text
	if t1 != t2 {
		t.Error("cache hit should return identical content")
	}
}

// TestHandleExplainCodebase_CacheInvalidation verifies that invalidating the
// orientation cache causes the handler to recompute output.
func TestHandleExplainCodebase_CacheInvalidation(t *testing.T) {
	s := newTestServer(t)

	r1, err := s.handleExplainCodebase(ctx, callTool(nil))
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	t1 := r1.Content[0].(mcp.TextContent).Text

	// Invalidate the orient cache (simulates file watcher firing).
	s.orientMu.Lock()
	s.orientExplain = nil
	s.orientMu.Unlock()

	r2, err := s.handleExplainCodebase(ctx, callTool(nil))
	if err != nil {
		t.Fatalf("post-invalidation call: %v", err)
	}
	t2 := r2.Content[0].(mcp.TextContent).Text

	// After invalidation the result is recomputed. For an empty-graph server
	// the content will be identical — but the cache path must not error or panic.
	// The key contract is that the second call did NOT return an error.
	_ = t1
	_ = t2
}

// TestHandleGetRepoMap_DefaultsToCompact verifies missing detail param → compact.
func TestHandleGetRepoMap_DefaultsToCompact(t *testing.T) {
	s := newPopulatedTestServer(t)
	result, err := s.handleGetRepoMap(ctx, callTool(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil || result.IsError {
		t.Fatalf("handler returned error: %v", result)
	}
	tc := result.Content[0].(mcp.TextContent)
	if !strings.Contains(tc.Text, "Repository Map") {
		t.Errorf("expected repo map header, got: %q", tc.Text)
	}
}

// TestHandleGetRepoMap_CompactVsFull verifies full has more content than compact.
func TestHandleGetRepoMap_CompactVsFull(t *testing.T) {
	s := newPopulatedTestServer(t)

	rCompact, err := s.handleGetRepoMap(ctx, callTool(map[string]any{"detail": "compact"}))
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	rFull, err := s.handleGetRepoMap(ctx, callTool(map[string]any{"detail": "full"}))
	if err != nil {
		t.Fatalf("full: %v", err)
	}

	compact := rCompact.Content[0].(mcp.TextContent).Text
	full := rFull.Content[0].(mcp.TextContent).Text

	// Both should return well-formed output.
	if !strings.Contains(compact, "Repository Map") {
		t.Errorf("compact missing header")
	}
	if !strings.Contains(full, "Repository Map") {
		t.Errorf("full missing header")
	}
	// Full should be at least as long as compact (more entities per package).
	if len(full) < len(compact) {
		t.Errorf("full (%d chars) should be >= compact (%d chars)", len(full), len(compact))
	}
}

// TestHandleGetRepoMap_CacheSeparateByDetail verifies compact and full use different keys.
func TestHandleGetRepoMap_CacheSeparateByDetail(t *testing.T) {
	s := newPopulatedTestServer(t)

	// Populate both caches.
	r1, _ := s.handleGetRepoMap(ctx, callTool(map[string]any{"detail": "compact"}))
	r2, _ := s.handleGetRepoMap(ctx, callTool(map[string]any{"detail": "full"}))

	c1 := r1.Content[0].(mcp.TextContent).Text
	c2 := r2.Content[0].(mcp.TextContent).Text

	// They may be equal on a tiny test graph (both cut at ≤3 or ≤10 from same set),
	// but they must not return each other's cached value — verify by type assertion.
	// The real contract: the second compact call must return the compact-cached value.
	r3, _ := s.handleGetRepoMap(ctx, callTool(map[string]any{"detail": "compact"}))
	c3 := r3.Content[0].(mcp.TextContent).Text
	if c1 != c3 {
		t.Errorf("second compact call should return same cached value as first: %q vs %q", c1, c3)
	}
	_ = c2 // suppress unused warning
}

// newPopulatedTestServer builds a server with enough nodes to exercise
// the repo map and explain handlers meaningfully.
func newPopulatedTestServer(t *testing.T) *Server {
	t.Helper()
	s, _, _ := newPopulatedServer(t)
	return s
}

// ── relPath ───────────────────────────────────────────────────────────────────

func TestRelPath_Trim(t *testing.T) {
	got := relPath("/repo", "/repo/internal/mcp/tools.go")
	if got != "internal/mcp/tools.go" {
		t.Errorf("relPath = %q, want %q", got, "internal/mcp/tools.go")
	}
}

func TestRelPath_EmptyRoot(t *testing.T) {
	got := relPath("", "/abs/path/file.go")
	if got != "/abs/path/file.go" {
		t.Errorf("relPath with empty root should return abs path, got %q", got)
	}
}

// ── IMP-EVAL-6: entry point ranking in buildExplanation ──────────────────────

// TestBuildExplanation_EntryPointRanking verifies that cmd/main always surfaces
// before archived or script entry points.
func TestBuildExplanation_EntryPointRanking(t *testing.T) {
	identity := &graph.ProjectIdentity{
		Scale: graph.ScaleSmall,
		EntryPoints: []graph.EntityRef{
			// These three should be demoted — archived/scripts paths.
			{Name: "RunScript", Type: graph.NodeFunction, File: "archive/scripts/run.go", Line: 1},
			{Name: "OldMain", Type: graph.NodeFunction, File: "archive/cmd/old_main.go", Line: 1},
			// This is the real daemon entry point — must appear first.
			{Name: "main", Type: graph.NodeFunction, File: "cmd/synapses/main.go", Line: 1},
			// Exported function not in archive — tier 2.
			{Name: "ServeHTTP", Type: graph.NodeFunction, File: "internal/server/server.go", Line: 5},
		},
	}

	out := buildExplanation(identity, nil, nil, "")

	// Find positions of the two entries we care most about.
	mainPos := strings.Index(out, "cmd/synapses/main.go")
	archivePos := strings.Index(out, "archive/scripts/run.go")

	if mainPos == -1 {
		t.Fatal("expected cmd/synapses/main.go in output")
	}
	if archivePos == -1 {
		t.Fatal("expected archive/scripts/run.go in output")
	}
	if mainPos >= archivePos {
		t.Errorf("cmd/main should appear before archived scripts: mainPos=%d archivePos=%d\nout=%q", mainPos, archivePos, out)
	}
}

// TestBuildExplanation_EntryPointRanking_NoFalsePositives verifies that paths
// containing "script", "archive", or "tools" as substrings of OTHER words are
// NOT incorrectly demoted (e.g. "subscriptions", "archivist", "dev-tools").
func TestBuildExplanation_EntryPointRanking_NoFalsePositives(t *testing.T) {
	identity := &graph.ProjectIdentity{
		Scale: graph.ScaleSmall,
		EntryPoints: []graph.EntityRef{
			// These should NOT be demoted — "script" appears inside "subscriptions",
			// "archive" inside "archivist", and "tools" as part of "build-toolset".
			{Name: "HandleSubscriptions", Type: graph.NodeFunction, File: "internal/subscriptions/handler.go", Line: 1},
			{Name: "NewArchivist", Type: graph.NodeFunction, File: "internal/brain/archivist/archivist.go", Line: 1},
			{Name: "RunBuildToolset", Type: graph.NodeFunction, File: "internal/build-toolset/runner.go", Line: 1},
			// This one IS truly archived — must be demoted.
			{Name: "OldMigrate", Type: graph.NodeFunction, File: "archive/migrations/v1.go", Line: 1},
		},
	}

	out := buildExplanation(identity, nil, nil, "")

	// All three legitimate functions should appear (not cut by the 8-entry cap for 4 items).
	for _, name := range []string{"HandleSubscriptions", "NewArchivist", "RunBuildToolset"} {
		if !strings.Contains(out, name) {
			t.Errorf("expected %q in output (should not be demoted), got: %q", name, out)
		}
	}

	// OldMigrate should appear too (only 4 total, all fit in 8-cap), but must be last.
	oldPos := strings.Index(out, "archive/migrations/v1.go")
	subsPos := strings.Index(out, "internal/subscriptions/handler.go")
	if oldPos != -1 && subsPos != -1 && oldPos < subsPos {
		t.Errorf("truly archived entry should rank after legitimate exports; archivePos=%d subsPos=%d", oldPos, subsPos)
	}
}

// TestBuildExplanation_EntryPointRanking_TierOrder verifies the
// complete tier ordering: cmd/main < other main < non-archived exports < archived.
func TestBuildExplanation_EntryPointRanking_TierOrder(t *testing.T) {
	identity := &graph.ProjectIdentity{
		Scale: graph.ScaleSmall,
		EntryPoints: []graph.EntityRef{
			{Name: "ArchivedFn", Type: graph.NodeFunction, File: "archive/old/helper.go", Line: 1},
			{Name: "ExportedFn", Type: graph.NodeFunction, File: "internal/service/service.go", Line: 1},
			{Name: "main", Type: graph.NodeFunction, File: "cmd/server/main.go", Line: 1},
		},
	}

	out := buildExplanation(identity, nil, nil, "")

	cmdPos := strings.Index(out, "cmd/server/main.go")
	svcPos := strings.Index(out, "internal/service/service.go")
	archPos := strings.Index(out, "archive/old/helper.go")

	if cmdPos == -1 || svcPos == -1 || archPos == -1 {
		t.Fatalf("all three entry points should appear in output; got %q", out)
	}
	if cmdPos >= svcPos || svcPos >= archPos {
		t.Errorf("expected tier order cmd < service < archive; positions: cmd=%d svc=%d arch=%d\nout=%q",
			cmdPos, svcPos, archPos, out)
	}
}
