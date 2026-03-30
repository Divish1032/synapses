package parser

import (
	"testing"
)

func TestExtractTextFromWordXML(t *testing.T) {
	xml := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:body>
    <w:p><w:r><w:t>INTRODUCTION</w:t></w:r></w:p>
    <w:p><w:r><w:t>This is the first paragraph.</w:t></w:r></w:p>
    <w:p><w:r><w:t>CHAPTER ONE</w:t></w:r></w:p>
    <w:p><w:r><w:t>Some content here.</w:t></w:r></w:p>
  </w:body>
</w:document>`)

	text := extractTextFromWordXML(xml)
	if text == "" {
		t.Fatal("expected non-empty text")
	}
	if !contains(text, "INTRODUCTION") {
		t.Error("missing INTRODUCTION")
	}
	if !contains(text, "CHAPTER ONE") {
		t.Error("missing CHAPTER ONE")
	}
	if !contains(text, "first paragraph") {
		t.Error("missing body text")
	}
}

func TestExtractTextFromWordXML_Empty(t *testing.T) {
	xml := []byte(`<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body></w:body></w:document>`)
	text := extractTextFromWordXML(xml)
	if len(text) > 5 { // may have trailing whitespace
		t.Errorf("expected near-empty text, got %q", text)
	}
}

func TestExtractTextFromWordXML_MultipleParagraphs(t *testing.T) {
	xml := []byte(`<w:document xmlns:w="urn:test"><w:body>
		<w:p><w:r><w:t>Line 1</w:t></w:r></w:p>
		<w:p><w:r><w:t>Line 2</w:t></w:r></w:p>
	</w:body></w:document>`)
	text := extractTextFromWordXML(xml)
	// Each paragraph should produce a newline-separated line.
	lines := splitNonEmpty(text)
	if len(lines) < 2 {
		t.Errorf("expected at least 2 lines, got %d from %q", len(lines), text)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && containsSubstring(s, substr)
}

func containsSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, line := range splitLines(s) {
		trimmed := trimSpace(line)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func trimSpace(s string) string {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\r' || s[i] == '\n') {
		i++
	}
	j := len(s)
	for j > i && (s[j-1] == ' ' || s[j-1] == '\t' || s[j-1] == '\r' || s[j-1] == '\n') {
		j--
	}
	return s[i:j]
}
