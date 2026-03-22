package parser

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// TestParser_NilTree_NoPanic is a regression test verifying that parsers
// return nil (no error) without panicking when tree-sitter produces a nil
// tree from unparseable or degenerate input.
func TestParser_NilTree_NoPanic(t *testing.T) {
	// Inputs designed to stress tree-sitter: completely wrong language,
	// binary-like gibberish, null bytes, and empty content.
	inputs := []struct {
		name string
		data []byte
	}{
		{"empty", []byte{}},
		{"null_bytes", []byte{0x00, 0x00, 0x00}},
		{"binary_gibberish", []byte{0xff, 0xfe, 0x00, 0x01, 0x80, 0x7f}},
		{"single_null", []byte{0x00}},
		{"wrong_language_go", []byte("<html><body>not go code</body></html>")},
		{"wrong_language_py", []byte("CREATE TABLE foo (id INT);")},
	}

	parsers := []struct {
		name     string
		parser   LanguageParser
		filePath string
	}{
		{"Go", NewGoParser(), "/tmp/test.go"},
		{"Python", NewPythonParser(), "/tmp/test.py"},
		{"TypeScript", NewTypeScriptParser(), "/tmp/test.ts"},
		{"Rust", NewRustParser(), "/tmp/test.rs"},
	}

	for _, p := range parsers {
		for _, input := range inputs {
			t.Run(p.name+"/"+input.name, func(t *testing.T) {
				g := graph.New("test")
				// Must not panic. A nil return (no error) is the expected
				// behaviour when tree-sitter cannot produce a tree.
				err := p.parser.Parse(g, p.filePath, input.data)
				if err != nil {
					t.Errorf("expected nil error for degenerate input, got: %v", err)
				}
			})
		}
	}
}
