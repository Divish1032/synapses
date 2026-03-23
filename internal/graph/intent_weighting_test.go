package graph_test

// Tests for F18: Intent-Aware BFS Edge Weighting.
//
// Covers:
//   - IntentCarveWeights returns the right map for each intent
//   - IntentDirectionBoost returns the right value for each intent
//   - Positive DirectionBoost assigns higher relevance to callees than callers
//   - Negative DirectionBoost assigns higher relevance to callers than callees
//   - Review intent (IMPLEMENTS boost) makes interface node beat a CALLS node
//   - Cache isolation: two intent-specific calls on the same root return
//     results consistent with their own config (no cross-intent cache collision)

import (
	"math"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// buildDirectionalFixture creates a graph with explicit callers and callees:
//
//	callerA ─CALLS─►
//	callerB ─CALLS─► target ─CALLS─► calleeA
//	                        ─CALLS─► calleeB
func buildDirectionalFixture(t *testing.T) (*graph.Graph, map[string]graph.NodeID) {
	t.Helper()
	g := graph.New("testrepo")

	ids := map[string]graph.NodeID{
		"target":  g.MakeNodeID("target.go", "target"),
		"callerA": g.MakeNodeID("caller.go", "callerA"),
		"callerB": g.MakeNodeID("caller.go", "callerB"),
		"calleeA": g.MakeNodeID("callee.go", "calleeA"),
		"calleeB": g.MakeNodeID("callee.go", "calleeB"),
	}

	g.AddNode(&graph.Node{ID: ids["target"], Type: graph.NodeFunction, Name: "target", File: "target.go"})
	g.AddNode(&graph.Node{ID: ids["callerA"], Type: graph.NodeFunction, Name: "callerA", File: "caller.go"})
	g.AddNode(&graph.Node{ID: ids["callerB"], Type: graph.NodeFunction, Name: "callerB", File: "caller.go"})
	g.AddNode(&graph.Node{ID: ids["calleeA"], Type: graph.NodeFunction, Name: "calleeA", File: "callee.go"})
	g.AddNode(&graph.Node{ID: ids["calleeB"], Type: graph.NodeFunction, Name: "calleeB", File: "callee.go"})

	// callerA and callerB call target (incoming CALLS to target).
	g.AddEdge(&graph.Edge{From: ids["callerA"], To: ids["target"], Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: ids["callerB"], To: ids["target"], Type: graph.EdgeCalls})
	// target calls calleeA and calleeB (outgoing CALLS from target).
	g.AddEdge(&graph.Edge{From: ids["target"], To: ids["calleeA"], Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: ids["target"], To: ids["calleeB"], Type: graph.EdgeCalls})

	return g, ids
}

// buildImplementsFixture creates a graph with one CALLS neighbor and one
// IMPLEMENTS neighbor at hop 1, to test weight-differentiated traversal.
//
//	target ─CALLS──────► calleeNode
//	target ─IMPLEMENTS─► ifaceNode
func buildImplementsFixture(t *testing.T) (*graph.Graph, map[string]graph.NodeID) {
	t.Helper()
	g := graph.New("testrepo")

	ids := map[string]graph.NodeID{
		"target":    g.MakeNodeID("svc.go", "target"),
		"calleeNode": g.MakeNodeID("svc.go", "calleeNode"),
		"ifaceNode": g.MakeNodeID("iface.go", "ifaceNode"),
	}

	g.AddNode(&graph.Node{ID: ids["target"], Type: graph.NodeFunction, Name: "target", File: "svc.go"})
	g.AddNode(&graph.Node{ID: ids["calleeNode"], Type: graph.NodeFunction, Name: "calleeNode", File: "svc.go"})
	g.AddNode(&graph.Node{ID: ids["ifaceNode"], Type: graph.NodeInterface, Name: "ifaceNode", File: "iface.go"})

	g.AddEdge(&graph.Edge{From: ids["target"], To: ids["calleeNode"], Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: ids["target"], To: ids["ifaceNode"], Type: graph.EdgeImplements})

	return g, ids
}

// relevanceMap extracts nodeID → relevance from a SubGraph.
func relevanceMap(sub *graph.SubGraph) map[graph.NodeID]float64 {
	m := make(map[graph.NodeID]float64, len(sub.Nodes))
	for _, cn := range sub.Nodes {
		m[cn.Node.ID] = cn.Relevance
	}
	return m
}

// ── IntentCarveWeights ────────────────────────────────────────────────────────

func TestIntentCarveWeights_NonNilForAllIntents(t *testing.T) {
	intents := []string{"modify", "debug", "review", "understand", "add", "plan"}
	for _, intent := range intents {
		w := graph.IntentCarveWeights(intent)
		if w == nil {
			t.Errorf("IntentCarveWeights(%q) returned nil", intent)
		}
		if _, ok := w[graph.EdgeCalls]; !ok {
			t.Errorf("IntentCarveWeights(%q) missing EdgeCalls entry", intent)
		}
	}
}

func TestIntentCarveWeights_UnknownFallsBackToDefault(t *testing.T) {
	w := graph.IntentCarveWeights("unknown-intent")
	def := graph.DefaultEdgeWeights
	for et, dw := range def {
		if w[et] != dw {
			t.Errorf("IntentCarveWeights(unknown)[%s] = %v, want %v (default)", et, w[et], dw)
		}
	}
}

func TestIntentCarveWeights_ModifyReducesImplements(t *testing.T) {
	w := graph.IntentCarveWeights("modify")
	def := graph.DefaultEdgeWeights
	if w[graph.EdgeImplements] >= def[graph.EdgeImplements] {
		t.Errorf("modify: EdgeImplements weight %v should be < default %v (focus on callees, not contracts)",
			w[graph.EdgeImplements], def[graph.EdgeImplements])
	}
}

func TestIntentCarveWeights_DebugBoostsDataFlows(t *testing.T) {
	w := graph.IntentCarveWeights("debug")
	def := graph.DefaultEdgeWeights
	if w[graph.EdgeDataFlows] <= def[graph.EdgeDataFlows] {
		t.Errorf("debug: EdgeDataFlows weight %v should be > default %v (data flow traces bugs)",
			w[graph.EdgeDataFlows], def[graph.EdgeDataFlows])
	}
}

func TestIntentCarveWeights_ReviewBoostsImplements(t *testing.T) {
	w := graph.IntentCarveWeights("review")
	def := graph.DefaultEdgeWeights
	if w[graph.EdgeImplements] <= def[graph.EdgeImplements] {
		t.Errorf("review: EdgeImplements weight %v should be > default %v (interface coverage)",
			w[graph.EdgeImplements], def[graph.EdgeImplements])
	}
}

func TestIntentCarveWeights_AddBoostsImports(t *testing.T) {
	w := graph.IntentCarveWeights("add")
	def := graph.DefaultEdgeWeights
	if w[graph.EdgeImports] <= def[graph.EdgeImports] {
		t.Errorf("add: EdgeImports weight %v should be > default %v (follow import patterns)",
			w[graph.EdgeImports], def[graph.EdgeImports])
	}
}

func TestIntentCarveWeights_PlanBoostsImplements(t *testing.T) {
	w := graph.IntentCarveWeights("plan")
	def := graph.DefaultEdgeWeights
	if w[graph.EdgeImplements] <= def[graph.EdgeImplements] {
		t.Errorf("plan: EdgeImplements weight %v should be > default %v (interface scope)",
			w[graph.EdgeImplements], def[graph.EdgeImplements])
	}
}

func TestIntentCarveWeights_UnderstandMatchesDefault(t *testing.T) {
	w := graph.IntentCarveWeights("understand")
	def := graph.DefaultEdgeWeights
	for et, dw := range def {
		if w[et] != dw {
			t.Errorf("understand[%s] = %v, want default %v", et, w[et], dw)
		}
	}
}

// ── IntentDirectionBoost ─────────────────────────────────────────────────────

func TestIntentDirectionBoost_Values(t *testing.T) {
	cases := []struct {
		intent string
		want   float64
	}{
		{"modify", 0.3},
		{"debug", -0.3},
		{"review", 0.0},
		{"understand", 0.2},
		{"add", 0.2},
		{"plan", 0.2},
		{"unknown", 0.2}, // default fallback
	}
	for _, tc := range cases {
		got := graph.IntentDirectionBoost(tc.intent)
		if got != tc.want {
			t.Errorf("IntentDirectionBoost(%q) = %v, want %v", tc.intent, got, tc.want)
		}
	}
}

// ── DirectionBoost relevance scoring ─────────────────────────────────────────

// TestCarveEgoGraph_PositiveBoostGivesCalleesHigherRelevance verifies that when
// DirectionBoost > 0, outgoing CALLS neighbors (callees) receive a higher
// relevance score than incoming CALLS neighbors (callers) at the same hop.
func TestCarveEgoGraph_PositiveBoostGivesCalleesHigherRelevance(t *testing.T) {
	g, ids := buildDirectionalFixture(t)

	cfg := graph.DefaultCarveConfig()
	cfg.MaxDepth = 1
	cfg.TokenBudget = 100000 // no pruning — we test relevance, not survival
	cfg.DirectionBoost = 0.5
	cfg.IntentID = "test-positive"
	cfg.EdgeWeights = map[graph.EdgeType]float64{
		graph.EdgeCalls: 1.0,
	}

	sub, err := g.CarveEgoGraph(ids["target"], cfg)
	if err != nil {
		t.Fatalf("CarveEgoGraph: %v", err)
	}

	rel := relevanceMap(sub)

	calleeARel := rel[ids["calleeA"]]
	callerARel := rel[ids["callerA"]]

	if calleeARel == 0 {
		t.Fatal("calleeA missing from subgraph")
	}
	if callerARel == 0 {
		t.Fatal("callerA missing from subgraph")
	}

	if calleeARel <= callerARel {
		t.Errorf("positive DirectionBoost=0.5: calleeA relevance %v should be > callerA relevance %v",
			calleeARel, callerARel)
	}
}

// TestCarveEgoGraph_NegativeBoostGivesCallersHigherRelevance verifies that when
// DirectionBoost < 0, incoming CALLS neighbors (callers) receive a higher
// relevance score than outgoing CALLS neighbors (callees) at the same hop.
func TestCarveEgoGraph_NegativeBoostGivesCallersHigherRelevance(t *testing.T) {
	g, ids := buildDirectionalFixture(t)

	cfg := graph.DefaultCarveConfig()
	cfg.MaxDepth = 1
	cfg.TokenBudget = 100000 // no pruning
	cfg.DirectionBoost = -0.5
	cfg.IntentID = "test-negative"
	cfg.EdgeWeights = map[graph.EdgeType]float64{
		graph.EdgeCalls: 1.0,
	}

	sub, err := g.CarveEgoGraph(ids["target"], cfg)
	if err != nil {
		t.Fatalf("CarveEgoGraph: %v", err)
	}

	rel := relevanceMap(sub)

	calleeARel := rel[ids["calleeA"]]
	callerARel := rel[ids["callerA"]]

	if calleeARel == 0 {
		t.Fatal("calleeA missing from subgraph")
	}
	if callerARel == 0 {
		t.Fatal("callerA missing from subgraph")
	}

	if callerARel <= calleeARel {
		t.Errorf("negative DirectionBoost=-0.5: callerA relevance %v should be > calleeA relevance %v",
			callerARel, calleeARel)
	}
}

// TestCarveEgoGraph_ZeroBoostGivesEqualRelevance verifies that DirectionBoost=0
// gives callers and callees identical relevance (symmetrical BFS).
func TestCarveEgoGraph_ZeroBoostGivesEqualRelevance(t *testing.T) {
	g, ids := buildDirectionalFixture(t)

	cfg := graph.DefaultCarveConfig()
	cfg.MaxDepth = 1
	cfg.TokenBudget = 100000
	cfg.DirectionBoost = 0.0
	cfg.IntentID = "test-zero"
	cfg.EdgeWeights = map[graph.EdgeType]float64{
		graph.EdgeCalls: 1.0,
	}

	sub, err := g.CarveEgoGraph(ids["target"], cfg)
	if err != nil {
		t.Fatalf("CarveEgoGraph: %v", err)
	}

	rel := relevanceMap(sub)

	calleeARel := rel[ids["calleeA"]]
	callerARel := rel[ids["callerA"]]

	if calleeARel != callerARel {
		t.Errorf("zero DirectionBoost: callerA (%v) and calleeA (%v) should have equal relevance",
			callerARel, calleeARel)
	}
}

// TestCarveEgoGraph_PositiveBoostMagnitudeIsCorrect verifies the exact relevance
// multiplier: callee relevance = base × (1 + DirectionBoost).
func TestCarveEgoGraph_PositiveBoostMagnitudeIsCorrect(t *testing.T) {
	g, ids := buildDirectionalFixture(t)

	const boost = 0.3
	const edgeW = 1.0
	const decay = 0.5

	// target has 4 edges total (callerA, callerB in + calleeA, calleeB out).
	// Adaptive decay: localDecay = decay / (1 + log2(numEdges+1))
	localDecay := decay / (1.0 + math.Log2(float64(4+1)))

	cfg := graph.DefaultCarveConfig()
	cfg.MaxDepth = 1
	cfg.TokenBudget = 100000
	cfg.MinRelevance = 0 // test scoring, not pruning
	cfg.UsePPR = false   // test exact BFS score arithmetic
	cfg.DirectionBoost = boost
	cfg.DecayFactor = decay
	cfg.IntentID = "test-magnitude"
	cfg.EdgeWeights = map[graph.EdgeType]float64{graph.EdgeCalls: edgeW}

	sub, err := g.CarveEgoGraph(ids["target"], cfg)
	if err != nil {
		t.Fatalf("CarveEgoGraph: %v", err)
	}

	rel := relevanceMap(sub)

	// Expected: callee = edgeW × localDecay × (1 + boost)
	wantCallee := edgeW * localDecay * (1.0 + boost)
	// Expected: caller = edgeW × localDecay (no boost)
	wantCaller := edgeW * localDecay

	const eps = 0.001
	if diff := rel[ids["calleeA"]] - wantCallee; diff > eps || diff < -eps {
		t.Errorf("calleeA relevance: got %v, want %v (±%v)", rel[ids["calleeA"]], wantCallee, eps)
	}
	if diff := rel[ids["callerA"]] - wantCaller; diff > eps || diff < -eps {
		t.Errorf("callerA relevance: got %v, want %v (±%v)", rel[ids["callerA"]], wantCaller, eps)
	}
}

// TestCarveEgoGraph_NegativeBoostMagnitudeIsCorrect verifies caller relevance
// = base × (1 − DirectionBoost) when DirectionBoost is negative.
func TestCarveEgoGraph_NegativeBoostMagnitudeIsCorrect(t *testing.T) {
	g, ids := buildDirectionalFixture(t)

	const boost = -0.3
	const edgeW = 1.0
	const decay = 0.5

	// target has 4 edges total (callerA, callerB in + calleeA, calleeB out).
	// Adaptive decay: localDecay = decay / (1 + log2(numEdges+1))
	localDecay := decay / (1.0 + math.Log2(float64(4+1)))

	cfg := graph.DefaultCarveConfig()
	cfg.MaxDepth = 1
	cfg.TokenBudget = 100000
	cfg.MinRelevance = 0 // test scoring, not pruning
	cfg.UsePPR = false   // test exact BFS score arithmetic
	cfg.DirectionBoost = boost
	cfg.DecayFactor = decay
	cfg.IntentID = "test-neg-magnitude"
	cfg.EdgeWeights = map[graph.EdgeType]float64{graph.EdgeCalls: edgeW}

	sub, err := g.CarveEgoGraph(ids["target"], cfg)
	if err != nil {
		t.Fatalf("CarveEgoGraph: %v", err)
	}

	rel := relevanceMap(sub)

	// Expected: caller = edgeW × localDecay × (1 − boost) — 1.0 - (-0.3) = 1.3
	wantCaller := edgeW * localDecay * (1.0 - boost)
	// Expected: callee = edgeW × localDecay (no boost when boost < 0)
	wantCallee := edgeW * localDecay

	const eps = 0.001
	if diff := rel[ids["callerA"]] - wantCaller; diff > eps || diff < -eps {
		t.Errorf("callerA relevance: got %v, want %v (±%v)", rel[ids["callerA"]], wantCaller, eps)
	}
	if diff := rel[ids["calleeA"]] - wantCallee; diff > eps || diff < -eps {
		t.Errorf("calleeA relevance: got %v, want %v (±%v)", rel[ids["calleeA"]], wantCallee, eps)
	}
}

// ── Intent edge-weight differentiation ───────────────────────────────────────

// TestCarveEgoGraph_ReviewWeightsMakeIfaceNodeMoreRelevant verifies that the
// review intent's boosted IMPLEMENTS weight causes an interface neighbor to
// score higher than a CALLS neighbor — the reverse of the default ordering.
func TestCarveEgoGraph_ReviewWeightsMakeIfaceNodeMoreRelevant(t *testing.T) {
	g, ids := buildImplementsFixture(t)

	const decay = 0.5

	// Default config: EdgeCalls=1.0 > EdgeImplements=0.9 → callee > iface.
	defaultCfg := graph.DefaultCarveConfig()
	defaultCfg.MaxDepth = 1
	defaultCfg.TokenBudget = 100000
	defaultCfg.MinRelevance = 0 // test scoring, not pruning
	defaultCfg.DecayFactor = decay
	defaultCfg.DirectionBoost = 0.0 // disable directional effect
	defaultCfg.IntentID = "default-test"

	defSub, err := g.CarveEgoGraph(ids["target"], defaultCfg)
	if err != nil {
		t.Fatalf("default CarveEgoGraph: %v", err)
	}
	defRel := relevanceMap(defSub)

	if defRel[ids["calleeNode"]] <= defRel[ids["ifaceNode"]] {
		t.Errorf("default weights: calleeNode (%v) should beat ifaceNode (%v) (CALLS=1.0 > IMPLEMENTS=0.9)",
			defRel[ids["calleeNode"]], defRel[ids["ifaceNode"]])
	}

	// Review config: EdgeImplements=1.2 > EdgeCalls=0.8 → iface > callee.
	reviewCfg := graph.DefaultCarveConfig()
	reviewCfg.MaxDepth = 1
	reviewCfg.TokenBudget = 100000
	reviewCfg.MinRelevance = 0 // test scoring, not pruning
	reviewCfg.DecayFactor = decay
	reviewCfg.EdgeWeights = graph.IntentCarveWeights("review")
	reviewCfg.DirectionBoost = graph.IntentDirectionBoost("review") // 0.0
	reviewCfg.IntentID = "review"

	revSub, err := g.CarveEgoGraph(ids["target"], reviewCfg)
	if err != nil {
		t.Fatalf("review CarveEgoGraph: %v", err)
	}
	revRel := relevanceMap(revSub)

	if revRel[ids["ifaceNode"]] <= revRel[ids["calleeNode"]] {
		t.Errorf("review weights: ifaceNode (%v) should beat calleeNode (%v) (IMPLEMENTS=1.2 > CALLS=0.8)",
			revRel[ids["ifaceNode"]], revRel[ids["calleeNode"]])
	}
}

// ── Cache isolation ───────────────────────────────────────────────────────────

// TestCarveEgoGraph_IntentCacheIsolation verifies that two calls with different
// IntentIDs on the same root return results consistent with their own config
// and do not serve stale cross-intent results from the cache.
//
// Strategy: call with "modify" config (positive boost → callees higher), then
// call with "debug" config (negative boost → callers higher). If cache
// incorrectly shares the result, the second call will return wrong ordering.
func TestCarveEgoGraph_IntentCacheIsolation(t *testing.T) {
	g, ids := buildDirectionalFixture(t)

	base := graph.DefaultCarveConfig()
	base.MaxDepth = 1
	base.TokenBudget = 100000
	base.MinRelevance = 0 // test scoring, not pruning
	base.DecayFactor = 0.5
	base.EdgeWeights = map[graph.EdgeType]float64{graph.EdgeCalls: 1.0}

	// First call: "modify" — positive boost, callees should be higher.
	modifyCfg := base
	modifyCfg.DirectionBoost = 0.5
	modifyCfg.IntentID = "modify"

	modSub, err := g.CarveEgoGraph(ids["target"], modifyCfg)
	if err != nil {
		t.Fatalf("modify CarveEgoGraph: %v", err)
	}
	modRel := relevanceMap(modSub)
	if modRel[ids["calleeA"]] <= modRel[ids["callerA"]] {
		t.Errorf("modify call: calleeA (%v) should be > callerA (%v)",
			modRel[ids["calleeA"]], modRel[ids["callerA"]])
	}

	// Second call: "debug" — negative boost, callers should be higher.
	// If the cache incorrectly returned the "modify" result, callers would be lower.
	debugCfg := base
	debugCfg.DirectionBoost = -0.5
	debugCfg.IntentID = "debug"

	dbgSub, err := g.CarveEgoGraph(ids["target"], debugCfg)
	if err != nil {
		t.Fatalf("debug CarveEgoGraph: %v", err)
	}
	dbgRel := relevanceMap(dbgSub)
	if dbgRel[ids["callerA"]] <= dbgRel[ids["calleeA"]] {
		t.Errorf("debug call: callerA (%v) should be > calleeA (%v) — possible cache collision with modify result",
			dbgRel[ids["callerA"]], dbgRel[ids["calleeA"]])
	}
}

// TestCarveEgoGraph_SameIntentHitsCacheOnSecondCall verifies that the second call
// with the same config actually hits the cache (returns identical result).
func TestCarveEgoGraph_SameIntentHitsCacheOnSecondCall(t *testing.T) {
	g, ids := buildDirectionalFixture(t)

	cfg := graph.DefaultCarveConfig()
	cfg.MaxDepth = 1
	cfg.TokenBudget = 100000
	cfg.DirectionBoost = 0.3
	cfg.IntentID = "modify"

	sub1, err := g.CarveEgoGraph(ids["target"], cfg)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	sub2, err := g.CarveEgoGraph(ids["target"], cfg)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	// Cache hit: same pointer returned.
	if sub1 != sub2 {
		t.Error("second CarveEgoGraph call with identical config should return cached (same pointer) result")
	}
}
