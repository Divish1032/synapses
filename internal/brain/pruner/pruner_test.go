package pruner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/brain/llm"
)

func TestNew_PositiveTimeout(t *testing.T) {
	client := llm.NewMockClient("")
	p := New(client, 5*time.Second)
	if p == nil {
		t.Fatal("expected non-nil Pruner")
	}
	if p.timeout != 5*time.Second {
		t.Errorf("expected timeout 5s, got %v", p.timeout)
	}
}

func TestNew_ZeroTimeoutDefaultsTen(t *testing.T) {
	client := llm.NewMockClient("")
	p := New(client, 0)
	if p == nil {
		t.Fatal("expected non-nil Pruner")
	}
	if p.timeout != 10*time.Second {
		t.Errorf("expected default timeout 10s, got %v", p.timeout)
	}
}

func TestPrune_EmptyString(t *testing.T) {
	client := llm.NewMockClient("some response")
	p := New(client, 5*time.Second)
	result, err := p.Prune(context.Background(), "")
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

func TestPrune_LLMReturnsValidContent(t *testing.T) {
	expected := "  trimmed content  "
	client := llm.NewMockClient(expected)
	p := New(client, 5*time.Second)
	result, err := p.Prune(context.Background(), "some input content")
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
	if result != strings.TrimSpace(expected) {
		t.Errorf("expected trimmed %q, got %q", strings.TrimSpace(expected), result)
	}
}

func TestPrune_LLMError_ReturnsOriginalContentAndError(t *testing.T) {
	originalContent := "original input content"
	client := llm.NewMockClient("")
	client.Err = errors.New("llm failure")
	p := New(client, 5*time.Second)
	result, err := p.Prune(context.Background(), originalContent)
	if err == nil {
		t.Error("expected non-nil error")
	}
	if result != originalContent {
		t.Errorf("expected original content %q, got %q", originalContent, result)
	}
}

func TestPrune_LLMReturnsEmpty_ReturnsOriginalContent(t *testing.T) {
	originalContent := "original input content"
	client := llm.NewMockClient("")
	p := New(client, 5*time.Second)
	result, err := p.Prune(context.Background(), originalContent)
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
	if result != originalContent {
		t.Errorf("expected original content %q, got %q", originalContent, result)
	}
}

func TestPrune_ContentExceedsMaxInputChars_TruncatesBeforeLLM(t *testing.T) {
	// Build content longer than maxInputChars (3000)
	longContent := strings.Repeat("a", maxInputChars+500)
	client := llm.NewMockClient("pruned result")
	p := New(client, 5*time.Second)
	result, err := p.Prune(context.Background(), longContent)
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
	if result != "pruned result" {
		t.Errorf("expected pruned result, got %q", result)
	}
}

func TestTruncate_ContentShorterThanMax_Unchanged(t *testing.T) {
	input := "short content"
	result := truncate(input, 100)
	if result != input {
		t.Errorf("expected unchanged %q, got %q", input, result)
	}
}

func TestTruncate_ContentLongerThanMax_TruncatedWithEllipsis(t *testing.T) {
	input := strings.Repeat("x", 200)
	maxChars := 50
	result := truncate(input, maxChars)
	// truncate takes maxChars runes then appends "..." so total length is maxChars+3
	if len(result) > maxChars+3 {
		t.Errorf("expected result length <= %d, got %d", maxChars+3, len(result))
	}
	if !strings.HasSuffix(result, "...") {
		t.Errorf("expected result to end with '...', got %q", result)
	}
}

func TestTruncate_ExactMax_Unchanged(t *testing.T) {
	input := strings.Repeat("y", 50)
	result := truncate(input, 50)
	if result != input {
		t.Errorf("expected unchanged content at exact max, got %q", result)
	}
}
