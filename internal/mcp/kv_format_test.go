package mcp

import (
	"strings"
	"testing"
)

func TestFormatKV_Basic(t *testing.T) {
	fields := []KVField{
		{Key: "Status", Value: "0 pending tasks"},
		{Key: "Branch", Value: "main"},
	}
	out := FormatKV("SESSION", "proj/foo", fields, 0)

	if !strings.HasPrefix(out, "# SESSION (proj/foo)\n") {
		t.Errorf("unexpected header: %q", out)
	}
	if !strings.Contains(out, "Status: 0 pending tasks") {
		t.Errorf("missing Status field: %q", out)
	}
	if !strings.Contains(out, "Branch: main") {
		t.Errorf("missing Branch field: %q", out)
	}
}

func TestFormatKV_NoSubtitle(t *testing.T) {
	out := FormatKV("VALIDATE", "", []KVField{{Key: "Finding", Value: "missing auth"}}, 0)
	if !strings.HasPrefix(out, "# VALIDATE\n") {
		t.Errorf("unexpected header: %q", out)
	}
}

func TestFormatKV_ImportantFieldsFirst(t *testing.T) {
	fields := []KVField{
		{Key: "Regular", Value: "regular value"},
		{Key: "Critical", Value: "critical value", Important: true},
	}
	out := FormatKV("TEST", "", fields, 0)
	// Important field must appear before regular field
	critIdx := strings.Index(out, "Critical:")
	regIdx := strings.Index(out, "Regular:")
	if critIdx == -1 || regIdx == -1 {
		t.Fatalf("expected both fields in output: %q", out)
	}
	if critIdx > regIdx {
		t.Errorf("important field should appear before regular field, got critIdx=%d regIdx=%d", critIdx, regIdx)
	}
}

func TestFormatKV_EmptyFieldsSkipped(t *testing.T) {
	fields := []KVField{
		{Key: "Present", Value: "yes"},
		{Key: "", Value: "orphan"},
		{Key: "Empty", Value: ""},
	}
	out := FormatKV("TEST", "", fields, 0)
	if strings.Contains(out, "orphan") {
		t.Errorf("empty-key field should be skipped: %q", out)
	}
	if strings.Contains(out, "Empty:") {
		t.Errorf("empty-value field should be skipped: %q", out)
	}
	if !strings.Contains(out, "Present: yes") {
		t.Errorf("present field missing: %q", out)
	}
}

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		s    string
		want int
	}{
		{"", 0},
		{"hello", 2},    // 5/4 = 1.25 → ceil → 2
		{"hello world", 3}, // 11/4 = 2.75 → ceil → 3
	}
	for _, tc := range cases {
		got := EstimateTokens(tc.s)
		if got != tc.want {
			t.Errorf("EstimateTokens(%q) = %d, want %d", tc.s, got, tc.want)
		}
	}
}

func TestTrimToTokenBudget_FitsWithinBudget(t *testing.T) {
	s := "short text"
	out := TrimToTokenBudget(s, 1000)
	if out != s {
		t.Errorf("expected unchanged, got %q", out)
	}
}

func TestTrimToTokenBudget_ZeroBudget(t *testing.T) {
	s := "any text"
	out := TrimToTokenBudget(s, 0)
	if out != s {
		t.Errorf("budget=0 should return unchanged, got %q", out)
	}
}

func TestTrimToTokenBudget_Truncates(t *testing.T) {
	// Build a string longer than 10 tokens (40 chars)
	lines := []string{
		"Line one content here",
		"Line two content here",
		"Line three content here",
		"Line four content here",
	}
	s := strings.Join(lines, "\n")
	// Tiny budget = 5 tokens → must truncate
	out := TrimToTokenBudget(s, 5)
	if !strings.Contains(out, "truncated") {
		t.Errorf("expected truncation indicator, got %q", out)
	}
	// Must preserve complete lines (no mid-line cut)
	// The output before the trailer must end at a newline boundary
	trailerIdx := strings.Index(out, "[truncated")
	before := out[:trailerIdx]
	if len(before) > 0 && before[len(before)-1] != '\n' {
		t.Errorf("truncation should happen at line boundary, got %q", before)
	}
}

func TestTrimToTokenBudget_LargeBudgetNoTruncation(t *testing.T) {
	s := strings.Repeat("a", 100)
	out := TrimToTokenBudget(s, 100) // 100 tokens = 400 chars > 100 chars
	if out != s {
		t.Errorf("expected no truncation for budget=100, got %q", out)
	}
}

func TestKvContentHash_Deterministic(t *testing.T) {
	h1 := kvContentHash("hello world")
	h2 := kvContentHash("hello world")
	if h1 != h2 {
		t.Errorf("hash should be deterministic: %q vs %q", h1, h2)
	}
}

func TestKvContentHash_DifferentInputs(t *testing.T) {
	h1 := kvContentHash("content A")
	h2 := kvContentHash("content B")
	if h1 == h2 {
		t.Errorf("different content should produce different hash")
	}
}

func TestKvContentHash_Length(t *testing.T) {
	h := kvContentHash("test")
	if len(h) != 16 {
		t.Errorf("expected 16-char hex hash, got len=%d: %q", len(h), h)
	}
}
