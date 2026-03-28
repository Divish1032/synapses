package store_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/store"
)

func TestInsertContextDelivery_HappyPath(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Must not panic or error.
	st.InsertContextDelivery(store.ContextDelivery{
		SessionID: "sess-1",
		AgentID:   "agent-1",
		ToolName:  "get_context",
		Entity:    "AuthService",
		Refetched: false,
	})
}

func TestInsertContextDelivery_RefetchedFlag(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// First call — not a refetch.
	st.InsertContextDelivery(store.ContextDelivery{
		SessionID: "sess-2",
		ToolName:  "get_context",
		Entity:    "TokenValidator",
		Refetched: false,
	})
	// Second call — refetch.
	st.InsertContextDelivery(store.ContextDelivery{
		SessionID: "sess-2",
		ToolName:  "get_context",
		Entity:    "TokenValidator",
		Refetched: true,
	})
}

func TestCorrelateSessionOutcome_UpdatesRows(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Insert two deliveries for the same session.
	st.InsertContextDelivery(store.ContextDelivery{
		SessionID: "sess-3",
		ToolName:  "get_context",
		Entity:    "PaymentService",
	})
	st.InsertContextDelivery(store.ContextDelivery{
		SessionID: "sess-3",
		ToolName:  "prepare_context",
		Entity:    "InvoiceHandler",
	})
	// Unrelated session — must NOT be updated.
	st.InsertContextDelivery(store.ContextDelivery{
		SessionID: "sess-other",
		ToolName:  "get_context",
		Entity:    "Unrelated",
	})

	n, err := st.CorrelateSessionOutcome("sess-3", "success")
	if err != nil {
		t.Fatalf("CorrelateSessionOutcome: %v", err)
	}
	if n != 2 {
		t.Errorf("rows updated: want 2, got %d", n)
	}
}

func TestCorrelateSessionOutcome_IdempotentOnAlreadySet(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	st.InsertContextDelivery(store.ContextDelivery{
		SessionID: "sess-4",
		ToolName:  "get_context",
		Entity:    "FooService",
	})

	n1, err := st.CorrelateSessionOutcome("sess-4", "success")
	if err != nil {
		t.Fatalf("first correlate: %v", err)
	}
	if n1 != 1 {
		t.Errorf("first correlate: want 1 row updated, got %d", n1)
	}

	// Second call with a different outcome — rows already set should not change.
	n2, err := st.CorrelateSessionOutcome("sess-4", "unknown")
	if err != nil {
		t.Fatalf("second correlate: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second correlate: expected 0 rows updated (already set), got %d", n2)
	}
}

func TestInsertContextDelivery_EmptySessionID(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Deliveries with no session ID are valid — stdio paths have no session ID.
	st.InsertContextDelivery(store.ContextDelivery{
		SessionID: "",
		AgentID:   "solo-agent",
		ToolName:  "get_context",
		Entity:    "Standalone",
	})

	// CorrelateSessionOutcome with empty session ID is a no-op — must not error.
	n, err := st.CorrelateSessionOutcome("", "success")
	if err != nil {
		t.Fatalf("CorrelateSessionOutcome empty: %v", err)
	}
	if n != 0 {
		t.Errorf("empty session_id correlate: expected 0 rows, got %d", n)
	}
}

func TestInsertContextDelivery_EmptyToolName_Skipped(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Empty ToolName must be silently skipped — no row inserted.
	st.InsertContextDelivery(store.ContextDelivery{
		SessionID: "sess-bad",
		ToolName:  "",
		Entity:    "SomeEntity",
	})

	// Verify no rows were inserted by correlating — should update 0 rows.
	n, err := st.CorrelateSessionOutcome("sess-bad", "success")
	if err != nil {
		t.Fatalf("CorrelateSessionOutcome: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 rows for empty ToolName, got %d", n)
	}
}

func TestInsertContextDelivery_NilStore(t *testing.T) {
	t.Parallel()
	// Must not panic when store is nil.
	var st *store.Store
	st.InsertContextDelivery(store.ContextDelivery{ToolName: "get_context", Entity: "X"})

	n, err := st.CorrelateSessionOutcome("sess", "success")
	if err != nil {
		t.Errorf("nil store CorrelateSessionOutcome: unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("nil store CorrelateSessionOutcome: want 0, got %d", n)
	}
}
