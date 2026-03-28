package store

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// TestCrossDomainEdgeStats_Empty verifies the function returns all zeros on an
// empty manual_edges table — no error, no panic.
func TestCrossDomainEdgeStats_Empty(t *testing.T) {
	st := openFromTemplate(t)
	auto, confirmed, manual, err := st.CrossDomainEdgeStats()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auto != 0 || confirmed != 0 || manual != 0 {
		t.Errorf("expected all zeros on empty table, got auto=%d confirmed=%d manual=%d",
			auto, confirmed, manual)
	}
}

// TestCrossDomainEdgeStats_Buckets verifies each row is placed in the correct
// bucket: auto (namematcher, unconfirmed), confirmed (confirmed=1), manual (other).
func TestCrossDomainEdgeStats_Buckets(t *testing.T) {
	st := openFromTemplate(t)

	n1 := graph.NodeID("repo::a.go::A")
	n2 := graph.NodeID("repo::b.go::B")
	n3 := graph.NodeID("repo::c.go::C")
	n4 := graph.NodeID("repo::d.go::D")

	// Auto: namematcher, not confirmed.
	if _, err := st.SaveSyntheticEdge(n1, n2, graph.EdgeMentions, 0.7); err != nil {
		t.Fatalf("SaveSyntheticEdge: %v", err)
	}
	// Manual: user via link_entities.
	if _, err := st.SaveManualEdge(n1, n3, string(graph.EdgeDeploys), "infra", "agent-x", 1.0, true); err != nil {
		t.Fatalf("SaveManualEdge manual: %v", err)
	}
	// Confirmed: started as auto, then human-approved.
	if _, err := st.SaveSyntheticEdge(n2, n4, graph.EdgeMentions, 0.8); err != nil {
		t.Fatalf("SaveSyntheticEdge (pre-confirm): %v", err)
	}
	if err := st.ConfirmEdge(n2, n4, string(graph.EdgeMentions), true); err != nil {
		t.Fatalf("ConfirmEdge: %v", err)
	}
	// Suppressed: must not appear in any bucket.
	if _, err := st.SaveManualEdge(n3, n4, string(graph.EdgeConsumes), "api", "agent-x", 0.6, true); err != nil {
		t.Fatalf("SaveManualEdge suppressed: %v", err)
	}
	if err := st.ConfirmEdge(n3, n4, string(graph.EdgeConsumes), false); err != nil {
		t.Fatalf("ConfirmEdge(suppress): %v", err)
	}

	auto, confirmed, manual, err := st.CrossDomainEdgeStats()
	if err != nil {
		t.Fatalf("CrossDomainEdgeStats: %v", err)
	}
	if auto != 1 {
		t.Errorf("expected auto=1, got %d", auto)
	}
	if confirmed != 1 {
		t.Errorf("expected confirmed=1, got %d", confirmed)
	}
	if manual != 1 {
		t.Errorf("expected manual=1, got %d", manual)
	}
}

// TestCrossDomainEdgeStats_AllSuppressed verifies that when every edge is
// suppressed the function returns (0, 0, 0) — not a count of suppressed rows.
func TestCrossDomainEdgeStats_AllSuppressed(t *testing.T) {
	st := openFromTemplate(t)

	n1 := graph.NodeID("repo::a.go::A")
	n2 := graph.NodeID("repo::b.go::B")

	if _, err := st.SaveSyntheticEdge(n1, n2, graph.EdgeMentions, 0.7); err != nil {
		t.Fatalf("SaveSyntheticEdge: %v", err)
	}
	if err := st.ConfirmEdge(n1, n2, string(graph.EdgeMentions), false); err != nil {
		t.Fatalf("ConfirmEdge(suppress): %v", err)
	}

	auto, confirmed, manual, err := st.CrossDomainEdgeStats()
	if err != nil {
		t.Fatalf("CrossDomainEdgeStats: %v", err)
	}
	if auto+confirmed+manual != 0 {
		t.Errorf("expected all zeros when all edges suppressed, got auto=%d confirmed=%d manual=%d",
			auto, confirmed, manual)
	}
}

// TestCrossDomainEdgeStats_CustomRelation verifies that user-created edges
// with non-catalog relation strings (e.g. "REFERENCES") count as manual.
func TestCrossDomainEdgeStats_CustomRelation(t *testing.T) {
	st := openFromTemplate(t)

	n1 := graph.NodeID("repo::a.go::A")
	n2 := graph.NodeID("repo::b.go::B")

	if _, err := st.SaveManualEdge(n1, n2, "REFERENCES", "docs", "agent-x", 1.0, true); err != nil {
		t.Fatalf("SaveManualEdge custom: %v", err)
	}

	_, _, manual, err := st.CrossDomainEdgeStats()
	if err != nil {
		t.Fatalf("CrossDomainEdgeStats: %v", err)
	}
	if manual != 1 {
		t.Errorf("expected custom-relation edge counted as manual=1, got %d", manual)
	}
}
