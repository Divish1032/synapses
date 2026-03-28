package federation

import (
	"context"

	"github.com/SynapsesOS/synapses/internal/store"
)

// StructuralSignatureDiff is exported for testing.
var StructuralSignatureDiff = structuralSignatureDiff

// FileFromNodeID is exported for testing.
var FileFromNodeID = fileFromNodeID

// LangFromExt is exported for testing.
var LangFromExt = langFromExt

// StripGoCommentsAndStrings is exported for testing.
var StripGoCommentsAndStrings = stripGoCommentsAndStrings

// IsSiblingStoreFresh is exported for testing.
// Signature matches isSiblingStoreFresh: (ctx, store, head, repoPath).
func IsSiblingStoreFresh(r *Resolver, st *store.Store, head, repoPath string) bool {
	return r.isSiblingStoreFresh(context.Background(), st, head, repoPath)
}

// GetStore is now exported directly on Resolver — no test export needed.
