// Package archivist — deterministic extraction path (Sprint 30.2).
//
// DeterministicArchivist extracts session learnings from pre-fetched store data
// without any LLM call. Zero hallucination, zero latency.
//
// The caller (handleEndSession) fetches the raw data from the store and passes it
// in a DeterministicRequest. This keeps the archivist package free of store
// imports and makes the extraction trivially testable.
package archivist

import (
	"fmt"
	"strings"
	"unicode"
)

// ExplorationEntry is a single tool call entry fetched from the store's
// exploration_log table. Mirrors store.ExplorationEntry without the import.
type ExplorationEntry struct {
	ToolName       string
	EntityQueried  string
	FindingSummary string
}

// FailedApproach is a rejected approach fetched from the store's
// rejected_approaches table. Mirrors store.RejectedApproach without the import.
type FailedApproach struct {
	Approach      string
	FailureReason string
	Blocker       string
}

// DeterministicRequest carries all pre-fetched session data for extraction.
// The caller populates every slice from store queries; empty slices are valid.
type DeterministicRequest struct {
	// SessionID is informational only (used for debugging, not extraction logic).
	SessionID string
	// AgentID is informational only.
	AgentID string
	// ExplorationEntries are the exploration_log rows for this session,
	// ordered most-recent first (as returned by GetSessionExplorationLog).
	ExplorationEntries []ExplorationEntry
	// FailedApproaches are the rejected_approaches rows created during this
	// session window, ordered most-recent first.
	FailedApproaches []FailedApproach
}

// maxDeterministicMemories is the cap on memories produced in a single Extract
// call. Prioritised: manual decisions > failed approaches > validate > get_context/get_impact > search.
const maxDeterministicMemories = 30

// DeterministicArchivist extracts session memories without LLM.
// All outputs are derived directly from the input data — no inference.
type DeterministicArchivist struct{}

// NewDeterministic returns a zero-alloc DeterministicArchivist.
func NewDeterministic() *DeterministicArchivist { return &DeterministicArchivist{} }

// Extract produces a MemorizeResponse from the pre-fetched session data.
// It is safe to call with a zero-value DeterministicRequest (returns empty response).
// The function is deterministic: same input always produces same output.
func (da *DeterministicArchivist) Extract(req DeterministicRequest) MemorizeResponse {
	// Buckets by priority (highest first: manual > failed > validate > context/impact > search).
	// We gather all candidates, deduplicate by key, then enforce the cap in priority order.
	type candidate struct {
		priority int // lower = higher priority
		mem      Memory
		ann      *Annotation // optional, for get_context entries
	}
	var candidates []candidate

	seenKeys := make(map[string]struct{})

	add := func(priority int, mem Memory, ann *Annotation) {
		if mem.Key == "" || mem.Content == "" {
			return
		}
		if _, dup := seenKeys[mem.Key]; dup {
			return
		}
		seenKeys[mem.Key] = struct{}{}
		candidates = append(candidates, candidate{priority: priority, mem: mem, ann: ann})
	}

	// ── Bucket 1: manual decisions (memory saves, priority 0) ──────────────────
	// Exploration entries with tool_name="memory" and query_context="save"
	// represent explicit agent decisions. Pass them through directly.
	for _, e := range req.ExplorationEntries {
		if e.ToolName != "memory" || e.FindingSummary == "" {
			continue
		}
		key := snakeKey(e.EntityQueried, "decision")
		add(0, Memory{
			Key:     key,
			Content: e.FindingSummary,
		}, nil)
	}

	// ── Bucket 2: failed approaches (priority 1) ────────────────────────────────
	for _, fa := range req.FailedApproaches {
		if fa.Approach == "" || fa.FailureReason == "" {
			continue
		}
		content := fmt.Sprintf("Tried: %s. Failed: %s", truncate(fa.Approach, 120), truncate(fa.FailureReason, 120))
		if fa.Blocker != "" {
			content += fmt.Sprintf(". Blocker: %s", truncate(fa.Blocker, 80))
		}
		key := snakeKey(fa.Approach, "failed")
		add(1, Memory{
			Key:     key,
			Content: content,
		}, nil)
	}

	// ── Bucket 3: validate findings (priority 2) ────────────────────────────────
	for _, e := range req.ExplorationEntries {
		if e.ToolName != "validate" || e.FindingSummary == "" {
			continue
		}
		entity := e.EntityQueried
		if entity == "" {
			entity = "files"
		}
		key := snakeKey("validated_"+entity, "validate")
		add(2, Memory{
			Key:     key,
			Content: fmt.Sprintf("Validation of %s: %s", truncate(entity, 60), e.FindingSummary),
		}, nil)
	}

	// ── Bucket 4: get_context and get_impact findings (priority 3) ─────────────
	for _, e := range req.ExplorationEntries {
		if (e.ToolName != "get_context" && e.ToolName != "get_impact") || e.FindingSummary == "" {
			continue
		}
		if e.EntityQueried == "" {
			continue
		}
		key := snakeKey(e.EntityQueried, "entity")
		mem := Memory{
			Key:      key,
			Content:  e.FindingSummary,
			Entities: []string{e.EntityQueried},
		}
		ann := &Annotation{
			Node: e.EntityQueried,
			Note: truncate(e.FindingSummary, 200),
		}
		add(3, mem, ann)
	}

	// ── Bucket 5: search findings (priority 4) ──────────────────────────────────
	for _, e := range req.ExplorationEntries {
		if e.ToolName != "search" || e.FindingSummary == "" {
			continue
		}
		if e.EntityQueried == "" {
			continue
		}
		key := snakeKey("search_"+e.EntityQueried, "search")
		add(4, Memory{
			Key:     key,
			Content: fmt.Sprintf("Searched for %q: %s", truncate(e.EntityQueried, 60), e.FindingSummary),
		}, nil)
	}

	// ── Enforce cap in priority order ──────────────────────────────────────────
	// candidates is in insertion order (bucket 0 first, then 1, 2, 3, 4).
	// Within each bucket, GetSessionExplorationLog returns newest-first, so the
	// first occurrence of a key is the newest entry (seenKeys dedup ensures only
	// the newest survives). Cap by truncating lowest-priority (search) entries.
	if len(candidates) > maxDeterministicMemories {
		candidates = candidates[:maxDeterministicMemories]
	}

	resp := MemorizeResponse{
		NewMemories: make([]Memory, 0, len(candidates)),
		Annotations: make([]Annotation, 0),
	}
	for _, c := range candidates {
		resp.NewMemories = append(resp.NewMemories, c.mem)
		if c.ann != nil {
			resp.Annotations = append(resp.Annotations, *c.ann)
		}
	}
	return resp
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// snakeKey converts an arbitrary string to a compact snake_case key suitable
// for use as a Memory.Key. Prefix is prepended when the result would be empty.
// Maximum output length is 60 characters (keys are lookup identifiers, not content).
func snakeKey(s, prefix string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return prefix
	}
	// Build snake_case: keep alphanumeric, replace runs of non-alnum with "_".
	var b strings.Builder
	prevWasUnderscore := false
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
			prevWasUnderscore = false
		} else if !prevWasUnderscore && b.Len() > 0 {
			b.WriteRune('_')
			prevWasUnderscore = true
		}
	}
	result := strings.Trim(b.String(), "_")
	if result == "" {
		return prefix
	}
	if len([]rune(result)) > 60 {
		runes := []rune(result)
		result = string(runes[:60])
		result = strings.TrimRight(result, "_")
	}
	return result
}

// truncate shortens s to at most maxLen characters (byte-safe: operates on runes).
func truncate(s string, maxLen int) string {
	if maxLen <= 0 || s == "" {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen-1]) + "…"
}
