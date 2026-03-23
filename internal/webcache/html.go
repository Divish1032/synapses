package webcache

import (
	"strings"
	"unicode"
)

// StripHTML removes HTML tags and decodes common entities from s,
// returning plain text. Uses stdlib only — no external dependencies.
//
// This is intentionally simple: it handles pkg.go.dev and most documentation
// pages well enough for API signature verification. It does not implement a
// full HTML parser.
func StripHTML(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	inTag := false
	inScript := false
	inStyle := false
	i := 0

	for i < len(s) {
		c := s[i]

		// Detect opening <script> and <style> blocks to skip their content.
		if !inTag && c == '<' {
			if hasPrefixFold(s[i:], "<script") {
				inScript = true
			} else if hasPrefixFold(s[i:], "<style") {
				inStyle = true
			} else if hasPrefixFold(s[i:], "</script") {
				inScript = false
			} else if hasPrefixFold(s[i:], "</style") {
				inStyle = false
			}
			inTag = true
			i++
			continue
		}

		if inTag {
			if c == '>' {
				inTag = false
				// Treat block-level closing tags as newlines for readability.
				// Pre-lowercase the 20-byte window once to avoid O(n*m*9)
				// containsFold calls on crafted input.
				restLower := strings.ToLower(s[max(0, i-20) : i])
				for _, tag := range []string{"/p", "/div", "/li", "/h1", "/h2", "/h3", "/h4", "/pre", "/section", "br"} {
					if strings.Contains(restLower, tag) {
						b.WriteByte('\n')
						break
					}
				}
			}
			i++
			continue
		}

		if inScript || inStyle {
			i++
			continue
		}

		// Decode common HTML entities.
		if c == '&' {
			end := strings.IndexByte(s[i:], ';')
			if end > 0 && end < 10 {
				entity := s[i : i+end+1]
				decoded := decodeEntity(entity)
				b.WriteString(decoded)
				i += end + 1
				continue
			}
		}

		b.WriteByte(c)
		i++
	}

	// Collapse runs of whitespace (preserve single newlines).
	return collapseWhitespace(b.String())
}

func decodeEntity(e string) string {
	switch e {
	case "&amp;":
		return "&"
	case "&lt;":
		return "<"
	case "&gt;":
		return ">"
	case "&quot;":
		return `"`
	case "&#39;", "&apos;":
		return "'"
	case "&nbsp;":
		return " "
	case "&mdash;", "&#8212;":
		return "—"
	case "&ndash;", "&#8211;":
		return "–"
	case "&hellip;", "&#8230;":
		return "..."
	default:
		return e // leave unknown entities as-is
	}
}

// collapseWhitespace reduces runs of spaces/tabs to a single space,
// and runs of 3+ newlines to 2, while preserving structure.
func collapseWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	prevNewline := 0
	prevSpace := false

	for _, r := range s {
		switch {
		case r == '\n':
			prevNewline++
			prevSpace = false
			if prevNewline <= 2 {
				b.WriteRune('\n')
			}
		case unicode.IsSpace(r):
			if !prevSpace && prevNewline == 0 {
				b.WriteRune(' ')
			}
			prevSpace = true
		default:
			prevNewline = 0
			prevSpace = false
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func hasPrefixFold(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	return strings.EqualFold(s[:len(prefix)], prefix)
}

