package scout

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is a fail-silent HTTP client for the synapses-scout sidecar.
// Every method returns zero/nil values on failure — callers never need to
// handle scout errors, and the graph-only path always degrades gracefully.
type Client struct {
	baseURL string
	cli     *http.Client
}

// NewClient creates a Client targeting the given base URL. timeoutSec is the
// per-request HTTP timeout; pass 0 to use the default of 30 seconds.
// Scout fetches real pages, so the timeout must be generous.
func NewClient(baseURL string, timeoutSec int) *Client {
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	return &Client{
		baseURL: baseURL,
		cli: &http.Client{
			Timeout: time.Duration(timeoutSec) * time.Second,
		},
	}
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
