package skills

import "fmt"

// TrustOrigin classifies where a skill recipe came from.
// Origin determines the default permission set the recipe is granted.
type TrustOrigin string

const (
	// TrustBuiltin is compiled into the binary — fully trusted.
	TrustBuiltin TrustOrigin = "builtin"
	// TrustUser is loaded from ~/.synapses/skills/ — trusted for most operations.
	TrustUser TrustOrigin = "user"
	// TrustProject is loaded from .synapses/skills/ in the workspace — read-only graph access only.
	// Project-scoped recipes are checked into version control and may come from untrusted repos.
	TrustProject TrustOrigin = "project"
	// TrustRemote is fetched from a remote source — most restrictive.
	// Reserved for Phase 2+; Phase 1 never loads remote recipes.
	TrustRemote TrustOrigin = "remote"
)

// Permission is a named capability gate checked before recipe execution.
type Permission string

const (
	// PermGraphRead allows read-only access to the code graph (get_context, get_impact, etc.).
	PermGraphRead Permission = "graph_read"
	// PermGraphWrite allows mutations to the graph or task store (annotate_node, update_task, etc.).
	PermGraphWrite Permission = "graph_write"
	// PermIntelligence allows calls that reach the intelligence sidecar (LLM-powered tools).
	PermIntelligence Permission = "intelligence"
	// PermShell allows spawning shell commands. Reserved for Layer 2 hooks; never granted in Phase 1.
	PermShell Permission = "shell"
	// PermNetwork allows outbound network calls (web_search, web_fetch via scout).
	PermNetwork Permission = "network"
)

// SecurityPolicy defines which permissions each TrustOrigin is allowed.
// It is consulted by the Executor before running any recipe step.
type SecurityPolicy struct {
	grants map[TrustOrigin]map[Permission]bool
}

// DefaultPolicy returns the production security policy.
//
// Grant table:
//
//	builtin  → graph_read, graph_write, intelligence, network  (shell reserved for Phase 2)
//	user     → graph_read, graph_write, intelligence
//	project  → graph_read only
//	remote   → graph_read only
func DefaultPolicy() *SecurityPolicy {
	p := &SecurityPolicy{grants: make(map[TrustOrigin]map[Permission]bool)}

	p.grants[TrustBuiltin] = map[Permission]bool{
		PermGraphRead:    true,
		PermGraphWrite:   true,
		PermIntelligence: true,
		PermNetwork:      true,
		// PermShell intentionally omitted — reserved for Phase 2 with explicit user opt-in
	}
	p.grants[TrustUser] = map[Permission]bool{
		PermGraphRead:    true,
		PermGraphWrite:   true,
		PermIntelligence: true,
	}
	p.grants[TrustProject] = map[Permission]bool{
		PermGraphRead: true,
	}
	p.grants[TrustRemote] = map[Permission]bool{
		PermGraphRead: true,
	}
	return p
}

// toolPermissions maps tool name prefixes to the permission they require.
// This is used to infer permissions when a recipe has empty RequiredPermissions.
var toolPermissions = map[string]Permission{
	"remember":          PermGraphWrite,
	"annotate":          PermGraphWrite,
	"create_plan":       PermGraphWrite,
	"update_task":       PermGraphWrite,
	"create_rule":       PermGraphWrite,
	"add_violation":     PermGraphWrite,
	"brain_":            PermIntelligence,
	"enrich":            PermIntelligence,
	"web_search":        PermNetwork,
	"web_fetch":         PermNetwork,
	"fetch_package_doc": PermNetwork,
}

// inferPermissionsFromSteps derives the set of permissions a recipe needs
// based on the tools it invokes. Used when RequiredPermissions is empty.
func inferPermissionsFromSteps(steps []RecipeStep) []string {
	perms := make(map[Permission]bool)
	// All recipes implicitly need graph_read.
	perms[PermGraphRead] = true
	for _, step := range steps {
		for prefix, perm := range toolPermissions {
			if step.Tool == prefix || len(step.Tool) > len(prefix) && step.Tool[:len(prefix)] == prefix {
				perms[perm] = true
			}
		}
	}
	out := make([]string, 0, len(perms))
	for p := range perms {
		out = append(out, string(p))
	}
	return out
}

// Check returns nil if the recipe's origin is granted all required permissions,
// or a descriptive error listing the first denied permission.
// An empty required list now infers minimum permissions from the recipe steps
// rather than granting unconditional access.
// An unrecognised origin is treated as TrustRemote (most restrictive).
func (p *SecurityPolicy) Check(skillID string, origin TrustOrigin, required []string) error {
	if len(required) == 0 {
		// For builtin recipes, empty permissions is safe (fully trusted).
		if TrustOrigin(origin) == TrustBuiltin {
			return nil
		}
		// For non-builtin recipes, require at least graph_read.
		required = []string{string(PermGraphRead)}
	}
	allowed := p.grants[origin]
	// Unknown origin → fall back to remote (most restrictive).
	if allowed == nil {
		allowed = p.grants[TrustRemote]
	}
	for _, req := range required {
		if !allowed[Permission(req)] {
			return fmt.Errorf(
				"skills: recipe %q (origin=%q) requires permission %q which is not granted to %q origin",
				skillID, origin, req, origin,
			)
		}
	}
	return nil
}

// CheckWithSteps validates a recipe's origin against both declared and inferred permissions.
// This provides defense-in-depth: even if RequiredPermissions is empty, the tools
// in the steps are checked against the origin's allowed set.
func (p *SecurityPolicy) CheckWithSteps(skillID string, origin TrustOrigin, required []string, steps []RecipeStep) error {
	// First check declared permissions.
	if err := p.Check(skillID, origin, required); err != nil {
		return err
	}
	// Then check inferred permissions from steps.
	if TrustOrigin(origin) != TrustBuiltin {
		inferred := inferPermissionsFromSteps(steps)
		allowed := p.grants[origin]
		if allowed == nil {
			allowed = p.grants[TrustRemote]
		}
		for _, req := range inferred {
			if !allowed[Permission(req)] {
				return fmt.Errorf(
					"skills: recipe %q (origin=%q) step requires permission %q which is not granted",
					skillID, origin, req,
				)
			}
		}
	}
	return nil
}

