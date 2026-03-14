// Package enricher implements the Context Enricher — Feature 2 of synapses-intelligence.
//
// During a get_context call, the enricher:
//  1. Loads pre-computed summaries from brain.sqlite (fast, no LLM)
//  2. Optionally generates a 2-sentence insight about the root entity's architectural role
//
// The summaries replace raw code in get_context responses, dramatically reducing
// token usage for the main LLM (Claude, Gemini, GPT, etc.).
package enricher

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/SynapsesOS/synapses/internal/brain/llm"
	"github.com/SynapsesOS/synapses/internal/brain/store"
)

// domainFocusMap maps file path substrings to domain-specific focus lines.
// When a root file path contains one of these substrings, the corresponding focus
// line is appended to the enricher prompt so the LLM applies domain expertise.
var domainFocusMap = []struct {
	pattern string
	focus   string
}{
	{"internal/parser/", "Focus on: AST correctness, language quirks, tree-sitter query patterns, edge cases in public/private detection."},
	{"internal/mcp/", "Focus on: tool contract (fail-silent), handler latency, context.WithTimeout usage, JSON serialization correctness."},
	{"internal/graph/", "Focus on: BFS correctness, edge type semantics, complexity invariants, memory efficiency."},
	{"internal/store/", "Focus on: SQL correctness, migration safety, FTS5 index, CGo-free driver constraints."},
	{"internal/brain/", "Focus on: HTTP timeout handling, fail-silent pattern, client retry, interface contract."},
	{"internal/scout/", "Focus on: HTTP timeout handling, fail-silent pattern, client retry, interface contract."},
}

const (
	// maxNamesInPrompt limits how many callee/caller names are sent to the LLM.
	// 10 is appropriate for 7b models; reduce to 5 for 1-2b models.
	maxNamesInPrompt = 10

	promptTemplate = `You are a code architecture analyst. Analyze this code entity and provide:
1. A precise 2-sentence description of its architectural role and responsibility
2. 1-3 specific concerns an engineer should know before modifying it

Output ONLY valid JSON: {"insight": "...", "concerns": ["...", "..."]}

Entity: %s (%s)
Calls: %s
Called by: %s%s

Focus on: architectural patterns, coupling risks, invariants, and design intent.`
)

// Request carries the carved subgraph data for enrichment.
type Request struct {
	RootID       string
	RootName     string
	RootType     string
	RootFile     string // file path of the root entity; used for domain detection
	CalleeNames  []string
	CallerNames  []string
	RelatedNames []string
	TaskContext  string
}

// Response is added to the get_context output.
type Response struct {
	RootSummary string // populated by the SIL model (ROOT_SUMMARY: label)
	Insight     string
	Concerns    []string
	LLMUsed     bool // true when the LLM was called; false on cache hit (future)
}

type insightJSON struct {
	Insight  string   `json:"insight"`
	Concerns []string `json:"concerns"`
}

// silGraphNode is a node in the SIL graph JSON format the model was trained on.
type silGraphNode struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Package string `json:"package,omitempty"`
	File    string `json:"file,omitempty"`
	Line    int    `json:"line"`
}

// silGraphEdge is an edge in the SIL graph JSON format.
type silGraphEdge struct {
	From int    `json:"from"`
	To   int    `json:"to"`
	Type string `json:"type"`
}

// silGraphPacket is the top-level object the SIL model receives as input.
type silGraphPacket struct {
	Nodes    []silGraphNode `json:"nodes"`
	Edges    []silGraphEdge `json:"edges"`
	Language string         `json:"language"`
}

// Enricher adds semantic context to get_context responses.
type Enricher struct {
	llm     llm.LLMClient
	store   *store.Store
	timeout time.Duration
	silMode bool // when true, builds graph JSON prompts for the SIL fine-tuned model
}

// New creates an Enricher.
func New(client llm.LLMClient, st *store.Store, timeout time.Duration) *Enricher {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &Enricher{llm: client, store: st, timeout: timeout}
}

// WithSILMode configures the enricher to build "Graph: {json}" prompts suited
// to the fine-tuned SIL model rather than the generic JSON-output Ollama prompt.
// Call this when brain config backend == "local".
func (e *Enricher) WithSILMode() *Enricher {
	e.silMode = true
	return e
}

// Enrich generates a 2-sentence insight for the root entity.
// This calls the LLM; callers should handle errors gracefully (fail-silent).
func (e *Enricher) Enrich(ctx context.Context, req Request) (Response, error) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	var prompt string
	if e.silMode {
		// SIL model path: send graph JSON format that matches training data.
		// The SIL system prompt (baked into the GGUF) handles the output schema.
		prompt = e.buildSILPrompt(req)
	} else {
		// Generic Ollama path: structured prose prompt asking for JSON output.
		prompt = e.buildPrompt(req)
	}
	raw, err := e.llm.Generate(ctx, prompt)
	if err != nil {
		return Response{}, fmt.Errorf("llm generate: %w", err)
	}

	result, err := parseInsight(raw)
	if err != nil {
		return Response{}, fmt.Errorf("parse insight: %w (raw: %q)", err, llm.Truncate(raw, 100))
	}

	return result, nil
}

func (e *Enricher) buildPrompt(req Request) string {
	callees := joinNames(req.CalleeNames, maxNamesInPrompt)
	callers := joinNames(req.CallerNames, maxNamesInPrompt)
	nodeType := req.RootType
	if nodeType == "" {
		nodeType = "entity"
	}
	if callees == "" {
		callees = "none"
	}
	if callers == "" {
		callers = "none"
	}

	taskSection := ""
	if req.TaskContext != "" {
		taskSection = "\nTask context: " + req.TaskContext
	}

	domainSection := ""
	if focus := detectDomain(req.RootFile); focus != "" {
		domainSection = "\n" + focus
	}

	return fmt.Sprintf(promptTemplate,
		req.RootName, nodeType,
		callees, callers,
		taskSection+domainSection,
	)
}

// detectDomain returns a domain-specific focus line for the given file path,
// or "" if no domain pattern matches.
func detectDomain(filePath string) string {
	for _, d := range domainFocusMap {
		if strings.Contains(filePath, d.pattern) {
			return d.focus
		}
	}
	return ""
}

func parseInsight(raw string) (Response, error) {
	// First try the SIL labeled format (ROOT_SUMMARY: / INSIGHT: / CONCERNS:).
	// ParseSILResponse also strips <think> blocks.
	rootSummary, insight, concerns := llm.ParseSILResponse(raw)
	if insight != "" || rootSummary != "" {
		return Response{
			RootSummary: rootSummary,
			Insight:     insight,
			Concerns:    concerns,
			LLMUsed:     true,
		}, nil
	}

	// Fall back to JSON format {"insight": "...", "concerns": [...]}
	// for backward compatibility with standard Ollama models.
	extracted := llm.ExtractJSON(raw)
	var result insightJSON
	if jsonErr := json.Unmarshal([]byte(extracted), &result); jsonErr == nil {
		if txt := strings.TrimSpace(result.Insight); txt != "" {
			return Response{Insight: txt, Concerns: result.Concerns, LLMUsed: true}, nil
		}
	}

	// Last resort: treat the whole response as raw insight text.
	fallback := strings.TrimSpace(raw)
	fallback = strings.TrimPrefix(fallback, "```")
	fallback = strings.TrimSuffix(fallback, "```")
	fallback = strings.TrimSpace(fallback)
	if fallback == "" {
		return Response{}, fmt.Errorf("empty response from LLM")
	}
	if len(fallback) > 400 {
		fallback = fallback[:400] + "…"
	}
	return Response{Insight: fallback, LLMUsed: true}, nil
}

// joinNames joins up to n names into a comma-separated string.
func joinNames(names []string, n int) string {
	if len(names) > n {
		names = names[:n]
	}
	return strings.Join(names, ", ")
}

// ---------------------------------------------------------------------------
// SIL graph JSON prompt builder
// ---------------------------------------------------------------------------

// buildSILPrompt converts a Request into the "Graph: {json}" format the SIL
// fine-tuned model was trained on.
//
// Node layout:
//   - id=0 : root entity (BFS root; matches training data convention)
//   - id=1..N : callees (edges: root → callee, type=CALLS)
//   - id=N+1..M : callers (edges: caller → root, type=CALLS)
//
// The SIL model's system prompt (embedded in the GGUF) instructs it to
// output ROOT_SUMMARY / INSIGHT / CONCERNS labels from this graph JSON.
func (e *Enricher) buildSILPrompt(req Request) string {
	nodes := []silGraphNode{{
		ID:      0,
		Name:    req.RootName,
		Type:    nodeTypeOrDefault(req.RootType),
		Package: packageFromFile(req.RootFile),
		File:    req.RootFile,
		Line:    0,
	}}
	edges := []silGraphEdge{}

	id := 1
	for i, name := range req.CalleeNames {
		if i >= maxNamesInPrompt {
			break
		}
		nodes = append(nodes, silGraphNode{ID: id, Name: name, Type: "function"})
		edges = append(edges, silGraphEdge{From: 0, To: id, Type: "CALLS"})
		id++
	}
	for i, name := range req.CallerNames {
		if i >= maxNamesInPrompt {
			break
		}
		nodes = append(nodes, silGraphNode{ID: id, Name: name, Type: "function"})
		edges = append(edges, silGraphEdge{From: id, To: 0, Type: "CALLS"})
		id++
	}

	packet := silGraphPacket{
		Nodes:    nodes,
		Edges:    edges,
		Language: languageFromFile(req.RootFile),
	}
	data, _ := json.Marshal(packet)
	return "Graph: " + string(data)
}

// languageFromFile infers the programming language from a file path extension.
func languageFromFile(filePath string) string {
	switch strings.ToLower(filepath.Ext(filePath)) {
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx":
		return "javascript"
	case ".rs":
		return "rust"
	case ".java":
		return "java"
	case ".rb":
		return "ruby"
	default:
		return "unknown"
	}
}

// packageFromFile extracts the immediate parent directory name from a file path.
// "internal/auth/service.go" → "auth"
func packageFromFile(filePath string) string {
	return filepath.Base(filepath.Dir(filePath))
}

// nodeTypeOrDefault returns t if non-empty, otherwise "function".
func nodeTypeOrDefault(t string) string {
	if t == "" {
		return "function"
	}
	return t
}
