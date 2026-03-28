package llm

import (
	"context"
	"testing"
)

// ============================================================
// LocalClient.Generate — cancelled context exercises ctx.Done branch
// ============================================================

func TestLocalClient_Generate_CancelledContext(t *testing.T) {
	// available=true + model non-nil: passes early-exit check.
	// Cancelled context: the select case <-ctx.Done() fires.
	c := &LocalClient{
		available: true,
		model:     struct{}{}, // non-nil interface{} to pass nil guard
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	_, err := c.Generate(ctx, "prompt")
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

// ============================================================
// LocalClient.Generate — active context calls c.generate() (stub path)
// ============================================================

func TestLocalClient_Generate_CallsStub(t *testing.T) {
	// available=true + model non-nil + active context → calls c.generate() which is
	// the stub (local_stub.go) and returns errCGODisabled.
	c := &LocalClient{
		available: true,
		model:     struct{}{}, // non-nil
	}
	_, err := c.Generate(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected errCGODisabled from stub generate")
	}
}

// ============================================================
// LocalClient.Available — available=true but llamaCtx=nil → false
// ============================================================

func TestLocalClient_Available_TrueButNilCtx(t *testing.T) {
	c := &LocalClient{available: true, llamaCtx: nil}
	if c.Available(context.Background()) {
		t.Error("Available() should return false when llamaCtx is nil")
	}
}

// ============================================================
// LocalClient.Available — available=true and llamaCtx non-nil → true
// ============================================================

func TestLocalClient_Available_TrueAndCtxSet(t *testing.T) {
	c := &LocalClient{available: true, llamaCtx: struct{}{}}
	if !c.Available(context.Background()) {
		t.Error("Available() should return true when available=true and llamaCtx non-nil")
	}
}
