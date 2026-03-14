package orchestrator

import (
	"context"
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
