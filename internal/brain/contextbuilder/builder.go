// Package contextbuilder assembles a structured Context Packet from a Synapses
// graph snapshot, SDLC phase/mode, and the Brain's learned data.
//
// The Context Packet replaces raw graph nodes with semantic summaries, active
// constraints, team coordination info, quality gate requirements, and learned
// co-occurrence hints — capped at ~800 tokens for the main LLM consumer.
//
// All types here are internal; pkg/brain converts them to public brain.* types.
package contextbuilder

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SynapsesOS/synapses/internal/brain/enricher"
	"github.com/SynapsesOS/synapses/internal/brain/sdlc"
	"github.com/SynapsesOS/synapses/internal/brain/store"
)

const (
	maxDependencySummaries = 8
	maxPatternHints        = 3
	maxConstraints         = 5
	maxTeamStatus          = 5
)

// ConstraintItem is a rule the agent must respect.
type ConstraintItem struct {
	RuleID      string
	Severity    string
	Description string
	Hint        string // cached fix suggestion, may be empty
}

// AgentItem represents another agent's work claim.
type AgentItem struct {
	AgentID   string
	Scope     string
	ScopeType string
	ExpiresIn int // seconds until expiry (0 = unknown)
}

// PatternItem is a learned co-occurrence: "when editing Trigger, also check CoChange".
type PatternItem struct {
	Trigger    string
	CoChange   string
	Reason     string
	Confidence float64
}

// Packet is the assembled context document returned by Builder.Build().
// pkg/brain converts this to brain.ContextPacket for the public API.
type Packet struct {
	AgentID     string
	EntityName  string
	EntityType  string
	GeneratedAt string
	Phase       string
	QualityMode string

	RootSummary         string
	DependencySummaries map[string]string

	Insight  string
	Concerns []string
	LLMUsed  bool // true when the LLM was called for this packet

	ActiveConstraints []ConstraintItem
	// PacketQuality is a 0.0-1.0 heuristic reflecting how complete the packet is:
	//   1.0 = root summary + dep summaries + LLM insight all present
	//   0.5 = root summary present, no LLM insight
	//   0.0 = empty packet (no summaries ingested yet)
	PacketQuality float64
	TeamStatus    []AgentItem
	Gate          sdlc.Gate
	PatternHints  []PatternItem
	PhaseGuidance string

	// GraphWarnings are actionable warnings derived from graph topology.
	// Always populated (no LLM required) — deterministic, high-signal guidance.
	GraphWarnings []string

	// ComplexityScore is a dimensionless topology-derived risk indicator:
	// (fanIn + fanOut) * (1 + fanOut/10.0). Always populated (no LLM required).
	// 0.0 = isolated/leaf node; higher = more callers + dependencies = higher change risk.
	ComplexityScore float64

	// DeterministicPath is true when Phase and ComplexityScore were computed
	// from graph topology (always). False only when the enricher was not called.
	DeterministicPath bool
}

// RuleRef is a single architectural rule reference from the Synapses snapshot.
type RuleRef struct {
	RuleID      string
	Severity    string
	Description string
}

// ClaimRef is a work claim from another agent.
type ClaimRef struct {
	AgentID   string
	Scope     string
	ScopeType string
	ExpiresAt string // RFC3339; empty = unknown
}

// Request is the input to Builder.Build().
type Request struct {
	ProjectID   string
	AgentID     string
	Phase       string // "" = use stored project phase
	QualityMode string // "" = use stored project mode
	EnableLLM   bool   // true = allow LLM insight generation (adds ~2-3s)

	// From Synapses snapshot:
	RootNodeID      string
	RootName        string
	RootType        string
	RootFile        string   // used for constraint hint lookups
	CalleeNames     []string // direct callees
	CallerNames     []string // direct callers
	RelatedNames    []string // transitive neighbours
	ApplicableRules []RuleRef
	ActiveClaims    []ClaimRef
	TaskContext     string
	TaskID          string

	// Graph topology signals (populated by synapses core):
	HasTests bool   // whether *_test.go exists for root file
	FanIn    int    // total caller count (may exceed len(CallerNames) when capped)
	RootDoc  string // AST doc comment; used as fallback when brain.sqlite has no summary
}

// Builder assembles a Context Packet from a Synapses snapshot and brain data.
type Builder struct {
	store   *store.Store
	manager *sdlc.Manager
	enr     *enricher.Enricher // nil = LLM insight disabled
}

// New creates a Builder. enr may be nil to disable LLM insight generation.
func New(st *store.Store, mgr *sdlc.Manager, enr *enricher.Enricher) *Builder {
	return &Builder{store: st, manager: mgr, enr: enr}
}

// Build assembles and returns a Context Packet for the given request.
// The fast path (SQLite lookups) always runs; the LLM path runs only if
// req.EnableLLM is true and the enricher is available.
func (b *Builder) Build(ctx context.Context, req Request) (*Packet, error) {
	// 1. Resolve effective phase and quality mode (request override or stored).
	phase := b.manager.ResolvePhase(req.Phase)
	mode := b.manager.ResolveMode(req.QualityMode)
	sections := sdlc.SectionsForPhase(phase)

	pkt := &Packet{
		AgentID:     req.AgentID,
		EntityName:  req.RootName,
		EntityType:  req.RootType,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Phase:       phase,
		QualityMode: mode,
	}

	// Section 1: Root summary (fast path — SQLite).
	// Falls back to the AST doc comment so packet_quality ≥ 0.4 on cold brain.
	if sections.RootSummary && req.RootNodeID != "" {
		pkt.RootSummary = b.store.GetSummary(req.ProjectID, req.RootNodeID)
		if pkt.RootSummary == "" && req.RootDoc != "" {
			pkt.RootSummary = req.RootDoc
		}
	}

	// Section 1b: Dependency summaries (fast path — SQLite).
	if sections.DependencySummaries {
		names := collectDepNames(req, maxDependencySummaries)
		if len(names) > 0 {
			pkt.DependencySummaries = b.store.GetSummariesByName(names)
		}
	}

	// Section 2: Enricher (deterministic pass always; LLM pass when enabled + no cache hit).
	if b.enr != nil && req.RootNodeID != "" {
		enrReq := enricher.Request{
			RootID:       req.RootNodeID,
			RootName:     req.RootName,
			RootType:     req.RootType,
			RootFile:     req.RootFile,
			CalleeNames:  req.CalleeNames,
			CallerNames:  req.CallerNames,
			RelatedNames: req.RelatedNames,
			TaskContext:  req.TaskContext,
			FanIn:        req.FanIn,
		}

		// Fast path: check LLM insight cache before calling the enricher's LLM path.
		// The deterministic pass always runs inside enr.Enrich regardless of cache.
		var cachedInsight string
		var cachedConcerns []string
		hasCachedInsight := false
		if sections.LLMInsight && req.EnableLLM {
			if cached, ok := b.store.GetInsightCache(req.RootNodeID, phase); ok {
				cachedInsight = cached.Insight
				cachedConcerns = cached.Concerns
				hasCachedInsight = true
			}
		}

		// Disable LLM in the enricher call when cache hit or LLM disabled —
		// the deterministic pass still runs and populates Phase + ComplexityScore.
		if hasCachedInsight || !sections.LLMInsight || !req.EnableLLM {
			enrReq.CalleeNames = req.CalleeNames // ensure passed for fanOut computation
		}

		r, err := b.enr.Enrich(ctx, enrReq)
		if err == nil {
			// Deterministic fields — always apply when enricher ran.
			pkt.ComplexityScore = r.ComplexityScore
			pkt.DeterministicPath = r.DeterministicHit

			// Phase fallback: use enricher's file-path-inferred phase when the
			// stored SDLC config has no phase set (phase == "").
			if pkt.Phase == "" && r.Phase != "" {
				pkt.Phase = r.Phase
			}

			// LLM fields: prefer cached insight; fall back to fresh LLM result.
			if hasCachedInsight {
				pkt.Insight = cachedInsight
				pkt.Concerns = cachedConcerns
				// LLMUsed stays false — served from cache
			} else if r.LLMUsed {
				// SIL model may provide a root summary more specific than SQLite.
				if r.RootSummary != "" {
					pkt.RootSummary = r.RootSummary
				}
				pkt.Insight = r.Insight
				pkt.Concerns = r.Concerns
				pkt.LLMUsed = r.LLMUsed
				// Store in cache for future requests.
				if req.RootNodeID != "" {
					_ = b.store.UpsertInsightCache(req.RootNodeID, phase, r.Insight, r.Concerns)
				}
			}
		}
		// err is non-fatal — deterministic fields still delivered when err == nil above.
	}

	// Section 3: Active constraints.
	if sections.ActiveConstraints {
		pkt.ActiveConstraints = buildConstraints(b.store, req.ApplicableRules, req.RootFile, maxConstraints)
	}

	// Section 4: Team status.
	if sections.TeamStatus {
		pkt.TeamStatus = buildTeamStatus(req.AgentID, req.ActiveClaims, maxTeamStatus)
	}

	// Section 5: Quality gate.
	if sections.QualityGate {
		pkt.Gate = sdlc.GateForMode(mode, phase)
	}

	// Section 6: Learned pattern hints.
	if sections.PatternHints {
		triggers := patternTriggers(req)
		patterns := b.store.GetPatternsForTriggers(triggers, maxPatternHints)
		pkt.PatternHints = toPatternItems(patterns)
	}

	// Section 7: Phase guidance.
	if sections.PhaseGuidance {
		pkt.PhaseGuidance = sdlc.PhaseGuidance(phase, mode)
	}

	// Section 8: Graph-derived warnings (deterministic, always populated).
	pkt.GraphWarnings = buildGraphWarnings(req)

	// Compute packet quality: 0.0 (empty) → 0.5 (summaries) → 1.0 (full with insight).
	pkt.PacketQuality = computeQuality(pkt)

	// SIL Verification Pass: deterministically annotate LLM claims against graph topology.
	// Verified claims get [✓]; contradicted claims get [⚠ UNVERIFIED: actual value].
	// Zero latency cost — no LLM involved.
	verifyPacket(pkt, req)

	return pkt, nil
}

// --- helpers ---

// collectDepNames returns callee names first, then caller names, capped at limit.
// Priority: callees (what root CALLS) are most relevant during development.
func collectDepNames(req Request, limit int) []string {
	seen := make(map[string]bool)
	var out []string
	for _, name := range append(req.CalleeNames, req.CallerNames...) {
		if name == "" || name == req.RootName || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// buildConstraints converts rule refs to ConstraintItems and looks up cached hints.
func buildConstraints(st *store.Store, rules []RuleRef, rootFile string, limit int) []ConstraintItem {
	if len(rules) == 0 {
		return nil
	}
	if limit > len(rules) {
		limit = len(rules)
	}
	out := make([]ConstraintItem, 0, limit)
	for _, r := range rules[:limit] {
		item := ConstraintItem{
			RuleID:      r.RuleID,
			Severity:    r.Severity,
			Description: r.Description,
		}
		// Try to find a cached fix hint from a past violation.
		if rootFile != "" {
			_, fix, found := st.GetViolationExplanation(r.RuleID, rootFile)
			if found && fix != "" {
				item.Hint = fix
			}
		}
		out = append(out, item)
	}
	return out
}

// buildTeamStatus converts claim refs to AgentItems, excluding the requesting agent.
func buildTeamStatus(selfAgentID string, claims []ClaimRef, limit int) []AgentItem {
	out := make([]AgentItem, 0, limit)
	for _, c := range claims {
		if c.AgentID == selfAgentID {
			continue // skip own claim
		}
		item := AgentItem{
			AgentID:   c.AgentID,
			Scope:     c.Scope,
			ScopeType: c.ScopeType,
		}
		if c.ExpiresAt != "" {
			if t, err := time.Parse(time.RFC3339, c.ExpiresAt); err == nil {
				secs := int(time.Until(t).Seconds())
				if secs > 0 {
					item.ExpiresIn = secs
				}
			}
		}
		out = append(out, item)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// patternTriggers returns trigger names for pattern lookups: root + top callees.
func patternTriggers(req Request) []string {
	triggers := []string{}
	if req.RootName != "" {
		triggers = append(triggers, req.RootName)
	}
	for i, name := range req.CalleeNames {
		if i >= 4 {
			break
		}
		if name != "" {
			triggers = append(triggers, name)
		}
	}
	return triggers
}

// toPatternItems converts store patterns to PatternItems.
func toPatternItems(patterns []store.ContextPattern) []PatternItem {
	out := make([]PatternItem, len(patterns))
	for i, p := range patterns {
		out[i] = PatternItem{
			Trigger:    p.Trigger,
			CoChange:   p.CoChange,
			Reason:     p.Reason,
			Confidence: p.Confidence,
		}
	}
	return out
}

// buildGraphWarnings produces actionable warnings from graph topology signals.
// These are deterministic — no LLM required.
func buildGraphWarnings(req Request) []string {
	var warnings []string

	// Blast radius warning: high fanin means changes here have wide impact.
	fanin := req.FanIn
	if fanin == 0 {
		fanin = len(req.CallerNames) // fall back to slice length if FanIn not set
	}
	if fanin > 5 {
		callerList := ""
		if len(req.CallerNames) > 0 {
			cap := req.CallerNames
			if len(cap) > 5 {
				cap = cap[:5]
			}
			callerList = " (e.g. " + strings.Join(cap, ", ") + ")"
		}
		warnings = append(warnings, fmt.Sprintf(
			"⚠ High blast radius: %d callers%s — run get_impact before modifying",
			fanin, callerList,
		))
	}

	// Test coverage signal.
	if !req.HasTests && req.RootFile != "" {
		warnings = append(warnings, "⚠ No test file found for this entity — consider adding tests before changing")
	}

	return warnings
}

// computeQuality returns a 0.0–1.0 heuristic for how complete a packet is.
//
// Scoring:
//   - Enricher deterministic path ran (ComplexityScore or DeterministicPath): +0.1
//   - RootSummary present: +0.4
//   - At least one DependencySummary: +0.1
//   - LLM Insight present (live or cached): +0.4
func computeQuality(pkt *Packet) float64 {
	var q float64
	// Only count the deterministic enricher pass — not SDLC phase from stored config.
	if pkt.DeterministicPath || pkt.ComplexityScore > 0 {
		q += 0.1
	}
	if pkt.RootSummary != "" {
		q += 0.4
	}
	if len(pkt.DependencySummaries) > 0 {
		q += 0.1
	}
	if pkt.Insight != "" {
		q += 0.4
	}
	if q > 1.0 {
		q = 1.0
	}
	return q
}
