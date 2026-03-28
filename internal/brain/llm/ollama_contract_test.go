package llm

// API contract tests for OllamaClient — verify exact HTTP payloads sent to Ollama.
// Every test uses httptest.NewServer to intercept and inspect the raw request body,
// confirming that WithJSONFormat, WithChatMode, WithKeepAlive, WithNumPredict, and
// WarmUp send exactly the fields the Ollama API requires.
//
// These tests protect against regressions where a method is wired but the field
// never reaches the HTTP layer — the class of silent bug that caused production
// failures in the original Navigator/Archivist implementation.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// captureRequest intercepts one POST request and decodes the body into dst.
// Responds with a valid Ollama generate or chat response so the client doesn't error.
func captureGenerateRequest(t *testing.T, dst interface{}, handler ...func(r *http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r.Body)
		if err := json.Unmarshal(body, dst); err != nil {
			t.Errorf("captureGenerateRequest: unmarshal failed: %v — body: %s", err, body)
		}
		if len(handler) > 0 {
			handler[0](r)
		}
		w.Header().Set("Content-Type", "application/json")
		// Respond as either /api/generate or /api/chat depending on path.
		if r.URL.Path == "/api/chat" {
			resp := ollamaChatResponse{Message: ollamaMessage{Role: "assistant", Content: `{"ok":true}`}, Done: true}
			json.NewEncoder(w).Encode(resp) //nolint:errcheck
		} else {
			resp := ollamaResponse{Response: `{"ok":true}`, Done: true}
			json.NewEncoder(w).Encode(resp) //nolint:errcheck
		}
	}))
}

// ── WithJSONFormat ────────────────────────────────────────────────────────────

func TestOllamaClient_WithJSONFormat_True_SendsFormatField(t *testing.T) {
	var captured ollamaRequest
	srv := captureGenerateRequest(t, &captured)
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "llama3", 5000).WithJSONFormat(true)
	_, _ = c.Generate(context.Background(), "test prompt")

	if captured.Format != "json" {
		t.Errorf(`WithJSONFormat(true): Format = %q, want "json"`, captured.Format)
	}
}

func TestOllamaClient_WithJSONFormat_False_OmitsFormatField(t *testing.T) {
	var captured ollamaRequest
	srv := captureGenerateRequest(t, &captured)
	defer srv.Close()

	// Default client — no JSON format.
	c := NewOllamaClient(srv.URL, "llama3", 5000)
	_, _ = c.Generate(context.Background(), "test prompt")

	if captured.Format != "" {
		t.Errorf("default client: Format = %q, want empty (omitted)", captured.Format)
	}
}

func TestOllamaClient_WithJSONFormat_ExplicitFalse_OmitsFormatField(t *testing.T) {
	var captured ollamaRequest
	srv := captureGenerateRequest(t, &captured)
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "llama3", 5000).WithJSONFormat(false)
	_, _ = c.Generate(context.Background(), "test prompt")

	if captured.Format != "" {
		t.Errorf("WithJSONFormat(false): Format = %q, want empty", captured.Format)
	}
}

// ── WithChatMode ──────────────────────────────────────────────────────────────

func TestOllamaClient_WithChatMode_UsesApiChatEndpoint(t *testing.T) {
	var capturedPath string
	var capturedChatReq ollamaChatRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		body, _ := readAll(r.Body)
		json.Unmarshal(body, &capturedChatReq) //nolint:errcheck
		w.Header().Set("Content-Type", "application/json")
		resp := ollamaChatResponse{Message: ollamaMessage{Role: "assistant", Content: "response"}, Done: true}
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "qwen3.5:2b", 5000).WithChatMode(true)
	got, err := c.Generate(context.Background(), "user prompt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Must hit /api/chat, not /api/generate.
	if capturedPath != "/api/chat" {
		t.Errorf("endpoint = %q, want /api/chat", capturedPath)
	}
	// Prompt must be wrapped as user message.
	if len(capturedChatReq.Messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(capturedChatReq.Messages))
	}
	if capturedChatReq.Messages[0].Role != "user" {
		t.Errorf("message role = %q, want user", capturedChatReq.Messages[0].Role)
	}
	if capturedChatReq.Messages[0].Content != "user prompt" {
		t.Errorf("message content = %q, want 'user prompt'", capturedChatReq.Messages[0].Content)
	}
	// Response must be extracted from message.content.
	if got != "response" {
		t.Errorf("Generate() = %q, want 'response'", got)
	}
}

func TestOllamaClient_WithChatMode_False_UsesApiGenerateEndpoint(t *testing.T) {
	var capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ollamaResponse{Response: "ok", Done: true}) //nolint:errcheck
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "llama3", 5000) // default: no chat mode
	_, _ = c.Generate(context.Background(), "prompt")

	if capturedPath != "/api/generate" {
		t.Errorf("endpoint = %q, want /api/generate", capturedPath)
	}
}

func TestOllamaClient_WithChatMode_WithJSONFormat_SendsFormatInChatRequest(t *testing.T) {
	var captured ollamaChatRequest
	srv := captureGenerateRequest(t, &captured)
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "qwen3.5:2b", 5000).
		WithChatMode(true).
		WithJSONFormat(true)
	_, _ = c.Generate(context.Background(), "prompt")

	if captured.Format != "json" {
		t.Errorf(`WithChatMode+WithJSONFormat: chat request Format = %q, want "json"`, captured.Format)
	}
}

func TestOllamaClient_WithChatMode_StripThinkBlocks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := ollamaChatResponse{
			Message: ollamaMessage{Role: "assistant", Content: "<think>hidden</think>clean answer"},
			Done:    true,
		}
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "qwen3.5:2b", 5000).WithChatMode(true)
	got, err := c.Generate(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "clean answer" {
		t.Errorf("expected think blocks stripped from chat response, got %q", got)
	}
}

// ── WithKeepAlive ─────────────────────────────────────────────────────────────

func TestOllamaClient_WithKeepAlive_Negative1_PinsModel(t *testing.T) {
	var captured ollamaRequest
	srv := captureGenerateRequest(t, &captured)
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "llama3", 5000).WithKeepAlive(-1)
	_, _ = c.Generate(context.Background(), "prompt")

	if captured.KeepAlive == nil {
		t.Fatal("keep_alive field not sent (nil), want -1")
	}
	if *captured.KeepAlive != -1 {
		t.Errorf("keep_alive = %d, want -1 (pin forever)", *captured.KeepAlive)
	}
}

func TestOllamaClient_WithKeepAlive_Zero_EvictsImmediately(t *testing.T) {
	var captured ollamaRequest
	srv := captureGenerateRequest(t, &captured)
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "llama3", 5000).WithKeepAlive(0)
	_, _ = c.Generate(context.Background(), "prompt")

	if captured.KeepAlive == nil {
		t.Fatal("keep_alive field not sent (nil), want 0")
	}
	if *captured.KeepAlive != 0 {
		t.Errorf("keep_alive = %d, want 0 (evict immediately)", *captured.KeepAlive)
	}
}

func TestOllamaClient_WithKeepAlive_300_SetsTTL(t *testing.T) {
	var captured ollamaRequest
	srv := captureGenerateRequest(t, &captured)
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "llama3", 5000).WithKeepAlive(300)
	_, _ = c.Generate(context.Background(), "prompt")

	if captured.KeepAlive == nil {
		t.Fatal("keep_alive field not sent (nil), want 300")
	}
	if *captured.KeepAlive != 300 {
		t.Errorf("keep_alive = %d, want 300 (5-min TTL)", *captured.KeepAlive)
	}
}

func TestOllamaClient_NoKeepAlive_FieldOmitted(t *testing.T) {
	var captured ollamaRequest
	srv := captureGenerateRequest(t, &captured)
	defer srv.Close()

	// Default client — no WithKeepAlive call.
	c := NewOllamaClient(srv.URL, "llama3", 5000)
	_, _ = c.Generate(context.Background(), "prompt")

	if captured.KeepAlive != nil {
		t.Errorf("keep_alive = %d, want nil (omitted — use Ollama server default)", *captured.KeepAlive)
	}
}

func TestOllamaClient_WithKeepAlive_ChatMode_SendsKeepAliveInChatRequest(t *testing.T) {
	var captured ollamaChatRequest
	srv := captureGenerateRequest(t, &captured)
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "qwen3.5:2b", 5000).
		WithChatMode(true).
		WithKeepAlive(-1)
	_, _ = c.Generate(context.Background(), "prompt")

	if captured.KeepAlive == nil {
		t.Fatal("keep_alive not sent in chat request, want -1")
	}
	if *captured.KeepAlive != -1 {
		t.Errorf("chat keep_alive = %d, want -1", *captured.KeepAlive)
	}
}

// ── WithNumPredict ────────────────────────────────────────────────────────────

func TestOllamaClient_WithNumPredict_SetsNumPredictInOptions(t *testing.T) {
	var captured ollamaRequest
	srv := captureGenerateRequest(t, &captured)
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "llama3", 5000).WithNumPredict(1024)
	_, _ = c.Generate(context.Background(), "prompt")

	if captured.Options.NumPredict != 1024 {
		t.Errorf("options.num_predict = %d, want 1024", captured.Options.NumPredict)
	}
}

func TestOllamaClient_DefaultNumPredict_Is400(t *testing.T) {
	var captured ollamaRequest
	srv := captureGenerateRequest(t, &captured)
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "llama3", 5000)
	_, _ = c.Generate(context.Background(), "prompt")

	if captured.Options.NumPredict != 400 {
		t.Errorf("default options.num_predict = %d, want 400", captured.Options.NumPredict)
	}
}

func TestOllamaClient_WithNumPredict_Zero_IgnoredKeepsDefault(t *testing.T) {
	var captured ollamaRequest
	srv := captureGenerateRequest(t, &captured)
	defer srv.Close()

	// WithNumPredict(0) must be ignored — zero would disable generation entirely.
	c := NewOllamaClient(srv.URL, "llama3", 5000).WithNumPredict(0)
	_, _ = c.Generate(context.Background(), "prompt")

	if captured.Options.NumPredict != 400 {
		t.Errorf("WithNumPredict(0) changed num_predict to %d, want 400 (ignored)", captured.Options.NumPredict)
	}
}

func TestOllamaClient_WithNumPredict_ChatMode_SetsNumPredictInOptions(t *testing.T) {
	var captured ollamaChatRequest
	srv := captureGenerateRequest(t, &captured)
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "qwen3.5:2b", 5000).
		WithChatMode(true).
		WithNumPredict(1024)
	_, _ = c.Generate(context.Background(), "prompt")

	if captured.Options.NumPredict != 1024 {
		t.Errorf("chat options.num_predict = %d, want 1024", captured.Options.NumPredict)
	}
}

// ── WarmUp ────────────────────────────────────────────────────────────────────

func TestOllamaClient_WarmUp_UsesClientKeepAlive_NotHardcoded(t *testing.T) {
	// This is the regression test for Bug 5: WarmUp previously hardcoded keep_alive=-1
	// regardless of the client's configured keepAlive. In Optimal mode (keepAlive=0),
	// this caused all models to be pinned in RAM forever after warmup, blowing the budget.
	var captured ollamaRequest
	srv := captureGenerateRequest(t, &captured)
	defer srv.Close()

	// Client configured for Optimal mode: evict immediately after each call.
	c := NewOllamaClient(srv.URL, "synapses/librarian:q4", 5000).WithKeepAlive(0)
	if err := c.WarmUp(context.Background()); err != nil {
		t.Fatalf("WarmUp() unexpected error: %v", err)
	}

	if captured.KeepAlive == nil {
		t.Fatal("WarmUp did not send keep_alive field — cannot respect Optimal mode budget")
	}
	if *captured.KeepAlive != 0 {
		t.Errorf("WarmUp keep_alive = %d, want 0 (client configured for evict-on-use)", *captured.KeepAlive)
	}
}

func TestOllamaClient_WarmUp_NilKeepAlive_DefaultsPinForever(t *testing.T) {
	// When no WithKeepAlive is called (nil), WarmUp should default to -1 (pin)
	// to preserve historical warmup behaviour.
	var captured ollamaRequest
	srv := captureGenerateRequest(t, &captured)
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "llama3", 5000) // no WithKeepAlive
	if err := c.WarmUp(context.Background()); err != nil {
		t.Fatalf("WarmUp() unexpected error: %v", err)
	}

	if captured.KeepAlive == nil {
		t.Fatal("WarmUp did not send keep_alive field")
	}
	if *captured.KeepAlive != -1 {
		t.Errorf("WarmUp keep_alive = %d, want -1 (default pin-forever for unconfigured clients)", *captured.KeepAlive)
	}
}

func TestOllamaClient_WarmUp_PinnedClient_SendsNegativeOne(t *testing.T) {
	var captured ollamaRequest
	srv := captureGenerateRequest(t, &captured)
	defer srv.Close()

	// Standard/Full mode: Sentry and Librarian are pinned.
	c := NewOllamaClient(srv.URL, "synapses/sentry:q4", 5000).WithKeepAlive(-1)
	if err := c.WarmUp(context.Background()); err != nil {
		t.Fatalf("WarmUp() unexpected error: %v", err)
	}

	if captured.KeepAlive == nil || *captured.KeepAlive != -1 {
		t.Errorf("WarmUp keep_alive = %v, want -1 (pinned client stays pinned after warmup)", captured.KeepAlive)
	}
}

func TestOllamaClient_WarmUp_MinimalTokens(t *testing.T) {
	// WarmUp must request exactly 1 token — enough to load the model, nothing wasted.
	var captured ollamaRequest
	srv := captureGenerateRequest(t, &captured)
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "llama3", 5000)
	_ = c.WarmUp(context.Background())

	if captured.Options.NumPredict != 1 {
		t.Errorf("WarmUp num_predict = %d, want 1 (minimal)", captured.Options.NumPredict)
	}
}

func TestOllamaClient_WarmUp_HTTPError_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "llama3", 5000)
	err := c.WarmUp(context.Background())
	if err == nil {
		t.Fatal("expected error for HTTP 503, got nil")
	}
	if fmt.Sprintf("%v", err) == "" {
		t.Error("expected non-empty error message")
	}
}
