package mcp

import (
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── goalReinforcer unit tests ─────────────────────────────────────────────────

func TestGoalReinforcer_Disabled(t *testing.T) {
	r := newGoalReinforcer(0)
	for i := 0; i < 20; i++ {
		if r.recordAndShouldFire("s1") {
			t.Fatalf("interval=0 should never fire, fired at call %d", i+1)
		}
	}
}

func TestGoalReinforcer_FiresAtInterval(t *testing.T) {
	r := newGoalReinforcer(5)
	fired := []int{}
	for i := 1; i <= 15; i++ {
		if r.recordAndShouldFire("s1") {
			fired = append(fired, i)
		}
	}
	// Should fire at calls 5, 10, 15.
	if len(fired) != 3 {
		t.Fatalf("expected 3 fires, got %d at calls %v", len(fired), fired)
	}
	if fired[0] != 5 || fired[1] != 10 || fired[2] != 15 {
		t.Errorf("expected fires at 5, 10, 15; got %v", fired)
	}
}

func TestGoalReinforcer_IntervalOne(t *testing.T) {
	r := newGoalReinforcer(1)
	for i := 1; i <= 5; i++ {
		if !r.recordAndShouldFire("s1") {
			t.Errorf("interval=1: expected fire at call %d", i)
		}
	}
}

func TestGoalReinforcer_SessionIsolation(t *testing.T) {
	r := newGoalReinforcer(3)
	// s1 records 3 calls → fires.
	for i := 0; i < 2; i++ {
		r.recordAndShouldFire("s1")
	}
	// s2 only has 1 call so far.
	r.recordAndShouldFire("s2")

	// 3rd call for s1 should fire.
	if !r.recordAndShouldFire("s1") {
		t.Error("s1 should fire at 3rd call")
	}
	// 2nd call for s2 should NOT fire (only 2/3).
	if r.recordAndShouldFire("s2") {
		t.Error("s2 should not fire at 2nd call with interval=3")
	}
}

func TestGoalReinforcer_ClearResetsCounter(t *testing.T) {
	r := newGoalReinforcer(3)
	r.recordAndShouldFire("s1")
	r.recordAndShouldFire("s1")
	r.clear("s1")

	if r.responseCountFor("s1") != 0 {
		t.Error("count should be 0 after clear")
	}
	// After clear, next fire should be at call 3 again, not call 1.
	r.recordAndShouldFire("s1")
	r.recordAndShouldFire("s1")
	if r.recordAndShouldFire("s1") {
		// good — fired at 3rd call post-clear
	} else {
		t.Error("should fire at 3rd call after clear")
	}
}

func TestGoalReinforcer_EmptySessionID(t *testing.T) {
	r := newGoalReinforcer(1)
	for i := 0; i < 5; i++ {
		if r.recordAndShouldFire("") {
			t.Error("empty sessionID should never fire")
		}
	}
	if r.responseCountFor("") != 0 {
		t.Error("empty sessionID should not create entry")
	}
}

// ── buildReinforcementBlock unit tests ───────────────────────────────────────

func TestBuildReinforcementBlock_BothEmpty(t *testing.T) {
	if got := buildReinforcementBlock("", nil); got != "" {
		t.Errorf("expected empty string for empty inputs, got %q", got)
	}
}

func TestBuildReinforcementBlock_GoalOnly(t *testing.T) {
	out := buildReinforcementBlock("Implement auth flow", nil)
	if !strings.Contains(out, "Implement auth flow") {
		t.Errorf("expected goal in output, got %q", out)
	}
	if !strings.Contains(out, "📌 Reminder:") {
		t.Errorf("expected reminder prefix, got %q", out)
	}
}

func TestBuildReinforcementBlock_ConventionsOnly(t *testing.T) {
	out := buildReinforcementBlock("", []string{"Use table-driven tests", "gofmt on save"})
	if !strings.Contains(out, "Use table-driven tests") {
		t.Errorf("expected convention in output, got %q", out)
	}
}

func TestBuildReinforcementBlock_Both(t *testing.T) {
	out := buildReinforcementBlock("Add OAuth support", []string{"All handlers use AuthMiddleware"})
	if !strings.Contains(out, "Add OAuth support") {
		t.Errorf("expected goal in output, got %q", out)
	}
	if !strings.Contains(out, "AuthMiddleware") {
		t.Errorf("expected convention in output, got %q", out)
	}
	// Must start with a newline so it appears on its own line in the response.
	if !strings.HasPrefix(out, "\n") {
		t.Errorf("expected leading newline, got %q", out)
	}
}

// ── Integration test: injection fires at correct interval ────────────────────

func TestGoalReinforcement_InjectedAtNthResponse(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	// Enable reinforcement every 3 responses.
	cfg.Session.ReinforcementInterval = 3

	// Insert an in-progress task so currentTaskGoal returns something.
	planID, _, err := st.CreatePlan("Test Plan", "desc", "agent", []store.TaskInput{
		{Title: "Implement login flow", Description: "add login"},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	tasks, err := st.GetPendingTasks(planID, "")
	if err != nil || len(tasks) == 0 {
		t.Fatalf("GetPendingTasks: %v (tasks=%d)", err, len(tasks))
	}
	if _, _, err := st.UpdateTask(tasks[0].ID, "in_progress", "", "agent"); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	// Force a session ID so the reinforcer tracks state.
	sessionID := "test-reinforce-session"
	srv.goalReinforcer.entries["sid"] = nil // no-op — actual wiring goes through ledger session ID

	// Call injectGoalReinforcement directly: result without reinforcement.
	result := mcp.NewToolResultText(`{"test":"value"}`)
	srv.injectGoalReinforcement(result, sessionID)

	// Should have added a new content block with the task goal.
	if len(result.Content) < 2 {
		t.Fatalf("expected at least 2 content blocks after reinforcement, got %d", len(result.Content))
	}
	combined := ""
	for _, c := range result.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			combined += tc.Text
		}
	}
	if !strings.Contains(combined, "Implement login flow") {
		t.Errorf("expected task goal in reinforcement output, got: %q", combined)
	}
	if !strings.Contains(combined, "📌 Reminder:") {
		t.Errorf("expected reminder marker in output, got: %q", combined)
	}
}

// TestGoalReinforcement_NoInjectionWhenGoalAndConventionsEmpty verifies that
// we silently skip injection rather than appending a useless empty reminder.
func TestGoalReinforcement_NoInjectionWhenGoalAndConventionsEmpty(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Session.ReinforcementInterval = 1
	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	// No tasks and no conventions → should not inject.
	result := mcp.NewToolResultText(`{"test":"value"}`)
	initialLen := len(result.Content)
	srv.injectGoalReinforcement(result, "session-empty")

	if len(result.Content) != initialLen {
		t.Errorf("expected no new content block when goal and conventions are empty, got %d blocks", len(result.Content))
	}
}
