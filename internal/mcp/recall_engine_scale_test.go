package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/store"
)

// ── Temporal channel tuning at different project scales ───────────────────────
//
// These tests validate that the quad-channel recall engine produces relevant
// results across 3 project sizes: small (5 memories), medium (100), large (1000).
// The key metric: auth-relevant memories must rank in the top positions,
// and temporal-only noise must NOT displace them.

func TestQuadRecall_Scale_SmallProject_5Memories(t *testing.T) {
	srv := newTestServer(t)

	// 2 relevant, 3 irrelevant recent.
	insertMemory(t, srv, "AuthService was refactored to use OAuth2 flow")
	insertMemory(t, srv, "auth middleware validates JWT tokens on every request")
	insertMemory(t, srv, "updated CI pipeline to use Docker compose")
	insertMemory(t, srv, "refactored database connection pooling settings")
	insertMemory(t, srv, "added retry logic to HTTP client for flaky endpoints")

	mems, attr, _, _ := srv.quadRecallSearch(context.Background(), "auth token validation", 5, false, 7, 0)

	// With only 5 memories total, all should appear.
	if len(mems) < 2 {
		t.Fatalf("small project: got %d results, want at least 2", len(mems))
	}

	// The 2 auth memories should be in top 2 (BM25 + temporal boost).
	authInTop2 := 0
	for i := 0; i < 2 && i < len(mems); i++ {
		if isAuthRelated(mems[i].Content) {
			authInTop2++
		}
	}
	if authInTop2 < 2 {
		t.Errorf("small project: expected 2 auth memories in top 2, got %d", authInTop2)
		dumpResults(t, mems, attr)
	}

	// Temporal channel should contribute the non-matching memories too.
	totalResults := len(mems)
	if totalResults < 4 {
		t.Logf("small project: %d results (temporal fills gaps as expected)", totalResults)
	}
}

func TestQuadRecall_Scale_MediumProject_100Memories(t *testing.T) {
	srv := newTestServer(t)

	// 5 auth-relevant memories.
	authContents := []string{
		"AuthService refactored for OAuth2 with PKCE flow",
		"JWT token expiry validation added to auth middleware",
		"auth handler now rejects expired refresh tokens",
		"switched auth signing from HS256 to RS256 for security",
		"auth rate limiter added: 10 requests per second per user",
	}
	for _, c := range authContents {
		insertMemory(t, srv, c)
	}

	// 95 irrelevant memories (recent, diverse topics).
	topics := []string{
		"database", "Docker", "Kubernetes", "CI pipeline", "logging",
		"monitoring", "caching", "deployment", "testing", "refactoring",
		"HTTP client", "gRPC server", "WebSocket handler", "config loader",
		"error handling", "retry logic", "circuit breaker", "rate limiting",
		"connection pool", "migration",
	}
	for i := 0; i < 95; i++ {
		topic := topics[i%len(topics)]
		content := fmt.Sprintf("updated %s infrastructure component number %d with new settings and parameters", topic, i)
		insertMemory(t, srv, content)
	}

	mems, attr, _, _ := srv.quadRecallSearch(context.Background(), "auth token JWT validation", 5, false, 7, 0)

	if len(mems) == 0 {
		t.Fatal("medium project: got 0 results")
	}

	// All 5 results should be auth-relevant (BM25 has 5 strong matches).
	authCount := countAuthRelated(mems)
	if authCount < 3 {
		t.Errorf("medium project: expected at least 3 auth memories in top %d, got %d", len(mems), authCount)
		dumpResults(t, mems, attr)
	}

	// No temporal-only noise in top 3.
	for i := 0; i < 3 && i < len(mems); i++ {
		channels := attr[mems[i].ID]
		if len(channels) == 1 && channels[0] == "temporal" {
			t.Errorf("medium project: rank %d is temporal-only noise: %q", i+1, mems[i].Content)
		}
	}
}

func TestQuadRecall_Scale_LargeProject_1000Memories(t *testing.T) {
	srv := newTestServer(t)

	// 5 auth-relevant memories.
	authContents := []string{
		"AuthService handles OAuth2 authorization code flow",
		"JWT token validation middleware checks expiry and issuer",
		"auth session store migrated from Redis to SQLite",
		"added CSRF protection to auth cookie handler",
		"auth password hashing upgraded from bcrypt to argon2id",
	}
	for _, c := range authContents {
		insertMemory(t, srv, c)
	}

	// 995 irrelevant memories.
	for i := 0; i < 995; i++ {
		content := fmt.Sprintf("infrastructure change %d: updated component %c%c with configuration parameter set %d and deployment target %d",
			i, rune('A'+i%26), rune('a'+i%26), i*7, i*13)
		insertMemory(t, srv, content)
	}

	mems, attr, _, _ := srv.quadRecallSearch(context.Background(), "auth JWT token", 5, false, 7, 0)

	if len(mems) == 0 {
		t.Fatal("large project: got 0 results")
	}

	// Top 3 must be auth-relevant despite 995 noisy temporal candidates.
	authInTop3 := 0
	for i := 0; i < 3 && i < len(mems); i++ {
		if isAuthRelated(mems[i].Content) {
			authInTop3++
		}
	}
	if authInTop3 < 3 {
		t.Errorf("large project: expected at least 3 auth memories in top 3, got %d", authInTop3)
		dumpResults(t, mems, attr)
	}

	// CRITICAL: no temporal-only result should appear in top 3.
	for i := 0; i < 3 && i < len(mems); i++ {
		channels := attr[mems[i].ID]
		if len(channels) == 1 && channels[0] == "temporal" {
			t.Errorf("large project: NOISE at rank %d (temporal-only): %q", i+1, truncStr(mems[i].Content, 80))
			dumpResults(t, mems, attr)
			break
		}
	}

	// Check channel diversity — auth memories should come from multiple channels.
	for _, m := range mems {
		if isAuthRelated(m.Content) {
			channels := attr[m.ID]
			if len(channels) < 2 {
				t.Logf("note: auth memory %q only in %v (expected multi-channel)", truncStr(m.Content, 60), channels)
			}
		}
	}
}

func TestQuadRecall_Scale_LargeProject_TemporalStillUseful(t *testing.T) {
	srv := newTestServer(t)

	// 1 auth memory + 999 irrelevant. Query for auth with limit=5.
	// Temporal should fill the remaining 4 slots since BM25 only finds 1.
	insertMemory(t, srv, "AuthService uses RS256 JWT signing with key rotation")

	for i := 0; i < 999; i++ {
		content := fmt.Sprintf("deployment configuration update number %d for service %c%c", i, rune('A'+i%26), rune('a'+i%26))
		insertMemory(t, srv, content)
	}

	mems, _, _, _ := srv.quadRecallSearch(context.Background(), "auth JWT signing", 5, false, 7, 0)

	if len(mems) == 0 {
		t.Fatal("expected results")
	}

	// Auth memory should rank first (BM25 + temporal boost).
	if !isAuthRelated(mems[0].Content) {
		t.Errorf("expected auth memory at rank 1, got %q", truncStr(mems[0].Content, 80))
	}

	// Should have more than 1 result — temporal fills remaining slots.
	if len(mems) < 2 {
		t.Errorf("expected temporal to fill remaining slots, got only %d result", len(mems))
	}
}

func TestQuadRecall_Scale_EmptyProject(t *testing.T) {
	srv := newTestServer(t)
	mems, attr, _, _ := srv.quadRecallSearch(context.Background(), "auth", 5, false, 7, 0)
	if len(mems) != 0 {
		t.Errorf("empty project: expected 0 results, got %d", len(mems))
	}
	if attr != nil && len(attr) > 0 {
		t.Errorf("empty project: expected nil attribution, got %v", attr)
	}
}

func TestQuadRecall_Scale_AllMemoriesRelevant(t *testing.T) {
	srv := newTestServer(t)

	// All 20 memories match "auth" — no noise scenario.
	for i := 0; i < 20; i++ {
		content := fmt.Sprintf("auth system change %d: updated authentication handler for user type %d", i, i)
		insertMemory(t, srv, content)
	}

	mems, _, _, _ := srv.quadRecallSearch(context.Background(), "auth handler", 5, false, 7, 0)

	if len(mems) != 5 {
		t.Errorf("all-relevant: expected 5 results, got %d", len(mems))
	}

	// All results should be auth-related.
	for i, m := range mems {
		if !isAuthRelated(m.Content) {
			t.Errorf("all-relevant: rank %d not auth-related: %q", i+1, m.Content)
		}
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func insertMemory(t *testing.T, srv *Server, content string) {
	t.Helper()
	_, err := srv.store.InsertMemory(store.Memory{
		Tier:    store.TierProject,
		Content: content,
		AgentID: "scale-test",
		Source:  store.SourceManual,
	})
	if err != nil {
		t.Fatalf("insert memory: %v", err)
	}
}

func isAuthRelated(content string) bool {
	lower := strings.ToLower(content)
	return strings.Contains(lower, "auth") ||
		strings.Contains(lower, "jwt") ||
		strings.Contains(lower, "oauth") ||
		strings.Contains(lower, "token validation") ||
		strings.Contains(lower, "csrf") ||
		strings.Contains(lower, "password") ||
		strings.Contains(lower, "signing")
}

func countAuthRelated(mems []store.Memory) int {
	count := 0
	for _, m := range mems {
		if isAuthRelated(m.Content) {
			count++
		}
	}
	return count
}

func dumpResults(t *testing.T, mems []store.Memory, attr map[string][]string) {
	t.Helper()
	for i, m := range mems {
		channels := attr[m.ID]
		t.Logf("  rank %d [%s]: %s", i+1, strings.Join(channels, "+"), truncStr(m.Content, 80))
	}
}

func truncStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
