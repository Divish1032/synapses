package store

import (
	"testing"
)

func TestAppendLedger(t *testing.T) {
	s := openTestStore(t)

	err := s.AppendLedger(LedgerEntry{
		SessionID: "sess-1",
		ProjectID: "proj-1",
		ToolName:  "get_context",
		EntityIDs: []string{"AuthService.Login"},
		FilePaths: []string{"api/auth.go"},
	})
	if err != nil {
		t.Fatalf("AppendLedger: %v", err)
	}

	// Verify it was written
	entities, files, err := s.SessionLedgerEntities("sess-1")
	if err != nil {
		t.Fatalf("SessionLedgerEntities: %v", err)
	}
	if len(entities) != 1 || entities[0] != "AuthService.Login" {
		t.Fatalf("expected [AuthService.Login], got %v", entities)
	}
	if len(files) != 1 || files[0] != "api/auth.go" {
		t.Fatalf("expected [api/auth.go], got %v", files)
	}
}

func TestActiveSessionWork_ExcludesSelf(t *testing.T) {
	s := openTestStore(t)

	// Insert a direct session row for the LEFT JOIN
	_, _ = s.knowledgeDB.Exec(
		`INSERT INTO sessions (id, agent_id, project_id, mcp_session_id, intent, started_at, last_seen_at, state)
		 VALUES ('sess-1', 'agent-1', 'proj-1', 'mcp-1', 'fix auth', strftime('%s','now'), strftime('%s','now'), 'active')`)

	_ = s.AppendLedger(LedgerEntry{
		SessionID: "sess-1", ProjectID: "proj-1", ToolName: "get_context",
		EntityIDs: []string{"AuthService"}, FilePaths: []string{"auth.go"},
	})

	// Query from same session — should get nothing
	results, err := s.ActiveSessionWork("proj-1", "sess-1", 15)
	if err != nil {
		t.Fatalf("ActiveSessionWork: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results (self excluded), got %d", len(results))
	}
}

func TestActiveSessionWork_FindsOtherSessions(t *testing.T) {
	s := openTestStore(t)

	// Create two session rows
	_, _ = s.knowledgeDB.Exec(
		`INSERT INTO sessions (id, agent_id, project_id, mcp_session_id, intent, started_at, last_seen_at, state)
		 VALUES ('sess-1', 'agent-1', 'proj-1', 'mcp-1', 'fix auth', strftime('%s','now'), strftime('%s','now'), 'active')`)
	_, _ = s.knowledgeDB.Exec(
		`INSERT INTO sessions (id, agent_id, project_id, mcp_session_id, intent, started_at, last_seen_at, state)
		 VALUES ('sess-2', 'agent-2', 'proj-1', 'mcp-2', 'add login', strftime('%s','now'), strftime('%s','now'), 'active')`)

	_ = s.AppendLedger(LedgerEntry{
		SessionID: "sess-1", ProjectID: "proj-1", ToolName: "get_context",
		EntityIDs: []string{"AuthService"}, FilePaths: []string{"auth.go"},
	})
	_ = s.AppendLedger(LedgerEntry{
		SessionID: "sess-2", ProjectID: "proj-1", ToolName: "get_file_context",
		EntityIDs: nil, FilePaths: []string{"login.go"},
	})

	// Query from session 2 — should see session 1's work
	results, err := s.ActiveSessionWork("proj-1", "sess-2", 15)
	if err != nil {
		t.Fatalf("ActiveSessionWork: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].SessionID != "sess-1" {
		t.Fatalf("expected session sess-1, got %s", results[0].SessionID)
	}
	if len(results[0].EntityIDs) != 1 || results[0].EntityIDs[0] != "AuthService" {
		t.Fatalf("expected [AuthService], got %v", results[0].EntityIDs)
	}
}

func TestPruneLedger(t *testing.T) {
	s := openTestStore(t)

	_ = s.AppendLedger(LedgerEntry{
		SessionID: "sess-1", ProjectID: "proj-1", ToolName: "search",
		EntityIDs: []string{"Foo"}, FilePaths: nil,
	})

	// Backdate the row so prune can find it
	_, _ = s.knowledgeDB.Exec(`UPDATE work_ledger SET created_at = datetime('now', '-2 hours')`)

	// Prune entries older than 1 hour
	n, err := s.PruneLedger(1 * 60 * 60 * 1e9) // 1 hour in nanoseconds (time.Duration)
	if err != nil {
		t.Fatalf("PruneLedger: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 pruned, got %d", n)
	}
}

func TestSessionLedgerEntities_Deduplicates(t *testing.T) {
	s := openTestStore(t)

	_ = s.AppendLedger(LedgerEntry{
		SessionID: "sess-1", ProjectID: "proj-1", ToolName: "get_context",
		EntityIDs: []string{"AuthService"}, FilePaths: []string{"auth.go"},
	})
	_ = s.AppendLedger(LedgerEntry{
		SessionID: "sess-1", ProjectID: "proj-1", ToolName: "get_impact",
		EntityIDs: []string{"AuthService", "TokenStore"}, FilePaths: []string{"auth.go", "token.go"},
	})

	entities, files, err := s.SessionLedgerEntities("sess-1")
	if err != nil {
		t.Fatalf("SessionLedgerEntities: %v", err)
	}
	if len(entities) != 2 {
		t.Fatalf("expected 2 unique entities, got %d: %v", len(entities), entities)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 unique files, got %d: %v", len(files), files)
	}
}

func TestDeduplicateJSONArrayConcat(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int
	}{
		{"empty", "", 0},
		{"single", `["a","b"]`, 2},
		{"concat_dedup", `["a","b"]|||["b","c"]`, 3},
		{"empty_arrays", `[]|||[]`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deduplicateJSONArrayConcat(tt.raw, "|||")
			if len(got) != tt.want {
				t.Errorf("got %d items %v, want %d", len(got), got, tt.want)
			}
		})
	}
}
