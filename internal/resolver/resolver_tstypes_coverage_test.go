package resolver_test

// Tests that exercise the scanner/edge-matching inner loop of
// ResolveTSTypesCallEdges by injecting a fake "node" executable via PATH.
// Skipped on Windows (shell scripts not available).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/resolver"
)

// writeFakeNodeScript writes a shell script named "node" to dir that cats the
// given NDJSON file. Returns the dir so callers can prepend it to PATH.
func writeFakeNodeScript(t *testing.T, ndjsonPath string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake node script not supported on Windows")
	}
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\ncat %q\n", ndjsonPath)
	fakeNode := filepath.Join(dir, "node")
	if err := os.WriteFile(fakeNode, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// injectFakeNode prepends fakeNodeDir to PATH for the duration of the test.
func injectFakeNode(t *testing.T, fakeNodeDir string) {
	t.Helper()
	origPath := os.Getenv("PATH")
	t.Cleanup(func() { _ = os.Setenv("PATH", origPath) })
	if err := os.Setenv("PATH", fakeNodeDir+":"+origPath); err != nil {
		t.Fatal(err)
	}
}

// tsEdgeJSON marshals a fake tsEdge JSON line.
func tsEdgeJSON(fromFile string, fromLine int, toFile string, toLine int) string {
	b, _ := json.Marshal(map[string]interface{}{
		"from_file": fromFile,
		"from_line": fromLine,
		"to_file":   toFile,
		"to_line":   toLine,
	})
	return string(b)
}

// TestResolveTSTypesCallEdges_ScannerMatchEdge verifies that the scanner inner
// loop matches from_file/from_line → to_file/to_line to real graph nodes and
// adds a CALLS edge.
func TestResolveTSTypesCallEdges_ScannerMatchEdge(t *testing.T) {
	root := t.TempDir()
	fileA := filepath.Join(root, "src", "a.ts")
	fileB := filepath.Join(root, "src", "b.ts")

	// Write the NDJSON output the fake node will emit.
	line := tsEdgeJSON(fileA, 10, fileB, 20)
	ndjsonPath := filepath.Join(t.TempDir(), "out.ndjson")
	if err := os.WriteFile(ndjsonPath, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeDir := writeFakeNodeScript(t, ndjsonPath)
	injectFakeNode(t, fakeDir)

	// Build graph with matching nodes (file + line).
	g := graph.New(root)
	idA := g.MakeNodeID(fileA, "funcA")
	g.AddNode(&graph.Node{ID: idA, Name: "funcA", Type: graph.NodeFunction, File: fileA, Line: 10})
	idB := g.MakeNodeID(fileB, "funcB")
	g.AddNode(&graph.Node{ID: idB, Name: "funcB", Type: graph.NodeFunction, File: fileB, Line: 20})

	n, err := resolver.ResolveTSTypesCallEdges(g, root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 edge added, got %d", n)
	}
}

// TestResolveTSTypesCallEdges_MalformedLineSkipped verifies that invalid JSON
// lines are silently skipped and processing continues.
func TestResolveTSTypesCallEdges_MalformedLineSkipped(t *testing.T) {
	root := t.TempDir()
	fileA := filepath.Join(root, "a.ts")
	fileB := filepath.Join(root, "b.ts")

	validLine := tsEdgeJSON(fileA, 1, fileB, 1)
	ndjsonPath := filepath.Join(t.TempDir(), "out.ndjson")
	content := "not valid JSON\n" + validLine + "\n"
	if err := os.WriteFile(ndjsonPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeDir := writeFakeNodeScript(t, ndjsonPath)
	injectFakeNode(t, fakeDir)

	g := graph.New(root)
	idA := g.MakeNodeID(fileA, "funcA")
	g.AddNode(&graph.Node{ID: idA, Name: "funcA", Type: graph.NodeFunction, File: fileA, Line: 1})
	idB := g.MakeNodeID(fileB, "funcB")
	g.AddNode(&graph.Node{ID: idB, Name: "funcB", Type: graph.NodeFunction, File: fileB, Line: 1})

	n, err := resolver.ResolveTSTypesCallEdges(g, root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 edge (malformed line skipped), got %d", n)
	}
}

// TestResolveTSTypesCallEdges_UnknownNodeSkipped verifies that edges where
// file/line don't match any graph node are silently skipped.
func TestResolveTSTypesCallEdges_UnknownNodeSkipped(t *testing.T) {
	root := t.TempDir()

	// Emit an edge for files that are NOT in the graph.
	line := tsEdgeJSON("/nonexistent/a.ts", 1, "/nonexistent/b.ts", 1)
	ndjsonPath := filepath.Join(t.TempDir(), "out.ndjson")
	if err := os.WriteFile(ndjsonPath, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeDir := writeFakeNodeScript(t, ndjsonPath)
	injectFakeNode(t, fakeDir)

	g := graph.New(root) // empty graph → no matching nodes

	n, err := resolver.ResolveTSTypesCallEdges(g, root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 edges (unknown nodes), got %d", n)
	}
}

// TestResolveTSTypesCallEdges_DuplicateEdgeDeduped verifies that emitting the
// same edge twice results in only one CALLS edge added.
func TestResolveTSTypesCallEdges_DuplicateEdgeDeduped(t *testing.T) {
	root := t.TempDir()
	fileA := filepath.Join(root, "a.ts")
	fileB := filepath.Join(root, "b.ts")

	line := tsEdgeJSON(fileA, 5, fileB, 5)
	// Emit the same edge twice.
	ndjsonPath := filepath.Join(t.TempDir(), "out.ndjson")
	if err := os.WriteFile(ndjsonPath, []byte(line+"\n"+line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeDir := writeFakeNodeScript(t, ndjsonPath)
	injectFakeNode(t, fakeDir)

	g := graph.New(root)
	idA := g.MakeNodeID(fileA, "funcA")
	g.AddNode(&graph.Node{ID: idA, Name: "funcA", Type: graph.NodeFunction, File: fileA, Line: 5})
	idB := g.MakeNodeID(fileB, "funcB")
	g.AddNode(&graph.Node{ID: idB, Name: "funcB", Type: graph.NodeFunction, File: fileB, Line: 5})

	n, err := resolver.ResolveTSTypesCallEdges(g, root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 edge (dedup), got %d", n)
	}
}

// TestResolveTSTypesCallEdges_ExistingEdgeNotDuplicated verifies that edges
// already in the graph are not re-added.
func TestResolveTSTypesCallEdges_ExistingEdgeNotDuplicated(t *testing.T) {
	root := t.TempDir()
	fileA := filepath.Join(root, "a.ts")
	fileB := filepath.Join(root, "b.ts")

	line := tsEdgeJSON(fileA, 3, fileB, 3)
	ndjsonPath := filepath.Join(t.TempDir(), "out.ndjson")
	if err := os.WriteFile(ndjsonPath, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeDir := writeFakeNodeScript(t, ndjsonPath)
	injectFakeNode(t, fakeDir)

	g := graph.New(root)
	idA := g.MakeNodeID(fileA, "funcA")
	g.AddNode(&graph.Node{ID: idA, Name: "funcA", Type: graph.NodeFunction, File: fileA, Line: 3})
	idB := g.MakeNodeID(fileB, "funcB")
	g.AddNode(&graph.Node{ID: idB, Name: "funcB", Type: graph.NodeFunction, File: fileB, Line: 3})
	// Pre-add the edge so it already exists in the graph.
	g.AddEdge(&graph.Edge{From: idA, To: idB, Type: graph.EdgeCalls})

	n, err := resolver.ResolveTSTypesCallEdges(g, root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 new edges (edge already in graph), got %d", n)
	}
}

// TestResolveTSTypesCallEdges_NodeExitNonZero verifies that a non-zero exit
// from the node subprocess is handled without crashing (logs to stderr).
func TestResolveTSTypesCallEdges_NodeExitNonZero(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake node script not supported on Windows")
	}
	root := t.TempDir()
	fileA := filepath.Join(root, "a.ts")
	fileB := filepath.Join(root, "b.ts")

	// Valid output then exit 1.
	line := tsEdgeJSON(fileA, 7, fileB, 7)
	ndjsonPath := filepath.Join(t.TempDir(), "out.ndjson")
	if err := os.WriteFile(ndjsonPath, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\ncat %q\nexit 1\n", ndjsonPath)
	fakeNode := filepath.Join(dir, "node")
	if err := os.WriteFile(fakeNode, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	injectFakeNode(t, dir)

	g := graph.New(root)
	idA := g.MakeNodeID(fileA, "funcA")
	g.AddNode(&graph.Node{ID: idA, Name: "funcA", Type: graph.NodeFunction, File: fileA, Line: 7})
	idB := g.MakeNodeID(fileB, "funcB")
	g.AddNode(&graph.Node{ID: idB, Name: "funcB", Type: graph.NodeFunction, File: fileB, Line: 7})

	// The function should not return an error even though node exits 1.
	// (it just logs to stderr per the fail-silent contract)
	n, err := resolver.ResolveTSTypesCallEdges(g, root)
	if err != nil {
		t.Fatalf("expected no error even with node exit 1, got: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 edge added even with non-zero exit, got %d", n)
	}
}
