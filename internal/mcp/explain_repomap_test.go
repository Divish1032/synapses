package mcp

// White-box tests for explain.go and repomap.go.
// Uses package mcp for direct access to unexported builder functions.

import (
	"strings"
	"testing"

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
	out := buildExplanation(identity, nodes, nil, fanin, "/repo")
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
