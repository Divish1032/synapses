package webcache

import (
	"strings"
	"testing"
)

func TestStripHTML_BasicTags(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "simple paragraph",
			input: "<p>Hello World</p>",
			want:  "Hello World",
		},
		{
			name:  "nested tags",
			input: "<div><p>Hello <strong>World</strong></p></div>",
			want:  "Hello World",
		},
		{
			name:  "no tags",
			input: "plain text",
			want:  "plain text",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "br tag becomes newline",
			input: "line1<br>line2",
			want:  "line1\nline2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StripHTML(tt.input)
			if got != tt.want {
				t.Errorf("StripHTML(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestStripHTML_ScriptAndStyle(t *testing.T) {
	input := `<html><head><script>alert('xss')</script><style>.cls{color:red}</style></head><body>Content</body></html>`
	got := StripHTML(input)
	if strings.Contains(got, "alert") {
		t.Errorf("script content not stripped: %q", got)
	}
	if strings.Contains(got, ".cls") {
		t.Errorf("style content not stripped: %q", got)
	}
	if !strings.Contains(got, "Content") {
		t.Errorf("body content missing: %q", got)
	}
}

func TestStripHTML_Entities(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"&amp;", "&"},
		{"&lt;", "<"},
		{"&gt;", ">"},
		{"&quot;", `"`},
		{"&#39;", "'"},
		{"&apos;", "'"},
		{"hello&nbsp;world", "hello world"},
		{"&mdash;", "\u2014"},
		{"&ndash;", "\u2013"},
		{"&hellip;", "..."},
		{"&#8212;", "\u2014"},
		{"&#8211;", "\u2013"},
		{"&#8230;", "..."},
	}
	for _, tt := range tests {
		got := StripHTML(tt.input)
		if got != tt.want {
			t.Errorf("StripHTML(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestStripHTML_BlockTags_Newlines(t *testing.T) {
	input := "<h1>Title</h1><p>Paragraph</p><div>Div content</div>"
	got := StripHTML(input)
	// Block tags should produce newlines
	if !strings.Contains(got, "\n") {
		t.Errorf("expected newlines from block tags, got: %q", got)
	}
	if !strings.Contains(got, "Title") {
		t.Errorf("missing Title: %q", got)
	}
	if !strings.Contains(got, "Paragraph") {
		t.Errorf("missing Paragraph: %q", got)
	}
}

func TestDecodeEntity(t *testing.T) {
	// Unknown entity stays as-is
	got := decodeEntity("&unknown;")
	if got != "&unknown;" {
		t.Errorf("unknown entity: got %q, want &unknown;", got)
	}
}

func TestCollapseWhitespace(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "multiple spaces",
			input: "hello    world",
			want:  "hello world",
		},
		{
			name:  "tabs to space",
			input: "hello\t\tworld",
			want:  "hello world",
		},
		{
			name:  "more than 2 newlines collapsed",
			input: "a\n\n\n\nb",
			want:  "a\n\nb",
		},
		{
			name:  "two newlines preserved",
			input: "a\n\nb",
			want:  "a\n\nb",
		},
		{
			name:  "leading trailing whitespace trimmed",
			input: "  hello  ",
			want:  "hello",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := collapseWhitespace(tt.input)
			if got != tt.want {
				t.Errorf("collapseWhitespace(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestMax(t *testing.T) {
	if max(3, 5) != 5 {
		t.Error("max(3,5) should be 5")
	}
	if max(7, 2) != 7 {
		t.Error("max(7,2) should be 7")
	}
	if max(4, 4) != 4 {
		t.Error("max(4,4) should be 4")
	}
}
