package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── PruneStaleData Correctness Tests ─────────────────────────────────────────
//
// Sprint 9 #5 — Council QA #5. These tests verify that PruneStaleData actually
// deletes the right rows and preserves the right rows, rather than just
// verifying it doesn't crash.

// pruneCountRows counts rows in a knowledgeDB table.
func pruneCountRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	err := s.knowledgeDB.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&n)
	if err != nil {
		t.Fatalf("pruneCountRows(%s): %v", table, err)
	}
	return n
}

// resetDebounce clears the prune debounce so PruneStaleData runs immediately.
func resetDebounce(s *Store) {
	s.lastPruneMu.Lock()
	s.lastPruneStaleAt = time.Time{}
	s.lastPruneMu.Unlock()
}

// seedOldToolCalls inserts tool_calls with a backdated created_at timestamp.
func seedOldToolCalls(t *testing.T, s *Store, n int, age time.Duration) {
	t.Helper()
	ts := time.Now().UTC().Add(-age).Format(time.RFC3339)
	for i := 0; i < n; i++ {
		_, err := s.knowledgeDB.Exec(
			`INSERT INTO tool_calls(tool_name, agent_id, session_id, entity, duration_ms, success, created_at)
			 VALUES(?, ?, '', '', 10, 1, ?)`,
			fmt.Sprintf("tool_%d", i), "test-agent", ts,
		)
		if err != nil {
			t.Fatalf("seedOldToolCalls: %v", err)
		}
	}
}

// seedOldEvents inserts events via direct SQL (bypasses AppendEvent's inline prune).
func seedOldEvents(t *testing.T, s *Store, n int, age time.Duration) {
	t.Helper()
	ts := time.Now().UTC().Add(-age).Format(time.RFC3339)
	for i := 0; i < n; i++ {
		_, err := s.knowledgeDB.Exec(
			`INSERT INTO events(type, agent_id, payload, created_at) VALUES(?, ?, '{}', ?)`,
			fmt.Sprintf("test_event_%d", i), "test-agent", ts,
		)
		if err != nil {
			t.Fatalf("seedOldEvents: %v", err)
		}
	}
}

// seedRecentEvents inserts events via direct SQL with current timestamp.
func seedRecentEvents(t *testing.T, s *Store, n int) {
	t.Helper()
	ts := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < n; i++ {
		_, err := s.knowledgeDB.Exec(
			`INSERT INTO events(type, agent_id, payload, created_at) VALUES(?, ?, '{}', ?)`,
			fmt.Sprintf("recent_event_%d", i), "agent-recent", ts,
		)
		if err != nil {
			t.Fatalf("seedRecentEvents: %v", err)
		}
	}
}

// seedOldEpisodes inserts episodes with a backdated created_at (Unix seconds).
func seedOldEpisodes(t *testing.T, s *Store, n int, age time.Duration) {
	t.Helper()
	ts := time.Now().Add(-age).Unix()
	for i := 0; i < n; i++ {
		_, err := s.knowledgeDB.Exec(
			`INSERT INTO episodes(id, agent_id, created_at, episode_type, outcome, decision, affected_files, affected_nodes, tags)
			 VALUES(?, 'test-agent', ?, 'decision', 'success', ?, '[]', '[]', '[]')`,
			fmt.Sprintf("ep-old-%d", i), ts, fmt.Sprintf("old decision %d", i),
		)
		if err != nil {
			t.Fatalf("seedOldEpisodes: %v", err)
		}
	}
}

// seedOldMessages inserts agent_messages with a backdated created_at (Unix seconds).
// Note: agent_messages.created_at is INTEGER (Unix seconds) in the schema.
func seedOldMessages(t *testing.T, s *Store, n int, age time.Duration) {
	t.Helper()
	ts := time.Now().Add(-age).Unix()
	for i := 0; i < n; i++ {
		_, err := s.knowledgeDB.Exec(
			`INSERT INTO agent_messages(id, from_agent, to_agent, topic, payload, project_id, created_at)
			 VALUES(?, 'sender', 'receiver', 'test', '{}', '', ?)`,
			fmt.Sprintf("msg-old-%d", i), ts,
		)
		if err != nil {
			t.Fatalf("seedOldMessages: %v", err)
		}
	}
}

// seedRecentMessages inserts agent_messages with current Unix timestamp.
func seedRecentMessages(t *testing.T, s *Store, n int) {
	t.Helper()
	ts := time.Now().Unix()
	for i := 0; i < n; i++ {
		_, err := s.knowledgeDB.Exec(
			`INSERT INTO agent_messages(id, from_agent, to_agent, topic, payload, project_id, created_at)
			 VALUES(?, 'sender', 'receiver', 'recent', '{}', '', ?)`,
			fmt.Sprintf("msg-recent-%d", i), ts,
		)
		if err != nil {
			t.Fatalf("seedRecentMessages: %v", err)
		}
	}
}

// seedOldContextDeliveries inserts context_deliveries with a backdated created_at (Unix seconds).
func seedOldContextDeliveries(t *testing.T, s *Store, n int, age time.Duration) {
	t.Helper()
	ts := time.Now().Add(-age).Unix()
	for i := 0; i < n; i++ {
		_, err := s.knowledgeDB.Exec(
			`INSERT INTO context_deliveries(session_id, agent_id, tool_name, entity, refetched, task_outcome, created_at)
			 VALUES('sess-1', 'test-agent', 'get_context', 'AuthService', 0, '', ?)`,
			ts,
		)
		if err != nil {
			t.Fatalf("seedOldContextDeliveries: %v", err)
		}
	}
}

// seedOldProposals inserts resolved proposals with a backdated updated_at.
func seedOldProposals(t *testing.T, s *Store, prefix, status string, age time.Duration) []string {
	t.Helper()
	ts := time.Now().UTC().Add(-age).Format(time.RFC3339)
	now := time.Now().UTC().Format(time.RFC3339)
	var ids []string
	id := fmt.Sprintf("prop-%s-%s", prefix, status)
	_, err := s.knowledgeDB.Exec(
		`INSERT INTO proposals(id, agent_id, title, description, affected_nodes, status, vote_threshold, created_at, updated_at)
		 VALUES(?, 'test-agent', ?, '', '[]', ?, 2, ?, ?)`,
		id, fmt.Sprintf("proposal %s", prefix), status, now, ts,
	)
	if err != nil {
		t.Fatalf("seedOldProposals: %v", err)
	}
	ids = append(ids, id)
	return ids
}

// seedProposalVotes inserts votes for a given proposal.
func seedProposalVotes(t *testing.T, s *Store, proposalID string, n int) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < n; i++ {
		_, err := s.knowledgeDB.Exec(
			`INSERT INTO proposal_votes(proposal_id, agent_id, vote, rationale, created_at)
			 VALUES(?, ?, 'approve', 'lgtm', ?)`,
			proposalID, fmt.Sprintf("voter-%s-%d", proposalID, i), now,
		)
		if err != nil {
			t.Fatalf("seedProposalVotes: %v", err)
		}
	}
}

// seedOldMemories inserts memories with a backdated timestamp and specified tier/expiry.
func seedOldMemories(t *testing.T, s *Store, prefix, tier string, age time.Duration, expiresAt string) {
	t.Helper()
	ts := time.Now().UTC().Add(-age).Format(time.RFC3339)
	id := fmt.Sprintf("mem-%s-%s", prefix, tier)
	_, err := s.knowledgeDB.Exec(
		`INSERT INTO memories(id, tier, content, entity_id, agent_id, task_id, tags, created_at, expires_at, last_accessed_at, source)
		 VALUES(?, ?, ?, '', 'test-agent', '', '[]', ?, ?, ?, 'manual')`,
		id, tier, fmt.Sprintf("memory %s", prefix), ts, expiresAt, ts,
	)
	if err != nil {
		t.Fatalf("seedOldMemories: %v", err)
	}
}

// TestPruneStaleData_ToolCalls_DeletesOldPreservesRecent verifies that old
// tool_calls are deleted and recent ones survive.
func TestPruneStaleData_ToolCalls_DeletesOldPreservesRecent(t *testing.T) {
	st := openTestStore(t)
	resetDebounce(st)

	// Seed 5 old rows (60 days old) and 3 recent rows (via public API).
	seedOldToolCalls(t, st, 5, 60*24*time.Hour)
	st.RecordToolCall("get_context", "agent-recent", "", "", 10, true)
	st.RecordToolCall("recall", "agent-recent", "", "", 20, true)
	st.RecordToolCall("search", "agent-recent", "", "", 5, true)

	before := pruneCountRows(t, st, "tool_calls")
	if before != 8 {
		t.Fatalf("expected 8 tool_calls before prune, got %d", before)
	}

	// Prune with 30-day retention — 60-day-old rows should be deleted.
	st.PruneStaleData(30)

	after := pruneCountRows(t, st, "tool_calls")
	if after != 3 {
		t.Errorf("expected 3 tool_calls after prune (30-day retention), got %d", after)
	}
}

// TestPruneStaleData_Events_DeletesOldPreservesRecent verifies event pruning.
// Note: AppendEvent prunes events >24h inline, so we seed via direct SQL.
func TestPruneStaleData_Events_DeletesOldPreservesRecent(t *testing.T) {
	st := openTestStore(t)
	resetDebounce(st)

	// Seed via direct SQL to avoid AppendEvent's inline prune.
	seedOldEvents(t, st, 4, 90*24*time.Hour)
	seedRecentEvents(t, st, 2)

	before := pruneCountRows(t, st, "events")
	if before != 6 {
		t.Fatalf("expected 6 events before prune, got %d", before)
	}

	st.PruneStaleData(30)

	after := pruneCountRows(t, st, "events")
	if after != 2 {
		t.Errorf("expected 2 events after prune, got %d", after)
	}
}

// TestPruneStaleData_Episodes_DeletesOldPreservesRecent verifies episode pruning.
// Episodes use Unix seconds for created_at (INTEGER), not RFC3339.
func TestPruneStaleData_Episodes_DeletesOldPreservesRecent(t *testing.T) {
	st := openTestStore(t)
	resetDebounce(st)

	seedOldEpisodes(t, st, 3, 45*24*time.Hour)
	// Insert a recent episode via public API.
	_, _ = st.RememberEpisode(Episode{
		AgentID:       "recent-agent",
		EpisodeType:   "decision",
		Outcome:       "success",
		Decision:      "recent decision",
		AffectedFiles: "[]",
		AffectedNodes: "[]",
		Tags:          "[]",
	})

	before := pruneCountRows(t, st, "episodes")
	if before != 4 {
		t.Fatalf("expected 4 episodes before prune, got %d", before)
	}

	st.PruneStaleData(30)

	after := pruneCountRows(t, st, "episodes")
	if after != 1 {
		t.Errorf("expected 1 episode after prune (30-day retention), got %d", after)
	}
}

// TestPruneStaleData_AgentMessages_DeletesOldPreservesRecent verifies that
// old agent_messages are deleted and recent ones survive. agent_messages stores
// created_at as Unix INTEGER seconds (see SendMessage), so the prune cutoff
// must also be Unix seconds.
func TestPruneStaleData_AgentMessages_DeletesOldPreservesRecent(t *testing.T) {
	st := openTestStore(t)
	resetDebounce(st)

	seedOldMessages(t, st, 4, 60*24*time.Hour)
	seedRecentMessages(t, st, 2)

	before := pruneCountRows(t, st, "agent_messages")
	if before != 6 {
		t.Fatalf("expected 6 agent_messages before prune, got %d", before)
	}

	st.PruneStaleData(30)

	after := pruneCountRows(t, st, "agent_messages")
	if after != 2 {
		t.Errorf("expected 2 agent_messages after prune (30-day retention), got %d", after)
	}
}

// TestPruneStaleData_ContextDeliveries_DeletesOldPreservesRecent verifies
// context_deliveries pruning (Unix seconds).
func TestPruneStaleData_ContextDeliveries_DeletesOldPreservesRecent(t *testing.T) {
	st := openTestStore(t)
	resetDebounce(st)

	seedOldContextDeliveries(t, st, 3, 60*24*time.Hour)
	// Insert a recent delivery.
	_, err := st.knowledgeDB.Exec(
		`INSERT INTO context_deliveries(session_id, agent_id, tool_name, entity, refetched, task_outcome, created_at)
		 VALUES('sess-recent', 'agent-1', 'get_context', 'Foo', 0, '', ?)`,
		time.Now().Unix(),
	)
	if err != nil {
		t.Fatalf("insert recent context_delivery: %v", err)
	}

	before := pruneCountRows(t, st, "context_deliveries")
	if before != 4 {
		t.Fatalf("expected 4 context_deliveries before prune, got %d", before)
	}

	st.PruneStaleData(30)

	after := pruneCountRows(t, st, "context_deliveries")
	if after != 1 {
		t.Errorf("expected 1 context_delivery after prune, got %d", after)
	}
}

// TestPruneStaleData_Memories_ExpiredAndSessionLog verifies that:
// - memories with a past expires_at are deleted
// - session_log memories older than retention are deleted
// - entity memories (non-session_log, non-expired) survive
func TestPruneStaleData_Memories_ExpiredAndSessionLog(t *testing.T) {
	st := openTestStore(t)
	resetDebounce(st)

	pastExpiry := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	futureExpiry := time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339)

	// Expired memory (expires_at in the past) — should be deleted.
	seedOldMemories(t, st, "exp1", "entity", 5*24*time.Hour, pastExpiry)
	seedOldMemories(t, st, "exp2", "entity", 5*24*time.Hour, pastExpiry)
	// Session_log memory older than retention — should be deleted.
	seedOldMemories(t, st, "slog1", "session_log", 60*24*time.Hour, "")
	seedOldMemories(t, st, "slog2", "session_log", 60*24*time.Hour, "")
	// Entity memory with future expiry — should survive.
	seedOldMemories(t, st, "keep1", "entity", 60*24*time.Hour, futureExpiry)
	// Recent session_log memory — should survive.
	seedOldMemories(t, st, "keep2", "session_log", 1*time.Hour, "")

	before := pruneCountRows(t, st, "memories")
	if before != 6 {
		t.Fatalf("expected 6 memories before prune, got %d", before)
	}

	st.PruneStaleData(30)

	after := pruneCountRows(t, st, "memories")
	// Survivors: 1 entity with future expiry + 1 recent session_log = 2
	if after != 2 {
		t.Errorf("expected 2 memories after prune, got %d", after)
	}
}

// TestPruneStaleData_Proposals_DeletesResolvedPreservesOpen verifies that
// resolved proposals (accepted/rejected/withdrawn) older than retention are
// deleted, while open proposals survive. Also verifies orphaned proposal_votes
// are cleaned up.
func TestPruneStaleData_Proposals_DeletesResolvedPreservesOpen(t *testing.T) {
	st := openTestStore(t)
	resetDebounce(st)

	// Old resolved proposals — should be deleted.
	ids1 := seedOldProposals(t, st, "a1", "accepted", 60*24*time.Hour)
	seedOldProposals(t, st, "a2", "accepted", 60*24*time.Hour)
	seedOldProposals(t, st, "r1", "rejected", 60*24*time.Hour)
	// Old open proposal — should survive (not resolved).
	ids4 := seedOldProposals(t, st, "o1", "open", 60*24*time.Hour)
	// Recent resolved proposal — should survive (within retention).
	seedOldProposals(t, st, "a3", "accepted", 1*time.Hour)

	// Add votes to a proposal that will be deleted and one that will survive.
	seedProposalVotes(t, st, ids1[0], 2)
	seedProposalVotes(t, st, ids4[0], 1)

	beforeProposals := pruneCountRows(t, st, "proposals")
	beforeVotes := pruneCountRows(t, st, "proposal_votes")
	if beforeProposals != 5 {
		t.Fatalf("expected 5 proposals before prune, got %d", beforeProposals)
	}
	if beforeVotes != 3 {
		t.Fatalf("expected 3 votes before prune, got %d", beforeVotes)
	}

	st.PruneStaleData(30)

	afterProposals := pruneCountRows(t, st, "proposals")
	// Survivors: 1 open + 1 recent accepted = 2
	if afterProposals != 2 {
		t.Errorf("expected 2 proposals after prune, got %d", afterProposals)
	}

	afterVotes := pruneCountRows(t, st, "proposal_votes")
	// prop-a1-accepted was deleted → its 2 votes are orphaned → deleted
	// prop-o1-open survives → its 1 vote survives
	if afterVotes != 1 {
		t.Errorf("expected 1 proposal_vote after prune (orphans cleaned), got %d", afterVotes)
	}
}

// TestPruneStaleData_ZeroRetention_DeletesAll verifies that retentionDays=0
// prunes all old timestamped data. We seed data 1 second in the past to avoid
// same-second cutoff race (episodes use Unix seconds, so created_at == cutoff
// means "not older than cutoff" in the DELETE WHERE created_at < ? predicate).
func TestPruneStaleData_ZeroRetention_DeletesAll(t *testing.T) {
	st := openTestStore(t)
	resetDebounce(st)

	// Seed data 2 seconds in the past to guarantee it's strictly before cutoff.
	past := time.Now().UTC().Add(-2 * time.Second)
	pastRFC := past.Format(time.RFC3339)
	pastUnix := past.Unix()

	_, _ = st.knowledgeDB.Exec(
		`INSERT INTO tool_calls(tool_name, agent_id, session_id, entity, duration_ms, success, created_at)
		 VALUES('get_context', 'agent', '', '', 5, 1, ?)`, pastRFC)
	_, _ = st.knowledgeDB.Exec(
		`INSERT INTO events(type, agent_id, payload, created_at) VALUES('test', 'agent', '{}', ?)`, pastRFC)
	_, _ = st.knowledgeDB.Exec(
		`INSERT INTO episodes(id, agent_id, created_at, episode_type, outcome, decision, affected_files, affected_nodes, tags)
		 VALUES('ep-zero', 'agent', ?, 'decision', 'success', 'd', '[]', '[]', '[]')`, pastUnix)
	_, _ = st.knowledgeDB.Exec(
		`INSERT INTO agent_messages(id, from_agent, to_agent, topic, payload, project_id, created_at)
		 VALUES('msg-zero', 'a', 'b', 't', '{}', '', ?)`, pastUnix)

	st.PruneStaleData(0)

	if n := pruneCountRows(t, st, "tool_calls"); n != 0 {
		t.Errorf("tool_calls: expected 0 after zero-retention prune, got %d", n)
	}
	if n := pruneCountRows(t, st, "events"); n != 0 {
		t.Errorf("events: expected 0 after zero-retention prune, got %d", n)
	}
	if n := pruneCountRows(t, st, "episodes"); n != 0 {
		t.Errorf("episodes: expected 0 after zero-retention prune, got %d", n)
	}
	if n := pruneCountRows(t, st, "agent_messages"); n != 0 {
		t.Errorf("agent_messages: expected 0 after zero-retention prune, got %d", n)
	}
}

// TestPruneStaleData_Annotations_OrphanedStaleDeleted verifies cross-DB
// annotation cleanup: stale annotations whose node_id no longer exists in
// graphDB are deleted, while non-stale annotations and annotations for
// existing nodes survive.
func TestPruneStaleData_Annotations_OrphanedStaleDeleted(t *testing.T) {
	st := openTestStore(t)
	resetDebounce(st)

	// Create a graph with one surviving node.
	g := graph.New("ann-prune-repo")
	existingNodeID := g.MakeNodeID("service.go", "MyService")
	g.AddNode(&graph.Node{
		ID: existingNodeID, Name: "MyService", Type: graph.NodeFunction,
		File: "service.go", Package: "svc",
	})
	if err := st.SaveGraph(g); err != nil {
		t.Fatalf("SaveGraph: %v", err)
	}

	absentNodeID := "ann-prune-repo::deleted/gone.go::GoneFunc"

	// Annotation 1: stale annotation for existing node → should survive (node exists).
	_, err := st.knowledgeDB.Exec(
		`INSERT INTO annotations(id, node_id, agent_id, note, created_at, source, stale)
		 VALUES('ann-1', ?, 'agent', 'still valid', ?, 'agent', 1)`,
		string(existingNodeID), time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		t.Fatalf("insert ann-1: %v", err)
	}

	// Annotation 2: stale annotation for absent node → should be deleted.
	_, err = st.knowledgeDB.Exec(
		`INSERT INTO annotations(id, node_id, agent_id, note, created_at, source, stale)
		 VALUES('ann-2', ?, 'agent', 'orphaned note', ?, 'agent', 1)`,
		absentNodeID, time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		t.Fatalf("insert ann-2: %v", err)
	}

	// Annotation 3: non-stale annotation for absent node → should survive (not stale).
	_, err = st.knowledgeDB.Exec(
		`INSERT INTO annotations(id, node_id, agent_id, note, created_at, source, stale)
		 VALUES('ann-3', ?, 'agent', 'fresh note but node gone', ?, 'agent', 0)`,
		absentNodeID, time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		t.Fatalf("insert ann-3: %v", err)
	}

	before := pruneCountRows(t, st, "annotations")
	if before != 3 {
		t.Fatalf("expected 3 annotations before prune, got %d", before)
	}

	st.PruneStaleData(30)

	after := pruneCountRows(t, st, "annotations")
	// Survivors: ann-1 (node exists) + ann-3 (not stale) = 2
	if after != 2 {
		t.Errorf("expected 2 annotations after prune, got %d", after)
	}

	// Verify the right ones survived.
	var exists bool
	err = st.knowledgeDB.QueryRow(`SELECT 1 FROM annotations WHERE id = 'ann-2'`).Scan(&exists)
	if err == nil {
		t.Error("ann-2 (stale, absent node) should have been pruned")
	}
	err = st.knowledgeDB.QueryRow(`SELECT 1 FROM annotations WHERE id = 'ann-1'`).Scan(&exists)
	if err != nil {
		t.Error("ann-1 (stale, existing node) should have survived")
	}
	err = st.knowledgeDB.QueryRow(`SELECT 1 FROM annotations WHERE id = 'ann-3'`).Scan(&exists)
	if err != nil {
		t.Error("ann-3 (not stale, absent node) should have survived")
	}
}

// TestPruneStaleData_Annotations_NoGraphDB verifies that annotation cleanup
// is safely skipped when graphDB is nil (knowledge-only mode).
func TestPruneStaleData_Annotations_NoGraphDB(t *testing.T) {
	st := openTestStore(t)
	resetDebounce(st)

	// Insert an annotation.
	_, err := st.knowledgeDB.Exec(
		`INSERT INTO annotations(id, node_id, agent_id, note, created_at, source, stale)
		 VALUES('ann-nograph', 'some::node', 'agent', 'note', ?, 'agent', 1)`,
		time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		t.Fatalf("insert annotation: %v", err)
	}

	// Simulate knowledge-only mode by temporarily setting graphDB to nil.
	savedGraphDB := st.graphDB
	st.graphDB = nil
	defer func() { st.graphDB = savedGraphDB }()

	st.PruneStaleData(30)

	// Annotation should survive — no graph to cross-reference.
	after := pruneCountRows(t, st, "annotations")
	if after != 1 {
		t.Errorf("expected annotation to survive when graphDB is nil, got %d", after)
	}
}

// TestPruneStaleData_Annotations_EmptyGraph verifies that when graphDB exists
// but has zero nodes, stale annotations are preserved (not nuked). This is the
// fail-safe for full-reindex scenarios where the graph is momentarily empty.
func TestPruneStaleData_Annotations_EmptyGraph(t *testing.T) {
	st := openTestStore(t)
	resetDebounce(st)

	// graphDB exists but has no nodes (empty graph).
	// Insert a stale annotation referencing a non-existent node.
	_, err := st.knowledgeDB.Exec(
		`INSERT INTO annotations(id, node_id, agent_id, note, created_at, source, stale)
		 VALUES('ann-emptygraph', 'some::repo::file.go::Func', 'agent', 'orphan note', ?, 'agent', 1)`,
		time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		t.Fatalf("insert annotation: %v", err)
	}

	// Verify graphDB is not nil but has 0 nodes.
	if st.graphDB == nil {
		t.Fatal("graphDB should not be nil")
	}
	var nodeCount int
	_ = st.graphDB.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodeCount)
	if nodeCount != 0 {
		t.Fatalf("expected 0 nodes in empty graph, got %d", nodeCount)
	}

	st.PruneStaleData(30)

	// Annotation must survive — empty graph means skip reconciliation.
	after := pruneCountRows(t, st, "annotations")
	if after != 1 {
		t.Errorf("expected stale annotation to survive when graph is empty (reindex safety), got %d", after)
	}
}

// TestPruneStaleData_AllTables_IntegrationRoundTrip seeds data across ALL
// tables that PruneStaleData targets, runs prune with specific retention,
// and verifies the exact surviving row counts.
func TestPruneStaleData_AllTables_IntegrationRoundTrip(t *testing.T) {
	st := openTestStore(t)
	resetDebounce(st)

	// --- Seed old data (90 days) ---
	seedOldToolCalls(t, st, 3, 90*24*time.Hour)
	seedOldEvents(t, st, 3, 90*24*time.Hour)
	seedOldEpisodes(t, st, 3, 90*24*time.Hour)
	seedOldMessages(t, st, 3, 90*24*time.Hour)
	seedOldContextDeliveries(t, st, 3, 90*24*time.Hour)
	seedOldProposals(t, st, "int-a1", "accepted", 90*24*time.Hour)
	seedOldProposals(t, st, "int-a2", "accepted", 90*24*time.Hour)

	// --- Seed recent data ---
	st.RecordToolCall("get_context", "agent-new", "", "", 10, true)
	seedRecentEvents(t, st, 1)
	_, _ = st.RememberEpisode(Episode{
		AgentID: "agent-new", EpisodeType: "decision", Outcome: "success",
		Decision: "new d", AffectedFiles: "[]", AffectedNodes: "[]", Tags: "[]",
	})
	seedRecentMessages(t, st, 1)

	// Recent context delivery.
	_, _ = st.knowledgeDB.Exec(
		`INSERT INTO context_deliveries(session_id, agent_id, tool_name, entity, refetched, task_outcome, created_at)
		 VALUES('sess-new', 'agent-new', 'get_context', 'X', 0, '', ?)`,
		time.Now().Unix(),
	)

	// --- Prune at 30 days ---
	st.PruneStaleData(30)

	// --- Verify ---
	checks := []struct {
		table    string
		expected int
	}{
		{"tool_calls", 1},
		{"events", 1},
		{"episodes", 1},
		{"agent_messages", 1},
		{"context_deliveries", 1},
		{"proposals", 0}, // both were old + resolved
	}
	for _, c := range checks {
		got := pruneCountRows(t, st, c.table)
		if got != c.expected {
			t.Errorf("%s: expected %d rows after prune, got %d", c.table, c.expected, got)
		}
	}
}
