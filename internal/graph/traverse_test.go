package graph_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// buildCarveFixture creates a 4-layer graph for traversal tests.
//
//	handler.go
//	  └─DEFINES─► HandleLogin   (func)
//	HandleLogin ─CALLS─► AuthService  (struct, auth.go)
//	AuthService ─CALLS─► UserRepo     (struct, db.go)
//	UserRepo    ─CALLS─► Database     (struct, db.go)
func buildCarveFixture(t *testing.T) (*graph.Graph, map[string]graph.NodeID) {
	t.Helper()
	g := graph.New("testrepo")

	ids := map[string]graph.NodeID{
		"handler":  g.MakeNodeID("handler.go", "HandleLogin"),
		"service":  g.MakeNodeID("auth.go", "AuthService"),
		"repo":     g.MakeNodeID("db.go", "UserRepo"),
		"database": g.MakeNodeID("db.go", "Database"),
	}

	g.AddNode(&graph.Node{ID: ids["handler"], Type: graph.NodeFunction, Name: "HandleLogin", File: "handler.go"})
	g.AddNode(&graph.Node{ID: ids["service"], Type: graph.NodeStruct, Name: "AuthService", File: "auth.go"})
	g.AddNode(&graph.Node{ID: ids["repo"], Type: graph.NodeStruct, Name: "UserRepo", File: "db.go"})
	g.AddNode(&graph.Node{ID: ids["database"], Type: graph.NodeStruct, Name: "Database", File: "db.go"})

	g.AddEdge(&graph.Edge{From: ids["handler"], To: ids["service"], Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: ids["service"], To: ids["repo"], Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: ids["repo"], To: ids["database"], Type: graph.EdgeCalls})

	return g, ids
}

func TestCarveEgoGraph_RootAlwaysPresent(t *testing.T) {
	g, ids := buildCarveFixture(t)

	sub, err := g.CarveEgoGraph(ids["service"], graph.DefaultCarveConfig())
	if err != nil {
		t.Fatalf("CarveEgoGraph returned error: %v", err)
	}
	if sub.Root != ids["service"] {
		t.Errorf("SubGraph.Root = %s, want %s", sub.Root, ids["service"])
	}

	foundRoot := false
	for _, cn := range sub.Nodes {
		if cn.Node.ID == ids["service"] {
			foundRoot = true
			if cn.Relevance != 1.0 {
				t.Errorf("root node relevance = %f, want 1.0", cn.Relevance)
			}
			if cn.Hop != 0 {
				t.Errorf("root node hop = %d, want 0", cn.Hop)
			}
		}
	}
	if !foundRoot {
		t.Error("root node missing from SubGraph.Nodes")
	}
}

func TestCarveEgoGraph_DepthLimits(t *testing.T) {
	g, ids := buildCarveFixture(t)

	cfg := graph.DefaultCarveConfig()
	cfg.MaxDepth = 1

	sub, err := g.CarveEgoGraph(ids["service"], cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	nodeIDs := nodeIDSet(sub)

	// Hop-1 neighbours should be present.
	if _, ok := nodeIDs[ids["handler"]]; !ok {
		t.Error("hop-1 neighbour HandleLogin missing with depth=1")
	}
	if _, ok := nodeIDs[ids["repo"]]; !ok {
		t.Error("hop-1 neighbour UserRepo missing with depth=1")
	}

	// Hop-2 node must NOT appear.
	if _, ok := nodeIDs[ids["database"]]; ok {
		t.Error("hop-2 node Database present with depth=1")
	}
}

func TestCarveEgoGraph_RelevanceDecay(t *testing.T) {
	g, ids := buildCarveFixture(t)

	cfg := graph.DefaultCarveConfig()
	cfg.MaxDepth = 2
	cfg.DecayFactor = 0.5

	sub, err := g.CarveEgoGraph(ids["service"], cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	relevance := make(map[graph.NodeID]float64)
	for _, cn := range sub.Nodes {
		relevance[cn.Node.ID] = cn.Relevance
	}

	// Root must have highest relevance.
	rootRel := relevance[ids["service"]]
	hop1Rel := relevance[ids["handler"]]

	if rootRel <= hop1Rel {
		t.Errorf("root relevance (%f) should be > hop-1 relevance (%f)", rootRel, hop1Rel)
	}
}

func TestCarveEgoGraph_TokenBudgetPrunesLowRelevance(t *testing.T) {
	g, ids := buildCarveFixture(t)

	cfg := graph.DefaultCarveConfig()
	cfg.MaxDepth = 3
	// Budget so tight it can only fit 1 node (per-node byte estimation kicks in).
	cfg.TokenBudget = 1

	sub, err := g.CarveEgoGraph(ids["handler"], cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(sub.Nodes) > 1 {
		t.Errorf("token budget of 80 should yield ≤1 node, got %d", len(sub.Nodes))
	}
}

func TestCarveEgoGraph_EdgesOnlyBetweenSurvivingNodes(t *testing.T) {
	g, ids := buildCarveFixture(t)

	cfg := graph.DefaultCarveConfig()
	cfg.MaxDepth = 1
	sub, err := g.CarveEgoGraph(ids["service"], cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	nodeIDs := nodeIDSet(sub)
	for _, e := range sub.Edges {
		if _, ok := nodeIDs[e.From]; !ok {
			t.Errorf("edge %s→%s: From node not in SubGraph", e.From, e.To)
		}
		if _, ok := nodeIDs[e.To]; !ok {
			t.Errorf("edge %s→%s: To node not in SubGraph", e.From, e.To)
		}
	}
}

func TestCarveEgoGraph_UnknownRootReturnsError(t *testing.T) {
	g := graph.New("testrepo")
	_, err := g.CarveEgoGraph("nonexistent::node", graph.DefaultCarveConfig())
	if err == nil {
		t.Error("expected error for unknown root node, got nil")
	}
}

func TestCarveEgoGraph_NoDuplicateEdges(t *testing.T) {
	g, ids := buildCarveFixture(t)

	sub, err := g.CarveEgoGraph(ids["service"], graph.DefaultCarveConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	seen := make(map[[2]graph.NodeID]struct{})
	for _, e := range sub.Edges {
		key := [2]graph.NodeID{e.From, e.To}
		if _, dup := seen[key]; dup {
			t.Errorf("duplicate edge in SubGraph: %s → %s", e.From, e.To)
		}
		seen[key] = struct{}{}
	}
}

func TestCarveEgoGraph_StructSeedsWithMethods(t *testing.T) {
	g := graph.New("testrepo")

	structID := g.MakeNodeID("svc.go", "Service")
	method1ID := g.MakeNodeID("svc.go", "Service.Start")
	method2ID := g.MakeNodeID("svc.go", "Service.Stop")
	helperID := g.MakeNodeID("util.go", "doWork")

	g.AddNode(&graph.Node{ID: structID, Type: graph.NodeStruct, Name: "Service", File: "svc.go"})
	g.AddNode(&graph.Node{ID: method1ID, Type: graph.NodeMethod, Name: "Service.Start", File: "svc.go"})
	g.AddNode(&graph.Node{ID: method2ID, Type: graph.NodeMethod, Name: "Service.Stop", File: "svc.go"})
	g.AddNode(&graph.Node{ID: helperID, Type: graph.NodeFunction, Name: "doWork", File: "util.go"})

	// Methods call a helper.
	g.AddEdge(&graph.Edge{From: method1ID, To: helperID, Type: graph.EdgeCalls})

	cfg := graph.DefaultCarveConfig()
	cfg.MaxDepth = 2
	cfg.MinRelevance = 0.0

	sub, err := g.CarveEgoGraph(structID, cfg)
	if err != nil {
		t.Fatalf("CarveEgoGraph: %v", err)
	}

	ids := nodeIDSet(sub)

	// Methods of the struct should be seeded into BFS.
	if _, ok := ids[method1ID]; !ok {
		t.Error("Service.Start should be in subgraph (struct method seeding)")
	}
	if _, ok := ids[method2ID]; !ok {
		t.Error("Service.Stop should be in subgraph (struct method seeding)")
	}
	// Helper called by method should be reachable.
	if _, ok := ids[helperID]; !ok {
		t.Error("doWork should be reachable via struct method seeding")
	}
}

func TestCarveEgoGraph_TruncationSignal(t *testing.T) {
	g := graph.New("testrepo")

	// Create a chain: root → n1 → n2 → n3 → n4
	root := g.MakeNodeID("a.go", "root")
	g.AddNode(&graph.Node{ID: root, Type: graph.NodeFunction, Name: "root", File: "a.go"})
	prev := root
	for i := 0; i < 4; i++ {
		name := string(rune('A'+i)) + "fn"
		id := g.MakeNodeID("a.go", name)
		g.AddNode(&graph.Node{ID: id, Type: graph.NodeFunction, Name: name, File: "a.go"})
		g.AddEdge(&graph.Edge{From: prev, To: id, Type: graph.EdgeCalls})
		prev = id
	}

	cfg := graph.DefaultCarveConfig()
	cfg.MaxDepth = 5
	cfg.TokenBudget = 1 // extremely tight — will truncate

	sub, err := g.CarveEgoGraph(root, cfg)
	if err != nil {
		t.Fatalf("CarveEgoGraph: %v", err)
	}

	if !sub.Truncated {
		t.Error("expected Truncated=true with very tight budget")
	}
	if sub.TruncatedCount == 0 {
		t.Error("expected TruncatedCount > 0")
	}
}

// ── FindTestsFor ──────────────────────────────────────────────────────────────

func TestFindTestsFor_DirectTestCaller(t *testing.T) {
	g := graph.New("repo")
	serviceID := g.MakeNodeID("service.go", "Service")
	testID := g.MakeNodeID("service_test.go", "TestService")

	g.AddNode(&graph.Node{ID: serviceID, Name: "Service", Type: graph.NodeFunction, File: "service.go"})
	g.AddNode(&graph.Node{ID: testID, Name: "TestService", Type: graph.NodeFunction, File: "service_test.go"})
	g.AddEdge(&graph.Edge{From: testID, To: serviceID, Type: graph.EdgeCalls})

	files := g.FindTestsFor(serviceID)
	if len(files) != 1 || files[0] != "service_test.go" {
		t.Errorf("expected [service_test.go], got %v", files)
	}
}

func TestFindTestsFor_NonTestCallerNotIncluded(t *testing.T) {
	g := graph.New("repo")
	serviceID := g.MakeNodeID("service.go", "Service")
	callerID := g.MakeNodeID("handler.go", "Handler")

	g.AddNode(&graph.Node{ID: serviceID, Name: "Service", Type: graph.NodeFunction, File: "service.go"})
	g.AddNode(&graph.Node{ID: callerID, Name: "Handler", Type: graph.NodeFunction, File: "handler.go"})
	g.AddEdge(&graph.Edge{From: callerID, To: serviceID, Type: graph.EdgeCalls})

	files := g.FindTestsFor(serviceID)
	if len(files) != 0 {
		t.Errorf("expected no test files, got %v", files)
	}
}

func TestFindTestsFor_IndirectTestViaHelper(t *testing.T) {
	// test → helper → service: test file should still be found via BFS
	g := graph.New("repo")
	serviceID := g.MakeNodeID("service.go", "Service")
	helperID := g.MakeNodeID("helpers.go", "setupService")
	testID := g.MakeNodeID("service_test.go", "TestIntegration")

	g.AddNode(&graph.Node{ID: serviceID, Name: "Service", Type: graph.NodeFunction, File: "service.go"})
	g.AddNode(&graph.Node{ID: helperID, Name: "setupService", Type: graph.NodeFunction, File: "helpers.go"})
	g.AddNode(&graph.Node{ID: testID, Name: "TestIntegration", Type: graph.NodeFunction, File: "service_test.go"})
	g.AddEdge(&graph.Edge{From: helperID, To: serviceID, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: testID, To: helperID, Type: graph.EdgeCalls})

	files := g.FindTestsFor(serviceID)
	if len(files) != 1 || files[0] != "service_test.go" {
		t.Errorf("expected [service_test.go], got %v", files)
	}
}

func TestFindTestsFor_NoCallers(t *testing.T) {
	g := graph.New("repo")
	serviceID := g.MakeNodeID("service.go", "Isolated")
	g.AddNode(&graph.Node{ID: serviceID, Name: "Isolated", Type: graph.NodeFunction, File: "service.go"})

	files := g.FindTestsFor(serviceID)
	if len(files) != 0 {
		t.Errorf("expected empty, got %v", files)
	}
}

func TestFindTestsFor_UnknownNode(t *testing.T) {
	g := graph.New("repo")
	files := g.FindTestsFor("nonexistent-id")
	if files != nil {
		t.Errorf("expected nil for unknown node, got %v", files)
	}
}

// ── isTestFile (Python prefix-naming fix) ────────────────────────────────────

func TestFindTestsFor_PythonPrefixTestFile(t *testing.T) {
	// Bug fix: "test_.py" suffix was wrong — test_auth.py has a prefix not suffix.
	g := graph.New("repo")
	funcID := g.MakeNodeID("auth.py", "validate_token")
	testID := g.MakeNodeID("test_auth.py", "test_validate_token")

	g.AddNode(&graph.Node{ID: funcID, Name: "validate_token", Type: graph.NodeFunction, File: "auth.py"})
	g.AddNode(&graph.Node{ID: testID, Name: "test_validate_token", Type: graph.NodeFunction, File: "test_auth.py"})
	g.AddEdge(&graph.Edge{From: testID, To: funcID, Type: graph.EdgeCalls})

	files := g.FindTestsFor(funcID)
	if len(files) != 1 || files[0] != "test_auth.py" {
		t.Errorf("expected [test_auth.py] for Python prefix-named test, got %v", files)
	}
}

func TestFindTestsFor_PythonSuffixTestFile(t *testing.T) {
	g := graph.New("repo")
	funcID := g.MakeNodeID("utils.py", "helper")
	testID := g.MakeNodeID("utils_test.py", "test_helper")

	g.AddNode(&graph.Node{ID: funcID, Name: "helper", Type: graph.NodeFunction, File: "utils.py"})
	g.AddNode(&graph.Node{ID: testID, Name: "test_helper", Type: graph.NodeFunction, File: "utils_test.py"})
	g.AddEdge(&graph.Edge{From: testID, To: funcID, Type: graph.EdgeCalls})

	files := g.FindTestsFor(funcID)
	if len(files) != 1 || files[0] != "utils_test.py" {
		t.Errorf("expected [utils_test.py] for Python suffix-named test, got %v", files)
	}
}

// nodeIDSet returns the set of NodeIDs present in a SubGraph for quick lookup.
func nodeIDSet(sub *graph.SubGraph) map[graph.NodeID]struct{} {
	m := make(map[graph.NodeID]struct{}, len(sub.Nodes))
	for _, cn := range sub.Nodes {
		m[cn.Node.ID] = struct{}{}
	}
	return m
}
