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

// FileContextEntity is one entity returned by get_file_context.
type FileContextEntity struct {
	Name     string `json:"name"`
	Line     int    `json:"line"`
	Type     string `json:"type"`
	Exported bool   `json:"exported"`
}

// FileContextResult is returned by GetFileContext.
type FileContextResult struct {
	File     string              `json:"file"`
	Entities []FileContextEntity `json:"entities"`
}

// HealthResult is the subset of the /v1/health response we care about.
type HealthResult struct {
	NodeCount int `json:"nodes"`
	EdgeCount int `json:"edges"`
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

// Endpoint returns the daemon endpoint URL.
func (c *SynapsesClient) Endpoint() string { return c.endpoint }

// WithProject returns a shallow copy of the client with a different project path.
// Used for per-repo routing in RepoBench-R: each sample gets routed to its own
// indexed project directory.
func (c *SynapsesClient) WithProject(project string) *SynapsesClient {
	return &SynapsesClient{
		endpoint:   c.endpoint,
		project:    project,
		authToken:  c.authToken,
		disabled:   c.disabled,
		httpClient: c.httpClient,
	}
}

// PrepareContext calls get_context with mode=intent (was prepare_context before Sprint 24).
func (c *SynapsesClient) PrepareContext(taskID, entity, intent string) (*ContextResult, error) {
	if c.disabled {
		return &ContextResult{}, nil
	}
	args := map[string]interface{}{
		"target": entity,
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
	// Default mode uses "entity" param (handlers_context.go).
	// mode=intent uses "target" param (intents.go).
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

// GetFileContext calls get_file_context to list all entities defined in a file.
func (c *SynapsesClient) GetFileContext(taskID, filePath string) (*FileContextResult, error) {
	if c.disabled {
		return &FileContextResult{}, nil
	}
	args := map[string]interface{}{"file": filePath}
	raw, err := c.callTool("get_file_context", args)
	if err != nil {
		return nil, err
	}
	c.recordAccess(taskID, "get_file_context", raw)

	// extractText concatenates multiple MCP text blocks, so the raw string
	// may have trailing non-JSON content (e.g. suggested_next_tools).
	// Try parsing the full string first; on failure, extract the first JSON object.
	var result FileContextResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		// Try extracting just the first JSON object.
		if start := strings.Index(raw, "{"); start >= 0 {
			depth, end := 0, start
			inStr := false
			for i := start; i < len(raw); i++ {
				if inStr {
					if raw[i] == '\\' {
						i++
					} else if raw[i] == '"' {
						inStr = false
					}
					continue
				}
				switch raw[i] {
				case '"':
					inStr = true
				case '{':
					depth++
				case '}':
					depth--
					if depth == 0 {
						end = i + 1
						goto done
					}
				}
			}
		done:
			if end > start {
				_ = json.Unmarshal([]byte(raw[start:end]), &result)
			}
		}
		if result.File == "" {
			return &FileContextResult{File: filePath}, nil
		}
	}
	return &result, nil
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

// RankCandidates calls the Synapses embedder via the embed_snippet tool (if
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

// GetContextJSONWithFile calls get_context with an optional file hint for disambiguation.
func (c *SynapsesClient) GetContextJSONWithFile(taskID, entity, detailLevel, fileHint string) (string, error) {
	if c.disabled {
		return "{}", nil
	}
	// Default mode uses "entity" param (handlers_context.go).
	args := map[string]interface{}{
		"entity":       entity,
		"format":       "json",
		"detail_level": detailLevel,
	}
	if fileHint != "" {
		args["file"] = fileHint
	}
	raw, err := c.callTool("get_context", args)
	if err != nil {
		return "", err
	}
	c.recordAccess(taskID, "get_context", raw)
	return raw, nil
}

// GetHealth calls the daemon health endpoint and returns node/edge counts.
func (c *SynapsesClient) GetHealth() (*HealthResult, error) {
	if c.disabled {
		return &HealthResult{}, nil
	}
	u := fmt.Sprintf("%s/v1/health", c.endpoint)
	resp, err := c.httpClient.Get(u)
	if err != nil {
		return nil, fmt.Errorf("health: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read health: %w", err)
	}
	// The health endpoint returns total_nodes and total_edges at the top level.
	var raw struct {
		TotalNodes int `json:"total_nodes"`
		TotalEdges int `json:"total_edges"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return &HealthResult{}, nil // non-fatal
	}
	return &HealthResult{
		NodeCount: raw.TotalNodes,
		EdgeCount: raw.TotalEdges,
	}, nil
}

// RunBenchmark calls the MCP benchmark tool with the given scenario name.
// Returns the raw JSON response string.
func (c *SynapsesClient) RunBenchmark(scenario string) (string, error) {
	if c.disabled {
		return "", fmt.Errorf("client disabled")
	}
	return c.callTool("benchmark", map[string]interface{}{
		"scenario": scenario,
	})
}

// fetchCSRFToken gets a CSRF token from the daemon admin API.
func (c *SynapsesClient) fetchCSRFToken() (string, error) {
	u := fmt.Sprintf("%s/api/admin/csrf-token", c.endpoint)
	resp, err := c.httpClient.Get(u)
	if err != nil {
		return "", fmt.Errorf("fetch csrf: %w", err)
	}
	defer resp.Body.Close()
	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("parse csrf: %w", err)
	}
	return result.Token, nil
}

// adminPost sends a POST to an admin API endpoint with CSRF token.
// Uses a longer timeout (5 min) since admin operations like reindex can be slow.
func (c *SynapsesClient) adminPost(path string, payload interface{}) ([]byte, error) {
	csrf, err := c.fetchCSRFToken()
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	u := fmt.Sprintf("%s%s", c.endpoint, path)
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	// Admin operations can be slow (reindex of large repos). Use a dedicated
	// client with 5-minute timeout instead of the default 30s.
	adminClient := &http.Client{Timeout: 5 * time.Minute}
	resp, err := adminClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("admin post: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("admin %s returned %d: %s", path, resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

// TriggerReindex forces a full clean reindex of the project via the admin API.
// The reindex runs asynchronously on the daemon — this method returns immediately.
func (c *SynapsesClient) TriggerReindex() error {
	if c.disabled {
		return fmt.Errorf("client disabled")
	}
	_, err := c.adminPost("/api/admin/projects/reindex", map[string]string{"path": c.project})
	return err
}

// TriggerIncrementalReindex performs an mtime-based incremental reindex on the
// existing in-memory graph. Unlike TriggerReindex (full teardown), this only
// re-parses files with changed mtimes. Runs synchronously — returns when done.
func (c *SynapsesClient) TriggerIncrementalReindex() (changed, removed int, err error) {
	if c.disabled {
		return 0, 0, fmt.Errorf("client disabled")
	}
	respBody, err := c.adminPost("/api/admin/projects/incremental-reindex", map[string]string{"path": c.project})
	if err != nil {
		return 0, 0, err
	}
	var result struct {
		Changed int `json:"changed"`
		Removed int `json:"removed"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return 0, 0, fmt.Errorf("parse response: %w", err)
	}
	return result.Changed, result.Removed, nil
}

// WaitForReady polls the health endpoint until the project's graph is loaded.
// Returns node and edge counts, or error on timeout.
func (c *SynapsesClient) WaitForReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		h, err := c.GetHealth()
		if err == nil && h.NodeCount > 0 {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timeout waiting for project ready after %v", timeout)
}

// SessionInit calls session_init for the project. Used by RecallBench to create sessions.
func (c *SynapsesClient) SessionInit(agentID, scope string) (string, error) {
	if c.disabled {
		return "", fmt.Errorf("client disabled")
	}
	args := map[string]interface{}{"agent_id": agentID}
	if scope != "" {
		args["scope"] = scope
	}
	return c.callTool("session_init", args)
}

// EndSession calls end_session for the project. Used by RecallBench to finalize sessions.
func (c *SynapsesClient) EndSession(agentID, outcome string) (string, error) {
	if c.disabled {
		return "", fmt.Errorf("client disabled")
	}
	return c.callTool("end_session", map[string]interface{}{
		"agent_id": agentID,
		"outcome":  outcome,
	})
}

// RecordEpisode saves an episode via the unified memory tool. Used by RecallBench warm-up.
func (c *SynapsesClient) RecordEpisode(agentID, episodeType, decision, outcome string) (string, error) {
	if c.disabled {
		return "", fmt.Errorf("client disabled")
	}
	return c.callTool("memory", map[string]interface{}{
		"action":       "save",
		"agent_id":     agentID,
		"episode_type": episodeType,
		"decision":     decision,
		"outcome":      outcome,
	})
}

// RecallWithProjects calls recall with the projects parameter for cross-project search.
func (c *SynapsesClient) RecallWithProjects(query string, projects string, limit int) (string, error) {
	if c.disabled {
		return "", fmt.Errorf("client disabled")
	}
	args := map[string]interface{}{
		"query": query,
		"limit": limit,
	}
	if projects != "" {
		args["projects"] = projects
	}
	return c.callTool("recall", args)
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

	case "get_file_context":
		var resp struct {
			File     string `json:"file"`
			Entities []struct {
				Line int `json:"line"`
			} `json:"entities"`
		}
		if err := json.Unmarshal([]byte(text), &resp); err == nil && resp.File != "" {
			for _, e := range resp.Entities {
				if e.Line > 0 {
					out = append(out, ContextAccess{
						TaskID:    taskID,
						Tool:      tool,
						File:      resp.File,
						LineStart: e.Line,
						LineEnd:   e.Line,
						Timestamp: now,
					})
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
