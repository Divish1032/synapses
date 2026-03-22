package parser

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"github.com/SynapsesOS/synapses/internal/logutil"
)

// disambiguateM inspects the first non-empty lines of a .m file to determine
// whether it is Objective-C or MATLAB. Returns the appropriate parser.
// Heuristic: #import, #include, @interface, @implementation, @protocol,
// @property → ObjC; otherwise MATLAB.
func (w *Walker) disambiguateM(path string) LanguageParser {
	if w.mObjCParser == nil || w.mMATLABParser == nil {
		return w.parsers[".m"]
	}
	f, err := os.Open(path)
	if err != nil {
		return w.mMATLABParser
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	linesChecked := 0
	for scanner.Scan() && linesChecked < 30 {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "/*") {
			continue
		}
		linesChecked++
		if strings.HasPrefix(line, "#import") || strings.HasPrefix(line, "#include") ||
			strings.HasPrefix(line, "@interface") || strings.HasPrefix(line, "@implementation") ||
			strings.HasPrefix(line, "@protocol") || strings.HasPrefix(line, "@property") {
			return w.mObjCParser
		}
	}
	return w.mMATLABParser
}

// resolveParser returns the correct parser for a file, handling ambiguous
// extensions like .m (ObjC vs MATLAB) via content-based disambiguation.
func (w *Walker) resolveParser(path, ext string) (LanguageParser, bool) {
	if ext == ".m" && w.mObjCParser != nil {
		return w.disambiguateM(path), true
	}
	p, ok := w.parsers[ext]
	if ok {
		return p, true
	}
	base := filepath.Base(path)
	p, ok = w.filenameParsers[base]
	if ok {
		return p, true
	}
	for _, entry := range w.filenamePrefixParsers {
		if strings.HasPrefix(base, entry.prefix) {
			return entry.parser, true
		}
	}
	return nil, false
}

// isPathContainedIn checks whether resolved is located inside (or equal to)
// root by walking up the directory tree and comparing inodes via os.SameFile.
//
// Why inodes instead of string comparison?
//   - Immune to case-sensitivity differences (e.g. macOS HFS+/APFS).
//   - Immune to Unicode normalization variants (NFC vs NFD).
//   - Compares device+inode, so bind-mounts and case-variant paths are
//     handled correctly.
//   - Eliminates the string-based TOCTOU window on the path comparison
//     itself (though the symlink target can still change between
//     EvalSymlinks and this walk — an inherent limitation).
//
// A depth limit of 256 prevents infinite loops on circular mounts.
func isPathContainedIn(resolved, root string) bool {
	rootInfo, err := os.Stat(root)
	if err != nil {
		return false
	}

	cur := resolved
	// If resolved is a file, start from its parent directory.
	if info, err := os.Stat(cur); err != nil || !info.IsDir() {
		cur = filepath.Dir(cur)
	}

	const maxDepth = 256
	for i := 0; i < maxDepth; i++ {
		curInfo, err := os.Stat(cur)
		if err != nil {
			return false
		}
		if os.SameFile(curInfo, rootInfo) {
			return true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached filesystem root without matching.
			return false
		}
		cur = parent
	}
	return false
}

// isSymlinkContained checks if a file is a symlink and, if so, verifies that
// it resolves to a target within the repository root. Returns true if the file
// is safe to process (not a symlink, or symlink within root).
func isSymlinkContained(info os.FileInfo, path, root string) bool {
	if info.Mode()&os.ModeSymlink == 0 {
		return true
	}
	resolved, symErr := filepath.EvalSymlinks(path)
	if symErr != nil {
		return false
	}
	if !isPathContainedIn(resolved, root) {
		logutil.Warn("synapses/security: skipped symlink resolving outside repo root: %s -> %s\n", path, resolved)
		return false
	}
	return true
}
