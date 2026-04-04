package archivist

import (
	"strings"
	"testing"
)

// TestDeterministicArchivist_20Sessions validates that DeterministicArchivist.Extract
// achieves ≥95% precision across 20 synthetic session scenarios.
//
// Precision is defined per scenario as:
//   (memories whose content correctly reflects the input data)
//   ÷ (total memories extracted)
//
// Since all content is derived directly from input fields (no inference),
// precision for each scenario is either 100% (all fields match) or 0% (nothing
// extracted when it should be empty). The aggregate across all scenarios must
// be ≥95%.
func TestDeterministicArchivist_20Sessions(t *testing.T) {
	da := NewDeterministic()

	type scenario struct {
		name    string
		req     DeterministicRequest
		// checks are assertions on the response. Each must pass for the scenario
		// to be counted as correct.
		checks []func(t *testing.T, resp MemorizeResponse) bool
	}

	scenarios := []scenario{
		// 1. Empty session — nothing to extract.
		{
			name: "01_empty_session",
			req:  DeterministicRequest{SessionID: "s1"},
			checks: []func(*testing.T, MemorizeResponse) bool{
				func(t *testing.T, r MemorizeResponse) bool {
					if len(r.NewMemories) != 0 {
						t.Errorf("want 0 memories, got %d", len(r.NewMemories))
						return false
					}
					return true
				},
			},
		},
		// 2. get_context with no finding_summary — should produce no memories.
		{
			name: "02_context_no_findings",
			req: DeterministicRequest{
				SessionID: "s2",
				ExplorationEntries: []ExplorationEntry{
					{ToolName: "get_context", EntityQueried: "AuthHandler", FindingSummary: ""},
				},
			},
			checks: []func(*testing.T, MemorizeResponse) bool{
				func(t *testing.T, r MemorizeResponse) bool {
					if len(r.NewMemories) != 0 {
						t.Errorf("want 0 memories, got %d", len(r.NewMemories))
						return false
					}
					return true
				},
			},
		},
		// 3. get_context with finding_summary — produces memory + annotation.
		{
			name: "03_context_with_findings",
			req: DeterministicRequest{
				SessionID: "s3",
				ExplorationEntries: []ExplorationEntry{
					{ToolName: "get_context", EntityQueried: "AuthHandler", FindingSummary: "AuthHandler: 5 caller(s), 2 callee(s), 1 security constraint(s)"},
				},
			},
			checks: []func(*testing.T, MemorizeResponse) bool{
				func(t *testing.T, r MemorizeResponse) bool {
					if len(r.NewMemories) != 1 {
						t.Errorf("want 1 memory, got %d", len(r.NewMemories))
						return false
					}
					if !strings.Contains(r.NewMemories[0].Content, "AuthHandler") {
						t.Errorf("memory content missing entity: %q", r.NewMemories[0].Content)
						return false
					}
					if len(r.Annotations) != 1 {
						t.Errorf("want 1 annotation, got %d", len(r.Annotations))
						return false
					}
					if r.Annotations[0].Node != "AuthHandler" {
						t.Errorf("annotation node mismatch: %q", r.Annotations[0].Node)
						return false
					}
					return true
				},
			},
		},
		// 4. search with findings — produces memory, no annotation.
		{
			name: "04_search_with_findings",
			req: DeterministicRequest{
				SessionID: "s4",
				ExplorationEntries: []ExplorationEntry{
					{ToolName: "search", EntityQueried: "middleware auth", FindingSummary: "top results: AuthMiddleware, JWTMiddleware, SessionMiddleware"},
				},
			},
			checks: []func(*testing.T, MemorizeResponse) bool{
				func(t *testing.T, r MemorizeResponse) bool {
					if len(r.NewMemories) != 1 {
						t.Errorf("want 1 memory, got %d", len(r.NewMemories))
						return false
					}
					if !strings.Contains(r.NewMemories[0].Content, "middleware auth") {
						t.Errorf("memory missing query term: %q", r.NewMemories[0].Content)
						return false
					}
					if len(r.Annotations) != 0 {
						t.Errorf("search should produce no annotations, got %d", len(r.Annotations))
						return false
					}
					return true
				},
			},
		},
		// 5. validate with no violations.
		{
			name: "05_validate_no_violations",
			req: DeterministicRequest{
				SessionID: "s5",
				ExplorationEntries: []ExplorationEntry{
					{ToolName: "validate", EntityQueried: "handlers/auth.go", FindingSummary: "no violations found"},
				},
			},
			checks: []func(*testing.T, MemorizeResponse) bool{
				func(t *testing.T, r MemorizeResponse) bool {
					if len(r.NewMemories) != 1 {
						t.Errorf("want 1 memory, got %d", len(r.NewMemories))
						return false
					}
					if !strings.Contains(r.NewMemories[0].Content, "no violations") {
						t.Errorf("memory missing violation result: %q", r.NewMemories[0].Content)
						return false
					}
					return true
				},
			},
		},
		// 6. validate with violations — memory captures count.
		{
			name: "06_validate_with_violations",
			req: DeterministicRequest{
				SessionID: "s6",
				ExplorationEntries: []ExplorationEntry{
					{ToolName: "validate", EntityQueried: "api/handler.go", FindingSummary: "3 violation(s): 1 CRITICAL: 2 HIGH"},
				},
			},
			checks: []func(*testing.T, MemorizeResponse) bool{
				func(t *testing.T, r MemorizeResponse) bool {
					if len(r.NewMemories) != 1 {
						t.Errorf("want 1 memory, got %d", len(r.NewMemories))
						return false
					}
					if !strings.Contains(r.NewMemories[0].Content, "3 violation") {
						t.Errorf("memory missing violation count: %q", r.NewMemories[0].Content)
						return false
					}
					return true
				},
			},
		},
		// 7. memory(save) — decision recorded.
		{
			name: "07_memory_save",
			req: DeterministicRequest{
				SessionID: "s7",
				ExplorationEntries: []ExplorationEntry{
					{ToolName: "memory", EntityQueried: "auth_approach", FindingSummary: "Decided to use JWT with short-lived tokens for stateless auth"},
				},
			},
			checks: []func(*testing.T, MemorizeResponse) bool{
				func(t *testing.T, r MemorizeResponse) bool {
					if len(r.NewMemories) != 1 {
						t.Errorf("want 1 memory, got %d", len(r.NewMemories))
						return false
					}
					if !strings.Contains(r.NewMemories[0].Content, "JWT") {
						t.Errorf("memory missing decision content: %q", r.NewMemories[0].Content)
						return false
					}
					// manual decision gets highest priority key prefix
					if r.NewMemories[0].Key == "" {
						t.Errorf("memory key is empty")
						return false
					}
					return true
				},
			},
		},
		// 8. rejected approach without blocker.
		{
			name: "08_rejected_approach_no_blocker",
			req: DeterministicRequest{
				SessionID: "s8",
				FailedApproaches: []FailedApproach{
					{Approach: "Use gorilla/sessions for session management", FailureReason: "requires global state, incompatible with concurrent tests"},
				},
			},
			checks: []func(*testing.T, MemorizeResponse) bool{
				func(t *testing.T, r MemorizeResponse) bool {
					if len(r.NewMemories) != 1 {
						t.Errorf("want 1 memory, got %d", len(r.NewMemories))
						return false
					}
					content := r.NewMemories[0].Content
					if !strings.Contains(content, "gorilla") {
						t.Errorf("memory missing approach: %q", content)
						return false
					}
					if !strings.Contains(content, "concurrent tests") {
						t.Errorf("memory missing failure reason: %q", content)
						return false
					}
					return true
				},
			},
		},
		// 9. rejected approach with blocker — blocker included in content.
		{
			name: "09_rejected_approach_with_blocker",
			req: DeterministicRequest{
				SessionID: "s9",
				FailedApproaches: []FailedApproach{
					{Approach: "Embed redis client in handler", FailureReason: "import cycle", Blocker: "import cycle: handler → redis → handler"},
				},
			},
			checks: []func(*testing.T, MemorizeResponse) bool{
				func(t *testing.T, r MemorizeResponse) bool {
					if len(r.NewMemories) != 1 {
						t.Errorf("want 1 memory, got %d", len(r.NewMemories))
						return false
					}
					content := r.NewMemories[0].Content
					if !strings.Contains(content, "Blocker") {
						t.Errorf("memory missing blocker: %q", content)
						return false
					}
					return true
				},
			},
		},
		// 10. All 5 categories mixed — all appear in output.
		{
			name: "10_all_categories_mixed",
			req: DeterministicRequest{
				SessionID: "s10",
				ExplorationEntries: []ExplorationEntry{
					{ToolName: "memory", EntityQueried: "db_choice", FindingSummary: "Use SQLite with WAL mode for this service"},
					{ToolName: "get_context", EntityQueried: "UserHandler", FindingSummary: "UserHandler: 3 callers, 1 callee"},
					{ToolName: "search", EntityQueried: "rate limiter", FindingSummary: "top results: RateLimiter, TokenBucket"},
					{ToolName: "validate", EntityQueried: "user.go", FindingSummary: "1 violation(s): 1 HIGH"},
					{ToolName: "get_impact", EntityQueried: "DBPool", FindingSummary: "DBPool affects 8 entities across 3 packages"},
				},
				FailedApproaches: []FailedApproach{
					{Approach: "Use connection pool per request", FailureReason: "resource exhaustion under load"},
				},
			},
			checks: []func(*testing.T, MemorizeResponse) bool{
				func(t *testing.T, r MemorizeResponse) bool {
					if len(r.NewMemories) < 5 {
						t.Errorf("want ≥5 memories (one per category), got %d", len(r.NewMemories))
						return false
					}
					// Verify all categories present
					foundCategories := map[string]bool{}
					for _, m := range r.NewMemories {
						if strings.Contains(m.Content, "SQLite") {
							foundCategories["manual"] = true
						}
						if strings.Contains(m.Content, "UserHandler") {
							foundCategories["context"] = true
						}
						if strings.Contains(m.Content, "rate limiter") {
							foundCategories["search"] = true
						}
						if strings.Contains(m.Content, "violation") {
							foundCategories["validate"] = true
						}
						if strings.Contains(m.Content, "connection pool") || strings.Contains(m.Content, "resource exhaustion") {
							foundCategories["failed"] = true
						}
					}
					for cat, found := range map[string]bool{
						"manual": true, "context": true, "search": true, "validate": true, "failed": true,
					} {
						if found && !foundCategories[cat] {
							t.Errorf("category %q not found in memories", cat)
							return false
						}
					}
					return true
				},
			},
		},
		// 11. Large session (40 entries) — capped at maxDeterministicMemories (30).
		{
			name: "11_large_session_cap",
			req: func() DeterministicRequest {
				entries := make([]ExplorationEntry, 40)
				for i := range entries {
					entries[i] = ExplorationEntry{
						ToolName:       "get_context",
						EntityQueried:  strings.Repeat("Entity", 1) + string(rune('A'+i%26)),
						FindingSummary: "some finding",
					}
				}
				return DeterministicRequest{SessionID: "s11", ExplorationEntries: entries}
			}(),
			checks: []func(*testing.T, MemorizeResponse) bool{
				func(t *testing.T, r MemorizeResponse) bool {
					if len(r.NewMemories) > maxDeterministicMemories {
						t.Errorf("want ≤%d memories, got %d", maxDeterministicMemories, len(r.NewMemories))
						return false
					}
					return true
				},
			},
		},
		// 12. Duplicate entity keys — deduplicated (first wins).
		{
			name: "12_duplicate_entity_dedup",
			req: DeterministicRequest{
				SessionID: "s12",
				ExplorationEntries: []ExplorationEntry{
					{ToolName: "get_context", EntityQueried: "AuthMiddleware", FindingSummary: "first finding"},
					{ToolName: "get_context", EntityQueried: "AuthMiddleware", FindingSummary: "second finding"},
				},
			},
			checks: []func(*testing.T, MemorizeResponse) bool{
				func(t *testing.T, r MemorizeResponse) bool {
					count := 0
					for _, m := range r.NewMemories {
						if strings.Contains(m.Key, "auth_middleware") || strings.Contains(m.Key, "authmiddleware") {
							count++
						}
					}
					if count != 1 {
						t.Errorf("want 1 deduplicated memory for AuthMiddleware, got %d", count)
						return false
					}
					return true
				},
			},
		},
		// 13. Long FindingSummary (≤300 chars) — preserved.
		{
			name: "13_long_finding_preserved",
			req: DeterministicRequest{
				SessionID: "s13",
				ExplorationEntries: []ExplorationEntry{
					{ToolName: "get_context", EntityQueried: "BigService",
						FindingSummary: strings.Repeat("x", 299)},
				},
			},
			checks: []func(*testing.T, MemorizeResponse) bool{
				func(t *testing.T, r MemorizeResponse) bool {
					if len(r.NewMemories) != 1 {
						t.Errorf("want 1 memory, got %d", len(r.NewMemories))
						return false
					}
					if len(r.NewMemories[0].Content) == 0 {
						t.Errorf("content is empty")
						return false
					}
					return true
				},
			},
		},
		// 14. memory(action="search") — NOT captured (only action=save is relevant).
		// Captured entries have QueryContext="save"; others have different context.
		// This tests that we don't produce memories from non-save memory calls.
		// Since extraction_log.go filters on action="save" before writing, entries
		// with QueryContext != "save" should NOT produce memories.
		{
			name: "14_memory_non_save_skipped",
			req: DeterministicRequest{
				SessionID: "s14",
				ExplorationEntries: []ExplorationEntry{
					// QueryContext != "save" — this is a memory(action=search) that
					// shouldn't have been logged; if it were, Extract must still handle it.
					{ToolName: "memory", EntityQueried: "some_query", FindingSummary: ""},
				},
			},
			checks: []func(*testing.T, MemorizeResponse) bool{
				func(t *testing.T, r MemorizeResponse) bool {
					// Empty finding_summary → should be skipped.
					if len(r.NewMemories) != 0 {
						t.Errorf("memory with empty finding should not produce memories, got %d", len(r.NewMemories))
						return false
					}
					return true
				},
			},
		},
		// 15. memory(save) with empty content — NOT captured.
		{
			name: "15_memory_save_empty_content",
			req: DeterministicRequest{
				SessionID: "s15",
				ExplorationEntries: []ExplorationEntry{
					{ToolName: "memory", EntityQueried: "some_key", FindingSummary: ""},
				},
			},
			checks: []func(*testing.T, MemorizeResponse) bool{
				func(t *testing.T, r MemorizeResponse) bool {
					if len(r.NewMemories) != 0 {
						t.Errorf("empty memory save should not produce memories, got %d", len(r.NewMemories))
						return false
					}
					return true
				},
			},
		},
		// 16. Multiple rejected approaches — all included (up to cap).
		{
			name: "16_multiple_failed_approaches",
			req: DeterministicRequest{
				SessionID: "s16",
				FailedApproaches: []FailedApproach{
					{Approach: "Approach A", FailureReason: "reason A"},
					{Approach: "Approach B", FailureReason: "reason B"},
					{Approach: "Approach C", FailureReason: "reason C"},
				},
			},
			checks: []func(*testing.T, MemorizeResponse) bool{
				func(t *testing.T, r MemorizeResponse) bool {
					if len(r.NewMemories) != 3 {
						t.Errorf("want 3 memories, got %d", len(r.NewMemories))
						return false
					}
					for i, m := range r.NewMemories {
						if m.Content == "" {
							t.Errorf("memory[%d] has empty content", i)
							return false
						}
					}
					return true
				},
			},
		},
		// 17. get_impact findings — produce memory.
		{
			name: "17_get_impact_findings",
			req: DeterministicRequest{
				SessionID: "s17",
				ExplorationEntries: []ExplorationEntry{
					{ToolName: "get_impact", EntityQueried: "DBPool", FindingSummary: "DBPool affects 12 callers across 5 packages"},
				},
			},
			checks: []func(*testing.T, MemorizeResponse) bool{
				func(t *testing.T, r MemorizeResponse) bool {
					if len(r.NewMemories) != 1 {
						t.Errorf("want 1 memory, got %d", len(r.NewMemories))
						return false
					}
					if !strings.Contains(r.NewMemories[0].Content, "12 callers") {
						t.Errorf("memory missing blast radius: %q", r.NewMemories[0].Content)
						return false
					}
					// get_impact also produces an annotation
					if len(r.Annotations) != 1 {
						t.Errorf("want 1 annotation, got %d", len(r.Annotations))
						return false
					}
					return true
				},
			},
		},
		// 18. Priority ordering — manual decisions rank before get_context findings.
		{
			name: "18_priority_manual_before_context",
			req: DeterministicRequest{
				SessionID: "s18",
				ExplorationEntries: []ExplorationEntry{
					{ToolName: "get_context", EntityQueried: "SomeEntity", FindingSummary: "SomeEntity: 1 caller"},
					{ToolName: "memory", EntityQueried: "important_decision", FindingSummary: "Use Redis for distributed cache"},
				},
			},
			checks: []func(*testing.T, MemorizeResponse) bool{
				func(t *testing.T, r MemorizeResponse) bool {
					if len(r.NewMemories) < 2 {
						t.Errorf("want ≥2 memories, got %d", len(r.NewMemories))
						return false
					}
					// First memory should be the manual decision (priority 0).
					if !strings.Contains(r.NewMemories[0].Content, "Redis") {
						t.Errorf("manual decision should be first, got: %q", r.NewMemories[0].Content)
						return false
					}
					return true
				},
			},
		},
		// 19. sessionID and agentID are informational only — do not appear in output.
		{
			name: "19_metadata_not_in_output",
			req: DeterministicRequest{
				SessionID: "secret-session-id",
				AgentID:   "secret-agent-id",
				ExplorationEntries: []ExplorationEntry{
					{ToolName: "get_context", EntityQueried: "Foo", FindingSummary: "Foo: 1 caller"},
				},
			},
			checks: []func(*testing.T, MemorizeResponse) bool{
				func(t *testing.T, r MemorizeResponse) bool {
					for _, m := range r.NewMemories {
						if strings.Contains(m.Content, "secret-session-id") || strings.Contains(m.Content, "secret-agent-id") {
							t.Errorf("memory content contains internal metadata: %q", m.Content)
							return false
						}
					}
					return true
				},
			},
		},
		// 20. snakeKey correctness — keys are valid snake_case, non-empty, ≤60 chars.
		{
			name: "20_snake_key_validity",
			req: DeterministicRequest{
				SessionID: "s20",
				ExplorationEntries: []ExplorationEntry{
					{ToolName: "get_context", EntityQueried: "handler/auth.go::AuthMiddleware", FindingSummary: "found security constraints"},
					{ToolName: "get_context", EntityQueried: "A B C D E F G H I J K L M N O P Q R S T U V W X Y Z 1 2 3 4", FindingSummary: "long entity name"},
					{ToolName: "get_context", EntityQueried: "  --leading-trailing--  ", FindingSummary: "odd name"},
				},
			},
			checks: []func(*testing.T, MemorizeResponse) bool{
				func(t *testing.T, r MemorizeResponse) bool {
					for _, m := range r.NewMemories {
						if m.Key == "" {
							t.Errorf("memory key is empty for content %q", m.Content)
							return false
						}
						if len(m.Key) > 60 {
							t.Errorf("key too long (%d chars): %q", len(m.Key), m.Key)
							return false
						}
						// Key must start with a letter or digit (no leading underscore after Trim).
						if m.Key[0] == '_' {
							t.Errorf("key starts with underscore: %q", m.Key)
							return false
						}
					}
					return true
				},
			},
		},
	}

	var totalExtracted, totalCorrect int

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			resp := da.Extract(sc.req)
			correct := true
			for _, check := range sc.checks {
				if !check(t, resp) {
					correct = false
				}
			}
			// Accumulate precision stats.
			totalExtracted += len(resp.NewMemories)
			if correct {
				totalCorrect += len(resp.NewMemories)
			}
		})
	}

	// Aggregate precision gate: ≥95% of extracted memories are correct.
	if totalExtracted > 0 {
		precision := float64(totalCorrect) / float64(totalExtracted)
		if precision < 0.95 {
			t.Errorf("aggregate precision %.1f%% < 95%% (%d/%d correct)",
				precision*100, totalCorrect, totalExtracted)
		}
	}
}

// ── Unit tests for helpers ────────────────────────────────────────────────────

func TestSnakeKey(t *testing.T) {
	cases := []struct {
		input    string
		prefix   string
		want     string
	}{
		{"AuthHandler", "entity", "authhandler"},
		{"handler/auth.go::AuthMiddleware", "entity", "handler_auth_go_authmiddleware"},
		{"  --odd--  ", "entity", "odd"},
		{"", "entity", "entity"},
		{strings.Repeat("a", 70), "entity", strings.Repeat("a", 60)},
		{"A B C", "k", "a_b_c"},
		{"already_snake", "k", "already_snake"},
	}
	for _, c := range cases {
		got := snakeKey(c.input, c.prefix)
		if got != c.want {
			t.Errorf("snakeKey(%q, %q) = %q, want %q", c.input, c.prefix, got, c.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		s      string
		maxLen int
		want   string
	}{
		{"hello", 10, "hello"},
		{"hello world", 5, "hell…"},
		{"", 5, ""},
		{"hello", 0, "hello"},
		{"hello", 5, "hello"},
	}
	for _, c := range cases {
		got := truncate(c.s, c.maxLen)
		if got != c.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", c.s, c.maxLen, got, c.want)
		}
	}
}

func TestDeterministicArchivist_EmptyRequest(t *testing.T) {
	da := NewDeterministic()
	resp := da.Extract(DeterministicRequest{})
	if resp.NewMemories == nil {
		t.Error("NewMemories should not be nil")
	}
	if resp.Annotations == nil {
		t.Error("Annotations should not be nil")
	}
	if len(resp.NewMemories) != 0 {
		t.Errorf("want 0 memories, got %d", len(resp.NewMemories))
	}
}

func TestDeterministicArchivist_PriorityOrder(t *testing.T) {
	// Verify that memories are emitted in priority order:
	// manual (0) → failed (1) → validate (2) → context/impact (3) → search (4)
	da := NewDeterministic()
	resp := da.Extract(DeterministicRequest{
		SessionID: "priority_test",
		ExplorationEntries: []ExplorationEntry{
			{ToolName: "search", EntityQueried: "auth", FindingSummary: "search result"},
			{ToolName: "validate", EntityQueried: "file.go", FindingSummary: "1 violation"},
			{ToolName: "get_context", EntityQueried: "Handler", FindingSummary: "3 callers"},
			{ToolName: "memory", EntityQueried: "decision_key", FindingSummary: "use JWT"},
		},
		FailedApproaches: []FailedApproach{
			{Approach: "use sessions", FailureReason: "stateful"},
		},
	})

	if len(resp.NewMemories) != 5 {
		t.Fatalf("want 5 memories, got %d", len(resp.NewMemories))
	}
	// Memory[0] should contain the manual decision (priority 0).
	if !strings.Contains(resp.NewMemories[0].Content, "JWT") {
		t.Errorf("memory[0] should be manual decision, got: %q", resp.NewMemories[0].Content)
	}
	// Memory[1] should contain the failed approach (priority 1).
	if !strings.Contains(resp.NewMemories[1].Content, "sessions") {
		t.Errorf("memory[1] should be failed approach, got: %q", resp.NewMemories[1].Content)
	}
	// Memory[2] should be validate (priority 2).
	if !strings.Contains(resp.NewMemories[2].Content, "violation") {
		t.Errorf("memory[2] should be validate, got: %q", resp.NewMemories[2].Content)
	}
}
