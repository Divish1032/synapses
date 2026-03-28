package orchestrator

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/brain/llm"
)

func TestCoordinate_Success(t *testing.T) {
	mock := llm.NewMockClient(`{"suggestion": "Agent A owns the auth package. Focus on the handlers/ directory instead.", "alternative_scope": "handlers/"}`)
	o := New(mock, 3*time.Second)

	resp, err := o.Coordinate(context.Background(), Request{
		NewAgentID: "agent-b",
		NewScope:   "internal/auth/",
		ConflictingClaims: []WorkClaim{
			{AgentID: "agent-a", Scope: "internal/auth/service.go", ScopeType: "file"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Suggestion == "" {
		t.Error("expected non-empty suggestion")
	}
	if resp.AlternativeScope == "" {
		t.Error("expected non-empty alternative scope")
	}
}

func TestCoordinate_FallbackOnBadJSON(t *testing.T) {
	mock := llm.NewMockClient(`this is not json`)
	o := New(mock, 3*time.Second)

	resp, err := o.Coordinate(context.Background(), Request{
		NewAgentID: "agent-b",
		NewScope:   "internal/auth/",
		ConflictingClaims: []WorkClaim{
			{AgentID: "agent-a", Scope: "internal/auth/", ScopeType: "directory"},
		},
	})
	if err != nil {
		t.Fatalf("expected no error with fallback, got: %v", err)
	}
	if resp.Suggestion == "" {
		t.Error("expected fallback suggestion to be non-empty")
	}
}

func TestCoordinate_NoConflicts_Fallback(t *testing.T) {
	mock := llm.NewMockClient(`this is not json`)
	o := New(mock, 3*time.Second)

	resp, err := o.Coordinate(context.Background(), Request{
		NewAgentID:        "agent-b",
		NewScope:          "handlers/",
		ConflictingClaims: nil,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Suggestion == "" {
		t.Error("expected non-empty suggestion")
	}
}

func TestNew_DefaultTimeout(t *testing.T) {
	// Test that timeout <= 0 defaults to 3 seconds
	o := New(nil, 0)
	if o.timeout != 3*time.Second {
		t.Errorf("New(_, 0) should default to 3s timeout, got %v", o.timeout)
	}

	o = New(nil, -1)
	if o.timeout != 3*time.Second {
		t.Errorf("New(_, -1) should default to 3s timeout, got %v", o.timeout)
	}
}

func TestNew_CustomTimeout(t *testing.T) {
	// Test that positive timeout is preserved
	custom := 5 * time.Second
	o := New(nil, custom)
	if o.timeout != custom {
		t.Errorf("New(_, %v) should use provided timeout, got %v", custom, o.timeout)
	}
}

func TestDeterministicCoordinate_NoConflicts(t *testing.T) {
	resp := DeterministicCoordinate(Request{
		NewAgentID:        "agent-b",
		NewScope:          "my-scope",
		ConflictingClaims: []WorkClaim{},
	})
	if resp.Suggestion == "" {
		t.Error("expected non-empty suggestion for no conflicts")
	}
	if resp.AlternativeScope != "my-scope" {
		t.Errorf("expected AlternativeScope to be 'my-scope', got %q", resp.AlternativeScope)
	}
}

func TestDeterministicCoordinate_WithConflicts(t *testing.T) {
	resp := DeterministicCoordinate(Request{
		NewAgentID: "agent-b",
		NewScope:   "my-scope",
		ConflictingClaims: []WorkClaim{
			{AgentID: "agent-a", Scope: "conflicting-scope", ScopeType: "file"},
		},
	})
	if resp.Suggestion == "" {
		t.Error("expected non-empty suggestion for conflicts")
	}
	if resp.AlternativeScope != "" {
		t.Errorf("expected AlternativeScope to be empty, got %q", resp.AlternativeScope)
	}
}

func TestCoordinate_LLMError(t *testing.T) {
	// Mock client that returns an error
	errMock := &llm.MockClient{
		Response: "",
		Err:      fmt.Errorf("llm unavailable"),
	}
	o := New(errMock, 3*time.Second)

	resp, err := o.Coordinate(context.Background(), Request{
		NewAgentID: "agent-b",
		NewScope:   "internal/auth/",
		ConflictingClaims: []WorkClaim{
			{AgentID: "agent-a", Scope: "internal/auth/", ScopeType: "directory"},
		},
	})
	if err == nil {
		t.Error("expected error when LLM fails, got nil")
	}
	// Response should be zero value
	if resp.Suggestion != "" {
		t.Errorf("expected empty suggestion on error, got %q", resp.Suggestion)
	}
}

func TestCoordinate_EmptySuggestionFallback(t *testing.T) {
	// LLM returns JSON with empty suggestion
	mock := llm.NewMockClient(`{"suggestion": "   ", "alternative_scope": "scope"}`)
	o := New(mock, 3*time.Second)

	resp, err := o.Coordinate(context.Background(), Request{
		NewAgentID: "agent-b",
		NewScope:   "internal/auth/",
		ConflictingClaims: []WorkClaim{
			{AgentID: "agent-a", Scope: "internal/auth/", ScopeType: "directory"},
		},
	})
	// Should fall back to deterministic response since suggestion is empty after trim
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Suggestion == "" {
		t.Error("expected fallback suggestion to be non-empty")
	}
}
