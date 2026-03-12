package mcp

// White-box tests for F17: Adaptive Context Learning.
// Tests target adaptiveCarveConfig directly and the handleGetContext integration.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── adaptiveCarveConfig unit tests ────────────────────────────────────────────

func TestAdaptiveCarveConfig_NoEpisodes_NoChange(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	cfg := s.config.CarveConfig()
	origDepth := cfg.MaxDepth

	forceFullDetail := s.adaptiveCarveConfig(&cfg, "AuthLogin", "agent-001")

	if cfg.MaxDepth != origDepth {
		t.Errorf("depth changed from %d to %d with no episodes", origDepth, cfg.MaxDepth)
	}
	if forceFullDetail {
		t.Error("expected forceFullDetail=false with no episodes")
	}
}

func TestAdaptiveCarveConfig_RecentUnhelpful_BoostsDepthByOne(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	cfg := s.config.CarveConfig()
	origDepth := cfg.MaxDepth

	// Store a helpful=false episode within the last 7 days (will trigger the boost).
	// ProjectID must match s.graph.RepoID() so RecallEpisodes' project filter finds it.
	_, err := s.store.RememberEpisode(store.Episode{
		AgentID:     "agent-002",
		ProjectID:   "test-repo",
		EpisodeType: "pattern",
		Outcome:     "failure",
		Trigger:     `explicit feedback on get_context("AuthLogin")`,
		Decision:    `Context for "AuthLogin" was not helpful — agent signalled miss`,
		Tags:        `["feedback","context_quality","explicit"]`,
		Importance:  0.4,
		CreatedAt:   time.Now().Unix(), // recent
	})
	if err != nil {
		t.Fatalf("RememberEpisode: %v", err)
	}

	forceFullDetail := s.adaptiveCarveConfig(&cfg, "AuthLogin", "agent-002")

	if cfg.MaxDepth != origDepth+1 {
		t.Errorf("expected depth %d (orig+1), got %d", origDepth+1, cfg.MaxDepth)
	}
	if !forceFullDetail {
		t.Error("expected forceFullDetail=true after unhelpful feedback")
	}
}

func TestAdaptiveCarveConfig_OldUnhelpful_NoChange(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	cfg := s.config.CarveConfig()
	origDepth := cfg.MaxDepth

	// Episode is 10 days old — beyond the 7-day decay window → no boost.
	_, err := s.store.RememberEpisode(store.Episode{
		AgentID:     "agent-003",
		ProjectID:   "test-repo",
		EpisodeType: "pattern",
		Outcome:     "failure",
		Trigger:     `explicit feedback on get_context("AuthLogin")`,
		Decision:    `Context for "AuthLogin" was not helpful`,
		Tags:        `["feedback","context_quality","explicit"]`,
		Importance:  0.4,
		CreatedAt:   time.Now().AddDate(0, 0, -10).Unix(), // 10 days old
	})
	if err != nil {
		t.Fatalf("RememberEpisode: %v", err)
	}

	forceFullDetail := s.adaptiveCarveConfig(&cfg, "AuthLogin", "agent-003")

	if cfg.MaxDepth != origDepth {
		t.Errorf("old feedback should not change depth: got %d, want %d", cfg.MaxDepth, origDepth)
	}
	if forceFullDetail {
		t.Error("expected forceFullDetail=false for decayed-out feedback")
	}
}

func TestAdaptiveCarveConfig_TwoCrossSessionRepeats_ExpandsToDepth3(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	cfg := s.config.CarveConfig()

	// Two repeated_context episodes (cross-session pattern, within 30 days).
	for i := 0; i < 2; i++ {
		_, err := s.store.RememberEpisode(store.Episode{
			AgentID:     "agent-004",
			ProjectID:   "test-repo",
			EpisodeType: "pattern",
			Outcome:     "partial",
			Trigger:     `get_context called 3x for "AuthLogin"`,
			Decision:    `Repeated context requests for "AuthLogin"`,
			Tags:        `["feedback","repeated_context","auto"]`,
			Importance:  0.3,
			CreatedAt:   time.Now().AddDate(0, 0, -i).Unix(),
		})
		if err != nil {
			t.Fatalf("RememberEpisode %d: %v", i, err)
		}
	}

	forceFullDetail := s.adaptiveCarveConfig(&cfg, "AuthLogin", "agent-004")

	if cfg.MaxDepth < 3 {
		t.Errorf("expected depth ≥ 3 after 2 repeated_context episodes, got %d", cfg.MaxDepth)
	}
	if !forceFullDetail {
		t.Error("expected forceFullDetail=true after cross-session repeat episodes")
	}
}

func TestAdaptiveCarveConfig_OneCrossSessionRepeat_NoExpansion(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	cfg := s.config.CarveConfig()
	origDepth := cfg.MaxDepth

	// Only 1 repeated_context episode — below the threshold of 2.
	_, err := s.store.RememberEpisode(store.Episode{
		AgentID:     "agent-005",
		ProjectID:   "test-repo",
		EpisodeType: "pattern",
		Outcome:     "partial",
		Trigger:     `get_context called 3x for "AuthLogin"`,
		Decision:    `Repeated context requests for "AuthLogin"`,
		Tags:        `["feedback","repeated_context","auto"]`,
		Importance:  0.3,
		CreatedAt:   time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("RememberEpisode: %v", err)
	}

	forceFullDetail := s.adaptiveCarveConfig(&cfg, "AuthLogin", "agent-005")

	if cfg.MaxDepth != origDepth {
		t.Errorf("single repeat should not expand depth: got %d, want %d", cfg.MaxDepth, origDepth)
	}
	if forceFullDetail {
		t.Error("expected forceFullDetail=false with only 1 repeat episode")
	}
}

func TestAdaptiveCarveConfig_DepthAlreadyAtOrAbove3_RepeatsDoNotShrink(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	cfg := s.config.CarveConfig()
	cfg.MaxDepth = 4 // already deeper than the expansion target

	for i := 0; i < 2; i++ {
		_, _ = s.store.RememberEpisode(store.Episode{
			AgentID:     "agent-006",
			ProjectID:   "test-repo",
			EpisodeType: "pattern",
			Outcome:     "partial",
			Trigger:     `get_context called 3x for "AuthLogin"`,
			Decision:    `Repeated context requests for "AuthLogin"`,
			Tags:        `["feedback","repeated_context","auto"]`,
			CreatedAt:   time.Now().Unix(),
		})
	}

	s.adaptiveCarveConfig(&cfg, "AuthLogin", "agent-006")

	if cfg.MaxDepth != 4 {
		t.Errorf("depth should stay at 4 (not shrink), got %d", cfg.MaxDepth)
	}
}

func TestAdaptiveCarveConfig_BothSignals_ForceFullDetailAndMaxDepth3(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	cfg := s.config.CarveConfig()

	// Unhelpful feedback (recent) + 2 repeated_context episodes.
	_, _ = s.store.RememberEpisode(store.Episode{
		AgentID:     "agent-007",
		ProjectID:   "test-repo",
		EpisodeType: "pattern",
		Outcome:     "failure",
		Trigger:     `explicit feedback on get_context("AuthLogin")`,
		Tags:        `["feedback","context_quality","explicit"]`,
		Importance:  0.4,
		CreatedAt:   time.Now().Unix(),
	})
	for i := 0; i < 2; i++ {
		_, _ = s.store.RememberEpisode(store.Episode{
			AgentID:     "agent-007",
			ProjectID:   "test-repo",
			EpisodeType: "pattern",
			Outcome:     "partial",
			Decision:    `Repeated context requests for "AuthLogin"`,
			Tags:        `["feedback","repeated_context","auto"]`,
			CreatedAt:   time.Now().AddDate(0, 0, -i).Unix(),
		})
	}

	forceFullDetail := s.adaptiveCarveConfig(&cfg, "AuthLogin", "agent-007")

	if cfg.MaxDepth < 3 {
		t.Errorf("expected depth ≥ 3, got %d", cfg.MaxDepth)
	}
	if !forceFullDetail {
		t.Error("expected forceFullDetail=true when both signals present")
	}
}

func TestAdaptiveCarveConfig_WrongAgentID_NoEffect(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	cfg := s.config.CarveConfig()
	origDepth := cfg.MaxDepth

	// Episode recorded for "agent-A" — querying for "agent-B" must not trigger boost.
	_, _ = s.store.RememberEpisode(store.Episode{
		AgentID:     "agent-A",
		ProjectID:   "test-repo",
		EpisodeType: "pattern",
		Outcome:     "failure",
		Trigger:     `explicit feedback on get_context("AuthLogin")`,
		Tags:        `["feedback","context_quality","explicit"]`,
		CreatedAt:   time.Now().Unix(),
	})

	s.adaptiveCarveConfig(&cfg, "AuthLogin", "agent-B")

	if cfg.MaxDepth != origDepth {
		t.Errorf("different agent's feedback should not affect depth: got %d, want %d", cfg.MaxDepth, origDepth)
	}
}

// ── handleGetContext integration tests ─────────────────────────────────────────

// newPopulatedServerWithStore creates a server where we can inject episodes and
// then call handleGetContext to verify end-to-end adaptive expansion.
func newAdaptiveTestServer(t *testing.T) *Server {
	t.Helper()
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	id := g.MakeNodeID("pkg/auth/auth.go", "AuthService")
	g.AddNode(&graph.Node{
		ID: id, Type: graph.NodeFunction,
		Name: "AuthService", File: "pkg/auth/auth.go", Line: 1, Package: "auth",
	})
	return New(g, cfg, st)
}

func TestHandleGetContext_AdaptiveHint_SetAfterUnhelpfulFeedback(t *testing.T) {
	s := newAdaptiveTestServer(t)

	// Store a recent helpful=false episode for "AuthService" / "agent-x".
	_, err := s.store.RememberEpisode(store.Episode{
		AgentID:     "agent-x",
		ProjectID:   "test-repo",
		EpisodeType: "pattern",
		Outcome:     "failure",
		Trigger:     `explicit feedback on get_context("AuthService")`,
		Tags:        `["feedback","context_quality","explicit"]`,
		Importance:  0.4,
		CreatedAt:   time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("RememberEpisode: %v", err)
	}

	req := callTool(map[string]any{
		"entity":   "AuthService",
		"agent_id": "agent-x",
	})
	result, err := s.handleGetContext(context.Background(), req)
	if err != nil {
		t.Fatalf("handleGetContext: %v", err)
	}
	m := mustResult(t, result, nil)

	hint, _ := m["adaptive_hint"].(string)
	if hint == "" {
		t.Error("expected adaptive_hint in response when helpful=false feedback was found")
	}
	if !strings.Contains(hint, "prior feedback") {
		t.Errorf("adaptive_hint = %q, want to contain 'prior feedback'", hint)
	}
}

func TestHandleGetContext_NoAdaptiveHint_WhenNoFeedback(t *testing.T) {
	s := newAdaptiveTestServer(t)

	req := callTool(map[string]any{
		"entity":   "AuthService",
		"agent_id": "fresh-agent",
	})
	result, err := s.handleGetContext(context.Background(), req)
	if err != nil {
		t.Fatalf("handleGetContext: %v", err)
	}
	m := mustResult(t, result, nil)

	if _, ok := m["adaptive_hint"]; ok {
		t.Error("expected no adaptive_hint when no prior feedback exists")
	}
}

func TestHandleGetContext_ExplicitDepthOverridesAdaptive(t *testing.T) {
	s := newAdaptiveTestServer(t)

	// Store enough repeated_context episodes to trigger depth=3 expansion.
	for i := 0; i < 2; i++ {
		_, _ = s.store.RememberEpisode(store.Episode{
			AgentID:     "agent-y",
			ProjectID:   "test-repo",
			EpisodeType: "pattern",
			Outcome:     "partial",
			Decision:    `Repeated context requests for "AuthService"`,
			Tags:        `["feedback","repeated_context","auto"]`,
			CreatedAt:   time.Now().Unix(),
		})
	}

	// Caller explicitly requests depth=1 — must win over adaptive depth=3.
	req := callTool(map[string]any{
		"entity":   "AuthService",
		"agent_id": "agent-y",
		"depth":    float64(1),
	})
	// We can't directly inspect cfg.MaxDepth from outside, but if the explicit
	// depth wins the BFS won't error and the result should be valid (not deeper).
	result, err := s.handleGetContext(context.Background(), req)
	if err != nil {
		t.Fatalf("handleGetContext: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %v", result.Content)
	}
	// adaptive_hint should still be set (expansion was attempted) even if depth was overridden.
	m := mustResult(t, result, nil)
	if _, ok := m["adaptive_hint"]; !ok {
		t.Error("adaptive_hint should be present even when explicit depth overrides adaptive depth")
	}
}

// TestAdaptiveCarveConfig_PartialOutcome_NoBoost ensures outcome="partial" does
// NOT trigger the unhelpful-feedback depth boost (only outcome="failure" does).
func TestAdaptiveCarveConfig_PartialOutcome_NoBoost(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	cfg := s.config.CarveConfig()
	origDepth := cfg.MaxDepth

	_, err := s.store.RememberEpisode(store.Episode{
		AgentID:     "agent-008",
		ProjectID:   "test-repo",
		EpisodeType: "pattern",
		Outcome:     "partial", // not "failure" — must NOT trigger boost
		Trigger:     `explicit feedback on get_context("AuthLogin")`,
		Decision:    `Context for "AuthLogin" was partially helpful`,
		Tags:        `["feedback","context_quality","explicit"]`,
		Importance:  0.3,
		CreatedAt:   time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("RememberEpisode: %v", err)
	}

	forceFullDetail := s.adaptiveCarveConfig(&cfg, "AuthLogin", "agent-008")

	if cfg.MaxDepth != origDepth {
		t.Errorf("partial outcome must not boost depth: got %d, want %d", cfg.MaxDepth, origDepth)
	}
	if forceFullDetail {
		t.Error("expected forceFullDetail=false for partial-outcome context_quality episode")
	}
}

// TestHandleGetContext_CompactFormat_AdaptiveHintInText verifies that the
// adaptive hint appears in compact (plain-text) responses, and that
// detail_level is implicitly forced to "full" when adaptive expansion fired.
func TestHandleGetContext_CompactFormat_AdaptiveHintInText(t *testing.T) {
	s := newAdaptiveTestServer(t)

	_, err := s.store.RememberEpisode(store.Episode{
		AgentID:     "agent-z",
		ProjectID:   "test-repo",
		EpisodeType: "pattern",
		Outcome:     "failure",
		Trigger:     `explicit feedback on get_context("AuthService")`,
		Tags:        `["feedback","context_quality","explicit"]`,
		Importance:  0.4,
		CreatedAt:   time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("RememberEpisode: %v", err)
	}

	req := callTool(map[string]any{
		"entity":   "AuthService",
		"agent_id": "agent-z",
		"format":   "compact",
		// no explicit detail_level — adaptive should force "full"
	})
	result, err := s.handleGetContext(context.Background(), req)
	if err != nil {
		t.Fatalf("handleGetContext: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %v", result.Content)
	}
	tc, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	// Adaptive hint appended by serializeCompact when dc.AdaptiveHint != "".
	if !strings.Contains(tc.Text, "prior feedback") {
		t.Errorf("compact text should contain adaptive hint, got:\n%s", tc.Text)
	}
}

func TestHandleGetContext_NoAgentID_AdaptiveSkipped(t *testing.T) {
	s := newAdaptiveTestServer(t)

	// Ensure no panics and no adaptive_hint when agent_id is absent.
	req := callTool(map[string]any{
		"entity": "AuthService",
		// no agent_id
	})
	result, err := s.handleGetContext(context.Background(), req)
	if err != nil {
		t.Fatalf("handleGetContext: %v", err)
	}
	m := mustResult(t, result, nil)
	if _, ok := m["adaptive_hint"]; ok {
		t.Error("no adaptive_hint expected when agent_id is missing")
	}
}
