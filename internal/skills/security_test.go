package skills

import (
	"context"
	"testing"
)

// ---------------------------------------------------------------------------
// DefaultPolicy — grant table correctness
// ---------------------------------------------------------------------------

func TestDefaultPolicy_BuiltinGrantsAll(t *testing.T) {
	p := DefaultPolicy()
	for _, perm := range []Permission{PermGraphRead, PermGraphWrite, PermIntelligence, PermNetwork} {
		if !p.Granted(TrustBuiltin, perm) {
			t.Errorf("builtin should have %q", perm)
		}
	}
}

func TestDefaultPolicy_BuiltinNoShell(t *testing.T) {
	// Shell is reserved for Phase 2 — must never be granted in Phase 1.
	p := DefaultPolicy()
	if p.Granted(TrustBuiltin, PermShell) {
		t.Error("builtin must NOT have shell permission in Phase 1")
	}
}

func TestDefaultPolicy_UserGrants(t *testing.T) {
	p := DefaultPolicy()
	granted := []Permission{PermGraphRead, PermGraphWrite, PermIntelligence}
	denied := []Permission{PermShell, PermNetwork}
	for _, perm := range granted {
		if !p.Granted(TrustUser, perm) {
			t.Errorf("user should have %q", perm)
		}
	}
	for _, perm := range denied {
		if p.Granted(TrustUser, perm) {
			t.Errorf("user must NOT have %q", perm)
		}
	}
}

func TestDefaultPolicy_ProjectReadOnly(t *testing.T) {
	p := DefaultPolicy()
	if !p.Granted(TrustProject, PermGraphRead) {
		t.Error("project should have graph_read")
	}
	for _, perm := range []Permission{PermGraphWrite, PermIntelligence, PermShell, PermNetwork} {
		if p.Granted(TrustProject, perm) {
			t.Errorf("project must NOT have %q", perm)
		}
	}
}

func TestDefaultPolicy_RemoteReadOnly(t *testing.T) {
	p := DefaultPolicy()
	if !p.Granted(TrustRemote, PermGraphRead) {
		t.Error("remote should have graph_read")
	}
	for _, perm := range []Permission{PermGraphWrite, PermIntelligence, PermShell, PermNetwork} {
		if p.Granted(TrustRemote, perm) {
			t.Errorf("remote must NOT have %q", perm)
		}
	}
}

// ---------------------------------------------------------------------------
// SecurityPolicy.Check
// ---------------------------------------------------------------------------

func TestCheck_EmptyRequiredAlwaysAllowed(t *testing.T) {
	p := DefaultPolicy()
	if err := p.Check("any-skill", TrustProject, nil); err != nil {
		t.Errorf("empty required should always pass, got: %v", err)
	}
	if err := p.Check("any-skill", TrustProject, []string{}); err != nil {
		t.Errorf("empty required should always pass, got: %v", err)
	}
}

func TestCheck_AllowedPermission(t *testing.T) {
	p := DefaultPolicy()
	if err := p.Check("my-recipe", TrustBuiltin, []string{"graph_read", "graph_write"}); err != nil {
		t.Errorf("builtin with graph_read+write should pass, got: %v", err)
	}
}

func TestCheck_DeniedPermission_Project(t *testing.T) {
	p := DefaultPolicy()
	err := p.Check("my-recipe", TrustProject, []string{"intelligence"})
	if err == nil {
		t.Error("project origin should not have intelligence permission")
	}
}

func TestCheck_DeniedPermission_User_Network(t *testing.T) {
	p := DefaultPolicy()
	err := p.Check("net-skill", TrustUser, []string{"network"})
	if err == nil {
		t.Error("user origin should not have network permission")
	}
}

func TestCheck_UnknownOriginFallsToRemote(t *testing.T) {
	p := DefaultPolicy()
	// Unknown origin should be treated as remote (most restrictive).
	err := p.Check("x", TrustOrigin("unknown"), []string{"graph_write"})
	if err == nil {
		t.Error("unknown origin should be denied graph_write (remote policy)")
	}
	// But graph_read should still be allowed.
	if err := p.Check("x", TrustOrigin("unknown"), []string{"graph_read"}); err != nil {
		t.Errorf("unknown origin should still get graph_read (remote policy), got: %v", err)
	}
}

func TestCheck_ShellNeverGranted(t *testing.T) {
	p := DefaultPolicy()
	for _, origin := range []TrustOrigin{TrustBuiltin, TrustUser, TrustProject, TrustRemote} {
		if err := p.Check("shell-recipe", origin, []string{"shell"}); err == nil {
			t.Errorf("origin %q must never be granted shell permission in Phase 1", origin)
		}
	}
}

// ---------------------------------------------------------------------------
// Executor integration: security gate fires before steps
// ---------------------------------------------------------------------------

type countingCaller struct{ calls int }

func (c *countingCaller) CallTool(_ context.Context, _ string, _ map[string]interface{}) (string, error) {
	c.calls++
	return "ok", nil
}

func TestExecutor_SecurityDenied_NoStepsRun(t *testing.T) {
	caller := &countingCaller{}
	exec := NewExecutor(caller, DefaultPolicy())

	// Project-origin recipe requesting intelligence — should be denied.
	r := Recipe{
		ID:                  "proj-recipe",
		Origin:              string(TrustProject),
		RequiredPermissions: []string{"intelligence"},
		Steps: []RecipeStep{
			{Tool: "get_context", Args: map[string]interface{}{"entity": "Foo"}},
		},
	}
	_, err := exec.Execute(context.Background(), r, nil)
	if err == nil {
		t.Fatal("expected security denial error")
	}
	if caller.calls != 0 {
		t.Errorf("no steps should run after security denial; got %d calls", caller.calls)
	}
}

func TestExecutor_SecurityAllowed_StepsRun(t *testing.T) {
	caller := &countingCaller{}
	exec := NewExecutor(caller, DefaultPolicy())

	// Builtin recipe with graph_read — should be allowed.
	r := Recipe{
		ID:                  "builtin-recipe",
		Origin:              string(TrustBuiltin),
		RequiredPermissions: []string{"graph_read"},
		Steps: []RecipeStep{
			{Tool: "get_context", Args: map[string]interface{}{"entity": "Foo"}},
		},
		Output: "merged",
	}
	result, err := exec.Execute(context.Background(), r, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if caller.calls != 1 {
		t.Errorf("expected 1 step to run, got %d", caller.calls)
	}
	if result.Degraded {
		t.Error("should not be degraded")
	}
}

func TestExecutor_NilPolicy_DefaultsToDefaultPolicy(t *testing.T) {
	caller := &countingCaller{}
	// Passing nil should use DefaultPolicy — project + shell should be denied.
	exec := NewExecutor(caller, nil)

	r := Recipe{
		ID:                  "shell-recipe",
		Origin:              string(TrustProject),
		RequiredPermissions: []string{"shell"},
		Steps:               []RecipeStep{{Tool: "get_context", Args: map[string]interface{}{}}},
	}
	_, err := exec.Execute(context.Background(), r, nil)
	if err == nil {
		t.Error("nil policy should default to DefaultPolicy which denies shell for project")
	}
	if caller.calls != 0 {
		t.Error("no steps should run after denial")
	}
}

func TestBuiltinRecipes_HaveValidPermissions(t *testing.T) {
	p := DefaultPolicy()
	for _, r := range BuiltinRecipes() {
		if err := p.Check(r.ID, TrustBuiltin, r.RequiredPermissions); err != nil {
			t.Errorf("builtin recipe %q fails its own permission check: %v", r.ID, err)
		}
	}
}
