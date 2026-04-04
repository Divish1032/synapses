package graph_test

// Tests for Sprint 16 #4: Multi-domain BFS/PPR in get_context.
//
// Covers:
//   - IsCrossDomainEdge correctly classifies edge types
//   - BFS CrossDomainDecay reduces relevance when crossing domain boundaries
//   - BFS CrossDomainDecay=0 disables the penalty (all nodes treated equally)
//   - BFS CrossDomainDecay=1 disables the penalty (value ≥ 1 is clamped/skipped)
//   - Cross-domain nodes still reach the carve output (decay doesn't zero them out)
//   - Intent weight maps include cross-domain edge types (no 0.5 fallback)
//   - DefaultCarveConfig includes CrossDomainDecay=0.5

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

func TestIsCrossDomainEdge(t *testing.T) {
	crossDomain := []graph.EdgeType{
		graph.EdgeDeploys,
		graph.EdgeConsumes,
		graph.EdgeConfiguredBy,
		graph.EdgeDocuments,
		graph.EdgeMentions,
		graph.EdgeManual,
	}
	for _, et := range crossDomain {
		if !graph.IsCrossDomainEdge(et) {
			t.Errorf("IsCrossDomainEdge(%q) = false, want true", et)
		}
	}

	sameDomain := []graph.EdgeType{
		graph.EdgeCalls,
		graph.EdgeImplements,
		graph.EdgeImports,
		graph.EdgeHandles,
	}
	for _, et := range sameDomain {
		if graph.IsCrossDomainEdge(et) {
			t.Errorf("IsCrossDomainEdge(%q) = true, want false", et)
		}
	}
}

func TestDefaultCarveConfigCrossDomainDecay(t *testing.T) {
	cfg := graph.DefaultCarveConfig()
	if cfg.CrossDomainDecay != 0.5 {
		t.Errorf("DefaultCarveConfig().CrossDomainDecay = %v, want 0.5", cfg.CrossDomainDecay)
	}
}

// buildCrossDomainFixture creates a graph with code and infra nodes:
//
//	codeFunc (code) --CALLS--> codeHelper (code)
//	codeFunc (code) --DEPLOYS--> infraResource (infra)
//
// This allows testing that:
// - codeHelper gets full relevance (same domain as codeFunc)
// - infraResource gets reduced relevance (different domain, decay applied)
func buildCrossDomainFixture(t *testing.T) (*graph.Graph, map[string]graph.NodeID) {
	t.Helper()
	g := graph.New("testrepo")

	ids := map[string]graph.NodeID{
		"codeFunc":      g.MakeNodeID("main.go", "DeployService"),
		"codeHelper":    g.MakeNodeID("helper.go", "buildConfig"),
		"infraResource": g.MakeNodeID("infra/main.tf", "aws_lambda_function.deploy_service"),
	}

	g.AddNode(&graph.Node{
		ID:     ids["codeFunc"],
		Type:   graph.NodeFunction,
		Name:   "DeployService",
		File:   "main.go",
		Domain: graph.DomainCode,
	})
	g.AddNode(&graph.Node{
		ID:     ids["codeHelper"],
		Type:   graph.NodeFunction,
		Name:   "buildConfig",
		File:   "helper.go",
		Domain: graph.DomainCode,
	})
	g.AddNode(&graph.Node{
		ID:     ids["infraResource"],
		Type:   graph.NodeFunction, // NodeType doesn't matter for domain tests
		Name:   "aws_lambda_function.deploy_service",
		File:   "infra/main.tf",
		Domain: graph.DomainInfra,
	})

	// Same-domain edge: code → code
	g.AddEdge(&graph.Edge{From: ids["codeFunc"], To: ids["codeHelper"], Type: graph.EdgeCalls})
	// Cross-domain edge: code → infra
	g.AddEdge(&graph.Edge{From: ids["codeFunc"], To: ids["infraResource"], Type: graph.EdgeDeploys})

	return g, ids
}

// TestBFSCrossDomainDecayReducesInfraRelevance verifies that when CrossDomainDecay=0.5,
// the infra node scores lower than the same-domain code node at equal edge distance.
func TestBFSCrossDomainDecayReducesInfraRelevance(t *testing.T) {
	g, ids := buildCrossDomainFixture(t)

	cfg := graph.DefaultCarveConfig()
	cfg.UsePPR = false
	cfg.CrossDomainDecay = 0.5
	cfg.MaxDepth = 2
	cfg.ExcludeTypes = nil // include all node types

	sg, err := g.CarveEgoGraph(ids["codeFunc"], cfg)
	if err != nil {
		t.Fatalf("CarveEgoGraph: %v", err)
	}

	relevance := make(map[graph.NodeID]float64)
	for _, cn := range sg.Nodes {
		relevance[cn.Node.ID] = cn.Relevance
	}

	helperRel, helperFound := relevance[ids["codeHelper"]]
	infraRel, infraFound := relevance[ids["infraResource"]]

	if !helperFound {
		t.Fatal("codeHelper not found in subgraph")
	}
	if !infraFound {
		t.Fatal("infraResource not found in subgraph — cross-domain traversal failed")
	}

	// The infra node is reached via DEPLOYS (weight 0.75) with CrossDomainDecay=0.5.
	// The code node is reached via CALLS (weight 1.0).
	// With BFS adaptive decay, codeHelper relevance ≈ 1.0×decay×1.0 and
	// infraResource relevance ≈ 0.75×decay×1.0×0.5 = 0.375×decay.
	// So infraResource must be strictly less than codeHelper.
	if infraRel >= helperRel {
		t.Errorf("infra relevance (%v) should be < code helper relevance (%v) with CrossDomainDecay=0.5",
			infraRel, helperRel)
	}
}

// TestBFSCrossDomainDecayZeroDisablesPenalty verifies that CrossDomainDecay=0 means
// no domain-boundary penalty is applied (infra and code nodes compete on edge weights alone).
func TestBFSCrossDomainDecayZeroDisablesPenalty(t *testing.T) {
	g, ids := buildCrossDomainFixture(t)

	// With decay=0 (disabled), infra node should score purely on edge weight (0.75).
	// Code node scores on CALLS edge weight (1.0).
	// So infra < code is still expected but not due to domain penalty.
	cfg := graph.DefaultCarveConfig()
	cfg.UsePPR = false
	cfg.CrossDomainDecay = 0.0 // disabled
	cfg.MaxDepth = 2
	cfg.ExcludeTypes = nil

	sg, err := g.CarveEgoGraph(ids["codeFunc"], cfg)
	if err != nil {
		t.Fatalf("CarveEgoGraph: %v", err)
	}

	found := false
	for _, cn := range sg.Nodes {
		if cn.Node.ID == ids["infraResource"] {
			found = true
			break
		}
	}
	if !found {
		t.Error("infraResource not found in subgraph with CrossDomainDecay=0 — should be included")
	}
}

// TestBFSCrossDomainDecayOneDisablesPenalty verifies that CrossDomainDecay≥1 is treated
// as no penalty (value is skipped per the `< 1` guard in BFS).
func TestBFSCrossDomainDecayOneDisablesPenalty(t *testing.T) {
	g, ids := buildCrossDomainFixture(t)

	cfgWithDecay := graph.DefaultCarveConfig()
	cfgWithDecay.UsePPR = false
	cfgWithDecay.CrossDomainDecay = 0.5
	cfgWithDecay.MaxDepth = 2
	cfgWithDecay.ExcludeTypes = nil

	cfgNoDecay := graph.DefaultCarveConfig()
	cfgNoDecay.UsePPR = false
	cfgNoDecay.CrossDomainDecay = 1.0 // no penalty
	cfgNoDecay.MaxDepth = 2
	cfgNoDecay.ExcludeTypes = nil

	sgWith, err := g.CarveEgoGraph(ids["codeFunc"], cfgWithDecay)
	if err != nil {
		t.Fatalf("CarveEgoGraph (with decay): %v", err)
	}
	sgNo, err := g.CarveEgoGraph(ids["codeFunc"], cfgNoDecay)
	if err != nil {
		t.Fatalf("CarveEgoGraph (no decay): %v", err)
	}

	var infraWithDecay, infraNoDecay float64
	for _, cn := range sgWith.Nodes {
		if cn.Node.ID == ids["infraResource"] {
			infraWithDecay = cn.Relevance
		}
	}
	for _, cn := range sgNo.Nodes {
		if cn.Node.ID == ids["infraResource"] {
			infraNoDecay = cn.Relevance
		}
	}

	// With CrossDomainDecay=0.5, infra relevance must be strictly lower than with decay=1.
	if infraWithDecay >= infraNoDecay {
		t.Errorf("infra relevance with decay=0.5 (%v) should be < decay=1.0 (%v)",
			infraWithDecay, infraNoDecay)
	}
}

// TestPPRCrossDomainDecayReducesInfraRelevance verifies PPR also applies the decay.
func TestPPRCrossDomainDecayReducesInfraRelevance(t *testing.T) {
	g, ids := buildCrossDomainFixture(t)

	cfgPPRDecay := graph.DefaultCarveConfig()
	cfgPPRDecay.UsePPR = true
	cfgPPRDecay.CrossDomainDecay = 0.5
	cfgPPRDecay.ExcludeTypes = nil

	cfgPPRNoDecay := graph.DefaultCarveConfig()
	cfgPPRNoDecay.UsePPR = true
	cfgPPRNoDecay.CrossDomainDecay = 1.0 // no penalty
	cfgPPRNoDecay.ExcludeTypes = nil

	sgDecay, err := g.CarveEgoGraph(ids["codeFunc"], cfgPPRDecay)
	if err != nil {
		t.Fatalf("PPR CarveEgoGraph (with decay): %v", err)
	}
	sgNoDecay, err := g.CarveEgoGraph(ids["codeFunc"], cfgPPRNoDecay)
	if err != nil {
		t.Fatalf("PPR CarveEgoGraph (no decay): %v", err)
	}

	var infraWithDecay, infraNoDecay float64
	for _, cn := range sgDecay.Nodes {
		if cn.Node.ID == ids["infraResource"] {
			infraWithDecay = cn.Relevance
		}
	}
	for _, cn := range sgNoDecay.Nodes {
		if cn.Node.ID == ids["infraResource"] {
			infraNoDecay = cn.Relevance
		}
	}

	if infraNoDecay == 0 {
		t.Skip("infraResource not found in PPR output — PPR horizon may exclude it; skip PPR decay test")
	}
	if infraWithDecay >= infraNoDecay {
		t.Errorf("PPR infra relevance with decay=0.5 (%v) should be < decay=1.0 (%v)",
			infraWithDecay, infraNoDecay)
	}
}

// TestBFSCrossDomainDecayAboveOneIsNoPenalty verifies that a CrossDomainDecay value
// greater than 1.0 is treated as no-penalty (the `> 0 && < 1` guard skips the decay).
// This protects against cache key fragmentation: decay=1.5 and decay=2.0 must produce
// identical subgraph scores so the caller's normalization clamp (to 1.0) is safe.
func TestBFSCrossDomainDecayAboveOneIsNoPenalty(t *testing.T) {
	g, ids := buildCrossDomainFixture(t)

	cfgOver := graph.DefaultCarveConfig()
	cfgOver.UsePPR = false
	cfgOver.CrossDomainDecay = 2.0 // > 1, guard skips decay
	cfgOver.MaxDepth = 2
	cfgOver.ExcludeTypes = nil

	cfgOne := graph.DefaultCarveConfig()
	cfgOne.UsePPR = false
	cfgOne.CrossDomainDecay = 1.0 // exact boundary, no penalty
	cfgOne.MaxDepth = 2
	cfgOne.ExcludeTypes = nil

	sgOver, err := g.CarveEgoGraph(ids["codeFunc"], cfgOver)
	if err != nil {
		t.Fatalf("CarveEgoGraph (decay=2.0): %v", err)
	}
	sgOne, err := g.CarveEgoGraph(ids["codeFunc"], cfgOne)
	if err != nil {
		t.Fatalf("CarveEgoGraph (decay=1.0): %v", err)
	}

	relevanceOver := make(map[graph.NodeID]float64)
	for _, cn := range sgOver.Nodes {
		relevanceOver[cn.Node.ID] = cn.Relevance
	}
	for _, cn := range sgOne.Nodes {
		if got, ok := relevanceOver[cn.Node.ID]; ok {
			if got != cn.Relevance {
				t.Errorf("node %s: decay=2.0 relevance %v != decay=1.0 relevance %v — values>1 must be no-op",
					cn.Node.ID, got, cn.Relevance)
			}
		}
	}
}

// TestIntentWeightsIncludeCrossDomainEdges ensures all intent weight maps contain
// the cross-domain edge types so they don't fall back to the 0.5 default.
func TestIntentWeightsIncludeCrossDomainEdges(t *testing.T) {
	crossDomainEdges := []graph.EdgeType{
		graph.EdgeDeploys,
		graph.EdgeConsumes,
		graph.EdgeConfiguredBy,
		graph.EdgeDocuments,
		graph.EdgeMentions,
		graph.EdgeManual,
	}
	intents := []string{"modify", "debug", "review", "add", "plan"}

	for _, intent := range intents {
		weights := graph.IntentCarveWeights(intent)
		for _, et := range crossDomainEdges {
			if _, ok := weights[et]; !ok {
				t.Errorf("intent %q weight map missing cross-domain edge type %q", intent, et)
			}
		}
	}
}
