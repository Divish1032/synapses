package mcp

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/skills"
)

// registerPrompts registers all loaded prompt templates as MCP Prompts so that
// MCP clients can list and fetch them via the prompts/list and prompts/get
// protocol methods. This exposes the same templates that are auto-injected into
// get_context and session_init responses.
func (s *Server) registerPrompts() {
	for _, pt := range s.promptTemplates {
		pt := pt // capture loop variable
		prompt := mcp.NewPrompt(pt.ID, mcp.WithPromptDescription(pt.Description))
		s.mcp.AddPrompt(prompt, func(_ context.Context, _ mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{
				Description: pt.Description,
				Messages: []mcp.PromptMessage{
					{
						Role:    mcp.RoleUser,
						Content: mcp.TextContent{Type: "text", Text: pt.Body},
					},
				},
			}, nil
		})
	}
}

// getMatchingPrompts returns the prompt templates that match the given entity
// context. Called from handleGetContext to populate dc.ActivePrompts.
func (s *Server) getMatchingPrompts(file, entity, pkg string) []skills.PromptTemplate {
	if len(s.promptTemplates) == 0 {
		return nil
	}
	return skills.MatchPrompts(s.promptTemplates, file, entity, pkg)
}

// getAutoLoadPrompts returns templates marked auto_load: true.
// Called from handleSessionInit to include project-wide conventions.
func (s *Server) getAutoLoadPrompts() []skills.PromptTemplate {
	if len(s.promptTemplates) == 0 {
		return nil
	}
	return skills.AutoLoadPrompts(s.promptTemplates)
}

// SetPromptTemplates wires loaded prompt templates into the server.
// Must be called before the server starts serving requests. In practice,
// called from cmdStartDirect and cmdStartDaemon after loading prompts
// from builtin, user (~/.synapses/prompts/), and project (.synapses/prompts/)
// directories.
func (s *Server) SetPromptTemplates(templates []skills.PromptTemplate) {
	s.promptTemplates = templates
	// Re-register prompts now that templates are available.
	// Note: registerPrompts is also called (with empty templates) in New(),
	// so we call it again here after templates are populated.
	s.registerPrompts()
}

// promptSummaries returns a list of {id, description, source} maps for all
// loaded templates — used in session_init hint text.
func (s *Server) promptSummaries() []map[string]string {
	if len(s.promptTemplates) == 0 {
		return nil
	}
	out := make([]map[string]string, 0, len(s.promptTemplates))
	for _, pt := range s.promptTemplates {
		out = append(out, map[string]string{
			"id":          pt.ID,
			"description": pt.Description,
			"source":      pt.Source,
		})
	}
	return out
}

