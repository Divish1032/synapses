package store

import (
	"os"
	"testing"
	"time"
)

// openTestStore is a helper that creates a temp SQLite store and registers cleanup.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	f, err := os.CreateTemp("", "test-agents-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })
	st, err := Open(f.Name())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// ── Item 2: multi-scope visibility from work_claims ───────────────────────────

// TestGetActiveAgents_MultiScope verifies that when an agent claims several
// scopes the full list appears in GetActiveAgents — not just the last one.
func TestGetActiveAgents_MultiScope(t *testing.T) {
	st := openTestStore(t)

	// Register agent-a with no activity.
	if err := st.UpsertAgent("agent-a", nil); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Claim two separate scopes for agent-a.
	scopes := []string{"internal/auth", "internal/store"}
	for _, s := range scopes {
		if _, err := st.ClaimWork("agent-a", s, "directory", 30); err != nil {
			t.Fatalf("claim %q: %v", s, err)
		}
	}

	peers, err := st.GetActiveAgents("other-agent")
	if err != nil {
		t.Fatalf("GetActiveAgents: %v", err)
	}
	if len(peers) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(peers))
	}
	p := peers[0]
	if p.ID != "agent-a" {
		t.Errorf("expected agent-a, got %q", p.ID)
	}
	// Both scopes must be present — NOT just the last claimed one.
	if len(p.Scopes) != 2 {
		t.Errorf("expected 2 scopes, got %d: %v", len(p.Scopes), p.Scopes)
	}
	scopeSet := make(map[string]bool)
	for _, s := range p.Scopes {
		scopeSet[s] = true
	}
	for _, want := range scopes {
		if !scopeSet[want] {
			t.Errorf("missing scope %q in peer scopes %v", want, p.Scopes)
		}
	}
}

// TestGetActiveAgents_ExcludesCaller verifies the caller is not returned as a peer.
func TestGetActiveAgents_ExcludesCaller(t *testing.T) {
	st := openTestStore(t)
	_ = st.UpsertAgent("agent-a", nil)
	_ = st.UpsertAgent("agent-b", nil)

	peers, err := st.GetActiveAgents("agent-a")
	if err != nil {
		t.Fatalf("GetActiveAgents: %v", err)
	}
	for _, p := range peers {
		if p.ID == "agent-a" {
			t.Errorf("caller agent-a must not appear in its own peer list")
		}
	}
}

// TestGetActiveAgents_ClaimOverridesPresence verifies that an agent with a
// stale last_seen but a live work claim is still returned as active.
// This covers LLMs in long thinking loops between tool calls.
func TestGetActiveAgents_ClaimOverridesPresence(t *testing.T) {
	st := openTestStore(t)

	// Insert agent with last_seen 20 minutes ago (would normally be invisible).
	staleTime := time.Now().UTC().Add(-20 * time.Minute).Format(time.RFC3339)
	_, err := st.db.Exec(
		`INSERT INTO agents (id, last_seen, metadata, current_task_title, current_focus, project_id)
		 VALUES ('slow-thinker', ?, '{}', 'deep refactor', '', '')`,
		staleTime,
	)
	if err != nil {
		t.Fatalf("insert stale agent: %v", err)
	}

	// Give it an active claim (expires in 10 minutes).
	if _, err := st.ClaimWork("slow-thinker", "internal/store", "directory", 10); err != nil {
		t.Fatalf("ClaimWork: %v", err)
	}

	peers, err := st.GetActiveAgents("other-agent")
	if err != nil {
		t.Fatalf("GetActiveAgents: %v", err)
	}

	var found *AgentSummary
	for i := range peers {
		if peers[i].ID == "slow-thinker" {
			found = &peers[i]
			break
		}
	}
	if found == nil {
		t.Fatal("expected slow-thinker to appear via claim-based override, but it was absent")
	}
	if found.Presence != "active" {
		t.Errorf("expected presence=active via claim override, got %q", found.Presence)
	}
	if len(found.Scopes) == 0 {
		t.Errorf("expected scopes to be populated from claim")
	}
}

// TestGetActiveAgents_Presence checks the three presence tiers.
func TestGetActiveAgents_Presence(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		lastSeen time.Time
		want     string
	}{
		{now.Add(-1 * time.Minute), "active"},
		{now.Add(-9 * time.Minute), "idle"},
		{now.Add(-20 * time.Minute), "inactive"},
	}
	for _, tc := range cases {
		got := classifyPresence(tc.lastSeen.Format(time.RFC3339), now)
		if got != tc.want {
			t.Errorf("classifyPresence(%v) = %q, want %q", tc.lastSeen, got, tc.want)
		}
	}
}

// ── Item 3: message TTL pruning ───────────────────────────────────────────────

// TestSendMessage_PrunesReadMessages verifies that read messages older than
// 24 h are removed on the next SendMessage call.
func TestSendMessage_PrunesReadMessages(t *testing.T) {
	st := openTestStore(t)

	// Insert a message manually with a timestamp in the past (> 24 h ago)
	// and mark it as read, simulating an aged-out message.
	oldTime := time.Now().Add(-25 * time.Hour).Unix()
	readTime := time.Now().Add(-24 * time.Hour).Unix()
	_, err := st.db.Exec(
		`INSERT INTO agent_messages (id, from_agent, to_agent, topic, payload, project_id, created_at, read_at)
		 VALUES ('old-msg-1', 'agent-a', 'agent-b', 'test', '{}', '', ?, ?)`,
		oldTime, readTime,
	)
	if err != nil {
		t.Fatalf("insert old message: %v", err)
	}

	// Verify it exists before pruning.
	var count int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM agent_messages WHERE id='old-msg-1'`).Scan(&count)
	if count != 1 {
		t.Fatalf("setup: expected old message to exist")
	}

	// Sending a new message triggers pruning.
	if _, err := st.SendMessage("agent-a", "agent-b", "ping", "{}", ""); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// Old read message must be gone.
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM agent_messages WHERE id='old-msg-1'`).Scan(&count)
	if count != 0 {
		t.Errorf("expected old read message to be pruned, but it still exists")
	}
}

// TestSendMessage_KeepsUnreadMessages verifies that unread messages within the
// 7-day window are NOT pruned.
func TestSendMessage_KeepsUnreadMessages(t *testing.T) {
	st := openTestStore(t)

	// Insert an unread message from 2 days ago (well within 7-day window).
	recentTime := time.Now().Add(-48 * time.Hour).Unix()
	_, err := st.db.Exec(
		`INSERT INTO agent_messages (id, from_agent, to_agent, topic, payload, project_id, created_at)
		 VALUES ('recent-unread', 'agent-a', 'agent-b', 'test', '{}', '', ?)`,
		recentTime,
	)
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}

	// Trigger pruning via SendMessage.
	if _, err := st.SendMessage("agent-a", "agent-b", "ping", "{}", ""); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	var count int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM agent_messages WHERE id='recent-unread'`).Scan(&count)
	if count != 1 {
		t.Errorf("expected unread message within 7-day window to survive pruning")
	}
}

// TestSendMessage_PrunesVeryOldUnread verifies that unread messages older than
// 7 days are also pruned.
func TestSendMessage_PrunesVeryOldUnread(t *testing.T) {
	st := openTestStore(t)

	veryOld := time.Now().Add(-8 * 24 * time.Hour).Unix()
	_, err := st.db.Exec(
		`INSERT INTO agent_messages (id, from_agent, to_agent, topic, payload, project_id, created_at)
		 VALUES ('stale-unread', 'agent-a', 'agent-b', 'test', '{}', '', ?)`,
		veryOld,
	)
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}

	if _, err := st.SendMessage("agent-a", "agent-b", "ping", "{}", ""); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	var count int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM agent_messages WHERE id='stale-unread'`).Scan(&count)
	if count != 0 {
		t.Errorf("expected 8-day-old unread message to be pruned")
	}
}

// ── Item 4: cross-project (remote) agent visibility ──────────────────────────

// TestUpsertRemoteAgent_AppearsInGetActiveAgents verifies that a remote agent
// upserted by the health monitor appears in GetActiveAgents with the correct
// project label and scopes parsed from metadata.
func TestUpsertRemoteAgent_AppearsInGetActiveAgents(t *testing.T) {
	st := openTestStore(t)

	remoteScopes := []string{"internal/auth", "cmd/server"}
	err := st.UpsertRemoteAgent(
		"backend-repo::cursor-agent",
		"backend-repo",
		&AgentActivity{TaskTitle: "fix login bug", Focus: "AuthHandler"},
		remoteScopes,
	)
	if err != nil {
		t.Fatalf("UpsertRemoteAgent: %v", err)
	}

	peers, err := st.GetActiveAgents("local-agent")
	if err != nil {
		t.Fatalf("GetActiveAgents: %v", err)
	}
	if len(peers) != 1 {
		t.Fatalf("expected 1 remote peer, got %d", len(peers))
	}
	p := peers[0]

	if p.ID != "backend-repo::cursor-agent" {
		t.Errorf("wrong ID: %q", p.ID)
	}
	if p.Project != "backend-repo" {
		t.Errorf("wrong Project: %q, want %q", p.Project, "backend-repo")
	}
	if p.Task != "fix login bug" {
		t.Errorf("wrong Task: %q", p.Task)
	}
	if p.Focus != "AuthHandler" {
		t.Errorf("wrong Focus: %q", p.Focus)
	}
	if len(p.Scopes) != 2 {
		t.Errorf("expected 2 scopes, got %d: %v", len(p.Scopes), p.Scopes)
	}
	scopeSet := make(map[string]bool)
	for _, s := range p.Scopes {
		scopeSet[s] = true
	}
	for _, want := range remoteScopes {
		if !scopeSet[want] {
			t.Errorf("missing remote scope %q", want)
		}
	}
}

// TestUpsertRemoteAgent_AgesOut verifies that remote agents become inactive
// after 15 minutes and stop appearing in GetActiveAgents.
func TestUpsertRemoteAgent_AgesOut(t *testing.T) {
	st := openTestStore(t)

	// Insert the remote agent with a stale last_seen (20 min ago).
	staleTime := time.Now().UTC().Add(-20 * time.Minute).Format(time.RFC3339)
	_, err := st.db.Exec(
		`INSERT INTO agents (id, last_seen, metadata, current_task_title, current_focus, project_id)
		 VALUES ('remote-repo::old-agent', ?, '{}', '', '', 'remote-repo')`,
		staleTime,
	)
	if err != nil {
		t.Fatalf("insert stale remote agent: %v", err)
	}

	peers, err := st.GetActiveAgents("local-agent")
	if err != nil {
		t.Fatalf("GetActiveAgents: %v", err)
	}
	for _, p := range peers {
		if p.ID == "remote-repo::old-agent" {
			t.Errorf("stale remote agent (20 min ago) must not appear as active peer")
		}
	}
}

// TestGetAgents_IncludesProjectID verifies GetAgents surfaces project_id for
// remote agents so callers can distinguish local vs federated peers.
func TestGetAgents_IncludesProjectID(t *testing.T) {
	st := openTestStore(t)

	_ = st.UpsertAgent("local-agent", nil)
	_ = st.UpsertRemoteAgent("peer-repo::remote-agent", "peer-repo", nil, nil)

	agents, err := st.GetAgents()
	if err != nil {
		t.Fatalf("GetAgents: %v", err)
	}

	found := make(map[string]string) // id → project_id
	for _, a := range agents {
		found[a.ID] = a.ProjectID
	}

	if pid := found["local-agent"]; pid != "" {
		t.Errorf("local agent should have empty ProjectID, got %q", pid)
	}
	if pid := found["peer-repo::remote-agent"]; pid != "peer-repo" {
		t.Errorf("remote agent should have ProjectID=%q, got %q", "peer-repo", pid)
	}
}
