package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/skills"
)

// CallTool implements skills.ToolCaller. It dispatches by tool name to the
// corresponding handler on the Server, converting args to a CallToolRequest.
func (s *Server) CallTool(ctx context.Context, toolName string, args map[string]interface{}) (string, error) {
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args

	var result *mcp.CallToolResult
	var err error

	switch toolName {
	case "get_context":
		result, err = s.handleGetContext(ctx, req)
	case "get_impact":
		result, err = s.handleGetImpact(ctx, req)
	case "get_call_chain":
		result, err = s.handleGetCallChain(ctx, req)
	case "get_violations":
		result, err = s.handleGetViolations(ctx, req)
	case "recall":
		result, err = s.handleRecall(ctx, req)
	case "find_entity":
		result, err = s.handleFindEntity(ctx, req)
	case "get_file_context":
		result, err = s.handleGetFileContext(ctx, req)
	case "search":
		result, err = s.handleSearch(ctx, req)
	case "check_plan_safety":
		result, err = s.handleCheckPlanSafety(ctx, req)
	case "validate_plan":
		result, err = s.handleValidatePlan(ctx, req)
	case "get_working_state":
		result, err = s.handleGetWorkingState(ctx, req)
	default:
		return "", fmt.Errorf("skills.CallTool: unknown tool %q", toolName)
	}

	if err != nil {
		return "", fmt.Errorf("skills.CallTool: %s: %w", toolName, err)
	}
	if result == nil {
		return "", nil
	}
	// Extract text from first content block.
	if len(result.Content) > 0 {
		if tc, ok := result.Content[0].(mcp.TextContent); ok {
			// If the result signals an error via IsError, surface it.
			if result.IsError {
				return "", fmt.Errorf("skills.CallTool: %s: %s", toolName, tc.Text)
			}
			return tc.Text, nil
		}
	}
	return "", nil
}

// handleExecuteSkill runs a named recipe and returns the aggregated result.
func (s *Server) handleExecuteSkill(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	skillID, _ := req.GetArguments()["skill_id"].(string)
	if skillID == "" {
		return mcp.NewToolResultError("execute_skill: skill_id is required"), nil
	}

	// Find recipe.
	var found *skills.Recipe
	for i := range s.skillRecipes {
		if s.skillRecipes[i].ID == skillID {
			found = &s.skillRecipes[i]
			break
		}
	}
	if found == nil {
		return mcp.NewToolResultError(fmt.Sprintf("execute_skill: unknown skill %q", skillID)), nil
	}

	// Extract params (optional map).
	params := map[string]interface{}{}
	if p, ok := req.GetArguments()["params"]; ok {
		switch v := p.(type) {
		case map[string]interface{}:
			params = v
		}
	}
	// Also accept flat top-level keys as params (convenience).
	for k, v := range req.GetArguments() {
		if k != "skill_id" && k != "params" {
			if _, already := params[k]; !already {
				params[k] = v
			}
		}
	}

	result, err := s.skillExecutor.Execute(ctx, *found, params)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("execute_skill: %v", err)), nil
	}

	out, err := json.Marshal(result)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("execute_skill: marshal: %v", err)), nil
	}
	return mcp.NewToolResultText(string(out)), nil
}

// handleListSkills returns all available skill recipes (id, description, origin, params).
func (s *Server) handleListSkills(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	type summary struct {
		ID          string               `json:"id"`
		Description string               `json:"description"`
		Origin      string               `json:"origin"`
		Params      []skills.RecipeParam `json:"params,omitempty"`
		Steps       int                  `json:"steps"`
	}

	out := make([]summary, 0, len(s.skillRecipes))
	for _, r := range s.skillRecipes {
		out = append(out, summary{
			ID:          r.ID,
			Description: r.Description,
			Origin:      r.Origin,
			Params:      r.Params,
			Steps:       len(r.Steps),
		})
	}

	b, err := json.Marshal(map[string]interface{}{
		"skills": out,
		"count":  len(out),
	})
	if err != nil {
		return mcp.NewToolResultError("list_skills: marshal failed"), nil
	}
	return mcp.NewToolResultText(string(b)), nil
}

// registerSkillTools wires execute_skill and list_skills into the MCP server.
// Called from New(); handlers are no-ops until SetSkillRecipes populates recipes.
func (s *Server) registerSkillTools() {
	s.mcp.AddTool(
		mcp.NewTool("execute_skill",
			mcp.WithDescription("Execute a named skill recipe that composes multiple tools into a single call. Returns aggregated results from all steps."),
			mcp.WithString("skill_id",
				mcp.Required(),
				mcp.Description("ID of the skill to execute (see list_skills)."),
			),
			mcp.WithObject("params",
				mcp.Description("Parameters for the skill. Keys match the skill's declared params."),
			),
		),
		s.handleExecuteSkill,
	)
	s.mcp.AddTool(
		mcp.NewTool("list_skills",
			mcp.WithDescription("List all available skill recipes with their IDs, descriptions, parameters, and step counts."),
		),
		s.handleListSkills,
	)
}

// SetSkillRecipes wires loaded recipes into the server and creates the Executor.
// Must be called before the server starts serving requests.
func (s *Server) SetSkillRecipes(recipes []skills.Recipe) {
	s.skillRecipes = recipes
	s.skillExecutor = skills.NewExecutor(s)
}
