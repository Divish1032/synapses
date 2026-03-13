package skills

import (
	"context"
	"errors"
	"testing"
)

// mockCaller records calls and returns configured responses.
type mockCaller struct {
	responses map[string]string
	errors    map[string]error
	calls     []string
}

func newMockCaller() *mockCaller {
	return &mockCaller{
		responses: make(map[string]string),
		errors:    make(map[string]error),
	}
}

func (m *mockCaller) CallTool(_ context.Context, toolName string, _ map[string]interface{}) (string, error) {
	m.calls = append(m.calls, toolName)
	if err, ok := m.errors[toolName]; ok {
		return "", err
	}
	if resp, ok := m.responses[toolName]; ok {
		return resp, nil
	}
	return `{"ok":true}`, nil
}

// --- Executor.Execute ---

func TestExecutor_AllStepsRun(t *testing.T) {
	caller := newMockCaller()
	caller.responses["get_context"] = `{"entity":"Graph"}`
	caller.responses["get_violations"] = `{"violations":[]}`

	exec := NewExecutor(caller, nil)
	recipe := Recipe{
		ID:     "test-recipe",
		Output: "structured",
		Params: []RecipeParam{{Name: "target", Type: "string", Required: true}},
		Steps: []RecipeStep{
			{Tool: "get_context", Args: map[string]interface{}{"entity": "$target"}, OutputKey: "ctx"},
			{Tool: "get_violations", Args: map[string]interface{}{}, OutputKey: "violations"},
		},
	}

	result, err := exec.Execute(context.Background(), recipe, map[string]interface{}{"target": "Graph"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Steps) != 2 {
		t.Errorf("expected 2 steps, got %d", len(result.Steps))
	}
	if result.Degraded {
		t.Error("should not be degraded")
	}
	if _, ok := result.Structured["ctx"]; !ok {
		t.Error("structured output missing 'ctx' key")
	}
}

func TestExecutor_OptionalStepSkipped(t *testing.T) {
	caller := newMockCaller()
	caller.responses["get_context"] = `{"entity":"Graph"}`
	caller.errors["recall"] = errors.New("store unavailable")

	exec := NewExecutor(caller, nil)
	recipe := Recipe{
		ID:     "test-optional",
		Output: "structured",
		Steps: []RecipeStep{
			{Tool: "get_context", Args: map[string]interface{}{}, OutputKey: "ctx"},
			{Tool: "recall", Args: map[string]interface{}{}, OutputKey: "failures", Optional: true},
		},
	}

	result, err := exec.Execute(context.Background(), recipe, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should complete with degraded=true, not abort.
	if !result.Degraded {
		t.Error("expected degraded=true when optional step skipped")
	}
	if len(result.Steps) != 2 {
		t.Errorf("expected 2 step records, got %d", len(result.Steps))
	}
	if !result.Steps[1].Skipped {
		t.Error("recall step should be marked skipped")
	}
}

func TestExecutor_RequiredStepFailAborts(t *testing.T) {
	caller := newMockCaller()
	caller.errors["get_context"] = errors.New("graph offline")

	exec := NewExecutor(caller, nil)
	recipe := Recipe{
		ID: "test-abort",
		Steps: []RecipeStep{
			{Tool: "get_context", Args: map[string]interface{}{}}, // not optional
		},
	}

	_, err := exec.Execute(context.Background(), recipe, nil)
	if err == nil {
		t.Error("expected error when required step fails")
	}
}

func TestExecutor_MissingRequiredParam(t *testing.T) {
	exec := NewExecutor(newMockCaller(), nil)
	recipe := Recipe{
		ID:     "test-param",
		Params: []RecipeParam{{Name: "target", Type: "string", Required: true}},
		Steps:  []RecipeStep{{Tool: "get_context", Args: map[string]interface{}{}}},
	}

	_, err := exec.Execute(context.Background(), recipe, nil)
	if err == nil {
		t.Error("expected error for missing required param")
	}
}

func TestExecutor_DefaultParamApplied(t *testing.T) {
	caller := newMockCaller()
	exec := NewExecutor(caller, nil)
	recipe := Recipe{
		ID: "test-default",
		Params: []RecipeParam{
			{Name: "depth", Type: "number", Required: false, Default: float64(3)},
		},
		Steps: []RecipeStep{
			{Tool: "get_context", Args: map[string]interface{}{"depth": "$depth"}, OutputKey: "ctx"},
		},
		Output: "structured",
	}

	result, err := exec.Execute(context.Background(), recipe, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Steps) != 1 || result.Steps[0].Skipped {
		t.Error("expected step to run with default param")
	}
}

func TestExecutor_PrevResultPropagated(t *testing.T) {
	caller := newMockCaller()
	caller.responses["get_context"] = "first-output"
	caller.responses["get_impact"] = "impact-result"

	exec := NewExecutor(caller, nil)
	recipe := Recipe{
		ID:     "test-prev",
		Output: "merged",
		Steps: []RecipeStep{
			{Tool: "get_context", Args: map[string]interface{}{}},
			// Second step uses $prev_result
			{Tool: "get_impact", Args: map[string]interface{}{"symbol": "$prev_result"}},
		},
	}

	result, err := exec.Execute(context.Background(), recipe, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.MergedOutput == "" {
		t.Error("merged output should not be empty")
	}
}

func TestExecutor_MergedOutput(t *testing.T) {
	caller := newMockCaller()
	caller.responses["get_context"] = "ctx"
	caller.responses["get_violations"] = "violations"

	exec := NewExecutor(caller, nil)
	recipe := Recipe{
		ID:     "test-merge",
		Output: "merged",
		Steps: []RecipeStep{
			{Tool: "get_context", Args: map[string]interface{}{}},
			{Tool: "get_violations", Args: map[string]interface{}{}},
		},
	}

	result, err := exec.Execute(context.Background(), recipe, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.MergedOutput == "" {
		t.Error("merged output should not be empty")
	}
	// Both outputs should appear.
	if result.MergedOutput != "ctx\n\n---\n\nviolations" {
		t.Errorf("merged output: %q", result.MergedOutput)
	}
}
