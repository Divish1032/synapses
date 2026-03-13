package skills

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// --- ResolveArgs ---

func TestResolveArgs_ParamSubstitution(t *testing.T) {
	args := map[string]interface{}{"entity": "$target", "depth": "$depth"}
	params := map[string]interface{}{"target": "AuthService", "depth": float64(3)}
	got := ResolveArgs(args, params, "", nil)
	if got["entity"] != "AuthService" {
		t.Errorf("entity: got %v", got["entity"])
	}
	if got["depth"] != float64(3) {
		t.Errorf("depth: got %v", got["depth"])
	}
}

func TestResolveArgs_PrevResult(t *testing.T) {
	args := map[string]interface{}{"query": "$prev_result"}
	got := ResolveArgs(args, nil, "some-output", nil)
	if got["query"] != "some-output" {
		t.Errorf("prev_result: got %v", got["query"])
	}
}

func TestResolveArgs_StepOutput(t *testing.T) {
	args := map[string]interface{}{"input": "$step_context"}
	stepOutputs := map[string]string{"context": "ctx-data"}
	got := ResolveArgs(args, nil, "", stepOutputs)
	if got["input"] != "ctx-data" {
		t.Errorf("step_context: got %v", got["input"])
	}
}

func TestResolveArgs_Unresolved(t *testing.T) {
	args := map[string]interface{}{"query": "$unknown"}
	got := ResolveArgs(args, nil, "", nil)
	if got["query"] != "$unknown" {
		t.Errorf("unresolved should be passed through, got %v", got["query"])
	}
}

func TestResolveArgs_NonStringPassthrough(t *testing.T) {
	args := map[string]interface{}{"count": 42, "flag": true}
	got := ResolveArgs(args, nil, "", nil)
	if got["count"] != 42 || got["flag"] != true {
		t.Errorf("non-string values should pass through unchanged: %v", got)
	}
}

// --- LoadRecipeDir ---

func TestLoadRecipeDir_NonExistent(t *testing.T) {
	rs, err := LoadRecipeDir("/no/such/dir", "user")
	if err != nil {
		t.Errorf("non-existent dir should return nil error, got: %v", err)
	}
	if rs != nil {
		t.Errorf("non-existent dir should return nil slice")
	}
}

func TestLoadRecipeDir_LoadsJSON(t *testing.T) {
	dir := t.TempDir()
	r := Recipe{
		ID:          "my-recipe",
		Description: "A test recipe",
		Steps: []RecipeStep{
			{Tool: "get_context", Args: map[string]interface{}{"entity": "$target"}},
		},
	}
	data, _ := json.Marshal(r)
	if err := os.WriteFile(filepath.Join(dir, "my-recipe.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	// Non-JSON file should be ignored.
	os.WriteFile(filepath.Join(dir, "ignore.txt"), []byte("ignored"), 0o644)

	rs, err := LoadRecipeDir(dir, "project")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rs) != 1 {
		t.Fatalf("expected 1 recipe, got %d", len(rs))
	}
	if rs[0].ID != "my-recipe" {
		t.Errorf("ID: got %q", rs[0].ID)
	}
	if rs[0].Origin != "project" {
		t.Errorf("Origin: got %q", rs[0].Origin)
	}
}

func TestLoadRecipeDir_FallbackIDFromFilename(t *testing.T) {
	dir := t.TempDir()
	// Recipe with no ID field — should fall back to filename stem.
	r := Recipe{Description: "no id"}
	data, _ := json.Marshal(r)
	os.WriteFile(filepath.Join(dir, "auto-name.json"), data, 0o644)

	rs, err := LoadRecipeDir(dir, "user")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].ID != "auto-name" {
		t.Errorf("ID fallback: got %q", rs[0].ID)
	}
}

func TestLoadRecipeDir_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{not json}"), 0o644)
	os.WriteFile(filepath.Join(dir, "good.json"), []byte(`{"id":"ok","description":"fine"}`), 0o644)

	rs, err := LoadRecipeDir(dir, "user")
	if err != nil {
		t.Fatal(err)
	}
	// bad.json silently skipped; good.json loaded.
	if len(rs) != 1 || rs[0].ID != "ok" {
		t.Errorf("expected 1 valid recipe, got %d: %v", len(rs), rs)
	}
}

// --- DeduplicateRecipes ---

func TestDeduplicateRecipes_LastWins(t *testing.T) {
	recipes := []Recipe{
		{ID: "onboard-to-module", Description: "builtin", Origin: "builtin"},
		{ID: "other", Description: "stays", Origin: "builtin"},
		{ID: "onboard-to-module", Description: "user override", Origin: "user"},
		{ID: "onboard-to-module", Description: "project override", Origin: "project"},
	}
	got := DeduplicateRecipes(recipes)
	if len(got) != 2 {
		t.Fatalf("expected 2, got %d", len(got))
	}
	for _, r := range got {
		if r.ID == "onboard-to-module" && r.Origin != "project" {
			t.Errorf("onboard-to-module should be project version, got origin=%q", r.Origin)
		}
	}
}

func TestDeduplicateRecipes_NoDuplicates(t *testing.T) {
	recipes := []Recipe{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	got := DeduplicateRecipes(recipes)
	if len(got) != 3 {
		t.Errorf("no dups: expected 3, got %d", len(got))
	}
}

// --- BuiltinRecipes ---

func TestBuiltinRecipes_NotEmpty(t *testing.T) {
	rs := BuiltinRecipes()
	if len(rs) == 0 {
		t.Fatal("expected at least one built-in recipe")
	}
	for _, r := range rs {
		if r.ID == "" {
			t.Error("built-in recipe missing ID")
		}
		if r.Description == "" {
			t.Errorf("built-in recipe %q missing description", r.ID)
		}
		if len(r.Steps) == 0 {
			t.Errorf("built-in recipe %q has no steps", r.ID)
		}
		if r.Origin != "builtin" {
			t.Errorf("built-in recipe %q has wrong origin: %q", r.ID, r.Origin)
		}
	}
}
