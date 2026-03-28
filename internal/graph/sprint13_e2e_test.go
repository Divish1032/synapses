package graph_test

// Sprint 13 end-to-end integration test.
//
// Exercises the complete post-BFS/PPR pipeline in a single CarveEgoGraph call:
//   1. Personalized PageRank  (UsePPR=true)                      — Task #2
//   2. Multi-seed interface implementor expansion                 — Task #7
//   3. Precomputed eigenvector centrality boost (β=0.2)          — Task #4
//   4. Semantic-structural hybrid scoring (λ=0.3)                — Task #3
//   5. Adaptive decay (BFS path — used as reference comparison)  — Task #5
//
// Graph topology (interface root → two implementors → shared sink):
//
//	     Storer  (interface)
//	    ↑       ↑
//	FileStore  NetStore   ← concrete implementors (IMPLEMENTS edges)
//	    \       /
//	     \     /
//	      Store  (struct, hub — both implementors call it)
//	        |
//	      Flush  (function, 1-hop from hub)
//
// Expected outcomes:
//   - With PPR: Store outranks Flush because 2 paths converge on it (diamond boost).
//   - With eigenvector centrality: Store (high degree) gets an additional boost.
//   - With hybrid scoring: nodes semantically aligned with root get boosted;
//     "unrelated" node gets relatively lower score.
//   - FileStore and NetStore surface (multi-seed expansion from interface root).

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// makeNormVec builds a unit vector in R^dim with value 1.0 on axis `axis`.
func makeNormVec(dim, axis int) []float32 {
	v := make([]float32, dim)
	v[axis] = 1.0
	return v
}

func TestSprint13_E2E_FullPipeline(t *testing.T) {
	g := graph.New("e2e-sprint13")
	mkID := func(file, name string) graph.NodeID {
		return g.MakeNodeID(file, name)
	}

	// ── Nodes ──────────────────────────────────────────────────────────────
	storerID := mkID("store.go", "Storer")       // interface root
	fileStoreID := mkID("store.go", "FileStore") // implementor
	netStoreID := mkID("store.go", "NetStore")   // implementor
	storeID := mkID("store.go", "Store")         // hub — both call it
	flushID := mkID("store.go", "Flush")         // 1-hop from hub (leaf)
	unrelatedID := mkID("other.go", "Parser")    // no path to Storer

	g.AddNode(&graph.Node{ID: storerID, Type: graph.NodeInterface, Name: "Storer", File: "store.go"})
	g.AddNode(&graph.Node{ID: fileStoreID, Type: graph.NodeStruct, Name: "FileStore", File: "store.go"})
	g.AddNode(&graph.Node{ID: netStoreID, Type: graph.NodeStruct, Name: "NetStore", File: "store.go"})
	g.AddNode(&graph.Node{ID: storeID, Type: graph.NodeStruct, Name: "Store", File: "store.go"})
	g.AddNode(&graph.Node{ID: flushID, Type: graph.NodeFunction, Name: "Flush", File: "store.go"})
	g.AddNode(&graph.Node{ID: unrelatedID, Type: graph.NodeStruct, Name: "Parser", File: "other.go"})

	// ── Edges ──────────────────────────────────────────────────────────────
	// FileStore and NetStore implement Storer.
	g.AddEdge(&graph.Edge{From: fileStoreID, To: storerID, Type: graph.EdgeImplements})
	g.AddEdge(&graph.Edge{From: netStoreID, To: storerID, Type: graph.EdgeImplements})
	// Both implementors call the shared Store hub — creates diamond convergence on Store.
	g.AddEdge(&graph.Edge{From: fileStoreID, To: storeID, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: netStoreID, To: storeID, Type: graph.EdgeCalls})
	// Store calls Flush (1-hop leaf).
	g.AddEdge(&graph.Edge{From: storeID, To: flushID, Type: graph.EdgeCalls})

	// ── Embeddings ─────────────────────────────────────────────────────────
	// All store-domain nodes aligned on axis 0; Parser on axis 1.
	const dim = 4
	embeds := map[graph.NodeID][]float32{
		storerID:    makeNormVec(dim, 0),
		fileStoreID: makeNormVec(dim, 0),
		netStoreID:  makeNormVec(dim, 0),
		storeID:     makeNormVec(dim, 0),
		flushID:     makeNormVec(dim, 0),
		unrelatedID: makeNormVec(dim, 1), // unrelated domain
	}
	lookupFn := func(nodeIDs []graph.NodeID) map[graph.NodeID][]float32 {
		result := make(map[graph.NodeID][]float32, len(nodeIDs))
		for _, id := range nodeIDs {
			if v, ok := embeds[id]; ok {
				result[id] = v
			}
		}
		return result
	}

	// ── Config: all Sprint 13 features on ─────────────────────────────────
	cfg := graph.DefaultCarveConfig()
	cfg.MaxDepth = 5
	cfg.MinRelevance = 0
	cfg.TokenBudget = 0
	cfg.UsePPR = true
	cfg.EmbeddingLookup = lookupFn
	cfg.HybridLambda = 0.3

	sub, err := g.CarveEgoGraph(storerID, cfg)
	if err != nil {
		t.Fatalf("CarveEgoGraph Sprint 13 E2E: %v", err)
	}

	rel := make(map[graph.NodeID]float64)
	for _, cn := range sub.Nodes {
		rel[cn.Node.ID] = cn.Relevance
	}

	// ── Assertions ─────────────────────────────────────────────────────────

	// 1. Root pinned at 1.0 (invariant across all Sprint 13 features).
	if rel[storerID] != 1.0 {
		t.Errorf("root (Storer) relevance = %.4f, want 1.0", rel[storerID])
	}

	// 2. Multi-seed interface expansion: both implementors must appear.
	if rel[fileStoreID] == 0 {
		t.Error("FileStore (implementor) should be in results via multi-seed expansion")
	}
	if rel[netStoreID] == 0 {
		t.Error("NetStore (implementor) should be in results via multi-seed expansion")
	}

	// 3. PPR diamond boost: Store (2 converging paths) must outrank Flush (1 path).
	if rel[storeID] == 0 {
		t.Fatal("Store (hub) not found in results")
	}
	if rel[flushID] == 0 {
		t.Fatal("Flush (leaf) not found in results")
	}
	if rel[storeID] <= rel[flushID] {
		t.Errorf("PPR+centrality: Store (%.4f) should outrank Flush (%.4f) — 2 converging paths + hub centrality",
			rel[storeID], rel[flushID])
	}

	// 4. Unrelated node must be absent (no path from Storer to Parser).
	if rel[unrelatedID] != 0 {
		t.Errorf("unrelated Parser node should have relevance 0, got %.4f", rel[unrelatedID])
	}

	// 5. All store-domain nodes have same cosine similarity to root (axis 0 aligned).
	//    After hybrid blend they should all maintain positive relevance — not zeroed out.
	for _, id := range []graph.NodeID{fileStoreID, netStoreID, storeID, flushID} {
		if rel[id] <= 0 {
			t.Errorf("store-domain node %q: relevance %.4f should be > 0 after hybrid blend", id, rel[id])
		}
	}

	t.Logf("Sprint 13 E2E pipeline results:")
	t.Logf("  Storer (root):   %.4f", rel[storerID])
	t.Logf("  FileStore:       %.4f", rel[fileStoreID])
	t.Logf("  NetStore:        %.4f", rel[netStoreID])
	t.Logf("  Store (hub):     %.4f", rel[storeID])
	t.Logf("  Flush (leaf):    %.4f", rel[flushID])
	t.Logf("  Parser (unrel):  %.4f", rel[unrelatedID])
}

// TestSprint13_E2E_BFSvsPPR_SameTopology runs the same topology with BFS and
// PPR and compares Store's rank advantage over Flush in each mode.
// PPR's diamond advantage must be strictly larger than BFS's.
func TestSprint13_E2E_BFSvsPPR_SameTopology(t *testing.T) {
	buildGraph := func() (*graph.Graph, map[string]graph.NodeID) {
		g := graph.New("bfs-vs-ppr")
		mkID := func(name string) graph.NodeID { return g.MakeNodeID("main.go", name) }
		ids := map[string]graph.NodeID{
			"Root":  mkID("Root"),
			"A":     mkID("A"),
			"B":     mkID("B"),
			"Store": mkID("Store"),
			"Flush": mkID("Flush"),
		}
		for name, id := range ids {
			g.AddNode(&graph.Node{ID: id, Type: graph.NodeFunction, Name: name, File: "main.go"})
		}
		g.AddEdge(&graph.Edge{From: ids["Root"], To: ids["A"], Type: graph.EdgeCalls})
		g.AddEdge(&graph.Edge{From: ids["Root"], To: ids["B"], Type: graph.EdgeCalls})
		g.AddEdge(&graph.Edge{From: ids["A"], To: ids["Store"], Type: graph.EdgeCalls})
		g.AddEdge(&graph.Edge{From: ids["B"], To: ids["Store"], Type: graph.EdgeCalls})
		g.AddEdge(&graph.Edge{From: ids["A"], To: ids["Flush"], Type: graph.EdgeCalls})
		return g, ids
	}

	baseCfg := graph.DefaultCarveConfig()
	baseCfg.MaxDepth = 5
	baseCfg.MinRelevance = 0
	baseCfg.TokenBudget = 0

	// BFS run.
	gBFS, idsBFS := buildGraph()
	cfgBFS := baseCfg
	cfgBFS.UsePPR = false
	subBFS, err := gBFS.CarveEgoGraph(idsBFS["Root"], cfgBFS)
	if err != nil {
		t.Fatalf("BFS carve: %v", err)
	}
	relBFS := make(map[graph.NodeID]float64)
	for _, cn := range subBFS.Nodes {
		relBFS[cn.Node.ID] = cn.Relevance
	}
	bfsAdvantage := relBFS[idsBFS["Store"]] / relBFS[idsBFS["Flush"]]

	// PPR run.
	gPPR, idsPPR := buildGraph()
	cfgPPR := baseCfg
	cfgPPR.UsePPR = true
	subPPR, err := gPPR.CarveEgoGraph(idsPPR["Root"], cfgPPR)
	if err != nil {
		t.Fatalf("PPR carve: %v", err)
	}
	relPPR := make(map[graph.NodeID]float64)
	for _, cn := range subPPR.Nodes {
		relPPR[cn.Node.ID] = cn.Relevance
	}
	pprAdvantage := relPPR[idsPPR["Store"]] / relPPR[idsPPR["Flush"]]

	if pprAdvantage <= bfsAdvantage {
		t.Errorf("PPR Store/Flush advantage (%.2fx) should exceed BFS advantage (%.2fx) on diamond topology",
			pprAdvantage, bfsAdvantage)
	}
	t.Logf("BFS Store/Flush=%.2fx   PPR Store/Flush=%.2fx   PPR gain=%.2fx",
		bfsAdvantage, pprAdvantage, pprAdvantage/bfsAdvantage)
}
