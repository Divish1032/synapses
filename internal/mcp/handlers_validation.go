package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
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
			SafetyStatus:   safetyStatus,
			RuleIDs:        string(ruleIDsJSON),
			AgentID:        agentIDForPulse,
			ProjectID:      projID,
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
		"violations": violations,
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

// handleVerifyImplementation checks the actual graph state of written files
// against architectural rules and (optionally) a task's expectations.
// This is the write-side complement to validate_plan: validate_plan checks
// *before* writing, verify_implementation checks *after*.
func (s *Server) handleVerifyImplementation(
	_ context.Context,
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
		File             string                   `json:"file"`
		InGraph          bool                     `json:"in_graph"`
		NodeCount        int                      `json:"node_count"`
		Entities         []string                 `json:"entities,omitempty"`
		Violations       []config.Violation       `json:"violations,omitempty"`
		SecurityFindings []security.Violation     `json:"security_findings,omitempty"`
		SignatureImpact  []signatureImpactEntry   `json:"signature_impact,omitempty"`
		FreshnessWarning string                   `json:"freshness_warning,omitempty"`
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
			r.Violations = violations
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

		// Sprint 26.7: security pattern findings.
		if s.graph != nil && s.patternEngine != nil {
			if findings := s.patternEngine.CheckFile(s.graph, absFile, fileContent); len(findings) > 0 {
				r.SecurityFindings = findings
				totalSecurityFindings += len(findings)
			}
		}

		reports = append(reports, r)
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
	// Escalate status when CRITICAL or HIGH security findings exist.
	// An agent that reads only the top-level status must not get a false "pass"
	// while there are blocking security issues. Sprint 27.4 adds full severity-tier
	// enforcement; this ensures the status field is honest now.
	if totalSecurityFindings > 0 && (status == "pass" || status == "pending_indexing") {
		for _, r := range reports {
			for _, f := range r.SecurityFindings {
				if f.Severity == security.SeverityCritical || f.Severity == security.SeverityHigh {
					status = "security_findings_found"
					goto doneSecurityStatus
				}
			}
		}
	}
doneSecurityStatus:

	// P3-5: emit validation outcome event.
	if pc := s.getPulseClient(); pc != nil {
		agentIDForPulse := stringArg(req, "agent_id")
		projID := s.projectID
		pc.RecordValidationEvent(pulse.ValidationEvent{
			ToolName:       "verify_implementation",
			Status:         status,
			ViolationCount: totalViolations,
			AgentID:        agentIDForPulse,
			ProjectID:      projID,
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
	if taskVerification != nil {
		result["task_verification"] = taskVerification
	}
	if notIndexed > 0 {
		result["indexing_hint"] = fmt.Sprintf("%d file(s) not yet in graph — wait for indexing or re-run verify_implementation.", notIndexed)
	}
	if totalImpactWarnings > 0 {
		result["impact_hint"] = fmt.Sprintf("%d exported symbol(s) have callers — review signature_impact in each file to ensure call sites are still valid.", totalImpactWarnings)
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

	// _summary: one-line digest for quick scanning.
	// Format: "{F} files verified, {V} violations, {I} impact warnings."
	{
		status := "pass"
		if totalViolations > 0 {
			status = "violations_found"
		}
		result["_summary"] = fmt.Sprintf("%s: %d file(s), %d violation(s), %d security finding(s), %d impact warning(s)",
			status, len(files), totalViolations, totalSecurityFindings, totalImpactWarnings)
	}

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

// ── Tool Catalog for discover_tools ─────────────────────────────────────────

// toolCatalogEntry describes a single Synapses tool for discovery purposes.
