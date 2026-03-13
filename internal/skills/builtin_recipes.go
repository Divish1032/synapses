package skills

// BuiltinRecipes returns the built-in recipe definitions compiled into the binary.
// These cover the highest-value multi-step workflows an agent typically performs.
func BuiltinRecipes() []Recipe {
	return []Recipe{
		{
			ID:          "onboard-to-module",
			Description: "Comprehensive onboarding to a module: context, call chains, violations, and failure history.",
			Origin:      "builtin",
			Params: []RecipeParam{
				{Name: "target", Type: "string", Required: true},
				{Name: "depth", Type: "number", Required: false, Default: 3},
			},
			Steps: []RecipeStep{
				{Tool: "get_context", Args: map[string]interface{}{"entity": "$target", "depth": "$depth"}, OutputKey: "context"},
				{Tool: "get_call_chain", Args: map[string]interface{}{"from": "$target", "to": ""}, OutputKey: "calls", Optional: true},
				{Tool: "get_violations", Args: map[string]interface{}{}, OutputKey: "violations", Optional: true},
				{Tool: "recall", Args: map[string]interface{}{"query": "$target", "episode_type": "failure"}, OutputKey: "failures", Optional: true},
			},
			Output:              "structured",
			RequiredPermissions: []string{"graph_read"},
		},
		{
			ID:          "pre-review-checklist",
			Description: "Pre-change review: code context, downstream impact, current violations, and failure history for a target symbol.",
			Origin:      "builtin",
			Params: []RecipeParam{
				{Name: "target", Type: "string", Required: true},
			},
			Steps: []RecipeStep{
				{Tool: "get_context", Args: map[string]interface{}{"entity": "$target"}, OutputKey: "context"},
				{Tool: "get_impact", Args: map[string]interface{}{"symbol": "$target"}, OutputKey: "impact"},
				{Tool: "get_violations", Args: map[string]interface{}{}, OutputKey: "violations", Optional: true},
				{Tool: "recall", Args: map[string]interface{}{"query": "$target", "episode_type": "failure"}, OutputKey: "failures", Optional: true},
			},
			Output:              "structured",
			RequiredPermissions: []string{"graph_read"},
		},
		{
			ID:          "impact-audit",
			Description: "Full impact audit for a symbol: downstream effects, violations, and failure episodes.",
			Origin:      "builtin",
			Params: []RecipeParam{
				{Name: "target", Type: "string", Required: true},
				{Name: "depth", Type: "number", Required: false, Default: 3},
			},
			Steps: []RecipeStep{
				{Tool: "get_impact", Args: map[string]interface{}{"symbol": "$target", "depth": "$depth"}, OutputKey: "impact"},
				{Tool: "get_violations", Args: map[string]interface{}{}, OutputKey: "violations", Optional: true},
				{Tool: "recall", Args: map[string]interface{}{"query": "$target", "episode_type": "failure"}, OutputKey: "failures", Optional: true},
			},
			Output:              "structured",
			RequiredPermissions: []string{"graph_read"},
		},
		{
			ID:          "dependency-health",
			Description: "Dependency health check: import context, downstream impact, and failure history.",
			Origin:      "builtin",
			Params: []RecipeParam{
				{Name: "target", Type: "string", Required: true},
			},
			Steps: []RecipeStep{
				{Tool: "get_context", Args: map[string]interface{}{"entity": "$target", "mode": "imports"}, OutputKey: "context"},
				{Tool: "get_impact", Args: map[string]interface{}{"symbol": "$target"}, OutputKey: "impact"},
				{Tool: "recall", Args: map[string]interface{}{"query": "$target", "episode_type": "failure"}, OutputKey: "failures", Optional: true},
			},
			Output:              "structured",
			RequiredPermissions: []string{"graph_read"},
		},
	}
}
