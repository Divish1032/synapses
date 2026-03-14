// Package sdlc provides SDLC phase awareness and quality mode profiles
// for the synapses-intelligence Context Packet builder.
//
// This file contains only static lookup tables — pure functions with no side effects,
// no SQLite, no LLM, no external imports. Phase and mode values are plain strings
// that match the SDLCPhase / QualityMode constants defined in pkg/brain.
package sdlc

// Phase constants — string values mirror brain.SDLCPhase constants.
const (
	PhasePlanning    = "planning"
	PhaseDevelopment = "development"
	PhaseTesting     = "testing"
	PhaseReview      = "review"
	PhaseDeployment  = "deployment"
)

// Mode constants — string values mirror brain.QualityMode constants.
const (
	ModeQuick      = "quick"
	ModeStandard   = "standard"
	ModeEnterprise = "enterprise"
)

// SectionFlags controls which ContextPacket sections are assembled for a given phase.
// Sections disabled by phase are simply omitted from the packet (zero-value).
type SectionFlags struct {
	RootSummary         bool
	DependencySummaries bool
	LLMInsight          bool
	ActiveConstraints   bool
	TeamStatus          bool
	QualityGate         bool
	PatternHints        bool
	PhaseGuidance       bool
}

// Gate is the SDLC-internal quality gate representation.
// pkg/brain converts this to brain.QualityGate for the public API.
type Gate struct {
	RequireTests   bool
	RequireDocs    bool
	RequirePRCheck bool
	Checklist      []string
}

// SectionsForPhase returns the active section flags for a given SDLC phase string.
// The matrix encodes what context is relevant for each phase:
//
//	Section         | planning | development | testing | review | deployment
//	RootSummary     |    ✓     |      ✓      |    ✓    |   ✓    |     ✓
//	DepSummaries    |    ✓     |      ✓      |    —    |   ✓    |     —
//	LLMInsight      |    ✓     |      ✓      |    —    |   ✓    |     —
//	Constraints     |    —     |      ✓      |    ✓    |   ✓    |     —
//	TeamStatus      |    ✓     |      ✓      |    ✓    |   ✓    |     ✓
//	QualityGate     |    —     |      ✓      |    ✓    |   ✓    |     —
//	PatternHints    |    —     |      ✓      |    —    |   ✓    |     —
//	PhaseGuidance   |    ✓     |      ✓      |    ✓    |   ✓    |     ✓
func SectionsForPhase(phase string) SectionFlags {
	switch phase {
	case PhasePlanning:
		return SectionFlags{
			RootSummary:         true,
			DependencySummaries: true,
			LLMInsight:          true,
			ActiveConstraints:   false, // Plan is being formed; constraints come during dev
			TeamStatus:          true,
			QualityGate:         false, // Nothing to commit yet
			PatternHints:        false, // Not editing code yet
			PhaseGuidance:       true,
		}
	case PhaseDevelopment:
		return SectionFlags{
			RootSummary:         true,
			DependencySummaries: true,
			LLMInsight:          true,
			ActiveConstraints:   true,
			TeamStatus:          true,
			QualityGate:         true,
			PatternHints:        true,
			PhaseGuidance:       true,
		}
	case PhaseTesting:
		return SectionFlags{
			RootSummary:         true,
			DependencySummaries: false, // Tester cares about interfaces, not implementation
			LLMInsight:          false, // Structural insight irrelevant during test writing
			ActiveConstraints:   true,
			TeamStatus:          true,
			QualityGate:         true,
			PatternHints:        false, // Not editing implementation
			PhaseGuidance:       true,
		}
	case PhaseReview:
		return SectionFlags{
			RootSummary:         true,
			DependencySummaries: true,
			LLMInsight:          true,
			ActiveConstraints:   true,
			TeamStatus:          true,
			QualityGate:         true,
			PatternHints:        true,
			PhaseGuidance:       true,
		}
	case PhaseDeployment:
		return SectionFlags{
			RootSummary:         true,
			DependencySummaries: false, // No code changes in deployment
			LLMInsight:          false,
			ActiveConstraints:   false, // Don't touch code; don't show rules
			TeamStatus:          true,
			QualityGate:         false,
			PatternHints:        false,
			PhaseGuidance:       true,
		}
	default: // unknown phase — show everything, safe default
		return SectionFlags{
			RootSummary:         true,
			DependencySummaries: true,
			LLMInsight:          true,
			ActiveConstraints:   true,
			TeamStatus:          true,
			QualityGate:         true,
			PatternHints:        true,
			PhaseGuidance:       true,
		}
	}
}

// GateForMode returns the quality Gate requirements for a mode+phase combination.
// The gate is only meaningful in development, testing, and review phases.
func GateForMode(mode, phase string) Gate {
	// Quality gate doesn't apply in planning or deployment.
	if phase == PhasePlanning || phase == PhaseDeployment {
		return Gate{}
	}

	switch mode {
	case ModeQuick:
		return Gate{
			RequireTests:   false,
			RequireDocs:    false,
			RequirePRCheck: false,
			Checklist: []string{
				"Verify the code compiles without errors",
				"Verify the primary use case works",
			},
		}

	case ModeEnterprise:
		switch phase {
		case PhaseTesting:
			return Gate{
				RequireTests:   true,
				RequireDocs:    true,
				RequirePRCheck: false,
				Checklist: []string{
					"Write unit tests for every modified function/method",
					"Write integration tests for all API boundaries",
					"Verify test coverage does not decrease",
					"All tests must pass (including pre-existing tests)",
					"Run get_violations — ensure zero errors",
				},
			}
		case PhaseReview:
			return Gate{
				RequireTests:   true,
				RequireDocs:    true,
				RequirePRCheck: true,
				Checklist: []string{
					"All tests passing (unit + integration)",
					"All exported symbols have doc comments",
					"CHANGELOG or release notes updated",
					"PR checklist filled out and signed off",
					"No active architectural rule violations",
					"Vote on any open proposals via vote_on_proposal",
				},
			}
		default: // development
			return Gate{
				RequireTests:   true,
				RequireDocs:    true,
				RequirePRCheck: true,
				Checklist: []string{
					"Write unit tests for new/modified functions",
					"Write integration tests for API boundaries",
					"Document all exported symbols (GoDoc/JSDoc)",
					"Run validate_plan before committing",
					"No active architectural rule violations",
					"CHANGELOG entry for user-visible changes",
				},
			}
		}

	default: // ModeStandard
		switch phase {
		case PhaseTesting:
			return Gate{
				RequireTests:   true,
				RequireDocs:    false,
				RequirePRCheck: false,
				Checklist: []string{
					"Write unit tests for modified functions",
					"All tests must pass",
					"Run get_violations — fix any errors",
				},
			}
		case PhaseReview:
			return Gate{
				RequireTests:   true,
				RequireDocs:    false,
				RequirePRCheck: false,
				Checklist: []string{
					"All tests passing",
					"No active architectural rule violations",
					"Exported symbols have doc comments",
					"Vote on any open proposals",
				},
			}
		default: // development
			return Gate{
				RequireTests:   true,
				RequireDocs:    false,
				RequirePRCheck: false,
				Checklist: []string{
					"Write unit tests for new/modified functions",
					"Run validate_plan — no rule violations",
					"Exported symbols should have doc comments",
				},
			}
		}
	}
}

// PhaseGuidance returns a plain-English guidance string for an agent in the given phase.
// These strings are injected directly into the ContextPacket — no LLM call needed.
func PhaseGuidance(phase, mode string) string {
	switch phase {
	case PhasePlanning:
		return "You are in planning phase. Design the solution architecture, create tasks via " +
			"create_plan, link tasks to code nodes via link_task_nodes. Propose architectural " +
			"boundaries via propose_change if needed. Do NOT write implementation code yet."
	case PhaseDevelopment:
		return "You are in development phase. Implement per the plan. Claim work before editing " +
			"(claim_work). Respect all active constraints. Run validate_plan before major changes. " +
			"Mark tasks done via update_task as you complete them."
	case PhaseTesting:
		return "You are in testing phase. Write tests for every modified entity. Run get_violations " +
			"to check for architectural drift. Focus on test coverage — do NOT add new features. " +
			"Fix failures only."
	case PhaseReview:
		base := "You are in review phase. Verify all quality gate items are satisfied. "
		if mode == ModeEnterprise {
			base += "Check that documentation is complete and CHANGELOG is updated. "
		}
		return base + "Run get_violations to confirm no active errors. Vote on open proposals via vote_on_proposal."
	case PhaseDeployment:
		return "You are in deployment phase. Do NOT modify source files. Monitor events via " +
			"get_events. Release your work claims via release_claims when deployment is confirmed. " +
			"Flag any issues as new tasks via update_task."
	default:
		return "Run get_project_identity to orient yourself, then get_pending_tasks to find your work."
	}
}
