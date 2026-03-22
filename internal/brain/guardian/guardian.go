// Package guardian implements the Rule Guardian — Feature 3 of synapses-intelligence.
//
// When Synapses detects an architectural rule violation (e.g., a view component
// importing a database package), the Guardian explains it in plain English and
// suggests a concrete fix. Results are cached in brain.sqlite so the LLM is
// only called once per unique (rule, file) pair.
package guardian

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/SynapsesOS/synapses/internal/brain/llm"
	"github.com/SynapsesOS/synapses/internal/brain/store"
)

const promptTemplate = `Explain this architectural rule violation to a developer. Be direct and actionable.
Output ONLY valid JSON with no other text: {"explanation": "...", "fix": "..."}

Rule violated: %s
Severity: %s
File: %s
This file is importing or calling: %s`

// Request carries a single architectural rule violation.
type Request struct {
	RuleID       string
	RuleSeverity string
	Description  string
	SourceFile   string
	TargetName   string
}

// Response is the plain-English explanation with fix.
type Response struct {
	Explanation string
	Fix         string
}

type violationJSON struct {
	Explanation string `json:"explanation"`
	Fix         string `json:"fix"`
}

// Guardian explains rule violations and caches results.
type Guardian struct {
	llm     llm.LLMClient
	store   *store.Store
	timeout time.Duration
}

// New creates a Guardian.
func New(client llm.LLMClient, st *store.Store, timeout time.Duration) *Guardian {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &Guardian{llm: client, store: st, timeout: timeout}
}

// Explain returns a plain-English explanation and fix for the given violation.
// Results are cached — the LLM is only called once per unique (rule_id, source_file) pair.
func (g *Guardian) Explain(ctx context.Context, req Request) (Response, error) {
	// Fast path: return cached result if available.
	if explanation, fix, ok := g.store.GetViolationExplanation(req.RuleID, req.SourceFile); ok {
		return Response{Explanation: explanation, Fix: fix}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	prompt := g.buildPrompt(req)
	raw, err := g.llm.Generate(ctx, prompt)
	if err != nil {
		return Response{}, fmt.Errorf("llm generate: %w", err)
	}

	result, err := parseViolation(raw)
	if err != nil {
		return Response{}, fmt.Errorf("parse response: %w (raw: %q)", err, llm.Truncate(raw, 100))
	}

	// Cache for future calls with the same (rule, file) pair.
	_ = g.store.UpsertViolationExplanation(
		req.RuleID, req.SourceFile,
		result.Explanation, result.Fix,
	)

	return result, nil
}

func (g *Guardian) buildPrompt(req Request) string {
	severity := req.RuleSeverity
	if severity == "" {
		severity = "warning"
	}
	description := req.Description
	if description == "" {
		description = req.RuleID
	}
	targetName := req.TargetName
	if targetName == "" {
		targetName = "(unknown target)"
	}
	return fmt.Sprintf(promptTemplate, description, severity, stripToRelative(req.SourceFile), targetName)
}

func stripToRelative(filePath string) string {
	for _, marker := range []string{"/internal/", "/cmd/", "/pkg/", "/src/"} {
		if idx := strings.Index(filePath, marker); idx >= 0 {
			return filePath[idx+1:]
		}
	}
	parts := strings.Split(filePath, "/")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return filePath
}

func parseViolation(raw string) (Response, error) {
	raw = llm.ExtractJSON(raw)
	var result violationJSON
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return Response{}, fmt.Errorf("unmarshal: %w", err)
	}
	explanation := strings.TrimSpace(result.Explanation)
	if explanation == "" {
		return Response{}, fmt.Errorf("empty explanation in response")
	}
	return Response{
		Explanation: explanation,
		Fix:         strings.TrimSpace(result.Fix),
	}, nil
}
