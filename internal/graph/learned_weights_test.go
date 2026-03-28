package graph_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// TestLearnedEdgeMult_NilMap returns 1.0 for nil map (neutral).
func TestLearnedEdgeMult_NilMap(t *testing.T) {
	t.Parallel()
	// buildCarveFixture gives us real NodeIDs.
	g, ids := buildCarveFixture(t)
	_ = g

	// CarveEgoGraph with no LearnedEdgeWeights should behave identically to
	// a config with LearnedEdgeWeights = nil. We verify indirectly: both
	// traversals should return the same root node.
	cfgDefault := graph.DefaultCarveConfig()
	subDefault, err := g.CarveEgoGraph(ids["handler"], cfgDefault)
	if err != nil {
		t.Fatalf("CarveEgoGraph (no learned weights): %v", err)
	}

	cfgNil := graph.DefaultCarveConfig()
	cfgNil.LearnedEdgeWeights = nil
	subNil, err := g.CarveEgoGraph(ids["handler"], cfgNil)
	if err != nil {
		t.Fatalf("CarveEgoGraph (nil learned weights): %v", err)
	}
	if len(subDefault.Nodes) != len(subNil.Nodes) {
		t.Errorf("nil vs absent LearnedEdgeWeights produced different node counts: %d vs %d",
			len(subDefault.Nodes), len(subNil.Nodes))
	}
}

// TestLearnedEdgeMult_PenalisedEdge verifies that a heavily penalised edge
// causes the downstream node to rank lower (or disappear) compared to the
// baseline traversal with no learned weights.
func TestLearnedEdgeMult_PenalisedEdge(t *testing.T) {
	t.Parallel()
	g, ids := buildCarveFixture(t)

	// Baseline: no learned weights — UserRepo should be reachable (3 hops).
	// Use BFS (UsePPR=false) for deterministic decay-based scoring that responds
	// predictably to per-edge multipliers.
	baseline := graph.DefaultCarveConfig()
	baseline.UsePPR = false
	subBase, err := g.CarveEgoGraph(ids["handler"], baseline)
	if err != nil {
		t.Fatalf("baseline CarveEgoGraph: %v", err)
	}
	baselineHasRepo := false
	for _, cn := range subBase.Nodes {
		if cn.Node.ID == ids["repo"] {
			baselineHasRepo = true
			break
		}
	}
	if !baselineHasRepo {
		t.Skip("UserRepo not in baseline context — test graph needs adjustment")
	}

	// Penalised: set the handler→service edge to floor (0.3×).
	// Use BFS (UsePPR=false) for deterministic decay-based scoring — PPR redistributes
	// probability mass globally, which can paradoxically raise downstream node scores
	// when one edge is penalised (mass flows via other paths).
	cfg := graph.DefaultCarveConfig()
	cfg.UsePPR = false
	cfg.MinRelevance = 0.05 // keep threshold low so floor matters, not threshold
	cfg.LearnedEdgeWeights = map[graph.EdgeWeightKey]float64{
		{From: ids["handler"], To: ids["service"], Type: graph.EdgeCalls}: 0.3,
	}
	subPenalised, err := g.CarveEgoGraph(ids["handler"], cfg)
	if err != nil {
		t.Fatalf("penalised CarveEgoGraph: %v", err)
	}

	// Find relevance of UserRepo in both traversals.
	baselineRepoScore := -1.0
	penalisedRepoScore := -1.0
	for _, cn := range subBase.Nodes {
		if cn.Node.ID == ids["repo"] {
			baselineRepoScore = cn.Relevance
		}
	}
	for _, cn := range subPenalised.Nodes {
		if cn.Node.ID == ids["repo"] {
			penalisedRepoScore = cn.Relevance
		}
	}

	if penalisedRepoScore >= baselineRepoScore {
		t.Errorf("penalised edge should lower downstream relevance: baseline=%f penalised=%f",
			baselineRepoScore, penalisedRepoScore)
	}
}

// TestLearnedEdgeMult_BoostedEdge verifies that a boosted edge raises
// downstream node relevance compared to the baseline.
func TestLearnedEdgeMult_BoostedEdge(t *testing.T) {
	t.Parallel()
	g, ids := buildCarveFixture(t)

	baseline := graph.DefaultCarveConfig()
	subBase, err := g.CarveEgoGraph(ids["handler"], baseline)
	if err != nil {
		t.Fatalf("baseline CarveEgoGraph: %v", err)
	}

	cfg := graph.DefaultCarveConfig()
	cfg.LearnedEdgeWeights = map[graph.EdgeWeightKey]float64{
		{From: ids["handler"], To: ids["service"], Type: graph.EdgeCalls}: 2.0,
	}
	subBoosted, err := g.CarveEgoGraph(ids["handler"], cfg)
	if err != nil {
		t.Fatalf("boosted CarveEgoGraph: %v", err)
	}

	// AuthService (direct neighbor via boosted edge) should rank higher.
	baselineServiceScore := -1.0
	boostedServiceScore := -1.0
	for _, cn := range subBase.Nodes {
		if cn.Node.ID == ids["service"] {
			baselineServiceScore = cn.Relevance
		}
	}
	for _, cn := range subBoosted.Nodes {
		if cn.Node.ID == ids["service"] {
			boostedServiceScore = cn.Relevance
		}
	}

	if boostedServiceScore <= baselineServiceScore {
		t.Errorf("boosted edge should raise direct neighbor relevance: baseline=%f boosted=%f",
			baselineServiceScore, boostedServiceScore)
	}
}
