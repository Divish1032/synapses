package archivist

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/brain/llm"
)

// --- New ---

func TestNew_PositiveTimeout(t *testing.T) {
	t.Parallel()
	client := llm.NewMockClient("")
	a := New(client, 10*time.Second)
	if a == nil {
		t.Fatal("expected non-nil Archivist")
	}
	if a.timeout != 10*time.Second {
		t.Errorf("timeout = %v, want 10s", a.timeout)
	}
}

func TestNew_ZeroTimeout_UsesDefault(t *testing.T) {
	t.Parallel()
	client := llm.NewMockClient("")
	a := New(client, 0)
	if a == nil {
		t.Fatal("expected non-nil Archivist")
	}
	// Zero timeout must be replaced with the 30s default.
	if a.timeout != 30*time.Second {
		t.Errorf("timeout = %v, want 30s (default)", a.timeout)
	}
}

func TestNew_NegativeTimeout_UsesDefault(t *testing.T) {
	t.Parallel()
	client := llm.NewMockClient("")
	a := New(client, -5*time.Second)
	if a.timeout != 30*time.Second {
		t.Errorf("timeout = %v, want 30s (default)", a.timeout)
	}
}

// --- Memorize: valid JSON response ---

func TestMemorize_ValidJSON_ParsesMemoriesAndAnnotations(t *testing.T) {
	t.Parallel()

	response := `{"new_memories":[{"key":"auth-pattern","content":"AuthHandler always co-changes with TokenStore","entities":["AuthHandler","TokenStore"]}],"annotations":[{"node":"AuthHandler","note":"Always update TokenStore together"}]}`
	client := llm.NewMockClient(response)
	a := New(client, 5*time.Second)

	req := MemorizeRequest{
		SessionEvents: []SessionEvent{
			{Tool: "get_context", Entity: "AuthHandler", Result: "auth handler code"},
			{Tool: "get_context", Entity: "TokenStore", Result: "token store code"},
		},
		ExistingMemory: []string{"some previous memory"},
	}

	resp, err := a.Memorize(context.Background(), req)
	if err != nil {
		t.Fatalf("Memorize: unexpected error: %v", err)
	}

	if len(resp.NewMemories) != 1 {
		t.Fatalf("NewMemories len = %d, want 1", len(resp.NewMemories))
	}
	mem := resp.NewMemories[0]
	if mem.Key != "auth-pattern" {
		t.Errorf("Key = %q, want auth-pattern", mem.Key)
	}
	if mem.Content != "AuthHandler always co-changes with TokenStore" {
		t.Errorf("Content = %q", mem.Content)
	}
	if len(mem.Entities) != 2 || mem.Entities[0] != "AuthHandler" || mem.Entities[1] != "TokenStore" {
		t.Errorf("Entities = %v", mem.Entities)
	}

	if len(resp.Annotations) != 1 {
		t.Fatalf("Annotations len = %d, want 1", len(resp.Annotations))
	}
	ann := resp.Annotations[0]
	if ann.Node != "AuthHandler" {
		t.Errorf("annotation Node = %q, want AuthHandler", ann.Node)
	}
	if ann.Note != "Always update TokenStore together" {
		t.Errorf("annotation Note = %q", ann.Note)
	}
}

// --- Memorize: LLM error ---

func TestMemorize_LLMError_ReturnsError(t *testing.T) {
	t.Parallel()

	client := llm.NewMockClient("")
	client.Err = errors.New("connection refused")
	a := New(client, 5*time.Second)

	_, err := a.Memorize(context.Background(), MemorizeRequest{})
	if err == nil {
		t.Fatal("expected error from LLM failure, got nil")
	}
	if !strings.Contains(err.Error(), "archivist") {
		t.Errorf("error should be wrapped with archivist prefix, got: %v", err)
	}
}

// --- Memorize: non-JSON response ---

func TestMemorize_NonJSON_ReturnsEmptyNoError(t *testing.T) {
	t.Parallel()

	client := llm.NewMockClient("Sorry, I cannot help with that request.")
	a := New(client, 5*time.Second)

	_, err := a.Memorize(context.Background(), MemorizeRequest{
		SessionEvents: []SessionEvent{{Tool: "get_context", Entity: "Foo"}},
	})
	if err == nil {
		t.Fatalf("expected parse error for non-JSON response, got nil")
	}
}

// --- Memorize: empty JSON ---

func TestMemorize_EmptyJSON_ReturnsEmptyNoError(t *testing.T) {
	t.Parallel()

	client := llm.NewMockClient(`{"new_memories":[],"annotations":[]}`)
	a := New(client, 5*time.Second)

	resp, err := a.Memorize(context.Background(), MemorizeRequest{})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(resp.NewMemories) != 0 {
		t.Errorf("NewMemories = %v, want empty", resp.NewMemories)
	}
	if len(resp.Annotations) != 0 {
		t.Errorf("Annotations = %v, want empty", resp.Annotations)
	}
}

// --- buildMemorizePrompt (tested indirectly via Memorize) ---

// capturingClient records the prompt passed to Generate so we can inspect it.
type capturingClient struct {
	*llm.MockClient
	capturedPrompt string
}

func (c *capturingClient) Generate(ctx context.Context, prompt string) (string, error) {
	c.capturedPrompt = prompt
	return c.MockClient.Generate(ctx, prompt)
}

func TestMemorize_PromptContainsEvents(t *testing.T) {
	t.Parallel()

	mock := llm.NewMockClient(`{"new_memories":[],"annotations":[]}`)
	capturing := &capturingClient{MockClient: mock}
	a := New(capturing, 5*time.Second)

	req := MemorizeRequest{
		SessionEvents: []SessionEvent{
			{Tool: "get_context", Entity: "Store", Result: "store code"},
		},
		ExistingMemory: []string{"memory1"},
	}

	_, _ = a.Memorize(context.Background(), req)

	if capturing.capturedPrompt == "" {
		t.Fatal("expected prompt to be captured, got empty string")
	}
	// The prompt must include the event tool name and entity.
	if !strings.Contains(capturing.capturedPrompt, "get_context") {
		t.Errorf("prompt missing event tool name; prompt = %q", capturing.capturedPrompt)
	}
	if !strings.Contains(capturing.capturedPrompt, "Store") {
		t.Errorf("prompt missing event entity; prompt = %q", capturing.capturedPrompt)
	}
	// The prompt must include existing memory.
	if !strings.Contains(capturing.capturedPrompt, "memory1") {
		t.Errorf("prompt missing existing memory; prompt = %q", capturing.capturedPrompt)
	}
}

// --- Memorize: markdown-fenced JSON (ExtractJSON regression) ---
// This is the regression test for the silent parse failure bug:
// Without llm.ExtractJSON, a ```json ... ``` wrapped response would fail
// json.Unmarshal silently, returning empty memories with no circuit breaker trip.

func TestMemorize_MarkdownFencedJSON_ParsesCorrectly(t *testing.T) {
	t.Parallel()

	// Simulate a model that wraps JSON in markdown fences despite format:json being set.
	// This can happen with older Ollama versions or when the model falls back.
	fencedResponse := "```json\n" +
		`{"new_memories":[{"key":"fence-test","content":"fenced memory content","entities":["FooService"]}],"annotations":[]}` +
		"\n```"

	client := llm.NewMockClient(fencedResponse)
	a := New(client, 5*time.Second)

	resp, err := a.Memorize(context.Background(), MemorizeRequest{
		SessionEvents: []SessionEvent{{Tool: "get_context", Entity: "FooService"}},
	})
	if err != nil {
		t.Fatalf("expected no error for markdown-fenced JSON, got: %v", err)
	}
	// Without ExtractJSON the unmarshal would fail silently and return empty.
	// With the fix, the memory must be parsed correctly.
	if len(resp.NewMemories) != 1 {
		t.Fatalf("NewMemories len = %d, want 1 — ExtractJSON is not stripping markdown fences", len(resp.NewMemories))
	}
	if resp.NewMemories[0].Key != "fence-test" {
		t.Errorf("Key = %q, want fence-test", resp.NewMemories[0].Key)
	}
	if resp.NewMemories[0].Content != "fenced memory content" {
		t.Errorf("Content = %q", resp.NewMemories[0].Content)
	}
	if len(resp.NewMemories[0].Entities) != 1 || resp.NewMemories[0].Entities[0] != "FooService" {
		t.Errorf("Entities = %v, want [FooService]", resp.NewMemories[0].Entities)
	}
}

func TestMemorize_PreambleTextBeforeJSON_ParsesCorrectly(t *testing.T) {
	t.Parallel()

	// Some models emit a preamble sentence before the JSON object.
	responseWithPreamble := `Here is the session summary: {"new_memories":[{"key":"preamble-test","content":"preamble memory","entities":[]}],"annotations":[]}`
	client := llm.NewMockClient(responseWithPreamble)
	a := New(client, 5*time.Second)

	resp, err := a.Memorize(context.Background(), MemorizeRequest{
		SessionEvents: []SessionEvent{{Tool: "search", Entity: "Store"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.NewMemories) != 1 {
		t.Fatalf("NewMemories len = %d, want 1 — ExtractJSON is not stripping preamble text", len(resp.NewMemories))
	}
	if resp.NewMemories[0].Key != "preamble-test" {
		t.Errorf("Key = %q, want preamble-test", resp.NewMemories[0].Key)
	}
}

// --- UnavailableMockClient does not prevent Memorize from being called ---
// (LLMClient.Available is not checked by Memorize — it delegates straight to Generate)

func TestMemorize_UnavailableMock_StillCallsGenerate(t *testing.T) {
	t.Parallel()

	// NewUnavailableMockClient has available=false but Err is nil and Response is "".
	// Generate() returns ("", nil) → JSON parse fails → empty response, no error.
	client := llm.NewUnavailableMockClient()
	a := New(client, 5*time.Second)

	_, err := a.Memorize(context.Background(), MemorizeRequest{})
	if err == nil {
		t.Fatalf("expected parse error from unavailable mock, got nil")
	}
}

// --- Entities parsing: string vs array ---

func TestMemorize_EntitiesAsString_ParsedCorrectly(t *testing.T) {
	t.Parallel()
	response := `{"new_memories":[{"key":"hub","content":"hub node","entities":"AuthService,TokenStore"}],"annotations":[]}`
	client := llm.NewMockClient(response)
	a := New(client, 5*time.Second)

	resp, err := a.Memorize(context.Background(), MemorizeRequest{
		SessionEvents: []SessionEvent{{Tool: "get_context", Entity: "AuthService"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.NewMemories) != 1 {
		t.Fatalf("NewMemories len = %d, want 1", len(resp.NewMemories))
	}
	entities := resp.NewMemories[0].Entities
	if len(entities) != 2 || entities[0] != "AuthService" || entities[1] != "TokenStore" {
		t.Errorf("Entities = %v, want [AuthService TokenStore]", entities)
	}
}

func TestMemorize_EntitiesAsArray_StillWorks(t *testing.T) {
	t.Parallel()
	response := `{"new_memories":[{"key":"hub","content":"hub node","entities":["AuthService"]}],"annotations":[]}`
	client := llm.NewMockClient(response)
	a := New(client, 5*time.Second)

	resp, err := a.Memorize(context.Background(), MemorizeRequest{
		SessionEvents: []SessionEvent{{Tool: "get_context", Entity: "AuthService"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.NewMemories) != 1 {
		t.Fatalf("NewMemories len = %d, want 1", len(resp.NewMemories))
	}
	entities := resp.NewMemories[0].Entities
	if len(entities) != 1 || entities[0] != "AuthService" {
		t.Errorf("Entities = %v, want [AuthService]", entities)
	}
}

// --- Retry-on-empty test (via counting mock) ---

// retryMockClient returns empty JSON on the first call, valid JSON on the second.
type retryMockClient struct {
	calls int
}

func (c *retryMockClient) Generate(_ context.Context, _ string) (string, error) {
	c.calls++
	if c.calls == 1 {
		return `{"new_memories":[],"annotations":[]}`, nil
	}
	return `{"new_memories":[{"key":"retry-hit","content":"found on retry","entities":"Foo"}],"annotations":[]}`, nil
}

func (c *retryMockClient) Available(_ context.Context) bool               { return true }
func (c *retryMockClient) ModelPulled(_ context.Context) bool             { return true }
func (c *retryMockClient) ModelName() string                              { return "mock" }
func (c *retryMockClient) PullModel(_ context.Context, _ io.Writer) error { return nil }

func TestMemorize_RetryMock_SecondCallReturnsData(t *testing.T) {
	t.Parallel()

	mock := &retryMockClient{}
	a := New(mock, 5*time.Second)

	// First call returns empty.
	resp1, err := a.Memorize(context.Background(), MemorizeRequest{
		SessionEvents: []SessionEvent{{Tool: "get_context", Entity: "Foo"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp1.NewMemories) != 0 {
		t.Errorf("first call should return empty, got %d memories", len(resp1.NewMemories))
	}
	if mock.calls != 1 {
		t.Errorf("expected 1 LLM call, got %d", mock.calls)
	}

	// Second call returns data (simulates retry at brain_impl level).
	resp2, err := a.Memorize(context.Background(), MemorizeRequest{
		SessionEvents: []SessionEvent{{Tool: "get_context", Entity: "Foo"}, {Tool: "search", Entity: "Bar"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp2.NewMemories) != 1 {
		t.Fatalf("second call should return 1 memory, got %d", len(resp2.NewMemories))
	}
	if resp2.NewMemories[0].Key != "retry-hit" {
		t.Errorf("Key = %q, want retry-hit", resp2.NewMemories[0].Key)
	}
}

// --- parseEntities: table-driven tests ---

func TestParseEntities(t *testing.T) {
	// Table-driven tests for comprehensive parseEntities coverage.
	// Tests cover all major branches: nil/empty input, array parsing, string parsing,
	// whitespace handling, and invalid input.
	tests := []struct {
		name    string
		raw     json.RawMessage
		want    []string
		wantNil bool // Set true if expecting nil (not empty slice)
	}{
		{
			name:    "nil_input",
			raw:     nil,
			wantNil: true,
		},
		{
			name:    "empty_byte_slice",
			raw:     []byte{},
			wantNil: true,
		},
		{
			name: "array_format_three_items",
			raw:  []byte(`["Service1","Service2","Service3"]`),
			want: []string{"Service1", "Service2", "Service3"},
		},
		{
			name: "array_with_empty_strings",
			raw:  []byte(`["Service1","","Service2"]`),
			want: []string{"Service1", "", "Service2"},
		},
		{
			name: "empty_array",
			raw:  []byte(`[]`),
			want: []string{}, // empty slice, not nil
		},
		{
			name: "string_format_no_whitespace",
			raw:  []byte(`"Service1,Service2,Service3"`),
			want: []string{"Service1", "Service2", "Service3"},
		},
		{
			name: "string_format_with_whitespace",
			raw:  []byte(`"Service1 , Service2 , Service3"`),
			want: []string{"Service1", "Service2", "Service3"},
		},
		{
			name: "string_with_empty_parts_multiple_commas",
			raw:  []byte(`"Service1,,Service2, , Service3"`),
			want: []string{"Service1", "Service2", "Service3"},
		},
		{
			name:    "empty_string_value",
			raw:     []byte(`""`),
			wantNil: true,
		},
		{
			name: "string_with_only_whitespace",
			raw:  []byte(`"   ,  ,   "`),
			want: []string{}, // empty slice after trimming whitespace-only parts
		},
		{
			name:    "invalid_json",
			raw:     []byte(`{not valid json}`),
			wantNil: true,
		},
		{
			name:    "numeric_json",
			raw:     []byte(`123`),
			wantNil: true,
		},
		{
			name: "single_item_array",
			raw:  []byte(`["OnlyOne"]`),
			want: []string{"OnlyOne"},
		},
		{
			name: "single_item_string",
			raw:  []byte(`"OnlyOne"`),
			want: []string{"OnlyOne"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := parseEntities(tt.raw)

			if tt.wantNil {
				if result != nil {
					t.Errorf("parseEntities(%q): got %v, want nil", string(tt.raw), result)
				}
				return
			}

			if result == nil {
				t.Errorf("parseEntities(%q): got nil, want %v", string(tt.raw), tt.want)
				return
			}

			if len(result) != len(tt.want) {
				t.Errorf("parseEntities(%q): got len %d, want %d. Result: %v",
					string(tt.raw), len(result), len(tt.want), result)
				return
			}

			for i, item := range result {
				if item != tt.want[i] {
					t.Errorf("parseEntities(%q)[%d]: got %q, want %q",
						string(tt.raw), i, item, tt.want[i])
				}
			}
		})
	}
}
