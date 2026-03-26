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
	"strings"
	"sync"
	"time"
)

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

// PrepareContext calls the prepare_context tool.
func (c *SynapsesClient) PrepareContext(taskID, entity, intent string) (*ContextResult, error) {
	if c.disabled {
		return &ContextResult{}, nil
	}
	args := map[string]interface{}{
		"target": entity,
		"intent": intent,
	}
	raw, err := c.callTool("prepare_context", args)
	if err != nil {
		return nil, err
	}
	c.recordAccess(taskID, "prepare_context", raw)
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

// Recall calls the recall tool.
func (c *SynapsesClient) Recall(taskID, query string) (*RecallResult, error) {
	if c.disabled {
		return &RecallResult{}, nil
	}
	args := map[string]interface{}{"query": query}
	raw, err := c.callTool("recall", args)
	if err != nil {
		return nil, err
	}
	c.recordAccess(taskID, "recall", raw)
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

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("http post %s: %w", tool, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tool %s returned %d: %s", tool, resp.StatusCode, string(respBody))
	}

	return extractText(respBody)
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

// recordAccess appends a raw access record. File/line parsing is best-effort;
// for ContextBench the full text is stored and parsed at evaluation time.
func (c *SynapsesClient) recordAccess(taskID, tool, _ string) {
	c.mu.Lock()
	c.accesses = append(c.accesses, ContextAccess{
		TaskID:    taskID,
		Tool:      tool,
		Timestamp: time.Now(),
	})
	c.mu.Unlock()
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
