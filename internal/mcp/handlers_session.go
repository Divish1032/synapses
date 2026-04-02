package mcp

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/SynapsesOS/synapses/internal/federation"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/logutil"
	"github.com/SynapsesOS/synapses/internal/pulse"
	"github.com/SynapsesOS/synapses/internal/store"
)

// safeBranchRe validates git branch names to prevent shell injection via undelimited arguments.
var safeBranchRe = regexp.MustCompile(`^[a-zA-Z0-9._/\-]+$`)

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
	agentVersion, _ := req.GetArguments()["agent_version"].(string)
	// scope controls response verbosity:
	//   "standard" (default) — tasks + working_state + scale_guidance (~500 tokens); lists deferred sections in more_available
	//   "full"               — all sections, backward compatible
	//   "quick"              — alias for standard (legacy; same lean response)
	//   "resume"             — tasks with session states + working_state + stale hints only
	scope, _ := req.GetArguments()["scope"].(string)
	if scope == "" {
		scope = "standard"
	}
	validScopes := map[string]bool{"full": true, "quick": true, "resume": true, "standard": true, "compaction": true}
	scopeWarning := ""
	if !validScopes[scope] {
		scopeWarning = fmt.Sprintf("unknown scope %q — defaulting to 'standard'. Valid values: full, standard, quick, resume, compaction.", scope)
		scope = "standard"
	}
	quickMode := scope == "quick" || scope == "standard"
	resumeMode := scope == "resume"
	compactionMode := scope == "compaction"
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

	// Sprint 27.1: Reset SDLC auto-detector on new session so detection
	// starts fresh (no carry-over from previous agent session).
	s.sdlcDetect.reset()

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
	// effectiveAgentID normalises "" to "anonymous" so session storage, task
	// queries, and recovery packet assembly all use a consistent non-empty value.
	effectiveAgentID := agentID
	if effectiveAgentID == "" {
		effectiveAgentID = "anonymous"
	}
	var sessionResumed bool
	var hibernateCtx *store.HibernateResumeContext
	if s.store != nil {
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
			s.goBackground(func() {
				pruned, _ := s.store.PruneToolCallsOlderThan(7 * 24 * time.Hour)
				// P7-7: emit lifecycle event for tool call pruning.
				if pruned > 0 {
					if pc := s.getPulseClient(); pc != nil {
						pc.RecordLifecycleEvent("prune_tool_calls", float64(pruned), s.projectID)
					}
				}
			})
			// Prune closed/hibernated sessions older than 90 days — debounced to once/day.
			s.goBackground(func() {
				pruned, _ := s.store.PruneOldSessions(90 * 24 * time.Hour)
				// P7-7: emit lifecycle event for session pruning.
				if pruned > 0 {
					if pc := s.getPulseClient(); pc != nil {
						pc.RecordLifecycleEvent("prune_sessions", float64(pruned), s.projectID)
					}
				}
			})
		}
	}

	// Compaction mode is explicit-only for mid-session compaction: agents pass
	// scope="compaction" when they know their context was compressed. No mid-session
	// auto-detection — avoids false positives that waste tokens.
	// Cross-connection resumes (hibernateCtx != nil) auto-inject the recovery packet
	// because they are a concrete signal of context loss (see below, Sprint 24.2).

	// Detect project-wide first session: count == 1 means this is the first
	// session_init ever recorded for this project — surfaced as highlights.
	// sessionResumed alone is insufficient: it is per-agent-session, not project-wide.
	// Detect project-wide first session: count == 1 means this is the first
	// session_init ever recorded for this project — surfaced as highlights.
	// Guard s.projectID != "" prevents false matches when projectID was never
	// set (e.g. unconfigured server or tests that don't call SetProjectID).
	var isFirstProjectSession bool
	if s.store != nil && !sessionResumed && s.projectID != "" {
		if count, countErr := s.store.CountProjectSessions(s.projectID); countErr == nil && count == 1 {
			isFirstProjectSession = true
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
		ver := agentVersion
		intentCopy := intent
		s.goBackground(func() {
			// P6-8: use RecordSessionEventFull to populate agent_version column.
			pc.RecordSessionEventFull(sessID, aid, projID, "start", ver)
			if mdl != "" {
				pc.RecordSessionModelWithID(sessID, aid, projID, mdl, prov)
			}
			// P8-1: propagate session intent to Pulse sessions table.
			if intentCopy != "" {
				pc.SetSessionIntent(sessID, intentCopy)
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
			ToolGuidance: "Knowledge mode — no code graph. Use memory(action=save/search), tasks(action=create_plan/pending/update), validate(phase=safety).",
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
				"count":    len(tasks),
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
						// Validate branch names to prevent git argument injection.
						if safeBranchRe.MatchString(prevBranch) && safeBranchRe.MatchString(currentBranch) &&
							!strings.Contains(prevBranch, "\x00") && !strings.Contains(currentBranch, "\x00") &&
							!strings.Contains(prevBranch, "...") && !strings.Contains(currentBranch, "...") {
							if diffOut, diffErr := exec.CommandContext(gitCtxDiff, "git", "-C", root, "diff", "--name-only", prevBranch+"..."+currentBranch).Output(); diffErr == nil {
								diffFiles := strings.Split(strings.TrimSpace(string(diffOut)), "\n")
								// Filter empty strings and paths that escape root.
								var files []string
								for _, f := range diffFiles {
									if f == "" {
										continue
									}
									if !pathWithinRoot(root, filepath.Join(root, f)) {
										continue
									}
									files = append(files, f)
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
			// P7-8: emit session event for stale detection.
			if pc := s.getPulseClient(); pc != nil {
				orphanCount := 0
				for _, ss := range stale {
					orphanCount += len(ss.OrphanedTasks)
				}
				pc.RecordLifecycleEvent("stale_detected", float64(len(stale)), s.projectID)
				if orphanCount > 0 {
					pc.RecordLifecycleEvent("orphaned_tasks_found", float64(orphanCount), s.projectID)
				}
			}
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
		sessionHint := "Pass latest_event_seq to get_events on the next call to receive only new events. Use scale_guidance to decide when to use Synapses tools vs Read/Grep."
		if identity != nil {
			highConf := 0
			for _, sr := range identity.SuggestedRules {
				if sr.Confidence >= 0.9 {
					highConf++
				}
			}
			if highConf > 0 {
				sessionHint += fmt.Sprintf(" %d high-confidence architectural pattern(s) detected in project_identity.suggested_rules. Run get_rule_candidates() to review, then upsert_rule() to enforce and upsert_adr() to document.", highConf)
			}
		}
		resp["session_hint"] = sessionHint
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
		// Sprint 24: enrich resume with Work Ledger data from prior session.
		if s.store != nil && hibernateCtx.ParentID != "" {
			entities, files, _ := s.store.SessionLedgerEntities(hibernateCtx.ParentID)
			if len(entities) > 0 || len(files) > 0 {
				resumeBlock["prior_entities"] = entities
				resumeBlock["prior_files"] = files
			}
		}
		resp["session_resumed"] = resumeBlock

		// Sprint 24.2: Auto-inject compaction recovery packet on hibernate resume.
		// When the agent restarts its editor and reconnects, it has genuinely lost
		// its prior working context. Use the PRIOR session's ID (ParentID) to
		// retrieve the work history — the new session is empty at this point.
		// This is distinct from scope=compaction (mid-session explicit recovery):
		// a hibernate resume is a concrete signal of context loss that requires
		// no agent awareness of compaction.
		if s.store != nil && hibernateCtx.ParentID != "" && !compactionMode {
			recovery := s.buildCompactionRecovery(effectiveAgentID, hibernateCtx.ParentID)
			if recovery != nil {
				recovery["hint"] = "You're resuming a prior session. This packet contains your working state from before the break. File contents can be re-read — focus on decisions and progress."
				resp["compaction_recovery"] = recovery
			}
		}
	}

	// Sprint 24.3: Signal 1 — re-init detection.
	// When the agent calls session_init again on an already-active session (Phase 1
	// same-connection resume: resumed=true, hibernateCtx=nil), it almost certainly
	// just recovered from context compaction. The hibernate resume path (24.2) handles
	// cross-connection resumes; this handles the more common within-connection case.
	// compactionMode is excluded — that path already injects recovery below.
	if sessionResumed && hibernateCtx == nil && !compactionMode && s.store != nil && synapseSessionID != "" {
		cs := s.getCompactDetectState(synapseSessionID)
		if cs.tryMarkInjected() {
			recovery := s.buildCompactionRecovery(effectiveAgentID, synapseSessionID)
			if recovery != nil {
				recovery["hint"] = "You called session_init again on an active session — this typically indicates context compaction. Here is your working state from before."
				resp["compaction_recovery"] = recovery
			} else {
				// Recovery returned nil (empty session) — release the injection slot
				// so future re-inits can retry once the session has accumulated state.
				cs.unmarkInjected()
			}
		}
	}

	// Sprint 24: Work Ledger — include cross-session briefing on every session_init.
	if s.store != nil && synapseSessionID != "" {
		others, _ := s.store.ActiveSessionWork(s.projectID, synapseSessionID, 15)
		if len(others) > 0 {
			var briefing []map[string]interface{}
			for _, o := range others {
				entry := map[string]interface{}{
					"session":     o.SessionID,
					"agent":       o.AgentID,
					"intent":      o.Intent,
					"last_active": o.LastActive,
				}
				if len(o.EntityIDs) > 0 {
					entry["entities"] = o.EntityIDs
				}
				if len(o.FilePaths) > 0 {
					entry["files"] = o.FilePaths
				}
				briefing = append(briefing, entry)
			}
			resp["active_sessions"] = briefing
		}
	}

	// Compaction recovery: when compaction mode is active, enrich the response
	// with a structured recovery packet so the agent can resume effectively.
	if compactionMode && s.store != nil && synapseSessionID != "" {
		recovery := s.buildCompactionRecovery(effectiveAgentID, synapseSessionID)
		if recovery != nil {
			resp["compaction_recovery"] = recovery
		}
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
		driftStart := time.Now()
		driftAlerts := s.federationResolver.CheckDrift(fedCtx, s.store)
		fedCancel()
		// P10-1: emit federation drift detection events to Pulse.
		// Always emit an aggregate "drift_check" event so we can measure check
		// frequency and duration even when no drift is found. Then emit per-project
		// "drift_detected" events (without duration — we only have the aggregate).
		if pc := s.getPulseClient(); pc != nil {
			driftMs := float64(time.Since(driftStart).Milliseconds())
			pc.RecordFederationEvent(pulse.FederationDetectEvent{
				AgentID:    agentID,
				ProjectID:  s.projectID,
				DepsFound:  len(driftAlerts),
				DurationMs: driftMs,
				EventType:  "drift_check",
			})
			// Per-project breakdown when drift is found.
			perProject := make(map[string]int, len(driftAlerts))
			for _, da := range driftAlerts {
				perProject[da.Project]++
			}
			for proj, count := range perProject {
				pc.RecordFederationEvent(pulse.FederationDetectEvent{
					AgentID:        agentID,
					ProjectID:      s.projectID,
					SiblingProject: proj,
					DepsFound:      count,
					EventType:      "drift_detected",
				})
			}
		}
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
				"note":       "These tools are recommended for your intent.",
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
				"note":      "enriches get_context with LLM summaries; required by validate(phase=upsert_adr/list_adrs)",
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
						// Brain panic — degrade gracefully. Update sidecars.brain so agents
						// see available=false rather than the stale available=true set above.
						if sidecars, ok := resp["sidecars"].(map[string]interface{}); ok {
							sidecars["brain"] = map[string]interface{}{
								"available": false,
								"note":      fmt.Sprintf("health check panicked: %v", stripInternalPaths(fmt.Sprint(r))),
							}
						}
						resp["brain_warning"] = fmt.Sprintf("brain health unavailable (internal error: %v)", stripInternalPaths(fmt.Sprint(r)))
					}
				}()
				health := bc.BrainHealth()
				if health == nil {
					// Brain is configured but health check returned nil — may be temporarily
					// unreachable. Update sidecars.brain with a structured warning rather
					// than leaving the field with the stale available=true set above.
					if sidecars, ok := resp["sidecars"].(map[string]interface{}); ok {
						sidecars["brain"] = map[string]interface{}{
							"available": false,
							"note":      "health check returned nil — brain may be temporarily unreachable",
						}
					}
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
			s.goBackground(func() { hintCh <- hintResult{hints: pc.FetchEffectiveness(projID, 5)} })
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
		// Rec #1: surface 7-day session effectiveness trend so agents see their
		// quality history at session start. Uses the same 100ms deadline pattern
		// as context_effectiveness_hints above. Omitted when fewer than 2 prior
		// sessions exist — insufficient data produces no actionable signal.
		if pc := s.getPulseClient(); pc != nil && agentID != "" {
			type trendResult struct{ days []pulse.DailyEffectiveness }
			trendCh := make(chan trendResult, 1)
			aid := agentID
			s.goBackground(func() {
				trendCh <- trendResult{days: pc.GetRecentEffectivenessTrend(7, aid)}
			})
			select {
			case tr := <-trendCh:
				if t := buildSessionTrend(tr.days, 7); t != nil {
					resp["session_effectiveness_trend"] = t
				}
			case <-time.After(100 * time.Millisecond):
				// Pulse sidecar slow or unavailable — skip.
			}
		}
	} // end !quickMode && !resumeMode

	// ── 8. Agent constraints (behavioral rules) ───────────────────────────
	// Surface all agent-type rules (no ForbiddenEdge, conversation-level constraints)
	// so every new session inherits decisions made in prior sessions without
	// the agent needing to re-discover or re-ask for them.
	s.rulesMu.RLock()
	var agentConstraints []map[string]string
	// briefingSecurityRules collects NL descriptions of structural (non-agent)
	// rules for the _briefing section. Capped at 5 to keep the briefing compact.
	var briefingSecurityRules []string
	for _, r := range s.config.Rules {
		if r.IsAgentRule() {
			agentConstraints = append(agentConstraints, map[string]string{
				"id":          r.ID,
				"description": r.Description,
				"severity":    r.Severity,
			})
		} else if r.Description != "" && len(briefingSecurityRules) < 5 {
			sev := strings.ToUpper(r.Severity)
			if sev == "" {
				sev = "WARNING"
			}
			briefingSecurityRules = append(briefingSecurityRules, fmt.Sprintf("%s [%s]", r.Description, sev))
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

	// ── Sprint 24.5: Recent decisions ────────────────────────────────────
	// Surface the 3 most recent decisions so the agent knows what architectural
	// choices were already evaluated — prevents re-deriving prior evaluations.
	// "This decision was already evaluated. X was chosen because Y."
	if s.store != nil {
		if recentDecs, err := s.store.GetRecentDecisions(agentID, s.projectID, 3); err == nil && len(recentDecs) > 0 {
			type decItem struct {
				ID           string `json:"id"`
				Choice       string `json:"choice"`
				Alternatives string `json:"alternatives,omitempty"`
				Reasoning    string `json:"reasoning,omitempty"`
				Context      string `json:"context,omitempty"`
			}
			items := make([]decItem, 0, len(recentDecs))
			for _, d := range recentDecs {
				items = append(items, decItem{
					ID:           d.ID,
					Choice:       d.Choice,
					Alternatives: d.Alternatives,
					Reasoning:    d.Reasoning,
					Context:      d.Context,
				})
			}
			resp["recent_decisions"] = map[string]interface{}{
				"count":     len(items),
				"decisions": items,
				"note":      "These decisions were already evaluated. Use memory(action='list_decisions') to search all decisions or memory(action='decide') to record new ones.",
			}
		}
	}

	// ── Sprint 24.6: Rejected approaches ────────────────────────────────
	// Surface the 3 most recent rejected approaches so the agent knows which
	// paths have already been explored and abandoned. Prevents agents from
	// re-attempting approaches that failed in prior sessions.
	// Warning: "A previous session tried X and abandoned it because Y."
	if s.store != nil {
		if recentRej, err := s.store.GetRecentRejectedApproaches(agentID, s.projectID, 3); err == nil && len(recentRej) > 0 {
			type rejItem struct {
				ID            string `json:"id"`
				Approach      string `json:"approach"`
				FailureReason string `json:"failure_reason"`
				Blocker       string `json:"blocker,omitempty"`
				Context       string `json:"context,omitempty"`
				CreatedAt     int64  `json:"created_at"`
			}
			items := make([]rejItem, 0, len(recentRej))
			for _, r := range recentRej {
				items = append(items, rejItem{
					ID:            r.ID,
					Approach:      r.Approach,
					FailureReason: r.FailureReason,
					Blocker:       r.Blocker,
					Context:       r.Context,
					CreatedAt:     r.CreatedAt,
				})
			}
			resp["rejected_approaches"] = map[string]interface{}{
				"count":    len(items),
				"entries":  items,
				"warning":  "The following approaches were tried in prior sessions and explicitly abandoned. Avoid re-attempting these paths unless the underlying blocker has been resolved.",
				"note":     "Use memory(action='list_rejected') to search all rejected approaches or memory(action='abandon') to record new ones.",
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

	// ── Knowledge graph stats (Sprint 16 #6) ─────────────────────────────
	// Expose entity counts by domain, cross-domain edge breakdown, and
	// freshness so agents can self-calibrate before issuing cross-domain queries.
	// Excluded from quick mode — agents in quick mode want minimal tokens.
	// Always included in full and resume modes.
	if s.graph != nil && !quickMode {
		// NodeCountsByDomain is O(N) under one read lock — no pointer copy,
		// no sort. This avoids the O(N log N) cost of AllNodes() which also
		// runs inside ProjectIdentity() earlier in this function.
		domainCounts := s.graph.NodeCountsByDomain()

		// Build sorted active-domains list for stable, deterministic output.
		sortedDomains := make([]string, 0, len(domainCounts))
		entitiesByDomain := make(map[string]int, len(domainCounts))
		for d, cnt := range domainCounts {
			sortedDomains = append(sortedDomains, string(d))
			entitiesByDomain[string(d)] = cnt
		}
		sort.Strings(sortedDomains)

		// Count cross-domain edges via a single aggregating SQL query.
		// All rows in manual_edges are cross-domain by design (only the
		// name-matcher and link_entities write to this table, both for
		// cross-domain connections). No IsCrossDomainEdge filtering needed —
		// that would silently exclude user-defined custom relation strings.
		var autoEdges, confirmedEdges, manualEdges int
		var cdStatsErr string
		if s.store != nil {
			var err error
			autoEdges, confirmedEdges, manualEdges, err = s.store.CrossDomainEdgeStats()
			if err != nil {
				logutil.Warn("session_init: CrossDomainEdgeStats: %v\n", err)
				cdStatsErr = "cross-domain stats temporarily unavailable"
			}
		}
		total := autoEdges + confirmedEdges + manualEdges

		// Freshness: live-indexed means the watcher re-indexes on every file
		// change event. "N files changed in last 15 min" tells agents which
		// areas of the graph were recently updated — useful for cache invalidation
		// decisions. We do NOT claim the graph is fully consistent because during
		// a burst of changes the indexer may lag by a few seconds.
		freshness := "live"
		if len(recentChanges) > 0 {
			freshness = fmt.Sprintf("live (%d files re-indexed in last 15 min)", len(recentChanges))
		}

		kgSection := map[string]interface{}{
			"entities_by_domain": entitiesByDomain,
			"active_domains":     sortedDomains,
			"cross_domain_edges": map[string]interface{}{
				"auto":      autoEdges,
				"confirmed": confirmedEdges,
				"manual":    manualEdges,
				"total":     total,
			},
			"freshness": freshness,
		}
		if cdStatsErr != "" {
			kgSection["stats_error"] = cdStatsErr
		}
		// Guide agents toward confirm_edge when unreviewed auto edges exist.
		if autoEdges > 0 {
			kgSection["hint"] = fmt.Sprintf(
				"%d auto-detected cross-domain edges await review. "+
					"Use confirm_edge(a, b, relation, confirmed=true/false) to approve or reject them. "+
					"Unconfirmed edges are included in get_context and get_impact at reduced weight.",
				autoEdges,
			)
		}
		resp["knowledge_graph"] = kgSection
	}

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

	// ── First-session "wow" moment (Sprint 18 #1) ────────────────────────
	// On the project's very first session, surface surprising insights that
	// demonstrate Synapses' value immediately: dead code, high-risk entities
	// with no test coverage, and active architectural violations.
	//
	// Fires in ALL scope modes (including quickMode) because this event is
	// one-shot — if suppressed on the first call, count becomes ≥2 and
	// highlights never appear again. In quickMode a compact (counts-only)
	// version is shown to respect the token budget.
	if isFirstProjectSession && s.graph != nil {
		var vlog []store.ViolationLogEntry
		if s.store != nil && !quickMode {
			// Fetch violation details only in non-quick modes; quickMode shows counts only.
			vlog, _ = s.store.GetViolationLog("", 20)
		}
		// Build recently-changed file set for recency-boosted risk sorting.
		// Entities in files touched within the last 15 min rank above equal-fanin
		// entities that are dormant — agent is actively working in those files.
		recentFileSet := make(map[string]bool, len(recentChanges))
		for _, rc := range recentChanges {
			if rc.File != "" {
				recentFileSet[rc.File] = true
			}
		}
		if highlights := computeFirstSessionHighlights(s.graph, vlog, recentFileSet, quickMode); highlights != nil {
			resp["first_session_highlights"] = highlights
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

	// Progressive disclosure: list sections suppressed in this scope so agents
	// know what to request with scope="full".
	//
	// Sections suppressed per scope:
	//   standard/quick (!quickMode&&!resumeMode AND !quickMode guards fire):
	//     all 12 rich sections are deferred.
	//   resume (!quickMode&&!resumeMode fires, !quickMode does NOT fire):
	//     only the 8 sections guarded by !quickMode&&!resumeMode are deferred;
	//     federation_health, relevant_memories, previous_session_work, knowledge_graph
	//     are already present in resume mode.
	if quickMode || resumeMode {
		var deferred []string
		if quickMode {
			// standard/quick: ALL rich sections are suppressed.
			deferred = []string{
				"project_identity", "session_hint", "recent_events",
				"sidecars", "brain_health", "federation_health",
				"relevant_memories", "previous_session_work", "knowledge_graph",
				"daemon_health", "context_effectiveness_hints", "session_effectiveness_trend",
			}
		} else {
			// resume: only sections guarded by !quickMode&&!resumeMode are suppressed.
			// federation_health, relevant_memories, previous_session_work, knowledge_graph
			// are NOT in this list because they ARE present in resume mode.
			deferred = []string{
				"project_identity", "session_hint", "recent_events",
				"sidecars", "brain_health",
				"daemon_health", "context_effectiveness_hints", "session_effectiveness_trend",
			}
		}
		resp["more_available"] = map[string]interface{}{
			"sections": deferred,
			"hint":     "Pass scope=\"full\" to session_init to receive all sections.",
		}
	}

	// ── Tool tiers (V2-C1) ────────────────────────────────────────────────
	// Surface standard-tier tools (beyond core) so agents know what is
	// available without calling discover_tools. Suppressed in quickMode/resumeMode
	// to respect the token budget — the hint in more_available covers this case.
	// Advanced tools are discoverable via discover_tools("what tools exist").
	if !quickMode && !resumeMode {
		var standardOnly []string
		for name := range standardTierTools {
			if !coreTierTools[name] {
				standardOnly = append(standardOnly, name)
			}
		}
		sort.Strings(standardOnly)
		if len(standardOnly) > 0 {
			resp["more_tools"] = map[string]interface{}{
				"standard": standardOnly,
				"hint": "These standard tools are always available. " +
					"Call discover_tools(query=\"...\") to find advanced tools by intent, " +
					"or discover_tools(query=\"list all\") to see everything.",
			}
		}
	}

	// _briefing: morning briefing — the five highest-priority items an agent
	// needs at session start. Natural language only; no code snippets. Present
	// in ALL scopes (including quickMode) so every agent gets oriented first.
	//
	//   (1) unfinished_work  — in-progress/pending tasks + prior session summary
	//   (2) conventions      — auto-load project prompts + key agent constraints
	//   (3) drift_alerts     — federation drift + active architectural violations
	//   (4) security_rules   — structural rule descriptions (NL, capped at 5)
	//   (5) recent_decisions — decision episodes from last 30 days (capped at 3)
	{
		briefing := make(map[string]interface{})

		// (1) Unfinished work.
		{
			var inProg, pend []string
			if tasks, ok := pendingSection["tasks"].([]taskWithState); ok {
				for _, t := range tasks {
					title := strings.TrimSpace(t.Title)
					// Normalize: use only the first line and cap length so
					// multi-line task titles don't corrupt the NL briefing.
					// TrimRight removes a trailing \r left by Windows-style \r\n endings.
					if nl := strings.IndexByte(title, '\n'); nl >= 0 {
						title = strings.TrimRight(title[:nl], "\r")
					}
					if rs := []rune(title); len(rs) > 80 {
						title = string(rs[:80]) + "…"
					}
					if title == "" {
						title = t.ID
					}
					switch t.Status {
					case "in_progress":
						inProg = append(inProg, title)
					case "pending":
						pend = append(pend, title)
					}
				}
			}
			// Cap displayed task names at 3 to keep the briefing compact.
			inProgCap := inProg
			if len(inProgCap) > 3 {
				inProgCap = inProgCap[:3]
			}
			var prevPkgNote string
			if pw, ok := resp["previous_session_work"].(map[string]interface{}); ok {
				if pkgs, ok := pw["packages"].([]PackageWork); ok && len(pkgs) > 0 {
					prevPkgNote = fmt.Sprintf(" Previous session: %d package(s) touched.", len(pkgs))
				}
			}
			var staleNote string
			if len(staleSessions) > 0 {
				staleNote = fmt.Sprintf(" %d session(s) ended without clean shutdown (review stale_sessions).", len(staleSessions))
			}
			var w string
			switch {
			case len(inProg) > 0 && len(pend) > 0:
				w = fmt.Sprintf("%d in-progress: %s. %d more pending.%s%s",
					len(inProg), strings.Join(inProgCap, ", "),
					len(pend), prevPkgNote, staleNote)
			case len(inProg) > 0:
				w = fmt.Sprintf("%d task(s) in progress: %s.%s%s",
					len(inProg), strings.Join(inProgCap, ", "),
					prevPkgNote, staleNote)
			case len(pend) > 0:
				w = fmt.Sprintf("%d task(s) pending (none in progress yet).%s%s",
					len(pend), prevPkgNote, staleNote)
			default:
				w = "No unfinished work from prior sessions." + prevPkgNote + staleNote
			}
			briefing["unfinished_work"] = w
		}

		// (2) Conventions: formatter detection + project prompts + agent-type rules.
		// Formatter conventions are prepended so agents always learn about
		// auto-formatting tools before other project norms. Agent rules are
		// manually configured behavioral conventions (e.g. "All handlers use
		// AuthMiddleware", "DB access goes through repository layer").
		// Sprint 29 will add cross-session learned conventions here; Sprint 23
		// establishes the delivery slot and populates it from configured rules.
		// Cap: formatter conventions are always included; 5 from prompts, up to
		// 8 total from prompts+rules so rule-heavy projects stay terse.
		{
			// Prepend formatter conventions so they are always delivered
			// regardless of how many prompts or rules the project has.
			// cachedFormatterConventions computes once per server lifetime.
			convs := s.cachedFormatterConventions()
			// promptsAdded tracks how many prompt bodies have been appended so
			// that the per-prompt cap (5) is counted independently of the
			// formatter conventions already in convs.
			promptsAdded := 0
			if ap, ok := resp["active_prompts"].(map[string]interface{}); ok {
				if pl, ok := ap["prompts"].([]map[string]string); ok {
					for _, p := range pl {
						body := p["body"]
						if body == "" {
							continue
						}
						// Surface only the first line to keep conventions terse.
						// TrimRight strips a trailing \r left by Windows-style \r\n endings.
						if nl := strings.IndexByte(body, '\n'); nl > 0 {
							body = strings.TrimRight(body[:nl], "\r")
						}
						// Rune-safe truncation: avoid splitting multi-byte UTF-8 sequences.
						if rs := []rune(body); len(rs) > 120 {
							body = string(rs[:120]) + "…"
						}
						convs = append(convs, body)
						promptsAdded++
						if promptsAdded >= 5 {
							break
						}
					}
				}
			}
			// Include all agent-type constraints (not just error severity) as
			// conventions. Every agent rule is a behavioral norm the agent must
			// follow regardless of severity — warning rules are equally important
			// for project conventions like testing style and layer boundaries.
			// The 8-item cap applies only to prompts+rules (formatter conventions
			// are included on top of this limit).
			rulesAdded := 0
			for _, ac := range agentConstraints {
				if promptsAdded+rulesAdded >= 8 {
					break
				}
				desc := ac["description"]
				if desc == "" {
					continue
				}
				// Apply the same rune-safe 120-char cap as prompt bodies so a
				// long rule description can't bloat the briefing.
				if rs := []rune(desc); len(rs) > 120 {
					desc = string(rs[:120]) + "…"
				}
				convs = append(convs, desc)
				rulesAdded++
			}
			if convs == nil {
				convs = []string{}
			}
			briefing["conventions"] = convs
		}

		// (3) Drift alerts: federation drift warnings + active violations count.
		{
			driftCount := 0
			if warnings, ok := resp["warnings"].([]string); ok {
				driftCount = len(warnings)
			}
			violCount := 0
			if ws, ok := resp["working_state"].(map[string]interface{}); ok {
				if v, ok := ws["active_violations"].(int); ok {
					violCount = v
				}
			}
			var msg string
			switch {
			case driftCount > 0 && violCount > 0:
				msg = fmt.Sprintf("%d federation drift alert(s) and %d active architectural violation(s). Review warnings and cross_project_drift before proceeding.",
					driftCount, violCount)
			case driftCount > 0:
				msg = fmt.Sprintf("%d federation drift alert(s). Check cross_project_drift for details.", driftCount)
			case violCount > 0:
				msg = fmt.Sprintf("%d active architectural violation(s). Run validate() to review.", violCount)
			default:
				msg = "No active drift or violations."
			}
			briefing["drift_alerts"] = msg
		}

		// (4) Security rules: NL descriptions of structural (non-agent) rules.
		// Collected alongside agentConstraints above; capped at 5.
		if briefingSecurityRules == nil {
			briefingSecurityRules = []string{}
		}
		briefing["security_rules"] = briefingSecurityRules

		// (5) Recent decisions and rejected approaches.
		//
		// Decision episodes: explicit decisions stored via memory(action=save).
		// Rejected approaches: the most relevant failure episode relative to the
		// current working context (already computed as recentFailure above).
		// Both are surfaced together so the agent avoids repeating past mistakes.
		{
			var decisions []map[string]interface{}
			if s.store != nil && primaryRepoID != "" {
				if eps, err := s.store.GetEpisodes(primaryRepoID, "", "decision", nil, 3, 30); err == nil {
					for _, e := range eps {
						if e.Decision == "" {
							continue // skip malformed episodes with no decision text
						}
						d := map[string]interface{}{
							"type":     "decision",
							"decision": e.Decision,
						}
						if e.Outcome != "" {
							d["outcome"] = e.Outcome
						}
						if e.CreatedAt > 0 {
							age := time.Since(time.Unix(e.CreatedAt, 0))
							d["when"] = formatSessionDuration(age) + " ago"
						}
						decisions = append(decisions, d)
					}
				}
			}
			// Include the most relevant rejected approach (failure episode) when
			// available — drawn from resp["recent_failure"] which is already computed.
			if rf, ok := resp["recent_failure"].(map[string]interface{}); ok {
				if dec, _ := rf["decision"].(string); dec != "" {
					rejected := map[string]interface{}{
						"type":     "rejected_approach",
						"decision": dec,
					}
					if outcome, _ := rf["outcome"].(string); outcome != "" {
						rejected["outcome"] = outcome
					}
					if at, _ := rf["created_at"].(int64); at > 0 {
						age := time.Since(time.Unix(at, 0))
						rejected["when"] = formatSessionDuration(age) + " ago"
					}
					decisions = append(decisions, rejected)
				}
			}
			if len(decisions) > 0 {
				briefing["recent_decisions"] = decisions
			}
		}

		resp["_briefing"] = briefing
	}

	// _summary: one-line template-based digest — no LLM, negligible tokens.
	// Helps agents scan response content without parsing every nested field.
	// Format: "{N} pending tasks, {M} recent changes, {V} violations. {federation_status}."
	{
		taskCount := 0
		if pt, ok := resp["pending_tasks"].(map[string]interface{}); ok {
			if c, ok := pt["count"].(int); ok {
				taskCount = c
			}
		}
		branch := ""
		if ws, ok := resp["working_state"].(map[string]interface{}); ok {
			if b, ok := ws["branch"].(string); ok {
				branch = b
			}
		}
		recentChangeCount := 0
		if re, ok := resp["recent_events"].(map[string]interface{}); ok {
			if c, ok := re["count"].(int); ok {
				recentChangeCount = c
			}
		}
		violationCount := 0
		if sa, ok := resp["safety_alerts"].(map[string]interface{}); ok {
			if v, ok := sa["violations"].([]interface{}); ok {
				violationCount = len(v)
			}
		}
		var parts []string
		parts = append(parts, fmt.Sprintf("%d pending task(s)", taskCount))
		if recentChangeCount > 0 {
			parts = append(parts, fmt.Sprintf("%d recent change(s)", recentChangeCount))
		}
		if violationCount > 0 {
			parts = append(parts, fmt.Sprintf("%d violation(s)", violationCount))
		}
		if branch != "" {
			parts = append(parts, fmt.Sprintf("branch=%s", branch))
		}
		if agentID != "" {
			parts = append(parts, fmt.Sprintf("agent=%s", agentID))
		}
		// Federation status
		if fed, ok := resp["federation_health"].(map[string]interface{}); ok {
			if status, ok := fed["status"].(string); ok && status != "" {
				parts = append(parts, fmt.Sprintf("federation=%s", status))
			}
		}
		// First-session highlights signal — agents scanning _summary see it immediately.
		if fsh, ok := resp["first_session_highlights"].(map[string]interface{}); ok {
			var fshParts []string
			if n, ok := fsh["dead_code_count"].(int); ok && n > 0 {
				fshParts = append(fshParts, fmt.Sprintf("%d dead", n))
			} else if dc, ok := fsh["dead_code"].(map[string]interface{}); ok {
				if n, ok := dc["total"].(int); ok && n > 0 {
					fshParts = append(fshParts, fmt.Sprintf("%d dead", n))
				}
			}
			if n, ok := fsh["high_risk_count"].(int); ok && n > 0 {
				fshParts = append(fshParts, fmt.Sprintf("%d high-risk", n))
			} else if hr, ok := fsh["high_risk_entities"].(map[string]interface{}); ok {
				if n, ok := hr["total"].(int); ok && n > 0 {
					fshParts = append(fshParts, fmt.Sprintf("%d high-risk", n))
				}
			}
			if len(fshParts) > 0 {
				parts = append(parts, fmt.Sprintf("first-session: %s", strings.Join(fshParts, ", ")))
			}
		}
		resp["_summary"] = strings.Join(parts, "; ")
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

// handleGetMyAnalytics returns a per-agent analytics summary for the current agent
// (Bug 57 — STO-D.4.5). This is the agent-facing complement to the admin HTTP
// /api/admin/pulse/summary endpoint.
func (s *Server) handleGetMyAnalytics(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	agentID, _ := args["agent_id"].(string)
	if agentID == "" {
		agentID = s.getLastAgent()
	}
	days := 7
	if v, ok := args["days"].(float64); ok && v > 0 && v <= 90 {
		days = int(v)
	}

	pc := s.getPulseClient()
	if pc == nil {
		return jsonResult(map[string]interface{}{
			"available": false,
			"note":      "Analytics not available (pulse not initialised).",
		})
	}

	// Get a project-scoped summary for the current project filtered by agent.
	sum := pc.GetSummaryForProject(days, s.projectID)

	result := map[string]interface{}{
		"available":  true,
		"days":       days,
		"agent_id":   agentID,
		"project_id": s.projectID,
	}

	if sum != nil && sum.Summary != nil {
		result["tool_calls"] = sum.Summary.TotalToolCalls
		result["context_deliveries"] = sum.Summary.ContextDeliveries
		result["tokens_saved"] = sum.Summary.TokensSaved
		result["cost_saved_usd"] = sum.Summary.CostSavedUSD
		result["sessions"] = sum.Summary.Sessions
		result["tasks_completed"] = sum.Summary.TasksCompleted
		result["cache_hit_rate"] = sum.Summary.CacheHitRate
		result["avg_latency_ms"] = sum.Summary.AvgLatencyMs
		result["savings_pct"] = sum.Summary.SavingsPct
	}

	// Effectiveness insights — agent can use this to understand which entities
	// their context requests are landing well on.
	insights := pc.FetchEffectiveness(s.projectID, 2)
	if len(insights) > 0 {
		type insight struct {
			Entity     string  `json:"entity"`
			Score      float64 `json:"score"`
			Signals    int     `json:"signals"`
			Suggestion string  `json:"suggestion"`
		}
		top := make([]insight, 0, len(insights))
		for _, e := range insights {
			top = append(top, insight{
				Entity:     e.Entity,
				Score:      e.Score,
				Signals:    e.Signals,
				Suggestion: e.Suggestion,
			})
		}
		result["effectiveness_insights"] = top
	}

	// FCRR tells the agent how often its context was "right first time".
	fcrr := pc.GetFirstContextRightRate(days)
	result["first_context_right_rate"] = fcrr

	return jsonResult(result)
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
		return mcp.NewToolResultError(stripInternalPaths(noteErr.Error())), nil
	}
	if note == "" {
		return mcp.NewToolResultError("note is required (a brief annotation, e.g., 'this function has O(n²) complexity')"), nil
	}
	agentID, _ := req.GetArguments()["agent_id"].(string)

	// OF-S2: scan note content for prompt injection patterns.
	var injectionWarning string
	if scanResult, scanErr := s.scanContent("note", note); scanErr != nil {
		return mcp.NewToolResultError(stripInternalPaths(scanErr.Error())), nil
	} else {
		note = scanResult.sanitized
		if scanResult.warning != "" {
			injectionWarning = scanResult.warning
			// P7-1: emit guard event for injection scan trigger.
			if pc := s.getPulseClient(); pc != nil {
				pc.RecordGuardEvent(pulse.GuardEvent{
					GuardType: "injection_scan", ToolName: "annotate_node",
					Category: "warn", AgentID: agentID, ProjectID: s.projectID,
				})
			}
		}
	}

	// Verify the node exists in the graph.
	if s.graph.GetNode(graph.NodeID(nodeID)) == nil {
		return mcp.NewToolResultError(fmt.Sprintf("node not found: %q", nodeID)), nil
	}

	id, err := s.store.AddAnnotation(nodeID, agentID, note)
	if err != nil {
		return toolError("add annotation", err)
	}

	// P7-11: emit memory op for annotation write.
	if pc := s.getPulseClient(); pc != nil {
		pc.RecordMemoryOp(pulse.MemoryOperationEvent{
			Operation: "annotation_write", Tier: "entity",
			ResultCount: 1, AgentID: agentID, ProjectID: s.projectID,
		})
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

// trimRepoRootSingle trims the repo root prefix from a single path.
func (s *Server) trimRepoRootSingle(p string) string {
	root := s.graph.Root()
	if root == "" || p == "" {
		return p
	}
	prefix := root
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return strings.TrimPrefix(p, prefix)
}

// ── Compaction Recovery ──────────────────────────────────────────────────────

// buildCompactionRecovery assembles a structured recovery packet for an agent
// that has compacted its context. Surfaces the knowledge that is typically lost
// during compaction: work summary, decisions, failures, rules, violations,
// and entity memories.
func (s *Server) buildCompactionRecovery(agentID, sessionID string) map[string]interface{} {
	if s.store == nil {
		return nil
	}

	// 1. Get work ledger entities/files for this session
	entities, files, _ := s.store.SessionLedgerEntities(sessionID)

	// 2. Get active session state (from any in-progress task)
	var activeState *store.SessionState
	if tasks, err := s.store.GetPendingTasks("", agentID); err == nil {
		for _, t := range tasks {
			if t.Status == "in_progress" {
				if st, stErr := s.store.GetSessionState(t.ID); stErr == nil && st != nil {
					activeState = st
					break
				}
			}
		}
	}

	// 3. Build work summary narrative
	workSummary := synthesizeWorkSummary(entities, files, activeState)

	// 4. Get session episodes (decisions and failures) via time window
	var sessionDecisions, sessionFailures []compactEpisode
	if sess, err := s.store.GetSession(sessionID); err == nil && sess != nil {
		start := time.Unix(sess.StartedAt, 0)
		end := time.Now()
		if decisions, err := s.store.GetEpisodesByTimeWindow(agentID, s.projectID, start, end, "decision", 5); err == nil {
			for _, ep := range decisions {
				sessionDecisions = append(sessionDecisions, compactEpisode{
					Decision:  ep.Decision,
					Rationale: ep.Rationale,
					Outcome:   ep.Outcome,
				})
			}
		}
		if failures, err := s.store.GetEpisodesByTimeWindow(agentID, s.projectID, start, end, "failure", 3); err == nil {
			for _, ep := range failures {
				sessionFailures = append(sessionFailures, compactEpisode{
					Decision:  ep.Decision,
					Rationale: ep.Rationale,
					Outcome:   ep.Outcome,
				})
			}
		}
	}

	// 5. Get applicable rules and violations for touched files
	var rules []compactRule
	if matched, err := s.store.GetRulesForFiles(files); err == nil {
		for _, r := range matched {
			rules = append(rules, compactRule{
				ID:          r.ID,
				Description: r.Description,
				Severity:    r.Severity,
			})
		}
	}

	var violations []compactViolation
	if matched, err := s.store.GetViolationsForFiles(files, 5); err == nil {
		for _, v := range matched {
			violations = append(violations, compactViolation{
				RuleID:   v.RuleID,
				Severity: v.Severity,
				FromNode: v.FromNode,
				ToNode:   v.ToNode,
			})
		}
	}

	// 6. Get entity memories
	var entityMemories []compactMemory
	if mems, err := s.store.GetMemoriesForEntitySet(entities, 10); err == nil {
		for _, m := range mems {
			entityMemories = append(entityMemories, compactMemory{
				EntityID: m.EntityID,
				Content:  m.Content,
			})
		}
	}

	// 7. Entity importance ranking + relationship map (merged from guide)
	importance := s.rankEntityImportance(sessionID, entities)
	relationships := s.buildRelationshipMap(entities)

	// 7b. Sprint 24.1: Exploration log — what was queried and what was found.
	// This layer gives the recovery packet concrete intelligence ("AuthService: 5
	// callers, 2 security constraints") rather than just entity names.
	var exploredEntities []compactExploration
	// Fetch a larger window and deduplicate by entity name (most-recent-first
	// from the query), keeping up to 10 unique entries. This prevents a single
	// repeatedly-queried entity from consuming all 10 slots.
	if elog, err := s.store.GetSessionExplorationLog(sessionID, 30); err == nil {
		seen := make(map[string]bool, len(elog))
		for _, e := range elog {
			if e.EntityQueried == "" && e.FindingSummary == "" {
				continue
			}
			if e.EntityQueried != "" {
				if seen[e.EntityQueried] {
					continue
				}
				seen[e.EntityQueried] = true
			}
			exploredEntities = append(exploredEntities, compactExploration{
				Entity:  e.EntityQueried,
				Tool:    e.ToolName,
				Finding: e.FindingSummary,
			})
			if len(exploredEntities) >= 10 {
				break
			}
		}
	}

	// 7c. Task progress — list pending/in_progress tasks for the agent so the
	// recovery packet answers "what was I doing?" at the task level.
	// Uses agent-scoped task query (not session-scoped) to capture any active
	// plan work regardless of which session created the tasks.
	var taskProgress *compactTaskProgress
	if tasks, err := s.store.GetPendingTasks("", agentID); err == nil && len(tasks) > 0 {
		tp := &compactTaskProgress{}
		maxList := 5
		for _, t := range tasks {
			switch t.Status {
			case "in_progress":
				if len(tp.InProgress) < maxList {
					tp.InProgress = append(tp.InProgress, compactTask{Title: t.Title, ID: t.ID})
				}
			case "pending":
				if len(tp.Pending) < maxList {
					tp.Pending = append(tp.Pending, compactTask{Title: t.Title, ID: t.ID})
				}
			}
		}
		// Only include task progress if there are active or pending tasks.
		if len(tp.InProgress) > 0 || len(tp.Pending) > 0 {
			taskProgress = tp
		}
	}

	// 8. Assemble the recovery packet
	recovery := map[string]interface{}{
		"work_summary": workSummary,
		"hint":         "Your context was compacted. This recovery packet contains your prior work state from Synapses. File contents and graph traversals can be re-queried — focus on decisions and approach.",
	}
	if taskProgress != nil {
		recovery["task_progress"] = taskProgress
	}
	if len(exploredEntities) > 0 {
		recovery["explored_entities"] = exploredEntities
	}
	if len(sessionDecisions) > 0 {
		recovery["session_decisions"] = sessionDecisions
	}
	if len(sessionFailures) > 0 {
		recovery["session_failures"] = sessionFailures
	}
	if len(rules) > 0 {
		recovery["active_rules"] = rules
	}
	if len(violations) > 0 {
		recovery["active_violations"] = violations
	}
	if len(entityMemories) > 0 {
		recovery["entity_memories"] = entityMemories
	}
	if activeState != nil && activeState.ContextSnapshot != "" {
		recovery["context_snapshot"] = activeState.ContextSnapshot
	}
	if len(importance) > 0 {
		recovery["entity_importance"] = importance
	}
	if len(relationships) > 0 {
		recovery["relationship_map"] = relationships
	}

	// 9. Sprint 24.4: Active hypotheses — inject working theories that survived
	// to recovery so the agent doesn't lose its in-progress reasoning thread.
	// Only ACTIVE hypotheses matter; confirmed/rejected are already resolved.
	if hyps, err := s.store.GetActiveHypotheses(agentID, s.projectID, 5); err == nil && len(hyps) > 0 {
		type compactHypothesis struct {
			ID      string `json:"id"`
			Content string `json:"content"`
			State   string `json:"state"`
		}
		items := make([]compactHypothesis, 0, len(hyps))
		for _, h := range hyps {
			items = append(items, compactHypothesis{
				ID:      h.ID,
				Content: h.Content,
				State:   h.State,
			})
		}
		recovery["active_hypotheses"] = items
	}

	// 10. Sprint 24.5: Recent decisions — inject structured decision records so the
	// agent doesn't re-derive past architectural choices after context compaction.
	// Up to 5 most recent decisions; omitted when none exist for this agent/project.
	if decisions, err := s.store.GetRecentDecisions(agentID, s.projectID, 5); err == nil && len(decisions) > 0 {
		type compactDecision struct {
			ID           string `json:"id"`
			Choice       string `json:"choice"`
			Alternatives string `json:"alternatives,omitempty"`
			Reasoning    string `json:"reasoning,omitempty"`
			Context      string `json:"context,omitempty"`
		}
		items := make([]compactDecision, 0, len(decisions))
		for _, d := range decisions {
			items = append(items, compactDecision{
				ID:           d.ID,
				Choice:       d.Choice,
				Alternatives: d.Alternatives,
				Reasoning:    d.Reasoning,
				Context:      d.Context,
			})
		}
		recovery["recent_decisions"] = items
	}

	// 11. Sprint 24.6: Rejected approaches — surface approaches abandoned in prior
	// sessions so the agent doesn't re-attempt them after context compaction.
	// Up to 3 most recent; omitted when none exist for this agent/project.
	if rejApproaches, err := s.store.GetRecentRejectedApproaches(agentID, s.projectID, 3); err == nil && len(rejApproaches) > 0 {
		type compactRejectedApproach struct {
			ID            string `json:"id"`
			Approach      string `json:"approach"`
			FailureReason string `json:"failure_reason"`
			Blocker       string `json:"blocker,omitempty"`
		}
		items := make([]compactRejectedApproach, 0, len(rejApproaches))
		for _, r := range rejApproaches {
			items = append(items, compactRejectedApproach{
				ID:            r.ID,
				Approach:      r.Approach,
				FailureReason: r.FailureReason,
				Blocker:       r.Blocker,
			})
		}
		recovery["rejected_approaches"] = items
	}

	// 12. Token budget enforcement: truncate if over ~8000 chars (~2000 tokens)
	truncateCompactionPacket(recovery, 8000)

	return recovery
}

// compactExploration is a lightweight representation of one exploration log entry
// for the compaction recovery packet. It surfaces what was queried and what was
// learned during this session — the "institutional memory" layer.
type compactExploration struct {
	Entity  string `json:"entity,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Finding string `json:"finding,omitempty"`
}

// compactEpisode is a lightweight episode representation for compaction recovery.
type compactEpisode struct {
	Decision  string `json:"decision"`
	Rationale string `json:"rationale,omitempty"`
	Outcome   string `json:"outcome"`
}

// compactRule is a lightweight rule representation for compaction recovery.
type compactRule struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Severity    string `json:"severity"`
}

// compactViolation is a lightweight violation representation for compaction recovery.
type compactViolation struct {
	RuleID   string `json:"rule_id"`
	Severity string `json:"severity"`
	FromNode string `json:"from_node"`
	ToNode   string `json:"to_node"`
}

// compactTask is a lightweight task representation for compaction recovery.
type compactTask struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// compactTaskProgress summarizes active task state for the recovery packet.
// In-progress tasks represent what the agent was actively working on; pending
// tasks show what comes next. Completed tasks are omitted — they're not relevant
// to recovery orientation.
type compactTaskProgress struct {
	InProgress []compactTask `json:"in_progress,omitempty"`
	Pending    []compactTask `json:"pending,omitempty"`
}

// compactMemory is a lightweight memory representation for compaction recovery.
type compactMemory struct {
	EntityID string `json:"entity_id"`
	Content  string `json:"content"`
}

// truncateCompactionPacket progressively drops lower-priority items from the
// recovery packet to fit within the character budget. Drop order:
// violations → memories → failures → decisions → rules.
// Always preserves work_summary and hint (the minimum useful payload).
// Safe for single-goroutine use — caller must not share packet concurrently.
func truncateCompactionPacket(packet map[string]interface{}, maxChars int) {
	data, err := json.Marshal(packet)
	if err != nil || len(data) <= maxChars {
		return
	}
	// Progressive truncation in priority order (lowest value first).
	// work_summary and hint are never dropped — they're the minimum useful payload.
	// task_progress is high-value (answers "what was I doing?") so drops late.
	// explored_entities is dropped after entity_memories (high-value intelligence,
	// but work_summary already captures entity/file names as a fallback).
	dropOrder := []string{"relationship_map", "entity_importance", "active_violations", "entity_memories", "explored_entities", "session_failures", "session_decisions", "recent_decisions", "rejected_approaches", "active_hypotheses", "active_rules", "task_progress", "context_snapshot"}
	for _, key := range dropOrder {
		if len(data) <= maxChars {
			return
		}
		if _, ok := packet[key]; ok {
			delete(packet, key)
			data, _ = json.Marshal(packet)
		}
	}
}

// computeFirstSessionHighlights analyses the code graph for patterns that
// demonstrate immediate value on a project's first session:
//
//   - Dead code: functions/methods with no CALLS in-edges and no test callers.
//   - High-risk entities: high call fanin but no test coverage.
//   - Architectural violations: active rule violations from the violation log.
//
// compact=true returns counts and hint only (no sample arrays) for quickMode.
// recentFiles: set of recently-changed file paths for recency-boosted sorting.
// Uses a single graph snapshot (one RLock) for efficiency — O(N+E).
// Returns nil when the graph is empty or no findings are present.
func computeFirstSessionHighlights(g *graph.Graph, vlog []store.ViolationLogEntry, recentFiles map[string]bool, compact bool) map[string]interface{} {
	// Single-lock snapshot: avoids holding the read lock across individual
	// per-node queries, which would require N separate lock acquisitions.
	outEdges, nodes := g.SnapshotEdgesAndNodes()
	if len(nodes) == 0 {
		return nil
	}

	// Build per-node CALLS fanin and test-caller flag from the snapshot.
	// Only EdgeCalls is considered — structural edges (EdgeContains, EdgeImports,
	// etc.) would produce false-positive "callers" for every container node.
	callFanin := make(map[graph.NodeID]int, len(nodes)/4)
	hasTestCaller := make(map[graph.NodeID]bool)
	for fromID, edges := range outEdges {
		fromNode := nodes[fromID]
		isTestFile := fromNode != nil && strings.HasSuffix(fromNode.File, "_test.go")
		for _, e := range edges {
			if e.Type == graph.EdgeCalls {
				callFanin[e.To]++
				if isTestFile {
					hasTestCaller[e.To] = true
				}
			}
		}
	}

	type deadEntry struct {
		Name string `json:"name"`
		File string `json:"file"`
		Type string `json:"type"`
	}
	type riskEntry struct {
		Name            string `json:"name"`
		File            string `json:"file"`
		Type            string `json:"type"`
		Fanin           int    `json:"call_fanin"`
		RecentlyChanged bool   `json:"recently_changed,omitempty"`
		Note            string `json:"note"`
		score           int    // internal sort key: fanin × recency_multiplier; not serialised
	}

	var deadCode []deadEntry
	var highRisk []riskEntry

	for id, n := range nodes {
		// Only analyse callable entities — skip files, packages, structs, etc.
		if n.Type != graph.NodeFunction && n.Type != graph.NodeMethod {
			continue
		}
		fanin := callFanin[id]
		tested := hasTestCaller[id]

		// Dead code: no callers, never exercised by tests, AND unexported.
		// Exported functions are public API — external packages may call them
		// even if no in-repo callers exist (graph only sees the current repo).
		// main() and init() are unexported Go entry points with no CALLS edges
		// by design; flagging them would produce misleading noise.
		if fanin == 0 && !tested && !n.Exported && n.Name != "main" && n.Name != "init" {
			deadCode = append(deadCode, deadEntry{
				Name: n.Name,
				File: n.File,
				Type: string(n.Type),
			})
		}

		// High risk: called frequently but never covered by tests.
		// Threshold ≥3 callers avoids noise from low-use utilities.
		// Risk score = fanin × recency_multiplier: recently-changed files are
		// 10× more urgent because the agent is actively working in them.
		if fanin >= 3 && !tested {
			recently := recentFiles[n.File]
			mult := 1
			if recently {
				mult = 10
			}
			highRisk = append(highRisk, riskEntry{
				Name:            n.Name,
				File:            n.File,
				Type:            string(n.Type),
				Fanin:           fanin,
				RecentlyChanged: recently,
				Note:            "frequently called — no test coverage",
				score:           fanin * mult,
			})
		}
	}

	// Stable, deterministic sort:
	// - Dead code: by name (no recency signal — dead code has no recent callers).
	// - High-risk: by score desc (fanin × recency_multiplier), then name asc.
	sort.Slice(deadCode, func(i, j int) bool { return deadCode[i].Name < deadCode[j].Name })
	sort.Slice(highRisk, func(i, j int) bool {
		if highRisk[i].score != highRisk[j].score {
			return highRisk[i].score > highRisk[j].score
		}
		return highRisk[i].Name < highRisk[j].Name
	})

	// Cap results to avoid token bloat.
	const maxDead = 10
	const maxRisk = 5
	const maxViolations = 5
	totalDead := len(deadCode)
	totalRisk := len(highRisk)
	if len(deadCode) > maxDead {
		deadCode = deadCode[:maxDead]
	}
	if len(highRisk) > maxRisk {
		highRisk = highRisk[:maxRisk]
	}

	// ── "What Synapses knows" (V2-C2) ────────────────────────────────────
	// Compute codebase_knowledge before deciding whether to return nil.
	// Always present on first session — makes the value of indexing concrete
	// even for clean codebases with no dead code or violations.
	var codebaseKnowledge map[string]interface{}
	{
		// Most-connected entity: highest call fanin.
		var topName, topFile string
		var topFanin int
		for id, n := range nodes {
			if fi := callFanin[id]; fi > topFanin {
				topFanin = fi
				topName = n.Name
				topFile = n.File
			}
		}

		// Deepest call chain: iterative DP via topological sort (Kahn's algorithm).
		// Avoids recursive DFS stack overflow on large repos with deep chains.
		// Considers only CALLS edges; capped at 20 000 nodes to stay O(N).
		deepest := 0
		if len(outEdges) > 0 {
			const maxNodes = 20_000
			// Build in-degree count and adjacency restricted to CALLS.
			inDeg := make(map[graph.NodeID]int, len(nodes))
			callsAdj := make(map[graph.NodeID][]graph.NodeID, len(nodes))
			nodeCount := 0
			for id := range nodes {
				if nodeCount >= maxNodes {
					break
				}
				nodeCount++
				_ = inDeg[id] // ensure entry exists (zero-value initialization)
				for _, e := range outEdges[id] {
					if e.Type == graph.EdgeCalls {
						callsAdj[id] = append(callsAdj[id], e.To)
						inDeg[e.To]++ // e.To may not be in nodes if graph is inconsistent
					}
				}
			}
			// Kahn's BFS: process zero-in-degree nodes first.
			dist := make(map[graph.NodeID]int, len(inDeg))
			queue := make([]graph.NodeID, 0, len(inDeg))
			for id := range inDeg {
				if inDeg[id] == 0 {
					queue = append(queue, id)
					dist[id] = 1
				}
			}
			for len(queue) > 0 {
				cur := queue[0]
				queue = queue[1:]
				if dist[cur] > deepest {
					deepest = dist[cur]
				}
				for _, next := range callsAdj[cur] {
					if dist[next] < dist[cur]+1 {
						dist[next] = dist[cur] + 1
					}
					inDeg[next]--
					if inDeg[next] == 0 {
						queue = append(queue, next)
					}
				}
			}
		}

		if compact {
			ck := map[string]interface{}{"total_entities": len(nodes)}
			if deepest > 1 {
				ck["deepest_call_chain"] = deepest
			}
			if topFanin > 0 {
				ck["most_connected_entity"] = topName
			}
			codebaseKnowledge = ck
		} else {
			ck := map[string]interface{}{
				"total_entities": len(nodes),
				"hint":           "All entities are accessible via get_context, search, get_impact, and recall.",
			}
			if topFanin > 0 {
				ck["most_connected_entity"] = map[string]interface{}{
					"name":       topName,
					"file":       topFile,
					"call_fanin": topFanin,
					"note":       "Most-called entity — changes here have the widest blast radius.",
				}
			}
			if deepest > 1 {
				ck["deepest_call_chain"] = deepest
			}
			codebaseKnowledge = ck
		}
	}

	// Return codebase_knowledge-only for clean codebases (no dead code / risk / violations).
	// This ensures the first-session "aha" moment fires even when the codebase is clean.
	if totalDead == 0 && totalRisk == 0 && len(vlog) == 0 {
		return map[string]interface{}{
			"codebase_knowledge": codebaseKnowledge,
		}
	}

	hint := "First session detected — Synapses scanned your codebase and found these patterns. " +
		"Review before starting work. Re-run get_violations() for full architectural detail."

	// compact mode (quickMode callers): counts + hint only — no sample arrays.
	// Keeps token cost to ~20 tokens while ensuring the signal is never silently lost.
	if compact {
		out := map[string]interface{}{
			"hint":               hint + " Call scope=\"full\" for entity samples.",
			"codebase_knowledge": codebaseKnowledge,
		}
		if totalDead > 0 {
			out["dead_code_count"] = totalDead
		}
		if totalRisk > 0 {
			out["high_risk_count"] = totalRisk
		}
		if len(vlog) > 0 {
			out["violation_count"] = len(vlog)
		}
		return out
	}

	// Full mode: include sample arrays for each finding category.
	out := map[string]interface{}{
		"hint":               hint,
		"codebase_knowledge": codebaseKnowledge,
	}
	if totalDead > 0 {
		out["dead_code"] = map[string]interface{}{
			"total":  totalDead,
			"sample": deadCode,
			"note":   "Functions/methods with no callers and no test coverage — likely dead code or untested paths.",
		}
	}
	if totalRisk > 0 {
		out["high_risk_entities"] = map[string]interface{}{
			"total":  totalRisk,
			"sample": highRisk,
			"note":   "Frequently called code with no test coverage — high blast radius if these functions fail.",
		}
	}
	if len(vlog) > 0 {
		sample := vlog
		if len(sample) > maxViolations {
			sample = sample[:maxViolations]
		}
		out["architectural_violations"] = map[string]interface{}{
			"total":  len(vlog),
			"sample": sample,
			"note":   "Active architectural rule violations. Run get_violations() for full list and remediation hints.",
		}
	}
	return out
}

// handleGetImpact performs reverse-BFS blast-radius analysis from a named entity.
// Returns nodes grouped by depth tier: direct (depth 1, confidence 1.0),
