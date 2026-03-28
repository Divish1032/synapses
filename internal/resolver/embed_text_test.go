package resolver

import (
	"strings"
	"testing"
)

func TestBuildSectionEmbedText_WithCodeBlocks(t *testing.T) {
	codeBlocksJSON := `[{"language":"python","content":"from flask import Flask, render_template\napp = Flask(__name__)","line":5}]`
	text := buildSectionEmbedText("Quick Start", "Install Flask and run the app.", codeBlocksJSON)

	if !strings.Contains(text, "Quick Start") {
		t.Error("missing title in embed text")
	}
	if !strings.Contains(text, "Install Flask") {
		t.Error("missing body in embed text")
	}
	if !strings.Contains(text, "[code:") {
		t.Error("missing code block identifiers suffix")
	}
	if !strings.Contains(text, "Flask") {
		t.Error("missing Flask in code identifiers")
	}
	if len(text) > 500 {
		t.Errorf("embed text exceeds 500 char cap: %d", len(text))
	}
}

func TestBuildSectionEmbedText_NoCodeBlocks(t *testing.T) {
	text := buildSectionEmbedText("Overview", "Some body text.", "")
	if !strings.Contains(text, "Overview") {
		t.Error("missing title")
	}
	if strings.Contains(text, "[code:") {
		t.Error("should not have code block suffix when no code blocks")
	}
}

func TestBuildSectionEmbedText_Cap500(t *testing.T) {
	longBody := strings.Repeat("x", 600)
	text := buildSectionEmbedText("Title", longBody, "")
	if len(text) > 500 {
		t.Errorf("should be capped at 500, got %d", len(text))
	}
}
