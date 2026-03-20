package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// ── Sprint 9 #8: Remember → Recall Integration Tests ─────────────────────────
//
// End-to-end round-trip through real Server + real Store:
//   remember(decision=..., anchor_nodes=[...]) → recall(query=...) → verify fields
// Covers FTS indexing, dual-write to memories table, anchor persistence, browse mode,
// field correctness, content concatenation, and summary/metadata on recall responses.

func TestRememberRecall_RoundTrip_FTSSearch(t *testing.T) {
	s := newTestServer(t)

	// ── remember ──
	remRes, err := s.handleRemember(ctx, callTool(map[string]any{
		"agent_id":     "integration-agent",
		"decision":     "auth switched to OAuth for all user-facing endpoints",
		"rationale":    "JWT was leaking session tokens in URL params",
		"episode_type": "decision",
		"outcome":      "success",
		"trigger":      "security audit finding SA-42",
		"tags":         `["auth","oauth","security"]`,
		"anchor_nodes": `["repo::pkg/auth.go::AuthService"]`,
	}))
	remMap := mustResult(t, remRes, err)

	episodeID, ok := remMap["episode_id"].(string)
	if !ok || episodeID == "" {
		t.Fatal("remember did not return episode_id")
	}
	if remMap["episode_type"] != "decision" {
		t.Errorf("expected episode_type=decision, got %v", remMap["episode_type"])
	}
	if remMap["outcome"] != "success" {
		t.Errorf("expected outcome=success, got %v", remMap["outcome"])
	}
	// Verify anchor count surfaced.
	if v, ok := remMap["anchored_to"].(float64); !ok || v != 1 {
		t.Errorf("expected anchored_to=1, got %v", remMap["anchored_to"])
	}

	// ── recall with query (FTS search mode) ──
	recRes, err := s.handleRecall(ctx, callTool(map[string]any{
		"query":    "OAuth auth",
		"agent_id": "integration-agent",
	}))
	recMap := mustResult(t, recRes, err)

	// Must be in search mode.
	if recMap["mode"] != "search" {
		t.Errorf("expected mode=search, got %v", recMap["mode"])
	}

	// Verify summary is non-empty and meaningful.
	summary, _ := recMap["summary"].(string)
	if summary == "" || summary == "no matching results" {
		t.Errorf("expected non-empty matching summary, got %q", summary)
	}

	// Verify hint is present (search mode has a hint).
	if recMap["hint"] == nil || recMap["hint"] == "" {
		t.Error("expected hint field in search mode response")
	}

	// Verify episodes list contains our episode with all fields.
	episodes, _ := recMap["episodes"].([]any)
	foundEpisode := false
	for _, raw := range episodes {
		ep, _ := raw.(map[string]any)
		if ep["id"] == episodeID {
			foundEpisode = true
			if ep["decision"] != "auth switched to OAuth for all user-facing endpoints" {
				t.Errorf("episode decision mismatch: %v", ep["decision"])
			}
			if ep["rationale"] != "JWT was leaking session tokens in URL params" {
				t.Errorf("episode rationale mismatch: %v", ep["rationale"])
			}
			if ep["episode_type"] != "decision" {
				t.Errorf("episode_type mismatch: %v", ep["episode_type"])
			}
			if ep["outcome"] != "success" {
				t.Errorf("outcome mismatch: %v", ep["outcome"])
			}
			if ep["trigger"] != "security audit finding SA-42" {
				t.Errorf("trigger mismatch: %v", ep["trigger"])
			}
			// Verify created_at is populated (non-zero).
			if ca, ok := ep["created_at"].(float64); !ok || ca == 0 {
				t.Errorf("episode created_at should be a non-zero timestamp, got %v", ep["created_at"])
			}
			// Verify tags round-trip.
			tagsStr, _ := ep["tags"].(string)
			if !strings.Contains(tagsStr, "auth") || !strings.Contains(tagsStr, "oauth") {
				t.Errorf("episode tags should contain 'auth' and 'oauth', got %q", tagsStr)
			}
			break
		}
	}

	// Also check the unified memories surface.
	memories, _ := recMap["memories"].([]any)

	// At least one of the two paths (episodes or memories) must find it.
	if !foundEpisode && len(memories) == 0 {
		t.Fatalf("recall(query='OAuth auth') found neither episode nor memory; episodes=%d, memories=%d",
			len(episodes), len(memories))
	}

	// Verify memory structure and content concatenation.
	for _, raw := range memories {
		mem, _ := raw.(map[string]any)
		content, _ := mem["content"].(string)
		if strings.Contains(content, "OAuth") {
			// handleRemember writes: decision + " — " + rationale
			if !strings.Contains(content, "auth switched to OAuth") {
				t.Errorf("memory content should contain the decision text, got %q", content)
			}
			if !strings.Contains(content, "JWT was leaking") {
				t.Errorf("memory content should contain the rationale text, got %q", content)
			}
			if !strings.Contains(content, " — ") {
				t.Errorf("memory content should concatenate decision and rationale with ' — ', got %q", content)
			}
			// Verify tier is "project" (all episodes get a project-tier memory).
			if mem["tier"] != "project" {
				t.Errorf("expected memory tier=project, got %v", mem["tier"])
			}
			// Verify source is "manual" (remember() writes source=manual).
			if mem["source"] != "manual" {
				t.Errorf("expected memory source=manual, got %v", mem["source"])
			}
			if mem["id"] == nil || mem["id"] == "" {
				t.Error("memory missing id")
			}
			// Verify created_at is non-empty.
			if mem["created_at"] == nil || mem["created_at"] == "" {
				t.Error("memory missing created_at")
			}
			break
		}
	}
}

func TestRememberRecall_RoundTrip_BrowseMode(t *testing.T) {
	s := newTestServer(t)

	// Store two episodes.
	for _, decision := range []string{
		"migrated database to PostgreSQL",
		"added rate limiting to API gateway",
	} {
		_, err := s.handleRemember(ctx, callTool(map[string]any{
			"agent_id": "browse-agent",
			"decision": decision,
			"outcome":  "success",
		}))
		if err != nil {
			t.Fatalf("remember failed: %v", err)
		}
	}

	// Browse mode: recall without query.
	recRes, err := s.handleRecall(ctx, callTool(map[string]any{
		"agent_id": "browse-agent",
	}))
	recMap := mustResult(t, recRes, err)

	if recMap["mode"] != "browse" {
		t.Errorf("expected mode=browse, got %v", recMap["mode"])
	}

	// Browse mode should have a hint explaining ordering.
	if recMap["hint"] == nil || recMap["hint"] == "" {
		t.Error("expected hint field in browse mode response")
	}

	episodes, _ := recMap["episodes"].([]any)
	if len(episodes) < 2 {
		t.Errorf("expected at least 2 episodes in browse, got %d", len(episodes))
	}

	// Verify all episodes have required fields populated.
	for i, raw := range episodes {
		ep, _ := raw.(map[string]any)
		if ep["id"] == nil || ep["id"] == "" {
			t.Errorf("episode[%d] missing id", i)
		}
		if ep["decision"] == nil || ep["decision"] == "" {
			t.Errorf("episode[%d] missing decision", i)
		}
		if ca, ok := ep["created_at"].(float64); !ok || ca == 0 {
			t.Errorf("episode[%d] has invalid created_at: %v", i, ep["created_at"])
		}
	}

	// summary should reflect the count.
	summary, _ := recMap["summary"].(string)
	if !strings.Contains(summary, "2") {
		t.Errorf("expected summary to reference 2 episodes, got %q", summary)
	}
}

func TestRememberRecall_RoundTrip_WithAnchorNodes(t *testing.T) {
	s := newTestServer(t)

	anchors := []string{"repo::pkg/auth.go::Login", "repo::pkg/auth.go::Logout"}
	anchorsJSON, _ := json.Marshal(anchors)

	remRes, err := s.handleRemember(ctx, callTool(map[string]any{
		"agent_id":     "anchor-int-agent",
		"decision":     "Login and Logout share a common session validator",
		"rationale":    "reduces code duplication",
		"outcome":      "success",
		"anchor_nodes": string(anchorsJSON),
	}))
	if err != nil {
		t.Fatalf("remember with anchors failed: %v", err)
	}
	remMap := mustResult(t, remRes, err)

	// Verify remember response shows anchored_to count.
	if v, ok := remMap["anchored_to"].(float64); !ok || v != 2 {
		t.Errorf("expected anchored_to=2, got %v", remMap["anchored_to"])
	}

	// Recall via FTS on the decision content.
	recRes, err := s.handleRecall(ctx, callTool(map[string]any{
		"query": "session validator",
	}))
	recMap := mustResult(t, recRes, err)

	// Verify something comes back (episode or memory).
	episodes, _ := recMap["episodes"].([]any)
	memories, _ := recMap["memories"].([]any)
	if len(episodes) == 0 && len(memories) == 0 {
		t.Fatal("recall found nothing after remember with anchor_nodes")
	}

	// Verify the memory content includes both decision and rationale.
	for _, raw := range memories {
		mem, _ := raw.(map[string]any)
		content, _ := mem["content"].(string)
		if strings.Contains(content, "session validator") {
			if !strings.Contains(content, "reduces code duplication") {
				t.Errorf("anchored memory should include rationale, got %q", content)
			}
			break
		}
	}
}

func TestRememberRecall_FailureEpisode_RecalledByType(t *testing.T) {
	s := newTestServer(t)

	// Store a failure episode.
	remRes, err := s.handleRemember(ctx, callTool(map[string]any{
		"agent_id":     "failure-agent",
		"decision":     "inlined all error handling into main",
		"rationale":    "caused 500-line function and test failures",
		"episode_type": "failure",
		"outcome":      "failure",
	}))
	remMap := mustResult(t, remRes, err)
	episodeID := remMap["episode_id"].(string)

	// Recall with query matching the failure.
	recRes, err := s.handleRecall(ctx, callTool(map[string]any{
		"query":          "error handling inline",
		"episode_type":   "failure",
		"outcome_filter": "failure",
	}))
	recMap := mustResult(t, recRes, err)

	episodes, _ := recMap["episodes"].([]any)
	found := false
	for _, raw := range episodes {
		ep, _ := raw.(map[string]any)
		if ep["id"] == episodeID {
			found = true
			if ep["episode_type"] != "failure" {
				t.Errorf("expected episode_type=failure, got %v", ep["episode_type"])
			}
			if ep["outcome"] != "failure" {
				t.Errorf("expected outcome=failure, got %v", ep["outcome"])
			}
			// Verify rationale survives round-trip on failure episodes too.
			if ep["rationale"] != "caused 500-line function and test failures" {
				t.Errorf("rationale mismatch: %v", ep["rationale"])
			}
		}
	}
	if !found && len(episodes) == 0 {
		// May be in memories instead (FTS tokenization can route differently).
		memories, _ := recMap["memories"].([]any)
		if len(memories) == 0 {
			t.Fatal("failure episode not found via recall")
		}
	}
}

func TestRememberRecall_MultipleEpisodes_AllPersisted(t *testing.T) {
	s := newTestServer(t)

	decisions := []string{
		"introduced caching layer for GraphQL resolvers",
		"switched from REST to gRPC for internal services",
		"added circuit breaker for external API calls",
	}
	ids := make([]string, 0, len(decisions))
	for _, d := range decisions {
		res, err := s.handleRemember(ctx, callTool(map[string]any{
			"agent_id": "multi-agent",
			"decision": d,
			"outcome":  "success",
		}))
		m := mustResult(t, res, err)
		ids = append(ids, m["episode_id"].(string))
	}

	// Browse all — should find all 3.
	recRes, err := s.handleRecall(ctx, callTool(map[string]any{
		"agent_id": "multi-agent",
	}))
	recMap := mustResult(t, recRes, err)

	episodes, _ := recMap["episodes"].([]any)
	if len(episodes) < 3 {
		t.Errorf("expected at least 3 episodes, got %d", len(episodes))
	}

	// Verify each specific episode ID is present.
	idSet := make(map[string]bool)
	for _, raw := range episodes {
		ep, _ := raw.(map[string]any)
		if id, ok := ep["id"].(string); ok {
			idSet[id] = true
		}
	}
	for _, id := range ids {
		if !idSet[id] {
			t.Errorf("episode %s not found in browse results", id)
		}
	}
}

func TestRememberRecall_WithoutRationale_StillRecallable(t *testing.T) {
	s := newTestServer(t)

	// Remember without rationale — common real-world usage.
	remRes, err := s.handleRemember(ctx, callTool(map[string]any{
		"agent_id": "no-rationale-agent",
		"decision": "switched logging framework to zerolog",
		"outcome":  "success",
	}))
	remMap := mustResult(t, remRes, err)
	episodeID := remMap["episode_id"].(string)

	// tier_hint should be present when no anchor_nodes provided.
	if remMap["tier_hint"] == nil || remMap["tier_hint"] == "" {
		t.Error("expected tier_hint when anchor_nodes not provided")
	}

	// Recall by query.
	recRes, err := s.handleRecall(ctx, callTool(map[string]any{
		"query": "zerolog logging",
	}))
	recMap := mustResult(t, recRes, err)

	episodes, _ := recMap["episodes"].([]any)
	memories, _ := recMap["memories"].([]any)

	// Must find via at least one channel.
	found := false
	for _, raw := range episodes {
		ep, _ := raw.(map[string]any)
		if ep["id"] == episodeID {
			found = true
			// Rationale should be empty (either "" or nil from JSON null).
			if r, _ := ep["rationale"].(string); r != "" {
				t.Errorf("expected empty rationale, got %v", ep["rationale"])
			}
			break
		}
	}
	if !found {
		// Check memories — content should be just the decision (no " — " suffix).
		foundMem := false
		for _, raw := range memories {
			mem, _ := raw.(map[string]any)
			content, _ := mem["content"].(string)
			if strings.Contains(content, "zerolog") {
				foundMem = true
				// Without rationale, content should be just the decision.
				if strings.Contains(content, " — ") {
					t.Errorf("memory content should not have ' — ' separator when no rationale, got %q", content)
				}
				break
			}
		}
		if !foundMem {
			t.Fatal("episode without rationale not found via recall")
		}
	}
}

func TestRememberRecall_IncludeStale_Flag(t *testing.T) {
	s := newTestServer(t)

	// Store an episode.
	_, err := s.handleRemember(ctx, callTool(map[string]any{
		"agent_id": "stale-agent",
		"decision": "stale test memory about redis caching",
		"outcome":  "success",
	}))
	if err != nil {
		t.Fatalf("remember failed: %v", err)
	}

	// Recall with include_stale=true — should still work without error.
	recRes, err := s.handleRecall(ctx, callTool(map[string]any{
		"query":         "redis caching",
		"include_stale": true,
	}))
	recMap := mustResult(t, recRes, err)

	// Should find results (the memory isn't actually stale, but the flag
	// must not cause errors and must still include non-stale results).
	episodes, _ := recMap["episodes"].([]any)
	memories, _ := recMap["memories"].([]any)
	if len(episodes) == 0 && len(memories) == 0 {
		t.Fatal("recall with include_stale=true found nothing")
	}
}
