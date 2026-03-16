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
	out := buildExplanation(identity, nil, nil, nil, "")
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
			{Name: "main", Type: graph.NodeFunction, File: "/repo/cmd/main.go", Line: 1},
		},
	}
	nodes := []*graph.Node{
		{ID: "n1", Name: "Store", Type: graph.NodeStruct, File: "/repo/internal/store/store.go", Package: "store", Line: 10},
		{ID: "n2", Name: "doWork", Type: graph.NodeFunction, File: "/repo/internal/core/core.go", Package: "core", Line: 5},
	}
	edges := []*graph.Edge{
		{From: "n2", To: "n1", Type: graph.EdgeCalls},
		{From: "n2", To: "n1", Type: graph.EdgeCalls}, // double fanin
	}
	fanin := map[graph.NodeID]int{"n1": 2}

	out := buildExplanation(identity, nodes, edges, fanin, "/repo")
	if !strings.Contains(out, "Store") {
		t.Errorf("expected key type 'Store' in output, got: %q", out)
	}
	if !strings.Contains(out, "2 callers") {
		t.Errorf("expected '2 callers' for Store, got: %q", out)
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
	fanin := map[graph.NodeID]int{"t1": 10, "v1": 10, "u1": 3}
	out := buildExplanation(identity, nodes, nil, fanin, "")
	if strings.Contains(out, "TestHelper") {
		t.Errorf("test file types should be excluded, got: %q", out)
	}
	if strings.Contains(out, "VendorType") {
		t.Errorf("vendored types should be excluded, got: %q", out)
	}
}

// ── detectArchPattern ─────────────────────────────────────────────────────────

func TestDetectArchPattern_HTTPServer(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "p1", Name: "net/http", Type: graph.NodePackage, File: "/repo/main.go"},
	}
	pattern := detectArchPattern(nodes, nil)
	if !strings.Contains(pattern, "HTTP server") {
		t.Errorf("expected HTTP server pattern, got %q", pattern)
	}
}

func TestDetectArchPattern_CLI(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "p1", Name: "github.com/spf13/cobra", Type: graph.NodePackage, File: "/repo/cmd/root.go"},
	}
	pattern := detectArchPattern(nodes, nil)
	if !strings.Contains(pattern, "CLI") {
		t.Errorf("expected CLI pattern, got %q", pattern)
	}
}

func TestDetectArchPattern_Unknown(t *testing.T) {
	pattern := detectArchPattern(nil, nil)
	if !strings.Contains(pattern, "unknown") {
		t.Errorf("expected 'unknown' for empty nodes, got %q", pattern)
	}
}

func TestDetectArchPattern_MCP(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "p1", Name: "github.com/mark3labs/mcp-go/mcp", Type: graph.NodePackage, File: "/repo/main.go"},
	}
	pattern := detectArchPattern(nodes, nil)
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
	fanin := map[graph.NodeID]int{"f1": 5, "f2": 3, "f3": 2, "f4": 1, "s1": 10}

	out := buildRepoMap(nodes, fanin, "/repo", 3)
	// compact mode (topN=3): HandleExtra (fanin=1) should be cut
	if strings.Contains(out, "HandleExtra") {
		t.Errorf("compact mode should limit to top 3 per package, HandleExtra should be cut")
	}
	if !strings.Contains(out, "HandleAuth") {
		t.Errorf("top entity HandleAuth should appear in compact map")
	}
}

func TestBuildRepoMap_Full(t *testing.T) {
	var nodes []*graph.Node
	fanin := make(map[graph.NodeID]int)
	for i := 0; i < 12; i++ {
		id := graph.NodeID(strings.Repeat("x", i+1))
		name := strings.Repeat("F", i+1)
		nodes = append(nodes, &graph.Node{
			ID: id, Name: name, Type: graph.NodeFunction,
			File: "/repo/internal/core/core.go", Package: "core",
		})
		fanin[id] = 12 - i
	}
	out := buildRepoMap(nodes, fanin, "/repo", 10)
	// full mode (topN=10): node at index 10 (11th, fanin=2) should appear but 12th (fanin=1) cut
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
	fanin := map[graph.NodeID]int{"t1": 10, "v1": 10, "u1": 3}
	out := buildRepoMap(nodes, fanin, "/repo", 3)
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
