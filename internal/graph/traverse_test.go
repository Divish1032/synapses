package graph_test

import (
	"fmt"
	"math"
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
	cfg.UsePPR = false // test BFS depth limits specifically

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

// ── Adaptive decay ────────────────────────────────────────────────────────────

// TestCarveEgoGraph_AdaptiveDecay_HubChildLowerThanNarrowChild verifies that
// children of a high-degree hub receive lower relevance than children of a
// low-degree narrow-chain node at the same hop depth. This is the core
// correctness property of degree-normalized adaptive decay.
//
// Graph:
//
//	root ─CALLS─► hub   ─CALLS─► h1, h2, h3, h4, h5  (5 children)
//	root ─CALLS─► narrow ─CALLS─► n1                   (1 child)
func TestCarveEgoGraph_AdaptiveDecay_HubChildLowerThanNarrowChild(t *testing.T) {
	g := graph.New("testrepo")

	rootID := g.MakeNodeID("root.go", "root")
	hubID := g.MakeNodeID("hub.go", "hub")
	narrowID := g.MakeNodeID("narrow.go", "narrow")
	n1ID := g.MakeNodeID("narrow.go", "n1")
	h1ID := g.MakeNodeID("hub.go", "h1")
	h2ID := g.MakeNodeID("hub.go", "h2")
	h3ID := g.MakeNodeID("hub.go", "h3")
	h4ID := g.MakeNodeID("hub.go", "h4")
	h5ID := g.MakeNodeID("hub.go", "h5")

	for _, id := range []graph.NodeID{rootID, hubID, narrowID, n1ID, h1ID, h2ID, h3ID, h4ID, h5ID} {
		g.AddNode(&graph.Node{ID: id, Type: graph.NodeFunction, Name: string(id), File: "test.go"})
	}

	g.AddEdge(&graph.Edge{From: rootID, To: hubID, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: rootID, To: narrowID, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: narrowID, To: n1ID, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: hubID, To: h1ID, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: hubID, To: h2ID, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: hubID, To: h3ID, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: hubID, To: h4ID, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: hubID, To: h5ID, Type: graph.EdgeCalls})

	cfg := graph.DefaultCarveConfig()
	cfg.MaxDepth = 2
	cfg.DecayFactor = 0.5
	cfg.MinRelevance = 0 // test scoring, not pruning

	sub, err := g.CarveEgoGraph(rootID, cfg)
	if err != nil {
		t.Fatalf("CarveEgoGraph: %v", err)
	}

	rel := make(map[graph.NodeID]float64)
	for _, cn := range sub.Nodes {
		rel[cn.Node.ID] = cn.Relevance
	}

	// n1 is a child of a 2-edge node (narrow + root←narrow); hub children come
	// from a 6-edge node (hub + 5 children + root←hub). Narrow chain's child
	// must score strictly higher than any hub child.
	if rel[n1ID] == 0 {
		t.Fatal("narrow child n1 missing from subgraph")
	}
	if rel[h1ID] == 0 {
		t.Fatal("hub child h1 missing from subgraph")
	}
	if rel[n1ID] <= rel[h1ID] {
		t.Errorf("narrow child relevance (%v) should be > hub child relevance (%v): adaptive decay not working", rel[n1ID], rel[h1ID])
	}
}

// TestCarveEgoGraph_AdaptiveDecay_ProductionPruning verifies that under the
// default production config (MinRelevance=0.01, TokenBudget=4000):
//
//  1. n1 (narrow chain hop-2) is NOT falsely pruned by MinRelevance=0.01.
//  2. n1 scores strictly above all hub hop-2 children.
//
// This guarantees the token budget will always select n1 over hub children
// when capacity is limited — the core hub-explosion prevention property.
func TestCarveEgoGraph_AdaptiveDecay_ProductionPruning(t *testing.T) {
	g := graph.New("testrepo")

	rootID := g.MakeNodeID("root.go", "root")
	hubID := g.MakeNodeID("hub.go", "hub")
	narrowID := g.MakeNodeID("narrow.go", "narrow")
	n1ID := g.MakeNodeID("narrow.go", "n1")

	// Hub has 19 children + 1 incoming (root→hub) = 20 total edges.
	const numHubChildren = 19
	var hubChildIDs []graph.NodeID
	for i := 0; i < numHubChildren; i++ {
		id := g.MakeNodeID("hub.go", fmt.Sprintf("hc%d", i))
		g.AddNode(&graph.Node{ID: id, Type: graph.NodeFunction, Name: fmt.Sprintf("hc%d", i), File: "hub.go"})
		hubChildIDs = append(hubChildIDs, id)
	}
	for _, id := range []graph.NodeID{rootID, hubID, narrowID, n1ID} {
		g.AddNode(&graph.Node{ID: id, Type: graph.NodeFunction, Name: string(id), File: "test.go"})
	}
	g.AddEdge(&graph.Edge{From: rootID, To: hubID, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: rootID, To: narrowID, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: narrowID, To: n1ID, Type: graph.EdgeCalls})
	for _, hcID := range hubChildIDs {
		g.AddEdge(&graph.Edge{From: hubID, To: hcID, Type: graph.EdgeCalls})
	}

	// Default production config — MinRelevance=0.01, TokenBudget=4000.
	cfg := graph.DefaultCarveConfig()
	cfg.MaxDepth = 2

	sub, err := g.CarveEgoGraph(rootID, cfg)
	if err != nil {
		t.Fatalf("CarveEgoGraph: %v", err)
	}
	rel := make(map[graph.NodeID]float64)
	for _, cn := range sub.Nodes {
		rel[cn.Node.ID] = cn.Relevance
	}

	// Property 1: n1 is not falsely pruned by MinRelevance=0.01.
	if rel[n1ID] == 0 {
		t.Fatalf("narrow chain hop-2 node n1 pruned by default MinRelevance — adaptive decay too aggressive for degree-2 nodes")
	}

	// Property 2: n1 outranks every hub child that survived MinRelevance.
	for i, hcID := range hubChildIDs {
		if rel[hcID] == 0 {
			continue // hub child pruned — acceptable
		}
		if rel[n1ID] <= rel[hcID] {
			t.Errorf("hub child hc%d (rel=%v) ≥ n1 (rel=%v): adaptive decay must rank narrow chain higher than hub bulk", i, rel[hcID], rel[n1ID])
		}
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

// ── Personalized PageRank ─────────────────────────────────────────────────────

// TestCarveEgoGraph_PPR_DiamondBoost verifies the core PPR value proposition:
// a convergent node reached by 2 independent call paths outranks structurally
// equivalent single-path nodes. BFS max-score heuristic cannot distinguish these.
//
// Graph (same as spike Topology 1):
//
//	Root → A → C   (C reached via A)
//	Root → B → C   (C reached via B — 2 independent paths)
//	     A → D     (D unique to A's subtree)
//	     B → E     (E unique to B's subtree)
//
// Expected with UsePPR=true: rank(C) > rank(D) == rank(E)
func TestCarveEgoGraph_PPR_DiamondBoost(t *testing.T) {
	g := graph.New("ppr-diamond")
	mkID := func(name string) graph.NodeID { return g.MakeNodeID("main.go", name) }

	ids := map[string]graph.NodeID{
		"Root": mkID("Root"),
		"A":    mkID("A"),
		"B":    mkID("B"),
		"C":    mkID("C"), // 2-path convergent — must rank highest
		"D":    mkID("D"), // 1-path unique to A
		"E":    mkID("E"), // 1-path unique to B
	}
	for name, id := range ids {
		g.AddNode(&graph.Node{ID: id, Type: graph.NodeFunction, Name: name, File: "main.go"})
	}
	g.AddEdge(&graph.Edge{From: ids["Root"], To: ids["A"], Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: ids["Root"], To: ids["B"], Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: ids["A"], To: ids["C"], Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: ids["B"], To: ids["C"], Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: ids["A"], To: ids["D"], Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: ids["B"], To: ids["E"], Type: graph.EdgeCalls})

	cfg := graph.DefaultCarveConfig()
	cfg.MaxDepth = 5
	cfg.MinRelevance = 0
	cfg.TokenBudget = 0
	cfg.UsePPR = true

	sub, err := g.CarveEgoGraph(ids["Root"], cfg)
	if err != nil {
		t.Fatalf("CarveEgoGraph PPR: %v", err)
	}

	rel := make(map[graph.NodeID]float64)
	for _, cn := range sub.Nodes {
		rel[cn.Node.ID] = cn.Relevance
	}

	if rel[ids["C"]] <= rel[ids["D"]] {
		t.Errorf("PPR: C (%.5f) should outrank D (%.5f) — C has 2 incoming paths", rel[ids["C"]], rel[ids["D"]])
	}
	if rel[ids["C"]] <= rel[ids["E"]] {
		t.Errorf("PPR: C (%.5f) should outrank E (%.5f) — C has 2 incoming paths", rel[ids["C"]], rel[ids["E"]])
	}
	t.Logf("PPR diamond: C=%.5f D=%.5f E=%.5f C/D=%.2fx", rel[ids["C"]], rel[ids["D"]], rel[ids["E"]], rel[ids["C"]]/rel[ids["D"]])
}

// TestCarveEgoGraph_PPR_BFSCannotDistinguishDiamond confirms that BFS (UsePPR=false)
// assigns identical scores to C, D, and E on the diamond topology — the exact
// weakness that PPR corrects.
func TestCarveEgoGraph_PPR_BFSCannotDistinguishDiamond(t *testing.T) {
	g := graph.New("bfs-diamond")
	mkID := func(name string) graph.NodeID { return g.MakeNodeID("main.go", name) }

	ids := map[string]graph.NodeID{
		"Root": mkID("Root"), "A": mkID("A"), "B": mkID("B"),
		"C": mkID("C"), "D": mkID("D"), "E": mkID("E"),
	}
	for name, id := range ids {
		g.AddNode(&graph.Node{ID: id, Type: graph.NodeFunction, Name: name, File: "main.go"})
	}
	g.AddEdge(&graph.Edge{From: ids["Root"], To: ids["A"], Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: ids["Root"], To: ids["B"], Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: ids["A"], To: ids["C"], Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: ids["B"], To: ids["C"], Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: ids["A"], To: ids["D"], Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: ids["B"], To: ids["E"], Type: graph.EdgeCalls})

	cfg := graph.DefaultCarveConfig()
	cfg.MaxDepth = 5
	cfg.MinRelevance = 0
	cfg.TokenBudget = 0
	cfg.UsePPR = false // this test proves BFS cannot distinguish diamond paths

	sub, err := g.CarveEgoGraph(ids["Root"], cfg)
	if err != nil {
		t.Fatalf("CarveEgoGraph BFS: %v", err)
	}

	rel := make(map[graph.NodeID]float64)
	for _, cn := range sub.Nodes {
		rel[cn.Node.ID] = cn.Relevance
	}

	// BFS max-score: C, D, E are hop-2 nodes with equal edge weights → same score.
	const tol = 1e-9
	if math.Abs(rel[ids["C"]]-rel[ids["D"]]) > tol {
		t.Errorf("BFS: C (%.9f) != D (%.9f) — BFS should be blind to path count", rel[ids["C"]], rel[ids["D"]])
	}
	if math.Abs(rel[ids["C"]]-rel[ids["E"]]) > tol {
		t.Errorf("BFS: C (%.9f) != E (%.9f) — BFS should be blind to path count", rel[ids["C"]], rel[ids["E"]])
	}
	t.Logf("BFS diamond confirmed: C=D=E=%.5f", rel[ids["C"]])
}

// TestCarveEgoGraph_PPR_RootAlwaysPinnedToOne verifies that the root node always
// appears at relevance=1.0 regardless of what PPR computes for it. In undirected
// PPR, high-degree neighbours can accumulate more random-walk mass than a degree-1
// root. The root-pin ensures agents always see the queried entity at full relevance.
func TestCarveEgoGraph_PPR_RootAlwaysPinnedToOne(t *testing.T) {
	g := graph.New("ppr-rootpin")

	// Chain: Root(deg=1) → A(deg=2) → B(deg=2) → C(deg=1)
	// In undirected PPR, A and B can rank above Root.
	rootID := g.MakeNodeID("chain.go", "Root")
	aID := g.MakeNodeID("chain.go", "A")
	bID := g.MakeNodeID("chain.go", "B")
	cID := g.MakeNodeID("chain.go", "C")

	for _, id := range []graph.NodeID{rootID, aID, bID, cID} {
		g.AddNode(&graph.Node{ID: id, Type: graph.NodeFunction, Name: string(id), File: "chain.go"})
	}
	g.AddEdge(&graph.Edge{From: rootID, To: aID, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: aID, To: bID, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: bID, To: cID, Type: graph.EdgeCalls})

	cfg := graph.DefaultCarveConfig()
	cfg.UsePPR = true
	cfg.MinRelevance = 0
	cfg.TokenBudget = 0

	sub, err := g.CarveEgoGraph(rootID, cfg)
	if err != nil {
		t.Fatalf("CarveEgoGraph PPR: %v", err)
	}

	for _, cn := range sub.Nodes {
		if cn.Node.ID == rootID {
			if cn.Relevance != 1.0 {
				t.Errorf("root PPR relevance = %.6f, want exactly 1.0 (root-pin)", cn.Relevance)
			}
			return
		}
	}
	t.Error("root node missing from PPR subgraph")
}

// TestCarveEgoGraph_PPR_PostProcessingPreserved verifies PPR results flow through
// the same post-processing pipeline as BFS: MinRelevance, token budget, edge
// deduplication and truncation signal all apply correctly.
func TestCarveEgoGraph_PPR_PostProcessingPreserved(t *testing.T) {
	g, ids := buildCarveFixture(t)

	// MinRelevance=0.99 prunes everything except root (pinned at 1.0).
	cfg := graph.DefaultCarveConfig()
	cfg.UsePPR = true
	cfg.MinRelevance = 0.99
	cfg.MaxDepth = 5
	cfg.TokenBudget = 0

	sub, err := g.CarveEgoGraph(ids["service"], cfg)
	if err != nil {
		t.Fatalf("CarveEgoGraph PPR: %v", err)
	}
	if len(sub.Nodes) != 1 {
		t.Errorf("expected 1 node after MinRelevance=0.99 pruning, got %d", len(sub.Nodes))
	}
	if len(sub.Nodes) > 0 && sub.Nodes[0].Node.ID != ids["service"] {
		t.Errorf("surviving node should be root (service), got %s", sub.Nodes[0].Node.ID)
	}

	// TokenBudget=1 forces truncation.
	cfg2 := graph.DefaultCarveConfig()
	cfg2.UsePPR = true
	cfg2.MinRelevance = 0
	cfg2.TokenBudget = 1

	sub2, err := g.CarveEgoGraph(ids["service"], cfg2)
	if err != nil {
		t.Fatalf("CarveEgoGraph PPR tight budget: %v", err)
	}
	if !sub2.Truncated {
		t.Error("expected Truncated=true with TokenBudget=1 and PPR")
	}

	// Edges only reference surviving nodes.
	ns := nodeIDSet(sub2)
	for _, e := range sub2.Edges {
		if _, ok := ns[e.From]; !ok {
			t.Errorf("PPR edge %s→%s: From not in surviving nodes", e.From, e.To)
		}
		if _, ok := ns[e.To]; !ok {
			t.Errorf("PPR edge %s→%s: To not in surviving nodes", e.From, e.To)
		}
	}
}

// TestCarveEgoGraph_PPR_StructMethodSeeding verifies that PPR correctly seeds
// struct receiver methods into the teleport vector via the idx==nil fallback
// path (no CSR index built — the common path in unit tests and at startup).
// Methods must appear in the output with non-trivial relevance, and nodes
// reachable through those methods must also be included.
func TestCarveEgoGraph_PPR_StructMethodSeeding(t *testing.T) {
	g := graph.New("ppr-struct")

	structID := g.MakeNodeID("svc.go", "Service")
	m1ID := g.MakeNodeID("svc.go", "Service.Start")
	m2ID := g.MakeNodeID("svc.go", "Service.Stop")
	helperID := g.MakeNodeID("util.go", "doWork")
	// unrelated is not reachable from any seeded node — should be absent.
	unrelatedID := g.MakeNodeID("other.go", "Unrelated")

	g.AddNode(&graph.Node{ID: structID, Type: graph.NodeStruct, Name: "Service", File: "svc.go"})
	g.AddNode(&graph.Node{ID: m1ID, Type: graph.NodeMethod, Name: "Service.Start", File: "svc.go"})
	g.AddNode(&graph.Node{ID: m2ID, Type: graph.NodeMethod, Name: "Service.Stop", File: "svc.go"})
	g.AddNode(&graph.Node{ID: helperID, Type: graph.NodeFunction, Name: "doWork", File: "util.go"})
	g.AddNode(&graph.Node{ID: unrelatedID, Type: graph.NodeFunction, Name: "Unrelated", File: "other.go"})

	// Service.Start calls doWork; Service.Stop is a leaf.
	g.AddEdge(&graph.Edge{From: m1ID, To: helperID, Type: graph.EdgeCalls})

	cfg := graph.DefaultCarveConfig()
	cfg.UsePPR = true
	cfg.MinRelevance = 0
	cfg.TokenBudget = 0

	sub, err := g.CarveEgoGraph(structID, cfg)
	if err != nil {
		t.Fatalf("CarveEgoGraph PPR struct: %v", err)
	}

	ids := nodeIDSet(sub)

	// Both methods must be seeded into the teleport vector and therefore
	// appear with non-trivial PPR rank.
	if _, ok := ids[m1ID]; !ok {
		t.Error("Service.Start missing from PPR subgraph — idx=nil method seeding fallback broken")
	}
	if _, ok := ids[m2ID]; !ok {
		t.Error("Service.Stop missing from PPR subgraph — idx=nil method seeding fallback broken")
	}
	// doWork is reachable via Service.Start → must also appear.
	if _, ok := ids[helperID]; !ok {
		t.Error("doWork missing from PPR subgraph — not reachable through method teleport seeds")
	}
	// Root always pinned at 1.0.
	for _, cn := range sub.Nodes {
		if cn.Node.ID == structID && cn.Relevance != 1.0 {
			t.Errorf("struct root relevance = %.6f, want 1.0 (root-pin)", cn.Relevance)
		}
	}
	// Methods should outrank the leaf helper (teleport seeds have higher rank).
	rel := make(map[graph.NodeID]float64)
	for _, cn := range sub.Nodes {
		rel[cn.Node.ID] = cn.Relevance
	}
	if rel[m1ID] <= rel[helperID] {
		t.Errorf("Service.Start (%.5f) should outrank helper doWork (%.5f) — method teleport seeds must dominate", rel[m1ID], rel[helperID])
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

// TestCentralityDelta_MinorChange_CachePreserved verifies that a topology
// change small enough to leave all centrality scores within the flush threshold
// does NOT clear the subgraph cache.
//
// Setup: 3-node chain A→B→C. Prime the cache with a carve. Add one isolated
// node (no edges to existing graph) — centrality delta for A/B/C is exactly 0.
// RebuildIndex must preserve all existing cache entries.
func TestCentralityDelta_MinorChange_CachePreserved(t *testing.T) {
	g := graph.New("minor")
	aID := g.MakeNodeID("f.go", "A")
	bID := g.MakeNodeID("f.go", "B")
	cID := g.MakeNodeID("f.go", "C")
	for _, id := range []graph.NodeID{aID, bID, cID} {
		g.AddNode(&graph.Node{ID: id, Type: graph.NodeFunction, Name: string(id), File: "f.go"})
	}
	g.AddEdge(&graph.Edge{From: aID, To: bID, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: bID, To: cID, Type: graph.EdgeCalls})

	if _, err := g.RebuildIndex(); err != nil {
		t.Fatalf("first RebuildIndex: %v", err)
	}
	cfg := graph.DefaultCarveConfig()
	if _, err := g.CarveEgoGraph(aID, cfg); err != nil {
		t.Fatalf("CarveEgoGraph: %v", err)
	}
	lenBefore := g.CacheLen()
	if lenBefore == 0 {
		t.Fatal("cache should be non-empty after priming")
	}

	// Add an isolated node — zero edges, centrality delta for A/B/C is exactly 0.
	isolatedID := g.MakeNodeID("f.go", "Isolated")
	g.AddNode(&graph.Node{ID: isolatedID, Type: graph.NodeFunction, Name: "Isolated", File: "f.go"})

	if _, err := g.RebuildIndex(); err != nil {
		t.Fatalf("second RebuildIndex: %v", err)
	}
	if g.CacheLen() < lenBefore {
		t.Errorf("cache shrank from %d to %d after minor change — should be preserved",
			lenBefore, g.CacheLen())
	}
}

// TestCentralityBoost_CacheInvalidatedOnRebuild verifies that after a second
// RebuildIndex that promotes a new hub, a fresh CarveEgoGraph call reflects the
// updated centrality — not a stale cache entry from before the rebuild.
//
// Setup: root calls leaf1 and leaf2.
// Initial graph: leaf1 has no other edges (low centrality).
// After rebuild: add 5 callers to leaf1 from a separate file → leaf1 becomes hub.
// Carve root before rebuild → cache populated with old (equal) leaf1/leaf2 scores.
// Carve root after rebuild → cache must be cleared, leaf1 must now outrank leaf2.
func TestCentralityBoost_CacheInvalidatedOnRebuild(t *testing.T) {
	g := graph.New("cachetest")

	rootID := g.MakeNodeID("main.go", "Root")
	leaf1ID := g.MakeNodeID("main.go", "Leaf1")
	leaf2ID := g.MakeNodeID("main.go", "Leaf2")

	g.AddNode(&graph.Node{ID: rootID, Type: graph.NodeFunction, Name: "Root", File: "main.go"})
	g.AddNode(&graph.Node{ID: leaf1ID, Type: graph.NodeFunction, Name: "Leaf1", File: "main.go"})
	g.AddNode(&graph.Node{ID: leaf2ID, Type: graph.NodeFunction, Name: "Leaf2", File: "main.go"})
	g.AddEdge(&graph.Edge{From: rootID, To: leaf1ID, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: rootID, To: leaf2ID, Type: graph.EdgeCalls})

	if _, err := g.RebuildIndex(); err != nil {
		t.Fatalf("first RebuildIndex: %v", err)
	}

	cfg := graph.DefaultCarveConfig()

	// Warm the cache: both leaves are at equal 1-hop relevance, equal centrality.
	sub1, err := g.CarveEgoGraph(rootID, cfg)
	if err != nil {
		t.Fatalf("first CarveEgoGraph: %v", err)
	}

	var leaf1RelBefore, leaf2RelBefore float64
	for _, cn := range sub1.Nodes {
		switch cn.Node.ID {
		case leaf1ID:
			leaf1RelBefore = cn.Relevance
		case leaf2ID:
			leaf2RelBefore = cn.Relevance
		}
	}
	// Before rebuild both leaves are symmetric — same relevance.
	if leaf1RelBefore != leaf2RelBefore {
		t.Errorf("before rebuild: leaf1 (%f) != leaf2 (%f) — should be equal", leaf1RelBefore, leaf2RelBefore)
	}

	// Add 5 external callers to leaf1 only — makes leaf1 a hub.
	for _, name := range []string{"Ca", "Cb", "Cc", "Cd", "Ce"} {
		cid := g.MakeNodeID("callers.go", name)
		g.AddNode(&graph.Node{ID: cid, Type: graph.NodeFunction, Name: name, File: "callers.go"})
		g.AddEdge(&graph.Edge{From: cid, To: leaf1ID, Type: graph.EdgeCalls})
	}

	// Rebuild updates centrality AND must clear the cache.
	if _, err := g.RebuildIndex(); err != nil {
		t.Fatalf("second RebuildIndex: %v", err)
	}

	// After rebuild the cache must have been cleared.  A fresh call must
	// reflect the new centrality — leaf1 is now the hub, leaf2 is still a leaf.
	sub2, err := g.CarveEgoGraph(rootID, cfg)
	if err != nil {
		t.Fatalf("second CarveEgoGraph: %v", err)
	}

	var leaf1RelAfter, leaf2RelAfter float64
	for _, cn := range sub2.Nodes {
		switch cn.Node.ID {
		case leaf1ID:
			leaf1RelAfter = cn.Relevance
		case leaf2ID:
			leaf2RelAfter = cn.Relevance
		}
	}

	if leaf1RelAfter == 0 || leaf2RelAfter == 0 {
		t.Fatalf("expected both leaves in sub-graph after rebuild: leaf1=%f leaf2=%f", leaf1RelAfter, leaf2RelAfter)
	}
	if leaf1RelAfter <= leaf2RelAfter {
		t.Errorf("after rebuild: leaf1 (%f) should outrank leaf2 (%f) due to centrality boost",
			leaf1RelAfter, leaf2RelAfter)
	}
}

// ── Hybrid Scoring (Sprint 13 #3) ────────────────────────────────────────────

// makeUnitVec builds a unit-length float32 slice with only component [direction]
// set to 1.0. Two vectors with the same direction have cosine similarity 1.0;
// perpendicular vectors have cosine similarity 0.0.
func makeUnitVec(dim, direction int) []float32 {
	v := make([]float32, dim)
	v[direction] = 1.0
	return v
}

// TestCarveEgoGraph_HybridScoring_DisabledByDefault verifies that when neither
// EmbeddingLookup nor HybridLambda is set, scoring is pure structural.
func TestCarveEgoGraph_HybridScoring_DisabledByDefault(t *testing.T) {
	g, ids := buildCarveFixture(t)

	cfg := graph.DefaultCarveConfig()
	// EmbeddingLookup and HybridLambda are zero-value — no blend expected.

	sub, err := g.CarveEgoGraph(ids["handler"], cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, cn := range sub.Nodes {
		if cn.Node.ID == ids["handler"] && cn.Relevance != 1.0 {
			t.Errorf("root relevance = %f, want 1.0", cn.Relevance)
		}
	}
}

// TestCarveEgoGraph_HybridScoring_LambdaZeroNoBlend verifies that lambda=0
// with a lookup provided still produces pure structural scores and does NOT
// invoke the lookup function.
func TestCarveEgoGraph_HybridScoring_LambdaZeroNoBlend(t *testing.T) {
	g, ids := buildCarveFixture(t)

	lookupCalled := false
	cfg := graph.DefaultCarveConfig()
	cfg.EmbeddingLookup = func(nodeIDs []graph.NodeID) map[graph.NodeID][]float32 {
		lookupCalled = true
		return nil
	}
	cfg.HybridLambda = 0 // explicitly zero

	_, err := g.CarveEgoGraph(ids["handler"], cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lookupCalled {
		t.Error("EmbeddingLookup must not be called when HybridLambda=0")
	}
}

// TestCarveEgoGraph_HybridScoring_NilLookupNoBlend verifies that a positive lambda
// with nil EmbeddingLookup produces pure structural scores without panicking.
func TestCarveEgoGraph_HybridScoring_NilLookupNoBlend(t *testing.T) {
	g, ids := buildCarveFixture(t)

	cfg := graph.DefaultCarveConfig()
	cfg.EmbeddingLookup = nil
	cfg.HybridLambda = 0.5

	sub, err := g.CarveEgoGraph(ids["service"], cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, cn := range sub.Nodes {
		if cn.Node.ID == ids["service"] {
			found = true
			if cn.Relevance != 1.0 {
				t.Errorf("root relevance = %f, want 1.0", cn.Relevance)
			}
		}
	}
	if !found {
		t.Fatal("root node not found in result")
	}
}

// TestCarveEgoGraph_HybridScoring_BlendApplied verifies the blend formula:
//
//	finalScore = (1-λ)×structural + λ×cosineSim(embed(root), embed(n))
//
// Uses unit vectors so cosine similarities are exact (0.0 or 1.0):
//   - "service" gets the same-axis embedding as root → cosineSim=1.0 → boosted
//   - "repo"    gets a perpendicular embedding        → cosineSim=0.0 → damped
//
// With λ=0.5, service must rank above repo after the blend.
func TestCarveEgoGraph_HybridScoring_BlendApplied(t *testing.T) {
	g, ids := buildCarveFixture(t)

	const dim = 4
	embeds := map[graph.NodeID][]float32{
		ids["handler"]: makeUnitVec(dim, 0), // root axis
		ids["service"]: makeUnitVec(dim, 0), // same axis as root → sim=1.0
		ids["repo"]:    makeUnitVec(dim, 1), // perpendicular      → sim=0.0
	}

	cfg := graph.DefaultCarveConfig()
	cfg.EmbeddingLookup = func(nodeIDs []graph.NodeID) map[graph.NodeID][]float32 {
		result := make(map[graph.NodeID][]float32, len(nodeIDs))
		for _, id := range nodeIDs {
			if v, ok := embeds[id]; ok {
				result[id] = v
			}
		}
		return result
	}
	cfg.HybridLambda = 0.5

	sub, err := g.CarveEgoGraph(ids["handler"], cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var serviceRel, repoRel float64
	for _, cn := range sub.Nodes {
		switch cn.Node.ID {
		case ids["handler"]:
			if cn.Relevance != 1.0 {
				t.Errorf("root relevance = %f, want 1.0", cn.Relevance)
			}
		case ids["service"]:
			serviceRel = cn.Relevance
		case ids["repo"]:
			repoRel = cn.Relevance
		}
	}
	if serviceRel == 0 {
		t.Fatal("service node not found in result")
	}
	if repoRel == 0 {
		t.Fatal("repo node not found in result")
	}
	// service (sim=1.0) must outrank repo (sim=0.0) after hybrid blend.
	if serviceRel <= repoRel {
		t.Errorf("service (%f) should outrank repo (%f) after semantic blend", serviceRel, repoRel)
	}
}

// TestCarveEgoGraph_HybridScoring_NoRootEmbeddingFallback verifies that when
// the root node has no stored embedding, scoring falls back to pure structural
// (no blend applied to any node).
func TestCarveEgoGraph_HybridScoring_NoRootEmbeddingFallback(t *testing.T) {
	g, ids := buildCarveFixture(t)

	// Embeddings for all nodes EXCEPT root.
	const dim = 4
	embeds := map[graph.NodeID][]float32{
		ids["service"]: makeUnitVec(dim, 0),
		ids["repo"]:    makeUnitVec(dim, 1),
	}

	cfg := graph.DefaultCarveConfig()
	cfg.EmbeddingLookup = func(nodeIDs []graph.NodeID) map[graph.NodeID][]float32 {
		result := make(map[graph.NodeID][]float32)
		for _, id := range nodeIDs {
			if v, ok := embeds[id]; ok {
				result[id] = v
			}
		}
		return result
	}
	cfg.HybridLambda = 0.5

	hybridSub, err := g.CarveEgoGraph(ids["handler"], cfg)
	if err != nil {
		t.Fatalf("hybrid sub error: %v", err)
	}

	// Pure structural baseline — use a different IntentID to avoid cache collision.
	pureCfg := graph.DefaultCarveConfig()
	pureCfg.IntentID = "pure"
	pureSub, err := g.CarveEgoGraph(ids["handler"], pureCfg)
	if err != nil {
		t.Fatalf("pure sub error: %v", err)
	}

	hybridRel := map[graph.NodeID]float64{}
	for _, cn := range hybridSub.Nodes {
		hybridRel[cn.Node.ID] = cn.Relevance
	}
	pureRel := map[graph.NodeID]float64{}
	for _, cn := range pureSub.Nodes {
		pureRel[cn.Node.ID] = cn.Relevance
	}

	// Non-root scores must equal pure structural (no root embedding → no blend).
	for id, pr := range pureRel {
		if id == ids["handler"] {
			continue
		}
		hr, ok := hybridRel[id]
		if !ok {
			t.Errorf("node %s missing from hybrid result", id)
			continue
		}
		if hr != pr {
			t.Errorf("node %s: hybrid=%f structural=%f — should be equal (no root embedding)", id, hr, pr)
		}
	}
}

// TestCarveEgoGraph_HybridScoring_MismatchedDimFallback verifies that a node
// whose embedding dimension differs from root's is treated as "no embedding"
// (dotProduct returns 0) and retains its structural score.
func TestCarveEgoGraph_HybridScoring_MismatchedDimFallback(t *testing.T) {
	g := graph.New("dimtest")
	root := g.MakeNodeID("f.go", "Root")
	child := g.MakeNodeID("f.go", "Child")
	g.AddNode(&graph.Node{ID: root, Type: graph.NodeFunction, Name: "Root", File: "f.go"})
	g.AddNode(&graph.Node{ID: child, Type: graph.NodeFunction, Name: "Child", File: "f.go"})
	g.AddEdge(&graph.Edge{From: root, To: child, Type: graph.EdgeCalls})

	// root: dim-2, child: dim-3 → dotProduct returns 0 → child score unchanged.
	cfg := graph.DefaultCarveConfig()
	cfg.HybridLambda = 0.5
	cfg.EmbeddingLookup = func(ids []graph.NodeID) map[graph.NodeID][]float32 {
		m := map[graph.NodeID][]float32{}
		for _, id := range ids {
			if id == root {
				m[id] = []float32{1, 0}
			} else {
				m[id] = []float32{1, 0, 0} // mismatched length
			}
		}
		return m
	}

	sub, err := g.CarveEgoGraph(root, cfg)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	for _, cn := range sub.Nodes {
		if cn.Node.ID == child {
			if cn.Relevance <= 0 {
				t.Errorf("child relevance = %f, expected > 0 (structural fallback)", cn.Relevance)
			}
			return
		}
	}
	t.Error("child node not found in result")
}

// TestCarveEgoGraph_HybridScoring_ClampStructuralAboveOne verifies that when a
// struct method's structural score exceeds 1.0 after eigenvector centrality boost
// (seeded at 0.9 × (1 + 0.2 × centrality)), the hybrid blend clamps it to 1.0
// before applying the formula, so the final score stays ≤ 1.0.
//
// Graph: struct root "HubService" + hub method "HubService.Run" + 15 spoke
// callers. After RebuildIndex the hub has centrality = 1.0 (highest connected
// node). Structural = 0.9 × (1 + 0.2 × 1.0) = 1.08 > 1.0.
// With λ=0.5 and sim=1.0: WITHOUT clamp score=1.04; WITH clamp score=1.0.
func TestCarveEgoGraph_HybridScoring_ClampStructuralAboveOne(t *testing.T) {
	g := graph.New("clamptest")
	const pkg = "mypkg"

	rootID := g.MakeNodeID("svc.go", "HubService")
	g.AddNode(&graph.Node{ID: rootID, Name: "HubService", Type: graph.NodeStruct, File: "svc.go", Package: pkg})

	hubID := g.MakeNodeID("svc.go", "HubService.Run")
	g.AddNode(&graph.Node{ID: hubID, Name: "HubService.Run", Type: graph.NodeMethod, File: "svc.go", Package: pkg})

	// 15 spokes all calling the hub → hub gets high eigenvector centrality.
	for i := 0; i < 15; i++ {
		name := fmt.Sprintf("Caller%d", i)
		sid := g.MakeNodeID("callers.go", name)
		g.AddNode(&graph.Node{ID: sid, Name: name, Type: graph.NodeFunction, File: "callers.go", Package: pkg})
		g.AddEdge(&graph.Edge{From: sid, To: hubID, Type: graph.EdgeCalls})
	}

	// Build the CSR index so eigenvector centrality is computed.
	if _, err := g.RebuildIndex(); err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}

	// Hub method and root both get same-axis unit embedding → cosine sim = 1.0.
	const dim = 4
	rootVec := makeUnitVec(dim, 0)
	hubVec := makeUnitVec(dim, 0)

	cfg := graph.DefaultCarveConfig()
	cfg.MinRelevance = 0
	cfg.EmbeddingLookup = func(ids []graph.NodeID) map[graph.NodeID][]float32 {
		m := map[graph.NodeID][]float32{}
		for _, id := range ids {
			switch id {
			case rootID:
				m[id] = rootVec
			case hubID:
				m[id] = hubVec
			}
		}
		return m
	}
	cfg.HybridLambda = 0.5

	sub, err := g.CarveEgoGraph(rootID, cfg)
	if err != nil {
		t.Fatalf("CarveEgoGraph: %v", err)
	}

	var hubScore float64
	for _, cn := range sub.Nodes {
		if cn.Node.ID == hubID {
			hubScore = cn.Relevance
			break
		}
	}
	if hubScore == 0 {
		t.Fatal("hub method not found in result")
	}
	// With clamp: score = (1-0.5)×1.0 + 0.5×1.0 = 1.0.
	// Without clamp: score = (1-0.5)×1.08 + 0.5×1.0 = 1.04 > 1.0.
	if hubScore > 1.0 {
		t.Errorf("hub score = %f > 1.0 — structural was not clamped before blend", hubScore)
	}
}

// TestCarveEgoGraph_InterfaceImplementorExpansion verifies that when the root
// is a NodeInterface, concrete implementors (via reverse IMPLEMENTS edges) and
// their receiver methods are seeded at relevance 0.85 in both BFS and PPR paths.
//
// Graph layout:
//
//	Reader (interface)
//	  ← IMPLEMENTS ─ FileReader  (struct + Read method)
//	  ← IMPLEMENTS ─ NetReader   (struct + Read method)
//	  (Unrelated struct MemStore — NOT an implementor, must not be seeded)
func TestCarveEgoGraph_InterfaceImplementorExpansion(t *testing.T) {
	g := graph.New("testrepo")

	// Interface
	ifaceID := g.MakeNodeID("io.go", "Reader")
	g.AddNode(&graph.Node{ID: ifaceID, Type: graph.NodeInterface, Name: "Reader", File: "io.go"})

	// FileReader struct + method
	fileReaderID := g.MakeNodeID("file.go", "FileReader")
	fileReaderReadID := g.MakeNodeID("file.go", "FileReader.Read")
	g.AddNode(&graph.Node{ID: fileReaderID, Type: graph.NodeStruct, Name: "FileReader", File: "file.go"})
	g.AddNode(&graph.Node{ID: fileReaderReadID, Type: graph.NodeMethod, Name: "FileReader.Read", File: "file.go"})
	// Struct→method DEFINES edge (as Go parser emits)
	g.AddEdge(&graph.Edge{From: fileReaderID, To: fileReaderReadID, Type: graph.EdgeDefines})
	// Concrete→interface IMPLEMENTS edge
	g.AddEdge(&graph.Edge{From: fileReaderID, To: ifaceID, Type: graph.EdgeImplements})

	// NetReader struct + method
	netReaderID := g.MakeNodeID("net.go", "NetReader")
	netReaderReadID := g.MakeNodeID("net.go", "NetReader.Read")
	g.AddNode(&graph.Node{ID: netReaderID, Type: graph.NodeStruct, Name: "NetReader", File: "net.go"})
	g.AddNode(&graph.Node{ID: netReaderReadID, Type: graph.NodeMethod, Name: "NetReader.Read", File: "net.go"})
	g.AddEdge(&graph.Edge{From: netReaderID, To: netReaderReadID, Type: graph.EdgeDefines})
	g.AddEdge(&graph.Edge{From: netReaderID, To: ifaceID, Type: graph.EdgeImplements})

	// Unrelated struct — NOT an implementor
	unrelatedID := g.MakeNodeID("store.go", "MemStore")
	g.AddNode(&graph.Node{ID: unrelatedID, Type: graph.NodeStruct, Name: "MemStore", File: "store.go"})

	type testCase struct {
		usePPR bool
		name   string
		// BFS seeds implementors at the literal 0.85; PPR normalises the teleport
		// vector across all seeds so each node's rank is lower than 0.85 but still
		// well above MinRelevance.  The thresholds reflect this difference.
		implMinRel   float64 // concrete implementor struct
		methodMinRel float64 // implementor methods
	}
	cases := []testCase{
		{usePPR: false, name: "BFS", implMinRel: 0.80, methodMinRel: 0.80},
		// PPR normalises teleport to sum=1 so with 5 seeds (iface+2 impls+2 methods)
		// at weights 1.0+0.85×4=4.4 the normalised weight per implementor ≈ 0.19.
		// After power iteration convergence, implementors end up at ~0.25, methods
		// at ~0.06 — well above MinRelevance=0.01 and significantly higher than if
		// they were not seeded at all (where they'd only accumulate propagation mass).
		{usePPR: true, name: "PPR", implMinRel: 0.10, methodMinRel: 0.03},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cfg := graph.DefaultCarveConfig()
			cfg.UsePPR = tc.usePPR
			cfg.MinRelevance = 0.01

			sub, err := g.CarveEgoGraph(ifaceID, cfg)
			if err != nil {
				t.Fatalf("CarveEgoGraph: %v", err)
			}

			nodeMap := make(map[graph.NodeID]float64)
			for _, cn := range sub.Nodes {
				nodeMap[cn.Node.ID] = cn.Relevance
			}

			// Concrete implementor structs must appear with meaningful relevance.
			for _, id := range []graph.NodeID{fileReaderID, netReaderID} {
				rel, ok := nodeMap[id]
				if !ok {
					t.Errorf("%s: concrete implementor %s missing from subgraph", tc.name, id)
					continue
				}
				if rel < tc.implMinRel {
					t.Errorf("%s: implementor %s relevance = %.3f, want >= %.3f", tc.name, id, rel, tc.implMinRel)
				}
			}

			// Implementor methods must also appear.
			for _, id := range []graph.NodeID{fileReaderReadID, netReaderReadID} {
				rel, ok := nodeMap[id]
				if !ok {
					t.Errorf("%s: implementor method %s missing from subgraph", tc.name, id)
					continue
				}
				if rel < tc.methodMinRel {
					t.Errorf("%s: implementor method %s relevance = %.3f, want >= %.3f", tc.name, id, rel, tc.methodMinRel)
				}
			}

			// Unrelated struct must not get high relevance (it is isolated).
			if rel, ok := nodeMap[unrelatedID]; ok && rel > 0.5 {
				t.Errorf("%s: unrelated MemStore has unexpectedly high relevance %.3f", tc.name, rel)
			}
		})
	}
}

// TestCarveEgoGraph_InterfaceImplementorExpansion_SameReceiverName verifies that
// when the interface and a concrete struct share the same name (e.g. "Store"
// interface and "Store" struct in different packages), the method seeding does
// not downgrade interface-method scores (0.9) to implementor-method scores (0.85).
//
// This covers the max-score guard in the BFS implementor seeding path.
func TestCarveEgoGraph_InterfaceImplementorExpansion_SameReceiverName(t *testing.T) {
	g := graph.New("testrepo")

	// Interface "Store" with method "Store.Get" (in iface.go)
	ifaceID := g.MakeNodeID("iface.go", "Store")
	ifaceMethID := g.MakeNodeID("iface.go", "Store.Get")
	g.AddNode(&graph.Node{ID: ifaceID, Type: graph.NodeInterface, Name: "Store", File: "iface.go"})
	g.AddNode(&graph.Node{ID: ifaceMethID, Type: graph.NodeMethod, Name: "Store.Get", File: "iface.go"})
	g.AddEdge(&graph.Edge{From: ifaceID, To: ifaceMethID, Type: graph.EdgeDefines})

	// Concrete struct also named "Store" (in impl.go) — same receiver name, different file/ID
	implID := g.MakeNodeID("impl.go", "Store")
	implMethID := g.MakeNodeID("impl.go", "Store.Get")
	g.AddNode(&graph.Node{ID: implID, Type: graph.NodeStruct, Name: "Store", File: "impl.go"})
	g.AddNode(&graph.Node{ID: implMethID, Type: graph.NodeMethod, Name: "Store.Get", File: "impl.go"})
	g.AddEdge(&graph.Edge{From: implID, To: implMethID, Type: graph.EdgeDefines})
	g.AddEdge(&graph.Edge{From: implID, To: ifaceID, Type: graph.EdgeImplements})

	cfg := graph.DefaultCarveConfig()
	cfg.UsePPR = false
	cfg.MinRelevance = 0.01

	sub, err := g.CarveEgoGraph(ifaceID, cfg)
	if err != nil {
		t.Fatalf("CarveEgoGraph: %v", err)
	}

	nodeMap := make(map[graph.NodeID]float64)
	for _, cn := range sub.Nodes {
		nodeMap[cn.Node.ID] = cn.Relevance
	}

	// The interface method seeded at 0.9 must not be downgraded to 0.85.
	// iface.go::Store.Get is the interface method (seeded at 0.9 by receiver-method seeding).
	// impl.go::Store.Get is the implementor method (would be seeded at 0.85 by implementor seeding).
	// Since both have receiver name "Store", the implementor seeding runs second;
	// the max-score guard must preserve the higher 0.9 for iface.go::Store.Get.
	ifaceMethRel, ok := nodeMap[ifaceMethID]
	if !ok {
		t.Fatal("interface method Store.Get (iface.go) missing from subgraph")
	}
	if ifaceMethRel < 0.89 {
		t.Errorf("interface method Store.Get (iface.go) relevance = %.3f, want >= 0.89 (must not be downgraded from 0.9 to 0.85)", ifaceMethRel)
	}

	// Implementor method must still appear at 0.85.
	implMethRel, ok := nodeMap[implMethID]
	if !ok {
		t.Fatal("implementor method Store.Get (impl.go) missing from subgraph")
	}
	if implMethRel < 0.80 {
		t.Errorf("implementor method Store.Get (impl.go) relevance = %.3f, want >= 0.80", implMethRel)
	}

	// Implementor struct must appear.
	if _, ok := nodeMap[implID]; !ok {
		t.Error("implementor struct Store (impl.go) missing from subgraph")
	}
}

// TestCarveEgoGraph_InterfaceImplementorExpansion_SelfLoop verifies that a
// self-loop IMPLEMENTS edge (e.From == e.To == rootID, a parser-bug scenario)
// does not corrupt the root node's relevance score.
func TestCarveEgoGraph_InterfaceImplementorExpansion_SelfLoop(t *testing.T) {
	g := graph.New("testrepo")

	ifaceID := g.MakeNodeID("iface.go", "Iface")
	g.AddNode(&graph.Node{ID: ifaceID, Type: graph.NodeInterface, Name: "Iface", File: "iface.go"})
	// Simulate a parser-bug self-loop: interface IMPLEMENTS itself.
	g.AddEdge(&graph.Edge{From: ifaceID, To: ifaceID, Type: graph.EdgeImplements})

	cfg := graph.DefaultCarveConfig()
	cfg.UsePPR = false

	sub, err := g.CarveEgoGraph(ifaceID, cfg)
	if err != nil {
		t.Fatalf("CarveEgoGraph: %v", err)
	}

	for _, cn := range sub.Nodes {
		if cn.Node.ID == ifaceID {
			if cn.Relevance != 1.0 {
				t.Errorf("root Iface relevance = %.3f after self-loop, want exactly 1.0", cn.Relevance)
			}
			return
		}
	}
	t.Error("root Iface node missing from subgraph")
}

// ── Sprint 15 #2: Quality score boost tests ───────────────────────────────────

// TestQualityScoreLookup_PositiveBoostsRanking verifies that a node with a
// positive quality score ranks higher than a structurally identical node with
// no quality record.
func TestQualityScoreLookup_PositiveBoostsRanking(t *testing.T) {
	g := graph.New("testrepo")
	root := g.MakeNodeID("main.go", "Root")
	good := g.MakeNodeID("good.go", "GoodHelper")
	neutral := g.MakeNodeID("neutral.go", "NeutralHelper")

	g.AddNode(&graph.Node{ID: root, Type: graph.NodeFunction, Name: "Root", File: "main.go"})
	g.AddNode(&graph.Node{ID: good, Type: graph.NodeFunction, Name: "GoodHelper", File: "good.go"})
	g.AddNode(&graph.Node{ID: neutral, Type: graph.NodeFunction, Name: "NeutralHelper", File: "neutral.go"})
	// Both helpers are at equal structural distance from root.
	g.AddEdge(&graph.Edge{From: root, To: good, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: root, To: neutral, Type: graph.EdgeCalls})

	cfg := graph.DefaultCarveConfig()
	cfg.UsePPR = false
	cfg.QualityScoreLookup = func(nodes []graph.QualityNode) map[graph.NodeID]float64 {
		return map[graph.NodeID]float64{
			good: 5.0, // strong positive — context was consistently helpful
			// neutral has no entry → no change
		}
	}

	sub, err := g.CarveEgoGraph(root, cfg)
	if err != nil {
		t.Fatalf("CarveEgoGraph error: %v", err)
	}

	var goodRel, neutralRel float64
	for _, cn := range sub.Nodes {
		switch cn.Node.ID {
		case good:
			goodRel = cn.Relevance
		case neutral:
			neutralRel = cn.Relevance
		}
	}
	if goodRel <= neutralRel {
		t.Errorf("good helper (score +5) relevance %.4f should be > neutral helper %.4f", goodRel, neutralRel)
	}
}

// TestQualityScoreLookup_NegativePenalisesRanking verifies that a node with a
// negative quality score ranks lower than a structurally identical node.
func TestQualityScoreLookup_NegativePenalisesRanking(t *testing.T) {
	g := graph.New("testrepo")
	root := g.MakeNodeID("main.go", "Root")
	bad := g.MakeNodeID("bad.go", "BadHelper")
	neutral := g.MakeNodeID("neutral.go", "NeutralHelper")

	g.AddNode(&graph.Node{ID: root, Type: graph.NodeFunction, Name: "Root", File: "main.go"})
	g.AddNode(&graph.Node{ID: bad, Type: graph.NodeFunction, Name: "BadHelper", File: "bad.go"})
	g.AddNode(&graph.Node{ID: neutral, Type: graph.NodeFunction, Name: "NeutralHelper", File: "neutral.go"})
	g.AddEdge(&graph.Edge{From: root, To: bad, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: root, To: neutral, Type: graph.EdgeCalls})

	cfg := graph.DefaultCarveConfig()
	cfg.UsePPR = false
	cfg.QualityScoreLookup = func(nodes []graph.QualityNode) map[graph.NodeID]float64 {
		return map[graph.NodeID]float64{
			bad: -5.0, // strong negative — context repeatedly caused corrections
		}
	}

	sub, err := g.CarveEgoGraph(root, cfg)
	if err != nil {
		t.Fatalf("CarveEgoGraph error: %v", err)
	}

	var badRel, neutralRel float64
	for _, cn := range sub.Nodes {
		switch cn.Node.ID {
		case bad:
			badRel = cn.Relevance
		case neutral:
			neutralRel = cn.Relevance
		}
	}
	if badRel >= neutralRel {
		t.Errorf("bad helper (score -5) relevance %.4f should be < neutral helper %.4f", badRel, neutralRel)
	}
}

// TestQualityScoreLookup_RootNeverPenalised verifies that the root node's
// relevance is always pinned at 1.0 regardless of quality score.
func TestQualityScoreLookup_RootNeverPenalised(t *testing.T) {
	g := graph.New("testrepo")
	root := g.MakeNodeID("main.go", "Root")
	child := g.MakeNodeID("child.go", "Child")

	g.AddNode(&graph.Node{ID: root, Type: graph.NodeFunction, Name: "Root", File: "main.go"})
	g.AddNode(&graph.Node{ID: child, Type: graph.NodeFunction, Name: "Child", File: "child.go"})
	g.AddEdge(&graph.Edge{From: root, To: child, Type: graph.EdgeCalls})

	cfg := graph.DefaultCarveConfig()
	cfg.UsePPR = false
	// Provide a very negative quality score for the root itself — must be ignored.
	cfg.QualityScoreLookup = func(nodes []graph.QualityNode) map[graph.NodeID]float64 {
		return map[graph.NodeID]float64{
			root: -100.0,
		}
	}

	sub, err := g.CarveEgoGraph(root, cfg)
	if err != nil {
		t.Fatalf("CarveEgoGraph error: %v", err)
	}

	for _, cn := range sub.Nodes {
		if cn.Node.ID == root {
			if cn.Relevance != 1.0 {
				t.Errorf("root relevance = %.4f after quality penalty; want exactly 1.0", cn.Relevance)
			}
			return
		}
	}
	t.Error("root node missing from subgraph")
}

// TestQualityScoreLookup_NilLookupIsNoop verifies that a nil QualityScoreLookup
// produces the same output as when no lookup is provided (backward-compatible default).
func TestQualityScoreLookup_NilLookupIsNoop(t *testing.T) {
	g, ids := buildCarveFixture(t)

	cfgNil := graph.DefaultCarveConfig()
	cfgNil.UsePPR = false
	cfgNil.QualityScoreLookup = nil

	cfgNoField := graph.DefaultCarveConfig()
	cfgNoField.UsePPR = false
	// QualityScoreLookup is already nil by default — just confirm matching output.

	subNil, err := g.CarveEgoGraph(ids["service"], cfgNil)
	if err != nil {
		t.Fatalf("nil lookup error: %v", err)
	}
	subNoField, err := g.CarveEgoGraph(ids["service"], cfgNoField)
	if err != nil {
		t.Fatalf("no-field lookup error: %v", err)
	}
	if len(subNil.Nodes) != len(subNoField.Nodes) {
		t.Errorf("node counts differ: nil=%d no-field=%d", len(subNil.Nodes), len(subNoField.Nodes))
	}
}

// TestQualityScoreLookup_EmptyResultIsNoop verifies that when the lookup returns
// an empty map (no quality data yet), relevance scores are unchanged.
func TestQualityScoreLookup_EmptyResultIsNoop(t *testing.T) {
	g, ids := buildCarveFixture(t)

	cfgBase := graph.DefaultCarveConfig()
	cfgBase.UsePPR = false

	cfgQuality := graph.DefaultCarveConfig()
	cfgQuality.UsePPR = false
	cfgQuality.QualityScoreLookup = func(nodes []graph.QualityNode) map[graph.NodeID]float64 {
		return nil // no quality data — new project
	}

	subBase, err := g.CarveEgoGraph(ids["handler"], cfgBase)
	if err != nil {
		t.Fatalf("base error: %v", err)
	}
	subQuality, err := g.CarveEgoGraph(ids["handler"], cfgQuality)
	if err != nil {
		t.Fatalf("quality error: %v", err)
	}

	baseMap := make(map[graph.NodeID]float64)
	for _, cn := range subBase.Nodes {
		baseMap[cn.Node.ID] = cn.Relevance
	}
	for _, cn := range subQuality.Nodes {
		base, ok := baseMap[cn.Node.ID]
		if !ok {
			t.Errorf("node %s in quality result but not in base", cn.Node.ID)
			continue
		}
		if math.Abs(cn.Relevance-base) > 1e-9 {
			t.Errorf("node %s: base relevance %.6f != quality relevance %.6f with empty map", cn.Node.ID, base, cn.Relevance)
		}
	}
}

// TestFlatGraph_PPR_ParityWithSlowPath verifies that enabling the FlatGraph BFS
// fast path produces the same ranked node set as the pointer-based slow path.
// This is the key correctness invariant: FlatGraph is a performance optimisation
// and must not change which nodes appear in the PPR subgraph.
func TestFlatGraph_PPR_ParityWithSlowPath(t *testing.T) {
	// Build a moderately-connected graph so PPR BFS exercises multi-hop expansion.
	g := graph.New("parity-repo")

	// Six nodes across three files — enough edges that PPR ranking is non-trivial.
	nA := g.MakeNodeID("a.go", "Alpha")
	nB := g.MakeNodeID("b.go", "Beta")
	nC := g.MakeNodeID("c.go", "Gamma")
	nD := g.MakeNodeID("a.go", "Delta")
	nE := g.MakeNodeID("b.go", "Epsilon")
	nF := g.MakeNodeID("c.go", "Zeta")

	for _, n := range []struct {
		id   graph.NodeID
		name string
		file string
	}{
		{nA, "Alpha", "a.go"}, {nB, "Beta", "b.go"}, {nC, "Gamma", "c.go"},
		{nD, "Delta", "a.go"}, {nE, "Epsilon", "b.go"}, {nF, "Zeta", "c.go"},
	} {
		g.AddNode(&graph.Node{ID: n.id, Type: graph.NodeFunction, Name: n.name, File: n.file})
	}

	g.AddEdge(&graph.Edge{From: nA, To: nB, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: nB, To: nC, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: nC, To: nD, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: nD, To: nE, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: nE, To: nF, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: nF, To: nA, Type: graph.EdgeCalls}) // cycle

	cfg := graph.DefaultCarveConfig()
	cfg.UsePPR = true
	cfg.MaxDepth = 6
	cfg.TokenBudget = 100_000

	// Slow path (no FlatGraph).
	slowSub, err := g.CarveEgoGraph(nA, cfg)
	if err != nil {
		t.Fatalf("slow path: %v", err)
	}

	// Enable FlatGraph fast path and re-run from fresh cache.
	g.EnableFlatGraph()
	g.InvalidateCache()

	fastSub, err := g.CarveEgoGraph(nA, cfg)
	if err != nil {
		t.Fatalf("fast path: %v", err)
	}

	// Both runs must return the same set of node IDs.
	slowSet := make(map[graph.NodeID]struct{}, len(slowSub.Nodes))
	for _, cn := range slowSub.Nodes {
		slowSet[cn.Node.ID] = struct{}{}
	}
	fastSet := make(map[graph.NodeID]struct{}, len(fastSub.Nodes))
	for _, cn := range fastSub.Nodes {
		fastSet[cn.Node.ID] = struct{}{}
	}

	for id := range slowSet {
		if _, ok := fastSet[id]; !ok {
			t.Errorf("FlatGraph fast path missing node %s that slow path found", id)
		}
	}
	for id := range fastSet {
		if _, ok := slowSet[id]; !ok {
			t.Errorf("FlatGraph fast path has extra node %s not in slow path", id)
		}
	}

	if t.Failed() {
		t.Logf("slow set (%d): %v", len(slowSet), slowSet)
		t.Logf("fast set (%d): %v", len(fastSet), fastSet)
	}
}

// TestCarveEgoGraph_DocEdgesReachable verifies that documentation sections
// linked via DOCUMENTS edges are reachable from the code entity.
func TestCarveEgoGraph_DocEdgesReachable(t *testing.T) {
	g := graph.New("testrepo")

	codeID := g.MakeNodeID("main.go", "BuildGraph")
	g.AddNode(&graph.Node{
		ID: codeID, Type: graph.NodeFunction, Name: "BuildGraph", File: "main.go",
		Domain: graph.DomainCode,
	})

	// Doc section linked via name-match.
	secNameMatch := g.MakeNodeID("README.md", "README.md § API")
	g.AddNode(&graph.Node{
		ID:   secNameMatch,
		Type: graph.NodeSection,
		Name: "README.md § API",
		File: "README.md",
		Metadata: map[string]string{
			"title":           "API",
			"body":            "The `BuildGraph` function.",
			"doc_link_source": "name_match",
		},
		Domain: graph.DomainDocs,
	})
	g.AddEdge(&graph.Edge{From: secNameMatch, To: codeID, Type: graph.EdgeDocuments})

	// Second doc section.
	secOther := g.MakeNodeID("docs.md", "docs.md § Overview")
	g.AddNode(&graph.Node{
		ID:   secOther,
		Type: graph.NodeSection,
		Name: "docs.md § Overview",
		File: "docs.md",
		Metadata: map[string]string{
			"title": "Overview",
			"body":  "General overview.",
		},
		Domain: graph.DomainDocs,
	})
	g.AddEdge(&graph.Edge{From: secOther, To: codeID, Type: graph.EdgeDocuments})

	cfg := graph.DefaultCarveConfig()
	cfg.MaxDepth = 2
	sub, err := g.CarveEgoGraph(codeID, cfg)
	if err != nil {
		t.Fatalf("CarveEgoGraph error: %v", err)
	}

	var nameMatchScore, otherScore float64
	for _, cn := range sub.Nodes {
		if cn.Node.ID == secNameMatch {
			nameMatchScore = cn.Relevance
		}
		if cn.Node.ID == secOther {
			otherScore = cn.Relevance
		}
	}

	if nameMatchScore <= 0 {
		t.Error("name-match section should be reachable")
	}
	if otherScore <= 0 {
		t.Error("second doc section should be reachable")
	}
}

// TestCarveEgoGraph_ImplementorsInOutput verifies that querying an interface
// returns its implementing structs in the SubGraph output.
func TestCarveEgoGraph_ImplementorsInOutput(t *testing.T) {
	g := graph.New("test")

	ifaceID := g.MakeNodeID("svc.py", "View")
	impl1ID := g.MakeNodeID("svc.py", "MethodView")
	impl2ID := g.MakeNodeID("svc.py", "ListView")

	// Python classes are NodeStruct (not NodeInterface), so test with struct root
	// to match real Python behavior where base classes are also structs.
	g.AddNode(&graph.Node{ID: ifaceID, Type: graph.NodeStruct, Name: "View", File: "svc.py", Package: "views"})
	g.AddNode(&graph.Node{ID: impl1ID, Type: graph.NodeStruct, Name: "MethodView", File: "svc.py", Package: "views"})
	g.AddNode(&graph.Node{ID: impl2ID, Type: graph.NodeStruct, Name: "ListView", File: "svc.py", Package: "views"})

	g.AddEdge(&graph.Edge{From: impl1ID, To: ifaceID, Type: graph.EdgeImplements})
	g.AddEdge(&graph.Edge{From: impl2ID, To: ifaceID, Type: graph.EdgeImplements})

	sg, err := g.CarveEgoGraph(ifaceID, graph.CarveConfig{
		MaxDepth:     3,
		TokenBudget:  2000,
		MinRelevance: 0.01,
	})
	if err != nil {
		t.Fatal(err)
	}

	found := map[string]bool{}
	for _, cn := range sg.Nodes {
		found[string(cn.Node.ID)] = true
	}
	if !found[string(impl1ID)] {
		t.Errorf("MethodView not in carve output")
	}
	if !found[string(impl2ID)] {
		t.Errorf("ListView not in carve output")
	}
}
