package store_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/store"
)

func TestInsertContextDelivery_HappyPath(t *testing.T) {
	st := openTestStore(t)

	st.InsertContextDelivery(store.ContextDelivery{
		SessionID: "sess-1",
		AgentID:   "agent-1",
		ToolName:  "get_context",
		Entity:    "AuthService",
		Refetched: false,
	})

	rows, err := st.GetContextDeliveriesForSession("sess-1")
	if err != nil {
		t.Fatalf("GetContextDeliveriesForSession: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	cd := rows[0]
	if cd.SessionID != "sess-1" {
		t.Errorf("SessionID: want %q got %q", "sess-1", cd.SessionID)
	}
	if cd.AgentID != "agent-1" {
		t.Errorf("AgentID: want %q got %q", "agent-1", cd.AgentID)
	}
	if cd.ToolName != "get_context" {
		t.Errorf("ToolName: want %q got %q", "get_context", cd.ToolName)
	}
	if cd.Entity != "AuthService" {
		t.Errorf("Entity: want %q got %q", "AuthService", cd.Entity)
	}
	if cd.Refetched {
		t.Error("Refetched: want false, got true")
	}
	if cd.TaskOutcome != "" {
		t.Errorf("TaskOutcome: want empty, got %q", cd.TaskOutcome)
	}
}

func TestInsertContextDelivery_RefetchedFlag(t *testing.T) {
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

	rows, err := st.GetContextDeliveriesForSession("sess-2")
	if err != nil {
		t.Fatalf("GetContextDeliveriesForSession: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0].Refetched {
		t.Error("first row: Refetched should be false")
	}
	if !rows[1].Refetched {
		t.Error("second row: Refetched should be true")
	}
}

func TestCorrelateSessionOutcome_UpdatesRows(t *testing.T) {
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

	// Verify rows for sess-3 now carry the outcome.
	rows, err := st.GetContextDeliveriesForSession("sess-3")
	if err != nil {
		t.Fatalf("GetContextDeliveriesForSession: %v", err)
	}
	for _, cd := range rows {
		if cd.TaskOutcome != "success" {
			t.Errorf("row entity=%q: TaskOutcome want %q got %q", cd.Entity, "success", cd.TaskOutcome)
		}
	}

	// Verify unrelated session is untouched.
	other, _ := st.GetContextDeliveriesForSession("sess-other")
	if len(other) == 1 && other[0].TaskOutcome != "" {
		t.Errorf("sess-other row should have empty outcome, got %q", other[0].TaskOutcome)
	}
}

func TestCorrelateSessionOutcome_IdempotentOnAlreadySet(t *testing.T) {
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

	// Verify the original "success" is preserved.
	rows, _ := st.GetContextDeliveriesForSession("sess-4")
	if len(rows) != 1 || rows[0].TaskOutcome != "success" {
		t.Errorf("outcome should still be %q, got %q", "success", rows[0].TaskOutcome)
	}
}

// TestCorrelateSessionOutcome_PartialUpdate covers the case where a session has
// some rows already correlated (e.g., from a prior interrupted end_session) and
// new rows without an outcome. Only uncorrelated rows must be updated.
func TestCorrelateSessionOutcome_PartialUpdate(t *testing.T) {
	st := openTestStore(t)

	// Row 1: already correlated from a prior call.
	st.InsertContextDelivery(store.ContextDelivery{
		SessionID: "sess-partial",
		ToolName:  "get_context",
		Entity:    "AlreadyDone",
	})
	// Manually set its outcome to simulate a prior partial correlation.
	_, err := st.CorrelateSessionOutcome("sess-partial", "success")
	if err != nil {
		t.Fatalf("setup correlate: %v", err)
	}

	// Row 2: inserted after the first correlate (e.g., race between goroutine and end_session).
	st.InsertContextDelivery(store.ContextDelivery{
		SessionID: "sess-partial",
		ToolName:  "prepare_context",
		Entity:    "NewEntity",
	})

	// Second correlate — should only touch the new row.
	n, err := st.CorrelateSessionOutcome("sess-partial", "unknown")
	if err != nil {
		t.Fatalf("second correlate: %v", err)
	}
	if n != 1 {
		t.Errorf("second correlate: want 1 row updated (new row only), got %d", n)
	}

	rows, err := st.GetContextDeliveriesForSession("sess-partial")
	if err != nil {
		t.Fatalf("GetContextDeliveriesForSession: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}

	// Row 1 must still be "success" — not overwritten.
	if rows[0].Entity != "AlreadyDone" || rows[0].TaskOutcome != "success" {
		t.Errorf("row[0]: want entity=AlreadyDone outcome=success, got entity=%q outcome=%q",
			rows[0].Entity, rows[0].TaskOutcome)
	}
	// Row 2 must now be "unknown".
	if rows[1].Entity != "NewEntity" || rows[1].TaskOutcome != "unknown" {
		t.Errorf("row[1]: want entity=NewEntity outcome=unknown, got entity=%q outcome=%q",
			rows[1].Entity, rows[1].TaskOutcome)
	}
}

func TestInsertContextDelivery_EmptySessionID(t *testing.T) {
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
	st := openTestStore(t)

	// Empty ToolName must be silently skipped — no row inserted.
	st.InsertContextDelivery(store.ContextDelivery{
		SessionID: "sess-bad",
		ToolName:  "",
		Entity:    "SomeEntity",
	})

	rows, err := st.GetContextDeliveriesForSession("sess-bad")
	if err != nil {
		t.Fatalf("GetContextDeliveriesForSession: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows for empty ToolName, got %d", len(rows))
	}
}

func TestInsertContextDelivery_NilStore(t *testing.T) {
	// Must not panic when store is nil.
	var st *store.Store
	st.InsertContextDelivery(store.ContextDelivery{ToolName: "get_context", Entity: "X"})

	rows, err := st.GetContextDeliveriesForSession("sess")
	if err != nil {
		t.Errorf("nil store GetContextDeliveriesForSession: unexpected error: %v", err)
	}
	if rows != nil {
		t.Errorf("nil store GetContextDeliveriesForSession: expected nil slice, got %v", rows)
	}

	n, err := st.CorrelateSessionOutcome("sess", "success")
	if err != nil {
		t.Errorf("nil store CorrelateSessionOutcome: unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("nil store CorrelateSessionOutcome: want 0, got %d", n)
	}
}

func TestGetContextDeliveriesForSession_OrderByInsertionOrder(t *testing.T) {
	st := openTestStore(t)

	// Insert three deliveries with the same session.
	// ORDER BY id ASC (not created_at) guarantees stable insertion order
	// even when all rows have the same Unix-second timestamp.
	entities := []string{"Alpha", "Beta", "Gamma"}
	for _, e := range entities {
		st.InsertContextDelivery(store.ContextDelivery{
			SessionID: "sess-order",
			ToolName:  "get_context",
			Entity:    e,
		})
	}

	rows, err := st.GetContextDeliveriesForSession("sess-order")
	if err != nil {
		t.Fatalf("GetContextDeliveriesForSession: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
	for i, e := range entities {
		if rows[i].Entity != e {
			t.Errorf("row[%d]: want entity %q, got %q (ORDER BY id not stable?)", i, e, rows[i].Entity)
		}
	}
}
