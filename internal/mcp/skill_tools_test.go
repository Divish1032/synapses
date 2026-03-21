package mcp

import (
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/skills"
)

// ── handleListSkills ──────────────────────────────────────────────────────────

func TestHandleListSkills_Empty(t *testing.T) {
	s := newTestServer(t)
	// No recipes set — skillRecipes is nil.
	res, err := s.handleListSkills(ctx, callTool(nil))
	m := mustResult(t, res, err)
	hasKey(t, m, "skills")
	hasKey(t, m, "count")
	if cnt, _ := m["count"].(float64); cnt != 0 {
		t.Errorf("expected count=0, got %v", cnt)
	}
}

func TestHandleListSkills_ReturnsRegisteredRecipes(t *testing.T) {
	s := newTestServer(t)
	s.SetSkillRecipes(unitTestRecipes())
	res, err := s.handleListSkills(ctx, callTool(nil))
	m := mustResult(t, res, err)

	list, ok := m["skills"].([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("expected non-empty skills list, got %v", m["skills"])
	}
	first, ok := list[0].(map[string]any)
	if !ok {
		t.Fatalf("expected map entry, got %T", list[0])
	}
	if first["id"] != "unit-test-skill" {
		t.Errorf("expected id=unit-test-skill, got %v", first["id"])
	}
	if first["description"] == "" {
		t.Error("expected non-empty description")
	}
	if steps, _ := first["steps"].(float64); steps != 1 {
		t.Errorf("expected steps=1, got %v", first["steps"])
	}
}

func TestHandleListSkills_SummaryIncludesOrigin(t *testing.T) {
	s := newTestServer(t)
	s.SetSkillRecipes(unitTestRecipes())
	res, err := s.handleListSkills(ctx, callTool(nil))
	m := mustResult(t, res, err)

	list := m["skills"].([]any)
	first := list[0].(map[string]any)
	for _, key := range []string{"id", "description", "origin", "steps"} {
		if _, ok := first[key]; !ok {
			t.Errorf("expected key %q in skill summary", key)
		}
	}
	if first["origin"] != "builtin" {
		t.Errorf("expected origin=builtin, got %v", first["origin"])
	}
}

func TestHandleListSkills_CountMatchesList(t *testing.T) {
	s := newTestServer(t)
	s.SetSkillRecipes(unitTestRecipes())
	res, err := s.handleListSkills(ctx, callTool(nil))
	m := mustResult(t, res, err)

	list := m["skills"].([]any)
	cnt, _ := m["count"].(float64)
	if int(cnt) != len(list) {
		t.Errorf("count=%v does not match len(skills)=%d", cnt, len(list))
	}
}

// ── handleExecuteSkill ────────────────────────────────────────────────────────

func TestHandleExecuteSkill_MissingSkillID(t *testing.T) {
	s := newTestServer(t)
	s.SetSkillRecipes(unitTestRecipes())
	res, err := s.handleExecuteSkill(ctx, callTool(nil))
	mustErrorResult(t, res, err)
}

func TestHandleExecuteSkill_EmptySkillID(t *testing.T) {
	s := newTestServer(t)
	s.SetSkillRecipes(unitTestRecipes())
	res, err := s.handleExecuteSkill(ctx, callTool(map[string]any{"skill_id": ""}))
	mustErrorResult(t, res, err)
}

func TestHandleExecuteSkill_UnknownSkillID_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	s.SetSkillRecipes(unitTestRecipes())
	res, err := s.handleExecuteSkill(ctx, callTool(map[string]any{"skill_id": "does-not-exist"}))
	text := mustErrorResult(t, res, err)
	if text == "" {
		t.Error("expected non-empty error message for unknown skill_id")
	}
}

func TestHandleExecuteSkill_NilExecutor_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	// Set recipes directly without SetSkillRecipes so executor stays nil.
	s.skillRecipes = unitTestRecipes()
	res, err := s.handleExecuteSkill(ctx, callTool(map[string]any{"skill_id": "unit-test-skill"}))
	text := mustErrorResult(t, res, err)
	if text == "" {
		t.Error("expected error message when executor is nil")
	}
}

func TestHandleExecuteSkill_ValidRecipe_ReturnsJSON(t *testing.T) {
	s := newTestServer(t)
	s.SetSkillRecipes(unitTestRecipes())
	res, err := s.handleExecuteSkill(ctx, callTool(map[string]any{"skill_id": "unit-test-skill"}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res == nil {
		t.Fatal("nil result")
	}
	// The recipe runs get_working_state. If it succeeds the output is JSON.
	if !res.IsError {
		if len(res.Content) == 0 {
			t.Fatal("expected content in result")
		}
		tc, ok := res.Content[0].(mcpgo.TextContent)
		if !ok {
			t.Fatalf("expected TextContent, got %T", res.Content[0])
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(tc.Text), &out); err != nil {
			t.Errorf("result is not valid JSON: %v\nraw: %s", err, tc.Text)
		}
		if out["recipe_id"] == nil {
			t.Error("expected recipe_id in execution result")
		}
	}
}

func TestHandleExecuteSkill_ParamsNestedMap(t *testing.T) {
	s := newTestServer(t)
	s.SetSkillRecipes(unitTestRecipes())
	// Pass params as nested map — should not cause a panic or Go error.
	res, err := s.handleExecuteSkill(ctx, callTool(map[string]any{
		"skill_id": "unit-test-skill",
		"params":   map[string]any{"extra": "value"},
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res == nil {
		t.Fatal("nil result")
	}
}

func TestHandleExecuteSkill_ParamsFlatTopLevel(t *testing.T) {
	s := newTestServer(t)
	s.SetSkillRecipes(unitTestRecipes())
	// Flat top-level keys (other than skill_id/params) are also injected as params.
	res, err := s.handleExecuteSkill(ctx, callTool(map[string]any{
		"skill_id": "unit-test-skill",
		"mykey":    "myval",
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res == nil {
		t.Fatal("nil result")
	}
}

// ── test fixtures ─────────────────────────────────────────────────────────────

// unitTestRecipes returns a minimal builtin recipe that calls get_working_state
// (a handler that works with any real store + graph).
func unitTestRecipes() []skills.Recipe {
	return []skills.Recipe{
		{
			ID:          "unit-test-skill",
			Description: "A minimal recipe used in unit tests only",
			Origin:      "builtin",
			Steps: []skills.RecipeStep{
				{
					Tool:      "get_working_state",
					Args:      map[string]interface{}{},
					OutputKey: "state",
				},
			},
		},
	}
}
