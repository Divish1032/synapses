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
	absRoot, absErr := filepath.Abs(root)
	if absErr != nil {
		return false
	}
	absResolved, resErr := filepath.Abs(resolved)
	if resErr != nil || (!strings.HasPrefix(absResolved, absRoot+"/") && absResolved != absRoot) {
		logutil.Warn("synapses/security: skipped symlink resolving outside repo root: %s -> %s\n", path, resolved)
		return false
	}
	return true
}
