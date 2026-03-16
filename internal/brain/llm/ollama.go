package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ModelWarmer can pre-load a model into memory before the first real request.
// OllamaClient implements this by sending an empty prompt with keep_alive=-1,
// which forces Ollama to load the model weights without generating any output.
type ModelWarmer interface {
	WarmUp(ctx context.Context) error
}

// OllamaClient calls the Ollama REST API at POST /api/generate.
// It keeps a reusable http.Client for connection pooling.
type OllamaClient struct {
	baseURL    string
	model      string
	httpClient *http.Client
	// think controls Qwen3.5 extended thinking mode via the Ollama API's
	// think: bool field (Ollama ≥0.6). When false, chain-of-thought is suppressed
	// and the model responds faster. Only effective for Qwen3.x models.
	think bool
	// useChat switches from /api/generate (raw prompt) to /api/chat (messages format).
	// Required for fine-tuned Qwen3.5 models which need chat-template formatting
	// to follow instructions correctly. Use WithChatMode(true) for these models.
	useChat bool
	// keepAlive controls how long Ollama keeps the model loaded after a request.
	// nil = use Ollama default (5 minutes). -1 = keep loaded indefinitely (pin in RAM).
	// 0 = evict immediately after each request. Set via WithKeepAlive().
	keepAlive *int
	// useJSON sets "format":"json" in the Ollama request body, constraining the
	// model to emit only valid JSON. Use for tiers that parse structured output
	// (Orchestrator, Archivist) where base models might otherwise produce free-text.
	useJSON bool
	// numPredict caps the maximum output tokens per request.
	// Default: 400 (sufficient for insight/coordination JSON).
	// Increase for tiers with longer outputs, e.g. Archivist (1024).
	numPredict int
}

// NewOllamaClient creates a client targeting the given Ollama base URL and model.
// timeoutMS is the per-request timeout in milliseconds.
func NewOllamaClient(baseURL, model string, timeoutMS int) *OllamaClient {
	if timeoutMS <= 0 {
		timeoutMS = 3000
	}
	return &OllamaClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		httpClient: &http.Client{
			Timeout: time.Duration(timeoutMS) * time.Millisecond,
		},
		numPredict: 400, // default: enough for insight/coordination JSON
	}
}

// WithThinking configures extended thinking mode for Qwen3.5 models.
// Call on construction: llm.NewOllamaClient(...).WithThinking(true)
// Returns the client to allow chaining.
func (c *OllamaClient) WithThinking(enabled bool) *OllamaClient {
	c.think = enabled
	return c
}

// WithChatMode switches the client from /api/generate to /api/chat.
// Required for fine-tuned Qwen3.5 models: they need the chat-template
// message structure to follow instructions correctly. Raw /api/generate
// prompts cause these models to echo training examples instead of responding.
// Returns the client to allow chaining.
func (c *OllamaClient) WithChatMode(enabled bool) *OllamaClient {
	c.useChat = enabled
	return c
}

// WithKeepAlive sets how long Ollama keeps the model loaded after a request.
// Pass -1 to pin the model in RAM indefinitely (hot-tier models called frequently).
// Pass 0 to evict immediately after each request (one-shot cold tasks).
// Pass positive seconds for a custom TTL. nil (default) uses Ollama's 5-minute default.
// Returns the client to allow chaining.
func (c *OllamaClient) WithKeepAlive(secs int) *OllamaClient {
	c.keepAlive = &secs
	return c
}

// WithJSONFormat enables Ollama's structured JSON output mode by setting
// "format":"json" in the request body. When enabled, the model is constrained
// to emit only valid JSON — it will not produce prose, markdown fences, or
// partial output. Use for tiers that parse structured responses (Orchestrator,
// Archivist) where base models might otherwise produce free-text.
// Returns the client to allow chaining.
func (c *OllamaClient) WithJSONFormat(enabled bool) *OllamaClient {
	c.useJSON = enabled
	return c
}

// WithNumPredict sets the maximum output tokens per request.
// Default is 400 (sufficient for insight/coordination JSON).
// Increase for tiers with longer structured outputs, e.g. Archivist (1024).
// Returns the client to allow chaining.
func (c *OllamaClient) WithNumPredict(n int) *OllamaClient {
	if n > 0 {
		c.numPredict = n
	}
	return c
}

// ollamaRequest is the payload for POST /api/generate.
type ollamaRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
	// Think controls extended thinking mode via the Ollama ≥0.6 API field.
	// When false, chain-of-thought is suppressed (faster). When true, the model
	// reasons before answering. Only Qwen3.x models honour this field.
	// omitempty: nil = not sent (non-Qwen3 models, or when not needed).
	Think *bool `json:"think,omitempty"`
	// KeepAlive overrides the server-level OLLAMA_KEEP_ALIVE for this request.
	// -1 = pin indefinitely, 0 = evict immediately, nil = use server default.
	KeepAlive *int `json:"keep_alive,omitempty"`
	// Format constrains output to a specific format. Set to "json" to force
	// valid JSON output — the model will not produce prose or markdown fences.
	// omitempty: empty string = not sent (free-form output).
	Format string `json:"format,omitempty"`
	// Options tuned for small models: low temperature for deterministic JSON output.
	Options ollamaOptions `json:"options"`
}

type ollamaOptions struct {
	Temperature float64  `json:"temperature"`
	NumPredict  int      `json:"num_predict"` // max output tokens
	Stop        []string `json:"stop,omitempty"`
}

// ollamaResponse is the non-streaming response from Ollama.
type ollamaResponse struct {
	Response string `json:"response"`
	Done     bool   `json:"done"`
	Error    string `json:"error,omitempty"`
}

// ollama chat API types — used when useChat=true.
type ollamaChatRequest struct {
	Model     string          `json:"model"`
	Messages  []ollamaMessage `json:"messages"`
	Stream    bool            `json:"stream"`
	KeepAlive *int            `json:"keep_alive,omitempty"`
	// Format constrains output to a specific format. Set to "json" to force
	// valid JSON output. omitempty: empty string = not sent (free-form output).
	Format  string        `json:"format,omitempty"`
	Options ollamaOptions `json:"options"`
}

type ollamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaChatResponse struct {
	Message ollamaMessage `json:"message"`
	Done    bool          `json:"done"`
	Error   string        `json:"error,omitempty"`
}

// Generate sends a prompt and returns the response text.
// Uses stream=false for simplicity and lowest latency on small outputs.
// For Qwen3.x models, sets the Ollama API think: bool field (≥0.6) to control
// chain-of-thought. Non-Qwen3 models receive no think field — they ignore it.
// When useChat=true, dispatches to /api/chat instead of /api/generate —
// required for fine-tuned Qwen3.5 models that need chat-template formatting.
func (c *OllamaClient) Generate(ctx context.Context, prompt string) (string, error) {
	if c.useChat {
		return c.generateChat(ctx, prompt)
	}
	return c.generateRaw(ctx, prompt)
}

// generateRaw calls POST /api/generate with the prompt as a raw string.
func (c *OllamaClient) generateRaw(ctx context.Context, prompt string) (string, error) {
	reqBody := ollamaRequest{
		Model:     c.model,
		Prompt:    prompt,
		Stream:    false,
		KeepAlive: c.keepAlive,
		Options: ollamaOptions{
			Temperature: 0.1, // near-deterministic for structured JSON
			NumPredict:  c.numPredict,
			// No stop tokens: models wrap JSON in markdown fences and stop tokens
			// fire immediately (e.g. "```" fires on the opening fence), producing
			// empty responses. ExtractJSON handles all formatting variants.
		},
	}
	if c.useJSON {
		reqBody.Format = "json"
	}

	// Set think: bool for Qwen3.x models via the Ollama ≥0.6 API field.
	// The old /think and /no_think prompt prefixes are intentionally removed:
	// Ollama 0.17.6+ ignores them for qwen3.5 models and returns an empty
	// response field when the prefix is present.
	if strings.HasPrefix(strings.ToLower(c.model), "qwen3") {
		think := c.think
		reqBody.Think = &think
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/generate", bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama generate: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("ollama returned %d: %s", resp.StatusCode, body)
	}

	var result ollamaResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if result.Error != "" {
		return "", fmt.Errorf("ollama error: %s", result.Error)
	}

	// Strip extended thinking blocks (<think>...</think>) that Qwen3.5 emits
	// when thinking mode is enabled. The actual answer follows after the block.
	response := thinkTagRe.ReplaceAllString(result.Response, "")
	return strings.TrimSpace(response), nil
}

// generateChat calls POST /api/chat, wrapping the prompt as a user message.
// The Modelfile's SYSTEM block is applied automatically by Ollama.
// Used for Qwen3.5 models (base or fine-tuned) where chat-template formatting
// improves structured output adherence.
func (c *OllamaClient) generateChat(ctx context.Context, prompt string) (string, error) {
	reqBody := ollamaChatRequest{
		Model: c.model,
		Messages: []ollamaMessage{
			{Role: "user", Content: prompt},
		},
		Stream:    false,
		KeepAlive: c.keepAlive,
		Options: ollamaOptions{
			Temperature: 0.1,
			NumPredict:  c.numPredict,
		},
	}
	if c.useJSON {
		reqBody.Format = "json"
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal chat request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/chat", bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("create chat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama chat: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("ollama chat returned %d: %s", resp.StatusCode, body)
	}

	var result ollamaChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode chat response: %w", err)
	}
	if result.Error != "" {
		return "", fmt.Errorf("ollama chat error: %s", result.Error)
	}

	response := thinkTagRe.ReplaceAllString(result.Message.Content, "")
	return strings.TrimSpace(response), nil
}

// WarmUp pre-loads the model into Ollama's memory by sending an empty prompt
// with keep_alive=-1 (pin indefinitely). The model weights are loaded without
// generating output, eliminating cold-start latency on the first real request.
// Implements ModelWarmer. Called in background goroutines at brain startup.
func (c *OllamaClient) WarmUp(ctx context.Context) error {
	pinForever := -1
	reqBody := ollamaRequest{
		Model:     c.model,
		Prompt:    "",
		Stream:    false,
		KeepAlive: &pinForever,
		Options: ollamaOptions{
			Temperature: 0.0,
			NumPredict:  1, // minimal tokens — just enough to confirm the model loaded
		},
	}
	data, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("warmup marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/generate", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("warmup request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("warmup: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("warmup returned %d: %s", resp.StatusCode, body)
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	return nil
}

// Available checks if Ollama is reachable by calling GET /api/tags.
// Returns true only if the HTTP call succeeds with a 200 status.
func (c *OllamaClient) Available(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/tags", nil)
	if err != nil {
		return false
	}
	// Use a short ping timeout regardless of the configured timeout.
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req = req.WithContext(pingCtx)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// ModelName returns the configured model tag.
func (c *OllamaClient) ModelName() string {
	return c.model
}

// ModelPulled returns true if the configured model is already present in
// Ollama's local model library (i.e. no pull is needed).
func (c *OllamaClient) ModelPulled(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/tags", nil)
	if err != nil {
		return false
	}
	resp, err := c.httpClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return false
	}
	defer resp.Body.Close()

	var result struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false
	}
	for _, m := range result.Models {
		// Ollama tags may have ":latest" appended; match with or without tag.
		if m.Name == c.model || strings.HasPrefix(m.Name, c.model+":") ||
			strings.TrimSuffix(m.Name, ":latest") == strings.TrimSuffix(c.model, ":latest") {
			return true
		}
	}
	return false
}

// ProbeLatency measures actual inference latency by generating a short fixed
// response. Uses a raw prompt (no thinking prefix) to measure baseline speed.
// Returns the wall-clock duration, or an error if the model doesn't respond
// within maxDuration. Callers should use this to pick the fastest installed model.
func (c *OllamaClient) ProbeLatency(ctx context.Context, maxDuration time.Duration) (time.Duration, error) {
	probeCtx, cancel := context.WithTimeout(ctx, maxDuration)
	defer cancel()

	reqBody := ollamaRequest{
		Model:  c.model,
		Prompt: "Reply with one word: ready",
		Stream: false,
		Options: ollamaOptions{
			Temperature: 0.0,
			NumPredict:  8, // minimal output — just enough to confirm the model runs
		},
	}
	data, err := json.Marshal(reqBody)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(probeCtx, http.MethodPost,
		c.baseURL+"/api/generate", bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	probeCli := &http.Client{Timeout: maxDuration + time.Second}
	start := time.Now()
	resp, err := probeCli.Do(req)
	if err != nil {
		return 0, fmt.Errorf("probe: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("probe returned HTTP %d", resp.StatusCode)
	}
	var result ollamaResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("probe decode: %w", err)
	}
	if result.Error != "" {
		return 0, fmt.Errorf("probe error: %s", result.Error)
	}
	return time.Since(start), nil
}

// ListInstalledModels returns all model names present in Ollama's local library.
func ListInstalledModels(ctx context.Context, baseURL string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(baseURL, "/")+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	cli := &http.Client{Timeout: 5 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list models: HTTP %d", resp.StatusCode)
	}
	var result struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("list models decode: %w", err)
	}
	names := make([]string, 0, len(result.Models))
	for _, m := range result.Models {
		names = append(names, m.Name)
	}
	return names, nil
}

// PullModel pulls the configured model from the Ollama registry, streaming
// progress lines to w. Pass os.Stderr for terminal feedback.
// Blocks until the pull completes or ctx is cancelled.
func (c *OllamaClient) PullModel(ctx context.Context, w io.Writer) error {
	body, err := json.Marshal(map[string]any{"name": c.model, "stream": true})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/pull", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	// Pull can take minutes; use a long timeout client.
	pullCli := &http.Client{Timeout: 30 * time.Minute}
	resp, err := pullCli.Do(req)
	if err != nil {
		return fmt.Errorf("pull request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("ollama pull returned %d: %s", resp.StatusCode, b)
	}

	// Stream newline-delimited JSON progress events.
	dec := json.NewDecoder(resp.Body)
	var lastStatus string
	for {
		var evt struct {
			Status    string `json:"status"`
			Total     int64  `json:"total"`
			Completed int64  `json:"completed"`
			Error     string `json:"error"`
		}
		if err := dec.Decode(&evt); err != nil {
			break // EOF = done
		}
		if evt.Error != "" {
			return fmt.Errorf("pull error: %s", evt.Error)
		}
		if evt.Status != lastStatus {
			if evt.Total > 0 {
				pct := int(float64(evt.Completed) / float64(evt.Total) * 100)
				fmt.Fprintf(w, "\r  %-40s %3d%%", evt.Status, pct)
			} else {
				fmt.Fprintf(w, "\r  %-40s     ", evt.Status)
			}
			lastStatus = evt.Status
		}
	}
	fmt.Fprintln(w) // newline after progress line
	return nil
}
