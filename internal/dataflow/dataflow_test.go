package dataflow_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/dataflow"
	"github.com/SynapsesOS/synapses/internal/graph"
)

// emptyCfg returns a config with no custom data-flow patterns.
func emptyCfg() *config.Config {
	return &config.Config{}
}

// addFn adds a NodeFunction to the graph and returns its ID.
func addFn(g *graph.Graph, file, name, pkg, sig string) graph.NodeID {
	id := g.MakeNodeID(file, name)
	g.AddNode(&graph.Node{
		ID:       id,
		Type:     graph.NodeFunction,
		Name:     name,
		Package:  pkg,
		File:     file,
		Metadata: map[string]string{"signature": sig},
	})
	return id
}

// addMethod adds a NodeMethod to the graph and returns its ID.
func addMethod(g *graph.Graph, file, name, pkg, sig string) graph.NodeID {
	id := g.MakeNodeID(file, name)
	g.AddNode(&graph.Node{
		ID:       id,
		Type:     graph.NodeMethod,
		Name:     name,
		Package:  pkg,
		File:     file,
		Metadata: map[string]string{"signature": sig},
	})
	return id
}

// countDataFlowEdges returns the number of DATA_FLOWS edges in the graph.
func countDataFlowEdges(g *graph.Graph) int {
	n := 0
	for _, e := range g.AllEdges() {
		if e.Type == graph.EdgeDataFlows {
			n++
		}
	}
	return n
}

// --- Source / sink detection via built-in heuristics ---

func TestAnnotateGraph_HTTPSource(t *testing.T) {
	g := graph.New("testrepo")

	// Handler with *http.Request in signature → source.
	handlerID := addFn(g, "handler.go", "HandleLogin", "web",
		"func HandleLogin(w http.ResponseWriter, r *http.Request)")

	// SQL sink.
	queryID := addMethod(g, "db.go", "DB.Query", "db",
		"func (d *DB) Query(sql string) (*sql.Rows, error)")
	// Ensure DB.Query is in *sql.DB signature territory — override with sql.DB sig
	g.GetNode(queryID).Metadata["signature"] = "func (d *sql.DB) Query(q string) (*sql.Rows, error)"

	// source → sink via CALLS
	g.AddEdge(&graph.Edge{From: handlerID, To: queryID, Type: graph.EdgeCalls})

	n := dataflow.AnnotateGraph(g, emptyCfg())
	if n == 0 {
		t.Error("expected at least 1 DATA_FLOWS edge (HTTP handler → SQL sink)")
	}

	srcNode := g.GetNode(handlerID)
	if srcNode.Metadata["data_role"] != "source" {
		t.Errorf("HandleLogin data_role = %q, want source", srcNode.Metadata["data_role"])
	}
	if srcNode.Metadata["data_label"] != "http_input" {
		t.Errorf("HandleLogin data_label = %q, want http_input", srcNode.Metadata["data_label"])
	}

	sinkNode := g.GetNode(queryID)
	if sinkNode.Metadata["data_role"] != "sink" {
		t.Errorf("DB.Query data_role = %q, want sink", sinkNode.Metadata["data_role"])
	}
}

func TestAnnotateGraph_ParserSource(t *testing.T) {
	g := graph.New("testrepo")

	parseID := addFn(g, "parser.go", "ParseRequest", "api", "func ParseRequest(data []byte) (*Req, error)")
	execID := addFn(g, "db.go", "execQuery", "db", "func execQuery(db *sql.DB, q string) error")

	g.AddEdge(&graph.Edge{From: parseID, To: execID, Type: graph.EdgeCalls})

	dataflow.AnnotateGraph(g, emptyCfg())

	parse := g.GetNode(parseID)
	if parse.Metadata["data_role"] != "source" {
		t.Errorf("ParseRequest data_role = %q, want source", parse.Metadata["data_role"])
	}
	if parse.Metadata["data_label"] != "parser" {
		t.Errorf("ParseRequest data_label = %q, want parser", parse.Metadata["data_label"])
	}
}

func TestAnnotateGraph_BFSReachability(t *testing.T) {
	g := graph.New("testrepo")

	// Chain: source → middle → sink (2 hops — within default maxHops=4)
	sourceID := addFn(g, "h.go", "HandleReq", "web", "func HandleReq(r *http.Request)")
	middleID := addFn(g, "svc.go", "processInput", "svc", "func processInput(s string)")
	sinkID := addFn(g, "db.go", "saveRecord", "db", "func saveRecord(db *sql.DB, r Record)")
	// Override sink sig to hit *sql.DB heuristic
	g.GetNode(sinkID).Metadata["signature"] = "func saveRecord(db *sql.DB, r Record)"

	g.AddEdge(&graph.Edge{From: sourceID, To: middleID, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: middleID, To: sinkID, Type: graph.EdgeCalls})

	n := dataflow.AnnotateGraph(g, emptyCfg())
	if n == 0 {
		t.Error("expected DATA_FLOWS edge via 2-hop BFS")
	}

	// Verify the DATA_FLOWS edge goes directly source → sink (summary edge).
	found := false
	for _, e := range g.AllEdges() {
		if e.Type == graph.EdgeDataFlows && e.From == sourceID && e.To == sinkID {
			found = true
		}
	}
	if !found {
		t.Error("expected DATA_FLOWS edge source → sink")
	}
}

func TestAnnotateGraph_NoSourcesNoSinks(t *testing.T) {
	g := graph.New("testrepo")
	// Plain functions with no matching heuristics.
	addFn(g, "a.go", "Foo", "p", "func Foo(x int) int")
	addFn(g, "a.go", "Bar", "p", "func Bar(x int) int")

	n := dataflow.AnnotateGraph(g, emptyCfg())
	if n != 0 {
		t.Errorf("expected 0 DATA_FLOWS edges with no sources/sinks, got %d", n)
	}
}

func TestAnnotateGraph_NoDuplicateEdges(t *testing.T) {
	g := graph.New("testrepo")

	sourceID := addFn(g, "h.go", "HandleReq", "web", "func HandleReq(r *http.Request)")
	sinkID := addFn(g, "db.go", "execSQL", "db", "func execSQL(db *sql.DB, q string)")
	g.GetNode(sinkID).Metadata["signature"] = "func execSQL(db *sql.DB, q string)"

	g.AddEdge(&graph.Edge{From: sourceID, To: sinkID, Type: graph.EdgeCalls})

	dataflow.AnnotateGraph(g, emptyCfg())
	// Call again — must not create duplicate DATA_FLOWS edges.
	dataflow.AnnotateGraph(g, emptyCfg())

	n := countDataFlowEdges(g)
	if n > 1 {
		t.Errorf("expected 1 DATA_FLOWS edge after 2 calls (dedup), got %d", n)
	}
}

func TestAnnotateGraph_MaxHopsRespected(t *testing.T) {
	g := graph.New("testrepo")

	// Chain longer than maxHops (default 4).
	// source → h1 → h2 → h3 → h4 → sink (5 hops — should NOT reach sink)
	sourceID := addFn(g, "h.go", "Handle", "web", "func Handle(r *http.Request)")
	prev := sourceID
	for i := range 5 {
		name := "middle" + string(rune('A'+i))
		id := addFn(g, "m.go", name, "svc", "func "+name+"()")
		g.AddEdge(&graph.Edge{From: prev, To: id, Type: graph.EdgeCalls})
		prev = id
	}
	// Sink at hop 6.
	sinkID := addFn(g, "db.go", "execFinal", "db", "func execFinal(db *sql.DB)")
	g.GetNode(sinkID).Metadata["signature"] = "func execFinal(db *sql.DB)"
	g.AddEdge(&graph.Edge{From: prev, To: sinkID, Type: graph.EdgeCalls})

	cfg := &config.Config{DataFlowMaxHops: 4}
	n := dataflow.AnnotateGraph(g, cfg)
	if n != 0 {
		t.Errorf("expected 0 DATA_FLOWS edges when sink is beyond maxHops=4, got %d", n)
	}
}

func TestAnnotateGraph_CustomSourcePattern(t *testing.T) {
	g := graph.New("testrepo")

	// A function that doesn't match built-ins but matches a custom config pattern.
	customSrcID := addFn(g, "custom.go", "readFromQueue", "queue", "func readFromQueue() ([]byte, error)")
	sinkID := addFn(g, "db.go", "insertRow", "db", "func insertRow(db *sql.DB, v string)")
	g.GetNode(sinkID).Metadata["signature"] = "func insertRow(db *sql.DB, v string)"

	g.AddEdge(&graph.Edge{From: customSrcID, To: sinkID, Type: graph.EdgeCalls})

	cfg := &config.Config{
		DataFlowSources: []config.DataFlowPattern{
			{NamePattern: "readFromQueue", Label: "queue_consumer"},
		},
	}

	dataflow.AnnotateGraph(g, cfg)

	src := g.GetNode(customSrcID)
	if src.Metadata["data_role"] != "source" {
		t.Errorf("readFromQueue data_role = %q, want source (custom pattern)", src.Metadata["data_role"])
	}
	if src.Metadata["data_label"] != "queue_consumer" {
		t.Errorf("readFromQueue data_label = %q, want queue_consumer", src.Metadata["data_label"])
	}
}

func TestAnnotateGraph_CustomSinkPattern(t *testing.T) {
	g := graph.New("testrepo")

	sourceID := addFn(g, "h.go", "Decode", "api", "func Decode(r io.Reader)")
	customSinkID := addFn(g, "log.go", "sendToSplunk", "logging", "func sendToSplunk(event string)")

	g.AddEdge(&graph.Edge{From: sourceID, To: customSinkID, Type: graph.EdgeCalls})

	cfg := &config.Config{
		DataFlowSinks: []config.DataFlowPattern{
			{NamePattern: "sendToSplunk", Label: "external_log_sink"},
		},
	}

	n := dataflow.AnnotateGraph(g, cfg)
	if n == 0 {
		t.Error("expected DATA_FLOWS edge with custom sink pattern")
	}

	sink := g.GetNode(customSinkID)
	if sink.Metadata["data_role"] != "sink" {
		t.Errorf("sendToSplunk data_role = %q, want sink", sink.Metadata["data_role"])
	}
}
