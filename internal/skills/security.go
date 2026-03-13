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

// Check returns nil if the recipe's origin is granted all required permissions,
// or a descriptive error listing the first denied permission.
// An empty required list is always allowed. An unrecognised origin is treated
// as TrustRemote (most restrictive) so unknown sources fail safe.
func (p *SecurityPolicy) Check(skillID string, origin TrustOrigin, required []string) error {
	if len(required) == 0 {
		return nil
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

// Granted returns true if the given origin has the given permission under this policy.
// Useful for inspection/testing without constructing a full required list.
func (p *SecurityPolicy) Granted(origin TrustOrigin, perm Permission) bool {
	return p.grants[origin][perm]
}
