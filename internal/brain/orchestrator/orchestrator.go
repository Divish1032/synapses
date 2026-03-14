// Package orchestrator implements the Task Orchestrator — Feature 4 of synapses-intelligence.
//
// When two or more agents try to claim overlapping code scopes, the orchestrator
// uses the LLM to suggest an intelligent work distribution: it tells the new agent
// what alternative scope to focus on instead of conflicting with the existing agent.
package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/SynapsesOS/synapses/internal/brain/llm"
)

const promptTemplate = `Two AI agents want to work on overlapping code. Suggest how to divide the work clearly.
Output ONLY valid JSON with no other text: {"suggestion": "...", "alternative_scope": "..."}

New agent wants to work on: %s
Existing conflicts:
%s`

// WorkClaim is an existing agent's active work claim.
type WorkClaim struct {
	AgentID   string
	Scope     string
	ScopeType string
}

// Request describes the conflict to resolve.
type Request struct {
	NewAgentID        string
	NewScope          string
	ConflictingClaims []WorkClaim
}

// Response suggests how the new agent should proceed.
type Response struct {
	Suggestion       string
	AlternativeScope string
}

type coordinateJSON struct {
	Suggestion       string `json:"suggestion"`
	AlternativeScope string `json:"alternative_scope"`
}

// Orchestrator suggests work distribution when agents conflict.
type Orchestrator struct {
	llm     llm.LLMClient
	timeout time.Duration
}

// New creates an Orchestrator.
func New(client llm.LLMClient, timeout time.Duration) *Orchestrator {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &Orchestrator{llm: client, timeout: timeout}
}

// Coordinate suggests how to resolve conflicting work claims.
// If only a single conflict exists, the suggestion is specific and actionable.
// Results are NOT cached since agent states change rapidly.
func (o *Orchestrator) Coordinate(ctx context.Context, req Request) (Response, error) {
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()

	prompt := o.buildPrompt(req)
	raw, err := o.llm.Generate(ctx, prompt)
	if err != nil {
		return Response{}, fmt.Errorf("llm generate: %w", err)
	}

	result, err := parseCoordinate(raw)
	if err != nil {
		// Fallback: produce a basic non-LLM response.
		return o.fallbackResponse(req), nil
	}
	return result, nil
}

func (o *Orchestrator) buildPrompt(req Request) string {
	var sb strings.Builder
	for _, c := range req.ConflictingClaims {
		fmt.Fprintf(&sb, "  - Agent %q owns %q (%s)\n", c.AgentID, c.Scope, c.ScopeType)
	}
	conflicts := sb.String()
	if conflicts == "" {
		conflicts = "  (none)\n"
	}
	return fmt.Sprintf(promptTemplate, req.NewScope, conflicts)
}

func (o *Orchestrator) fallbackResponse(req Request) Response {
	if len(req.ConflictingClaims) == 0 {
		return Response{
			Suggestion:       fmt.Sprintf("No conflicts detected. You can safely claim %q.", req.NewScope),
			AlternativeScope: req.NewScope,
		}
	}
	c := req.ConflictingClaims[0]
	return Response{
		Suggestion: fmt.Sprintf(
			"Agent %q is already working on %q. Consider a different scope to avoid conflicts.",
			c.AgentID, c.Scope,
		),
		AlternativeScope: "",
	}
}

func parseCoordinate(raw string) (Response, error) {
	raw = llm.ExtractJSON(raw)
	var result coordinateJSON
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return Response{}, fmt.Errorf("unmarshal: %w", err)
	}
	suggestion := strings.TrimSpace(result.Suggestion)
	if suggestion == "" {
		return Response{}, fmt.Errorf("empty suggestion in response")
	}
	return Response{
		Suggestion:       suggestion,
		AlternativeScope: strings.TrimSpace(result.AlternativeScope),
	}, nil
}
