package sdlc

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// SectionsForPhase
// ---------------------------------------------------------------------------

func TestSectionsForPhase_Planning(t *testing.T) {
	f := SectionsForPhase(PhasePlanning)

	// Expected true fields per source comment matrix.
	trueFields := map[string]bool{
		"RootSummary":         f.RootSummary,
		"DependencySummaries": f.DependencySummaries,
		"LLMInsight":          f.LLMInsight,
		"TeamStatus":          f.TeamStatus,
		"PhaseGuidance":       f.PhaseGuidance,
	}
	for name, got := range trueFields {
		if !got {
			t.Errorf("planning: %s should be true", name)
		}
	}

	// Expected false fields.
	falseFields := map[string]bool{
		"ActiveConstraints": f.ActiveConstraints,
		"QualityGate":       f.QualityGate,
		"PatternHints":      f.PatternHints,
	}
	for name, got := range falseFields {
		if got {
			t.Errorf("planning: %s should be false", name)
		}
	}
}

func TestSectionsForPhase_Development(t *testing.T) {
	f := SectionsForPhase(PhaseDevelopment)

	// All sections should be true in development.
	all := map[string]bool{
		"RootSummary":         f.RootSummary,
		"DependencySummaries": f.DependencySummaries,
		"LLMInsight":          f.LLMInsight,
		"ActiveConstraints":   f.ActiveConstraints,
		"TeamStatus":          f.TeamStatus,
		"QualityGate":         f.QualityGate,
		"PatternHints":        f.PatternHints,
		"PhaseGuidance":       f.PhaseGuidance,
	}
	for name, got := range all {
		if !got {
			t.Errorf("development: %s should be true", name)
		}
	}
}

func TestSectionsForPhase_Testing(t *testing.T) {
	f := SectionsForPhase(PhaseTesting)

	trueFields := map[string]bool{
		"RootSummary":       f.RootSummary,
		"ActiveConstraints": f.ActiveConstraints,
		"TeamStatus":        f.TeamStatus,
		"QualityGate":       f.QualityGate,
		"PhaseGuidance":     f.PhaseGuidance,
	}
	for name, got := range trueFields {
		if !got {
			t.Errorf("testing: %s should be true", name)
		}
	}

	falseFields := map[string]bool{
		"DependencySummaries": f.DependencySummaries,
		"LLMInsight":          f.LLMInsight,
		"PatternHints":        f.PatternHints,
	}
	for name, got := range falseFields {
		if got {
			t.Errorf("testing: %s should be false", name)
		}
	}
}

func TestSectionsForPhase_Review(t *testing.T) {
	f := SectionsForPhase(PhaseReview)

	// All sections are true in review.
	all := map[string]bool{
		"RootSummary":         f.RootSummary,
		"DependencySummaries": f.DependencySummaries,
		"LLMInsight":          f.LLMInsight,
		"ActiveConstraints":   f.ActiveConstraints,
		"TeamStatus":          f.TeamStatus,
		"QualityGate":         f.QualityGate,
		"PatternHints":        f.PatternHints,
		"PhaseGuidance":       f.PhaseGuidance,
	}
	for name, got := range all {
		if !got {
			t.Errorf("review: %s should be true", name)
		}
	}
}

func TestSectionsForPhase_Deployment(t *testing.T) {
	f := SectionsForPhase(PhaseDeployment)

	trueFields := map[string]bool{
		"RootSummary":   f.RootSummary,
		"TeamStatus":    f.TeamStatus,
		"PhaseGuidance": f.PhaseGuidance,
	}
	for name, got := range trueFields {
		if !got {
			t.Errorf("deployment: %s should be true", name)
		}
	}

	falseFields := map[string]bool{
		"DependencySummaries": f.DependencySummaries,
		"LLMInsight":          f.LLMInsight,
		"ActiveConstraints":   f.ActiveConstraints,
		"QualityGate":         f.QualityGate,
		"PatternHints":        f.PatternHints,
	}
	for name, got := range falseFields {
		if got {
			t.Errorf("deployment: %s should be false", name)
		}
	}
}

func TestSectionsForPhase_UnknownPhase_ShowsAll(t *testing.T) {
	// Unknown phases return a safe default with everything enabled.
	f := SectionsForPhase("unknown-phase")

	all := map[string]bool{
		"RootSummary":         f.RootSummary,
		"DependencySummaries": f.DependencySummaries,
		"LLMInsight":          f.LLMInsight,
		"ActiveConstraints":   f.ActiveConstraints,
		"TeamStatus":          f.TeamStatus,
		"QualityGate":         f.QualityGate,
		"PatternHints":        f.PatternHints,
		"PhaseGuidance":       f.PhaseGuidance,
	}
	for name, got := range all {
		if !got {
			t.Errorf("unknown phase: %s should be true (safe default shows everything)", name)
		}
	}
}

func TestSectionsForPhase_EmptyString_ShowsAll(t *testing.T) {
	f := SectionsForPhase("")
	all := map[string]bool{
		"RootSummary":         f.RootSummary,
		"DependencySummaries": f.DependencySummaries,
		"LLMInsight":          f.LLMInsight,
		"ActiveConstraints":   f.ActiveConstraints,
		"TeamStatus":          f.TeamStatus,
		"QualityGate":         f.QualityGate,
		"PatternHints":        f.PatternHints,
		"PhaseGuidance":       f.PhaseGuidance,
	}
	for name, got := range all {
		if !got {
			t.Errorf("empty phase: %s should be true (safe default)", name)
		}
	}
}

// ---------------------------------------------------------------------------
// GateForMode
// ---------------------------------------------------------------------------

func gateIsEmpty(g Gate) bool {
	return !g.RequireTests && !g.RequireDocs && !g.RequirePRCheck && len(g.Checklist) == 0
}

func TestGateForMode_Planning_ReturnsEmpty(t *testing.T) {
	for _, mode := range []string{ModeQuick, ModeStandard, ModeEnterprise} {
		g := GateForMode(mode, PhasePlanning)
		if !gateIsEmpty(g) {
			t.Errorf("mode=%q phase=planning: expected empty Gate{}, got %+v", mode, g)
		}
	}
}

func TestGateForMode_Deployment_ReturnsEmpty(t *testing.T) {
	for _, mode := range []string{ModeQuick, ModeStandard, ModeEnterprise} {
		g := GateForMode(mode, PhaseDeployment)
		if !gateIsEmpty(g) {
			t.Errorf("mode=%q phase=deployment: expected empty Gate{}, got %+v", mode, g)
		}
	}
}

func TestGateForMode_Quick_Development(t *testing.T) {
	g := GateForMode(ModeQuick, PhaseDevelopment)
	if g.RequireTests {
		t.Error("quick+development: RequireTests should be false")
	}
	if g.RequireDocs {
		t.Error("quick+development: RequireDocs should be false")
	}
	if g.RequirePRCheck {
		t.Error("quick+development: RequirePRCheck should be false")
	}
	if len(g.Checklist) == 0 {
		t.Error("quick+development: Checklist should be non-empty")
	}
}

func TestGateForMode_Quick_Testing(t *testing.T) {
	g := GateForMode(ModeQuick, PhaseTesting)
	if g.RequireTests {
		t.Error("quick+testing: RequireTests should be false")
	}
	if g.RequireDocs {
		t.Error("quick+testing: RequireDocs should be false")
	}
}

func TestGateForMode_Quick_Review(t *testing.T) {
	g := GateForMode(ModeQuick, PhaseReview)
	if g.RequireTests {
		t.Error("quick+review: RequireTests should be false")
	}
	if g.RequirePRCheck {
		t.Error("quick+review: RequirePRCheck should be false")
	}
}

func TestGateForMode_Standard_Development(t *testing.T) {
	g := GateForMode(ModeStandard, PhaseDevelopment)
	if !g.RequireTests {
		t.Error("standard+development: RequireTests should be true")
	}
	if g.RequireDocs {
		t.Error("standard+development: RequireDocs should be false")
	}
	if g.RequirePRCheck {
		t.Error("standard+development: RequirePRCheck should be false")
	}
	if len(g.Checklist) == 0 {
		t.Error("standard+development: Checklist should be non-empty")
	}
}

func TestGateForMode_Standard_Testing(t *testing.T) {
	g := GateForMode(ModeStandard, PhaseTesting)
	if !g.RequireTests {
		t.Error("standard+testing: RequireTests should be true")
	}
	if g.RequireDocs {
		t.Error("standard+testing: RequireDocs should be false")
	}
	if g.RequirePRCheck {
		t.Error("standard+testing: RequirePRCheck should be false")
	}
}

func TestGateForMode_Standard_Review(t *testing.T) {
	g := GateForMode(ModeStandard, PhaseReview)
	if !g.RequireTests {
		t.Error("standard+review: RequireTests should be true")
	}
	if g.RequirePRCheck {
		t.Error("standard+review: RequirePRCheck should be false")
	}
}

func TestGateForMode_Enterprise_Development(t *testing.T) {
	g := GateForMode(ModeEnterprise, PhaseDevelopment)
	if !g.RequireTests {
		t.Error("enterprise+development: RequireTests should be true")
	}
	if !g.RequireDocs {
		t.Error("enterprise+development: RequireDocs should be true")
	}
	if !g.RequirePRCheck {
		t.Error("enterprise+development: RequirePRCheck should be true")
	}
	if len(g.Checklist) == 0 {
		t.Error("enterprise+development: Checklist should be non-empty")
	}
}

func TestGateForMode_Enterprise_Testing(t *testing.T) {
	g := GateForMode(ModeEnterprise, PhaseTesting)
	if !g.RequireTests {
		t.Error("enterprise+testing: RequireTests should be true")
	}
	if !g.RequireDocs {
		t.Error("enterprise+testing: RequireDocs should be true")
	}
	if g.RequirePRCheck {
		t.Error("enterprise+testing: RequirePRCheck should be false")
	}
	if len(g.Checklist) == 0 {
		t.Error("enterprise+testing: Checklist should be non-empty")
	}
}

func TestGateForMode_Enterprise_Review(t *testing.T) {
	g := GateForMode(ModeEnterprise, PhaseReview)
	if !g.RequireTests {
		t.Error("enterprise+review: RequireTests should be true")
	}
	if !g.RequireDocs {
		t.Error("enterprise+review: RequireDocs should be true")
	}
	if !g.RequirePRCheck {
		t.Error("enterprise+review: RequirePRCheck should be true")
	}
	if len(g.Checklist) == 0 {
		t.Error("enterprise+review: Checklist should be non-empty")
	}
}

func TestGateForMode_UnknownMode_FallsBackToStandard(t *testing.T) {
	// An unknown mode hits the default branch (ModeStandard logic).
	g := GateForMode("unknown-mode", PhaseDevelopment)
	// Default/standard+development: RequireTests=true, RequireDocs=false.
	if !g.RequireTests {
		t.Error("unknown mode+development: expected RequireTests=true (standard fallback)")
	}
	if g.RequireDocs {
		t.Error("unknown mode+development: expected RequireDocs=false (standard fallback)")
	}
}

func TestGateForMode_ChecklistNonEmpty_ForActivePhases(t *testing.T) {
	// Every mode × active phase combo should have a non-empty checklist.
	activePhases := []string{PhaseDevelopment, PhaseTesting, PhaseReview}
	modes := []string{ModeQuick, ModeStandard, ModeEnterprise}

	for _, mode := range modes {
		for _, phase := range activePhases {
			g := GateForMode(mode, phase)
			if len(g.Checklist) == 0 {
				t.Errorf("mode=%q phase=%q: Checklist is empty", mode, phase)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// PhaseGuidance
// ---------------------------------------------------------------------------

func TestPhaseGuidance_AllPhasesReturnNonEmpty(t *testing.T) {
	phases := []string{
		PhasePlanning,
		PhaseDevelopment,
		PhaseTesting,
		PhaseReview,
		PhaseDeployment,
	}
	for _, phase := range phases {
		got := PhaseGuidance(phase, ModeStandard)
		if strings.TrimSpace(got) == "" {
			t.Errorf("PhaseGuidance(%q, standard): expected non-empty string", phase)
		}
	}
}

func TestPhaseGuidance_UnknownPhaseNonEmpty(t *testing.T) {
	got := PhaseGuidance("unknown", ModeStandard)
	if strings.TrimSpace(got) == "" {
		t.Error("PhaseGuidance(unknown): expected non-empty fallback string")
	}
}

func TestPhaseGuidance_Review_EnterpriseModeExtended(t *testing.T) {
	standard := PhaseGuidance(PhaseReview, ModeStandard)
	enterprise := PhaseGuidance(PhaseReview, ModeEnterprise)

	if strings.TrimSpace(standard) == "" {
		t.Error("review+standard: expected non-empty guidance")
	}
	if strings.TrimSpace(enterprise) == "" {
		t.Error("review+enterprise: expected non-empty guidance")
	}
	// Enterprise review guidance should be longer (includes doc/changelog instruction).
	if len(enterprise) <= len(standard) {
		t.Errorf("review+enterprise guidance (%d chars) should be longer than standard (%d chars)",
			len(enterprise), len(standard))
	}
}

func TestPhaseGuidance_ContainsPhaseKeyword(t *testing.T) {
	cases := []struct {
		phase   string
		keyword string
	}{
		{PhasePlanning, "planning"},
		{PhaseDevelopment, "development"},
		{PhaseTesting, "testing"},
		{PhaseReview, "review"},
		{PhaseDeployment, "deployment"},
	}
	for _, tc := range cases {
		got := PhaseGuidance(tc.phase, ModeStandard)
		if !strings.Contains(strings.ToLower(got), tc.keyword) {
			t.Errorf("PhaseGuidance(%q): expected output to mention %q, got: %q", tc.phase, tc.keyword, got)
		}
	}
}

func TestPhaseGuidance_AllModesReturnNonEmpty(t *testing.T) {
	modes := []string{ModeQuick, ModeStandard, ModeEnterprise}
	for _, mode := range modes {
		for _, phase := range []string{PhasePlanning, PhaseDevelopment, PhaseTesting, PhaseReview, PhaseDeployment} {
			got := PhaseGuidance(phase, mode)
			if strings.TrimSpace(got) == "" {
				t.Errorf("PhaseGuidance(%q, %q): expected non-empty string", phase, mode)
			}
		}
	}
}
