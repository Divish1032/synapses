package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSourceCache_Extract(t *testing.T) {
	// Create a temp dir with a test file.
	dir := t.TempDir()
	content := "line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\nline9\nline10\n"
	if err := os.WriteFile(filepath.Join(dir, "test.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cache := newSourceCache(dir)

	// Extract lines 2-4.
	got := cache.Extract("test.go", 2, 4)
	if got != "line2\nline3\nline4" {
		t.Errorf("Extract(2,4) = %q, want %q", got, "line2\nline3\nline4")
	}

	// Extract single line.
	got = cache.Extract("test.go", 1, 1)
	if got != "line1" {
		t.Errorf("Extract(1,1) = %q, want %q", got, "line1")
	}

	// Out of range — clamp to file.
	got = cache.Extract("test.go", 8, 20)
	if got == "" {
		t.Error("Extract(8,20) should return partial content, got empty")
	}

	// Start beyond file.
	got = cache.Extract("test.go", 100, 200)
	if got != "" {
		t.Errorf("Extract(100,200) should be empty, got %q", got)
	}

	// Missing file.
	got = cache.Extract("nonexistent.go", 1, 10)
	if got != "" {
		t.Errorf("Extract(missing) should be empty, got %q", got)
	}
}

func TestSourceCache_PathSecurity(t *testing.T) {
	dir := t.TempDir()
	// Create a file OUTSIDE the root.
	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	cache := newSourceCache(dir)

	// Attempt path traversal.
	got := cache.Extract(filepath.Join(outsideDir, "secret.txt"), 1, 1)
	if got != "" {
		t.Error("path traversal should be blocked, got content")
	}
}

func TestComputeEndLine(t *testing.T) {
	tests := []struct {
		name      string
		start     int
		nextStart int
		lineCount int
		fileLines int
		want      int
	}{
		{"from line_count", 10, 0, 20, 100, 29},
		{"from next entity", 10, 30, 0, 100, 28},
		{"fallback to maxBlock", 10, 0, 0, 200, 59},
		{"clamp to file end", 10, 0, 0, 15, 15},
		{"line_count exceeds file", 10, 0, 200, 50, 50},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeEndLine(tt.start, tt.nextStart, tt.lineCount, tt.fileLines)
			if got != tt.want {
				t.Errorf("computeEndLine(%d,%d,%d,%d) = %d, want %d",
					tt.start, tt.nextStart, tt.lineCount, tt.fileLines, got, tt.want)
			}
		})
	}
}
