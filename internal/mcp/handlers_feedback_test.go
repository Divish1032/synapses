package mcp

// handlers_feedback_test.go — Sprint 30.7 tests for:
//   (A) memory(action="feedback") — suppression entry + quality metric
//   (B) session_init onboarding messages for sessions 1-5

import (
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── Part A: memory(action="feedback") ─────────────────────────────────────────

// TestMemoryFeedback_RequiresContent verifies the handler returns a tool-level
// error when content is empty.
func TestMemoryFeedback_RequiresContent(t *testing.T) {
	srv := newTestServer(t)
	result, err := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action": "feedback",
	}))
	errText := mustErrorResult(t, result, err)
	if !strings.Contains(errText, "content is required") {
		t.Errorf("expected 'content is required' in error, got: %q", errText)
	}
}

// TestMemoryFeedback_StoresObservation verifies that a valid feedback call
// persists a user_feedback session observation.
func TestMemoryFeedback_StoresObservation(t *testing.T) {
	srv := newTestServer(t)
	srv.projectID = "proj-feedback-test"

	result, err := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":   "feedback",
		"content":  "The security finding about /health is wrong — this endpoint is public by design",
		"target":   "/api/health",
		"agent_id": "agent-fb-1",
	}))
	m := mustResult(t, result, err)

	if stored, _ := m["stored"].(bool); !stored {
		t.Errorf("expected stored=true, got: %v", m["stored"])
	}
	if _, ok := m["note"].(string); !ok {
		t.Error("expected note string in response")
	}
	if target, _ := m["target"].(string); target != "/api/health" {
		t.Errorf("expected target='/api/health', got %q", target)
	}

	// Verify the observation is in the store.
	obs, err := srv.store.GetObservationsByCategory(srv.projectID, store.ObsCategoryUserFeedback, 10)
	if err != nil {
		t.Fatalf("GetObservationsByCategory: %v", err)
	}
	if len(obs) != 1 {
		t.Fatalf("expected 1 feedback observation, got %d", len(obs))
	}
	if obs[0].Key != "/api/health" {
		t.Errorf("key: want '/api/health', got %q", obs[0].Key)
	}
	if obs[0].Value != "The security finding about /health is wrong — this endpoint is public by design" {
		t.Errorf("value mismatch: %q", obs[0].Value)
	}
	if obs[0].Confidence != 1.0 {
		t.Errorf("confidence: want 1.0, got %v", obs[0].Confidence)
	}
}

// TestMemoryFeedback_WithoutTarget uses "general" as the default key.
func TestMemoryFeedback_WithoutTarget(t *testing.T) {
	srv := newTestServer(t)
	srv.projectID = "proj-feedback-notarget"

	result, err := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":  "feedback",
		"content": "The conventions section is always empty but we do have conventions",
	}))
	m := mustResult(t, result, err)

	if stored, _ := m["stored"].(bool); !stored {
		t.Errorf("expected stored=true")
	}
	// target field should be absent when not provided.
	if _, present := m["target"]; present {
		t.Error("target should be absent when not provided")
	}

	obs, err := srv.store.GetObservationsByCategory(srv.projectID, store.ObsCategoryUserFeedback, 10)
	if err != nil {
		t.Fatalf("GetObservationsByCategory: %v", err)
	}
	if len(obs) != 1 {
		t.Fatalf("expected 1 observation, got %d", len(obs))
	}
	if obs[0].Key != "general" {
		t.Errorf("key: want 'general', got %q", obs[0].Key)
	}
}

// TestCountUserFeedback_ZeroWhenNone verifies CountUserFeedback returns 0 on a
// fresh project with no feedback.
func TestCountUserFeedback_ZeroWhenNone(t *testing.T) {
	srv := newTestServer(t)
	count := srv.store.CountUserFeedback("proj-no-feedback")
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}
}

// TestCountUserFeedback_CountsCorrectly verifies CountUserFeedback counts only
// user_feedback observations (not other categories).
func TestCountUserFeedback_CountsCorrectly(t *testing.T) {
	srv := newTestServer(t)
	projectID := "proj-count-feedback"

	// Insert 2 feedback observations and 1 non-feedback.
	for _, key := range []string{"key1", "key2"} {
		_, err := srv.store.InsertSessionObservation(store.SessionObservation{
			SessionID:  "sess-1",
			ProjectID:  projectID,
			AgentID:    "agent-1",
			Category:   store.ObsCategoryUserFeedback,
			Key:        key,
			Value:      "wrong",
			Confidence: 1.0,
		})
		if err != nil {
			t.Fatalf("InsertSessionObservation: %v", err)
		}
	}
	_, _ = srv.store.InsertSessionObservation(store.SessionObservation{
		SessionID: "sess-1",
		ProjectID: projectID,
		AgentID:   "agent-1",
		Category:  store.ObsCategoryUserPref,
		Key:       "prefers_verbose",
		Value:     "",
	})

	count := srv.store.CountUserFeedback(projectID)
	if count != 2 {
		t.Errorf("expected 2, got %d", count)
	}
}

// TestMemoryFeedback_UnknownActionFails verifies the dispatch still rejects
// unknown actions, and the error message lists "feedback" as a valid action.
func TestMemoryFeedback_UnknownActionFails(t *testing.T) {
	srv := newTestServer(t)
	result, err := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action": "unknown_action_xyz",
	}))
	errText := mustErrorResult(t, result, err)
	if !strings.Contains(errText, "unknown memory action") {
		t.Errorf("expected 'unknown memory action' in error, got: %q", errText)
	}
	if !strings.Contains(errText, "feedback") {
		t.Errorf("expected 'feedback' in valid actions list, got: %q", errText)
	}
}

// TestMemoryFeedback_NilStore verifies that feedback without a persistent store
// returns stored=false and a warning, but does not return an error.
func TestMemoryFeedback_NilStore(t *testing.T) {
	srv := newTestServer(t)
	srv.store = nil // simulate no persistent store

	result, err := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":  "feedback",
		"content": "The entity count is always wrong",
	}))
	m := mustResult(t, result, err)

	if stored, _ := m["stored"].(bool); stored {
		t.Error("expected stored=false when store is nil")
	}
	if _, ok := m["warning"].(string); !ok {
		t.Error("expected warning string when store is nil")
	}
}

// ── Part B: session_init onboarding ───────────────────────────────────────────

// newOnboardingServer creates a server with a given projectID that has N prior
// sessions already in the store (simulating project history).
func newOnboardingServer(t *testing.T, projectID string, priorSessions int) *Server {
	t.Helper()
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	// Pre-insert N prior sessions via GetOrResumeSession (same mechanism that
	// CountProjectSessions reads from).
	for i := 0; i < priorSessions; i++ {
		if _, _, _, insertErr := st.GetOrResumeSession(
			"prior-agent", projectID,
			"prior-mcp-conn-"+string(rune('a'+i)),
			"prior work", 0, -1,
		); insertErr != nil {
			t.Fatalf("pre-insert session %d: %v", i, insertErr)
		}
	}
	srv := New(g, cfg, st)
	srv.projectID = projectID
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })
	return srv
}

// TestSessionInit_OnboardingFirstSession verifies that session 1 gets the
// "FIRST SESSION | ... | No memories yet" onboarding message.
func TestSessionInit_OnboardingFirstSession(t *testing.T) {
	// 0 prior sessions → first session_init creates session 1.
	srv := newOnboardingServer(t, "proj-onboard-s1", 0)

	result, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "agent-ob-1",
	}))
	m := mustResult(t, result, err)

	ob, ok := m["onboarding"].(string)
	if !ok || ob == "" {
		t.Fatalf("expected onboarding string on first session, got %T: %v", m["onboarding"], m["onboarding"])
	}
	if !strings.Contains(ob, "FIRST SESSION") {
		t.Errorf("first session onboarding should contain 'FIRST SESSION', got: %q", ob)
	}
	if !strings.Contains(ob, "No memories yet") {
		t.Errorf("first session onboarding should mention 'No memories yet', got: %q", ob)
	}
}

// TestSessionInit_OnboardingLearningPhase verifies that sessions 2-5 get the
// "Learning phase (session N of 5)" message.
func TestSessionInit_OnboardingLearningPhase(t *testing.T) {
	// 1 prior session → session_init creates session 2.
	srv := newOnboardingServer(t, "proj-onboard-learn", 1)

	result, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "agent-ob-learn",
	}))
	m := mustResult(t, result, err)

	ob, ok := m["onboarding"].(string)
	if !ok || ob == "" {
		t.Fatalf("expected onboarding string on learning-phase session, got %T", m["onboarding"])
	}
	if !strings.Contains(ob, "Learning phase") {
		t.Errorf("sessions 2-5 onboarding should say 'Learning phase', got: %q", ob)
	}
}

// TestSessionInit_OnboardingAbsentAfterSession5 verifies that session 6+ has
// no onboarding field (graduated from learning phase).
func TestSessionInit_OnboardingAbsentAfterSession5(t *testing.T) {
	// 5 prior sessions → session_init creates session 6.
	srv := newOnboardingServer(t, "proj-onboard-grad", 5)

	result, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "agent-ob-grad",
	}))
	m := mustResult(t, result, err)

	if ob, present := m["onboarding"]; present {
		t.Errorf("onboarding should be absent after session 5, got: %v", ob)
	}
}

// TestSessionInit_OnboardingAbsentInResumeMode verifies that resume mode
// does not include the onboarding message (it's for session continuity, not
// first-session orientation).
func TestSessionInit_OnboardingAbsentInResumeMode(t *testing.T) {
	// 0 prior sessions → first session; but resume mode should suppress onboarding.
	srv := newOnboardingServer(t, "proj-onboard-resume", 0)

	result, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "agent-ob-resume",
		"scope":    "resume",
	}))
	m := mustResult(t, result, err)

	if ob, present := m["onboarding"]; present {
		t.Errorf("onboarding should be absent in resume mode, got: %v", ob)
	}
}
