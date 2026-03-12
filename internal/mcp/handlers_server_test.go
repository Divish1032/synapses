package mcp

// Tests for server.go and resources.go unexported/semi-exported methods:
// callStartTimes.set/pop, packet cache CRUD, SetXxx setters, MCPServer,
// InvalidatePacketCache(ForFile), checkViolations, formatChangeAge,
// handleActiveContextResource, handleFileResource, handleViolationsResource.

import (
	"context"
	"testing"
	"time"

	mcp "github.com/mark3labs/mcp-go/mcp"
)

// ── callStartTimes ─────────────────────────────────────────────────────────

func TestCallStartTimes_SetAndPop(t *testing.T) {
	var c callStartTimes
	now := time.Now()
	c.set("get_context", now)
	got := c.pop("get_context")
	if !got.Equal(now) {
		t.Errorf("pop: got %v, want %v", got, now)
	}
	// Second pop returns zero time.
	zero := c.pop("get_context")
	if !zero.IsZero() {
		t.Error("second pop should return zero time")
	}
}

func TestCallStartTimes_SetLazyInit(t *testing.T) {
	var c callStartTimes
	// set with nil inner map should not panic (lazy init).
	c.set("tool", time.Now())
	got := c.pop("tool")
	if got.IsZero() {
		t.Error("expected non-zero time after set+pop")
	}
}

// ── packetCache ─────────────────────────────────────────────────────────────

func TestPacketCache_SetAndGet(t *testing.T) {
	s := newTestServer(t)
	s.setPacketCache("key-a", "payload-a")
	got := s.getPacketFromCache("key-a")
	if got == nil {
		t.Fatal("expected cached value")
	}
	if got.(string) != "payload-a" {
		t.Errorf("got %v, want %q", got, "payload-a")
	}
}

func TestPacketCache_Miss(t *testing.T) {
	s := newTestServer(t)
	got := s.getPacketFromCache("missing")
	if got != nil {
		t.Error("expected nil for missing key")
	}
}

func TestPacketCache_Eviction(t *testing.T) {
	s := newTestServer(t)
	// Fill cache to packetCacheMax+1 to trigger eviction.
	for i := 0; i < packetCacheMax+1; i++ {
		s.setPacketCache(string(rune('a'+i)), i)
	}
	// Cache should still be at most packetCacheMax entries.
	s.packetCacheMu.Lock()
	size := len(s.packetCache)
	s.packetCacheMu.Unlock()
	if size > packetCacheMax {
		t.Errorf("cache size %d exceeds max %d", size, packetCacheMax)
	}
}

func TestPacketCache_Expired(t *testing.T) {
	s := newTestServer(t)
	s.packetCacheMu.Lock()
	if s.packetCache == nil {
		s.packetCache = make(map[string]*packetCacheEntry, packetCacheMax)
	}
	// Insert an already-expired entry.
	s.packetCache["stale"] = &packetCacheEntry{
		pkt:       "old",
		expiresAt: time.Now().Add(-time.Minute),
	}
	s.packetCacheMu.Unlock()

	got := s.getPacketFromCache("stale")
	if got != nil {
		t.Error("expected nil for expired entry")
	}
}

// ── Server setters ───────────────────────────────────────────────────────────

func TestServer_Setters(t *testing.T) {
	s := newTestServer(t)

	// SetChangeSource — nil is fine.
	s.SetChangeSource(nil)

	// SetPeerManager.
	s.SetPeerManager("fake-peer-manager")
	if s.peerManager != "fake-peer-manager" {
		t.Error("SetPeerManager did not store value")
	}

	// SetBrainClient.
	s.SetBrainClient("fake-brain")
	if s.brainClient != "fake-brain" {
		t.Error("SetBrainClient did not store value")
	}

	// SetScoutClient.
	s.SetScoutClient("fake-scout")
	if s.scoutClient != "fake-scout" {
		t.Error("SetScoutClient did not store value")
	}

	// SetTechStack.
	s.SetTechStack([]string{"go", "sqlite"})
	if s.techStack == nil {
		t.Error("SetTechStack did not store value")
	}

	// SetEmbedClient — nil is acceptable.
	s.SetEmbedClient(nil)
}

func TestServer_MCPServer_NotNil(t *testing.T) {
	s := newTestServer(t)
	mcp := s.MCPServer()
	if mcp == nil {
		t.Fatal("expected non-nil MCPServer")
	}
}

// ── InvalidatePacketCache ────────────────────────────────────────────────────

func TestInvalidatePacketCache(t *testing.T) {
	s := newTestServer(t)
	s.setPacketCache("k", "v")
	s.InvalidatePacketCache()

	// Cache should be cleared.
	if got := s.getPacketFromCache("k"); got != nil {
		t.Error("expected nil after InvalidatePacketCache")
	}
}

func TestInvalidatePacketCacheForFile_WithFile(t *testing.T) {
	s := newTestServer(t)
	s.setPacketCache("k", "v")
	// Must not panic; brain is nil so warmBrainCache exits early.
	s.InvalidatePacketCacheForFile("internal/auth/auth.go")
	if got := s.getPacketFromCache("k"); got != nil {
		t.Error("expected nil after InvalidatePacketCacheForFile")
	}
}

func TestInvalidatePacketCacheForFile_EmptyFile(t *testing.T) {
	s := newTestServer(t)
	s.setPacketCache("k", "v")
	s.InvalidatePacketCacheForFile("")
	if got := s.getPacketFromCache("k"); got != nil {
		t.Error("expected nil after InvalidatePacketCacheForFile empty")
	}
}

// ── formatChangeAge ──────────────────────────────────────────────────────────

func TestFormatChangeAge(t *testing.T) {
	cases := []struct {
		offset time.Duration
		want   string
	}{
		{10 * time.Second, "just now"},
		{2 * time.Minute, "2m"},
		{3 * time.Hour, "3h"},
		{50 * time.Hour, "2d"},
	}
	for _, tc := range cases {
		t := t
		tc := tc
		t.Run(tc.want, func(t *testing.T) {
			ts := time.Now().Add(-tc.offset)
			got := formatChangeAge(ts)
			if got != tc.want {
				t.Errorf("formatChangeAge(-%v) = %q, want %q", tc.offset, got, tc.want)
			}
		})
	}
}

// ── checkViolations ──────────────────────────────────────────────────────────

func TestCheckViolations_NoConfig(t *testing.T) {
	s := newTestServer(t)
	s.config = nil
	vs := s.checkViolations()
	if vs != nil {
		t.Error("expected nil violations with nil config")
	}
}

func TestCheckViolations_EmptyGraph(t *testing.T) {
	s := newTestServer(t)
	vs := s.checkViolations()
	// Empty graph → no violations.
	if len(vs) != 0 {
		t.Errorf("expected 0 violations, got %d", len(vs))
	}
}

// ── handleActiveContextResource ───────────────────────────────────────────────

func TestHandleActiveContextResource_Basic(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleActiveContextResource(context.Background(), mcp.ReadResourceRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("expected at least one resource content")
	}
	tc, ok := res[0].(mcp.TextResourceContents)
	if !ok {
		t.Fatalf("expected TextResourceContents, got %T", res[0])
	}
	if tc.Text == "" {
		t.Error("expected non-empty text content")
	}
}

func TestHandleActiveContextResource_WithViolations(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleActiveContextResource(context.Background(), mcp.ReadResourceRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("expected resource content")
	}
}

// ── handleFileResource ────────────────────────────────────────────────────────

func TestHandleFileResource_EmptyURI(t *testing.T) {
	s := newTestServer(t)
	req := mcp.ReadResourceRequest{}
	req.Params.URI = "synapses://file/"
	_, err := s.handleFileResource(context.Background(), req)
	if err == nil {
		t.Error("expected error for empty file path")
	}
}

func TestHandleFileResource_NoNodes(t *testing.T) {
	s := newTestServer(t)
	req := mcp.ReadResourceRequest{}
	req.Params.URI = "synapses://file/nonexistent/file.go"
	res, err := s.handleFileResource(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("expected fallback content for unknown file")
	}
}

func TestHandleFileResource_WithNodes(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	req := mcp.ReadResourceRequest{}
	req.Params.URI = "synapses://file/pkg/auth/auth.go"
	res, err := s.handleFileResource(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("expected content for known file")
	}
}

// ── handleViolationsResource ───────────────────────────────────────────────────

func TestHandleViolationsResource_NoViolations(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleViolationsResource(context.Background(), mcp.ReadResourceRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("expected resource content")
	}
	tc := res[0].(mcp.TextResourceContents)
	if tc.Text == "" {
		t.Error("expected non-empty text")
	}
}
