package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// templateGraphDB and templateKnowledgeDB hold paths to pre-initialized
// SQLite databases created once in TestMain. openMCPTestStore copies these
// files instead of re-running 50+ DDL migrations per test.
var templateGraphDB string
var templateKnowledgeDB string

// TestMain redirects the synapses cache for all mcp package tests and
// creates a template store for fast per-test setup.
func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "synapses-mcp-test-cache-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create test cache dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmp)
	os.Setenv("SYNAPSES_CACHE_DIR", tmp)

	// Create template store once — runs all DDL, migrations, PRAGMA setup.
	templateDir, err := os.MkdirTemp("", "synapses-mcp-test-template-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create template dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(templateDir)

	templatePath := filepath.Join(templateDir, "template.db")
	st, err := store.Open(templatePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "store.Open template: %v\n", err)
		os.Exit(1)
	}
	st.Close()

	templateGraphDB = templatePath
	templateKnowledgeDB = store.KnowledgePath(templatePath)

	os.Exit(m.Run())
}

// ── Server constructors ───────────────────────────────────────────────────────

// newTestServer creates a real Server with a real temp SQLite store and an
// empty graph. Sidecars (brain, scout, pulse) are all nil. Use for tests
// that don't need pre-populated code nodes.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })
	return srv
}

// newPopulatedServer creates a Server with a small graph containing two
// functions (AuthLogin, AuthLogout) in pkg/auth.go, plus callers. Use for
// tests that exercise get_context, get_impact, get_call_chain, etc.
//
// Returns the server plus the NodeIDs of the two main functions.
func newPopulatedServer(t *testing.T) (*Server, graph.NodeID, graph.NodeID) {
	t.Helper()
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	addFunc := func(file, name, pkg string, exported bool) graph.NodeID {
		id := g.MakeNodeID(file, name)
		g.AddNode(&graph.Node{
			ID:       id,
			Type:     graph.NodeFunction,
			Name:     name,
			File:     file,
			Line:     1,
			Package:  pkg,
			Exported: exported,
		})
		return id
	}

	loginID := addFunc("pkg/auth/auth.go", "AuthLogin", "auth", true)
	logoutID := addFunc("pkg/auth/auth.go", "AuthLogout", "auth", true)
	callerID := addFunc("pkg/api/handler.go", "HandleRequest", "api", true)

	g.AddEdge(&graph.Edge{From: callerID, To: loginID, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: callerID, To: logoutID, Type: graph.EdgeCalls})

	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })
	return srv, loginID, logoutID
}

// ── CallToolRequest builder ───────────────────────────────────────────────────

// callTool builds a CallToolRequest from a plain map. Keys match the JSON
// parameter names each handler reads via req.GetArguments().
func callTool(args map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: args},
	}
}

// ── Result helpers ────────────────────────────────────────────────────────────

// mustResult calls handler(ctx, req), fatals if err != nil or result is
// an error, and returns the parsed JSON map.
func mustResult(t *testing.T, result *mcp.CallToolResult, err error) map[string]any {
	t.Helper()
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result == nil {
		t.Fatal("handler returned nil result")
	}
	if result.IsError {
		t.Fatalf("handler returned tool error: %v", result.Content)
	}
	if len(result.Content) == 0 {
		t.Fatal("handler returned empty content")
	}
	tc, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(tc.Text), &m); err != nil {
		t.Fatalf("unmarshal result: %v\nraw: %s", err, tc.Text)
	}
	return m
}

// mustErrorResult asserts the result is a tool-level error (not a Go error)
// and returns the raw text.
func mustErrorResult(t *testing.T, result *mcp.CallToolResult, err error) string {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if result == nil {
		t.Fatal("nil result")
	}
	if !result.IsError {
		t.Fatal("expected error result but got success")
	}
	if len(result.Content) == 0 {
		return ""
	}
	if tc, ok := result.Content[0].(mcp.TextContent); ok {
		return tc.Text
	}
	return ""
}

// hasKey asserts a top-level key exists in the result map.
func hasKey(t *testing.T, m map[string]any, key string) {
	t.Helper()
	if _, ok := m[key]; !ok {
		t.Errorf("expected key %q in result, keys present: %v", key, mapKeys(m))
	}
}

// noKey asserts a top-level key is absent.
func noKey(t *testing.T, m map[string]any, key string) {
	t.Helper()
	if _, ok := m[key]; ok {
		t.Errorf("key %q should be absent but is present", key)
	}
}

// mapKeys returns the keys of a map for error messages.
func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// ctx is a convenience alias used across test files.
var ctx = context.Background()

// tempStorePath returns a unique temp DB path per test.
func tempStorePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "test.db")
}

// openTestStore opens a real SQLite store at a temp path.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(tempStorePath(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
