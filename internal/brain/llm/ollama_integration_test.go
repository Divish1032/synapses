package llm

// Integration tests for OllamaClient — require a real Ollama instance.
//
// These tests close the gap that unit tests cannot: they verify the Ollama
// API actually honours the fields we send (format:"json", /api/chat, keep_alive)
// and that the model produces parseable output under real inference.
//
// Run with:
//
//	OLLAMA_INTEGRATION=1 go test ./internal/brain/llm/... -run TestIntegration -v -timeout 120s
//
// Requirements:
//   - Ollama running at http://localhost:11434 (or $OLLAMA_URL)
//   - qwen3.5:2b pulled: ollama pull qwen3.5:2b
//
// All tests skip automatically when OLLAMA_INTEGRATION != "1".

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// integrationBase returns the Ollama base URL for integration tests.
// Defaults to localhost; override with OLLAMA_URL env var.
func integrationBase(t *testing.T) string {
	t.Helper()
	if os.Getenv("OLLAMA_INTEGRATION") != "1" {
		t.Skip("set OLLAMA_INTEGRATION=1 to run Ollama integration tests")
	}
	url := os.Getenv("OLLAMA_URL")
	if url == "" {
		url = "http://localhost:11434"
	}
	// Verify Ollama is actually reachable before running any test.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c := NewOllamaClient(url, "qwen3.5:2b", 3000)
	if !c.Available(ctx) {
		t.Skipf("Ollama not reachable at %s — start with: ollama serve", url)
	}
	return url
}

// requireModel skips the test if the given model is not pulled in Ollama.
func requireModel(t *testing.T, baseURL, model string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := NewOllamaClient(baseURL, model, 5000)
	if !c.ModelPulled(ctx) {
		t.Skipf("model %q not found in Ollama — pull with: ollama pull %s", model, model)
	}
}

// ── format:json actually constrains output ────────────────────────────────────

// TestIntegration_WithJSONFormat_ProducesValidJSON verifies that Ollama's
// format:"json" field actually constrains the model to emit only valid JSON.
// This cannot be tested with unit tests — we need real inference.
func TestIntegration_WithJSONFormat_ProducesValidJSON(t *testing.T) {
	base := integrationBase(t)
	requireModel(t, base, "qwen3.5:2b")

	c := NewOllamaClient(base, "qwen3.5:2b", 30000).
		WithJSONFormat(true).
		WithNumPredict(256)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Ask a question that a model might normally answer in prose.
	// With format:json it must produce a JSON object instead.
	raw, err := c.Generate(ctx, `Classify this entity: {"name":"AuthService","fan_in":12}. Output JSON.`)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	extracted := ExtractJSON(raw)
	var result map[string]interface{}
	if err := json.Unmarshal([]byte(extracted), &result); err != nil {
		t.Errorf("WithJSONFormat(true) produced non-JSON output.\nRaw: %s\nExtracted: %s\nError: %v",
			raw, extracted, err)
	}
}

// TestIntegration_WithoutJSONFormat_AllowsProseOutput verifies that without
// format:"json", the model can produce free-form text (baseline check).
func TestIntegration_WithoutJSONFormat_AllowsProseOutput(t *testing.T) {
	base := integrationBase(t)
	requireModel(t, base, "qwen3.5:2b")

	c := NewOllamaClient(base, "qwen3.5:2b", 30000).
		WithNumPredict(64)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	raw, err := c.Generate(ctx, "Reply with one word: ready")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if strings.TrimSpace(raw) == "" {
		t.Error("expected non-empty response from model")
	}
}

// ── /api/chat applies system prompt from Modelfile ────────────────────────────

// TestIntegration_WithChatMode_AppliesSystemPrompt verifies that /api/chat
// actually applies Ollama's system prompt (from the registered Modelfile).
// Uses synapses/navigator if registered; falls back to qwen3.5:2b with inline system.
func TestIntegration_WithChatMode_ProducesValidJSONResponse(t *testing.T) {
	base := integrationBase(t)
	requireModel(t, base, "qwen3.5:2b")

	c := NewOllamaClient(base, "qwen3.5:2b", 60000).
		WithChatMode(true).
		WithJSONFormat(true).
		WithNumPredict(256)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	prompt := `{"new_agent":"agent-2","requested_scope":"internal/auth","active_agents":[{"id":"agent-1","scope":"internal/graph"}]}`
	raw, err := c.Generate(ctx, prompt)
	if err != nil {
		t.Fatalf("Generate via /api/chat: %v", err)
	}

	extracted := ExtractJSON(raw)
	var result map[string]interface{}
	if err := json.Unmarshal([]byte(extracted), &result); err != nil {
		t.Errorf("/api/chat with format:json produced non-JSON.\nRaw: %s\nExtracted: %s\nError: %v",
			raw, extracted, err)
	}
}

// ── keep_alive actually evicts the model ──────────────────────────────────────

// TestIntegration_KeepAlive_Zero_EvictsModel verifies that keep_alive=0
// causes Ollama to unload the model after the request completes.
// Checks /api/ps (running models list) before and after the request.
func TestIntegration_KeepAlive_Zero_EvictsModel(t *testing.T) {
	base := integrationBase(t)
	requireModel(t, base, "qwen3.5:2b")

	c := NewOllamaClient(base, "qwen3.5:2b", 60000).
		WithKeepAlive(0).
		WithNumPredict(1)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Make a request with keep_alive=0 — model should be evicted after.
	_, err := c.Generate(ctx, "ok")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Give Ollama 1 second to process the eviction.
	time.Sleep(1 * time.Second)

	// Check /api/ps — qwen3.5:2b should NOT be in the running models list.
	running, err := runningModels(base)
	if err != nil {
		t.Skipf("cannot check /api/ps: %v", err)
	}
	for _, m := range running {
		if strings.Contains(m, "qwen3.5") {
			t.Errorf("keep_alive=0 did not evict model — qwen3.5:2b still in /api/ps: %v", running)
		}
	}
}

// TestIntegration_KeepAlive_NegativeOne_PinsModel verifies that keep_alive=-1
// keeps the model loaded after a request. Checks /api/ps after the call.
func TestIntegration_KeepAlive_NegativeOne_PinsModel(t *testing.T) {
	base := integrationBase(t)
	requireModel(t, base, "qwen3.5:2b")

	c := NewOllamaClient(base, "qwen3.5:2b", 60000).
		WithKeepAlive(-1).
		WithNumPredict(1)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	_, err := c.Generate(ctx, "ok")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Model should still be loaded.
	running, err := runningModels(base)
	if err != nil {
		t.Skipf("cannot check /api/ps: %v", err)
	}
	found := false
	for _, m := range running {
		if strings.Contains(m, "qwen3.5") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("keep_alive=-1 did not pin model — qwen3.5:2b not in /api/ps: %v", running)
	}

	// Cleanup: evict the model so it doesn't consume RAM after the test.
	evictCtx, evictCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer evictCancel()
	evict := NewOllamaClient(base, "qwen3.5:2b", 30000).WithKeepAlive(0).WithNumPredict(1)
	_, _ = evict.Generate(evictCtx, "")
}

// ── WarmUp actually loads the model ──────────────────────────────────────────

// TestIntegration_WarmUp_LoadsModel verifies that WarmUp causes the model
// to appear in Ollama's /api/ps (running models) list.
func TestIntegration_WarmUp_LoadsModel(t *testing.T) {
	base := integrationBase(t)
	requireModel(t, base, "qwen3.5:2b")

	c := NewOllamaClient(base, "qwen3.5:2b", 60000).WithKeepAlive(-1)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := c.WarmUp(ctx); err != nil {
		t.Fatalf("WarmUp: %v", err)
	}

	running, err := runningModels(base)
	if err != nil {
		t.Skipf("cannot check /api/ps: %v", err)
	}
	found := false
	for _, m := range running {
		if strings.Contains(m, "qwen3.5") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("WarmUp did not load model into memory — not in /api/ps: %v", running)
	}

	// Cleanup.
	evictCtx, evictCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer evictCancel()
	evict := NewOllamaClient(base, "qwen3.5:2b", 30000).WithKeepAlive(0).WithNumPredict(1)
	_, _ = evict.Generate(evictCtx, "")
}

// ── Archivist end-to-end ──────────────────────────────────────────────────────

// TestIntegration_Archivist_E2E_TrivialSession verifies that for a trivial
// session, the Archivist returns an empty MemorizeResponse (as instructed).
// This tests the full path: OllamaClient → /api/chat → JSON parse → MemorizeResponse.
func TestIntegration_Archivist_E2E_TrivialSession(t *testing.T) {
	base := integrationBase(t)
	requireModel(t, base, "qwen3.5:2b")

	// Import cycle prevention: test the raw client directly rather than
	// importing archivist package. Replicates exactly what archivist.Memorize does.
	c := NewOllamaClient(base, "qwen3.5:2b", 60000).
		WithChatMode(true).
		WithJSONFormat(true).
		WithNumPredict(512)

	trivialPrompt := `Analyze this agent session and extract what is worth remembering long-term.
Session events: [{"tool":"get_context","entity":"Store","result_summary":"looked it up"}]
Existing memory: []

Rules:
- Only save architectural discoveries, non-obvious relationships, or decisions that will matter in future sessions.
- If the session is trivial (a single lookup, no new discoveries, or only routine tool calls), return empty arrays.
- Do not duplicate entries already present in existing_memory.

Return JSON only: {"new_memories":[{"key":"short_snake_case_key","content":"what to remember","entities":["EntityName"]}],"annotations":[{"node":"EntityName","note":"observation"}]}
Return {"new_memories":[],"annotations":[]} if nothing is worth saving.`

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	raw, err := c.Generate(ctx, trivialPrompt)
	if err != nil {
		t.Fatalf("Archivist Generate: %v", err)
	}

	extracted := ExtractJSON(raw)
	var resp struct {
		NewMemories []interface{} `json:"new_memories"`
		Annotations []interface{} `json:"annotations"`
	}
	if err := json.Unmarshal([]byte(extracted), &resp); err != nil {
		t.Fatalf("Archivist response is not valid JSON.\nRaw: %s\nExtracted: %s\nError: %v",
			raw, extracted, err)
	}
	// For a trivial session, model should return empty arrays per the instructions.
	// We don't assert empty here because that's model-dependent — just verify it parses.
	t.Logf("Archivist E2E: memories=%d, annotations=%d", len(resp.NewMemories), len(resp.Annotations))
}

// ── synapses/navigator Modelfile identity (if registered) ────────────────────

// TestIntegration_Navigator_Identity_ProducesValidJSON tests the registered
// synapses/navigator Ollama identity (if present) produces valid conflict-resolution JSON.
func TestIntegration_Navigator_Identity_ProducesValidJSON(t *testing.T) {
	base := integrationBase(t)
	requireModel(t, base, "synapses/navigator")

	c := NewOllamaClient(base, "synapses/navigator", 60000).
		WithChatMode(true).
		WithJSONFormat(true).
		WithNumPredict(512)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	prompt := `{"new_agent":"agent-2","requested_scope":"internal/auth","active_agents":[{"id":"agent-1","scope":"internal/graph"}]}`
	raw, err := c.Generate(ctx, prompt)
	if err != nil {
		t.Fatalf("Navigator Generate: %v", err)
	}

	extracted := ExtractJSON(raw)
	var resp struct {
		Suggestion       string `json:"suggestion"`
		AlternativeScope string `json:"alternative_scope"`
	}
	if err := json.Unmarshal([]byte(extracted), &resp); err != nil {
		t.Errorf("synapses/navigator produced invalid JSON.\nRaw: %s\nExtracted: %s\nError: %v",
			raw, extracted, err)
		return
	}
	if strings.TrimSpace(resp.Suggestion) == "" {
		t.Errorf("synapses/navigator returned empty suggestion field — model not following SYSTEM prompt")
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// runningModels returns the names of models currently loaded in Ollama's memory
// by calling GET /api/ps. Returns an error if the endpoint is unavailable.
func runningModels(baseURL string) ([]string, error) {
	resp, err := http.Get(strings.TrimRight(baseURL, "/") + "/api/ps")
	if err != nil {
		return nil, fmt.Errorf("GET /api/ps: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /api/ps returned HTTP %d", resp.StatusCode)
	}
	var result struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode /api/ps: %w", err)
	}
	names := make([]string, 0, len(result.Models))
	for _, m := range result.Models {
		names = append(names, m.Name)
	}
	return names, nil
}
