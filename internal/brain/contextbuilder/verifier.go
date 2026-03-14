package contextbuilder

import (
	"fmt"
	"strings"
)

// verifyPacket is the SIL Verification Pass (Phase 3).
//
// After the LLM generates Insight and Concerns, this pass deterministically
// checks each claim against the graph topology snapshot in req. Claims that
// can be verified are annotated with [✓]; claims that are contradicted by
// the graph data are annotated with [⚠ UNVERIFIED: actual value].
//
// If the Insight text contains a contradicted claim, a warning is appended
// to pkt.GraphWarnings rather than modifying the insight text in-place
// (the insight is an LLM paragraph; character-level injection is fragile).
//
// This pass is deterministic, has zero latency cost (no LLM), and is safe
// to run even when the graph snapshot is incomplete — it only annotates
// when it has enough data to be confident.
func verifyPacket(pkt *Packet, req Request) {
	if pkt == nil {
		return
	}

	topo := buildTopo(req)

	// Verify each concern in-place.
	for i, c := range pkt.Concerns {
		pkt.Concerns[i] = verifyClaim(c, topo)
	}

	// Verify insight — contradictions go to GraphWarnings (don't corrupt LLM text).
	if pkt.Insight != "" {
		if warn := insightContradictions(pkt.Insight, topo); warn != "" {
			pkt.GraphWarnings = append(pkt.GraphWarnings, warn)
		}
	}
}

// topoSnapshot holds pre-computed topology values derived from a Request.
// Built once per verify pass to avoid repeated computation.
type topoSnapshot struct {
	fanIn        int
	calleeCount  int
	callerCount  int
	hasTests     bool
	hasCycle     bool // bidirectional edge detected in ego-graph
	isOrphan     bool // fanIn == 0 && calleeCount == 0
	isHighFanIn  bool // fanIn > 5
	isHighCallee bool // calleeCount > 5
	rootFile     string
}

func buildTopo(req Request) topoSnapshot {
	fanIn := req.FanIn
	if fanIn == 0 {
		fanIn = len(req.CallerNames)
	}

	// Detect cycle heuristic: if any callee of root is also a direct caller,
	// there is a bidirectional edge — a strong cycle signal in the ego-graph.
	calleeSet := make(map[string]bool, len(req.CalleeNames))
	for _, c := range req.CalleeNames {
		calleeSet[c] = true
	}
	hasCycle := false
	for _, caller := range req.CallerNames {
		if calleeSet[caller] {
			hasCycle = true
			break
		}
	}

	calleeCount := len(req.CalleeNames)

	return topoSnapshot{
		fanIn:        fanIn,
		calleeCount:  calleeCount,
		callerCount:  len(req.CallerNames),
		hasTests:     req.HasTests,
		hasCycle:     hasCycle,
		isOrphan:     fanIn == 0 && calleeCount == 0,
		isHighFanIn:  fanIn > 5,
		isHighCallee: calleeCount > 5,
		rootFile:     req.RootFile,
	}
}

// verifyClaim annotates a single concern string based on topology.
// Returns the original string with [✓] appended if confirmed, or
// [⚠ UNVERIFIED: ...] appended if contradicted by graph data.
// Returns the original string unchanged if the claim is not verifiable.
func verifyClaim(concern string, t topoSnapshot) string {
	lower := strings.ToLower(concern)

	// --- Orphan / isolated claim ---
	if containsAny(lower, "orphan", "isolated", "no callers", "no dependencies", "unreachable") {
		if t.isOrphan {
			return concern + " [✓]"
		}
		if t.fanIn > 0 || t.calleeCount > 0 {
			return concern + fmt.Sprintf(" [⚠ UNVERIFIED: node has %d caller(s) and %d callee(s)]",
				t.fanIn, t.calleeCount)
		}
	}

	// --- Gravity center / hub / high fan-in claim ---
	if containsAny(lower, "gravity center", "gravity", "hub", "highly connected", "central node", "high fan") {
		if t.isHighFanIn {
			return concern + fmt.Sprintf(" [✓ fanIn=%d]", t.fanIn)
		}
		if t.fanIn > 0 && !t.isHighFanIn {
			return concern + fmt.Sprintf(" [⚠ UNVERIFIED: fanIn is only %d, not >5]", t.fanIn)
		}
	}

	// --- High blast radius claim ---
	if containsAny(lower, "blast radius", "wide impact", "ripple effect", "breaking change") {
		if t.isHighFanIn {
			return concern + fmt.Sprintf(" [✓ fanIn=%d]", t.fanIn)
		}
		if t.fanIn > 0 && !t.isHighFanIn {
			return concern + fmt.Sprintf(" [⚠ UNVERIFIED: only %d caller(s) detected]", t.fanIn)
		}
	}

	// --- Cycle / circular dependency claim ---
	if containsAny(lower, "cycle", "circular", "circular dependency", "mutual dependency") {
		if t.hasCycle {
			return concern + " [✓ bidirectional edge detected in ego-graph]"
		}
		// Only annotate UNVERIFIED if we have both callers and callees to compare —
		// if one side is empty we simply don't have enough data to contradict.
		if t.callerCount > 0 && t.calleeCount > 0 && !t.hasCycle {
			return concern + " [⚠ UNVERIFIED: no bidirectional edge found in ego-graph]"
		}
	}

	// --- No test coverage claim ---
	if containsAny(lower, "no test", "untested", "missing test", "lacks test", "no coverage") {
		if !t.hasTests {
			return concern + " [✓ no test file found]"
		}
		if t.hasTests {
			return concern + " [⚠ UNVERIFIED: test file exists for this entity]"
		}
	}

	// --- High coupling / many dependencies claim ---
	if containsAny(lower, "tightly coupled", "high coupling", "many dependencies", "too many callees") {
		if t.isHighCallee {
			return concern + fmt.Sprintf(" [✓ %d direct callees]", t.calleeCount)
		}
		if t.calleeCount > 0 && !t.isHighCallee {
			return concern + fmt.Sprintf(" [⚠ UNVERIFIED: only %d direct callee(s)]", t.calleeCount)
		}
	}

	return concern // not verifiable with available data — leave unchanged
}

// insightContradictions checks the insight text for claims that are contradicted
// by graph topology. Returns a warning string to append to GraphWarnings, or ""
// if nothing is contradicted.
// Modifies the insight text directly would be fragile; appending to GraphWarnings
// keeps the original LLM output intact while surfacing the contradiction clearly.
func insightContradictions(insight string, t topoSnapshot) string {
	lower := strings.ToLower(insight)
	var contradictions []string

	if containsAny(lower, "orphan", "isolated", "unreachable") {
		if t.fanIn > 0 || t.calleeCount > 0 {
			contradictions = append(contradictions,
				fmt.Sprintf("orphan/isolated claim (actual: %d caller(s), %d callee(s))", t.fanIn, t.calleeCount))
		}
	}

	if containsAny(lower, "gravity center", "hub", "highly connected") {
		if !t.isHighFanIn && t.fanIn > 0 {
			contradictions = append(contradictions,
				fmt.Sprintf("hub/gravity claim (actual fanIn: %d, threshold: >5)", t.fanIn))
		}
	}

	if containsAny(lower, "no test", "untested") {
		if t.hasTests {
			contradictions = append(contradictions, "no-test claim (test file exists)")
		}
	}

	if len(contradictions) == 0 {
		return ""
	}
	return "⚠ INSIGHT UNVERIFIED claim(s): " + strings.Join(contradictions, "; ")
}

// containsAny returns true if s contains any of the given substrings.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
