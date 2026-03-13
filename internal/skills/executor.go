package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ToolCaller is implemented by the MCP server to allow the Executor to call
// existing tool handlers without going over the wire.
type ToolCaller interface {
	CallTool(ctx context.Context, toolName string, args map[string]interface{}) (string, error)
}

// StepResult holds the output of a single recipe step.
type StepResult struct {
	Tool      string `json:"tool"`
	OutputKey string `json:"output_key,omitempty"`
	Output    string `json:"output"`
	Skipped   bool   `json:"skipped,omitempty"` // true when optional step failed
	Error     string `json:"error,omitempty"`
}

// ExecutionResult is the aggregate output of a recipe run.
type ExecutionResult struct {
	RecipeID     string                 `json:"recipe_id"`
	Steps        []StepResult           `json:"steps"`
	MergedOutput string                 `json:"merged_output,omitempty"`   // when Output == "merged"
	Structured   map[string]interface{} `json:"structured,omitempty"`      // when Output == "structured"
	TotalMs      int64                  `json:"total_ms"`
	Degraded     bool                   `json:"degraded,omitempty"` // true if any non-optional step was skipped
}

// Executor runs Recipe definitions by dispatching steps to a ToolCaller.
type Executor struct {
	caller ToolCaller
}

// NewExecutor creates an Executor backed by the given ToolCaller.
func NewExecutor(caller ToolCaller) *Executor {
	return &Executor{caller: caller}
}

// Execute runs recipe r with the given params.
// Required params that are missing and have no Default cause an error.
// Optional steps that fail are recorded with Skipped=true; non-optional step failures abort.
func (e *Executor) Execute(ctx context.Context, r Recipe, params map[string]interface{}) (*ExecutionResult, error) {
	// Validate and apply defaults for params.
	resolved := make(map[string]interface{}, len(r.Params))
	for _, p := range r.Params {
		v, ok := params[p.Name]
		if !ok {
			if p.Required && p.Default == nil {
				return nil, fmt.Errorf("skills.Executor: recipe %q missing required param %q", r.ID, p.Name)
			}
			if p.Default != nil {
				resolved[p.Name] = p.Default
			}
		} else {
			resolved[p.Name] = v
		}
	}
	// Also pass through any extra caller-supplied params (useful for dynamic recipes).
	for k, v := range params {
		if _, already := resolved[k]; !already {
			resolved[k] = v
		}
	}

	start := time.Now()
	result := &ExecutionResult{
		RecipeID:   r.ID,
		Steps:      make([]StepResult, 0, len(r.Steps)),
		Structured: make(map[string]interface{}),
	}

	prevResult := ""
	stepOutputs := make(map[string]string) // keyed by Step.OutputKey

	for _, step := range r.Steps {
		args := ResolveArgs(step.Args, resolved, prevResult, stepOutputs)

		out, err := e.caller.CallTool(ctx, step.Tool, args)
		sr := StepResult{Tool: step.Tool, OutputKey: step.OutputKey}
		if err != nil {
			if step.Optional {
				sr.Skipped = true
				sr.Error = err.Error()
				result.Steps = append(result.Steps, sr)
				continue
			}
			return nil, fmt.Errorf("skills.Executor: step %q in recipe %q failed: %w", step.Tool, r.ID, err)
		}

		sr.Output = out
		result.Steps = append(result.Steps, sr)
		prevResult = out
		if step.OutputKey != "" {
			stepOutputs[step.OutputKey] = out
		}
	}

	result.TotalMs = time.Since(start).Milliseconds()

	// Check degraded: any step that was skipped but wasn't optional at the param level
	// (already handled above; here we flag if any step was skipped at all).
	for _, sr := range result.Steps {
		if sr.Skipped {
			result.Degraded = true
			break
		}
	}

	outputMode := r.Output
	if outputMode == "" {
		outputMode = "merged"
	}

	switch outputMode {
	case "structured":
		for _, sr := range result.Steps {
			if sr.OutputKey != "" && !sr.Skipped {
				// Try to unmarshal as JSON; fall back to raw string.
				var v interface{}
				if json.Unmarshal([]byte(sr.Output), &v) == nil {
					result.Structured[sr.OutputKey] = v
				} else {
					result.Structured[sr.OutputKey] = sr.Output
				}
			}
		}
	default: // "merged"
		var parts []string
		for _, sr := range result.Steps {
			if !sr.Skipped && sr.Output != "" {
				parts = append(parts, sr.Output)
			}
		}
		result.MergedOutput = strings.Join(parts, "\n\n---\n\n")
	}

	return result, nil
}
