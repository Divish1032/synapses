// Package agent provides the HTTP client that calls a running Synapses daemon
// via its REST transport (/v1/tools/{tool}?project=...).
//
// The REST protocol is:
//
//	POST /v1/tools/{tool_name}?project={absolute_project_path}
//	Content-Type: application/json
//	Body: JSON object of tool arguments
//
//	Response 200: {"content": [{"type":"text","text":"..."}], ...}
//	Response 4xx/5xx: {"error": "..."}
//
// All tool calls are logged for Context F1 tracking (ContextBench).
package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// markdownFileLineRe matches patterns like "path/to/file.go:123" or
// "path/to/file.py:45" in Markdown/text tool responses (prepare_context, recall).
// Hyphen is placed first in the character class to avoid range interpretation.
var markdownFileLineRe = regexp.MustCompile(`([-\w./]+\.\w+):(\d+)`)

// ContextAccess records a single file+line-range access made via a Synapses tool.
// Used for ContextBench Context F1 calculation.
type ContextAccess struct {
	TaskID    string
	File      string
	LineStart int
	LineEnd   int
	Tool      string // "prepare_context", "search", "get_impact", "recall"
	Timestamp time.Time
}

// SearchResult is returned by the Search tool.
type SearchResult struct {
	Raw  string
	Text string
}

// ContextResult is returned by PrepareContext.
type ContextResult struct {
	Raw  string
	Text string
}

// ImpactResult is returned by GetImpact.
type ImpactResult struct {
	Raw  string
	Text string
}

// RecallResult is returned by Recall.
type RecallResult struct {
	Raw  string
	Text string
}

// SynapsesClient calls a running Synapses daemon over HTTP.
// Set disabled=true for a control run where all tools return empty results.
type SynapsesClient struct {
	endpoint  string
	project   string
	authToken string
	disabled  bool

	httpClient *http.Client

	mu       sync.Mutex
	accesses []ContextAccess
}

// NewClient creates a live client that calls the Synapses daemon.
// authToken is optional — read from ~/.synapses/auth_token when the daemon
// requires Bearer auth.
func NewClient(endpoint, project string) *SynapsesClient {
	return &SynapsesClient{
		endpoint:   strings.TrimRight(endpoint, "/"),
		project:    project,
		authToken:  readAuthToken(),
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// readAuthToken reads the daemon auth token from ~/.synapses/auth_token.
func readAuthToken() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".synapses", "auth_token"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// NewDisabledClient creates a no-op client for control runs.
// All tool calls succeed but return empty results.
func NewDisabledClient() *SynapsesClient {
	return &SynapsesClient{disabled: true}
}

// WithProject returns a shallow copy of the client with a different project path.
// Used for per-repo routing in RepoBench-R: each sample gets routed to its own
// indexed project directory.
func (c *SynapsesClient) WithProject(project string) *SynapsesClient {
	copy := *c
	copy.project = project
	return &copy
}

// PrepareContext calls get_context with mode=intent (was prepare_context before Sprint 24).
func (c *SynapsesClient) PrepareContext(taskID, entity, intent string) (*ContextResult, error) {
	if c.disabled {
		return &ContextResult{}, nil
	}
	args := map[string]interface{}{
		"entity": entity,
		"intent": intent,
		"mode":   "intent",
	}
	raw, err := c.callTool("get_context", args)
	if err != nil {
		return nil, err
	}
	c.recordAccess(taskID, "get_context", raw)
	return &ContextResult{Raw: raw, Text: raw}, nil
}

// Search calls the search tool.
func (c *SynapsesClient) Search(taskID, query string) (*SearchResult, error) {
	if c.disabled {
		return &SearchResult{}, nil
	}
	args := map[string]interface{}{"query": query}
	raw, err := c.callTool("search", args)
	if err != nil {
		return nil, err
	}
	c.recordAccess(taskID, "search", raw)
	return &SearchResult{Raw: raw, Text: raw}, nil
}

// SearchWithMode calls the search tool with an explicit mode (e.g. "vector").
func (c *SynapsesClient) SearchWithMode(query, mode string) (string, error) {
	if c.disabled {
		return "", nil
	}
	args := map[string]interface{}{"query": query, "mode": mode}
	return c.callTool("search", args)
}

// GetImpact calls the get_impact tool.
func (c *SynapsesClient) GetImpact(taskID, entity string) (*ImpactResult, error) {
	if c.disabled {
		return &ImpactResult{}, nil
	}
	args := map[string]interface{}{"symbol": entity}
	raw, err := c.callTool("get_impact", args)
	if err != nil {
		return nil, err
	}
	c.recordAccess(taskID, "get_impact", raw)
	return &ImpactResult{Raw: raw, Text: raw}, nil
}

// GetContextJSON calls get_context with format=json, returning the raw JSON string.
// This gives structured callees, callers, related nodes — far more reliable than
// regex-parsing Markdown from prepare_context.
func (c *SynapsesClient) GetContextJSON(taskID, entity, detailLevel string) (string, error) {
	if c.disabled {
		return "{}", nil
	}
	args := map[string]interface{}{
		"entity":       entity,
		"format":       "json",
		"detail_level": detailLevel,
	}
	raw, err := c.callTool("get_context", args)
	if err != nil {
		return "", err
	}
	c.recordAccess(taskID, "get_context", raw)
	return raw, nil
}

// GetImpactWithDepth calls get_impact with an explicit depth parameter.
func (c *SynapsesClient) GetImpactWithDepth(taskID, entity string, depth int) (*ImpactResult, error) {
	if c.disabled {
		return &ImpactResult{}, nil
	}
	args := map[string]interface{}{"symbol": entity, "depth": float64(depth)}
	raw, err := c.callTool("get_impact", args)
	if err != nil {
		return nil, err
	}
	c.recordAccess(taskID, "get_impact", raw)
	return &ImpactResult{Raw: raw, Text: raw}, nil
}

// Recall calls the recall tool.
// Recall calls memory(action=search) (was recall before Sprint 24).
func (c *SynapsesClient) Recall(taskID, query string) (*RecallResult, error) {
	if c.disabled {
		return &RecallResult{}, nil
	}
	args := map[string]interface{}{"action": "search", "query": query}
	raw, err := c.callTool("memory", args)
	if err != nil {
		return nil, err
	}
	c.recordAccess(taskID, "memory", raw)
	return &RecallResult{Raw: raw, Text: raw}, nil
}

// EmbedSnippets calls the Synapses embedder via the embed_snippet tool (if
// available) or falls back to requesting the daemon to rank a list of candidate
// snippets against a query. Used by RepoBench-R (Approach B: embedding-based
// ranking without needing a live repo index).
//
// Returns a ranked list of (index, score) pairs, highest score first.
func (c *SynapsesClient) RankCandidates(query string, candidates []string) ([]RankedCandidate, error) {
	if c.disabled {
		// Return identity order.
		out := make([]RankedCandidate, len(candidates))
		for i := range candidates {
			out[i] = RankedCandidate{Index: i, Score: 0}
		}
		return out, nil
	}

	args := map[string]interface{}{
		"query":      query,
		"candidates": candidates,
	}
	raw, err := c.callTool("rank_candidates", args)
	if err != nil {
		return nil, fmt.Errorf("rank_candidates: %w", err)
	}

	// Parse ranked list from response.
	return parseRankedCandidates(raw, len(candidates))
}

// RankedCandidate is one entry in a ranked candidate list.
type RankedCandidate struct {
	Index int     // original index in the candidates slice
	Score float64 // higher = more relevant
}

// DrainAccesses returns all recorded context accesses and clears the log.
func (c *SynapsesClient) DrainAccesses() []ContextAccess {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.accesses
	c.accesses = nil
	return out
}

// ─── internals ───────────────────────────────────────────────────────────────

// callTool sends a POST to /v1/tools/{tool}?project=... and returns the
// concatenated text content from the response.
func (c *SynapsesClient) callTool(tool string, args map[string]interface{}) (string, error) {
	body, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("marshal args: %w", err)
	}

	u := fmt.Sprintf("%s/v1/tools/%s", c.endpoint, tool)
	if c.project != "" {
		u += "?project=" + url.QueryEscape(c.project)
	}

	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}

	// Retry loop for rate limiting (429) with exponential backoff.
	var respBody []byte
	for attempt := 0; attempt < 5; attempt++ {
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("http post %s: %w", tool, err)
		}

		respBody, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return "", fmt.Errorf("read response: %w", err)
		}

		if resp.StatusCode == 429 {
			wait := time.Duration(1<<uint(attempt)) * time.Second
			time.Sleep(wait)
			// Rebuild request body reader for retry.
			req, _ = http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if c.authToken != "" {
				req.Header.Set("Authorization", "Bearer "+c.authToken)
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("tool %s returned %d: %s", tool, resp.StatusCode, string(respBody))
		}

		return extractText(respBody)
	}
	return "", fmt.Errorf("tool %s: rate limited after 5 retries", tool)
}

// extractText parses the MCP CallToolResult JSON and returns concatenated text.
// The response shape is: {"content": [{"type":"text","text":"..."},...], ...}
func extractText(body []byte) (string, error) {
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		// Not JSON or unexpected shape — return raw string.
		return string(body), nil
	}
	if result.Error != "" {
		return "", fmt.Errorf("tool error: %s", result.Error)
	}
	var sb strings.Builder
	for _, c := range result.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	return sb.String(), nil
}

// recordAccess parses the tool response and appends one ContextAccess per
// file+line block extracted. Falls back to a single record with no file/line
// when parsing yields nothing (ensures DrainAccesses always sees a record).
func (c *SynapsesClient) recordAccess(taskID, tool, responseText string) {
	accesses := extractContextAccesses(taskID, tool, responseText)
	c.mu.Lock()
	c.accesses = append(c.accesses, accesses...)
	c.mu.Unlock()
}

// extractContextAccesses parses a tool response and returns one ContextAccess
// per file+line hit. Three parsers cover the actual Synapses response schemas:
//
//   - search:          JSON {"results":[{"file":"...","line":N,...},...]}
//   - get_impact:      JSON {"tiers":[{"nodes":[{"file":"...","line":N},...]},...]}
//   - prepare_context / recall: Markdown text with "path/to/file.ext:NNN" patterns
//
// If no file/line blocks are found (empty response, parse error, unsupported
// tool), a single placeholder record is returned so callers always see ≥1 entry.
func extractContextAccesses(taskID, tool, text string) []ContextAccess {
	now := time.Now()
	var out []ContextAccess

	switch tool {
	case "search":
		var resp struct {
			Results []struct {
				File string `json:"file"`
				Line int    `json:"line"`
			} `json:"results"`
		}
		if err := json.Unmarshal([]byte(text), &resp); err == nil {
			for _, r := range resp.Results {
				if r.File != "" && r.Line > 0 {
					out = append(out, ContextAccess{
						TaskID:    taskID,
						Tool:      tool,
						File:      r.File,
						LineStart: r.Line,
						LineEnd:   r.Line,
						Timestamp: now,
					})
				}
			}
		}

	case "get_impact":
		var resp struct {
			Tiers []struct {
				Nodes []struct {
					File string `json:"file"`
					Line int    `json:"line"`
				} `json:"nodes"`
			} `json:"tiers"`
		}
		if err := json.Unmarshal([]byte(text), &resp); err == nil {
			for _, tier := range resp.Tiers {
				for _, n := range tier.Nodes {
					if n.File != "" && n.Line > 0 {
						out = append(out, ContextAccess{
							TaskID:    taskID,
							Tool:      tool,
							File:      n.File,
							LineStart: n.Line,
							LineEnd:   n.Line,
							Timestamp: now,
						})
					}
				}
			}
		}

	default: // prepare_context, recall, and any future text-format tools
		for _, m := range markdownFileLineRe.FindAllStringSubmatch(text, -1) {
			if len(m) == 3 {
				lineNum, err := strconv.Atoi(m[2])
				if err == nil && lineNum > 0 {
					out = append(out, ContextAccess{
						TaskID:    taskID,
						Tool:      tool,
						File:      m[1],
						LineStart: lineNum,
						LineEnd:   lineNum,
						Timestamp: now,
					})
				}
			}
		}
	}

	// Fallback: always return at least one record so DrainAccesses sees the call.
	if len(out) == 0 {
		out = []ContextAccess{{
			TaskID:    taskID,
			Tool:      tool,
			Timestamp: now,
		}}
	}
	return out
}

// parseRankedCandidates parses the rank_candidates tool response.
// Expected format (JSON array): [{"index":0,"score":0.92}, ...]
// Falls back to identity order on parse failure.
func parseRankedCandidates(raw string, n int) ([]RankedCandidate, error) {
	var items []struct {
		Index int     `json:"index"`
		Score float64 `json:"score"`
	}
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		// Fallback: identity order.
		out := make([]RankedCandidate, n)
		for i := range out {
			out[i] = RankedCandidate{Index: i, Score: 0}
		}
		return out, nil
	}
	out := make([]RankedCandidate, len(items))
	for i, it := range items {
		out[i] = RankedCandidate{Index: it.Index, Score: it.Score}
	}
	return out, nil
}
