// Package brain provides in-process access to the Thinking Brain.
// Previously a separate HTTP sidecar (synapses-intelligence), the brain is now
// embedded directly so no external process or port is required.
//
// All public methods are fail-silent: errors are silently discarded so that
// brain failures never degrade the MCP hot path. The graph-only path always works.
package brain

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/SynapsesOS/synapses/internal/brain/archivist"
	brainconfig "github.com/SynapsesOS/synapses/internal/brain/config"
	"github.com/SynapsesOS/synapses/internal/brain/llm"
)

// Client wraps the in-process Brain implementation. It exposes the same method
// signatures as the former HTTP client so all callers compile without changes.
// Create with NewInProcess; always non-nil (uses NullBrain on failure).
//
// Background scheduling: when the brain is enabled, Client creates a SystemPulse,
// a ModelManager, and a Scheduler. Low-priority background tasks (Ingest) are
// submitted to the Scheduler as P2 tasks and executed by the drain goroutine only
// when system health is Green AND the ModelManager confirms a model can be loaded.
// High-priority P0 tasks (BuildContextPacket, ExplainViolation) check
// ShouldDegrade() before invoking the LLM to fast-fail under resource pressure.
type Client struct {
	brain      Brain
	scheduler  *Scheduler
	pulse      *SystemPulse  // owned by Client; nil when brain is disabled
	modelMgr   *ModelManager // owned by Client; nil when brain is disabled
	ollamaBase string        // Ollama base URL; empty when Ollama not configured
}

// NewInProcess creates a Client backed by an in-process Brain. If cfg is nil or
// cfg.Enabled is false, returns a Client wrapping NullBrain (all methods return
// zero values). Never returns nil.
//
// When enabled, NewInProcess starts a SystemPulse (health monitor) and a Scheduler
// (priority task queue). Both are stopped when Close() is called.
func NewInProcess(cfg *brainconfig.BrainConfig) *Client {
	if cfg == nil || !cfg.Enabled {
		// NullBrain path: scheduler with nil pulse runs tasks immediately (no-op).
		return &Client{
			brain:     &NullBrain{},
			scheduler: NewScheduler(nil),
		}
	}

	// Start system health monitoring so the scheduler can make health-aware
	// decisions about when to run P1/P2 tasks. Point the pulse at the same
	// Ollama instance that ModelManager and OllamaClients use so that
	// OllamaModelLoaded correctly reflects model residency even when
	// OllamaURL is set to a non-default host or port.
	ollamaBase := strings.TrimRight(cfg.OllamaURL, "/")
	pulse := NewSystemPulse().WithOllamaURL(ollamaBase + "/api/ps")
	pulse.Start()

	// ModelManager uses the same pulse for RAM checks and pre-loads the model
	// before each drain cycle according to the configured intelligence mode.
	mgr := NewModelManager(pulse, *cfg)

	sched := NewScheduler(pulse).WithModelManager(mgr)
	sched.Start()

	return &Client{
		brain:      New(*cfg),
		scheduler:  sched,
		pulse:      pulse,
		modelMgr:   mgr,
		ollamaBase: ollamaBase,
	}
}

// NewClient is a backward-compatible constructor kept for callers that still use
// NewClient(url, timeout). It now ignores both arguments and returns a NullBrain
// client. Callers should migrate to NewInProcess(cfg).
//
// Deprecated: use NewInProcess.
func NewClient(_ string, _ int) *Client {
	return &Client{
		brain:     &NullBrain{},
		scheduler: NewScheduler(nil),
	}
}

// ListInstalledModels returns the names of all Ollama models installed locally.
// Returns nil when Ollama is not configured or the query fails.
func (c *Client) ListInstalledModels(ctx context.Context) []string {
	if c.ollamaBase == "" {
		return nil
	}
	models, err := llm.ListInstalledModels(ctx, c.ollamaBase)
	if err != nil {
		return nil
	}
	return models
}

// SystemUnderRAMPressure returns true when the system health is Yellow or Red —
// indicating that available RAM is below 3 GB (Yellow) or 1.5 GB (Red).
// The embedding background pass should be deferred when this returns true to
// avoid OOM during concurrent indexing + embedding on memory-constrained machines.
//
// Returns false when the pulse is nil (NullBrain / brain disabled) so that
// the embed pass always runs when the brain is not configured.
func (c *Client) SystemUnderRAMPressure() bool {
	if c.pulse == nil {
		return false
	}
	return c.pulse.Current().Health != HealthGreen
}

// HealthCheck returns ("ok", nil) when the brain is available, or an error when not.
func (c *Client) HealthCheck(_ context.Context) (string, error) {
	if c.brain.Available() {
		return c.brain.ModelName(), nil
	}
	return "", nil // NullBrain — brain disabled, not an error
}

// BuildContextPacket builds and returns an enriched context packet.
//
// Returns nil when:
//   - The brain is unavailable or returns an error.
//   - System health is Red, or Yellow with no model loaded (ShouldDegrade).
//     Callers fall back to raw Synapses context unchanged.
func (c *Client) BuildContextPacket(ctx context.Context, req ContextPacketRequest) *ContextPacket {
	// P0 degradation check: skip the LLM call if system is under memory pressure
	// and no model is already loaded. Returning nil is the documented fallback.
	if c.scheduler.ShouldDegrade() {
		return nil
	}
	pkt, err := c.brain.BuildContextPacket(ctx, req)
	if err != nil {
		return nil
	}
	return pkt
}

// Ingest submits a code node for semantic summarization.
//
// The request is enqueued as a P2 (IDLE priority) task via the Scheduler and
// executed by the background drain goroutine when system health is Green.
// Under Yellow or Red health, the task is deferred up to 15 minutes.
//
// The caller's ctx is intentionally not forwarded to the queued fn — the context
// may expire before the task is eligible to run. The queued fn uses a fresh
// background context so the LLM call succeeds when the drain goroutine fires.
func (c *Client) Ingest(_ context.Context, req IngestRequest) {
	// Build a stable dedup key: projectID + nodeID + task type.
	key := req.ProjectID + ":" + req.NodeID + ":ingest"
	c.scheduler.Submit(key, PriorityP2, func() {
		_, _ = c.brain.Ingest(context.Background(), req)
	})
}

// ExplainViolation returns (explanation, fix) for an architecture violation.
//
// Returns ("", "") when:
//   - The brain is unavailable.
//   - System health warrants degradation (ShouldDegrade returns true).
func (c *Client) ExplainViolation(ctx context.Context, req ViolationRequest) (string, string) {
	// P0 degradation check: the caller (validate_plan handler) has a fallback
	// rule-template message when explanation is empty.
	if c.scheduler.ShouldDegrade() {
		return "", ""
	}
	resp, err := c.brain.ExplainViolation(ctx, req)
	if err != nil {
		return "", ""
	}
	return resp.Explanation, resp.Fix
}

// GetSummary returns the cached summary for nodeID, or "" if not yet summarized.
func (c *Client) GetSummary(_ context.Context, nodeID string) string {
	return c.brain.Summary("", nodeID)
}

// Summary returns the brain-generated summary for a node, scoped by projectID.
// Implements the federation.BrainSummaryProvider interface.
func (c *Client) Summary(projectID, nodeID string) string {
	return c.brain.Summary(projectID, nodeID)
}

// Available reports whether the brain LLM backend is accessible.
// Implements the federation.BrainSummaryProvider interface.
func (c *Client) Available() bool {
	return c.brain.Available()
}

// Generate sends a prompt to the brain's LLM and returns the raw response.
// Returns ("", nil) if brain is unavailable or system health warrants degradation.
// Used for brain-enhanced drift summaries in the federation resolver.
func (c *Client) Generate(ctx context.Context, prompt string) (string, error) {
	if c.scheduler.ShouldDegrade() {
		return "", nil
	}
	return c.brain.Generate(ctx, prompt)
}

// hydeTimeout is the hard deadline for HyDE hypothesis generation. P0 (user-waiting)
// tasks must not block the search response for more than this duration.
const hydeTimeout = 500 * time.Millisecond

// GenerateHypothetical generates a hypothetical code entity definition for
// HyDE-enhanced semantic search (Hypothetical Document Embeddings).
//
// The hypothesis is a realistic function signature or type definition that the
// LLM imagines would match the user's query. Embedding the hypothesis instead of
// the raw query bridges the vocabulary gap between natural-language queries and
// code entity names in the HNSW index.
//
// Returns "" when:
//   - Brain is unavailable (NullBrain / brain disabled).
//   - System health warrants degradation (ShouldDegrade returns true).
//   - The 500 ms P0 timeout fires (LLM too slow — caller falls back to raw query).
//   - The LLM returns an empty response.
//
// The caller should embed the returned hypothesis and, on empty return, fall back
// to embedding the original query unchanged.
// hydeMaxQueryRunes caps the query length used in the HyDE prompt.
// Typical semantic queries are 5-20 words; 150 runes keeps the total prompt
// well under the "~100 token" budget specified in the design doc.
const hydeMaxQueryRunes = 150

func (c *Client) GenerateHypothetical(ctx context.Context, query string) string {
	if c.scheduler.ShouldDegrade() {
		return ""
	}
	hydeCtx, cancel := context.WithTimeout(ctx, hydeTimeout)
	defer cancel()

	// Truncate long queries to keep the prompt compact. Long queries (e.g. a full
	// sentence or pasted code) could push the LLM past its context budget.
	q := query
	if rr := []rune(q); len(rr) > hydeMaxQueryRunes {
		q = string(rr[:hydeMaxQueryRunes])
	}

	// Use fmt.Sprintf %q to safely quote the query. This escapes any embedded
	// double-quotes so they cannot accidentally terminate the delimited region
	// and confuse the LLM about where the query ends.
	prompt := fmt.Sprintf(
		"Write a realistic function signature or type definition for code that answers: %q. Output only the code definition, no explanation.",
		q,
	)
	hyp, err := c.brain.Generate(hydeCtx, prompt)
	if err != nil || hyp == "" {
		return ""
	}
	return strings.TrimSpace(hyp)
}

// LogDecision records a reasoning decision. Fire-and-forget.
func (c *Client) LogDecision(ctx context.Context, req DecisionRequest) {
	_ = c.brain.LogDecision(ctx, req)
}

// SetPhase updates the active SDLC phase. Returns the updated SDLCConfig.
func (c *Client) SetPhase(_ context.Context, req SetPhaseRequest) (*SDLCConfig, error) {
	if err := c.brain.SetSDLCPhase(SDLCPhase(req.Phase), ""); err != nil {
		return nil, err
	}
	cfg := c.brain.GetSDLCConfig()
	return &cfg, nil
}

// Prune uses the Tier 0 LLM to extract core technical content from raw text
// (e.g. web pages, over-budget context packets), discarding boilerplate.
// Returns the original content unchanged if brain unavailable.
func (c *Client) Prune(ctx context.Context, content string) (string, error) {
	return c.brain.Prune(ctx, content)
}

// Memorize synthesizes a session transcript into persistent memory entries.
// Returns empty response (no error) when the Archivist LLM is unavailable.
func (c *Client) Memorize(ctx context.Context, req archivist.MemorizeRequest) (archivist.MemorizeResponse, error) {
	return c.brain.Memorize(ctx, req)
}

// SetQualityMode updates the active quality mode. Returns the updated SDLCConfig.
func (c *Client) SetQualityMode(_ context.Context, mode QualityMode) (*SDLCConfig, error) {
	if err := c.brain.SetQualityMode(mode, ""); err != nil {
		return nil, err
	}
	cfg := c.brain.GetSDLCConfig()
	return &cfg, nil
}

// UpsertADR creates or updates an ADR. Returns the stored ADR.
func (c *Client) UpsertADR(_ context.Context, req ADRRequest) (*ADR, error) {
	if err := c.brain.UpsertADR(req); err != nil {
		return nil, err
	}
	adr, err := c.brain.GetADR(req.ID)
	if err != nil {
		return nil, err
	}
	return &adr, nil
}

// GetADR retrieves an ADR by ID.
func (c *Client) GetADR(_ context.Context, id string) (*ADR, error) {
	adr, err := c.brain.GetADR(id)
	if err != nil {
		return nil, err
	}
	return &adr, nil
}

// GetADRs returns all ADRs, optionally filtered by file path.
func (c *Client) GetADRs(_ context.Context, fileFilter string) ([]ADR, error) {
	if fileFilter != "" {
		return c.brain.GetADRsForFile(fileFilter, 50)
	}
	return c.brain.AllADRs()
}

// QueryDecisions returns up to limit decision log entries, optionally filtered
// by entityName (empty string = all), ordered by created_at DESC.
func (c *Client) QueryDecisions(ctx context.Context, entityName string, limit int) ([]DecisionLogEntry, error) {
	return c.brain.QueryDecisions(ctx, entityName, limit)
}

// BrainHealth returns structured per-tier health data for session_init.
// Returns nil if the underlying Brain does not implement BrainStatsProvider
// (e.g. NullBrain when brain is disabled).
func (c *Client) BrainHealth() map[string]interface{} {
	sp, ok := c.brain.(BrainStatsProvider)
	if !ok {
		return nil
	}
	stats := sp.BrainStats()

	// Gather circuit breaker state (optional — NullBrain won't have it).
	var tierStatus map[string]TierState
	if tp, ok := c.brain.(TierStatusProvider); ok {
		tierStatus = tp.TierStatus()
	}

	tiers := []string{"ingest", "enrich", "guardian", "orchestrate", "archivist", "context_builder"}
	tierMap := make(map[string]interface{}, len(tiers))

	for _, tier := range tiers {
		callsKey := tier + "_calls"
		successKey := tier + "_success"
		avgKey := tier + "_avg_ms"

		calls, _ := stats[callsKey].(int64)
		success, _ := stats[successKey].(int64)
		avgMS, _ := stats[avgKey].(int64)

		var successRate float64
		if calls > 0 {
			successRate = float64(success) / float64(calls)
		}

		circuit := "closed"
		if ts, ok := tierStatus[tier]; ok && ts.Open {
			circuit = "open"
		}

		tierMap[tier] = map[string]interface{}{
			"calls":        calls,
			"success_rate": successRate,
			"avg_ms":       avgMS,
			"circuit":      circuit,
		}
	}

	return map[string]interface{}{
		"model": c.brain.ModelName(),
		"tiers": tierMap,
	}
}

// ── NL-to-graph Tier 2 (Sprint 17 #5) ───────────────────────────────────────

// NLCandidate is a single entity candidate for Tier 2 LLM classification.
// It carries the candidate name and its surrounding context sentence so the
// LLM can infer entity type and relationship type from minimal context.
type NLCandidate struct {
	// Name is the normalised candidate name (lowercase, trimmed).
	Name string
	// Context is up to 200 chars of surrounding text for classification.
	Context string
	// NodeID is the existing graph NodeID of the knowledge node to update.
	// Empty means no node was created in Tier 0/1 (should not happen in practice).
	NodeID string
}

// NLClassifyResult is the LLM's classification of a single NLCandidate.
type NLClassifyResult struct {
	// NodeID matches the input NLCandidate.NodeID.
	NodeID string
	// NodeType is one of: concept | entity | artifact | decision.
	// Empty means the LLM returned an unrecognised value; caller keeps the default.
	NodeType string
	// Description is a one-sentence LLM-generated summary of the entity.
	// Empty when the LLM is unavailable or returns an invalid response.
	Description string
}

// NLClassifyRequest bundles the file path and candidates for a single
// ScheduleNLClassification call. Used as a P1 scheduler task key.
type NLClassifyRequest struct {
	// FilePath is the source markdown file, used as the scheduler dedup key.
	FilePath string
	// Candidates are the unresolved entity candidates from Tier 0/1 extraction.
	Candidates []NLCandidate
}

// ScheduleNLClassification enqueues a P1 brain task that classifies the
// entity type of each NLCandidate using the LLM and calls applyFn with the
// results. applyFn is called from within the scheduler goroutine; it must be
// safe to call concurrently with graph reads but not concurrent graph writes
// (the watcher reparseMu serialises graph mutations).
//
// No-op when:
//   - The candidate list is empty.
//   - The brain is unavailable (NullBrain).
//   - System health warrants P1 deferral (task is queued; applyFn runs later).
//
// The caller's context is NOT forwarded to the queued task — it may expire
// before the P1 scheduler fires. A fresh background context is used instead.
func (c *Client) ScheduleNLClassification(req NLClassifyRequest, applyFn func([]NLClassifyResult)) {
	if len(req.Candidates) == 0 || applyFn == nil {
		return
	}
	key := "nl-classify:" + req.FilePath
	// Capture copies for the closure — req is value, applyFn is a func pointer.
	candidates := req.Candidates
	c.scheduler.Submit(key, PriorityP1, func() {
		results := classifyNLCandidates(c.brain, candidates)
		if len(results) > 0 {
			applyFn(results)
		}
	})
}

// classifyNLCandidates sends classification + description prompts per candidate
// to the LLM. Each prompt is small (~50-80 tokens input, ~1-10 tokens output)
// so the full batch for a typical document (20 candidates) completes in
// ~15-30 seconds on a local 4B model.
//
// Two-pass strategy within a single model residency window:
//  1. Entity type classification: concept | entity | artifact | decision
//  2. One-sentence description generation (skipped if classification fails)
//
// Valid NodeType responses: concept | entity | artifact | decision
// Anything else is silently ignored (caller keeps the Tier 0 default "concept").
func classifyNLCandidates(b Brain, candidates []NLCandidate) []NLClassifyResult {
	if !b.Available() {
		return nil
	}
	ctx := context.Background()
	var results []NLClassifyResult
	for _, c := range candidates {
		if c.NodeID == "" || c.Name == "" {
			continue
		}
		// Pass 0: Binary relevance filter for low-confidence candidates.
		// Skip LLM call for high-confidence candidates (backticks, frontmatter).
		if c.Context != "frontmatter tag" && c.Context != "frontmatter category" {
			relPrompt := buildRelevancePrompt(c.Name, c.Context)
			relResp, relErr := b.Generate(ctx, relPrompt)
			if relErr == nil && relResp != "" && !parseRelevanceResponse(relResp) {
				continue // LLM says not a meaningful concept — skip
			}
		}

		// Pass 1: Entity type classification.
		prompt := buildEntityTypePrompt(c.Name, c.Context)
		resp, err := b.Generate(ctx, prompt)
		if err != nil || resp == "" {
			continue
		}
		nodeType := parseEntityTypeResponse(resp)
		if nodeType == "" {
			continue
		}

		result := NLClassifyResult{
			NodeID:   c.NodeID,
			NodeType: nodeType,
		}

		// Pass 2: One-sentence description.
		descPrompt := buildDescriptionPrompt(c.Name, c.Context)
		descResp, descErr := b.Generate(ctx, descPrompt)
		if descErr == nil && descResp != "" {
			result.Description = parseDescriptionResponse(descResp)
		}

		results = append(results, result)
	}
	return results
}

// buildRelevancePrompt returns a binary yes/no prompt for entity relevance filtering.
// Used to filter low-confidence candidates before spending tokens on full classification.
func buildRelevancePrompt(name, context string) string {
	return "Is \"" + name + "\" a specific technical concept, named entity, or design decision? Answer yes or no only."
}

// parseRelevanceResponse extracts a yes/no answer from the LLM's response.
// Returns true for "yes" (relevant), false for "no" (not relevant).
// Defaults to true (include) when the response is ambiguous.
func parseRelevanceResponse(resp string) bool {
	r := strings.ToLower(strings.TrimSpace(resp))
	if fields := strings.Fields(r); len(fields) > 0 {
		r = fields[0]
	}
	r = strings.Trim(r, ".,;:!?\"'")
	return r != "no"
}

// buildEntityTypePrompt returns a minimal classification prompt for the LLM.
// Prompt is intentionally tiny: one-word output keeps latency low and
// format-compliance high (IFEval 89.8 on qwen3.5:4b is enough for one word).
func buildEntityTypePrompt(name, context string) string {
	ctx := strings.TrimSpace(context)
	if len(ctx) > 150 {
		// Truncate at word boundary to avoid splitting multi-byte UTF-8 runes.
		cut := ctx[:150]
		if idx := strings.LastIndexByte(cut, ' '); idx > 0 {
			ctx = cut[:idx]
		} else {
			ctx = cut
		}
	}
	prompt := "Entity: \"" + name + "\"\n"
	if ctx != "" {
		prompt += "Context: \"" + ctx + "\"\n"
	}
	prompt += "Type (one word only — concept / entity / artifact / decision):"
	return prompt
}

// buildDescriptionPrompt returns a minimal one-sentence description prompt.
// Output should be a single sentence (max ~30 words). Designed for low-latency
// classification within the same model residency window as entity type prompts.
func buildDescriptionPrompt(name, context string) string {
	ctx := strings.TrimSpace(context)
	if len(ctx) > 150 {
		cut := ctx[:150]
		if idx := strings.LastIndexByte(cut, ' '); idx > 0 {
			ctx = cut[:idx]
		} else {
			ctx = cut
		}
	}
	prompt := "Describe \"" + name + "\" in one sentence"
	if ctx != "" {
		prompt += " based on: \"" + ctx + "\""
	}
	prompt += ".\nDescription:"
	return prompt
}

// parseDescriptionResponse cleans the LLM's description response.
// Returns a trimmed single sentence, or "" if the response is too long or empty.
func parseDescriptionResponse(resp string) string {
	s := strings.TrimSpace(resp)
	// Take only the first line/sentence — LLM might explain further.
	if idx := strings.IndexByte(s, '\n'); idx > 0 {
		s = s[:idx]
	}
	s = strings.TrimSpace(s)
	// Guard against excessively long descriptions (LLM rambling).
	if len(s) > 200 {
		if idx := strings.LastIndexByte(s[:200], '.'); idx > 0 {
			s = s[:idx+1]
		} else {
			s = s[:200]
		}
	}
	return s
}

// parseEntityTypeResponse extracts a valid node type from the LLM's one-word response.
// Returns "" if the response is not one of the four valid types.
func parseEntityTypeResponse(resp string) string {
	// Normalise: lowercase, trim punctuation, then split on any whitespace
	// (space, tab, newline) so "concept\nSome explanation" → "concept".
	r := strings.ToLower(strings.Trim(strings.TrimSpace(resp), ".,;:!?\"'"))
	// Fields splits on any whitespace — handles newlines that IndexByte(' ') misses.
	if fields := strings.Fields(r); len(fields) > 0 {
		r = fields[0]
	}
	switch r {
	case "concept", "entity", "artifact", "decision":
		return r
	default:
		return ""
	}
}

// Close shuts down the in-process brain, scheduler, and system pulse,
// releasing all associated resources.
func (c *Client) Close() {
	// Stop the scheduler first so no new tasks are dispatched after brain close.
	if c.scheduler != nil {
		c.scheduler.Stop()
	}
	// Stop the system pulse sampler.
	if c.pulse != nil {
		c.pulse.Stop()
	}
	// Close the brain (releases LLM client, SQLite store).
	if closer, ok := c.brain.(io.Closer); ok {
		_ = closer.Close()
	}
}
