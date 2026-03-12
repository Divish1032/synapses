package dataflow_test

// Additional tests targeting uncovered branches in dataflow.go:
// - Non-function/method node type skip
// - Nil metadata source node
// - maxDataFlowsPerSource limit
// - Custom source/sink with empty label → default label
// - getenv/lookupenv env_input source
// - SQL exec/query/prepare switch sinks
// - exec_sink via fileInPackage (/exec/ path)
// - file_write_sink (*os.File write)
// - write_sink (io.Writer write)
// - matchesPattern: NodeType mismatch, FilePattern glob, SigPattern
// - BFS visited node skip (diamond graph)

import (
	"fmt"
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/dataflow"
	"github.com/SynapsesOS/synapses/internal/graph"
)

// TestAnnotateGraph_NonFunctionNodeSkipped verifies that non-function/method
// nodes (e.g. NodePackage) are skipped during role detection.
func TestAnnotateGraph_NonFunctionNodeSkipped(t *testing.T) {
	g := graph.New("/repo")

	// Add a package node — should be skipped by the type guard.
	pkgID := g.MakeNodeID("/repo/pkg/auth.go", "auth")
	g.AddNode(&graph.Node{
		ID:   pkgID,
		Name: "auth",
		Type: graph.NodePackage,
		File: "/repo/pkg/auth.go",
	})
	// Also add a source+sink so AnnotateGraph has work to do if roles are found.
	srcID := addFn(g, "/repo/h.go", "HandleReq", "web", "func HandleReq(r *http.Request)")
	sinkID := addFn(g, "/repo/db.go", "execSQL", "db", "func execSQL(db *sql.DB)")
	g.GetNode(sinkID).Metadata["signature"] = "func execSQL(db *sql.DB)"
	g.AddEdge(&graph.Edge{From: srcID, To: sinkID, Type: graph.EdgeCalls})

	n := dataflow.AnnotateGraph(g, emptyCfg())
	_ = n // must not panic
}

// TestAnnotateGraph_NilMetadataSource verifies that a node with nil Metadata
// that is detected as a source gets its Metadata map initialised properly.
func TestAnnotateGraph_NilMetadataSource(t *testing.T) {
	g := graph.New("/repo")

	// ParseFoo matches the parse* heuristic but has nil Metadata.
	srcID := g.MakeNodeID("/repo/api/api.go", "ParseFoo")
	g.AddNode(&graph.Node{
		ID:       srcID,
		Name:     "ParseFoo",
		Type:     graph.NodeFunction,
		File:     "/repo/api/api.go",
		Package:  "api",
		Metadata: nil, // intentionally nil
	})
	sinkID := addFn(g, "/repo/db.go", "ExecSQL", "db", "func ExecSQL(db *sql.DB)")
	g.GetNode(sinkID).Metadata["signature"] = "func ExecSQL(db *sql.DB)"
	g.AddEdge(&graph.Edge{From: srcID, To: sinkID, Type: graph.EdgeCalls})

	// Must not panic even though Metadata is nil.
	n := dataflow.AnnotateGraph(g, emptyCfg())
	_ = n
	node := g.GetNode(srcID)
	if node.Metadata == nil {
		t.Error("expected Metadata to be initialised after role detection")
	}
}

// TestAnnotateGraph_MaxDataFlowsPerSourceCap verifies that when a source can
// reach more than 20 sinks, only 20 DATA_FLOWS edges are created.
func TestAnnotateGraph_MaxDataFlowsPerSourceCap(t *testing.T) {
	g := graph.New("/repo")

	srcID := addFn(g, "/repo/h.go", "Handle", "web", "func Handle(r *http.Request)")

	// Create 25 reachable sinks.
	for i := 0; i < 25; i++ {
		name := fmt.Sprintf("execSink%02d", i)
		sinkID := addFn(g, "/repo/db.go", name, "db", "func "+name+"(db *sql.DB)")
		g.GetNode(sinkID).Metadata["signature"] = "func " + name + "(db *sql.DB)"
		g.AddEdge(&graph.Edge{From: srcID, To: sinkID, Type: graph.EdgeCalls})
	}

	n := dataflow.AnnotateGraph(g, emptyCfg())
	// Cap is 20 per source.
	if n > 20 {
		t.Errorf("expected ≤20 DATA_FLOWS edges (cap), got %d", n)
	}
}

// TestAnnotateGraph_CustomSourceEmptyLabel checks that a custom source pattern
// with no Label defaults to "custom_source".
func TestAnnotateGraph_CustomSourceEmptyLabel(t *testing.T) {
	g := graph.New("/repo")

	srcID := addFn(g, "/repo/q.go", "readQueue", "queue", "func readQueue() []byte")
	sinkID := addFn(g, "/repo/db.go", "insertRow", "db", "func insertRow(db *sql.DB, v string)")
	g.GetNode(sinkID).Metadata["signature"] = "func insertRow(db *sql.DB, v string)"
	g.AddEdge(&graph.Edge{From: srcID, To: sinkID, Type: graph.EdgeCalls})

	cfg := &config.Config{
		DataFlowSources: []config.DataFlowPattern{
			{NamePattern: "readQueue", Label: ""}, // empty label → "custom_source"
		},
	}
	dataflow.AnnotateGraph(g, cfg)

	src := g.GetNode(srcID)
	if src.Metadata["data_label"] != "custom_source" {
		t.Errorf("expected label %q, got %q", "custom_source", src.Metadata["data_label"])
	}
}

// TestAnnotateGraph_CustomSinkEmptyLabel checks that a custom sink pattern
// with no Label defaults to "custom_sink".
func TestAnnotateGraph_CustomSinkEmptyLabel(t *testing.T) {
	g := graph.New("/repo")

	srcID := addFn(g, "/repo/h.go", "ParseBody", "web", "func ParseBody(b []byte)")
	sinkID := addFn(g, "/repo/log.go", "sendToSplunk", "log", "func sendToSplunk(msg string)")
	g.AddEdge(&graph.Edge{From: srcID, To: sinkID, Type: graph.EdgeCalls})

	cfg := &config.Config{
		DataFlowSinks: []config.DataFlowPattern{
			{NamePattern: "sendToSplunk", Label: ""}, // empty label → "custom_sink"
		},
	}
	dataflow.AnnotateGraph(g, cfg)

	sink := g.GetNode(sinkID)
	if sink.Metadata["data_label"] != "custom_sink" {
		t.Errorf("expected label %q, got %q", "custom_sink", sink.Metadata["data_label"])
	}
}

// TestAnnotateGraph_EnvInputSource verifies that functions named "getenv" or
// "lookupenv" are detected as env_input sources.
func TestAnnotateGraph_EnvInputSource(t *testing.T) {
	g := graph.New("/repo")

	// "getenv" — exact name match.
	getenvID := addFn(g, "/repo/cfg.go", "GetEnv", "cfg", "func GetEnv(key string) string")
	sinkID := addFn(g, "/repo/db.go", "execQuery", "db", "func execQuery(db *sql.DB, q string)")
	g.GetNode(sinkID).Metadata["signature"] = "func execQuery(db *sql.DB, q string)"
	g.AddEdge(&graph.Edge{From: getenvID, To: sinkID, Type: graph.EdgeCalls})

	dataflow.AnnotateGraph(g, emptyCfg())

	src := g.GetNode(getenvID)
	if src.Metadata["data_role"] != "source" {
		t.Errorf("GetEnv data_role = %q, want source", src.Metadata["data_role"])
	}
	if src.Metadata["data_label"] != "env_input" {
		t.Errorf("GetEnv data_label = %q, want env_input", src.Metadata["data_label"])
	}
}

// TestAnnotateGraph_SQLSwitchSink verifies that function names like "exec",
// "query", "queryrow", "prepare" are detected as sql_sink.
func TestAnnotateGraph_SQLSwitchSink(t *testing.T) {
	for _, name := range []string{"Exec", "Query", "QueryRow", "QueryContext", "ExecContext", "Prepare", "PrepareContext"} {
		t.Run(name, func(t *testing.T) {
			g := graph.New("/repo")
			srcID := addFn(g, "/repo/h.go", "HandleReq", "web", "func HandleReq(r *http.Request)")
			sinkID := addFn(g, "/repo/db.go", name, "db", "func "+name+"(q string)")
			g.AddEdge(&graph.Edge{From: srcID, To: sinkID, Type: graph.EdgeCalls})

			dataflow.AnnotateGraph(g, emptyCfg())

			sink := g.GetNode(sinkID)
			if sink.Metadata["data_role"] != "sink" {
				t.Errorf("%s: data_role = %q, want sink", name, sink.Metadata["data_role"])
			}
			if sink.Metadata["data_label"] != "sql_sink" {
				t.Errorf("%s: data_label = %q, want sql_sink", name, sink.Metadata["data_label"])
			}
		})
	}
}

// TestAnnotateGraph_ExecSink verifies that a function named "Command" or "Run"
// in a file with "/exec/" in its path is detected as exec_sink.
func TestAnnotateGraph_ExecSink(t *testing.T) {
	g := graph.New("/repo")

	srcID := addFn(g, "/repo/h.go", "HandleReq", "web", "func HandleReq(r *http.Request)")
	// File path contains "/exec/" so fileInPackage("exec") returns true.
	sinkID := addFn(g, "/repo/vendor/os/exec/exec.go", "Command", "exec", "func Command(name string) *Cmd")
	g.AddEdge(&graph.Edge{From: srcID, To: sinkID, Type: graph.EdgeCalls})

	dataflow.AnnotateGraph(g, emptyCfg())

	sink := g.GetNode(sinkID)
	if sink.Metadata["data_role"] != "sink" {
		t.Errorf("Command data_role = %q, want sink", sink.Metadata["data_role"])
	}
	if sink.Metadata["data_label"] != "exec_sink" {
		t.Errorf("Command data_label = %q, want exec_sink", sink.Metadata["data_label"])
	}
}

// TestAnnotateGraph_ExecSinkByPackage verifies fileInPackage via Package field match.
func TestAnnotateGraph_ExecSinkByPackage(t *testing.T) {
	g := graph.New("/repo")

	srcID := addFn(g, "/repo/h.go", "HandleReq", "web", "func HandleReq(r *http.Request)")
	// Package == "exec" triggers fileInPackage.
	sinkID := g.MakeNodeID("/repo/runner.go", "Run")
	g.AddNode(&graph.Node{
		ID:      sinkID,
		Name:    "Run",
		Type:    graph.NodeFunction,
		File:    "/repo/runner.go",
		Package: "exec", // fileInPackage will match via n.Package == pkg
		Metadata: map[string]string{"signature": "func Run(cmd string)"},
	})
	g.AddEdge(&graph.Edge{From: srcID, To: sinkID, Type: graph.EdgeCalls})

	dataflow.AnnotateGraph(g, emptyCfg())

	sink := g.GetNode(sinkID)
	if sink.Metadata["data_role"] != "sink" {
		t.Errorf("Run data_role = %q, want sink", sink.Metadata["data_role"])
	}
}

// TestAnnotateGraph_FileWriteSink verifies that functions with *os.File in
// signature and a "write" prefix name are detected as file_write_sink.
func TestAnnotateGraph_FileWriteSink(t *testing.T) {
	g := graph.New("/repo")

	srcID := addFn(g, "/repo/h.go", "ParseBody", "web", "func ParseBody(b []byte)")
	sinkID := addFn(g, "/repo/fs.go", "WriteFile", "fs",
		"func WriteFile(f *os.File, data []byte) error")
	g.AddEdge(&graph.Edge{From: srcID, To: sinkID, Type: graph.EdgeCalls})

	cfg := &config.Config{
		DataFlowSources: []config.DataFlowPattern{
			{NamePattern: "ParseBody"},
		},
	}
	dataflow.AnnotateGraph(g, cfg)

	sink := g.GetNode(sinkID)
	if sink.Metadata["data_role"] != "sink" {
		t.Errorf("WriteFile data_role = %q, want sink", sink.Metadata["data_role"])
	}
	if sink.Metadata["data_label"] != "file_write_sink" {
		t.Errorf("WriteFile data_label = %q, want file_write_sink", sink.Metadata["data_label"])
	}
}

// TestAnnotateGraph_WriterSink verifies that functions with io.Writer in
// signature and a "write" prefix are detected as write_sink.
func TestAnnotateGraph_WriterSink(t *testing.T) {
	g := graph.New("/repo")

	srcID := addFn(g, "/repo/h.go", "ParseBody", "web", "func ParseBody(b []byte)")
	sinkID := addFn(g, "/repo/resp.go", "WriteResponse", "resp",
		"func WriteResponse(w io.Writer, data []byte)")
	g.AddEdge(&graph.Edge{From: srcID, To: sinkID, Type: graph.EdgeCalls})

	cfg := &config.Config{
		DataFlowSources: []config.DataFlowPattern{
			{NamePattern: "ParseBody"},
		},
	}
	dataflow.AnnotateGraph(g, cfg)

	sink := g.GetNode(sinkID)
	if sink.Metadata["data_role"] != "sink" {
		t.Errorf("WriteResponse data_role = %q, want sink", sink.Metadata["data_role"])
	}
	if sink.Metadata["data_label"] != "write_sink" {
		t.Errorf("WriteResponse data_label = %q, want write_sink", sink.Metadata["data_label"])
	}
}

// TestAnnotateGraph_PatternNodeTypeMismatch verifies that matchesPattern
// returns false when the pattern's NodeType doesn't match the node's type.
func TestAnnotateGraph_PatternNodeTypeMismatch(t *testing.T) {
	g := graph.New("/repo")

	// NodeFunction won't match a pattern looking for NodeMethod.
	srcID := addFn(g, "/repo/h.go", "readData", "api", "func readData() []byte")
	sinkID := addFn(g, "/repo/db.go", "insertRow", "db", "func insertRow(db *sql.DB, v string)")
	g.GetNode(sinkID).Metadata["signature"] = "func insertRow(db *sql.DB, v string)"
	g.AddEdge(&graph.Edge{From: srcID, To: sinkID, Type: graph.EdgeCalls})

	cfg := &config.Config{
		DataFlowSources: []config.DataFlowPattern{
			{
				NodeType:    graph.NodeMethod, // node is NodeFunction → mismatch → false
				NamePattern: "readData",
			},
		},
	}
	dataflow.AnnotateGraph(g, cfg)

	src := g.GetNode(srcID)
	// Should NOT be marked as source (pattern rejected due to type mismatch).
	if src.Metadata["data_role"] == "source" {
		t.Error("expected node NOT to be matched by NodeType-mismatched pattern")
	}
}

// TestAnnotateGraph_PatternFileGlob verifies the FilePattern glob matching
// branch inside matchesPattern (filepath.Match path).
func TestAnnotateGraph_PatternFileGlob(t *testing.T) {
	g := graph.New("/repo")

	// FilePattern as a glob that matches the base name.
	srcID := addFn(g, "/repo/api/handler.go", "readData", "api", "func readData() []byte")
	sinkID := addFn(g, "/repo/db/db.go", "insertRow", "db", "func insertRow(db *sql.DB, v string)")
	g.GetNode(sinkID).Metadata["signature"] = "func insertRow(db *sql.DB, v string)"
	g.AddEdge(&graph.Edge{From: srcID, To: sinkID, Type: graph.EdgeCalls})

	cfg := &config.Config{
		DataFlowSources: []config.DataFlowPattern{
			{
				NamePattern: "readData",
				FilePattern: "handler*.go", // glob matches "handler.go"
			},
		},
	}
	dataflow.AnnotateGraph(g, cfg)

	src := g.GetNode(srcID)
	if src.Metadata["data_role"] != "source" {
		t.Error("expected file-glob pattern to match handler.go")
	}
}

// TestAnnotateGraph_PatternSigMatch verifies SigPattern matching and mismatch
// branches inside matchesPattern.
func TestAnnotateGraph_PatternSigMatch(t *testing.T) {
	g := graph.New("/repo")

	// Source with a specific signature — pattern must match it.
	srcID := addFn(g, "/repo/h.go", "readData", "api", "func readData(ctx context.Context) []byte")
	sinkID := addFn(g, "/repo/db.go", "insertRow", "db", "func insertRow(db *sql.DB, v string)")
	g.GetNode(sinkID).Metadata["signature"] = "func insertRow(db *sql.DB, v string)"
	g.AddEdge(&graph.Edge{From: srcID, To: sinkID, Type: graph.EdgeCalls})

	// Pattern with SigPattern that MATCHES.
	cfgMatch := &config.Config{
		DataFlowSources: []config.DataFlowPattern{
			{NamePattern: "readData", SigPattern: "context.Context"},
		},
	}
	dataflow.AnnotateGraph(g, cfgMatch)

	src := g.GetNode(srcID)
	if src.Metadata["data_role"] != "source" {
		t.Error("expected SigPattern match to detect source")
	}

	// Pattern with SigPattern that does NOT match — same node in fresh graph.
	g2 := graph.New("/repo")
	src2ID := addFn(g2, "/repo/h.go", "readData", "api", "func readData(ctx context.Context) []byte")
	sink2ID := addFn(g2, "/repo/db.go", "insertRow", "db", "func insertRow(db *sql.DB, v string)")
	g2.GetNode(sink2ID).Metadata["signature"] = "func insertRow(db *sql.DB, v string)"
	g2.AddEdge(&graph.Edge{From: src2ID, To: sink2ID, Type: graph.EdgeCalls})

	cfgNoMatch := &config.Config{
		DataFlowSources: []config.DataFlowPattern{
			{NamePattern: "readData", SigPattern: "http.Request"}, // not in sig
		},
	}
	dataflow.AnnotateGraph(g2, cfgNoMatch)

	src2 := g2.GetNode(src2ID)
	if src2.Metadata["data_role"] == "source" {
		t.Error("expected SigPattern mismatch to NOT detect source")
	}
}

// TestAnnotateGraph_DiamondGraph verifies that BFS correctly handles a diamond
// call graph (source → A → sink AND source → B → sink), exercising the
// "already visited" skip path.
func TestAnnotateGraph_DiamondGraph(t *testing.T) {
	g := graph.New("/repo")

	srcID := addFn(g, "/repo/h.go", "HandleReq", "web", "func HandleReq(r *http.Request)")
	aID := addFn(g, "/repo/svc.go", "processA", "svc", "func processA(s string)")
	bID := addFn(g, "/repo/svc.go", "processB", "svc", "func processB(s string)")
	sinkID := addFn(g, "/repo/db.go", "execQuery", "db", "func execQuery(db *sql.DB, q string)")
	g.GetNode(sinkID).Metadata["signature"] = "func execQuery(db *sql.DB, q string)"

	// Diamond: two paths from source to sink.
	g.AddEdge(&graph.Edge{From: srcID, To: aID, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: srcID, To: bID, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: aID, To: sinkID, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: bID, To: sinkID, Type: graph.EdgeCalls})

	n := dataflow.AnnotateGraph(g, emptyCfg())
	// Only 1 DATA_FLOWS edge should be created (dedup in BFS).
	if n != 1 {
		t.Errorf("expected 1 DATA_FLOWS edge in diamond graph, got %d", n)
	}
}
