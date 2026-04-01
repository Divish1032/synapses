// source.go provides per-request source code extraction from project files.
// Used by the investigate handler to return actual code alongside graph metadata.
package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// maxBlockLines caps individual code blocks to prevent token explosion.
const maxBlockLines = 50

// sourceCache reads and caches file content for one handler invocation.
// Multiple nodes in the same file share a single read. The cache is created
// at handler start and garbage-collected when the handler returns.
type sourceCache struct {
	mu    sync.Mutex
	root  string              // project root for security check
	files map[string][]string // abs path → lines (1-indexed via index+1)
}

// newSourceCache creates a per-request source cache rooted at the project directory.
func newSourceCache(root string) *sourceCache {
	return &sourceCache{
		root:  root,
		files: make(map[string][]string),
	}
}

// loadFile reads and caches a file's lines. Returns nil if the file can't be
// read or is outside the project root.
func (c *sourceCache) loadFile(filePath string) []string {
	abs := filePath
	if !filepath.IsAbs(abs) {
		if c.root != "" {
			abs = filepath.Join(c.root, abs)
		} else {
			// No root set — cannot resolve relative paths.
			return nil
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if lines, ok := c.files[abs]; ok {
		return lines
	}

	// Security: reject paths that escape the project root.
	if c.root != "" && !pathWithinRoot(c.root, abs) {
		c.files[abs] = nil
		return nil
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		c.files[abs] = nil
		return nil
	}

	// Skip binary files (files with null bytes in first 512 bytes).
	preview := data
	if len(preview) > 512 {
		preview = preview[:512]
	}
	for _, b := range preview {
		if b == 0 {
			c.files[abs] = nil
			return nil
		}
	}

	lines := strings.Split(string(data), "\n")
	c.files[abs] = lines
	return lines
}

// Extract returns source code lines [startLine, endLine] (1-indexed, inclusive).
// Returns empty string if the file can't be read or lines are out of range.
// Caps extraction at maxBlockLines to prevent token explosion.
func (c *sourceCache) Extract(filePath string, startLine, endLine int) string {
	lines := c.loadFile(filePath)
	if lines == nil || startLine < 1 {
		return ""
	}

	// Clamp to file bounds.
	if startLine > len(lines) {
		return ""
	}
	if endLine > len(lines) {
		endLine = len(lines)
	}
	if endLine < startLine {
		endLine = startLine
	}

	// Enforce max block size.
	if endLine-startLine+1 > maxBlockLines {
		endLine = startLine + maxBlockLines - 1
	}

	// Convert to 0-indexed slice range.
	block := lines[startLine-1 : endLine]
	return strings.Join(block, "\n")
}

// TotalLines returns the total line count of a file. Returns 0 if unreadable.
func (c *sourceCache) TotalLines(filePath string) int {
	lines := c.loadFile(filePath)
	if lines == nil {
		return 0
	}
	return len(lines)
}

// computeEndLine estimates where an entity ends given:
//   - startLine: the entity's start line
//   - nextStart: the next entity's start line in the same file (0 if last entity)
//   - lineCount: metadata line_count from the parser (0 if unavailable)
//   - fileLines: total lines in the file
//
// Heuristic priority:
//  1. If parser provided line_count → use startLine + lineCount - 1
//  2. If there's a next entity in the file → use nextStart - 1 (leave a gap)
//  3. Fall back to startLine + maxBlockLines - 1
func computeEndLine(startLine, nextStart, lineCount, fileLines int) int {
	// Parser-provided line count is most accurate.
	if lineCount > 0 {
		end := startLine + lineCount - 1
		if end > fileLines {
			end = fileLines
		}
		return end
	}

	// Next entity boundary.
	if nextStart > startLine {
		end := nextStart - 1
		// Leave at least a 1-line gap for visual separation.
		if end > startLine && end-startLine > 2 {
			end-- // skip blank line before next entity
		}
		return end
	}

	// Fallback: cap at maxBlockLines.
	end := startLine + maxBlockLines - 1
	if end > fileLines {
		end = fileLines
	}
	return end
}
