package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/brain"
	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/federation"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
	"github.com/SynapsesOS/synapses/internal/pulse"
	"github.com/SynapsesOS/synapses/internal/security"
	"github.com/SynapsesOS/synapses/internal/store"
)

// pathWithinRoot reports whether path is inside (or equal to) root.
// It prevents directory traversal attacks by comparing cleaned absolute paths.
//
// When root is empty the project boundary is unknown; filesystem access is
// blocked (returns false) rather than allowing arbitrary paths — fail-closed.
//
// For paths that exist on disk, symlinks are resolved via filepath.EvalSymlinks
// so that a symlink inside root pointing outside (e.g. to /etc/passwd) is
// correctly rejected. For non-existent paths (proposed files), only
// filepath.Clean is used since EvalSymlinks requires existence.
func pathWithinRoot(root, path string) bool {
	if root == "" {
		return false
	}
	cleanRoot := filepath.Clean(root)
	cleanPath := filepath.Clean(path)

	// Resolve symlinks for paths that exist on disk.
	if resolved, err := filepath.EvalSymlinks(cleanPath); err == nil {
		cleanPath = resolved
	} else {
		// Path doesn't exist yet (proposed file). Walk up to find the deepest
		// existing ancestor and resolve its symlinks, then reattach the suffix.
		// This handles macOS /var→/private/var without false containment failures.
		ancestor := filepath.Dir(cleanPath)
		for ancestor != filepath.Dir(ancestor) {
			if resolved, err := filepath.EvalSymlinks(ancestor); err == nil {
				rel := cleanPath[len(ancestor):]
				cleanPath = filepath.Join(resolved, rel)
				break
			}
			ancestor = filepath.Dir(ancestor)
		}
	}
	if resolved, err := filepath.EvalSymlinks(cleanRoot); err == nil {
		cleanRoot = resolved
	}

	return cleanPath == cleanRoot ||
		strings.HasPrefix(cleanPath, cleanRoot+string(filepath.Separator))
}

// archViolationAction maps an architectural rule violation's severity string to
// the three-tier action directive used in validate responses (Sprint 27.4).
//
// Arch rule severities use "error"/"warning" (from config.Violation.Severity),
// not the CRITICAL/HIGH/MEDIUM enum used by security patterns. The mapping:
//   - "error"   → "warn"   (HIGH equivalent: strong warning, can override)
//   - "warning" → "inform" (MEDIUM equivalent: informational, agent decides)
//
// CRITICAL / "block" is reserved for security pattern violations only.
func archViolationAction(severity string) string {
	if severity == "error" {
		return "warn"
	}
	return "inform"
}

// archViolationResponse wraps config.Violation with the derived action field
// for the three-tier severity model (Sprint 27.4). Used in JSON responses only
// — internal calculations still use config.Violation.
//
// Embedded struct fields are promoted to the top level in JSON, so the output
// includes all config.Violation fields plus the `action` field.
type archViolationResponse struct {
	config.Violation
	Action string `json:"action"`
}

// enrichArchViolations returns an arch-violation slice enriched with the
// derived action field. Returns nil when the input is empty.
// Called only at response-building time; internal calculations use []config.Violation.
func enrichArchViolations(vs []config.Violation) []archViolationResponse {
	if len(vs) == 0 {
		return nil
	}
	out := make([]archViolationResponse, len(vs))
	for i, v := range vs {
		out[i] = archViolationResponse{Violation: v, Action: archViolationAction(v.Severity)}
	}
	return out
}

// worstActionRequired returns the highest-priority action directive across all
// security findings and architectural violations in a validate response.
// Priority order: "block" > "warn" > "inform" > "none".
//
// secFindings must have their Action field populated (by security.withActions).
// archViolations are evaluated via archViolationAction.
func worstActionRequired(secFindings []security.Violation, archViolations []config.Violation) string {
	rank := func(action string) int {
		switch action {
		case "block":
			return 3
		case "warn":
			return 2
		case "inform":
			return 1
		default:
			return 0
		}
	}
	best := 0
	for _, f := range secFindings {
		if r := rank(f.Action); r > best {
			best = r
		}
	}
	for _, v := range archViolations {
		if r := rank(archViolationAction(v.Severity)); r > best {
			best = r
		}
	}
	switch best {
	case 3:
		return "block"
	case 2:
		return "warn"
	case 1:
		return "inform"
	default:
		return "none"
	}
}

// ProposedChange is a single entry in a validate_plan request.
type ProposedChange struct {
	File          string `json:"file"`
	AddsCallTo    string `json:"adds_call_to,omitempty"`
	RemovesCallTo string `json:"removes_call_to,omitempty"`
}

// handleValidatePlan checks proposed changes against architectural rules.
// When check_safety=true is passed, it also runs check_plan_safety inline
// (500ms cap) so agents get both history-based warnings and structural
// violations in a single round-trip.
func (s *Server) handleValidatePlan(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	changesRaw, ok := req.GetArguments()["changes"].(string)
	if !ok || changesRaw == "" {
		return mcp.NewToolResultError(`changes is required (e.g., [{"file": "internal/auth.go", "adds_call_to": "ValidateToken"}])`), nil
	}

	var changes []ProposedChange
	if err := json.Unmarshal([]byte(changesRaw), &changes); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid changes JSON: %v", stripInternalPaths(err.Error()))), nil
	}

	if s.graph == nil {
		return jsonResult(map[string]interface{}{
			"status":  "unprotected",
			"message": "graph not loaded — start the daemon with a valid project path to enable structural validation",
		})
	}

	// Optional inline safety check — runs check_plan_safety before structural validation.
	var safetyCheck map[string]interface{}
	if checkSafety, _ := req.GetArguments()["check_safety"].(bool); checkSafety && s.store != nil {
		planDesc := stringArg(req, "plan_description")
		if planDesc == "" {
			var files []string
			for _, c := range changes {
				if c.File != "" {
					files = append(files, c.File)
				}
			}
			if len(files) > 0 {
				planDesc = strings.Join(files, ", ")
			}
		}
		if planDesc != "" {
			safetyCtx, safetyCancel := context.WithTimeout(ctx, 500*time.Millisecond)
			ep, safetyErr := s.store.CheckPlanSafetyCtx(safetyCtx, planDesc, "")
			// Capture deadline state BEFORE calling safetyCancel — calling cancel
			// sets Err() to context.Canceled (non-nil) even when the deadline never
			// fired, which previously caused every call to report "timed out".
			timedOut := errors.Is(safetyCtx.Err(), context.DeadlineExceeded)
			safetyCancel()
			switch {
			case safetyErr == nil && ep != nil:
				safetyCheck = map[string]interface{}{
					"status": "warning",
					"match": map[string]interface{}{
						"episode_id": ep.ID,
						"decision":   ep.Decision,
						"outcome":    ep.Outcome,
						"rationale":  ep.Rationale,
					},
					"message": fmt.Sprintf("⚠ Past failure match: %q (outcome: %s). Review before proceeding.", ep.Decision, ep.Outcome),
				}
			case timedOut || errors.Is(safetyErr, context.DeadlineExceeded):
				safetyCheck = map[string]interface{}{"status": "timeout", "note": "safety check timed out (>500ms) — past failures not checked"}
			case ep == nil && safetyErr == nil && s.store.HasNoFailureEpisodes():
				safetyCheck = map[string]interface{}{"status": "clear_no_data", "note": "no failure episodes recorded yet"}
			default:
				safetyCheck = map[string]interface{}{"status": "clear"}
			}
		}
	}

	// GAP-4: Check freshness of files in the changes array.
	var freshWarnings []string
	repoRoot := s.graph.Root()
	for _, c := range changes {
		if c.File == "" {
			continue
		}
		absFile := c.File
		if repoRoot != "" && !filepath.IsAbs(absFile) {
			absFile = filepath.Join(repoRoot, absFile)
		}
		// Security: reject paths that escape the project root.
		if !pathWithinRoot(repoRoot, absFile) {
			continue
		}
		if fi, err := os.Stat(absFile); err == nil {
			if age := time.Since(fi.ModTime()); age < 10*time.Second {
				freshWarnings = append(freshWarnings, fmt.Sprintf("%s (modified %s ago)", c.File, age.Round(time.Second)))
			}
		}
	}

	// Build a temporary overlay graph that includes the proposed additions.
	overlay := cloneGraph(s.graph)
	var warnings []string
	var skipped []string
	for _, change := range changes {
		if change.AddsCallTo == "" {
			continue
		}
		// Find the callee node.
		callees := overlay.FindByName(change.AddsCallTo)
		if len(callees) == 0 {
			// Not alarming — the target may not exist yet (new symbol).
			// Skip this edge; it cannot violate rules that reference existing nodes.
			skipped = append(skipped, fmt.Sprintf("adds_call_to %q not yet in graph — edge skipped (no rules can fire for unknown targets)", change.AddsCallTo))
			continue
		}
		// Find nodes in the source file. Accepts both absolute and relative paths
		// (e.g. "synapses/internal/graph/graph.go" resolves against absolute paths
		// stored by the parser via the suffix-based FindByFile match).
		sources := overlay.FindByFile(change.File)
		if len(sources) == 0 {
			skipped = append(skipped, fmt.Sprintf("file %q: no nodes found in graph (check path is correct relative to repo root)", change.File))
			continue
		}
		// Add edges to all name-matched callees so CheckViolations can
		// detect rule violations regardless of which callee is the intended target.
		for _, callee := range callees {
			overlay.AddEdge(&graph.Edge{
				From: sources[0].ID,
				To:   callee.ID,
				Type: graph.EdgeCalls,
			})
		}
	}

	s.rulesMu.RLock()
	var violations []config.Violation
	var hasRules bool
	if s.config != nil {
		violations = s.config.CheckViolations(overlay)
		hasRules = len(s.config.Rules) > 0
	}
	s.rulesMu.RUnlock()

	status := "ok"
	if len(violations) > 0 {
		status = "violations_found"
	} else if !hasRules {
		// Issue 3: distinguish "validated clean" from "not checked at all".
		// "ok" must only appear when at least one rule was evaluated and passed.
		status = "unprotected"
	}

	// P3-5: emit validation outcome event.
	if pc := s.getPulseClient(); pc != nil {
		safetyStatus := ""
		if safetyCheck != nil {
			safetyStatus, _ = safetyCheck["status"].(string)
		}
		agentIDForPulse := stringArg(req, "agent_id")
		projID := s.projectID

		// P3B-6: collect unique rule IDs from violations and encode as JSON.
		ruleIDSet := make(map[string]struct{})
		for _, v := range violations {
			if v.RuleID != "" {
				ruleIDSet[v.RuleID] = struct{}{}
			}
		}
		ruleIDs := make([]string, 0, len(ruleIDSet))
		for rid := range ruleIDSet {
			ruleIDs = append(ruleIDs, rid)
		}
		sort.Strings(ruleIDs)
		ruleIDsJSON, _ := json.Marshal(ruleIDs)

		pc.RecordValidationEvent(pulse.ValidationEvent{
			ToolName:       "validate_plan",
			Status:         status,
			ViolationCount: len(violations),
			// SecurityFindingCount is not set here: the pulse event fires before
			// the security scan executes in handleValidatePlan. Security findings
			// are included in the tool response but not in this pulse event.
			SafetyStatus: safetyStatus,
			RuleIDs:      string(ruleIDsJSON),
			AgentID:      agentIDForPulse,
			ProjectID:    projID,
		})
	}

	// GAP-8: Auto pattern extraction — when violations are found, record an
	// episode so check_plan_safety surfaces this warning for similar future plans.
	// This fills episodic memory without requiring agents to call remember() manually.
	if len(violations) > 0 && s.store != nil {
		agentIDForEp := stringArg(req, "agent_id")
		planDescForEp := stringArg(req, "plan_description")
		if planDescForEp == "" {
			var files []string
			for _, c := range changes {
				if c.File != "" {
					files = append(files, c.File)
				}
			}
			planDescForEp = strings.Join(files, ", ")
		}
		var sb strings.Builder
		for i, v := range violations {
			if i >= 3 {
				fmt.Fprintf(&sb, "... and %d more", len(violations)-3)
				break
			}
			fmt.Fprintf(&sb, "[%s] %s; ", v.RuleID, v.Description)
		}
		ep := store.Episode{
			AgentID:     agentIDForEp,
			EpisodeType: "failure",
			Outcome:     "failure",
			Trigger:     fmt.Sprintf("validate_plan: %d violation(s) for: %s", len(violations), planDescForEp),
			Decision:    fmt.Sprintf("Plan failed validation: %s", sb.String()),
			Rationale:   "Auto-recorded when validate_plan detected violations. check_plan_safety will surface this for similar future plans.",
			Tags:        `["auto","validate_plan","violation"]`,
			Importance:  0.6,
		}
		s.goBackground(func() {
			if _, err := s.store.RememberEpisode(ep); err != nil {
				log.Printf("mcp: auto-record validate_plan episode: %v", err)
			}
		})
	}

	// RX3: Logic-level anomaly detection — heuristic AST checks on files in the
	// changes array. Each check is a pure tree-sitter pattern match, no LLM needed.
	// Both caps are intentional: maxLogicWarnings prevents response bloat;
	// maxLogicFiles prevents O(N) file-read latency on large validate_plan calls
	// (agents occasionally pass 30+ files in a single call during big refactors).
	const maxLogicWarnings = 5
	const maxLogicFiles = 10
	var logicWarnings []parser.LogicWarning
	if skipLogic, _ := req.GetArguments()["skip_logic_checks"].(bool); !skipLogic {
		filesScanned := 0
		for _, c := range changes {
			if c.File == "" {
				continue
			}
			if filesScanned >= maxLogicFiles {
				break
			}
			absFile := c.File
			if repoRoot != "" && !filepath.IsAbs(absFile) {
				absFile = filepath.Join(repoRoot, absFile)
			}
			// Security: reject paths that escape the project root.
			if !pathWithinRoot(repoRoot, absFile) {
				continue
			}
			src, err := os.ReadFile(absFile)
			if err != nil {
				continue // file may not exist yet (proposed new file)
			}
			filesScanned++
			w := parser.RunLogicChecks(c.File, src)
			logicWarnings = append(logicWarnings, w...)
			if len(logicWarnings) >= maxLogicWarnings {
				logicWarnings = logicWarnings[:maxLogicWarnings]
				break
			}
		}
	}

	// Sprint 26.7: security pattern scan over changed files.
	// Runs graph-based checks (import, middleware, admin) without file content.
	// Content-based checks (hardcoded secrets) are included when the file exists on disk.
	var securityFindings []security.Violation
	if s.graph != nil && s.patternEngine != nil {
		const maxSecurityFiles = 10
		scanned := 0
		for _, c := range changes {
			if c.File == "" || scanned >= maxSecurityFiles {
				continue
			}
			absFile := c.File
			if repoRoot != "" && !filepath.IsAbs(absFile) {
				absFile = filepath.Join(repoRoot, absFile)
			}
			if !pathWithinRoot(repoRoot, absFile) {
				continue
			}
			src, _ := os.ReadFile(absFile) // nil on non-existent proposed files
			scanned++
			securityFindings = append(securityFindings, s.patternEngine.CheckFile(s.graph, absFile, src)...)
			securityFindings = append(securityFindings, s.patternEngine.CheckImports(s.graph, absFile)...)
			// Sprint 27.5: norm-based detection — catches deviations from observed package conventions.
			securityFindings = append(securityFindings, s.patternEngine.CheckNorms(s.graph, absFile)...)
		}
	}

	// Escalate status when security scan found CRITICAL or HIGH findings.
	// Must run after the security scan so securityFindings is populated.
	// Prevents a false "ok"/"unprotected" when blocking security issues exist.
	if status == "ok" || status == "unprotected" {
		for _, f := range securityFindings {
			if f.Severity == security.SeverityCritical || f.Severity == security.SeverityHigh {
				status = "security_findings_found"
				break
			}
		}
	}

	result := map[string]interface{}{
		"status":     status,
		"violations": enrichArchViolations(violations),
	}
	if len(skipped) > 0 {
		result["skipped"] = skipped
	}
	if len(warnings) > 0 {
		result["warnings"] = warnings
	}
	if !hasRules {
		result["message"] = "No architectural rules active — plan was NOT validated. Add rules via upsert_rule() or configure them in synapses.json. Proceeding without rules gives no protection against architecture violations."
	}
	if safetyCheck != nil {
		result["safety_check"] = safetyCheck
	}
	if len(freshWarnings) > 0 {
		result["graph_freshness"] = fmt.Sprintf("⚠ %d file(s) modified very recently — graph may not reflect latest changes. Consider re-indexing: %s", len(freshWarnings), strings.Join(freshWarnings, "; "))
	}
	if len(logicWarnings) > 0 {
		result["logic_warnings"] = logicWarnings
	}
	if len(securityFindings) > 0 {
		result["security_findings"] = securityFindings
	}

	// Sprint 27.4: action_required — worst directive across all findings.
	// Present whenever there are any findings so the agent knows what to do.
	if len(securityFindings) > 0 || len(violations) > 0 {
		result["action_required"] = worstActionRequired(securityFindings, violations)
	}

	// Cross-project drift check: if any changed file has entities with
	// cross-project dependencies, check for drift in those dependencies.
	// This catches "planning to modify a function that depends on a changed
	// sibling entity" before any code is written.
	if s.federationResolver != nil && s.store != nil {
		fedCtx, fedCancel := context.WithTimeout(ctx, 2*time.Second)
		var driftedDeps []federation.CrossProjectDepStatus
		for _, change := range changes {
			if change.File == "" {
				continue
			}
			// Find entities in the changed file.
			fileNodes := s.graph.FindByFile(change.File)
			for _, fn := range fileNodes {
				deps := s.federationResolver.GetDepsForEntity(fedCtx, string(fn.ID), s.store)
				for _, dep := range deps {
					if dep.Drifted {
						driftedDeps = append(driftedDeps, dep)
					}
				}
			}
		}
		fedCancel()
		if len(driftedDeps) > 0 {
			result["cross_project_drift"] = map[string]interface{}{
				"count":   len(driftedDeps),
				"drifted": driftedDeps,
				"warning": "⚠ Entities in these files depend on sibling project functions whose signatures have changed. Review drift before implementing.",
			}
		}
	}

	// _summary: one-line digest for quick scanning.
	// Format: "Plan {status}: {V} violations, {W} logic warnings. Safety: {safety}."
	{
		status := "ok"
		if len(violations) > 0 {
			status = "violations_found"
		}
		safetyStatus := "pass"
		if safetyCheck != nil {
			if s, ok := safetyCheck["level"].(string); ok {
				safetyStatus = s
			}
		}
		if !hasRules {
			safetyStatus = "no_rules"
		}
		result["_summary"] = fmt.Sprintf("Plan %s: %d violation(s), %d security finding(s), %d logic warning(s), %d change(s). Safety: %s.",
			status, len(violations), len(securityFindings), len(logicWarnings), len(changes), safetyStatus)
	}

	return jsonResult(result)
}

// gitShowHEADContent returns the content of a file as it exists in the most
// recent git commit (HEAD). Returns (nil, false) when the file is new (not yet
// committed), git is unavailable, or the operation times out. Callers treat
// false as "no baseline available — all findings are new."
func gitShowHEADContent(repoRoot, relPath string) ([]byte, bool) {
	if repoRoot == "" || relPath == "" {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", repoRoot, "show", "HEAD:"+relPath).Output()
	if err != nil {
		return nil, false
	}
	return out, true
}

// categorizeFindingChanges partitions current (after-write) security findings
// against a baseline (before-write) set. Returns:
//   - newOnes:  findings present now but absent in the baseline (agent introduced them)
//   - existing: findings present in both baseline and now (pre-existed, not fixed)
//   - fixed:    findings present in the baseline but absent now (agent fixed them)
//
// Keyed on (PatternID, Target) so two findings of the same pattern on the same
// target are treated as the same violation regardless of message wording.
func categorizeFindingChanges(before, after []security.Violation) (newOnes, existing, fixed []security.Violation) {
	type key struct{ patternID, target string }

	beforeSet := make(map[key]security.Violation, len(before))
	for _, v := range before {
		beforeSet[key{v.PatternID, v.Target}] = v
	}

	afterSet := make(map[key]bool, len(after))
	for _, v := range after {
		k := key{v.PatternID, v.Target}
		afterSet[k] = true
		if _, inBefore := beforeSet[k]; inBefore {
			existing = append(existing, v)
		} else {
			newOnes = append(newOnes, v)
		}
	}

	for k, v := range beforeSet {
		if !afterSet[k] {
			fixed = append(fixed, v)
		}
	}
	return
}

// handleVerifyImplementation checks the actual graph state of written files
// against architectural rules and (optionally) a task's expectations.
// This is the write-side complement to validate_plan: validate_plan checks
// *before* writing, verify_implementation checks *after*.
func (s *Server) handleVerifyImplementation(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	filesRaw := stringArg(req, "files_written")
	if filesRaw == "" {
		return mcp.NewToolResultError(`files_written is required (e.g., ["internal/auth/service.go"])`), nil
	}

	var files []string
	if err := json.Unmarshal([]byte(filesRaw), &files); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid files_written JSON: %v", stripInternalPaths(err.Error()))), nil
	}
	if len(files) == 0 {
		return mcp.NewToolResultError("files_written must contain at least one file path"), nil
	}

	taskID := stringArg(req, "task_id")
	repoRoot := s.graph.Root()

	// callerRef is a minimal reference to a node that calls an exported symbol.
	type callerRef struct {
		Name string `json:"name"`
		File string `json:"file"`
		Line int    `json:"line"`
	}

	// signatureImpactEntry reports callers of one exported symbol whose signature changed,
	// or (when Warning is set) a high-fanin export in the written file whose edit
	// could break callers even without a signature change.
	type signatureImpactEntry struct {
		Symbol    string      `json:"symbol"`
		Type      string      `json:"type"`
		Before    string      `json:"before,omitempty"`    // signature before the change
		Signature string      `json:"signature,omitempty"` // current signature
		Warning   string      `json:"warning,omitempty"`   // BUG-EVAL-8: high-fanin blast-radius note
		Callers   []callerRef `json:"callers"`
	}

	// Per-file analysis.
	type fileReport struct {
		File                     string                 `json:"file"`
		InGraph                  bool                   `json:"in_graph"`
		NodeCount                int                    `json:"node_count"`
		Entities                 []string               `json:"entities,omitempty"`
		Violations               []archViolationResponse `json:"violations,omitempty"`
		SecurityFindings         []security.Violation   `json:"security_findings,omitempty"`          // all current findings
		SecurityFindingsNew      []security.Violation   `json:"security_findings_new,omitempty"`      // Sprint 27.3: introduced by this change
		SecurityFindingsExisting []security.Violation   `json:"security_findings_existing,omitempty"` // Sprint 27.3: pre-existed, not fixed
		SecurityFindingsFixed    []security.Violation   `json:"security_findings_fixed,omitempty"`    // Sprint 27.3: resolved by this change
		SignatureImpact          []signatureImpactEntry `json:"signature_impact,omitempty"`
		FreshnessWarning         string                 `json:"freshness_warning,omitempty"`
	}

	var reports []fileReport
	totalViolations := 0
	totalImpactWarnings := 0
	totalSecurityFindings := 0

	for _, f := range files {
		r := fileReport{File: f}

		nodes := s.graph.FindByFile(f)
		r.InGraph = len(nodes) > 0
		r.NodeCount = len(nodes)

		for _, n := range nodes {
			r.Entities = append(r.Entities, n.Name)
		}

		// Check architectural violations for this file.
		if r.InGraph {
			s.rulesMu.RLock()
			violations := s.config.CheckViolationsForFile(s.graph, f)
			s.rulesMu.RUnlock()
			r.Violations = enrichArchViolations(violations)
			totalViolations += len(violations)
		}

		// Signature impact: find exported entities in this file whose signature
		// actually changed since the last graph save. Only symbols with real changes
		// are reported — no noise for files where nothing changed.
		// Falls back to no-op when store is unavailable.
		const maxCallersPerSymbol = 30

		if s.store != nil {
			sigChanges, err := s.store.GetSignatureChanges(f)
			if err != nil {
				log.Printf("mcp: GetSignatureChanges(%s): %v", f, err)
			}
			for _, sc := range sigChanges {
				nid := graph.NodeID(sc.NodeID)
				impact, err := s.graph.ImpactAnalysis(nid, 1)
				if err != nil || impact == nil || impact.TotalAffected == 0 {
					continue
				}

				// Collect direct callers (depth-1 tier only).
				var callers []callerRef
				for _, tier := range impact.Tiers {
					if tier.Depth != 1 {
						continue
					}
					for i, ref := range tier.Nodes {
						if i >= maxCallersPerSymbol {
							break
						}
						callers = append(callers, callerRef{
							Name: ref.Name,
							File: ref.File,
							Line: ref.Line,
						})
					}
				}
				if len(callers) == 0 {
					continue
				}

				r.SignatureImpact = append(r.SignatureImpact, signatureImpactEntry{
					Symbol:    sc.Name,
					Type:      sc.NodeType,
					Signature: sc.NewSig,
					Before:    sc.OldSig,
					Callers:   callers,
				})
				totalImpactWarnings++
			}
		}

		// BUG-EVAL-8: High-fanin exported symbols — blast-radius warning even when
		// signature didn't change. Any edit to a file containing widely-used exports
		// risks breaking callers; surface these so agents review call sites.
		const (
			highFaninThreshold  = 10 // callers needed to trigger a warning
			maxExportsToCheck   = 50 // cap ImpactAnalysis scans on large files
			maxHighFaninEntries = 5  // max new entries added per file
		)
		if r.InGraph {
			alreadyReported := make(map[string]bool)
			for _, e := range r.SignatureImpact {
				alreadyReported[e.Symbol] = true
			}
			// Sort candidates by fanin (in-edge count) descending so the most
			// widely-used exports are checked first. This ensures the scan cap
			// (maxExportsToCheck) never skips a genuinely high-fanin symbol in
			// favour of an alphabetically-earlier low-fanin one.
			// graph.Fanin is O(1) — reads the pre-built in-edge index.
			sortedNodes := make([]*graph.Node, 0, len(nodes))
			for _, n := range nodes {
				if !n.Exported {
					continue
				}
				if n.Type != graph.NodeFunction && n.Type != graph.NodeMethod &&
					n.Type != graph.NodeStruct && n.Type != graph.NodeInterface {
					continue
				}
				if alreadyReported[n.Name] {
					continue
				}
				sortedNodes = append(sortedNodes, n)
			}
			// callFanin counts CALLS and IMPLEMENTS incoming edges — the same two edge
			// types that ImpactAnalysis traverses — so the sort key matches what the
			// blast-radius check actually measures. EMBEDS edges are excluded because
			// they don’t represent callers or implementors (just struct composition)
			// and would inflate the score of struct nodes with no real dependents.
			callFanin := func(id graph.NodeID) int {
				n := 0
				for _, e := range s.graph.InEdges(id) {
					if e.Type == graph.EdgeCalls || e.Type == graph.EdgeImplements {
						n++
					}
				}
				return n
			}
			sort.Slice(sortedNodes, func(i, j int) bool {
				fi, fj := callFanin(sortedNodes[i].ID), callFanin(sortedNodes[j].ID)
				if fi != fj {
					return fi > fj // highest CALLS-fanin first — most important symbols scanned first
				}
				return sortedNodes[i].Name < sortedNodes[j].Name // stable alphabetical tiebreaker
			})
			newEntries := 0
			checked := 0
			for _, n := range sortedNodes {
				if checked >= maxExportsToCheck || newEntries >= maxHighFaninEntries {
					break
				}
				checked++
				impact, err := s.graph.ImpactAnalysis(n.ID, 1)
				if err != nil || impact == nil || impact.TotalAffected < highFaninThreshold {
					continue
				}
				var callers []callerRef
				for _, tier := range impact.Tiers {
					if tier.Depth != 1 {
						continue
					}
					for i, ref := range tier.Nodes {
						if i >= maxCallersPerSymbol {
							break
						}
						callers = append(callers, callerRef{Name: ref.Name, File: ref.File, Line: ref.Line})
					}
				}
				if len(callers) < highFaninThreshold {
					continue
				}
				r.SignatureImpact = append(r.SignatureImpact, signatureImpactEntry{
					Symbol:  n.Name,
					Type:    string(n.Type),
					Warning: fmt.Sprintf("high-fanin export (%d callers) — blast radius risk even if signature unchanged", len(callers)),
					Callers: callers,
				})
				totalImpactWarnings++
				newEntries++
			}
		}

		// Freshness check + security pattern scan.
		absFile := f
		if repoRoot != "" && !filepath.IsAbs(absFile) {
			absFile = filepath.Join(repoRoot, absFile)
		}
		// Security: reject paths that escape the project root.
		if !pathWithinRoot(repoRoot, absFile) {
			reports = append(reports, r)
			continue
		}

		// Read content once; used by both freshness check and security scan.
		fileContent, _ := os.ReadFile(absFile)

		if fi, err := os.Stat(absFile); err == nil {
			if age := time.Since(fi.ModTime()); age < 10*time.Second {
				r.FreshnessWarning = fmt.Sprintf("modified %s ago — graph may be stale", age.Round(time.Second))
			}
		}

		// Sprint 26.7: security pattern findings (CheckFile) + Sprint 26.11: unknown import detection (CheckImports).
		// Sprint 27.3: before/after comparison to attribute findings as new, existing, or fixed.
		// Sprint 27.5: norm-based violation detection (CheckNorms) — fires for observed convention deviations.
		// Sprint 28.5: LSP-triggered re-verification upgrades MEDIUM→HIGH / LOW→MEDIUM when LSP confirms entity.
		if s.graph != nil && s.patternEngine != nil {
			afterFindings := s.patternEngine.CheckFile(s.graph, absFile, fileContent)
			afterFindings = append(afterFindings, s.patternEngine.CheckImports(s.graph, absFile)...)
			afterFindings = append(afterFindings, s.patternEngine.CheckNorms(s.graph, absFile)...)
			if s.lspManager != nil {
				afterFindings = security.NewLSPEnricher(s.lspManager).Enrich(ctx, afterFindings, s.graph)
			}
			r.SecurityFindings = afterFindings
			totalSecurityFindings += len(afterFindings)

			// Attempt to load the pre-write content from git HEAD to classify
			// findings as new (introduced), existing (pre-existed), or fixed.
			// relPath is the project-relative path needed by git.
			var relPath string
			if repoRoot != "" {
				if rel, err := filepath.Rel(repoRoot, absFile); err == nil {
					relPath = rel
				}
			}
			if priorContent, ok := gitShowHEADContent(repoRoot, relPath); ok {
				beforeFindings := s.patternEngine.CheckFile(s.graph, absFile, priorContent)
				// CheckImports and CheckNorms are graph-derived (not content-derived), so
				// their results are the same before and after; include both to avoid
				// mis-classifying pre-existing findings as "new."
				beforeFindings = append(beforeFindings, s.patternEngine.CheckImports(s.graph, absFile)...)
				beforeFindings = append(beforeFindings, s.patternEngine.CheckNorms(s.graph, absFile)...)
				newOnes, existing, fixed := categorizeFindingChanges(beforeFindings, afterFindings)
				if len(newOnes) > 0 {
					r.SecurityFindingsNew = newOnes
				}
				if len(existing) > 0 {
					r.SecurityFindingsExisting = existing
				}
				if len(fixed) > 0 {
					r.SecurityFindingsFixed = fixed
				}
			} else {
				// No baseline available (new file or non-git repo): all current
				// findings are attributed as "new."
				if len(afterFindings) > 0 {
					r.SecurityFindingsNew = afterFindings
				}
			}
		}

		reports = append(reports, r)
	}

	// Sprint 27.3: project-scope security patterns (e.g. cross-transport auth consistency).
	// Run once after all per-file checks — these patterns require the full project graph.
	var projectSecurityFindings []security.Violation
	if s.graph != nil && s.patternEngine != nil {
		projectSecurityFindings = s.patternEngine.CheckProject(s.graph)
		if s.lspManager != nil {
			projectSecurityFindings = security.NewLSPEnricher(s.lspManager).Enrich(ctx, projectSecurityFindings, s.graph)
		}
		totalSecurityFindings += len(projectSecurityFindings)
	}

	// Task-level verification: compare actual graph entities against task's linked_nodes
	// and check spec item coverage (Sprint 25.1).
	var taskVerification map[string]interface{}
	if taskID != "" && s.store != nil {
		task, err := s.store.GetTask(taskID)
		if err == nil && task != nil {
			tv := map[string]interface{}{
				"task_id":    taskID,
				"task_title": task.Title,
			}

			// linked_nodes check: which graph entities are still reachable.
			if len(task.LinkedNodes) > 0 {
				var found, missing []string
				for _, nodeID := range task.LinkedNodes {
					if n := s.graph.GetNode(graph.NodeID(nodeID)); n != nil {
						found = append(found, n.Name)
					} else {
						missing = append(missing, nodeID)
					}
				}
				tv["linked_found"] = found
				tv["linked_missing"] = missing
			}

			// Spec coverage check (Sprint 25.1): warn when spec items remain incomplete.
			// This is the "completion illusion" guard — agents often declare a task done
			// at 30-40% completion. Informational only: doesn't block or change status.
			if len(task.SpecItems) > 0 {
				doneCount := 0
				var pendingLabels []string
				for _, item := range task.SpecItems {
					if item.Done {
						doneCount++
					} else {
						label := item.Label
						if label == "" {
							label = item.ID // fall back to ID if label is empty
						}
						pendingLabels = append(pendingLabels, label)
					}
				}
				total := len(task.SpecItems)
				tv["spec_coverage"] = map[string]interface{}{
					"total":    total,
					"done":     doneCount,
					"pending":  total - doneCount,
					"complete": doneCount == total,
				}
				if doneCount < total {
					tv["spec_coverage_warning"] = fmt.Sprintf(
						"%d of %d spec items are not yet marked complete: %s. "+
							"Mark items done via tasks(action=update_spec_item) before closing this task.",
						total-doneCount, total, strings.Join(pendingLabels, ", "),
					)
				}
			}

			// Multi-file change tracking (Sprint 25.2): warn when tracked files are
			// absent from the files_written list. Uses base name matching as a fallback
			// so that "internal/auth/handler.go" matches "handler.go" in files_written.
			if len(task.TrackedFiles) > 0 {
				// Build a set of all written paths AND their base names for loose matching.
				writtenSet := make(map[string]struct{}, len(files)*2)
				for _, f := range files {
					writtenSet[f] = struct{}{}
					writtenSet[filepath.Base(f)] = struct{}{}
				}
				var unmodified []string
				for _, tf := range task.TrackedFiles {
					_, byFull := writtenSet[tf]
					_, byBase := writtenSet[filepath.Base(tf)]
					if !byFull && !byBase {
						unmodified = append(unmodified, tf)
					}
				}
				total := len(task.TrackedFiles)
				modifiedCount := total - len(unmodified)
				tv["file_tracking"] = map[string]interface{}{
					"total_tracked":    total,
					"modified_count":   modifiedCount,
					"unmodified_count": len(unmodified),
					"complete":         len(unmodified) == 0,
				}
				if len(unmodified) > 0 {
					tv["file_tracking_warning"] = fmt.Sprintf(
						"%d of %d tracked files have not been modified in this validate call: %s. "+
							"These files were identified as needing changes for this task. "+
							"Ensure they are updated before marking the task complete.",
						len(unmodified), total, strings.Join(unmodified, ", "),
					)
				}
			}

			taskVerification = tv
		}
	}

	// Build result.
	status := "pass"
	if totalViolations > 0 {
		status = "violations_found"
	}
	// Escalate status when CRITICAL or HIGH security findings exist — including
	// project-scope findings from CheckProject (Sprint 27.3). An agent that reads
	// only the top-level status must not get a false "pass" while there are
	// blocking security issues. Sprint 27.4 adds full severity-tier enforcement;
	// this ensures the status field is honest now.
	if totalSecurityFindings > 0 && (status == "pass" || status == "pending_indexing") {
		// Check per-file findings.
		for _, r := range reports {
			for _, f := range r.SecurityFindings {
				if f.Severity == security.SeverityCritical || f.Severity == security.SeverityHigh {
					status = "security_findings_found"
					goto doneSecurityStatus
				}
			}
		}
		// Check project-scope findings.
		for _, f := range projectSecurityFindings {
			if f.Severity == security.SeverityCritical || f.Severity == security.SeverityHigh {
				status = "security_findings_found"
				break
			}
		}
	}
doneSecurityStatus:

	// P3-5: emit validation outcome event.
	// Sprint 27.3: include SecurityFindingCount so pulse analytics can distinguish
	// architectural violations from security pattern violations.
	if pc := s.getPulseClient(); pc != nil {
		agentIDForPulse := stringArg(req, "agent_id")
		projID := s.projectID
		pc.RecordValidationEvent(pulse.ValidationEvent{
			ToolName:             "verify_implementation",
			Status:               status,
			ViolationCount:       totalViolations,
			SecurityFindingCount: totalSecurityFindings,
			AgentID:              agentIDForPulse,
			ProjectID:            projID,
		})
	}

	// Check if any files are not yet in the graph.
	notIndexed := 0
	for _, r := range reports {
		if !r.InGraph {
			notIndexed++
		}
	}
	if notIndexed > 0 && status == "pass" {
		status = "pending_indexing"
	}

	result := map[string]interface{}{
		"status":                  status,
		"total_violations":        totalViolations,
		"impact_warnings":         totalImpactWarnings,
		"total_security_findings": totalSecurityFindings,
		"files":                   reports,
	}
	if len(projectSecurityFindings) > 0 {
		result["project_security_findings"] = projectSecurityFindings
	}
	if taskVerification != nil {
		result["task_verification"] = taskVerification
	}
	if notIndexed > 0 {
		result["indexing_hint"] = fmt.Sprintf("%d file(s) not yet in graph — wait for indexing or re-run verify_implementation.", notIndexed)
	}
	if totalImpactWarnings > 0 {
		result["impact_hint"] = fmt.Sprintf("%d exported symbol(s) have callers — review signature_impact in each file to ensure call sites are still valid.", totalImpactWarnings)
	}

	// Sprint 27.4: action_required — worst directive across all security findings
	// and architectural violations across all files and project-scope findings.
	if totalViolations > 0 || totalSecurityFindings > 0 {
		var allSec []security.Violation
		var allArch []config.Violation
		for _, r := range reports {
			allSec = append(allSec, r.SecurityFindings...)
			// r.Violations is []archViolationResponse; extract the embedded config.Violation
			// for worstActionRequired (which reads from config.Violation.Severity).
			for _, ev := range r.Violations {
				allArch = append(allArch, ev.Violation)
			}
		}
		allSec = append(allSec, projectSecurityFindings...)
		result["action_required"] = worstActionRequired(allSec, allArch)
	}

	// Auto-record episode when post-implementation violations are found.
	if totalViolations > 0 && s.store != nil {
		var fileSummary []string
		for _, r := range reports {
			if len(r.Violations) > 0 {
				fileSummary = append(fileSummary, fmt.Sprintf("%s: %d violation(s)", r.File, len(r.Violations)))
			}
		}
		ep := store.Episode{
			EpisodeType: "failure",
			Outcome:     "failure",
			Trigger:     "verify_implementation found post-write violations",
			Decision:    fmt.Sprintf("Post-implementation violations in: %s", strings.Join(fileSummary, "; ")),
			Rationale:   "Code was written that violates architectural rules. Fix violations or update rules.",
			Tags:        `["auto","verify_implementation","violation"]`,
			Importance:  0.7,
		}
		s.goBackground(func() {
			if _, err := s.store.RememberEpisode(ep); err != nil {
				log.Printf("mcp: auto-record verify_implementation episode: %v", err)
			}
		})
	}

	// Sprint 27.3: auto-record episode when CRITICAL or HIGH security findings are
	// introduced, so future sessions know about persistent security issues.
	// Sprint 27.10: deduplicate episodes — only write for findings not yet episoded
	// this session (persistent CheckProject findings fired on every call previously).
	if s.store != nil {
		sessID := s.getSynapseSessionID(SessionIDFromContext(ctx))
		var critHighFindings []string
		// Collect per-file findings that haven't been episoded this session.
		// CheckAndMarkEpisoded is atomic — prevents TOCTOU duplicates under concurrency.
		for _, r := range reports {
			for _, f := range r.SecurityFindingsNew {
				if f.Severity == security.SeverityCritical || f.Severity == security.SeverityHigh {
					if !s.findingQueue.CheckAndMarkEpisoded(sessID, f.PatternID, f.Target) {
						continue
					}
					critHighFindings = append(critHighFindings, fmt.Sprintf("%s in %s", f.PatternName, r.File))
				}
			}
		}
		// Collect project-scope findings that haven't been episoded this session.
		for _, f := range projectSecurityFindings {
			if f.Severity == security.SeverityCritical || f.Severity == security.SeverityHigh {
				if !s.findingQueue.CheckAndMarkEpisoded(sessID, f.PatternID, f.Target) {
					continue
				}
				critHighFindings = append(critHighFindings, fmt.Sprintf("%s (project-scope)", f.PatternName))
			}
		}
		if len(critHighFindings) > 0 {
			ep := store.Episode{
				EpisodeType: "failure",
				Outcome:     "failure",
				Trigger:     "verify_implementation found new CRITICAL/HIGH security findings",
				Decision:    fmt.Sprintf("Security findings introduced: %s", strings.Join(critHighFindings, "; ")),
				Rationale:   "Code was written that violates security patterns. Fix findings before merging.",
				Tags:        `["auto","verify_implementation","security"]`,
				Importance:  0.9,
			}
			s.goBackground(func() {
				if _, err := s.store.RememberEpisode(ep); err != nil {
					log.Printf("mcp: auto-record security episode: %v", err)
				}
			})
		}
	}

	// _summary: one-line digest for quick scanning. Use the outer status which
	// already accounts for all escalation paths (violations, security, indexing).
	result["_summary"] = fmt.Sprintf("%s: %d file(s), %d violation(s), %d security finding(s), %d impact warning(s)",
		status, len(files), totalViolations, totalSecurityFindings, totalImpactWarnings)

	return jsonResult(result)
}

// handleGetViolations returns all current architectural rule violations.
// Optional rule_id filters to a specific rule. Optional include_log=true appends the historical log.
func (s *Server) handleGetViolations(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ruleIDFilter := stringArg(req, "rule_id")
	includeLog, _ := req.GetArguments()["include_log"].(bool)
	logLimit := 50
	if l, ok := req.GetArguments()["log_limit"].(float64); ok && l > 0 {
		logLimit = int(l)
	}

	s.rulesMu.RLock()
	violations := s.config.CheckViolations(s.graph)
	s.rulesMu.RUnlock()

	// Apply optional rule_id filter.
	if ruleIDFilter != "" {
		filtered := make([]config.Violation, 0, len(violations))
		for _, v := range violations {
			if v.RuleID == ruleIDFilter {
				filtered = append(filtered, v)
			}
		}
		violations = filtered
	}

	// Persist to the audit log so agents can query violation history later.
	if s.store != nil && len(violations) > 0 {
		if err := s.store.LogViolations(violations); err != nil {
			log.Printf("mcp: log violations: %v", err)
		}
	}

	// Brain enrichment: add plain-English LLM explanations for each violation.
	if bc := s.getBrainClient(); bc != nil && len(violations) > 0 {
		// Single shared deadline for all ExplainViolation calls: 3s total budget,
		// not 3s per violation. Avoids N context allocations in the hot path.
		deadline := time.Now().Add(3 * time.Second)
		for i := range violations {
			v := &violations[i]
			fromNode := s.graph.GetNode(v.FromNode)
			toNode := s.graph.GetNode(v.ToNode)
			sourceFile := ""
			targetName := string(v.ToNode)
			if fromNode != nil {
				sourceFile = fromNode.File
			}
			if toNode != nil {
				targetName = toNode.Name
			}
			violCtx, violCancel := context.WithDeadline(ctx, deadline)
			explanation, fix := bc.ExplainViolation(violCtx, brain.ViolationRequest{
				RuleID:       v.RuleID,
				RuleSeverity: v.Severity,
				Description:  v.Description,
				SourceFile:   sourceFile,
				TargetName:   targetName,
			})
			violCancel()
			if explanation != "" {
				v.Explanation = explanation
			}
			if fix != "" && v.SuggestedFix == "" {
				v.SuggestedFix = fix
			}
		}
	}

	summary := "no violations found"
	if len(violations) > 0 {
		errorCount := 0
		for _, v := range violations {
			if v.Severity == "error" {
				errorCount++
			}
		}
		summary = fmt.Sprintf("%d violations (%d errors)", len(violations), errorCount)
	}

	// P7-15: emit validation event for standalone get_violations.
	if pc := s.getPulseClient(); pc != nil {
		st := "ok"
		if len(violations) > 0 {
			st = "violations_found"
		}
		pc.RecordValidationEvent(pulse.ValidationEvent{
			ToolName:       "get_violations",
			Status:         st,
			ViolationCount: len(violations),
			ProjectID:      s.projectID,
		})
	}

	result := map[string]interface{}{
		"summary":    summary,
		"violations": violations,
	}

	// Include historical log when requested.
	if includeLog && s.store != nil {
		if entries, err := s.store.GetViolationLog(ruleIDFilter, logLimit); err == nil {
			result["log"] = entries
		}
	}

	// R32: Append open quality gaps so agents see the full quality picture in
	// one call. Gaps are agent-discovered findings (reasoning-based) vs.
	// violations which are deterministic rule checks.
	// Always write both keys (even when store is nil) so callers can assert
	// m["quality_gap_count"] safely without a nil-key panic.
	if s.store != nil {
		if gaps, err := s.store.GetGaps(store.GapFilter{Status: "open"}); err == nil && len(gaps) > 0 {
			result["open_quality_gaps"] = gaps
			result["quality_gap_count"] = len(gaps)
		} else {
			result["open_quality_gaps"] = []interface{}{}
			result["quality_gap_count"] = 0
		}
	} else {
		result["open_quality_gaps"] = []interface{}{}
		result["quality_gap_count"] = 0
	}

	return jsonResult(result)
}

// handleUpsertRule creates or updates a dynamic architectural rule.
// The rule is persisted to SQLite first (so failure is safe — in-memory state
// stays consistent) and then atomically upserted into s.config.Rules.
func (s *Server) handleUpsertRule(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ruleID := stringArg(req, "rule_id")
	description := stringArg(req, "description")
	severity := stringArg(req, "severity")

	if ruleID == "" || description == "" {
		return mcp.NewToolResultError("rule_id and description are required"), nil
	}
	if severity != "error" && severity != "warning" {
		return mcp.NewToolResultError("severity must be 'error' or 'warning'"), nil
	}

	// R28: Semantic Firewall — reject rule creation when the triggering context
	// is not user-authored code. Two layers:
	// 1. Explicit: agent declared context_source="external"|"generated".
	// 2. Automatic: any entity name in rule_id or description resolves to a
	//    non-user-authored graph node (generated protobuf, vendored lib, etc.).
	if src := stringArg(req, "context_source"); src == "external" || src == "generated" {
		return mcp.NewToolResultError(
			fmt.Sprintf(
				"upsert_rule blocked: context_source=%q — architectural rules must be derived "+
					"from user-authored code, not %s content. "+
					"Re-evaluate the pattern against your own codebase before creating a rule.",
				src, src,
			),
		), nil
	}
	if detectedProv, detectedNode := s.detectRuleProvenance(ruleID, description); detectedProv != "" {
		return mcp.NewToolResultError(
			fmt.Sprintf(
				"upsert_rule blocked: the entity %q referenced in this rule is %s code, not user-authored. "+
					"Architectural rules must be grounded in your own codebase.",
				detectedNode, detectedProv,
			),
		), nil
	}

	fe := config.ForbiddenEdge{
		EdgeType:        graph.EdgeType(stringArg(req, "edge_type")),
		FromFilePattern: stringArg(req, "from_file_pattern"),
		ToFilePattern:   stringArg(req, "to_file_pattern"),
		ToNamePattern:   stringArg(req, "to_name_pattern"),
		PathPattern:     parsePathPattern(stringArg(req, "path_pattern")),
	}

	// Auto-detect rule type: if no ForbiddenEdge fields are set, this is a
	// behavioral/agent rule (conversation-level constraint), not a structural
	// code-graph rule. Agent rules are surfaced in session_init as
	// agent_constraints rather than being checked against the call graph.
	ruleType := "structural"
	if fe.EdgeType == "" && fe.FromFilePattern == "" && fe.ToFilePattern == "" &&
		fe.ToNamePattern == "" && len(fe.PathPattern) == 0 {
		ruleType = "agent"
	}

	rule := config.Rule{
		ID:            ruleID,
		Description:   description,
		Severity:      severity,
		ForbiddenEdge: fe,
		RuleType:      ruleType,
	}

	// Persist first — if the DB write fails, don't mutate in-memory state.
	if s.store != nil {
		if err := s.store.UpsertDynamicRule(rule); err != nil {
			return toolError("persist rule", err)
		}
	}

	// Atomically upsert into the in-memory rule slice.
	s.rulesMu.Lock()
	upserted := false
	for i, r := range s.config.Rules {
		if r.ID == ruleID {
			s.config.Rules[i] = rule
			upserted = true
			break
		}
	}
	if !upserted {
		s.config.Rules = append(s.config.Rules, rule)
	}
	s.rulesMu.Unlock()

	// Retroactive scan: check existing graph edges against the new rule and
	// log any violations so get_violations() surfaces them immediately without
	// requiring a new validate_plan call.
	// Agent rules have no ForbiddenEdge, so skip the graph scan for them.
	if s.store != nil && ruleType == "structural" {
		r := rule
		s.goBackground(func() {
			snapshot := config.Config{Rules: []config.Rule{r}}
			violations := snapshot.CheckViolations(s.graph)
			if len(violations) > 0 {
				if err := s.store.LogViolations(violations); err != nil {
					log.Printf("mcp: log violations (upsert_rule): %v", err)
				}
			}
		})
	}

	// P7-14: emit rule eval event for rule creation/update.
	if pc := s.getPulseClient(); pc != nil {
		pc.RecordRuleEvalEvent(pulse.RuleEvalEvent{
			RulesEvaluated: 1, ProjectID: s.projectID,
		})
	}

	return jsonResult(map[string]interface{}{
		"status":    "ok",
		"rule_id":   ruleID,
		"rule_type": ruleType,
		"message":   fmt.Sprintf("Rule %q (%s) is now active.", ruleID, ruleType),
	})
}

// handleDeleteRule removes a dynamic architectural rule by ID.
// Both the in-memory config and the persistent SQLite store are updated so the
// deletion survives daemon restart. Returns status="not_found" when the rule
// doesn't exist — not an error, since idempotent deletion is safe.
func (s *Server) handleDeleteRule(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	ruleID := strings.TrimSpace(stringArg(req, "rule_id"))
	if ruleID == "" {
		return mcp.NewToolResultError("rule_id is required"), nil
	}

	// Delete from store first so a store failure leaves in-memory state unchanged.
	// If we removed from memory first and then the store write failed, the rule
	// would be gone this session but reload on restart — a silent inconsistency.
	storeDeleted := false
	if s.store != nil {
		deleted, err := s.store.DeleteDynamicRule(ruleID)
		if err != nil {
			return toolError("delete rule", err)
		}
		storeDeleted = deleted
	}

	// Remove from in-memory config (covers both dynamic rules and config-file
	// rules that happen to share the same ID).
	removed := false
	s.rulesMu.Lock()
	if s.config != nil {
		newRules := s.config.Rules[:0]
		for _, r := range s.config.Rules {
			if r.ID == ruleID {
				removed = true
			} else {
				newRules = append(newRules, r)
			}
		}
		s.config.Rules = newRules
	}
	s.rulesMu.Unlock()
	removed = removed || storeDeleted

	if !removed {
		return jsonResult(map[string]interface{}{
			"status":  "not_found",
			"rule_id": ruleID,
			"message": fmt.Sprintf("Rule %q does not exist — nothing to delete.", ruleID),
		})
	}
	return jsonResult(map[string]interface{}{
		"status":  "ok",
		"rule_id": ruleID,
		"message": fmt.Sprintf("Rule %q deleted and no longer active.", ruleID),
	})
}

// detectRuleProvenance auto-detects whether a rule references non-user-authored
// entities. It tokenises ruleID and description, looks up each token in the
// graph, and returns (provenance, entityName) for the first non-user-authored
// node found. Returns ("", "") when all referenced entities are user-authored
// or when no tokens match graph nodes.
// This powers the automatic layer of the R28 Semantic Firewall so agents
// don't need to declare context_source="generated" explicitly.
func (s *Server) detectRuleProvenance(ruleID, description string) (string, string) {
	if s.graph == nil {
		return "", ""
	}
	seen := make(map[string]bool)
	for _, word := range strings.FieldsFunc(ruleID+" "+description, func(r rune) bool {
		return ('a' > r || r > 'z') && ('A' > r || r > 'Z') && ('0' > r || r > '9') && r != '_'
	}) {
		if len(word) < 3 || seen[word] {
			continue
		}
		seen[word] = true
		for _, n := range s.graph.FindByName(word) {
			p := string(n.Provenance)
			if p == "generated" || p == "vendored" || p == "external" {
				return p, n.Name
			}
		}
	}
	return "", ""
}

// parsePathPattern parses a comma-separated edge-type string from the
// path_pattern tool argument into a typed slice. Returns nil for empty input.
// Whitespace around each element is trimmed. Example: "CALLS, CALLS" → [CALLS, CALLS].
func parsePathPattern(s string) []graph.EdgeType {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]graph.EdgeType, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, graph.EdgeType(p))
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ── validate(phase="pre_write") ─────────────────────────────────────────────

// filePathInTextRe matches source file paths embedded in natural-language text.
// Covers Go, TypeScript/JavaScript, Python, Java, Rust, C/C++, and other common
// extensions. The match must be preceded by a word boundary or path separator.
var filePathInTextRe = regexp.MustCompile(
	`(?:^|[\s('"])` +
		`(` +
		`[\w./\-]+` +
		`\.(?:go|ts|tsx|js|jsx|mjs|cjs|py|java|rs|rb|php|cs|cpp|c|h|swift|kt|scala)` +
		`)` +
		`(?:[\s,'":)\]]|$)`,
)

// extractFilePathsFromText scans a natural-language string for source file path
// patterns and returns up to 20 unique matches. Paths must end in a recognised
// source-file extension to avoid false positives on common English words.
func extractFilePathsFromText(text string) []string {
	matches := filePathInTextRe.FindAllStringSubmatch(text, 40)
	seen := make(map[string]bool)
	var out []string
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		p := m[1]
		if !seen[p] && len(out) < 20 {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// preWriteFileResult holds the per-file analysis produced by handleValidatePreWrite.
type preWriteFileResult struct {
	File               string                  `json:"file"`
	IsNew              bool                    `json:"is_new"`
	SecurityFindings   []security.Violation    `json:"security_findings,omitempty"`
	ArchRuleViolations []archViolationResponse  `json:"arch_rule_violations,omitempty"`
	Norms              []string                `json:"norms,omitempty"`
}

// handleValidatePreWrite implements validate(phase="pre_write").
//
// The agent describes a proposed change in natural language (e.g. "adding a
// POST /api/users handler to handlers/users.go"). Synapses analyses the target
// area — existing or sibling files — and returns security constraints, norms,
// and architectural rule findings BEFORE any code is written.
//
// Differences from phase="pre":
//   - "pre" accepts a structured JSON changes array and checks proposed call-graph
//     edges against architectural rules (overlay-graph approach).
//   - "pre_write" accepts a natural-language description plus optional file paths.
//     For existing files it runs the pattern engine directly. For new files it
//     scans sibling files in the same directory to infer what security patterns
//     the new file must satisfy.
//
// Security timing: Pre-Write (agent calls before writing any code).
func (s *Server) handleValidatePreWrite(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	description := stringArg(req, "description")
	filesRaw := stringArg(req, "files")

	// At least one input is required.
	if description == "" && filesRaw == "" {
		return mcp.NewToolResultError(
			`description or files is required ` +
				`(e.g., description="adding a POST /api/users handler to handlers/users.go", ` +
				`files='["handlers/users.go"]')`), nil
	}

	// Parse explicit file list.
	var targetFiles []string
	if filesRaw != "" {
		if err := json.Unmarshal([]byte(filesRaw), &targetFiles); err != nil {
			return mcp.NewToolResultError(
				fmt.Sprintf("invalid files JSON: %v", stripInternalPaths(err.Error()))), nil
		}
	}
	// Fall back to extracting paths from the natural-language description.
	if len(targetFiles) == 0 && description != "" {
		targetFiles = extractFilePathsFromText(description)
	}

	// No graph: return an advisory without file analysis.
	if s.graph == nil {
		result := map[string]interface{}{
			"status":               "unprotected",
			"description_received": description,
			"message": "Graph not loaded — start the daemon with a valid project path " +
				"to enable pre-write security analysis.",
		}
		if len(targetFiles) > 0 {
			result["files_requested"] = targetFiles
		}
		result["_summary"] = "pre_write: graph unavailable. No security analysis performed."
		return jsonResult(result)
	}

	repoRoot := s.graph.Root()

	const (
		maxPreWriteFiles    = 10 // max target files analysed per call
		maxSiblingFiles     = 10 // max sibling files scanned for new-file inference
	)

	fileResults := make([]preWriteFileResult, 0, len(targetFiles))
	var allSecFindings []security.Violation
	var allArchViolations []config.Violation
	normSet := make(map[string]bool)

	analyzed := 0
	for _, f := range targetFiles {
		if analyzed >= maxPreWriteFiles {
			break
		}

		// Resolve to absolute path for all filesystem and security operations.
		absFile := f
		if repoRoot != "" && !filepath.IsAbs(absFile) {
			absFile = filepath.Join(repoRoot, absFile)
		}
		if !pathWithinRoot(repoRoot, absFile) {
			continue // silently skip traversal attempts
		}
		analyzed++

		fr := preWriteFileResult{File: f}

		_, statErr := os.Stat(absFile)
		isNew := statErr != nil
		fr.IsNew = isNew

		if !isNew {
			// ── Existing file: analyse it directly ──────────────────────────
			src, _ := os.ReadFile(absFile)

			if s.patternEngine != nil {
				findings := s.patternEngine.CheckFile(s.graph, absFile, src)
				findings = append(findings, s.patternEngine.CheckImports(s.graph, absFile)...)
				fr.SecurityFindings = findings
				allSecFindings = append(allSecFindings, findings...)
			}

			// Graph lookups (observeFileNorms, CheckViolationsForFile) require
			// relative paths — the graph stores project-relative paths, not absolute.
			relFile := f
			if repoRoot != "" && filepath.IsAbs(f) {
				if rel, err := filepath.Rel(repoRoot, f); err == nil {
					relFile = rel
				}
			}

			for _, n := range observeFileNorms(s.graph, relFile) {
				fr.Norms = append(fr.Norms, n)
				normSet[n] = true
			}

			s.rulesMu.RLock()
			if s.config != nil {
				archViol := s.config.CheckViolationsForFile(s.graph, relFile)
				fr.ArchRuleViolations = enrichArchViolations(archViol)
				allArchViolations = append(allArchViolations, archViol...)
			}
			s.rulesMu.RUnlock()
		} else {
			// ── New file: scan siblings to infer expected patterns ───────────
			dir := filepath.Dir(absFile)
			if !pathWithinRoot(repoRoot, dir) {
				fileResults = append(fileResults, fr)
				continue
			}

			// Glob sibling files with the same extension.
			ext := strings.ToLower(filepath.Ext(f))
			globPattern := filepath.Join(dir, "*"+ext)
			if ext == "" {
				globPattern = filepath.Join(dir, "*")
			}
			siblings, _ := filepath.Glob(globPattern)

			seenPatternID := make(map[string]bool)
			sibCount := 0
			for _, sib := range siblings {
				if sibCount >= maxSiblingFiles {
					break
				}
				if !pathWithinRoot(repoRoot, sib) {
					continue
				}
				sibCount++

				// Relative path for graph lookups (graph stores relative paths).
				relSib := sib
				if repoRoot != "" {
					if rel, err := filepath.Rel(repoRoot, sib); err == nil {
						relSib = rel
					}
				}

				if s.patternEngine != nil {
					sibSrc, _ := os.ReadFile(sib)
					for _, v := range s.patternEngine.CheckFile(s.graph, sib, sibSrc) {
						if seenPatternID[v.PatternID] {
							continue
						}
						seenPatternID[v.PatternID] = true
						// Reframe as a prospective constraint for the new file:
						// "pattern fires on sibling → new file must comply too."
						constraint := v
						constraint.Target = fmt.Sprintf("(new file — sibling pattern from %s)",
							filepath.Base(v.File))
						constraint.File = f
						fr.SecurityFindings = append(fr.SecurityFindings, constraint)
						allSecFindings = append(allSecFindings, constraint)
					}
				}

				for _, n := range observeFileNorms(s.graph, relSib) {
					if !normSet[n] {
						normSet[n] = true
						fr.Norms = append(fr.Norms, n)
					}
				}
			}
		}

		fileResults = append(fileResults, fr)
	}

	// Derive top-level status from findings.
	status := "clear"
	critCount, highCount := 0, 0
	for _, v := range allSecFindings {
		switch v.Severity {
		case security.SeverityCritical:
			critCount++
		case security.SeverityHigh:
			highCount++
		}
	}
	if critCount > 0 || highCount > 0 {
		status = "requires_attention"
	} else if len(allSecFindings) > 0 || len(allArchViolations) > 0 {
		status = "findings_found"
	} else if s.patternEngine == nil && (s.config == nil || len(s.config.Rules) == 0) {
		status = "unprotected"
	}

	// Deduplicated global norms list.
	var allNorms []string
	for n := range normSet {
		allNorms = append(allNorms, n)
	}
	sort.Strings(allNorms)

	result := map[string]interface{}{
		"status":               status,
		"description_received": description,
		"files_analyzed":       fileResults,
	}
	if len(allSecFindings) > 0 {
		result["security_findings"] = allSecFindings
	}
	if len(allArchViolations) > 0 {
		result["arch_violations"] = enrichArchViolations(allArchViolations)
	}
	if len(allNorms) > 0 {
		result["norms"] = allNorms
	}
	if len(targetFiles) == 0 {
		result["hint"] = "No target files could be identified from the description. " +
			"Provide files=[\"path/to/file.go\"] for targeted security analysis."
	}

	// Sprint 27.4: action_required — worst directive across all findings.
	actionRequired := worstActionRequired(allSecFindings, allArchViolations)
	if len(allSecFindings) > 0 || len(allArchViolations) > 0 {
		result["action_required"] = actionRequired
	}

	summaryAction := ""
	if actionRequired != "none" {
		summaryAction = fmt.Sprintf(" Action required: %s.", actionRequired)
	}
	result["_summary"] = fmt.Sprintf(
		"pre_write: %d file(s) analyzed, %d security finding(s) (%d CRITICAL, %d HIGH), "+
			"%d arch rule violation(s).%s Status: %s.",
		len(fileResults), len(allSecFindings), critCount, highCount,
		len(allArchViolations), summaryAction, status)

	// Sprint 29.4: surface failure patterns relevant to the planned change.
	// Matches patterns against the description so the agent gets a warning at
	// the moment of planning — before the first line of code is written.
	if s.store != nil && s.projectID != "" && description != "" {
		if patterns, err := s.store.GetProjectFailurePatterns(s.projectID, 0.6); err == nil && len(patterns) > 0 {
			descLower := strings.ToLower(description)
			const maxPreWriteWarnings = 3
			var faWarnings []string
			for _, fp := range patterns {
				if len(faWarnings) >= maxPreWriteWarnings {
					break
				}
				kw := fp.Keyword
				// Match keyword in description text.
				if strings.Contains(descLower, kw) {
					text := fp.Text
					if age := relativeAge(fp.LastRecordCreatedAt); age != "" {
						text += " (" + age + ")"
					}
					faWarnings = append(faWarnings, text)
				}
			}
			if len(faWarnings) > 0 {
				result["failure_avoidance"] = faWarnings
			}
		}
	}

	return jsonResult(result)
}

// ── Sprint 30.1: KV Format for validate ─────────────────────────────────────

// reformatValidateKV converts a JSON validate result to labeled key-value format.
// It parses the JSON body from the sub-handler result and extracts findings,
// action, and summary into a compact text representation.
//
// Format example (format=kv, detail_level=summary):
//
//	# VALIDATE | post | handlers/users.go
//	[CRITICAL] missing-auth: endpoint lacks auth middleware (8/8 routes have it)
//	[MEDIUM] coupling-increase: handlers → store direct call
//	Action: BLOCK — fix CRITICAL before proceeding
func reformatValidateKV(result *mcp.CallToolResult, phase string, req mcp.CallToolRequest, detailLevel string, tokenBudget int) *mcp.CallToolResult {
	if result == nil || len(result.Content) == 0 {
		return result
	}

	// Extract the JSON text from the result content.
	var rawText string
	for _, c := range result.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			rawText = tc.Text
			break
		}
	}
	if rawText == "" {
		return result
	}

	// Parse JSON body.
	var body map[string]interface{}
	if err := json.Unmarshal([]byte(rawText), &body); err != nil {
		// Not JSON (may already be text) — return as-is.
		return result
	}

	// Build subtitle from phase + target file (if available).
	subtitle := phase
	if fw, _ := req.GetArguments()["files_written"].(string); fw != "" {
		subtitle = fmt.Sprintf("%s | %s", phase, fw)
	} else if desc, _ := req.GetArguments()["description"].(string); desc != "" {
		if len(desc) > 60 {
			desc = desc[:57] + "..."
		}
		subtitle = fmt.Sprintf("%s | %s", phase, desc)
	}

	var fields []KVField

	// Overall action (BLOCK / WARN / OK).
	if action, _ := body["action"].(string); action != "" && action != "none" {
		actionStr := strings.ToUpper(action)
		fields = append(fields, KVField{Key: "Action", Value: actionStr, Important: true})
	}

	// Security violations.
	if secFindings, ok := body["security_findings"].([]interface{}); ok {
		for _, f := range secFindings {
			finding, ok := f.(map[string]interface{})
			if !ok {
				continue
			}
			sev, _ := finding["severity"].(string)
			if sev == "" {
				sev = "UNKNOWN"
			}
			msg, _ := finding["message"].(string)
			ruleID, _ := finding["pattern_id"].(string)
			if msg == "" {
				continue
			}
			if detailLevel == "signal" && strings.ToUpper(sev) != "CRITICAL" {
				continue // signal mode: only CRITICAL
			}
			label := fmt.Sprintf("[%s]", strings.ToUpper(sev))
			if ruleID != "" {
				label = fmt.Sprintf("[%s] %s", strings.ToUpper(sev), ruleID)
			}
			important := strings.ToUpper(sev) == "CRITICAL"
			fields = append(fields, KVField{Key: label, Value: msg, Important: important})
		}
	}

	// Architectural violations.
	if violations, ok := body["violations"].([]interface{}); ok {
		for _, v := range violations {
			viol, ok := v.(map[string]interface{})
			if !ok {
				continue
			}
			sev, _ := viol["severity"].(string)
			if sev == "" {
				sev = "UNKNOWN"
			}
			desc, _ := viol["description"].(string)
			ruleID, _ := viol["rule_id"].(string)
			if desc == "" {
				continue
			}
			if detailLevel == "signal" && strings.ToUpper(sev) != "CRITICAL" {
				continue
			}
			label := fmt.Sprintf("[%s]", strings.ToUpper(sev))
			if ruleID != "" {
				label = fmt.Sprintf("[%s] %s", strings.ToUpper(sev), ruleID)
			}
			important := strings.ToUpper(sev) == "CRITICAL" || strings.ToUpper(sev) == "ERROR"
			fields = append(fields, KVField{Key: label, Value: desc, Important: important})
		}
	}

	// Full detail: include norms and suggestions.
	if detailLevel == "full" {
		if norms, ok := body["norm_violations"].([]interface{}); ok {
			for _, n := range norms {
				norm, ok := n.(map[string]interface{})
				if !ok {
					continue
				}
				msg, _ := norm["message"].(string)
				if msg != "" {
					fields = append(fields, KVField{Key: "[MEDIUM] norm", Value: msg})
				}
			}
		}
	}

	// Summary / status fallback for clean responses.
	if len(fields) == 0 {
		status, _ := body["status"].(string)
		if status == "" {
			status = "ok"
		}
		fields = append(fields, KVField{Key: "Status", Value: status, Important: true})
	}

	text := FormatKV("VALIDATE", subtitle, fields, tokenBudget)
	return mcp.NewToolResultText(text)
}

// ── Tool Catalog for discover_tools ─────────────────────────────────────────

// toolCatalogEntry describes a single Synapses tool for discovery purposes.
