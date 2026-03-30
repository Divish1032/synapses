package mcp

import (
	"context"
	"strings"
	"testing"

	mcp "github.com/mark3labs/mcp-go/mcp"
)

// ── truncateAtWord ─────────────────────────────────────────────────────────────

func TestTruncateAtWord_ShortString(t *testing.T) {
	// Strings at or under the limit are returned unchanged, no ellipsis.
	s := "hello world"
	got := truncateAtWord(s, 60)
	if got != s {
		t.Errorf("short string: want %q, got %q", s, got)
	}
}

func TestTruncateAtWord_ExactLimit(t *testing.T) {
	s := strings.Repeat("x", 60)
	got := truncateAtWord(s, 60)
	if got != s {
		t.Errorf("exact-limit string: want unchanged, got %q", got)
	}
}

func TestTruncateAtWord_TruncatesAtWordBoundary(t *testing.T) {
	// "Function or method invocation" (32 chars) truncated at 20.
	// Should break at the last space before position 20.
	s := "Function or method invocation details here"
	got := truncateAtWord(s, 20)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated string should end with '…', got %q", got)
	}
	// Must not have cut a word mid-stream — the last char before '…' must be
	// an alphanumeric (end of a word), not a space.
	runes := []rune(got)
	beforeEllipsis := runes[len(runes)-2]
	if beforeEllipsis == ' ' {
		t.Errorf("truncated at space, want truncated at end of word: got %q", got)
	}
	// Whole result (including ellipsis) must be <= maxChars+1 runes.
	if len(runes) > 21 {
		t.Errorf("truncated result too long: %d runes, want <= 21", len(runes))
	}
}

func TestTruncateAtWord_NoSpaceInPrefix(t *testing.T) {
	// Single word longer than limit — hard cut at maxChars-1 + ellipsis.
	s := "abcdefghijklmnopqrstuvwxyz0123456789" // 36 chars, no spaces
	got := truncateAtWord(s, 10)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("no-space: should end with '…', got %q", got)
	}
	runes := []rune(got)
	if len(runes) > 11 { // maxChars + ellipsis
		t.Errorf("no-space: result too long: %d runes, want <= 11", len(runes))
	}
}

func TestTruncateAtWord_UnicodeMultibyte(t *testing.T) {
	// Verify it operates on runes, not bytes. "café" is 4 runes but 5 bytes.
	s := "café shop on the corner with a good view"
	got := truncateAtWord(s, 10)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("unicode: should truncate, got %q", got)
	}
	// Result must be valid UTF-8 (no split in the middle of a multi-byte char).
	if !isValidUTF8(got) {
		t.Errorf("unicode: result is not valid UTF-8: %q", got)
	}
}

func TestTruncateAtWord_EmptyString(t *testing.T) {
	got := truncateAtWord("", 10)
	if got != "" {
		t.Errorf("empty string: want \"\", got %q", got)
	}
}

func TestTruncateAtWord_MaxCharsZero(t *testing.T) {
	// maxChars=0 or 1 should not panic — hard cut at 0 chars + ellipsis.
	got := truncateAtWord("hello world", 1)
	// With maxChars=1, cut=0 after the no-space fallback, returns runes[:0]+"…" = "…"
	if !strings.HasSuffix(got, "…") {
		t.Errorf("maxChars=1: want ellipsis, got %q", got)
	}
}

// isValidUTF8 reports whether s is valid UTF-8 (no replacement chars from bad cuts).
func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '\uFFFD' {
			return false
		}
	}
	return true
}

// ── handleGetEdgeTypes ─────────────────────────────────────────────────────────

func TestHandleGetEdgeTypes_JSONDefault(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetEdgeTypes(context.Background(), callTool(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, res, nil)

	edgeTypes, ok := m["edge_types"].([]any)
	if !ok || len(edgeTypes) == 0 {
		t.Fatalf("expected non-empty edge_types, got %v", m["edge_types"])
	}

	// Spot-check CALLS is present with correct weight.
	foundCalls := false
	for _, raw := range edgeTypes {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if entry["name"] == "CALLS" {
			foundCalls = true
			w, _ := entry["semantic_weight"].(float64)
			if w != 1.0 {
				t.Errorf("CALLS: want semantic_weight=1.0, got %.2f", w)
			}
			if entry["domain"] != "code" {
				t.Errorf("CALLS: want domain=code, got %v", entry["domain"])
			}
			if entry["direction"] != "directed" {
				t.Errorf("CALLS: want direction=directed, got %v", entry["direction"])
			}
		}
	}
	if !foundCalls {
		t.Error("CALLS edge type not found in get_edge_types response")
	}

	// total must match the actual slice length.
	total, _ := m["total"].(float64)
	if int(total) != len(edgeTypes) {
		t.Errorf("total=%d does not match len(edge_types)=%d", int(total), len(edgeTypes))
	}
}

func TestHandleGetEdgeTypes_CompactFormat(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetEdgeTypes(context.Background(), callTool(map[string]any{"format": "compact"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tc, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", res.Content[0])
	}
	text := tc.Text

	// Compact output must contain the header and at least "CALLS".
	if !strings.Contains(text, "CALLS") {
		t.Errorf("compact format missing CALLS: %q", text)
	}
	if !strings.Contains(text, "Edge Type Catalog") {
		t.Errorf("compact format missing header: %q", text)
	}
	// Must contain the synthetic marker.
	if !strings.Contains(text, "*") {
		t.Errorf("compact format missing synthetic marker (*): %q", text)
	}
}

func TestHandleGetEdgeTypes_AllDescriptorsHaveDescription(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetEdgeTypes(context.Background(), callTool(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, res, nil)
	edgeTypes, _ := m["edge_types"].([]any)
	for _, raw := range edgeTypes {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := entry["name"]
		desc, _ := entry["description"].(string)
		if desc == "" {
			t.Errorf("edge type %v has empty description", name)
		}
	}
}

