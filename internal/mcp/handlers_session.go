package mcp

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/SynapsesOS/synapses/internal/logutil"
	"github.com/SynapsesOS/synapses/internal/federation"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/pulse"
	"github.com/SynapsesOS/synapses/internal/store"
)

// embeddingStatus returns a human-readable status string for the active
// memory embedder. Used in the session_init response to explain why recall()
// may return only FTS results (e.g. in air-gapped or unconfigured environments).
//
// Possible values:
//   - "off"                                  — no embedder configured
//   - "builtin (ready)"                      — model loaded, embeddings working
//   - "builtin (model cached)"               — model on disk, pool initializes on first recall()
//   - "builtin (model not yet downloaded)"   — no model on disk; will download on first recall()
//   - "builtin (unavailable)"                — init attempted but failed (e.g. air-gapped)
//   - "ollama"                               — delegating to local Ollama instance
//   - "unknown"                              — unrecognized embedder implementation
func embeddingStatus(e embed.Embedder) string {
	if e == nil {
		return "off"
	}
	switch v := e.(type) {
	case *embed.BuiltinEmbedder:
		return "builtin (" + v.StatusDetail() + ")"
	case *embed.OllamaEmbedder:
		return "ollama"
	default:
		return "unknown"
	}
}

type changeEntry struct {
	File         string `json:"file"`
	At           string `json:"at"`
	NodesAdded   int    `json:"nodes_added"`
	NodesRemoved int    `json:"nodes_removed"`
	EdgesAdded   int    `json:"edges_added"`
}

// handleGetWorkingState returns recent file changes from the watcher's change log,
// plus an optional git diff stat for the working tree.
// Answers "what was the developer just working on before calling this agent?"
func (s *Server) handleGetWorkingState(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	windowMinutes := 15
	if w, ok := req.GetArguments()["window_minutes"].(float64); ok && w > 0 {
		windowMinutes = int(w)
	}

	var events []changeEntry
	if s.changeSource != nil {
		for _, e := range s.changeSource.RecentChanges(windowMinutes) {
			events = append(events, changeEntry{
				File:         e.File,
				At:           e.Timestamp.Format("15:04:05"),
				NodesAdded:   e.NodesAdded,
				NodesRemoved: e.NodesRemoved,
				EdgesAdded:   e.EdgesAdded,
			})
		}
	}
	if events == nil {
		events = []changeEntry{}
	}

	// Also surface recently-modified config files (synapses.json, Makefile, etc.)
	// that the watcher ignores since they aren't source code.
	root := s.graph.Root()
	if root != "" {
		cutoff := time.Now().Add(-time.Duration(windowMinutes) * time.Minute)
		configPatterns := []string{"synapses.json", "Makefile", "Dockerfile"}
		if entries, err := os.ReadDir(root); err == nil {
			for _, de := range entries {
				if de.IsDir() {
					continue
				}
				name := de.Name()
				ext := filepath.Ext(name)
				isConfig := ext == ".json" || ext == ".yaml" || ext == ".yml" || ext == ".toml"
				if !isConfig {
					for _, p := range configPatterns {
						if name == p {
							isConfig = true
							break
						}
					}
				}
				if !isConfig {
					continue
				}
				if info, err := de.Info(); err == nil && info.ModTime().After(cutoff) {
					events = append(events, changeEntry{
						File: filepath.Join(root, name),
						At:   info.ModTime().Format("15:04:05"),
					})
				}
			}
		}
	}

	result := map[string]interface{}{
		"window_minutes": windowMinutes,
		"recent_changes": events,
	}

	// Best-effort git diff stat — omitted when git is unavailable or not a git repo.
	// Use a 2s timeout to prevent blocking when git hangs (e.g. network mounts).
	if root != "" {
		gitCtx, gitCancel := context.WithTimeout(ctx, 2*time.Second)
		if out, err := exec.CommandContext(gitCtx, "git", "-C", root, "diff", "--stat", "HEAD").Output(); err == nil {
			if stat := strings.TrimSpace(string(out)); stat != "" {
				result["git_diff_stat"] = stat
			}
		}
		gitCancel()
	}

	// When no recent file watcher changes, fall back to recent git log so agents
	// always get meaningful orientation rather than an empty response.
	if len(events) == 0 && root != "" {
		gitCtx, gitCancel := context.WithTimeout(ctx, 2*time.Second)
		if out, err := exec.CommandContext(gitCtx, "git", "-C", root, "log", "--oneline", "-7").Output(); err == nil {
			if log := strings.TrimSpace(string(out)); log != "" {
				result["fallback_git_log"] = log
				result["fallback_note"] = fmt.Sprintf("No file changes in the last %d minutes. Showing recent git commits for context.", windowMinutes)
			}
		}
		gitCancel()
	}

	// Phase 6: entity-level impact enrichment for recently changed files.
	// Shows which high-impact entities were modified so the agent knows what
	// to check. Capped at top 5 files, entities with fanin > 3 only.
	if s.graph != nil && len(events) > 0 {
		type entityImpact struct {
			Name  string `json:"name"`
			File  string `json:"file"`
			Fanin int    `json:"fanin"`
			Type  string `json:"type"`
		}
		var impacts []entityImpact
		seenFiles := make(map[string]bool)
		limit := 5
		for _, ev := range events {
			if ev.File == "" || seenFiles[ev.File] {
				continue
			}
			seenFiles[ev.File] = true
			if len(seenFiles) > limit {
				break
			}
			// FindByFile uses suffix matching: strings.HasSuffix(n.File, "/"+filePath).
			// Graph stores absolute paths; watcher ChangeEvent.File is repo-relative.
			// Suffix match correctly bridges this: "/repo/internal/auth.go" has suffix
			// "/internal/auth.go". Tested in TestEntityImpact_RelativePathMatchesAbsoluteGraph.
			nodes := s.graph.FindByFile(ev.File)
			for _, n := range nodes {
				fanin := s.graph.Fanin(n.ID)
				if fanin > 3 {
					impacts = append(impacts, entityImpact{
						Name:  n.Name,
						File:  ev.File,
						Fanin: fanin,
						Type:  string(n.Type),
					})
				}
			}
		}
		if len(impacts) > 0 {
			// Sort by fanin descending, cap at 10.
			sort.Slice(impacts, func(i, j int) bool { return impacts[i].Fanin > impacts[j].Fanin })
			if len(impacts) > 10 {
				impacts = impacts[:10]
			}
			result["modified_entities"] = map[string]interface{}{
				"count":    len(impacts),
				"entities": impacts,
				"hint":     "High-impact entities modified recently. Consider running get_impact(symbol=\"Name\") to assess blast radius.",
			}
		}
	}

	// Context-aware suggestions based on what was recently changed.
	result["suggested_tools"] = suggestToolsForChanges(events)

	return jsonResult(result)
}

// suggestToolsForChanges returns ordered tool suggestions based on the recently changed files.
// Helps agents decide what to investigate or verify after seeing working state.
func suggestToolsForChanges(events []changeEntry) []toolSuggestion {
	if len(events) == 0 {
		return []toolSuggestion{
			{Tool: "get_project_identity", Reason: "orient yourself before starting work"},
			{Tool: "get_pending_tasks", Reason: "find your next task"},
		}
	}
	suggestions := []toolSuggestion{
		{Tool: "get_violations", Reason: "check if recent edits introduced architectural violations"},
		{Tool: "update_task", Reason: "mark in-progress tasks complete if work is finished"},
	}
	// Suggest exploring changed files.
	seen := make(map[string]bool)
	for _, e := range events {
		if !seen[e.File] && e.File != "" {
			seen[e.File] = true
			suggestions = append(suggestions, toolSuggestion{
				Tool:   "get_file_context",
				Reason: "see all entities in recently changed file: " + e.File,
			})
		}
		if len(seen) >= 2 {
			break // limit to 2 file suggestions
		}
	}
	return suggestions
}

// hashIdentity produces a SHA-1 hex digest of the serialised ProjectIdentity.
// Used to detect whether the project structure has changed since the last
// session_init call, allowing incremental responses that skip unchanged data.
// formatSessionDuration returns a human-readable duration string for session
// gap and age display. Uses the largest meaningful unit (hours/minutes/seconds).
func formatSessionDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

func hashIdentity(identity *graph.ProjectIdentity) string {
	b, err := json.Marshal(identity)
	if err != nil {
		return ""
	}
	h := sha1.Sum(b)
	return fmt.Sprintf("%x", h)
}

// handleSessionInit is the single-call session bootstrap that replaces the
// three-step startup ritual (get_pending_tasks → get_project_identity →
// get_working_state). One MCP round-trip returns all the context an agent
// needs to start work, including scale-aware tool guidance and recent events.
//
// Incremental mode: when agent_id is provided and the agent has called
// session_init before, unchanged sections are omitted to save tokens.
func (s *Server) handleSessionInit(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	agentID, _ := req.GetArguments()["agent_id"].(string)
	model, _ := req.GetArguments()["model"].(string)
	provider, _ := req.GetArguments()["provider"].(string)
	intent, _ := req.GetArguments()["intent"].(string)
	// scope controls response verbosity:
	//   "full"   (default) — all sections, backward compatible
	//   "quick"  — tasks + working_state + scale_guidance only (~500 tokens)
	//   "resume" — tasks with session states + working_state + stale hints only
	scope, _ := req.GetArguments()["scope"].(string)
	if scope == "" {
		scope = "full"
	}
	validScopes := map[string]bool{"full": true, "quick": true, "resume": true}
	scopeWarning := ""
	if !validScopes[scope] {
		scopeWarning = fmt.Sprintf("unknown scope %q — defaulting to 'full'. Valid values: full, quick, resume.", scope)
		scope = "full"
	}
	quickMode := scope == "quick"
	resumeMode := scope == "resume"
	s.upsertAgentIfNeeded(agentID)
	// Phase 6: reset component health tracker for this agent on new session
	// so auto-disabled components get a fresh chance. Per-agent scoped.
	if agentID != "" {
		s.componentHealth.reset(agentID)
	}
	// B29: Store declared intent so peers can see what this agent is working on.
	if agentID != "" && intent != "" && s.store != nil {
		s.upsertAgentWithActivity(agentID, &store.AgentActivity{Intent: intent})
	}
	// Remember this agent so subsequent tool calls that omit agent_id
	// can still be attributed correctly in Pulse analytics.
	s.setLastAgent(agentID)
	// R29: Reset the correction/escalation counters for this agent when a new
	// session starts. Without this, a reconnecting agent (same agentID, new
	// session) inherits stale repeat-counts and fires spurious signals.
	if agentID != "" {
		s.ctxCallMu.Lock()
		for k := range s.ctxCalls {
			if strings.HasPrefix(k, agentID+"\x00") {
				delete(s.ctxCalls, k)
			}
		}
		s.ctxCallMu.Unlock()
	}

	// Pulse session event is deferred until after synapseSessionID is obtained
	// (see below) so we can pass the main store's UUID to eliminate session ID
	// collision.

	// Emit session-start event so peers polling get_events see a new agent arrive.
	if s.store != nil && agentID != "" {
		sessionPayload, _ := json.Marshal(map[string]string{"agent_id": agentID})
		_ = s.store.AppendEvent("agent_session_start", agentID, string(sessionPayload))
	}

	// ── Session Intelligence: get or resume session record ───────────────
	// GetOrResumeSession uses mcpSessionID (MCP transport connection ID) as
	// the primary discriminator for same-connection resumes. For cross-connection
	// resumes (e.g. user restarted editor after a break), it looks for a recent
	// session from the same (agentID, projectID) within the hibernate window,
	// provided its last_seen_at is older than the reconnect window (not a live
	// concurrent editor window). Two concurrent Claude Code windows on the same
	// project always get independent sessions — never steal from each other.
	// Stale detection runs later (after recentChanges). Skipped on same-connection resume.
	var synapseSessionID string
	var sessionResumed bool
	var hibernateCtx *store.HibernateResumeContext
	if s.store != nil {
		effectiveAgentID := agentID
		if effectiveAgentID == "" {
			effectiveAgentID = "anonymous"
		}
		mcpSessionID := synapseSessionKey(SessionIDFromContext(ctx)) // normalise "" → "stdio"
		reconnectWindow := 0
		hibernateWindow := 0
		if s.config != nil {
			reconnectWindow = s.config.Session.ReconnectWindowSecs
			hibernateWindow = s.config.Session.HibernateWindowSecs
		}
		if id, resumed, hibCtx, sessErr := s.store.GetOrResumeSession(effectiveAgentID, s.projectID, mcpSessionID, intent, reconnectWindow, hibernateWindow); sessErr == nil {
			synapseSessionID = id
			sessionResumed = resumed
			hibernateCtx = hibCtx
			s.registerSynapseSession(mcpSessionID, synapseSessionID, effectiveAgentID, model)
			// Prune tool_calls older than 7 days on session start — debounced inside,
			// so concurrent session_init calls are safe and only one prune runs/hour.
			s.goBackground(func() { s.store.PruneToolCallsOlderThan(7 * 24 * time.Hour) }) //nolint:errcheck
			// Prune closed/hibernated sessions older than 90 days — debounced to once/day.
			s.goBackground(func() { s.store.PruneOldSessions(90 * 24 * time.Hour) }) //nolint:errcheck
		}
	}

	// Notify pulse of session start using the Synapses session UUID (eliminates
	// session ID collision from the old synthetic agentID:projectID:date format).
	if pc := s.getPulseClient(); pc != nil && agentID != "" && s.logSessions {
		aid := agentID
		projID := s.projectID
		mdl := model
		prov := provider
		sessID := synapseSessionID // use main store's UUID
		s.goBackground(func() {
			pc.RecordSessionEventWithID(sessID, aid, projID, "start")
			if mdl != "" {
				pc.RecordSessionModelWithID(sessID, aid, projID, mdl, prov)
			}
		})
	}

	// ── Look up agent context profile for incremental delivery ───────────
	var agentCtx *store.AgentContext
	incremental := false
	var collisionWarning string
	if agentID != "" && s.store != nil {
		if ac, err := s.store.GetAgentContext(agentID); err == nil && ac != nil {
			agentCtx = ac
			incremental = true
			// Collision detection: if the same agent_id started a session within the
			// last 2 minutes, warn that two sessions may be competing under the same ID.
			if ac.LastSession != "" {
				if prev, parseErr := time.Parse(time.RFC3339, ac.LastSession); parseErr == nil {
					if time.Since(prev) < 2*time.Minute {
						collisionWarning = fmt.Sprintf(
							"agent_id %q was last seen %.0f seconds ago — another session may already be using this ID. "+
								"If this is a new session, append a unique suffix (e.g. %q) to avoid overwriting peer state.",
							agentID, time.Since(prev).Seconds(), agentID+"-2",
						)
					}
				}
			}
		}
	}

	// ── 1. Project identity + scale guidance ─────────────────────────────
	var identity *graph.ProjectIdentity
	var currentHash string
	crossCallCount := 0
	var linkedRepos []string
	var primaryRepoID string

	if s.graph != nil {
		identity = s.graph.ProjectIdentity()
		currentHash = hashIdentity(identity)

		// Enrich with federation summary (mirrors handleGetProjectIdentity).
		primaryRepoID = s.graph.RepoID()
		linkedSet := make(map[string]bool)
		for _, e := range s.graph.AllEdges() {
			if e.Type != graph.EdgeCalls {
				continue
			}
			fromIdx := strings.Index(string(e.From), "::")
			toIdx := strings.Index(string(e.To), "::")
			if fromIdx < 0 || toIdx < 0 {
				continue
			}
			fromRepo := string(e.From)[:fromIdx]
			toRepo := string(e.To)[:toIdx]
			if fromRepo != toRepo {
				crossCallCount++
				for _, r := range []string{fromRepo, toRepo} {
					if r != primaryRepoID && !linkedSet[r] {
						linkedSet[r] = true
						linkedRepos = append(linkedRepos, r)
					}
				}
			}
		}
		sort.Strings(linkedRepos)
	} else {
		// Knowledge mode: no graph, build minimal identity.
		primaryRepoID = filepath.Base(s.projectPath)
		identity = &graph.ProjectIdentity{
			RepoID:       primaryRepoID,
			ToolGuidance: "Knowledge mode — no code graph. Use remember, recall, create_plan, update_task, send_message, get_messages.",
		}
	}

	// In incremental mode, skip project_identity if the hash hasn't changed.
	identitySkipped := false
	var projectSection interface{}
	if incremental && agentCtx.IdentityHash == currentHash && currentHash != "" {
		identitySkipped = true
		projectSection = map[string]interface{}{
			"skipped": true,
			"reason":  "unchanged since last session — identity_hash matches",
		}
	} else {
		idPayload := map[string]interface{}{
			"identity": identity,
			"federation": map[string]interface{}{
				"is_federated":        len(linkedRepos) > 0,
				"linked_repos":        linkedRepos,
				"cross_project_edges": crossCallCount,
			},
		}
		if s.knowledgeMode {
			idPayload["mode"] = "knowledge"
		}
		projectSection = idPayload
	}

	// ── 2. Pending tasks ──────────────────────────────────────────────────
	type taskWithState struct {
		store.Task
		SessionState *store.SessionState `json:"session_state,omitempty"`
	}
	var pendingSection map[string]interface{}
	if s.store != nil {
		tasks, err := s.store.GetPendingTasks("", agentID)
		if err == nil {
			inProgressIDs := make([]string, 0)
			for _, t := range tasks {
				if t.Status == "in_progress" {
					inProgressIDs = append(inProgressIDs, t.ID)
				}
			}
			stateMap, _ := s.store.GetSessionStateForTasks(inProgressIDs)
			result := make([]taskWithState, len(tasks))
			for i, t := range tasks {
				result[i] = taskWithState{Task: t}
				if stateMap != nil {
					result[i].SessionState = stateMap[t.ID]
				}
			}
			summary := "no pending tasks"
			if len(tasks) > 0 {
				summary = fmt.Sprintf("%d task(s) pending/in-progress", len(tasks))
			}
			pendingSection = map[string]interface{}{
				"summary":  summary,
				"reminder": "Call update_task(id, 'in_progress') before starting a task and update_task(id, 'done', notes) immediately when finished. Never batch completions.",
			}
			if len(result) > 0 {
				pendingSection["tasks"] = result
			}
		}
	}
	if pendingSection == nil {
		pendingSection = map[string]interface{}{"summary": "no pending tasks"}
	}

	// ── 3. Working state ──────────────────────────────────────────────────
	windowMinutes := 15
	var recentChanges []changeEntry
	if s.changeSource != nil {
		for _, e := range s.changeSource.RecentChanges(windowMinutes) {
			recentChanges = append(recentChanges, changeEntry{
				File:         e.File,
				At:           e.Timestamp.Format("15:04:05"),
				NodesAdded:   e.NodesAdded,
				NodesRemoved: e.NodesRemoved,
				EdgesAdded:   e.EdgesAdded,
			})
		}
	}
	if recentChanges == nil {
		recentChanges = []changeEntry{}
	}
	workingSection := map[string]interface{}{
		"recent_changes": recentChanges,
	}
	// R32: include open quality gap count so agents know whether to check get_violations().
	// Always write the key (zero when store is nil) so callers can assert it safely.
	workingSection["open_quality_gaps"] = 0
	if s.store != nil {
		if gaps, err := s.store.GetGaps(store.GapFilter{Status: "open"}); err == nil {
			workingSection["open_quality_gaps"] = len(gaps)
		}
	}
	// Surface active architecture violations so agents see them at session start.
	// Zero noise when no violations exist (field is 0, not omitted — consistent with open_quality_gaps).
	workingSection["active_violations"] = 0
	if s.store != nil {
		if vlog, err := s.store.GetViolationLog("", 0); err == nil {
			workingSection["active_violations"] = len(vlog)
		}
	}
	var root string
	if s.graph != nil {
		root = s.graph.Root()
	} else if s.projectPath != "" {
		root = s.projectPath
	}
	if root != "" {
		// Use a 2s timeout to prevent blocking when git hangs (e.g. network mounts).
		gitCtx, gitCancel := context.WithTimeout(ctx, 2*time.Second)
		if out, err := exec.CommandContext(gitCtx, "git", "-C", root, "diff", "--stat", "HEAD").Output(); err == nil {
			if stat := strings.TrimSpace(string(out)); stat != "" {
				workingSection["git_diff_stat"] = stat
			}
		}
		gitCancel()
		if len(recentChanges) == 0 {
			gitCtx2, gitCancel2 := context.WithTimeout(ctx, 2*time.Second)
			if out, err := exec.CommandContext(gitCtx2, "git", "-C", root, "log", "--oneline", "-7").Output(); err == nil {
				if log := strings.TrimSpace(string(out)); log != "" {
					workingSection["fallback_git_log"] = log
					workingSection["fallback_note"] = fmt.Sprintf("No file changes in the last %d minutes. Showing recent git commits for context.", windowMinutes)
				}
			}
			gitCancel2()
		}

		// R22: Branch-aware context — detect current branch and surface it.
		// Detached HEAD returns "HEAD" — surfaced as-is (no branch diff in that case).
		gitCtxBranch, gitCancelBranch := context.WithTimeout(ctx, 2*time.Second)
		if out, err := exec.CommandContext(gitCtxBranch, "git", "-C", root, "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
			currentBranch := strings.TrimSpace(string(out))
			if currentBranch != "" {
				workingSection["current_branch"] = currentBranch

				// Persist branch for future change detection.
				if s.store != nil && synapseSessionID != "" {
					s.store.SetSessionBranch(synapseSessionID, currentBranch)
				}

				// Detect branch change: compare with previous session's branch.
				// Skip on detached HEAD (no meaningful diff target) and on session resume
				// (same session, no branch change to detect).
				if currentBranch != "HEAD" && !sessionResumed && s.store != nil {
					effectiveAgent := agentID
					if effectiveAgent == "" {
						effectiveAgent = "anonymous"
					}
					if prevBranch := s.store.GetLastBranch(effectiveAgent); prevBranch != "" && prevBranch != currentBranch {
						workingSection["branch_changed"] = true
						workingSection["previous_branch"] = prevBranch

						// Surface which files differ between branches so the agent
						// knows what context may have changed. Capped at 50 files.
						gitCtxDiff, gitCancelDiff := context.WithTimeout(ctx, 2*time.Second)
						if diffOut, diffErr := exec.CommandContext(gitCtxDiff, "git", "-C", root, "diff", "--name-only", prevBranch+"..."+currentBranch).Output(); diffErr == nil {
							diffFiles := strings.Split(strings.TrimSpace(string(diffOut)), "\n")
							// Filter empty strings from split.
							var files []string
							for _, f := range diffFiles {
								if f != "" {
									files = append(files, f)
								}
							}
							if len(files) > 50 {
								workingSection["branch_diff_truncated"] = true
								files = files[:50]
							}
							if len(files) > 0 {
								workingSection["branch_diff_files"] = files
								workingSection["branch_diff_note"] = fmt.Sprintf("Switched from %s to %s. %d file(s) differ between branches. The file watcher handles re-indexing automatically.", prevBranch, currentBranch, len(files))
							}
						}
						gitCancelDiff()
					}
				}
			}
		}
		gitCancelBranch()
	}

	// ── 3b. Stale session detection ──────────────────────────────────────
	// Find sessions that went silent > 30 min ago without a clean end_session.
	// For each, collect orphaned tasks and infer likely completion status from
	// file-change evidence. Results are advisory — never auto-resolved.
	var staleSessions []store.StaleSession
	// Skip stale detection on reconnect: sessionResumed means this is the
	// same live session as before — it cannot be stale from its own perspective.
	staleThreshold := 30 * time.Minute
	if s.config != nil && s.config.Session.StaleThresholdMins > 0 {
		staleThreshold = time.Duration(s.config.Session.StaleThresholdMins) * time.Minute
	}
	if s.store != nil && synapseSessionID != "" && !sessionResumed {
		if stale, err := s.store.GetStaleSessions(s.projectID, synapseSessionID, staleThreshold); err == nil && len(stale) > 0 {
			for i := range stale {
				if orphans, err := s.store.GetOrphanedTasks(stale[i].SessionID); err == nil && len(orphans) > 0 {
					for j := range orphans {
						orphans[j] = s.inferOrphanEvidence(orphans[j], recentChanges)
					}
					stale[i].OrphanedTasks = orphans
				}
			}
			staleSessions = stale
		}
	}

	// ── 4. Recent agent events ───────────────────────────────────────────
	// In incremental mode, only return events since the agent's last known seq.
	var recentEvents []store.Event
	var latestEventSeq int64
	if s.store != nil {
		sinceSeq := int64(0)
		limit := 20
		if incremental && agentCtx.LastEventSeq > 0 {
			sinceSeq = agentCtx.LastEventSeq
			limit = 50 // allow more events when fetching delta
		}
		events, seq, err := s.store.GetEvents(sinceSeq, nil, "", limit)
		if err == nil {
			recentEvents = events
			latestEventSeq = seq
		}
	}
	if recentEvents == nil {
		recentEvents = []store.Event{}
	}

	// ── 4b. Agent awareness ─────────────────────────────────────────────
	// Zero noise by default. agent_awareness is omitted entirely when the calling
	// agent has no active peers and no unread messages. Most solo sessions
	// produce no output here at all.
	var agentAwareness map[string]interface{}
	var unreadMsgs []store.Message
	if s.store != nil && agentID != "" {
		// active_count: peers present (integer only — no list surfaced here).
		var activeCount int
		if n, err := s.store.CountActiveAgents(agentID); err == nil {
			activeCount = n
		}

		// Only build agent_awareness when there is a signal worth surfacing.
		if activeCount > 0 {
			agentAwareness = map[string]interface{}{
				"active_count": activeCount,
			}
		}

		if unread, err := s.store.CountUnreadMessages(agentID); err == nil && unread > 0 {
			if agentAwareness == nil {
				agentAwareness = map[string]interface{}{}
			}
			agentAwareness["unread_messages"] = unread
		}
		// Auto-deliver up to 10 unread messages so agents don't need a separate call.
		if msgs, _, err := s.store.GetMessages(agentID, 0, "", true, 10); err == nil && len(msgs) > 0 {
			unreadMsgs = msgs
		}
	}

	// ── 5. Cross-project impact alerts (federated projects only) ──────────
	// Pull unread cross_project_impact messages from the message bus. These are
	// broadcast by the file watcher whenever a local change breaks a dependency
	// in a linked project. Surfacing them here ensures agents see the warning
	// at the very start of a session, before they commit to a plan.
	var crossProjectAlerts []store.Message
	if s.store != nil && crossCallCount > 0 {
		msgs, _, err := s.store.GetMessages("", 0, "cross_project_impact", true, 10)
		if err == nil && len(msgs) > 0 {
			crossProjectAlerts = msgs
		}
	}

	// ── Proactive failure injection ───────────────────────────────────────
	// If ≥5 failure episodes exist for this project, recall the most relevant
	// one relative to the most recently changed file. Cold-start safe: the
	// field is omitted entirely when fewer than 5 failures are recorded.
	var recentFailure map[string]interface{}
	if s.store != nil && len(recentChanges) > 0 {
		failures, err := s.store.GetEpisodes(primaryRepoID, "", "failure", nil, 5, 0)
		if err == nil && len(failures) >= 5 {
			query := filepath.Base(recentChanges[0].File)
			if matches, mErr := s.store.RecallEpisodes(query, primaryRepoID, "", "failure", "", 1, 0); mErr == nil && len(matches) > 0 {
				e := matches[0]
				recentFailure = map[string]interface{}{
					"decision":   e.Decision,
					"rationale":  e.Rationale,
					"outcome":    e.Outcome,
					"created_at": e.CreatedAt,
				}
			}
		}
	}

	// ── Assemble response ─────────────────────────────────────────────────
	// quickMode (scope="quick"): tasks + working_state + scale_guidance only.
	// resumeMode (scope="resume"): tasks with session states + working_state only.
	// Full mode (default): all sections, backward compatible.
	resp := map[string]interface{}{
		"pending_tasks":  pendingSection,
		"working_state":  workingSection,
		"scale_guidance": identity.ToolGuidance,
	}
	if scopeWarning != "" {
		resp["scope_warning"] = scopeWarning
	}
	if !quickMode && !resumeMode {
		resp["project_identity"] = projectSection
		// Omit recent_events entirely when empty — reduces first-session noise on fresh projects.
		// Full list still available via scope="full" when events exist, or get_events() directly.
		if len(recentEvents) > 0 {
			resp["recent_events"] = recentEvents
		}
		resp["latest_event_seq"] = latestEventSeq
		resp["session_hint"] = "Pass latest_event_seq to get_events on the next call to receive only new events. Use scale_guidance to decide when to use Synapses tools vs Read/Grep."
	} else {
		resp["latest_event_seq"] = latestEventSeq
	}
	if incremental {
		resp["incremental"] = true
		if identitySkipped {
			resp["identity_skipped"] = true
		}
	}
	if agentAwareness != nil {
		resp["agent_awareness"] = agentAwareness
	}
	if len(unreadMsgs) > 0 {
		resp["unread_messages"] = map[string]interface{}{
			"count":    len(unreadMsgs),
			"messages": unreadMsgs,
			"hint":     "Use get_messages(mark_read_ids=[...]) to acknowledge. Messages are NOT auto-marked as read.",
		}
	}
	if collisionWarning != "" {
		resp["warning"] = collisionWarning
	}
	// Sprint 11: inform agent of the budget multiplier applied to default token budgets.
	if mult := modelBudgetMultiplier(model); mult != 1.0 {
		resp["budget_multiplier"] = mult
	}
	if recentFailure != nil {
		resp["recent_failure"] = recentFailure
	}
	// Session Intelligence: surface stale sessions with orphaned tasks.
	// Only included when stale sessions exist — zero-noise by default.
	if len(staleSessions) > 0 {
		resp["stale_sessions"] = map[string]interface{}{
			"count":    len(staleSessions),
			"sessions": staleSessions,
			"hint":     "Previous session(s) ended without clean shutdown. Review orphaned tasks — confirm done or reset to pending. Synapses never auto-closes tasks.",
		}
	}
	// Cross-connection hibernate resume: surface prior context when the agent
	// returns after a break and Synapses resumes the dormant session transparently.
	// Only included on hibernate resume — zero-noise for same-connection resumes
	// and fresh sessions.
	if hibernateCtx != nil {
		gapStr := formatSessionDuration(time.Duration(hibernateCtx.GapSeconds) * time.Second)
		resumeBlock := map[string]interface{}{
			"status":     "resumed",
			"gap":        gapStr,
			"tool_calls": hibernateCtx.PriorToolCalls,
			"hint":       "Your session was resumed after a break. Tool call history and memories are intact — no need to re-initialize.",
		}
		if hibernateCtx.PriorIntent != "" {
			resumeBlock["prior_intent"] = hibernateCtx.PriorIntent
		}
		if hibernateCtx.PriorSummary != "" {
			resumeBlock["prior_summary"] = hibernateCtx.PriorSummary
		}
		if hibernateCtx.StartedAt > 0 {
			totalAge := formatSessionDuration(time.Since(time.Unix(hibernateCtx.StartedAt, 0)))
			resumeBlock["session_age"] = totalAge
		}
		resp["session_resumed"] = resumeBlock
	}
	if len(crossProjectAlerts) > 0 {
		resp["cross_project_alerts"] = map[string]interface{}{
			"count":    len(crossProjectAlerts),
			"messages": crossProjectAlerts,
			"warning":  fmt.Sprintf("%d unread cross-project impact alert(s). A recent change may have broken dependencies in a linked project. Review before proceeding.", len(crossProjectAlerts)),
		}
	}
	// Federation drift detection: proactive alerts when sibling project
	// entities that this project depends on have changed signatures.
	// Only fires when federation is configured AND deps exist AND drift is detected.
	// Zero tokens when no drift — the entire section is omitted.
	if s.federationResolver != nil && s.store != nil && !quickMode {
		fedCtx, fedCancel := context.WithTimeout(ctx, 2*time.Second)

		// Federation health summary: counts of entries, healthy, stale.
		fedStatus := s.federationResolver.Status(fedCtx)
		if len(fedStatus) > 0 {
			healthy, stale := 0, 0
			for _, es := range fedStatus {
				switch es.Status {
				case "indexed", "ok":
					healthy++
				default:
					stale++
				}
			}
			resp["federation_health"] = map[string]interface{}{
				"entries": len(fedStatus),
				"healthy": healthy,
				"stale":   stale,
				"details": fedStatus,
			}
		}

		// Drift detection: compare stored deps against sibling state.
		driftAlerts := s.federationResolver.CheckDrift(fedCtx, s.store)
		fedCancel()
		if len(driftAlerts) > 0 {
			// Surface as top-level warnings array so agents can't miss them.
			warnings := make([]string, 0, len(driftAlerts))
			for _, da := range driftAlerts {
				warnings = append(warnings, fmt.Sprintf(
					"⚠ Entity %s in project '%s' has changed (%s). Run prepare_context(target='%s', projects='%s') to review.",
					da.Entity, da.Project, da.Change, da.Entity, da.Project,
				))
			}
			resp["warnings"] = warnings
			// Also keep the structured alerts for programmatic use.
			resp["cross_project_drift"] = map[string]interface{}{
				"count":  len(driftAlerts),
				"alerts": driftAlerts,
			}
		}
	}

	// Federation auto-discovery: suggest sibling projects when no federation is configured.
	if s.federationResolver == nil && s.projectPath != "" && !quickMode {
		if siblings := federation.DiscoverSiblings(s.projectPath); len(siblings) > 0 {
			names := make([]string, len(siblings))
			for i, sib := range siblings {
				names[i] = sib.Name
			}
			resp["federation_suggestions"] = map[string]interface{}{
				"discovered": siblings,
				"hint": fmt.Sprintf("Discovered %d sibling project(s) with Synapses indexes: %s. Add to federation config for cross-project awareness.",
					len(siblings), strings.Join(names, ", ")),
			}
		}
	}

	// ── 5b. Proactive tool suggestions (Phase 6) ─────────────────────────
	// When the agent declares an intent, suggest relevant deferred tools and
	// auto-promote them so MCP clients see them in the tool list immediately.
	// Zero tokens when no intent is declared.
	if intent != "" {
		if suggestions := s.SuggestToolsForIntent(intent); len(suggestions) > 0 {
			resp["suggested_tools"] = map[string]interface{}{
				"for_intent": intent,
				"tools":      suggestions,
				"note":       "These tools are recommended for your intent. Call discover_tools(query='...') to find others.",
			}
		}
	}

	// ── 6. Constitution (Hot Constitution — project principles) ───────────
	if s.config != nil && s.config.Constitution.InjectInSessionInit && len(s.config.Constitution.Principles) > 0 {
		resp["constitution"] = map[string]interface{}{
			"principles": s.config.Constitution.Principles,
			"count":      len(s.config.Constitution.Principles),
			"note":       "These project laws apply to all work in this session.",
		}
	}

	// ── Pre-warm brain cache for top entities (silent background op) ─────
	if bc := s.getBrainClient(); bc != nil {
		seen := make(map[string]bool)
		var warmFiles []string
		// Gather unique file paths from entry points and key entities.
		for _, ep := range identity.EntryPoints {
			if ep.File != "" && !seen[ep.File] {
				seen[ep.File] = true
				warmFiles = append(warmFiles, ep.File)
				if len(warmFiles) >= 5 {
					break
				}
			}
		}
		if len(warmFiles) < 5 {
			for _, ke := range identity.KeyEntities {
				if ke.File != "" && !seen[ke.File] {
					seen[ke.File] = true
					warmFiles = append(warmFiles, ke.File)
					if len(warmFiles) >= 5 {
						break
					}
				}
			}
		}
		if len(warmFiles) > 0 {
			files := make([]string, len(warmFiles))
			copy(files, warmFiles)
			s.goBackground(func() {
				for _, f := range files {
					s.warmBrainCache(f)
				}
			})
		}
	}

	// ── 7. Sidecar availability ───────────────────────────────────────────
	// Let agents skip tool calls for unavailable sidecars without trial-and-error.
	// Skipped in quick/resume mode — not critical for lightweight sessions.
	if !quickMode && !resumeMode {
		resp["sidecars"] = map[string]interface{}{
			"brain": map[string]interface{}{
				"available": s.getBrainClient() != nil,
				"note":      "enriches get_context with LLM summaries; required by upsert_adr, get_adrs",
			},
			"doc_cache": map[string]interface{}{
				"available": s.webCache != nil,
				"note":      "enables lookup_docs with version-pinned package documentation",
			},
			"pulse": map[string]interface{}{
				"available": s.getPulseClient() != nil,
				"note":      "enables agent analytics and token tracking",
			},
		}

		// ── BRAIN-1: Brain tier health ───────────────────────────────────────
		// Surface per-tier success rate, latency, and circuit breaker state so agents
		// discover broken models at session start instead of after getting garbage.
		// Wrapped in a recover so a brain panic never crashes the session_init handler.
		if bc := s.getBrainClient(); bc != nil {
			func() {
				defer func() {
					if r := recover(); r != nil {
						// Brain panic — degrade silently, don't crash session_init.
						resp["brain_warning"] = fmt.Sprintf("brain health unavailable (internal error: %v)", r)
					}
				}()
				health := bc.BrainHealth()
				if health == nil {
					return
				}
				resp["brain_health"] = health
				// Generate warnings for degraded tiers.
				if tiers, ok := health["tiers"].(map[string]interface{}); ok {
					var warnings []string
					for name, raw := range tiers {
						tier, ok := raw.(map[string]interface{})
						if !ok {
							continue
						}
						calls, _ := tier["calls"].(int64)
						if calls == 0 {
							continue // no data yet — no warning
						}
						rate, _ := tier["success_rate"].(float64)
						circuit, _ := tier["circuit"].(string)
						if circuit == "open" {
							// Circuit open subsumes the rate warning — one message per tier.
							warnings = append(warnings, fmt.Sprintf("brain tier %q circuit breaker is open — tier temporarily disabled", name))
						} else if rate < 0.5 {
							warnings = append(warnings, fmt.Sprintf("brain tier %q has %.0f%% success rate — model may be misconfigured", name, rate*100))
						}
					}
					if len(warnings) > 0 {
						sort.Strings(warnings)
						resp["brain_warning"] = strings.Join(warnings, "; ")
					}
				}
			}()
		}

		// R29: surface effectiveness hints for low-scoring entities so agents
		// know upfront which entities typically need deeper context fetches.
		// Run in a goroutine with a 100ms deadline so a slow or unavailable pulse
		// sidecar never adds latency to the critical session_init response path.
		if pc := s.getPulseClient(); pc != nil {
			type hintResult struct{ hints []pulse.EntityEffectiveness }
			hintCh := make(chan hintResult, 1)
			projID := s.projectID
			// minSignals=5: require at least 5 signals before surfacing hints.
			// With 2 signals a single correction event would score 0.0 and trigger
			// a false "frequently insufficient" warning — 5 provides minimal
			// statistical validity before an entity is flagged.
			go func() { hintCh <- hintResult{hints: pc.FetchEffectiveness(projID, 5)} }()
			select {
			case res := <-hintCh:
				if len(res.hints) > 0 {
					// Only surface entities with a non-empty suggestion (score < 0.6).
					var lowScoring []map[string]interface{}
					for _, h := range res.hints {
						if h.Suggestion == "" {
							continue
						}
						lowScoring = append(lowScoring, map[string]interface{}{
							"entity":     h.Entity,
							"score":      h.Score,
							"suggestion": h.Suggestion,
						})
						if len(lowScoring) >= 5 {
							break
						}
					}
					if len(lowScoring) > 0 {
						resp["context_effectiveness_hints"] = map[string]interface{}{
							"note":     "These entities historically required multiple context fetches. Use detail_level=full or increase depth on first call.",
							"entities": lowScoring,
						}
					}
				}
			case <-time.After(100 * time.Millisecond):
				// Pulse sidecar is slow or unavailable — skip hints rather than blocking.
			}
		}
	} // end !quickMode && !resumeMode

	// ── 8. Agent constraints (behavioral rules) ───────────────────────────
	// Surface all agent-type rules (no ForbiddenEdge, conversation-level constraints)
	// so every new session inherits decisions made in prior sessions without
	// the agent needing to re-discover or re-ask for them.
	s.rulesMu.RLock()
	var agentConstraints []map[string]string
	for _, r := range s.config.Rules {
		if r.IsAgentRule() {
			agentConstraints = append(agentConstraints, map[string]string{
				"id":          r.ID,
				"description": r.Description,
				"severity":    r.Severity,
			})
		}
	}
	s.rulesMu.RUnlock()
	if len(agentConstraints) > 0 {
		resp["agent_constraints"] = map[string]interface{}{
			"count":       len(agentConstraints),
			"constraints": agentConstraints,
			"note":        "These behavioral rules were established in prior sessions. Apply them throughout this session.",
		}
	}

	// ── 9. Relevant memories ─────────────────────────────────────────────
	// Surface institutional knowledge at session start so agents benefit from
	// prior sessions automatically. Three tiers:
	//   1. Entity memories for nodes linked to in-progress tasks
	//   2. Project-tier memories (always relevant)
	//   3. Session logs from this agent's recent sessions
	// Capped at ~500 chars total; all surfaced memories get touched (TTL renewal).
	// Skipped in quick mode — caller gets a lighter response.
	if s.store != nil && !quickMode {
		const memCap = 500
		var memoryItems []map[string]string
		var touchIDs []string
		totalLen := 0

		addMemory := func(m store.Memory, label string) bool {
			if totalLen >= memCap {
				return false
			}
			content := m.Content
			// UTF-8 safe cap: truncate by rune count, not byte count.
			if runes := []rune(content); totalLen+len(runes) > memCap {
				content = string(runes[:memCap-totalLen])
			}
			memoryItems = append(memoryItems, map[string]string{
				"tier":    m.Tier,
				"content": content,
				"label":   label,
			})
			touchIDs = append(touchIDs, m.ID)
			totalLen += len(content)
			return totalLen < memCap
		}

		// 1. Entity memories for in-progress task linked nodes.
		if pendingSection != nil {
			if tasks, ok := pendingSection["tasks"].([]taskWithState); ok {
				var linkedNodeIDs []string
				for _, t := range tasks {
					if t.Status == "in_progress" {
						linkedNodeIDs = append(linkedNodeIDs, t.LinkedNodes...)
					}
				}
				if len(linkedNodeIDs) > 0 {
					entityMems, _ := s.store.QueryMemoriesForEntities(linkedNodeIDs, 5)
					for _, nodeID := range linkedNodeIDs {
						for _, m := range entityMems[nodeID] {
							if !addMemory(m, "task_entity") {
								break
							}
						}
					}
				}
			}
		}

		// 2. Project-tier memories.
		if totalLen < memCap {
			projMems, _ := s.store.QueryMemories(store.TierProject, "", "", 5)
			for _, m := range projMems {
				if !addMemory(m, "project") {
					break
				}
			}
		}

		// 3. Session logs from this agent's recent sessions.
		if totalLen < memCap && agentID != "" {
			sessMems, _ := s.store.QueryRecentSessionMemories(agentID, 3)
			for _, m := range sessMems {
				if !addMemory(m, "session_history") {
					break
				}
			}
		}

		if len(memoryItems) > 0 {
			resp["relevant_memories"] = map[string]interface{}{
				"count":    len(memoryItems),
				"memories": memoryItems,
				"note":     "Institutional knowledge from prior sessions. These memories are auto-surfaced and renewed on access.",
			}
			// Touch surfaced memories in background — TTL renewal must not add
			// latency to the session_init response.
			if len(touchIDs) > 0 {
				ids := make([]string, len(touchIDs))
				copy(ids, touchIDs)
				s.goBackground(func() {
					for _, id := range ids {
						s.store.TouchMemory(id)
					}
				})
			}
		}
	}

	// ── AM-3: Invalidated memories ──────────────────────────────────────
	// Surface stale memories not yet shown to THIS agent. Per-agent tracking
	// via memory_surfaced table ensures every agent sees each invalidation.
	if s.store != nil {
		if invalMems, err := s.store.QueryInvalidatedMemories(agentID, 10); err == nil && len(invalMems) > 0 {
			resp["invalidated_memories"] = map[string]interface{}{
				"count":    len(invalMems),
				"memories": invalMems,
				"note":     "These beliefs were invalidated because their anchor nodes were removed or changed. Review before proceeding — they may no longer be true.",
			}
			resp["memory_integrity"] = fmt.Sprintf("warn — %d belief(s) were invalidated since last session. Review invalidated_memories before proceeding.", len(invalMems))
			// Mark surfaced for this agent in background so they don't re-appear.
			surfaceIDs := make([]string, len(invalMems))
			for i, m := range invalMems {
				surfaceIDs[i] = m.ID
			}
			aid := agentID
			s.goBackground(func() { _ = s.store.MarkMemoriesSurfaced(aid, surfaceIDs) })
		}
	}

	// ── R14C: Stale context hints ─────────────────────────────────────────
	// Cross-reference task-linked nodes and the previous session's entity register
	// against recently changed files. Returns entities whose containing file was
	// modified since the last session — signalling the agent to re-fetch them.
	if len(recentChanges) > 0 {
		// 1. Collect node IDs from in-progress task linked nodes.
		var nodeIDs []string
		if tasks, ok := pendingSection["tasks"].([]taskWithState); ok {
			for _, t := range tasks {
				nodeIDs = append(nodeIDs, t.LinkedNodes...)
			}
		}

		// 2. Augment with entity register from the most recent session log.
		// The session log embeds "Examined: X, Y, Z." — resolve names to node IDs.
		if s.store != nil && agentID != "" && s.graph != nil {
			if regMems, _ := s.store.QueryRecentSessionMemories(agentID, 1); len(regMems) > 0 {
				for _, name := range parseExaminedEntities(regMems[0].Content) {
					if nodes := s.graph.FindByName(name); len(nodes) > 0 {
						nodeIDs = append(nodeIDs, string(nodes[0].ID))
					}
				}
			}
		}

		if len(nodeIDs) > 0 {
			// Build a file-path → changed_at map for O(1) lookup.
			changedFiles := make(map[string]string)
			for _, rc := range recentChanges {
				changedFiles[rc.File] = rc.At
			}

			seen := make(map[string]bool)
			var hints []map[string]interface{}
			for _, nodeID := range nodeIDs {
				if seen[nodeID] {
					continue
				}
				seen[nodeID] = true

				// Node IDs are formatted as "repo::file::entityName".
				parts := strings.SplitN(nodeID, "::", 3)
				if len(parts) < 3 {
					continue
				}
				nodeFile, entityName := parts[1], parts[2]

				for changedFile, changedAt := range changedFiles {
					if containsFile([]string{changedFile}, nodeFile) {
						hints = append(hints, map[string]interface{}{
							"entity":     entityName,
							"file":       nodeFile,
							"changed_at": changedAt,
						})
						break
					}
				}
				if len(hints) >= 10 {
					break
				}
			}
			if len(hints) > 0 {
				resp["stale_context_hints"] = hints
			}
		}
	}

	// ── RX4: Previous session work summary ───────────────────────────────
	// Surface the package-grouped work from the most recent end_session call.
	// Zero tokens when no summary exists — not a new agent, not a first session.
	// Skipped in quick mode (not critical for lightweight sessions).
	if s.store != nil && agentID != "" && !quickMode {
		if workMem, wErr := s.store.GetLatestWorkSummary(agentID); wErr == nil && workMem != nil {
			var pkgs []PackageWork
			// Try envelope format first (new); fall back to raw array (legacy).
			var env workSummaryEnvelope
			if json.Unmarshal([]byte(workMem.Content), &env) == nil && len(env.Packages) > 0 {
				pkgs = env.Packages
			} else {
				if err := json.Unmarshal([]byte(workMem.Content), &pkgs); err != nil {
				logutil.Debug("synapses: session: unmarshal legacy work_summary packages for memory %q: %v\n", workMem.ID, err)
			}
			}
			if len(pkgs) > 0 {
				resp["previous_session_work"] = map[string]interface{}{
					"packages": pkgs,
					"note":     "Work grouped by package from the previous session. Use this to quickly re-orient — these are the files and entities you were actively changing.",
				}
				// Touch in background to renew TTL (same pattern as relevant_memories).
				wid := workMem.ID
				s.goBackground(func() { s.store.TouchMemory(wid) })
			}
		}
	}

	// ── Activation-context prompts (auto_load: true) ──────────────────────
	// Surface project-wide conventions so agents apply them from the first message.
	// Only prompts with auto_load: true are included here; entity-specific prompts
	// surface in individual get_context calls.
	if autoPrompts := s.getAutoLoadPrompts(); len(autoPrompts) > 0 {
		promptList := make([]map[string]string, 0, len(autoPrompts))
		for _, pt := range autoPrompts {
			promptList = append(promptList, map[string]string{
				"id":     pt.ID,
				"source": pt.Source,
				"body":   pt.Body,
			})
		}
		resp["active_prompts"] = map[string]interface{}{
			"count":   len(promptList),
			"prompts": promptList,
			"note":    "Project-wide activation context. Apply these conventions throughout the session.",
		}
	}

	// ── Daemon health (IMP-EVAL-3) ────────────────────────────────────────
	// Surface uptime, CPU%, and goroutine count so agents can detect an
	// overloaded or long-running daemon without external tooling.
	// Skipped in quick mode.
	if !quickMode && !resumeMode {
		{
			uptimeMin := int(time.Since(s.startTime).Minutes())
			health := map[string]interface{}{
				"uptime_minutes": uptimeMin,
				"goroutines":     runtime.NumGoroutine(),
			}
			var rusage syscall.Rusage
			if err := syscall.Getrusage(syscall.RUSAGE_SELF, &rusage); err == nil {
				cpuSec := float64(rusage.Utime.Sec) + float64(rusage.Utime.Usec)/1e6 +
					float64(rusage.Stime.Sec) + float64(rusage.Stime.Usec)/1e6
				wallSec := time.Since(s.startTime).Seconds()
				if wallSec > 0 {
					pct := math.Round(cpuSec/wallSec*1000) / 10 // one decimal place
					health["cpu_pct"] = pct
					if pct >= 20 {
						health["hint"] = "warn: daemon CPU usage is high — consider restarting for optimal performance"
					}
				}
			}
			resp["daemon_health"] = health
		}
	} // end !quickMode && !resumeMode (daemon_health)

	// ── Embedding health ──────────────────────────────────────────────────
	// Always included — agents need this to understand why recall() returns
	// only FTS results in air-gapped or unconfigured environments.
	resp["embeddings"] = embeddingStatus(s.memoryEmbedder)

	// Cross-project status: show ACL-allowed projects only.
	// Do not expose names of projects this project is not allowed to read from.
	if s.projectRegistry != nil {
		allProjects := s.projectRegistry.ListProjects()
		allowed := s.allowedProjectNames()
		if len(allProjects) > 1 { // only include if there are siblings
			status := map[string]interface{}{
				"registered_projects": len(allProjects),
			}
			if len(allowed) > 0 {
				status["accessible_projects"] = allowed
			}
			if len(allowed) == 0 {
				status["note"] = "No cross-project reads allowed. Configure federation_acl.allow_read_from in synapses.json to enable."
			}
			resp["cross_project_status"] = status
		}
	}

	// ── Update agent context profile ─────────────────────────────────────
	// Record what this agent now knows so the next session_init can be incremental.
	if agentID != "" && s.store != nil {
		if err := s.store.UpsertAgentContext(&store.AgentContext{
			AgentID:      agentID,
			LastEventSeq: latestEventSeq,
			IdentityHash: currentHash,
			LastSession:  time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			log.Printf("mcp: upsert agent context: %v", err)
		}
	}

	// OF-S4: tool description integrity check.
	// Re-derive the hash of all tool descriptions from the live toolDescs map
	// and compare against the baseline computed at startup. A mismatch means
	// a description was modified in memory after startup — surface a warning.
	// Runs for all scope modes (full/quick/resume) — security alerts are never
	// suppressed by verbosity settings.
	if s.toolDescBaseline != "" {
		if current := hashToolDescs(s.toolDescs); current != s.toolDescBaseline {
			logutil.Error("[synapses] tool description integrity violation — "+
				"hash mismatch detected at session_init. expected=%s actual=%s\n",
				s.toolDescBaseline, current)
			resp["tool_integrity_alert"] = map[string]interface{}{
				"severity": "HIGH",
				"message":  "Tool description hash mismatch — one or more tool descriptions were modified after server startup. This may indicate runtime tampering.",
				"expected": s.toolDescBaseline,
				"actual":   current,
			}
		}
	}

	// R6: response_tokens + context_window_pct — measure token cost of this
	// response and express it as a fraction of the agent's context window.
	// Strategy: marshal once to count bytes, add the two fields (their combined
	// size is ~60 bytes / ~17 tokens — negligible), return final marshal via
	// jsonResult. This means response_tokens is accurate to within ~1%.
	if jsonBytes, err := json.Marshal(resp); err == nil {
		responseTokens := len(jsonBytes) * 2 / 7
		resp["response_tokens"] = responseTokens
		// context_window_pct: how much of the agent's context window this response
		// consumes. Only computed when the model is known so we can look up window size.
		// Covers Claude 3/4 families, GPT-4 family, and Gemini. Falls back to omitting
		// the field when the model is unrecognised — no guessing.
		modelWindowTokens := map[string]int{
			// Claude 4 family
			"claude-opus-4-6":           200000,
			"claude-sonnet-4-6":         200000,
			"claude-haiku-4-5":          200000,
			"claude-haiku-4-5-20251001": 200000,
			// Claude 3 family
			"claude-3-5-sonnet-20241022": 200000,
			"claude-3-5-haiku-20241022":  200000,
			"claude-3-opus-20240229":     200000,
			// GPT-4 family
			"gpt-4o":      128000,
			"gpt-4o-mini": 128000,
			"gpt-4-turbo": 128000,
			"gpt-4":       8192,
			"o1":          200000,
			"o3-mini":     200000,
			// Gemini
			"gemini-2.0-flash": 1000000,
			"gemini-1.5-pro":   2000000,
		}
		if window, ok := modelWindowTokens[model]; ok {
			pct := math.Round(float64(responseTokens)/float64(window)*1000) / 10 // one decimal
			resp["context_window_pct"] = pct
		}
	}

	// Surface watcher health: if the file-watching loop has died, warn agents
	// so they know context may be stale.
	if whc, ok := s.changeSource.(WatcherHealthChecker); ok && !whc.IsAlive() {
		existing, _ := resp["warnings"].([]string)
		resp["warnings"] = append(existing, "file_watcher_stopped: file watching is no longer active — context may be stale. Restart the daemon to restore live updates.")
	}

	return jsonResult(resp)
}

// inferOrphanEvidence classifies an orphaned task's likely completion status
// using heuristic file-change evidence from the watcher's recent-change window.
// It checks whether any file linked to the task was modified after the task was
// started — a strong (but not conclusive) signal that work was completed.
//
// Returns the task with LikelyStatus and Evidence populated:
//   - "likely_done"      — files linked to the task were modified
//   - "unclear"          — no file evidence available (default)
//   - "likely_abandoned" — task was only created (never claimed)
func (s *Server) inferOrphanEvidence(ot store.OrphanedTask, recentChanges []changeEntry) store.OrphanedTask {
	// Tasks that were only created but never claimed are likely abandoned.
	if ot.Action == "created" && ot.Status == "pending" {
		ot.LikelyStatus = "likely_abandoned"
		ot.Evidence = "task was created but never claimed in the stale session"
		return ot
	}

	if s.graph == nil || s.store == nil {
		return ot
	}

	// Look up the task to get its linked nodes.
	task, err := s.store.GetTask(ot.TaskID)
	if err != nil || task == nil || len(task.LinkedNodes) == 0 {
		return ot
	}

	// Build a set of recently changed files for O(1) lookup.
	changedFileSet := make(map[string]bool, len(recentChanges))
	for _, rc := range recentChanges {
		changedFileSet[rc.File] = true
	}

	// Check if any linked node's file was recently modified.
	var modifiedFiles []string
	for _, nodeID := range task.LinkedNodes {
		n := s.graph.GetNode(graph.NodeID(nodeID))
		if n == nil || n.File == "" {
			continue
		}
		if changedFileSet[n.File] {
			modifiedFiles = append(modifiedFiles, n.File)
		}
	}

	if len(modifiedFiles) > 0 {
		ot.LikelyStatus = "likely_done"
		if len(modifiedFiles) == 1 {
			ot.Evidence = fmt.Sprintf("linked file %s was modified after task started", modifiedFiles[0])
		} else {
			ot.Evidence = fmt.Sprintf("%d linked files were modified after task started (%s, ...)", len(modifiedFiles), modifiedFiles[0])
		}
	}
	return ot
}

// handleReportUsage records agent-self-reported LLM token usage (Option B).
// The agent calls this after completing a response to give Synapses accurate
// model cost data that cannot be inferred from the MCP layer alone.
func (s *Server) handleReportUsage(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	model, _ := args["model"].(string)
	provider, _ := args["provider"].(string)
	agentID, _ := args["agent_id"].(string)
	if agentID == "" {
		agentID = s.getLastAgent()
	}

	inputTokens := 0
	outputTokens := 0
	costUSD := 0.0
	if v, ok := args["input_tokens"].(float64); ok {
		inputTokens = int(v)
	}
	if v, ok := args["output_tokens"].(float64); ok {
		outputTokens = int(v)
	}
	if v, ok := args["cost_usd"].(float64); ok {
		costUSD = v
	}

	if model == "" {
		return mcp.NewToolResultError("model is required (e.g., 'claude-sonnet-4-6', 'gpt-4o')"), nil
	}

	pc := s.getPulseClient()
	if pc != nil {
		mcpSessID := SessionIDFromContext(ctx)
		sessID := s.getSynapseSessionID(mcpSessID)
		if sessID == "" {
			sessID = agentID + ":" + s.projectID + ":" + time.Now().UTC().Format("2006-01-02")
		}
		evt := pulse.AgentLLMUsageEvent{
			SessionID:    sessID,
			AgentID:      agentID,
			ProjectID:    s.projectID,
			Model:        model,
			Provider:     provider,
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			CostUSD:      costUSD,
		}
		s.goBackground(func() { pc.RecordAgentLLMUsage(evt) })
	}

	return jsonResult(map[string]interface{}{
		"recorded": true,
		"model":    model,
		"note":     "Usage recorded. Thank you — this improves cost-savings accuracy in Analytics.",
	})
}

// cloneGraph creates a shallow copy of g with an independent edge set.
func cloneGraph(g *graph.Graph) *graph.Graph {
	clone := graph.New(g.RepoID())
	for _, n := range g.AllNodes() {
		clone.AddNode(n)
	}
	for _, e := range g.AllEdges() {
		clone.AddEdge(e)
	}
	return clone
}

func (s *Server) handleAnnotateNode(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("annotations unavailable: run 'synapses start' or 'synapses index' to create a persistent store"), nil
	}

	nodeID, _ := req.GetArguments()["node_id"].(string)
	if nodeID == "" {
		return mcp.NewToolResultError("node_id is required (use find_entity or search to get node IDs)"), nil
	}
	note, noteErr := stringArgLimited(req, "note", maxArgLengthNote)
	if noteErr != nil {
		return mcp.NewToolResultError(noteErr.Error()), nil
	}
	if note == "" {
		return mcp.NewToolResultError("note is required (a brief annotation, e.g., 'this function has O(n²) complexity')"), nil
	}
	agentID, _ := req.GetArguments()["agent_id"].(string)

	// OF-S2: scan note content for prompt injection patterns.
	var injectionWarning string
	if scanResult, scanErr := s.scanContent("note", note); scanErr != nil {
		return mcp.NewToolResultError(scanErr.Error()), nil
	} else {
		note = scanResult.sanitized
		if scanResult.warning != "" {
			injectionWarning = scanResult.warning
		}
	}

	// Verify the node exists in the graph.
	if s.graph.GetNode(graph.NodeID(nodeID)) == nil {
		return mcp.NewToolResultError(fmt.Sprintf("node not found: %q", nodeID)), nil
	}

	id, err := s.store.AddAnnotation(nodeID, agentID, note)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("add annotation: %v", err)), nil
	}

	// Dual-write to unified memories table (entity tier).
	_, _ = s.store.InsertMemory(store.Memory{
		Tier:     store.TierEntity,
		Content:  note,
		EntityID: nodeID,
		AgentID:  agentID,
		Source:   store.SourceManual,
		Tags:     `["annotation"]`,
	})

	// Emit event.
	annotPayload, _ := json.Marshal(map[string]string{"annotation_id": id, "node_id": nodeID})
	if err := s.store.AppendEvent("annotation_added", agentID, string(annotPayload)); err != nil {
		logutil.Warn("synapses: append annotation_added event: %v\n", err)
	}

	resp := map[string]interface{}{
		"annotation_id": id,
		"node_id":       nodeID,
		"message":       "Annotation saved. It will appear in get_context responses for this node.",
	}
	if injectionWarning != "" {
		resp["injection_warning"] = injectionWarning
	}
	return jsonResult(resp)
}

// trimRepoRoot strips the repo root prefix from a slice of absolute file paths,
// returning paths relative to the repo root. Mirrors the normalizeSubgraph
// behaviour so TestCoverage paths are consistent with all other file references
// in get_context/get_impact responses.
func (s *Server) trimRepoRoot(paths []string) []string {
	root := s.graph.Root()
	if root == "" || len(paths) == 0 {
		return paths
	}
	prefix := root
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = strings.TrimPrefix(p, prefix)
	}
	return out
}

// handleGetImpact performs reverse-BFS blast-radius analysis from a named entity.
// Returns nodes grouped by depth tier: direct (depth 1, confidence 1.0),
