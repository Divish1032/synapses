package mcp

import (
	"context"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── edgeActivationWeight ──────────────────────────────────────────────────────

func TestEdgeActivationWeight_CALLS(t *testing.T) {
	if w := edgeActivationWeight(graph.EdgeCalls); w != 1.0 {
		t.Errorf("CALLS weight = %v, want 1.0", w)
	}
}

func TestEdgeActivationWeight_IMPLEMENTS(t *testing.T) {
	if w := edgeActivationWeight(graph.EdgeImplements); w != 0.7 {
		t.Errorf("IMPLEMENTS weight = %v, want 0.7", w)
	}
}

func TestEdgeActivationWeight_IMPORTS(t *testing.T) {
	if w := edgeActivationWeight(graph.EdgeImports); w != 0.5 {
		t.Errorf("IMPORTS weight = %v, want 0.5", w)
	}
}

func TestEdgeActivationWeight_Default(t *testing.T) {
	if w := edgeActivationWeight(graph.EdgeDefines); w != 0.4 {
		t.Errorf("default weight = %v, want 0.4", w)
	}
}

// ── graphBFS: ActivationMap correctness ──────────────────────────────────────

// TestGraphBFS_ActivationMap_SeedIs1 verifies that seeds have activation 1.0
// in the ActivationMap and that direct neighbors receive values > 0 and
// proportional to edgeActivationWeight / fan_out.
func TestGraphBFS_ActivationMap_SeedIs1(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	aID := srv.graph.MakeNodeID("a.go", "FuncA")
	bID := srv.graph.MakeNodeID("b.go", "FuncB")
	cID := srv.graph.MakeNodeID("c.go", "FuncC") // second neighbor to force fan_out = 2
	for _, n := range []*graph.Node{
		{ID: aID, Name: "FuncA", Type: graph.NodeFunction, File: "a.go", Line: 1},
		{ID: bID, Name: "FuncB", Type: graph.NodeFunction, File: "b.go", Line: 1},
		{ID: cID, Name: "FuncC", Type: graph.NodeFunction, File: "c.go", Line: 1},
	} {
		srv.graph.AddNode(n)
	}
	// Two CALLS edges from A: fan_out = 2, each child gets 1.0 * 1.0 / 2 = 0.5.
	srv.graph.AddEdge(&graph.Edge{From: aID, To: bID, Type: graph.EdgeCalls})
	srv.graph.AddEdge(&graph.Edge{From: aID, To: cID, Type: graph.EdgeCalls})

	result := srv.graphBFS([]string{string(aID)}, 1)

	// Seed must be in ActivationMap at 1.0.
	if result.ActivationMap[string(aID)] != 1.0 {
		t.Errorf("seed activation = %v, want 1.0", result.ActivationMap[string(aID)])
	}
	// FuncB reachable at depth 1 — with fan_out=2 and weight=1.0, activation = 0.5.
	bAct := result.ActivationMap[string(bID)]
	if bAct <= 0 || bAct >= 1.0 {
		t.Errorf("FuncB activation = %v, want 0 < act < 1.0", bAct)
	}
}

// TestGraphBFS_ActivationMap_CALLSHigherThanIMPORTS verifies that CALLS edges
// propagate more activation than IMPORTS edges from the same seed.
func TestGraphBFS_ActivationMap_CALLSHigherThanIMPORTS(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	seedID := srv.graph.MakeNodeID("main.go", "Main")
	calleeID := srv.graph.MakeNodeID("a.go", "CalledFunc")
	importedID := srv.graph.MakeNodeID("b.go", "ImportedPkg")
	for _, n := range []*graph.Node{
		{ID: seedID, Name: "Main", Type: graph.NodeFunction, File: "main.go", Line: 1},
		{ID: calleeID, Name: "CalledFunc", Type: graph.NodeFunction, File: "a.go", Line: 1},
		{ID: importedID, Name: "ImportedPkg", Type: graph.NodeFunction, File: "b.go", Line: 1},
	} {
		srv.graph.AddNode(n)
	}
	// seed → callee via CALLS, seed → imported via IMPORTS.
	srv.graph.AddEdge(&graph.Edge{From: seedID, To: calleeID, Type: graph.EdgeCalls})
	srv.graph.AddEdge(&graph.Edge{From: seedID, To: importedID, Type: graph.EdgeImports})

	result := srv.graphBFS([]string{string(seedID)}, 1)

	calleeAct := result.ActivationMap[string(calleeID)]
	importedAct := result.ActivationMap[string(importedID)]

	if calleeAct <= 0 {
		t.Fatalf("callee activation = %v, want > 0", calleeAct)
	}
	if importedAct <= 0 {
		t.Fatalf("imported activation = %v, want > 0", importedAct)
	}
	if calleeAct <= importedAct {
		t.Errorf("CALLS activation (%v) should exceed IMPORTS activation (%v)", calleeAct, importedAct)
	}
}

// TestGraphBFS_ActivationMap_FanOutDilutes verifies that higher fan-out reduces
// per-neighbor activation (hub nodes don't flood their neighbors).
func TestGraphBFS_ActivationMap_FanOutDilutes(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	// Hub with two CALLS edges: fan_out = 2. Each child gets act = 1.0*1.0/2 = 0.5.
	hubID := srv.graph.MakeNodeID("hub.go", "Hub")
	childA := srv.graph.MakeNodeID("a.go", "ChildA")
	childB := srv.graph.MakeNodeID("b.go", "ChildB")
	for _, n := range []*graph.Node{
		{ID: hubID, Name: "Hub", Type: graph.NodeFunction, File: "hub.go", Line: 1},
		{ID: childA, Name: "ChildA", Type: graph.NodeFunction, File: "a.go", Line: 1},
		{ID: childB, Name: "ChildB", Type: graph.NodeFunction, File: "b.go", Line: 1},
	} {
		srv.graph.AddNode(n)
	}
	srv.graph.AddEdge(&graph.Edge{From: hubID, To: childA, Type: graph.EdgeCalls})
	srv.graph.AddEdge(&graph.Edge{From: hubID, To: childB, Type: graph.EdgeCalls})

	result := srv.graphBFS([]string{string(hubID)}, 1)

	actA := result.ActivationMap[string(childA)]
	actB := result.ActivationMap[string(childB)]

	// Both children should get activation = 1.0 * 1.0 / 2 = 0.5.
	const wantAct = 0.5
	if actA != wantAct {
		t.Errorf("ChildA activation = %v, want %v", actA, wantAct)
	}
	if actB != wantAct {
		t.Errorf("ChildB activation = %v, want %v", actB, wantAct)
	}

	// Single-child hub (fan_out=1): child gets full CALLS activation = 1.0.
	singleHub := srv.graph.MakeNodeID("single.go", "SingleHub")
	singleChild := srv.graph.MakeNodeID("sc.go", "SingleChild")
	for _, n := range []*graph.Node{
		{ID: singleHub, Name: "SingleHub", Type: graph.NodeFunction, File: "single.go", Line: 1},
		{ID: singleChild, Name: "SingleChild", Type: graph.NodeFunction, File: "sc.go", Line: 1},
	} {
		srv.graph.AddNode(n)
	}
	srv.graph.AddEdge(&graph.Edge{From: singleHub, To: singleChild, Type: graph.EdgeCalls})

	result2 := srv.graphBFS([]string{string(singleHub)}, 1)
	if result2.ActivationMap[string(singleChild)] != 1.0 {
		t.Errorf("single-child activation = %v, want 1.0", result2.ActivationMap[string(singleChild)])
	}
}

// TestGraphBFS_ActivationMap_TwoHopDecay verifies that activation decreases
// over multiple hops (depth-2 node has lower activation than depth-1 node).
func TestGraphBFS_ActivationMap_TwoHopDecay(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	// Linear chain: A → B → C (all CALLS, each with only one outgoing CALLS edge).
	aID := srv.graph.MakeNodeID("a.go", "A")
	bID := srv.graph.MakeNodeID("b.go", "B")
	cID := srv.graph.MakeNodeID("c.go", "C")
	for _, n := range []*graph.Node{
		{ID: aID, Name: "A", Type: graph.NodeFunction, File: "a.go", Line: 1},
		{ID: bID, Name: "B", Type: graph.NodeFunction, File: "b.go", Line: 1},
		{ID: cID, Name: "C", Type: graph.NodeFunction, File: "c.go", Line: 1},
	} {
		srv.graph.AddNode(n)
	}
	srv.graph.AddEdge(&graph.Edge{From: aID, To: bID, Type: graph.EdgeCalls})
	srv.graph.AddEdge(&graph.Edge{From: bID, To: cID, Type: graph.EdgeCalls})

	result := srv.graphBFS([]string{string(aID)}, 2)

	actB := result.ActivationMap[string(bID)]
	actC := result.ActivationMap[string(cID)]

	if actB <= 0 {
		t.Fatalf("B activation = %v, want > 0", actB)
	}
	if actC <= 0 {
		t.Fatalf("C activation = %v, want > 0", actC)
	}
	if actC >= actB {
		t.Errorf("depth-2 node C activation (%v) should be less than depth-1 node B activation (%v)", actC, actB)
	}
}

// TestGraphBFS_ActivationMap_MultiPathTakesMax verifies that a node reachable
// via two paths at different depths receives the higher activation value.
//
// Graph:
//
//	seed → target (CALLS, depth 1)  — high activation: 1.0 × 1.0 / 2 = 0.5
//	seed → low    (IMPORTS, depth 1) — low activation:  1.0 × 0.5 / 2 = 0.25
//	low  → target (CALLS, depth 2)  — low activation to target: 0.25 × 1.0 / 1 = 0.25
//	                                   (fan_out of low = 1: seed→low is IMPORTS,
//	                                    not in depth2Types, so not counted)
//
// Max(0.5, 0.25) = 0.5 — the direct CALLS path wins.
func TestGraphBFS_ActivationMap_MultiPathTakesMax(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	seedID := srv.graph.MakeNodeID("seed.go", "Seed")
	lowID := srv.graph.MakeNodeID("low.go", "Low")
	targetID := srv.graph.MakeNodeID("target.go", "Target")
	for _, n := range []*graph.Node{
		{ID: seedID, Name: "Seed", Type: graph.NodeFunction, File: "seed.go", Line: 1},
		{ID: lowID, Name: "Low", Type: graph.NodeFunction, File: "low.go", Line: 1},
		{ID: targetID, Name: "Target", Type: graph.NodeFunction, File: "target.go", Line: 1},
	} {
		srv.graph.AddNode(n)
	}
	// seed → target at depth 1 via CALLS (high-activation path).
	srv.graph.AddEdge(&graph.Edge{From: seedID, To: targetID, Type: graph.EdgeCalls})
	// seed → low via IMPORTS (low-activation path), then low → target via CALLS at depth 2.
	srv.graph.AddEdge(&graph.Edge{From: seedID, To: lowID, Type: graph.EdgeImports})
	srv.graph.AddEdge(&graph.Edge{From: lowID, To: targetID, Type: graph.EdgeCalls})

	result := srv.graphBFS([]string{string(seedID)}, 2)

	targetAct := result.ActivationMap[string(targetID)]
	lowAct := result.ActivationMap[string(lowID)]

	if targetAct <= 0 {
		t.Fatalf("target activation = %v, want > 0", targetAct)
	}

	// Direct CALLS path activation: 1.0 × 1.0 / fan_out(seed).
	// seed has two outgoing allowed edges at depth 1 (CALLS + IMPORTS) → fan_out = 2.
	const seedFanOut = 2.0
	highPathAct := 1.0 * edgeActivationWeight(graph.EdgeCalls) / seedFanOut // 0.5

	// Low path at depth 2: low fan_out = 1 (only CALLS to target; seed→low IMPORTS
	// is NOT in depth2Types so not counted in fan_out).
	lowPathAct := lowAct * edgeActivationWeight(graph.EdgeCalls) / 1.0

	want := highPathAct
	if lowPathAct > want {
		want = lowPathAct
	}

	const eps = 1e-9
	if targetAct < want-eps || targetAct > want+eps {
		t.Errorf("target activation = %v, want max(high=%v, low=%v) = %v",
			targetAct, highPathAct, lowPathAct, want)
	}
	// Sanity: direct CALLS path must win over the indirect IMPORTS→CALLS path.
	if highPathAct <= lowPathAct {
		t.Errorf("high path activation (%v) should exceed low path activation (%v)", highPathAct, lowPathAct)
	}
}

// TestGraphBFS_ActivationMap_EmptySeeds ensures empty seeds return empty map.
func TestGraphBFS_ActivationMap_EmptySeeds(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	result := srv.graphBFS(nil, 2)
	if len(result.ActivationMap) != 0 {
		t.Errorf("empty seeds should yield empty activation map, got %d entries", len(result.ActivationMap))
	}
}

// TestGraphBFS_ActivationMap_InvalidSeed ensures unknown seed IDs yield no activation.
func TestGraphBFS_ActivationMap_InvalidSeed(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	result := srv.graphBFS([]string{"nonexistent::node"}, 2)
	if len(result.ActivationMap) != 0 {
		t.Errorf("invalid seed should yield empty activation map, got %d entries", len(result.ActivationMap))
	}
}

// ── Integration: graph channel sorts memories by activation ──────────────────

// TestQuadRecallSearch_GraphChannel_SortsByActivation verifies that the graph
// channel returns memories sorted by their anchor node's activation score so
// that RRF assigns better ranks to high-activation-anchored memories.
//
// Setup:
//   - queryMem: a memory with FTS-matching content, anchored to seedNode.
//   - highMem:  a memory anchored to highNode (CALLS from seed, activation = 1.0).
//   - lowMem:   a memory anchored to lowNode (IMPORTS from seed, activation < 1.0 × highAct).
//
// Because the seed node has two outgoing edges (fan_out=2):
//   - highNode activation = 1.0 * 1.0 / 2 = 0.5
//   - lowNode  activation = 1.0 * 0.5 / 2 = 0.25
//
// The graph channel must return highMem before lowMem.
func TestQuadRecallSearch_GraphChannel_SortsByActivation(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	// Build graph: seedNode → highNode (CALLS), seedNode → lowNode (IMPORTS).
	seedNode := srv.graph.MakeNodeID("seed.go", "SeedFunc")
	highNode := srv.graph.MakeNodeID("high.go", "HighFunc")
	lowNode := srv.graph.MakeNodeID("low.go", "LowFunc")
	for _, n := range []*graph.Node{
		{ID: seedNode, Name: "SeedFunc", Type: graph.NodeFunction, File: "seed.go", Line: 1},
		{ID: highNode, Name: "HighFunc", Type: graph.NodeFunction, File: "high.go", Line: 1},
		{ID: lowNode, Name: "LowFunc", Type: graph.NodeFunction, File: "low.go", Line: 1},
	} {
		srv.graph.AddNode(n)
	}
	srv.graph.AddEdge(&graph.Edge{From: seedNode, To: highNode, Type: graph.EdgeCalls})
	srv.graph.AddEdge(&graph.Edge{From: seedNode, To: lowNode, Type: graph.EdgeImports})

	// Insert memories:
	// queryMem anchored to seedNode — its FTS match makes seedNode a seed.
	queryMemID, err := srv.store.InsertMemoryWithAnchors(store.Memory{
		Tier:    store.TierEntity,
		Content: "spreading activation query anchor uniquetoken42",
		AgentID: "agent-1",
		Source:  store.SourceManual,
	}, []string{string(seedNode)})
	if err != nil {
		t.Fatalf("insert queryMem: %v", err)
	}

	// highMem anchored to highNode (CALLS from seed — highest activation).
	highMemID, err := srv.store.InsertMemoryWithAnchors(store.Memory{
		Tier:    store.TierEntity,
		Content: "high activation memory anchored to CALLS neighbor",
		AgentID: "agent-1",
		Source:  store.SourceManual,
	}, []string{string(highNode)})
	if err != nil {
		t.Fatalf("insert highMem: %v", err)
	}

	// lowMem anchored to lowNode (IMPORTS from seed — lower activation).
	lowMemID, err := srv.store.InsertMemoryWithAnchors(store.Memory{
		Tier:    store.TierEntity,
		Content: "low activation memory anchored to IMPORTS neighbor",
		AgentID: "agent-1",
		Source:  store.SourceManual,
	}, []string{string(lowNode)})
	if err != nil {
		t.Fatalf("insert lowMem: %v", err)
	}

	// Suppress unused variable warnings — IDs used in assertions below.
	_ = queryMemID

	// Run quad recall with a query matching the seed memory's content.
	mems, attribution, _, _ := srv.quadRecallSearch(
		context.Background(), "uniquetoken42", "", 10, false, 7, nil, 1,
	)

	// Verify both highMem and lowMem appear in the graph channel results.
	highIdx, lowIdx := -1, -1
	for i, m := range mems {
		switch m.ID {
		case highMemID:
			highIdx = i
		case lowMemID:
			lowIdx = i
		}
	}

	if highIdx < 0 {
		t.Fatalf("highMem (ID=%s) not found in results; got %d memories", highMemID, len(mems))
	}
	if lowIdx < 0 {
		t.Fatalf("lowMem (ID=%s) not found in results; got %d memories", lowMemID, len(mems))
	}

	// Both should be attributed to the graph channel.
	for _, id := range []string{highMemID, lowMemID} {
		found := false
		for _, ch := range attribution[id] {
			if ch == "graph" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("memory %s should be attributed to graph channel, got %v", id, attribution[id])
		}
	}

	// The high-activation memory must rank ahead of the low-activation one.
	if highIdx >= lowIdx {
		t.Errorf("highMem (rank %d) should rank above lowMem (rank %d) due to higher activation", highIdx, lowIdx)
	}
}

// TestQuadRecallSearch_MultiAnchorUsesMaxActivation verifies that a memory with
// multiple anchor nodes is sorted by its MAXIMUM activation across all anchors,
// not the activation of its oldest anchor. This covers the correctness fix for
// GetAllMemoryAnchorNodeIDsInSet replacing the single-anchor sort path.
//
// Graph: seedNode → highNode (CALLS)  → activation 0.5
//        seedNode → lowNode  (IMPORTS) → activation 0.25
//
// dualAnchorMem is anchored to BOTH lowNode AND highNode.
// singleHighMem is anchored to highNode only.
// singleLowMem is anchored to lowNode only.
//
// Expected order: singleHighMem == dualAnchorMem (both 0.5) > singleLowMem (0.25).
// The key correctness assertion: dualAnchorMem must NOT rank below singleLowMem.
func TestQuadRecallSearch_MultiAnchorUsesMaxActivation(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	seedNode := srv.graph.MakeNodeID("seed2.go", "Seed2")
	highNode := srv.graph.MakeNodeID("high2.go", "High2")
	lowNode := srv.graph.MakeNodeID("low2.go", "Low2")
	for _, n := range []*graph.Node{
		{ID: seedNode, Name: "Seed2", Type: graph.NodeFunction, File: "seed2.go", Line: 1},
		{ID: highNode, Name: "High2", Type: graph.NodeFunction, File: "high2.go", Line: 1},
		{ID: lowNode, Name: "Low2", Type: graph.NodeFunction, File: "low2.go", Line: 1},
	} {
		srv.graph.AddNode(n)
	}
	srv.graph.AddEdge(&graph.Edge{From: seedNode, To: highNode, Type: graph.EdgeCalls})
	srv.graph.AddEdge(&graph.Edge{From: seedNode, To: lowNode, Type: graph.EdgeImports})

	// queryMem: triggers BFS from seedNode.
	_, err := srv.store.InsertMemoryWithAnchors(store.Memory{
		Tier:    store.TierEntity,
		Content: "multianchor activation query uniquetok99",
		AgentID: "agent-1",
		Source:  store.SourceManual,
	}, []string{string(seedNode)})
	if err != nil {
		t.Fatalf("insert queryMem: %v", err)
	}

	// dualAnchorMem: anchored to BOTH lowNode (inserted first) and highNode.
	// Without max-activation fix, it would be sorted by lowNode's activation (0.25).
	dualMemID, err := srv.store.InsertMemoryWithAnchors(store.Memory{
		Tier:    store.TierEntity,
		Content: "dual anchor memory with low and high anchors",
		AgentID: "agent-1",
		Source:  store.SourceManual,
	}, []string{string(lowNode), string(highNode)}) // lowNode first (older anchor)
	if err != nil {
		t.Fatalf("insert dualAnchorMem: %v", err)
	}

	// singleLowMem: anchored only to lowNode (activation 0.25).
	lowMemID, err := srv.store.InsertMemoryWithAnchors(store.Memory{
		Tier:    store.TierEntity,
		Content: "single anchor memory with low activation only",
		AgentID: "agent-1",
		Source:  store.SourceManual,
	}, []string{string(lowNode)})
	if err != nil {
		t.Fatalf("insert singleLowMem: %v", err)
	}

	mems, _, _, _ := srv.quadRecallSearch(
		context.Background(), "uniquetok99", "", 10, false, 7, nil, 1,
	)

	dualIdx, lowIdx := -1, -1
	for i, m := range mems {
		switch m.ID {
		case dualMemID:
			dualIdx = i
		case lowMemID:
			lowIdx = i
		}
	}
	if dualIdx < 0 {
		t.Fatalf("dualAnchorMem not found in results")
	}
	if lowIdx < 0 {
		t.Fatalf("singleLowMem not found in results")
	}
	// dualAnchorMem has max activation = 0.5 (via highNode).
	// singleLowMem has max activation = 0.25 (via lowNode only).
	// dualAnchorMem must rank at least as high as singleLowMem.
	if dualIdx > lowIdx {
		t.Errorf("dualAnchorMem (rank %d) should rank >= singleLowMem (rank %d): multi-anchor must use max activation", dualIdx, lowIdx)
	}
}
