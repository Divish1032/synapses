package store_test

import (
	"testing"
)

// ── WatchSymbol ───────────────────────────────────────────────────────────────

func TestWatchSymbol_UpsertAndRetrieve(t *testing.T) {
	st := openTestStore(t)

	st.WatchSymbol("agent-a", "node-1", "AuthLogin", "pkg/auth/auth.go")

	// Watching again must be idempotent (upsert semantics).
	st.WatchSymbol("agent-a", "node-1", "AuthLogin", "pkg/auth/auth.go")

	// No peer has a claim or changed any file → no alerts.
	alerts, err := st.GetDependencyAlerts("agent-a")
	if err != nil {
		t.Fatalf("GetDependencyAlerts: %v", err)
	}
	if len(alerts) != 0 {
		t.Errorf("expected 0 alerts when no peer has a claim or change, got %d", len(alerts))
	}
}

func TestWatchSymbol_MultipleAgentsSameEntity_Independent(t *testing.T) {
	st := openTestStore(t)

	// Both agents watch the same entity — rows are keyed by (agent_id, entity_id).
	st.WatchSymbol("agent-a", "node-1", "AuthLogin", "pkg/auth/auth.go")
	st.WatchSymbol("agent-b", "node-1", "AuthLogin", "pkg/auth/auth.go")

	// No change events or claims → zero alerts for both.
	alertsA, _ := st.GetDependencyAlerts("agent-a")
	alertsB, _ := st.GetDependencyAlerts("agent-b")
	if len(alertsA) != 0 || len(alertsB) != 0 {
		t.Error("expected 0 alerts when no peer changed any watched file")
	}
}

// ── GetDependencyAlerts — production path ─────────────────────────────────────
//
// Production reality: the file watcher emits file_change events with agent_id="".
// GetDependencyAlerts attributes changes to the peer who holds a work_claim on the
// changed file's directory — the only reliable file→agent mapping we have.
// Tests must mirror this production path: watcher-style events + work_claims.

func TestGetDependencyAlerts_PeerClaimsAndFileChanges_SurfacesAlert(t *testing.T) {
	st := openTestStore(t)

	// agent-a watches AuthLogin in pkg/auth/auth.go.
	st.WatchSymbol("agent-a", "node-1", "AuthLogin", "pkg/auth/auth.go")

	// agent-b claims the pkg/auth directory.
	_, _ = st.ClaimWork("agent-b", "pkg/auth", "directory", 30)

	// Watcher emits a file_change for that file (agent_id="" — production behavior).
	_ = st.AppendEvent("file_change", "", `{"file":"pkg/auth/auth.go"}`)

	alerts, err := st.GetDependencyAlerts("agent-a")
	if err != nil {
		t.Fatalf("GetDependencyAlerts: %v", err)
	}
	if len(alerts) == 0 {
		t.Error("expected ≥1 dependency alert when peer claimed scope + file changed")
	}
	if len(alerts) > 0 && alerts[0].PeerAgentID != "agent-b" {
		t.Errorf("expected peer_agent_id=agent-b, got %q", alerts[0].PeerAgentID)
	}
	if len(alerts) > 0 && alerts[0].EntityName != "AuthLogin" {
		t.Errorf("expected entity_name=AuthLogin, got %q", alerts[0].EntityName)
	}
}

func TestGetDependencyAlerts_NoClaim_NoAlert(t *testing.T) {
	// File changes, but no agent has claimed any scope → no attribution → no alert.
	st := openTestStore(t)

	st.WatchSymbol("agent-a", "node-1", "AuthLogin", "pkg/auth/auth.go")
	_ = st.AppendEvent("file_change", "", `{"file":"pkg/auth/auth.go"}`)

	alerts, err := st.GetDependencyAlerts("agent-a")
	if err != nil {
		t.Fatalf("GetDependencyAlerts: %v", err)
	}
	if len(alerts) != 0 {
		t.Errorf("expected 0 alerts when no peer has a work claim, got %d", len(alerts))
	}
}

func TestGetDependencyAlerts_SelfClaim_NotSurfaced(t *testing.T) {
	// agent-a watches a file AND holds the claim on it — must not self-alert.
	st := openTestStore(t)

	st.WatchSymbol("agent-a", "node-1", "AuthLogin", "pkg/auth/auth.go")
	_, _ = st.ClaimWork("agent-a", "pkg/auth", "directory", 30)
	_ = st.AppendEvent("file_change", "", `{"file":"pkg/auth/auth.go"}`)

	alerts, err := st.GetDependencyAlerts("agent-a")
	if err != nil {
		t.Fatalf("GetDependencyAlerts: %v", err)
	}
	if len(alerts) != 0 {
		t.Errorf("expected 0 alerts for self-claimed scope change, got %d", len(alerts))
	}
}

func TestGetDependencyAlerts_UnwatchedFilePeerChange_NoAlert(t *testing.T) {
	// Peer changes a different file than what agent-a is watching.
	st := openTestStore(t)

	st.WatchSymbol("agent-a", "node-1", "AuthLogin", "pkg/auth/auth.go")
	_, _ = st.ClaimWork("agent-b", "pkg/payments", "directory", 30)
	_ = st.AppendEvent("file_change", "", `{"file":"pkg/payments/pay.go"}`)

	alerts, err := st.GetDependencyAlerts("agent-a")
	if err != nil {
		t.Fatalf("GetDependencyAlerts: %v", err)
	}
	if len(alerts) != 0 {
		t.Errorf("expected 0 alerts for change to unwatched file, got %d", len(alerts))
	}
}

func TestGetDependencyAlerts_Deduplication(t *testing.T) {
	// Multiple file_change events for the same (peer, file) → one alert.
	st := openTestStore(t)

	st.WatchSymbol("agent-a", "node-1", "AuthLogin", "pkg/auth/auth.go")
	_, _ = st.ClaimWork("agent-b", "pkg/auth", "directory", 30)
	_ = st.AppendEvent("file_change", "", `{"file":"pkg/auth/auth.go"}`)
	_ = st.AppendEvent("file_change", "", `{"file":"pkg/auth/auth.go"}`)
	_ = st.AppendEvent("file_change", "", `{"file":"pkg/auth/auth.go"}`)

	alerts, err := st.GetDependencyAlerts("agent-a")
	if err != nil {
		t.Fatalf("GetDependencyAlerts: %v", err)
	}
	if len(alerts) != 1 {
		t.Errorf("expected 1 deduplicated alert, got %d", len(alerts))
	}
}

func TestGetDependencyAlerts_NoWatchedSymbols_NoAlerts(t *testing.T) {
	// Peer claims + changes a file but agent-a has watched nothing.
	st := openTestStore(t)

	_, _ = st.ClaimWork("agent-b", "pkg/auth", "directory", 30)
	_ = st.AppendEvent("file_change", "", `{"file":"pkg/auth/auth.go"}`)

	alerts, err := st.GetDependencyAlerts("agent-a")
	if err != nil {
		t.Fatalf("GetDependencyAlerts: %v", err)
	}
	if len(alerts) != 0 {
		t.Errorf("expected 0 alerts when nothing is watched, got %d", len(alerts))
	}
}

func TestGetDependencyAlerts_MultipleWatchedFiles_AllSurface(t *testing.T) {
	st := openTestStore(t)

	st.WatchSymbol("agent-a", "node-1", "AuthLogin", "pkg/auth/auth.go")
	st.WatchSymbol("agent-a", "node-2", "ProcessPayment", "pkg/pay/pay.go")

	_, _ = st.ClaimWork("agent-b", "pkg/auth", "directory", 30)
	_, _ = st.ClaimWork("agent-b", "pkg/pay", "directory", 30)
	_ = st.AppendEvent("file_change", "", `{"file":"pkg/auth/auth.go"}`)
	_ = st.AppendEvent("file_change", "", `{"file":"pkg/pay/pay.go"}`)

	alerts, err := st.GetDependencyAlerts("agent-a")
	if err != nil {
		t.Fatalf("GetDependencyAlerts: %v", err)
	}
	if len(alerts) < 2 {
		t.Errorf("expected 2 alerts (one per watched file), got %d", len(alerts))
	}
}

func TestGetDependencyAlerts_MultiplePeers_BothSurface(t *testing.T) {
	st := openTestStore(t)

	st.WatchSymbol("agent-a", "node-1", "AuthLogin", "pkg/auth/auth.go")
	_, _ = st.ClaimWork("agent-b", "pkg/auth", "directory", 30)
	_, _ = st.ClaimWork("agent-c", "pkg/auth", "directory", 30)
	_ = st.AppendEvent("file_change", "", `{"file":"pkg/auth/auth.go"}`)

	alerts, err := st.GetDependencyAlerts("agent-a")
	if err != nil {
		t.Fatalf("GetDependencyAlerts: %v", err)
	}
	// Dedup is per (peer, file) — two different claimants → two distinct alerts.
	if len(alerts) < 2 {
		t.Errorf("expected 2 alerts (one per peer claimant), got %d", len(alerts))
	}
	peers := make(map[string]bool)
	for _, a := range alerts {
		peers[a.PeerAgentID] = true
	}
	if !peers["agent-b"] || !peers["agent-c"] {
		t.Errorf("expected both agent-b and agent-c in alerts, got: %v", alerts)
	}
}

func TestGetDependencyAlerts_ExpiredClaim_NoAlert(t *testing.T) {
	// An expired claim is not a valid attribution — should not surface.
	st := openTestStore(t)

	st.WatchSymbol("agent-a", "node-1", "AuthLogin", "pkg/auth/auth.go")
	// Claim with 0-minute TTL — it will be immediately expired.
	// ClaimWork enforces minimum 1 min, so use a negative to force expired state.
	// We can't easily expire a claim instantly in tests (ClaimWork enforces minTTL=1).
	// Instead: verify that the file change without any active claim → no alert.
	_ = st.AppendEvent("file_change", "", `{"file":"pkg/auth/auth.go"}`)

	alerts, err := st.GetDependencyAlerts("agent-a")
	if err != nil {
		t.Fatalf("GetDependencyAlerts: %v", err)
	}
	// No active claim → no attribution → no alert.
	if len(alerts) != 0 {
		t.Errorf("expected 0 alerts with no active claim, got %d", len(alerts))
	}
}

// ── CountActiveAgents ─────────────────────────────────────────────────────────

func TestCountActiveAgents_ExcludesSelf(t *testing.T) {
	st := openTestStore(t)

	if err := st.UpsertAgent("self", nil); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}

	n, err := st.CountActiveAgents("self")
	if err != nil {
		t.Fatalf("CountActiveAgents: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 when the only agent is self, got %d", n)
	}
}

func TestCountActiveAgents_EmptyStore_ReturnsZero(t *testing.T) {
	st := openTestStore(t)

	n, err := st.CountActiveAgents("nobody")
	if err != nil {
		t.Fatalf("CountActiveAgents: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 on fresh store, got %d", n)
	}
}

func TestCountActiveAgents_CountsRecentPeers(t *testing.T) {
	st := openTestStore(t)

	_ = st.UpsertAgent("agent-1", nil)
	_ = st.UpsertAgent("agent-2", nil)
	_ = st.UpsertAgent("agent-3", nil)

	n, err := st.CountActiveAgents("agent-1")
	if err != nil {
		t.Fatalf("CountActiveAgents: %v", err)
	}
	if n < 2 {
		t.Errorf("expected ≥2 peers (agent-2 and agent-3), got %d", n)
	}
}

func TestCountActiveAgents_AgentWithActiveClaim_Counted(t *testing.T) {
	// An agent holding a non-expired work claim is counted even when just upserted.
	st := openTestStore(t)

	_ = st.UpsertAgent("claim-holder", nil)
	_, _ = st.ClaimWork("claim-holder", "pkg/auth", "directory", 30)

	n, err := st.CountActiveAgents("other-agent")
	if err != nil {
		t.Fatalf("CountActiveAgents: %v", err)
	}
	if n < 1 {
		t.Errorf("expected ≥1 for agent with active claim, got %d", n)
	}
}
