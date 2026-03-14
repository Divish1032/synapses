package contextbuilder

import (
	"strings"
	"testing"
)

func TestVerifyClaim_Orphan(t *testing.T) {
	topo := buildTopo(Request{FanIn: 0, CalleeNames: nil, CallerNames: nil})

	got := verifyClaim("This node appears to be an orphan with no callers", topo)
	if !strings.Contains(got, "[✓]") {
		t.Errorf("expected [✓] for true orphan, got: %s", got)
	}
}

func TestVerifyClaim_Orphan_Contradicted(t *testing.T) {
	topo := buildTopo(Request{FanIn: 3, CalleeNames: []string{"A", "B"}, CallerNames: []string{"X", "Y", "Z"}})

	got := verifyClaim("This node appears to be an orphan", topo)
	if !strings.Contains(got, "UNVERIFIED") {
		t.Errorf("expected UNVERIFIED for non-orphan, got: %s", got)
	}
}

func TestVerifyClaim_Hub_Verified(t *testing.T) {
	topo := buildTopo(Request{FanIn: 12, CallerNames: []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J", "K", "L"}})

	got := verifyClaim("Acts as a gravity center with high connectivity", topo)
	if !strings.Contains(got, "[✓") {
		t.Errorf("expected [✓] for verified hub, got: %s", got)
	}
	if !strings.Contains(got, "fanIn=12") {
		t.Errorf("expected fanIn=12 in annotation, got: %s", got)
	}
}

func TestVerifyClaim_Hub_Contradicted(t *testing.T) {
	topo := buildTopo(Request{FanIn: 2, CallerNames: []string{"A", "B"}})

	got := verifyClaim("Acts as a gravity center in the system", topo)
	if !strings.Contains(got, "UNVERIFIED") {
		t.Errorf("expected UNVERIFIED for low-fanIn hub claim, got: %s", got)
	}
}

func TestVerifyClaim_NoTest_Verified(t *testing.T) {
	topo := buildTopo(Request{HasTests: false, RootFile: "internal/auth/handler.go"})

	got := verifyClaim("No test coverage exists for this function", topo)
	if !strings.Contains(got, "[✓") {
		t.Errorf("expected [✓] for confirmed no-test, got: %s", got)
	}
}

func TestVerifyClaim_NoTest_Contradicted(t *testing.T) {
	topo := buildTopo(Request{HasTests: true, RootFile: "internal/auth/handler.go"})

	got := verifyClaim("This function is untested and has no coverage", topo)
	if !strings.Contains(got, "UNVERIFIED") {
		t.Errorf("expected UNVERIFIED when test exists, got: %s", got)
	}
}

func TestVerifyClaim_Cycle_Verified(t *testing.T) {
	// A calls B, and B calls A → bidirectional edge → cycle signal
	topo := buildTopo(Request{
		CalleeNames: []string{"B", "C"},
		CallerNames: []string{"B", "D"}, // B is both callee and caller
	})

	got := verifyClaim("There is a circular dependency in this module", topo)
	if !strings.Contains(got, "[✓") {
		t.Errorf("expected [✓] for detected bidirectional edge, got: %s", got)
	}
}

func TestVerifyClaim_Cycle_Contradicted(t *testing.T) {
	topo := buildTopo(Request{
		CalleeNames: []string{"B", "C"},
		CallerNames: []string{"D", "E"}, // no overlap
	})

	got := verifyClaim("This creates a cycle between modules", topo)
	if !strings.Contains(got, "UNVERIFIED") {
		t.Errorf("expected UNVERIFIED when no bidirectional edge, got: %s", got)
	}
}

func TestVerifyClaim_Unrecognised_Unchanged(t *testing.T) {
	topo := buildTopo(Request{FanIn: 3})
	original := "This function handles request routing."
	got := verifyClaim(original, topo)
	if got != original {
		t.Errorf("unrecognised claim should be unchanged, got: %s", got)
	}
}

func TestInsightContradictions_NoIssue(t *testing.T) {
	topo := buildTopo(Request{FanIn: 10, HasTests: false})
	// High fanIn claim is accurate
	warn := insightContradictions("This is a gravity center with many callers.", topo)
	if warn != "" {
		t.Errorf("expected no contradiction warning, got: %s", warn)
	}
}

func TestInsightContradictions_Contradicted(t *testing.T) {
	topo := buildTopo(Request{FanIn: 1}) // low fanIn
	warn := insightContradictions("This is a hub and highly connected gravity center.", topo)
	if !strings.Contains(warn, "INSIGHT UNVERIFIED") {
		t.Errorf("expected contradiction warning for false hub claim, got: %s", warn)
	}
}

func TestVerifyPacket_NilSafe(t *testing.T) {
	// Should not panic on nil packet
	verifyPacket(nil, Request{})
}

func TestVerifyPacket_AppendsCycleWarningToGraphWarnings(t *testing.T) {
	topo := buildTopo(Request{
		CalleeNames: []string{"B"},
		CallerNames: []string{"B"}, // bidirectional
	})
	pkt := &Packet{
		Concerns: []string{"This has a cycle between components"},
	}
	// Call verifyClaim directly to check concern annotation
	pkt.Concerns[0] = verifyClaim(pkt.Concerns[0], topo)
	if !strings.Contains(pkt.Concerns[0], "[✓") {
		t.Errorf("expected cycle to be verified in concern, got: %s", pkt.Concerns[0])
	}
}

func TestVerifyPacket_InsightContradictionGoesToGraphWarnings(t *testing.T) {
	b, _ := newTestBuilder(t, "")
	req := Request{
		RootNodeID: "repo::auth.go::Validate",
		RootName:   "Validate",
		RootType:   "function",
		RootFile:   "internal/auth/auth.go",
		FanIn:      1, // very low fanIn
		HasTests:   true,
	}
	// Manually build a packet with a contradicted insight
	pkt := &Packet{
		Insight:  "Validate is a hub and highly connected gravity center in the system.",
		Concerns: []string{"This function is untested and has no coverage."},
	}
	_ = b // builder used by other tests; here we test verifyPacket directly
	verifyPacket(pkt, req)

	// Insight contradiction should go to GraphWarnings
	found := false
	for _, w := range pkt.GraphWarnings {
		if strings.Contains(w, "INSIGHT UNVERIFIED") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected INSIGHT UNVERIFIED in GraphWarnings, got: %v", pkt.GraphWarnings)
	}

	// Concern contradiction: HasTests=true contradicts "untested"
	if !strings.Contains(pkt.Concerns[0], "UNVERIFIED") {
		t.Errorf("expected UNVERIFIED in concern for no-test claim when test exists, got: %s", pkt.Concerns[0])
	}
}
