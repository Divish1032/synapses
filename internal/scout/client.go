package scout

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// Client is a fail-silent HTTP client for the synapses-scout sidecar.
// Every method returns zero/nil values on failure — callers never need to
// handle scout errors, and the graph-only path always degrades gracefully.
//
// Supports two URL schemes:
//   - http://host:port/  — standard TCP connection
//   - unix:///path/to/scout.sock — HTTP over Unix domain socket
type Client struct {
	baseURL string
	cli     *http.Client
}

// DefaultUnixSocketURL returns the standard Unix socket URL for scout.
// The daemon uses this when no explicit URL is configured.
func DefaultUnixSocketURL() string {
	return "unix:///~/.synapses/scout.sock"
}

// NewClient creates a Client targeting the given base URL. timeoutSec is the
// per-request HTTP timeout; pass 0 to use the default of 30 seconds.
// Scout fetches real pages, so the timeout must be generous.
//
// If baseURL starts with "unix://", the client dials a Unix domain socket.
// HTTP requests are still issued normally; only the transport differs.
func NewClient(baseURL string, timeoutSec int) *Client {
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	timeout := time.Duration(timeoutSec) * time.Second

	var transport http.RoundTripper
	if strings.HasPrefix(baseURL, "unix://") {
		// Extract socket path from unix:///path  → /path or ~/path
		sockPath := strings.TrimPrefix(baseURL, "unix://")
		// Strip the leading slash that triple-slash notation adds before "~"
		if strings.HasPrefix(sockPath, "/~") {
			sockPath = sockPath[1:] // → "~/.synapses/scout.sock"
		}
		// Expand tilde — net.Dial does not shell-expand paths.
		if strings.HasPrefix(sockPath, "~/") {
			home, err := os.UserHomeDir()
			if err == nil {
				sockPath = home + sockPath[1:]
			}
		}
		transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", sockPath)
			},
		}
		// Requests must target http://localhost/... — the transport ignores host/port.
		baseURL = "http://localhost"
	}

	httpCli := &http.Client{Timeout: timeout}
	if transport != nil {
		httpCli.Transport = transport
	}
	return &Client{baseURL: baseURL, cli: httpCli}
}

// Health calls GET /v1/health and returns true if the service is reachable.
func (c *Client) Health(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/health", nil)
	if err != nil {
		return false
	}
	resp, err := c.cli.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Search calls POST /v1/search and returns the results. Returns nil on failure.
func (c *Client) Search(ctx context.Context, req SearchRequest) *SearchResponse {
	var out SearchResponse
	if err := c.post(ctx, "/v1/search", req, &out); err != nil {
		return nil
	}
	return &out
}

// Fetch calls POST /v1/fetch and returns the extracted content. Returns nil on failure.
func (c *Client) Fetch(ctx context.Context, req FetchRequest) *FetchResponse {
	var out FetchResponse
	if err := c.post(ctx, "/v1/fetch", req, &out); err != nil {
		return nil
	}
	return &out
}

// DeepSearch calls POST /v1/deep-search and returns the orchestrated results. Returns nil on failure.
func (c *Client) DeepSearch(ctx context.Context, req DeepSearchRequest) *DeepSearchResponse {
	var out DeepSearchResponse
	if err := c.post(ctx, "/v1/deep-search", req, &out); err != nil {
		return nil
	}
	return &out
}

// LookupDocs calls POST /v1/lookup-docs: searches + fetches in one round-trip.
// Returns nil on failure.
func (c *Client) LookupDocs(ctx context.Context, req LookupDocsRequest) *LookupDocsResponse {
	var out LookupDocsResponse
	if err := c.post(ctx, "/v1/lookup-docs", req, &out); err != nil {
		return nil
	}
	return &out
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
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
