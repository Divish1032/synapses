package mcp

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// mojibakePattern describes a known double-encoded Unicode byte sequence.
// These arise when UTF-8 text is mistakenly saved as Latin-1 and then
// re-encoded as UTF-8, producing 2-3 bytes per original byte.
type mojibakePattern struct {
	name  string
	bytes []byte
}

// knownMojibakePatterns is the authoritative list of byte sequences that
// must never appear in Go source files. Each entry is the double-encoded
// UTF-8 representation of a common Unicode character.
//
// To generate a new entry: take the UTF-8 bytes of the character (e.g.
// E2 80 94 for em-dash), then re-encode each byte as a UTF-8 code point
// (byte B >= 0x80 → C2 B0|B, byte B >= 0xC0 → C3 B0|(B-0x40)).
var knownMojibakePatterns = []mojibakePattern{
	// em-dash —  (U+2014, UTF-8: E2 80 94)
	{"em-dash", []byte{0xC3, 0xA2, 0xC2, 0x80, 0xC2, 0x94}},
	// en-dash –  (U+2013, UTF-8: E2 80 93)
	{"en-dash", []byte{0xC3, 0xA2, 0xC2, 0x80, 0xC2, 0x93}},
	// left double-quote "  (U+201C, UTF-8: E2 80 9C)
	{"left-double-quote", []byte{0xC3, 0xA2, 0xC2, 0x80, 0xC2, 0x9C}},
	// right double-quote " (U+201D, UTF-8: E2 80 9D)
	{"right-double-quote", []byte{0xC3, 0xA2, 0xC2, 0x80, 0xC2, 0x9D}},
	// bullet •  (U+2022, UTF-8: E2 80 A2)
	{"bullet", []byte{0xC3, 0xA2, 0xC2, 0x80, 0xC2, 0xA2}},
	// middle dot ·  (U+00B7, UTF-8: C2 B7)  — double-encoded: C3 82 C2 B7
	{"middle-dot", []byte{0xC3, 0x82, 0xC2, 0xB7}},
	// non-breaking space (U+00A0, UTF-8: C2 A0) — double-encoded: C3 82 C2 A0
	{"non-break-space", []byte{0xC3, 0x82, 0xC2, 0xA0}},
}

// moduleRoot walks up from dir until it finds go.mod, returning that
// directory. Falls back to dir if go.mod is not found.
func moduleRoot(dir string) string {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}

// TestNoMojibakeInGoSources scans every .go file in the module for known
// double-encoded Unicode byte sequences. A hit means a file was saved with
// the wrong encoding and will display garbled characters to users or agents
// reading tool descriptions, error messages, or comments.
//
// This test is the regression guard for the Sprint 8 #5 fix. If it fails,
// fix the file using byte-level replacement (not string replacement, since
// the mojibake bytes are not valid UTF-8 as a string in some editors).
func TestNoMojibakeInGoSources(t *testing.T) {
	// Resolve module root from the package directory (CWD during go test).
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := moduleRoot(cwd)

	type finding struct {
		file    string
		pattern string
		count   int
	}
	var findings []finding

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable dirs
		}
		if d.IsDir() {
			// Skip vendor, testdata, hidden dirs, archived sub-projects.
			name := d.Name()
			if name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			// Skip archived and non-Go sub-projects in the monorepo.
			if name == "archived" || name == "synapses-app" ||
				name == "synapses-fine-distilling" || name == "synapses-intelligence" ||
				name == "synapses-scout" || name == "synapses-pulse" {
				return filepath.SkipDir
			}
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil // skip unreadable files
		}

		rel, _ := filepath.Rel(root, path)
		for _, p := range knownMojibakePatterns {
			if n := bytes.Count(data, p.bytes); n > 0 {
				findings = append(findings, finding{rel, p.name, n})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	for _, f := range findings {
		t.Errorf("mojibake found: %s — %dx %s (double-encoded Unicode). "+
			"Fix with byte-level replacement of %v.",
			f.file, f.count, f.pattern,
			knownMojibakePatterns[findPatternIndex(f.pattern)].bytes)
	}
}

// TestServerGoIsValidUTF8 verifies that server.go is valid UTF-8 and contains
// known Unicode characters (em-dash) rather than mojibake surrogates.
func TestServerGoIsValidUTF8(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := moduleRoot(cwd)

	// Read server.go where tool descriptions live.
	serverPath := filepath.Join(root, "synapses", "internal", "mcp", "server.go")
	data, err := os.ReadFile(serverPath)
	if err != nil {
		// Try the package-relative path (when CWD is internal/mcp already).
		serverPath = "server.go"
		data, err = os.ReadFile(serverPath)
		if err != nil {
			t.Fatalf("could not read server.go: %v", err)
		}
	}

	// Verify the entire file is valid UTF-8.
	if !utf8.Valid(data) {
		t.Error("server.go is not valid UTF-8 — check for mixed encodings")
	}

	// Verify em-dash characters are present (used in tool descriptions).
	content := string(data)
	wantEmdash := "\u2014" // — (U+2014)
	if !strings.Contains(content, wantEmdash) {
		t.Errorf("server.go missing em-dash (—); check for mojibake")
	}
}

// findPatternIndex returns the index of the pattern with the given name,
// or 0 if not found.
func findPatternIndex(name string) int {
	for i, p := range knownMojibakePatterns {
		if p.name == name {
			return i
		}
	}
	return 0
}
