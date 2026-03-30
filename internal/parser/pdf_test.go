package parser

import (
	"testing"
)

func TestExtractPDFSections_BasicPages(t *testing.T) {
	// Simulate pdftotext output with form-feed page breaks.
	text := "INTRODUCTION\nThis is the intro.\n\fCHAPTER ONE\nSome content here.\n"

	sections := extractPDFSections(text)
	if len(sections) < 2 {
		t.Fatalf("expected at least 2 sections, got %d", len(sections))
	}
	if sections[0].Title != "INTRODUCTION" {
		t.Errorf("first section title: want INTRODUCTION, got %q", sections[0].Title)
	}
	if sections[0].Depth != 1 {
		t.Errorf("ALL CAPS heading should be depth 1, got %d", sections[0].Depth)
	}
}

func TestExtractPDFSections_NumberedHeadings(t *testing.T) {
	text := "1.1 Overview\nSome overview text.\n1.2 Details\nSome detail text.\n"

	sections := extractPDFSections(text)
	if len(sections) != 2 {
		t.Fatalf("expected 2 sections, got %d", len(sections))
	}
	if sections[0].Depth != 2 {
		t.Errorf("1.1 should be depth 2, got %d", sections[0].Depth)
	}
	if sections[1].Depth != 2 {
		t.Errorf("1.2 should be depth 2, got %d", sections[1].Depth)
	}
}

func TestExtractPDFSections_EmptyText(t *testing.T) {
	sections := extractPDFSections("")
	if len(sections) != 0 {
		t.Errorf("expected 0 sections for empty text, got %d", len(sections))
	}
}

func TestExtractPDFSections_PageFallback(t *testing.T) {
	// No headings detected — should create "Page N" fallback sections.
	text := "Just some regular text on page one.\n\fMore text on page two.\n"

	sections := extractPDFSections(text)
	if len(sections) < 1 {
		t.Fatalf("expected at least 1 section, got %d", len(sections))
	}
	if sections[0].Title != "Page 1" {
		t.Errorf("expected 'Page 1' fallback title, got %q", sections[0].Title)
	}
}

func TestParseNumberedHeading(t *testing.T) {
	tests := []struct {
		input string
		depth int
		title string
	}{
		{"1 Introduction", 1, "1 Introduction"},
		{"1.2 Overview", 2, "1.2 Overview"},
		{"1.2.3 Deep Section", 3, "1.2.3 Deep Section"},
		{"Not a heading", 0, ""},
		{"", 0, ""},
		{"123", 0, ""},       // no space + title
		{"1. X", 0, ""},      // title too short after numbering
	}
	for _, tt := range tests {
		depth, title := parseNumberedHeading(tt.input)
		if depth != tt.depth || (depth > 0 && title != tt.title) {
			t.Errorf("parseNumberedHeading(%q) = (%d, %q), want (%d, %q)", tt.input, depth, title, tt.depth, tt.title)
		}
	}
}

func TestIsAllCaps(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"HELLO WORLD", true},
		{"Hello World", false},
		{"ALL CAPS 123", true},
		{"123", false}, // no letters
		{"A", true},
		{"a", false},
	}
	for _, tt := range tests {
		got := isAllCaps(tt.input)
		if got != tt.want {
			t.Errorf("isAllCaps(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestMakeSection_Truncation(t *testing.T) {
	// Body longer than 2000 chars should be truncated.
	longBody := make([]byte, 3000)
	for i := range longBody {
		longBody[i] = 'a'
	}
	sec := makeSection("Title", 1, 1, string(longBody))
	if len(sec.Body) != 2000 {
		t.Errorf("body should be truncated to 2000 chars, got %d", len(sec.Body))
	}
	if len(sec.BodyPreview) != 200 {
		t.Errorf("preview should be truncated to 200 chars, got %d", len(sec.BodyPreview))
	}
}

func TestIsNumber(t *testing.T) {
	if !isNumber("123") {
		t.Error("123 should be a number")
	}
	if isNumber("abc") {
		t.Error("abc should not be a number")
	}
	if !isNumber("1.2.3") {
		t.Error("1.2.3 should be treated as number")
	}
}
