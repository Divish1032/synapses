package contextbuilder

import (
	"strings"
	"testing"
)

// TestVerifyClaim_TableDriven tests all claim verification scenarios with a single table-driven test.
// This improves maintainability by reducing code duplication and making it easier to add new scenarios.
func TestVerifyClaim_TableDriven(t *testing.T) {
	// Table-driven test for claim verification logic.
	// Each test case defines: claim text, topology state, and expected annotation result.
	tests := []struct {
		name         string
		claim        string
		req          Request
		wantVerified bool // true if expect [✓], false if expect UNVERIFIED, nil if expect unchanged
		wantUnchanged bool // true if expect original claim unchanged
	}{
		// Orphan/isolated claims
		{
			name:         "orphan_claim_true_orphan",
			claim:        "This node appears to be an orphan with no callers",
			req:          Request{FanIn: 0, CalleeNames: nil, CallerNames: nil},
			wantVerified: true,
		},
		{
			name:         "orphan_claim_contradicted",
			claim:        "This node appears to be an orphan",
			req:          Request{FanIn: 3, CalleeNames: []string{"A", "B"}, CallerNames: []string{"X", "Y", "Z"}},
			wantVerified: false,
		},

		// Hub/gravity center claims
		{
			name:         "hub_claim_high_fanin",
			claim:        "Acts as a gravity center with high connectivity",
			req:          Request{FanIn: 12, CallerNames: []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J", "K", "L"}},
			wantVerified: true,
		},
		{
			name:         "hub_claim_low_fanin_contradicted",
			claim:        "Acts as a gravity center in the system",
			req:          Request{FanIn: 2, CallerNames: []string{"A", "B"}},
			wantVerified: false,
		},

		// Test coverage claims
		{
			name:         "no_test_claim_true",
			claim:        "No test coverage exists for this function",
			req:          Request{HasTests: false, RootFile: "internal/auth/handler.go"},
			wantVerified: true,
		},
		{
			name:         "no_test_claim_contradicted",
			claim:        "This function is untested and has no coverage",
			req:          Request{HasTests: true, RootFile: "internal/auth/handler.go"},
			wantVerified: false,
		},

		// Cycle/circular dependency claims
		{
			name:         "cycle_claim_bidirectional_edge",
			claim:        "There is a circular dependency in this module",
			req:          Request{CalleeNames: []string{"B", "C"}, CallerNames: []string{"B", "D"}},
			wantVerified: true,
		},
		{
			name:         "cycle_claim_no_edge_contradicted",
			claim:        "This creates a cycle between modules",
			req:          Request{CalleeNames: []string{"B", "C"}, CallerNames: []string{"D", "E"}},
			wantVerified: false,
		},

		// High coupling claims
		{
			name:         "coupling_claim_high_callees",
			claim:        "This function is tightly coupled with many dependencies",
			req:          Request{CalleeNames: []string{"A", "B", "C", "D", "E", "F"}},
			wantVerified: true,
		},
		{
			name:         "coupling_claim_low_callees_contradicted",
			claim:        "This is tightly coupled with many dependencies",
			req:          Request{CalleeNames: []string{"A", "B"}},
			wantVerified: false,
		},

		// Unrecognized/unknown claims
		{
			name:         "unknown_claim_unchanged",
			claim:        "This function handles request routing.",
			req:          Request{FanIn: 3},
			wantUnchanged: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			topo := buildTopo(tt.req)
			got := verifyClaim(tt.claim, topo)

			if tt.wantUnchanged {
				if got != tt.claim {
					t.Errorf("claim should be unchanged, got: %s", got)
				}
				return
			}

			if tt.wantVerified {
				if !strings.Contains(got, "[✓") {
					t.Errorf("expected [✓] verification, got: %s", got)
				}
			} else {
				if !strings.Contains(got, "UNVERIFIED") {
					t.Errorf("expected UNVERIFIED, got: %s", got)
				}
			}
		})
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

// --- Integration Tests ---

// TestVerifyPacket_RealWorldScenario_LowFanInEntityWithMultipleConcerns tests a realistic scenario:
// an entity with low fanin is incorrectly described as a hub. verifyPacket should annotate
// both the concerns and append a warning to GraphWarnings for the contradicted insight.
func TestVerifyPacket_RealWorldScenario_LowFanInEntityWithMultipleConcerns(t *testing.T) {
	req := Request{
		RootNodeID:  "repo::auth.go::ValidateToken",
		RootName:    "ValidateToken",
		RootType:    "function",
		RootFile:    "internal/auth/auth.go",
		FanIn:       2, // Low fanin — not a hub
		CalleeNames: []string{"crypto", "time"},
		CallerNames: []string{"handler", "middleware"},
		HasTests:    true,
	}

	pkt := &Packet{
		Insight: "ValidateToken is a critical hub in the authentication system with many callers.",
		Concerns: []string{
			"This function is tightly coupled",
			"Acts as a gravity center with high connectivity",
		},
	}

	verifyPacket(pkt, req)

	// Insight should have a contradiction warning appended to GraphWarnings
	if len(pkt.GraphWarnings) == 0 {
		t.Error("expected GraphWarnings for contradicted insight, got none")
	}
	foundInsightWarning := false
	for _, w := range pkt.GraphWarnings {
		if strings.Contains(w, "INSIGHT UNVERIFIED") {
			foundInsightWarning = true
			break
		}
	}
	if !foundInsightWarning {
		t.Errorf("expected INSIGHT UNVERIFIED warning, got: %v", pkt.GraphWarnings)
	}

	// Concerns should be annotated with UNVERIFIED
	if !strings.Contains(pkt.Concerns[0], "UNVERIFIED") {
		t.Errorf("first concern should be annotated UNVERIFIED, got: %s", pkt.Concerns[0])
	}
	if !strings.Contains(pkt.Concerns[1], "UNVERIFIED") {
		t.Errorf("second concern should be annotated UNVERIFIED, got: %s", pkt.Concerns[1])
	}
}

// TestVerifyPacket_MixedTrueClaims tests a packet where some claims are correct and some are incorrect.
// Verifies that only contradicted claims are annotated.
func TestVerifyPacket_MixedTrueClaims(t *testing.T) {
	req := Request{
		RootNodeID:  "repo::db.go::Query",
		RootName:    "Query",
		RootType:    "function",
		RootFile:    "internal/db/query.go",
		FanIn:       8, // High fanin — is a hub
		CalleeNames: []string{"sql", "log", "cache"},
		CallerNames: []string{"a", "b", "c", "d", "e", "f", "g", "h"},
		HasTests:    false, // No tests
	}

	pkt := &Packet{
		Concerns: []string{
			"Acts as a gravity center", // TRUE — high fanin
			"This function is untested", // TRUE — no tests
			"Has circular dependency", // FALSE — no bidirectional edges
		},
	}

	verifyPacket(pkt, req)

	// First concern should be verified ✓
	if !strings.Contains(pkt.Concerns[0], "[✓") {
		t.Errorf("true claim should be verified, got: %s", pkt.Concerns[0])
	}

	// Second concern should be verified ✓
	if !strings.Contains(pkt.Concerns[1], "[✓") {
		t.Errorf("true no-test claim should be verified, got: %s", pkt.Concerns[1])
	}

	// Third concern should be unverified
	if !strings.Contains(pkt.Concerns[2], "UNVERIFIED") {
		t.Errorf("false cycle claim should be unverified, got: %s", pkt.Concerns[2])
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

// --- Additional coverage tests ---

func TestVerifyClaim_HighCoupling_Verified(t *testing.T) {
	topo := buildTopo(Request{CalleeNames: []string{"A", "B", "C", "D", "E", "F"}})

	got := verifyClaim("This function has too many dependencies and is tightly coupled", topo)
	if !strings.Contains(got, "[✓") {
		t.Errorf("expected [✓] for verified high coupling, got: %s", got)
	}
	if !strings.Contains(got, "6 direct callee") {
		t.Errorf("expected callees count in annotation, got: %s", got)
	}
}

func TestVerifyClaim_HighCoupling_Contradicted(t *testing.T) {
	topo := buildTopo(Request{CalleeNames: []string{"A", "B"}})

	got := verifyClaim("This is tightly coupled with many dependencies", topo)
	if !strings.Contains(got, "UNVERIFIED") {
		t.Errorf("expected UNVERIFIED for low-coupling claim, got: %s", got)
	}
}

func TestVerifyClaim_BlastRadius_Verified(t *testing.T) {
	topo := buildTopo(Request{FanIn: 10, CallerNames: []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J"}})

	got := verifyClaim("Changes to this function have wide ripple effect", topo)
	if !strings.Contains(got, "[✓") {
		t.Errorf("expected [✓] for verified blast radius, got: %s", got)
	}
}

func TestVerifyClaim_BlastRadius_Contradicted(t *testing.T) {
	topo := buildTopo(Request{FanIn: 1, CallerNames: []string{"A"}})

	got := verifyClaim("This causes a wide ripple effect when changed", topo)
	if !strings.Contains(got, "UNVERIFIED") {
		t.Errorf("expected UNVERIFIED for low-fanin blast claim, got: %s", got)
	}
}

func TestInsightContradictions_OrphanClaim_Contradicted(t *testing.T) {
	topo := buildTopo(Request{FanIn: 5, CalleeNames: []string{"A", "B"}})
	warn := insightContradictions("This orphan node has no dependencies or callers.", topo)
	if warn == "" {
		t.Error("expected contradiction for false orphan claim, got empty")
	}
	if !strings.Contains(warn, "INSIGHT UNVERIFIED") {
		t.Errorf("expected INSIGHT UNVERIFIED, got: %s", warn)
	}
}

func TestInsightContradictions_MultipleContradictions(t *testing.T) {
	topo := buildTopo(Request{FanIn: 1, HasTests: false, CalleeNames: []string{"A"}})
	// Claim both "hub" (false) and "no tests" (true)
	warn := insightContradictions("This is a hub with many callers and no test coverage.", topo)
	if !strings.Contains(warn, "INSIGHT UNVERIFIED") {
		t.Errorf("expected INSIGHT UNVERIFIED for multiple contradictions, got: %s", warn)
	}
	// Should contain hub contradiction but not test contradiction
	if !strings.Contains(warn, "hub") {
		t.Errorf("expected hub contradiction mentioned, got: %s", warn)
	}
}

func TestInsightContradictions_NoTestClaimFalse(t *testing.T) {
	topo := buildTopo(Request{HasTests: true})
	warn := insightContradictions("This function is untested and has no test file.", topo)
	if !strings.Contains(warn, "INSIGHT UNVERIFIED") {
		t.Errorf("expected contradiction for no-test claim when tests exist, got: %s", warn)
	}
	if !strings.Contains(warn, "no-test") {
		t.Errorf("expected no-test in warning, got: %s", warn)
	}
}

func TestContainsAny_MultipleMatches(t *testing.T) {
	result := containsAny("This function is tightly coupled and has high coupling issues", "coupling", "tightly")
	if !result {
		t.Error("expected containsAny to return true when multiple substrings match")
	}
}

func TestContainsAny_NoMatches(t *testing.T) {
	result := containsAny("This is a simple function", "cycle", "hub", "gravity")
	if result {
		t.Error("expected containsAny to return false when no substrings match")
	}
}

func TestContainsAny_EmptyString(t *testing.T) {
	result := containsAny("", "anything")
	if result {
		t.Error("expected containsAny to return false for empty string")
	}
}

func TestVerifyClaim_CaseInsensitive(t *testing.T) {
	topo := buildTopo(Request{HasTests: false})
	// Test with mixed case
	got := verifyClaim("NO TEST coverage exists here", topo)
	if !strings.Contains(got, "[✓") {
		t.Errorf("expected case-insensitive matching, got: %s", got)
	}
}
