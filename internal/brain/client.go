package brain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Client is a fail-silent HTTP client for the synapses-intelligence sidecar.
// Every method returns zero values on failure — callers never need to handle
// brain errors, and the graph-only path always degrades gracefully.
type Client struct {
	baseURL string
	cli     *http.Client
}

// NewClient creates a Client targeting the given base URL. timeoutSec is the
// per-request HTTP timeout; pass 0 to use the default of 5 seconds.
func NewClient(baseURL string, timeoutSec int) *Client {
	if timeoutSec <= 0 {
		timeoutSec = 5
	}
	return &Client{
		baseURL: baseURL,
		cli: &http.Client{
			Timeout: time.Duration(timeoutSec) * time.Second,
		},
	}
}

// HealthCheck calls GET /v1/health and returns the active model name.
// Returns an error if the service is unreachable or returns a non-200 status.
func (c *Client) HealthCheck(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/health", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.cli.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("health check: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "unknown", nil // healthy but couldn't parse model name
	}
	return out.Model, nil
}

// BuildContextPacket calls POST /v1/context-packet and returns the enriched
// context packet. Returns nil on any failure (brain unreachable, timeout, etc.).
// Uses the client's configured HTTP timeout (set via NewClient) so CPU-only
// Ollama deployments with slower inference do not always time out.
func (c *Client) BuildContextPacket(ctx context.Context, req ContextPacketRequest) *ContextPacket {
	var pkt ContextPacket
	if err := c.post(ctx, "/v1/context-packet", req, &pkt); err != nil {
		return nil
	}
	return &pkt
}

// Ingest calls POST /v1/ingest as a fire-and-forget operation.
// All errors are silently discarded.
func (c *Client) Ingest(ctx context.Context, req IngestRequest) {
	_ = c.post(ctx, "/v1/ingest", req, nil)
}

// BulkIngest sends a slice of IngestRequests as individual POST /v1/ingest calls.
// All errors are silently discarded (fire-and-forget semantics).
func (c *Client) BulkIngest(ctx context.Context, nodes []IngestRequest) {
	for _, n := range nodes {
		_ = c.post(ctx, "/v1/ingest", n, nil)
	}
}

// ExplainViolation calls POST /v1/explain-violation and returns (explanation, fix).
// Returns ("", "") on any failure.
func (c *Client) ExplainViolation(ctx context.Context, req ViolationRequest) (string, string) {
	var out ViolationExplanation
	if err := c.post(ctx, "/v1/explain-violation", req, &out); err != nil {
		return "", ""
	}
	return out.Explanation, out.Fix
}

// Coordinate calls POST /v1/coordinate and returns the suggestion string.
// Returns "" on any failure.
func (c *Client) Coordinate(ctx context.Context, req CoordinateRequest) string {
	var out CoordinateResponse
	if err := c.post(ctx, "/v1/coordinate", req, &out); err != nil {
		return ""
	}
	return out.Suggestion
}

// GetSummary calls GET /v1/summary/{nodeID} and returns the cached summary string.
// Returns "" on any failure (node not yet summarized, brain unreachable, timeout, etc.).
func (c *Client) GetSummary(ctx context.Context, nodeID string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/v1/summary/"+url.PathEscape(nodeID), nil)
	if err != nil {
		return ""
	}
	resp, err := c.cli.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		Summary string `json:"summary"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ""
	}
	return out.Summary
}

// LogDecision calls POST /v1/decision as a fire-and-forget operation.
// All errors are silently discarded.
func (c *Client) LogDecision(ctx context.Context, req DecisionRequest) {
	_ = c.post(ctx, "/v1/decision", req, nil)
}

// GetSDLC calls GET /v1/sdlc and returns (phase, qualityMode).
// Returns ("development", "standard") on any failure.
func (c *Client) GetSDLC(ctx context.Context) (string, string) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/sdlc", nil)
	if err != nil {
		return "development", "standard"
	}
	resp, err := c.cli.Do(httpReq)
	if err != nil {
		return "development", "standard"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "development", "standard"
	}
	var out SDLCConfig
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "development", "standard"
	}
	if out.Phase == "" {
		out.Phase = "development"
	}
	if out.QualityMode == "" {
		out.QualityMode = "standard"
	}
	return out.Phase, out.QualityMode
}

// SetPhase calls PUT /v1/sdlc/phase to update the current SDLC phase.
// Returns ("", error) on failure.
func (c *Client) SetPhase(ctx context.Context, req SetPhaseRequest) (*SDLCConfig, error) {
	var out SDLCConfig
	if err := c.put(ctx, "/v1/sdlc/phase", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// post marshals body as JSON, POSTs to the endpoint, and decodes the response
// into out (if out is non-nil). Returns an error on any failure.
func (c *Client) post(ctx context.Context, path string, body, out interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil // success, no body expected
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// put marshals body as JSON, PUTs to the endpoint, and decodes into out.
func (c *Client) put(ctx context.Context, path string, body, out interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
