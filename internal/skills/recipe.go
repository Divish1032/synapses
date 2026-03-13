package skills

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	if !strings.HasPrefix(s, "$") {
		return v
	}
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
