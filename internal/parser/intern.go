package parser

import "github.com/SynapsesOS/synapses/internal/graph"

// srcText extracts text from a source byte slice and deduplicates the
// resulting string via unique.Make(). This is a drop-in replacement for
// string(src[start:end]) that reduces heap usage by sharing identical
// strings across files (e.g. package names, common identifiers).
func srcText(src []byte, start, end uint) string {
	return graph.DedupString(string(src[start:end]))
}
