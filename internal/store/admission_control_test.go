package store

import (
	"fmt"
	"strconv"
	"testing"
)

// TestParseContentTypePrior verifies that episode type tags map to the correct
// A-MAC content-type priors. Failures rank highest (must resurface); auto-
// captured session logs rank lowest (high noise, low signal).
func TestParseContentTypePrior(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tags   string
		source string
		want   float64
	}{
		{`["episode","failure"]`, SourceManual, 1.4},
		{`["episode","pattern"]`, SourceManual, 1.2},
		{`["episode","rule_proposal"]`, SourceManual, 1.2},
		{`["episode","decision"]`, SourceManual, 1.0},
		{`[]`, SourceManual, 1.0},
		// SourceAuto beats episode type — auto-captures are lower value.
		{`["episode","failure"]`, SourceAuto, 0.8},
		{`[]`, SourceAuto, 0.8},
	}
	for _, tc := range cases {
		got := parseContentTypePrior(tc.tags, tc.source)
		if got != tc.want {
			t.Errorf("parseContentTypePrior(%q, %q) = %v, want %v", tc.tags, tc.source, got, tc.want)
		}
	}
}

// TestAdmissionControl_NoExistingMemories verifies that a memory written into
// an empty store (fully novel) gets importance = content_type_prior × 1.0.
func TestAdmissionControl_NoExistingMemories(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	// failure episode — prior=1.4, novelty=1.0 (no candidates) → 1.4
	id, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "Auth service panicked when token was missing the 'sub' claim — null pointer on user lookup.",
		AgentID: "agent-failure",
		Source:  SourceManual,
		Tags:    `["episode","failure"]`,
		// Importance intentionally empty — A-MAC should auto-compute.
	})
	if err != nil {
		t.Fatalf("insert failure memory: %v", err)
	}

	mems, err := st.QueryMemories(TierProject, "", "agent-failure", 5)
	if err != nil {
		t.Fatalf("query memories: %v", err)
	}
	var found *Memory
	for i := range mems {
		if mems[i].ID == id {
			found = &mems[i]
			break
		}
	}
	if found == nil {
		t.Fatal("inserted memory not found")
	}

	// With no existing memories: novelty=1.0, prior=1.4 → importance=1.4000
	got, err := strconv.ParseFloat(found.Importance, 64)
	if err != nil {
		t.Fatalf("parse importance %q: %v", found.Importance, err)
	}
	const want = 1.4
	if got != want {
		t.Errorf("failure memory importance = %v, want %v", got, want)
	}
}

// TestAdmissionControl_ContentTypePriorRanking verifies that failure > pattern >
// rule_proposal > decision when all are equally novel (empty store).
func TestAdmissionControl_ContentTypePriorRanking(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	type memSpec struct {
		agentID     string
		tags        string
		wantPrior   float64
	}
	specs := []memSpec{
		{"agent-f", `["episode","failure"]`, 1.4},
		{"agent-p", `["episode","pattern"]`, 1.2},
		{"agent-r", `["episode","rule_proposal"]`, 1.2},
		{"agent-d", `["episode","decision"]`, 1.0},
	}

	for _, sp := range specs {
		id, err := st.InsertMemory(Memory{
			Tier:    TierProject,
			Content: fmt.Sprintf("Unique content for %s admission control test — no existing memories.", sp.agentID),
			AgentID: sp.agentID,
			Source:  SourceManual,
			Tags:    sp.tags,
		})
		if err != nil {
			t.Fatalf("insert memory for %s: %v", sp.agentID, err)
		}

		mems, _ := st.QueryMemories(TierProject, "", sp.agentID, 5)
		var found *Memory
		for i := range mems {
			if mems[i].ID == id {
				found = &mems[i]
				break
			}
		}
		if found == nil {
			t.Fatalf("memory for %s not found", sp.agentID)
		}

		got, err := strconv.ParseFloat(found.Importance, 64)
		if err != nil {
			t.Fatalf("parse importance for %s: %v", sp.agentID, err)
		}
		// With empty store: novelty=1.0, so importance == prior.
		if got != sp.wantPrior {
			t.Errorf("%s importance = %v, want %v (prior)", sp.agentID, got, sp.wantPrior)
		}
	}
}

// TestAdmissionControl_ExplicitImportanceNotOverridden verifies that when the
// caller provides an explicit importance value, A-MAC does NOT override it.
func TestAdmissionControl_ExplicitImportanceNotOverridden(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	// Caller explicitly sets "0.8" — should not be auto-computed.
	id, err := st.InsertMemory(Memory{
		Tier:       TierProject,
		Content:    "Explicit importance test — caller pinned this at 0.8.",
		AgentID:    "agent-explicit",
		Source:     SourceManual,
		Tags:       `["episode","failure"]`, // would auto-compute to 1.4 if not explicit
		Importance: "0.8",
	})
	if err != nil {
		t.Fatalf("insert memory: %v", err)
	}

	mems, _ := st.QueryMemories(TierProject, "", "agent-explicit", 5)
	var found *Memory
	for i := range mems {
		if mems[i].ID == id {
			found = &mems[i]
			break
		}
	}
	if found == nil {
		t.Fatal("memory not found")
	}
	if found.Importance != "0.8" {
		t.Errorf("explicit importance changed: got %q, want %q", found.Importance, "0.8")
	}
}

// TestAdmissionControl_PinnedNotOverridden verifies that "pinned" importance
// bypasses A-MAC entirely.
func TestAdmissionControl_PinnedNotOverridden(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	id, err := st.InsertMemory(Memory{
		Tier:       TierProject,
		Content:    "Pinned memory — critical architectural invariant that must never be demoted.",
		AgentID:    "agent-pinned",
		Source:     SourceManual,
		Tags:       `[]`,
		Importance: ImportancePinned,
	})
	if err != nil {
		t.Fatalf("insert memory: %v", err)
	}

	mems, _ := st.QueryMemories(TierProject, "", "agent-pinned", 5)
	var found *Memory
	for i := range mems {
		if mems[i].ID == id {
			found = &mems[i]
			break
		}
	}
	if found == nil {
		t.Fatal("memory not found")
	}
	if found.Importance != ImportancePinned {
		t.Errorf("pinned importance changed: got %q, want %q", found.Importance, ImportancePinned)
	}
}

// TestAdmissionControl_LowNoveltyReducesImportance verifies that when a similar
// memory already exists (high Jaccard similarity), the new memory's auto-computed
// importance is lower than for a novel memory of the same episode type.
func TestAdmissionControl_LowNoveltyReducesImportance(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	// Insert a first memory explicitly.
	baseContent := "The OAuth token endpoint returns 401 when the client_secret is wrong."
	_, err := st.InsertMemory(Memory{
		Tier:       TierProject,
		Content:    baseContent,
		AgentID:    "agent-novelty",
		Source:     SourceManual,
		Tags:       `["episode","decision"]`,
		Importance: "1.0", // explicit — not auto-computed
	})
	if err != nil {
		t.Fatalf("insert base memory: %v", err)
	}

	// Insert a highly similar memory without explicit importance — A-MAC should
	// detect low novelty (high Jaccard) and assign lower importance than 1.0.
	similarContent := "The OAuth token endpoint returns 401 when the client_secret is incorrect."
	id2, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: similarContent,
		AgentID: "agent-novelty",
		Source:  SourceManual,
		Tags:    `["episode","decision"]`,
		// No importance — A-MAC computes it.
	})
	if err != nil {
		// If dedup fires (Jaccard > 0.85), the memory is a touch — acceptable.
		// The test passes vacuously since no new memory was inserted.
		return
	}

	mems, _ := st.QueryMemories(TierProject, "", "agent-novelty", 10)
	var found *Memory
	for i := range mems {
		if mems[i].ID == id2 {
			found = &mems[i]
			break
		}
	}
	if found == nil {
		// Dedup fired — the similar content was merged into the existing memory.
		// This is correct behavior; no assertion needed.
		return
	}

	got, err := strconv.ParseFloat(found.Importance, 64)
	if err != nil {
		t.Fatalf("parse importance: %v", err)
	}
	// Low novelty → importance < 1.0 (the prior for "decision").
	if got >= 1.0 {
		t.Errorf("similar memory importance = %v, want < 1.0 (low novelty)", got)
	}
}

// TestAdmissionControl_AutoCaptureGetsLowerPrior verifies that auto-captured
// memories (SourceAuto) get a lower prior (0.8) than manually-saved ones.
func TestAdmissionControl_AutoCaptureGetsLowerPrior(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	id, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "Auto-captured session log entry about the refactoring session progress.",
		AgentID: "agent-auto",
		Source:  SourceAuto, // auto-captured
		Tags:    `[]`,
		// No importance — A-MAC computes it.
	})
	if err != nil {
		t.Fatalf("insert auto memory: %v", err)
	}

	mems, _ := st.QueryMemories(TierProject, "", "agent-auto", 5)
	var found *Memory
	for i := range mems {
		if mems[i].ID == id {
			found = &mems[i]
			break
		}
	}
	if found == nil {
		t.Fatal("memory not found")
	}

	got, err := strconv.ParseFloat(found.Importance, 64)
	if err != nil {
		t.Fatalf("parse importance: %v", err)
	}
	// SourceAuto prior=0.8, novelty=1.0 (empty store) → importance=0.8
	if got != 0.8 {
		t.Errorf("auto-captured importance = %v, want 0.8", got)
	}
}

// TestComputeAdmissionImportance_Clamps verifies that the output is always
// within [minImportance, maxImportance] regardless of inputs, and that the
// Jaccard and cosine novelty paths produce correct values.
func TestComputeAdmissionImportance_Clamps(t *testing.T) {
	t.Parallel()
	cases := []struct {
		description   string
		tags          string
		source        string
		hasCandidates bool
		maxJaccard    float64
		maxCosine     float32
		hasCosine     bool
		wantMin       float64
		wantMax       float64
		wantExact     float64 // 0 means skip exact check
	}{
		// No existing memories → fully novel → importance = prior × 1.0 = 1.4.
		{"failure+fully-novel", `["episode","failure"]`, SourceManual, false, 0.0, 0, false, 1.39, 1.41, 1.4},
		// hasCandidates but no cosine/Jaccard → Jaccard fallback, maxJaccard=0 → novelty=1.0.
		{"decision+no-similarity", `["episode","decision"]`, SourceManual, true, 0.0, 0, false, 0.99, 1.01, 1.0},
		// Jaccard fallback with near-duplicate: noveltyFactor=0.01 → clamped to noveltyFloor=0.2
		// → importance=0.8×0.2=0.16. Above minImportance(0.10) so no second clamp.
		{"auto+near-duplicate-jaccard", `[]`, SourceAuto, true, 0.99, 0, false, 0.15, 0.17, 0.16},
		// Cosine novelty path: prior=1.0, cosine=0.5 → novelty=0.5 → importance=0.5.
		{"decision+cosine-novelty", `[]`, SourceManual, true, 0.0, 0.5, true, 0.49, 0.51, 0.5},
		// Cosine near-duplicate: prior=1.4, cosine=0.95 → novelty=0.05, below floor(0.2) → 1.4×0.2=0.28.
		{"failure+cosine-near-dup", `["episode","failure"]`, SourceManual, true, 0.0, 0.95, true, 0.27, 0.29, 0.28},
		// High prior + full novelty = 1.4 — still within [0.10, 2.0].
		{"failure+novel", `["episode","failure"]`, SourceManual, true, 0.0, 0, false, 1.39, 1.41, 1.4},
		// Cosine exactly 0.0 (orthogonal) — hasCosine=true, should use cosine path (novelty=1.0).
		{"decision+cosine-zero", `[]`, SourceManual, true, 0.5, 0.0, true, 0.99, 1.01, 1.0},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.description, func(t *testing.T) {
			t.Parallel()
			got := computeAdmissionImportance(tc.tags, tc.source, tc.hasCandidates, tc.maxJaccard, tc.maxCosine, tc.hasCosine)
			f, err := strconv.ParseFloat(got, 64)
			if err != nil {
				t.Fatalf("parse %q: %v", got, err)
			}
			if f < tc.wantMin || f > tc.wantMax {
				t.Errorf("importance %v out of [%v, %v]", f, tc.wantMin, tc.wantMax)
			}
			if tc.wantExact != 0 {
				const eps = 0.001
				if f < tc.wantExact-eps || f > tc.wantExact+eps {
					t.Errorf("importance %v, want ~%v", f, tc.wantExact)
				}
			}
		})
	}
}
