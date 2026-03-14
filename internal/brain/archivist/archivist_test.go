package archivist

import (
	"context"
	"errors"
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

	resp, err := a.Memorize(context.Background(), MemorizeRequest{
		SessionEvents: []SessionEvent{{Tool: "get_context", Entity: "Foo"}},
	})
	if err != nil {
		t.Fatalf("expected no error for non-JSON response, got: %v", err)
	}
	if len(resp.NewMemories) != 0 {
		t.Errorf("NewMemories = %v, want empty", resp.NewMemories)
	}
	if len(resp.Annotations) != 0 {
		t.Errorf("Annotations = %v, want empty", resp.Annotations)
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

// --- UnavailableMockClient does not prevent Memorize from being called ---
// (LLMClient.Available is not checked by Memorize — it delegates straight to Generate)

func TestMemorize_UnavailableMock_StillCallsGenerate(t *testing.T) {
	t.Parallel()

	// NewUnavailableMockClient has available=false but Err is nil and Response is "".
	// Generate() returns ("", nil) → JSON parse fails → empty response, no error.
	client := llm.NewUnavailableMockClient()
	a := New(client, 5*time.Second)

	resp, err := a.Memorize(context.Background(), MemorizeRequest{})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	// Empty string is not valid JSON → falls through to empty response.
	if len(resp.NewMemories) != 0 || len(resp.Annotations) != 0 {
		t.Errorf("expected empty response from unavailable mock, got: %+v", resp)
	}
}
