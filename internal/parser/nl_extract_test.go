package parser

import (
	"strings"
	"testing"
)

// ── ExtractEntityCandidates ──────────────────────────────────────────────────

func TestExtractEntityCandidates_Backticks(t *testing.T) {
	text := "The `TokenBucket` algorithm controls throughput via `RateLimiter`."
	cs := ExtractEntityCandidates(text)
	names := candidateNames(cs)
	if !hasName(names, "TokenBucket") {
		t.Errorf("expected TokenBucket in %v", names)
	}
	if !hasName(names, "RateLimiter") {
		t.Errorf("expected RateLimiter in %v", names)
	}
	// Backtick entries get highest confidence.
	for _, c := range cs {
		if c.Name == "TokenBucket" && c.Confidence < 0.85 {
			t.Errorf("backtick candidate TokenBucket confidence too low: %v", c.Confidence)
		}
	}
}

func TestExtractEntityCandidates_CamelCase(t *testing.T) {
	text := "TokenBucket uses an internal CircularQueue for backpressure."
	cs := ExtractEntityCandidates(text)
	names := candidateNames(cs)
	if !hasName(names, "TokenBucket") {
		t.Errorf("expected TokenBucket in %v", names)
	}
	if !hasName(names, "CircularQueue") {
		t.Errorf("expected CircularQueue in %v", names)
	}
}

func TestExtractEntityCandidates_QuotedTerms(t *testing.T) {
	text := `This implements the "token bucket" pattern for "rate limiting".`
	cs := ExtractEntityCandidates(text)
	names := candidateNames(cs)
	if !hasName(names, "token bucket") {
		t.Errorf("expected 'token bucket' in %v", names)
	}
	if !hasName(names, "rate limiting") {
		t.Errorf("expected 'rate limiting' in %v", names)
	}
}

func TestExtractEntityCandidates_StopWordsFiltered(t *testing.T) {
	text := "The is a an of in on to for from with by as it its"
	cs := ExtractEntityCandidates(text)
	for _, c := range cs {
		if nlStopWords[strings.ToLower(c.Name)] {
			t.Errorf("stop word %q should not appear in candidates", c.Name)
		}
	}
}

func TestExtractEntityCandidates_EmptyInput(t *testing.T) {
	cs := ExtractEntityCandidates("")
	if len(cs) != 0 {
		t.Errorf("expected no candidates for empty input, got %d", len(cs))
	}
}

func TestExtractEntityCandidates_ShortTokensFiltered(t *testing.T) {
	text := "`ab` `x` `foo` are short"
	cs := ExtractEntityCandidates(text)
	for _, c := range cs {
		if len(c.Name) < 3 {
			t.Errorf("candidate %q is shorter than 3 chars — should be filtered", c.Name)
		}
	}
}

func TestExtractEntityCandidates_Deduplication(t *testing.T) {
	// TokenBucket appears in backticks (high conf) AND as CamelCase (lower conf).
	// Should be deduplicated, keeping the highest confidence entry.
	text := "`TokenBucket` is implemented as a TokenBucket with slots."
	cs := ExtractEntityCandidates(text)
	count := 0
	for _, c := range cs {
		if strings.EqualFold(c.Name, "TokenBucket") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("TokenBucket should appear exactly once (dedup), got %d", count)
	}
	// The backtick entry (0.9) should win over CamelCase (0.75).
	for _, c := range cs {
		if strings.EqualFold(c.Name, "TokenBucket") && c.Confidence < 0.85 {
			t.Errorf("deduplicated TokenBucket should have backtick confidence, got %.2f", c.Confidence)
		}
	}
}

func TestExtractEntityCandidates_ContextField(t *testing.T) {
	text := "The `RateLimiter` controls concurrent requests to avoid overload."
	cs := ExtractEntityCandidates(text)
	for _, c := range cs {
		if c.Name == "RateLimiter" {
			if c.Context == "" {
				t.Error("context should be non-empty for extracted candidate")
			}
			if !strings.Contains(c.Context, "RateLimiter") {
				t.Errorf("context %q should contain 'RateLimiter'", c.Context)
			}
		}
	}
}

func TestExtractEntityCandidates_MultiLine(t *testing.T) {
	text := "Line 1 mentions `AuthService`.\nLine 2 mentions `TokenBucket`."
	cs := ExtractEntityCandidates(text)
	names := candidateNames(cs)
	if !hasName(names, "AuthService") {
		t.Errorf("expected AuthService")
	}
	if !hasName(names, "TokenBucket") {
		t.Errorf("expected TokenBucket")
	}
	// Check source lines are set.
	for _, c := range cs {
		if c.SourceLine == 0 {
			t.Errorf("candidate %q has SourceLine=0", c.Name)
		}
	}
}

func TestExtractEntityCandidates_NumbersFiltered(t *testing.T) {
	text := "`123` and `456.789` should not become entities."
	cs := ExtractEntityCandidates(text)
	for _, c := range cs {
		allDigits := true
		for _, r := range c.Name {
			if r != '.' && r != '-' && !(r >= '0' && r <= '9') {
				allDigits = false
				break
			}
		}
		if allDigits {
			t.Errorf("pure-number candidate %q should be filtered", c.Name)
		}
	}
}

// ── ExtractRelationshipSignals ───────────────────────────────────────────────

func TestExtractRelationshipSignals_DependsOn(t *testing.T) {
	candidates := []EntityCandidate{
		{Name: "AuthService"}, {Name: "TokenStore"},
	}
	text := "authservice depends on tokenstore for session management."
	sigs := ExtractRelationshipSignals(text, candidates)
	if len(sigs) == 0 {
		t.Fatal("expected at least one relationship signal")
	}
	found := false
	for _, s := range sigs {
		if s.Signal == "depends on" || strings.Contains(s.Signal, "depends") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'depends on' signal, got %v", sigs)
	}
}

func TestExtractRelationshipSignals_EmptyCandidates(t *testing.T) {
	sigs := ExtractRelationshipSignals("foo depends on bar", nil)
	if len(sigs) != 0 {
		t.Errorf("expected no signals with empty candidates, got %d", len(sigs))
	}
}

func TestExtractRelationshipSignals_EmptyText(t *testing.T) {
	candidates := []EntityCandidate{{Name: "Foo"}, {Name: "Bar"}}
	sigs := ExtractRelationshipSignals("", candidates)
	if len(sigs) != 0 {
		t.Errorf("expected no signals for empty text, got %d", len(sigs))
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func candidateNames(cs []EntityCandidate) []string {
	names := make([]string, len(cs))
	for i, c := range cs {
		names[i] = c.Name
	}
	return names
}

func hasName(names []string, target string) bool {
	for _, n := range names {
		if strings.EqualFold(n, target) {
			return true
		}
	}
	return false
}
