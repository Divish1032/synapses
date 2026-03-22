// Package ingestor implements the Semantic Ingestor — Feature 1 of synapses-intelligence.
//
// On a file-save event, the ingestor receives a code snippet, sends a short prompt
// to the local LLM, and persists a 1-sentence "intent summary" in brain.sqlite.
// These summaries are later served by the Enricher during get_context calls,
// replacing raw source code with compact semantic descriptions for the main LLM.
package ingestor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/SynapsesOS/synapses/internal/brain/llm"
	"github.com/SynapsesOS/synapses/internal/brain/store"
	"github.com/SynapsesOS/synapses/internal/secrets"
)

const (
	// maxCodeChars is the maximum code snippet size sent to the LLM.
	// Keeps prompts small for fast inference on 0.8-2B models.
	maxCodeChars = 500

	// promptTemplate generates a prose briefing suitable for LLM context delivery.
	// 2-3 sentences covering: what it does, its role, and any important concerns.
	// The summary replaces verbose raw code/doc in get_context responses, giving
	// Claude natural-language context that costs far fewer tokens than JSON.
	promptTemplate = `Write a 2-3 sentence technical briefing for this code entity: what it does, its role in the system, and any important patterns or concerns to be aware of.
Do not write code. Describe the entity in plain English sentences only.
Output ONLY valid JSON with no other text: {"summary": "...", "tags": ["tag1"]}

Name: %s (%s, package %s)
Code:
%s`
)

// Request carries a code snippet for summarization.
type Request struct {
	ProjectID string
	NodeID    string
	NodeName  string
	NodeType  string
	Package   string
	Code      string
}

// Response holds the generated summary and domain tags.
type Response struct {
	NodeID  string
	Summary string
	Tags    []string // 1-3 short domain labels (may be empty for legacy LLM responses)
}

// summaryJSON is used to parse the LLM's JSON output.
type summaryJSON struct {
	Summary string   `json:"summary"`
	Tags    []string `json:"tags,omitempty"`
}

// Ingestor summarizes code snippets via the LLM and persists them.
type Ingestor struct {
	llm     llm.LLMClient
	store   *store.Store
	timeout time.Duration
}

// New creates an Ingestor backed by the given LLM client and store.
func New(client llm.LLMClient, st *store.Store, timeout time.Duration) *Ingestor {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &Ingestor{llm: client, store: st, timeout: timeout}
}

// deterministicSummary returns a summary and tags for trivial nodes that don't
// need LLM analysis — test helpers, generated code, trivial getters/setters.
// Returns ("", nil, false) when the node is non-trivial and needs LLM.
// This fast path avoids ~900ms per node for the ~60% of entities that are trivial.
func deterministicSummary(req Request) (string, []string, bool) {
	name := strings.ToLower(req.NodeName)
	nodeType := strings.ToLower(req.NodeType)
	pkg := strings.ToLower(req.Package)
	code := strings.ToLower(req.Code)

	// Test helpers: *_test.go entities, Test* functions, mock* types
	if strings.HasSuffix(pkg, "_test") || strings.HasPrefix(name, "test") ||
		strings.HasPrefix(name, "mock") || strings.HasPrefix(name, "fake") ||
		strings.HasPrefix(name, "stub") {
		return fmt.Sprintf("Test helper %s in package %s.", req.NodeName, req.Package),
			[]string{"test"}, true
	}

	// Generated code markers
	if strings.Contains(code, "do not edit") || strings.Contains(code, "code generated") ||
		strings.Contains(code, "auto-generated") {
		return fmt.Sprintf("Generated code: %s in package %s.", req.NodeName, req.Package),
			[]string{"generated"}, true
	}

	// Trivial getters/setters: short functions with simple bodies
	if (nodeType == "function" || nodeType == "method") && len(req.Code) < 80 {
		if strings.HasPrefix(name, "get") || strings.HasPrefix(name, "set") ||
			strings.HasPrefix(name, "is") || strings.HasPrefix(name, "has") {
			return fmt.Sprintf("Accessor %s in package %s.", req.NodeName, req.Package),
				[]string{"util"}, true
		}
	}

	// init() functions
	if name == "init" {
		return fmt.Sprintf("Package initializer for %s.", req.Package),
			[]string{"config"}, true
	}

	return "", nil, false
}

// Summarize generates and stores a 1-sentence summary for the given code entity.
// Uses a deterministic fast path for trivial nodes (test helpers, generated code,
// getters/setters) that skips the LLM entirely. For non-trivial nodes, calls the
// LLM to generate a prose summary.
// If the LLM is unavailable or returns an unparseable response, an error is returned
// but the call is non-fatal — callers should log and continue.
func (ing *Ingestor) Summarize(ctx context.Context, req Request) (Response, error) {
	// Fast path: skip LLM for trivial nodes.
	if summary, tags, ok := deterministicSummary(req); ok {
		if err := ing.store.UpsertSummary(req.ProjectID, req.NodeID, req.NodeName, summary, tags); err != nil {
			return Response{NodeID: req.NodeID, Summary: summary, Tags: tags},
				fmt.Errorf("store summary: %w", err)
		}
		return Response{NodeID: req.NodeID, Summary: summary, Tags: tags}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, ing.timeout)
	defer cancel()

	prompt := ing.buildPrompt(req)

	raw, err := ing.llm.Generate(ctx, prompt)
	if err != nil {
		return Response{NodeID: req.NodeID}, fmt.Errorf("llm generate: %w", err)
	}

	summary, tags, err := parseSummary(raw)
	if err != nil {
		return Response{NodeID: req.NodeID}, fmt.Errorf("parse summary: %w (raw: %q)", err, llm.Truncate(raw, 100))
	}

	if err := ing.store.UpsertSummary(req.ProjectID, req.NodeID, req.NodeName, summary, tags); err != nil {
		return Response{NodeID: req.NodeID, Summary: summary, Tags: tags},
			fmt.Errorf("store summary: %w", err)
	}

	return Response{NodeID: req.NodeID, Summary: summary, Tags: tags}, nil
}

// buildPrompt constructs the LLM prompt for a code entity.
func (ing *Ingestor) buildPrompt(req Request) string {
	code := truncateCode(secrets.FilterLines(req.Code))
	nodeType := req.NodeType
	if nodeType == "" {
		nodeType = "entity"
	}
	pkg := req.Package
	if pkg == "" {
		pkg = "unknown"
	}
	return fmt.Sprintf(promptTemplate, sanitizePromptInput(req.NodeName), sanitizePromptInput(nodeType), sanitizePromptInput(pkg), code)
}


// parseSummary extracts the summary and tags from the LLM JSON response.
// Handles cases where the model wraps the JSON in markdown code fences.
// Falls back to treating the full response as a plain-text summary when
// the model ignores the JSON format instruction (common with small models).
// Tags are optional — returns nil if the model did not include them.
func parseSummary(raw string) (summary string, tags []string, err error) {
	extracted := llm.ExtractJSON(raw)
	var result summaryJSON
	if jsonErr := json.Unmarshal([]byte(extracted), &result); jsonErr == nil {
		summary = strings.TrimSpace(result.Summary)
		if summary != "" {
			if looksLikeCode(summary) {
				return "", nil, fmt.Errorf("LLM returned code instead of prose summary")
			}
			return summary, result.Tags, nil
		}
	}
	// JSON parse failed or summary field was empty — use raw text as the summary.
	// Strip any markdown fences and collapse whitespace.
	fallback := strings.TrimSpace(raw)
	fallback = strings.TrimPrefix(fallback, "```")
	fallback = strings.TrimSuffix(fallback, "```")
	fallback = strings.TrimSpace(fallback)
	if fallback == "" {
		return "", nil, fmt.Errorf("empty response from LLM")
	}
	if looksLikeCode(fallback) {
		return "", nil, fmt.Errorf("LLM returned code instead of prose summary")
	}
	// Limit to first 300 chars to keep summaries concise.
	if len(fallback) > 300 {
		fallback = fallback[:300] + "…"
	}
	return fallback, nil, nil
}

// looksLikeCode returns true when s appears to be source code rather than a
// prose description. It checks for patterns that are unambiguous code markers
// across the languages most likely to appear in LLM hallucinations:
//
//	Go:         ":="  |  "func "+"{" |  " struct {" |  "interface {"
//	Python:     "def "+"("+"):"
//	JavaScript: ") => "  |  " => {"  |  "function("  |  "function ("
func looksLikeCode(s string) bool {
	// Go: short variable declaration — never in English prose.
	if strings.Contains(s, ":=") {
		return true
	}
	// Go: function definition with body.
	if strings.Contains(s, "func ") && strings.Contains(s, "{") {
		return true
	}
	// Go: type/struct declaration.
	if strings.Contains(s, " struct {") {
		return true
	}
	// Go/TS: interface block.
	if strings.Contains(s, "interface {") {
		return true
	}
	// Python: function definition  "def foo(args):"
	if strings.Contains(s, "def ") && strings.Contains(s, "(") && strings.Contains(s, "):") {
		return true
	}
	// JS/TS: arrow function body or return type.
	if strings.Contains(s, " => {") || strings.Contains(s, ") => ") {
		return true
	}
	// JS/TS: function keyword with parens (avoids false-positive on plain "function").
	if strings.Contains(s, "function(") || strings.Contains(s, "function (") {
		return true
	}
	return false
}

// sanitizePromptInput escapes angle brackets to prevent prompt injection.
func sanitizePromptInput(s string) string {
	r := strings.NewReplacer("<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// truncateCode caps the code snippet at maxCodeChars runes.
func truncateCode(code string) string {
	code = strings.TrimSpace(code)
	if utf8.RuneCountInString(code) <= maxCodeChars {
		return code
	}
	runes := []rune(code)
	return string(runes[:maxCodeChars]) + "..."
}
