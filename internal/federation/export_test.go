package federation

import "github.com/SynapsesOS/synapses/internal/store"

// StructuralSignatureDiff is exported for testing.
var StructuralSignatureDiff = structuralSignatureDiff

// FileFromNodeID is exported for testing.
var FileFromNodeID = fileFromNodeID

// LangFromExt is exported for testing.
var LangFromExt = langFromExt

// StripGoCommentsAndStrings is exported for testing.
var StripGoCommentsAndStrings = stripGoCommentsAndStrings

// IsSiblingStoreFresh is exported for testing.
var IsSiblingStoreFresh = (*Resolver).isSiblingStoreFresh

// GetStore exports getStore for testing.
func (r *Resolver) GetStore(alias string) *store.Store {
	return r.getStore(alias)
}
