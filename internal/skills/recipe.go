package skills

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RecipeParam describes a named input parameter for a recipe.
type RecipeParam struct {
	Name     string      `json:"name"`
	Type     string      `json:"type"`     // "string" | "number" | "boolean"
	Required bool        `json:"required"`
	Default  interface{} `json:"default,omitempty"`
}

// RecipeStep is one tool invocation within a recipe.
type RecipeStep struct {
	Tool      string                 `json:"tool"`
	Args      map[string]interface{} `json:"args"`
	OutputKey string                 `json:"output_key,omitempty"` // name for $output_key references
	Optional  bool                   `json:"optional,omitempty"`   // if true, step failure doesn't abort
}

// Recipe is a named, composable workflow that calls multiple MCP tools in sequence.
type Recipe struct {
	ID          string        `json:"id"`
	Description string        `json:"description"`
	Origin      string        `json:"origin"` // "builtin" | "user" | "project"
	Params      []RecipeParam `json:"params,omitempty"`
	Steps       []RecipeStep  `json:"steps"`
	Output      string        `json:"output,omitempty"` // "merged" | "structured" (default: "merged")

	// RequiredPermissions lists the permission keys this recipe needs.
	// Checked by SecurityPolicy before execution.
	RequiredPermissions []string `json:"required_permissions,omitempty"`
}

// LoadRecipeDir reads all *.json files from dir, unmarshals them as Recipe structs,
// sets Origin on each, and returns the slice. Returns nil, nil if dir doesn't exist.
func LoadRecipeDir(dir, origin string) ([]Recipe, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("skills.LoadRecipeDir: %w", err)
	}

	var out []Recipe
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue // skip unreadable files silently
		}
		var r Recipe
		if err := json.Unmarshal(data, &r); err != nil {
			continue // skip malformed JSON silently
		}
		r.Origin = origin
		if r.ID == "" {
			r.ID = strings.TrimSuffix(e.Name(), ".json")
		}
		out = append(out, r)
	}
	return out, nil
}

// DeduplicateRecipes removes duplicate IDs. User/project recipes can override
// each other, but builtin IDs are protected — non-builtin recipes that collide
// with a builtin ID are silently dropped to prevent untrusted project recipes
// from shadowing trusted builtins.
func DeduplicateRecipes(recipes []Recipe) []Recipe {
	// First pass: collect builtin IDs.
	builtinIDs := make(map[string]bool)
	for _, r := range recipes {
		if r.ID != "" && r.Origin == string(TrustBuiltin) {
			builtinIDs[r.ID] = true
		}
	}

	// Second pass: keep last occurrence per ID, but skip non-builtin recipes
	// that collide with a builtin ID.
	last := make(map[string]int, len(recipes))
	for i, r := range recipes {
		if r.ID == "" {
			continue
		}
		if builtinIDs[r.ID] && r.Origin != string(TrustBuiltin) {
			continue // non-builtin cannot shadow builtin
		}
		last[r.ID] = i
	}
	out := make([]Recipe, 0, len(recipes))
	for i, r := range recipes {
		if r.ID == "" || last[r.ID] == i {
			out = append(out, r)
		}
	}
	return out
}

// resolveArg substitutes template variables in a single value:
//
//	$param_name   → from params map
//	$prev_result  → from prevResult string
//	$step_KEY     → from stepOutputs map (keyed by OutputKey)
func resolveArg(v interface{}, params map[string]interface{}, prevResult string, stepOutputs map[string]string) interface{} {
	s, ok := v.(string)
	if !ok {
		return v
	}
	if !strings.Contains(s, "$") {
		return v
	}
	// Exact-match fast path: entire string is a single "$varName".
	if strings.HasPrefix(s, "$") && !strings.ContainsAny(s[1:], " $") {
		key := s[1:] // strip leading $
		if key == "prev_result" {
			return prevResult
		}
		if strings.HasPrefix(key, "step_") {
			stepKey := key[5:] // strip "step_"
			if val, ok := stepOutputs[stepKey]; ok {
				return val
			}
		}
		if val, ok := params[key]; ok {
			return val
		}
		return v // unresolved — return original
	}
	// Embedded $param substitution: "prefix $param suffix" → replace all occurrences.
	// Replace longest keys first to prevent "$name" from corrupting "$name_prefix".
	result := s
	result = strings.ReplaceAll(result, "$prev_result", prevResult)

	// Sort step output keys longest-first.
	stepKeys := make([]string, 0, len(stepOutputs))
	for k := range stepOutputs {
		stepKeys = append(stepKeys, k)
	}
	sort.Slice(stepKeys, func(i, j int) bool { return len(stepKeys[i]) > len(stepKeys[j]) })
	for _, k := range stepKeys {
		result = strings.ReplaceAll(result, "$step_"+k, stepOutputs[k])
	}

	// Sort param keys longest-first.
	paramKeys := make([]string, 0, len(params))
	for k := range params {
		paramKeys = append(paramKeys, k)
	}
	sort.Slice(paramKeys, func(i, j int) bool { return len(paramKeys[i]) > len(paramKeys[j]) })
	for _, k := range paramKeys {
		if sv, ok := params[k].(string); ok {
			result = strings.ReplaceAll(result, "$"+k, sv)
		}
	}
	return result
}

// ResolveArgs applies resolveArg to all values in an args map.
func ResolveArgs(args map[string]interface{}, params map[string]interface{}, prevResult string, stepOutputs map[string]string) map[string]interface{} {
	if len(args) == 0 {
		return args
	}
	out := make(map[string]interface{}, len(args))
	for k, v := range args {
		out[k] = resolveArg(v, params, prevResult, stepOutputs)
	}
	return out
}
