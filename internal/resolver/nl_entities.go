package resolver

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ResolveNLEntities runs the Tier 0+1 NL-to-graph extraction pipeline for all
// markdown Section nodes in the graph.
//
// Tier 0: ExtractEntityCandidates scans section bodies for backtick spans,
// CamelCase tokens, quoted terms, and capitalized phrases.
//
// Tier 1: Each candidate is matched against existing code nodes by name.
//   - Match found → skip (docedges.go already created EXPLAINS/DOCUMENTED_BY).
//   - No match → create a NodeConcept knowledge node + RELATES_TO edge from
//     the section to the new knowledge node.
//
// Returns the unresolved candidates across all sections, suitable for Tier 2
// LLM classification via brain.Client.ScheduleNLClassification.
//
// Must be called after MarkdownParser.Parse (Section nodes must exist) and
// after ResolveDocEdges (so code-entity links don't get duplicated).
func ResolveNLEntities(g *graph.Graph) []parser.EntityCandidate {
	return resolveNLForSections(g, g.FindByType(graph.NodeSection))
}

// ResolveNLEntitiesForFile runs the Tier 0+1 NL-to-graph pipeline scoped to
// Section nodes belonging to filePath only. Use this in the watcher when a
// single markdown file changes — avoids rescanning all sections.
//
// Returns unresolved candidates from this file for Tier 2 classification.
func ResolveNLEntitiesForFile(g *graph.Graph, filePath string) []parser.EntityCandidate {
	abs := filepath.Clean(filePath)
	var sections []*graph.Node
	for _, s := range g.FindByType(graph.NodeSection) {
		if filepath.Clean(s.File) == abs {
			sections = append(sections, s)
		}
	}
	return resolveNLForSections(g, sections)
}

// ResolveNLEntitiesForFiles runs the Tier 0+1 pipeline for a set of markdown
// files in a single pass — buildCodeNames is called only once regardless of
// how many files are in the batch. Use this in the watcher for multi-file
// batches (initial index, branch switch) to avoid O(N×|graph|) redundancy.
//
// Returns a map from filePath → unresolved candidates for Tier 2 scheduling.
// Files with no unresolved candidates are omitted from the result.
func ResolveNLEntitiesForFiles(g *graph.Graph, filePaths []string) map[string][]parser.EntityCandidate {
	if len(filePaths) == 0 {
		return nil
	}

	// Build lookup maps ONCE for the entire batch.
	codeNames := buildCodeNames(g)
	codeNamesLower := buildCodeNamesLower(codeNames)
	// existingKnowledge is intentionally shared across all files in the batch:
	// knowledge NodeIDs are file-scoped so file A's nodes won't mask file B's,
	// but the shared map prevents duplicates within the same batch run.
	existingKnowledge := buildKnowledgeNames(g)

	// Index sections by cleaned file path for O(1) per-file lookup.
	type cleanPath = string
	absPaths := make(map[cleanPath]string, len(filePaths)) // abs → original
	for _, fp := range filePaths {
		absPaths[filepath.Clean(fp)] = fp
	}
	sectByFile := make(map[cleanPath][]*graph.Node, len(filePaths))
	for _, s := range g.FindByType(graph.NodeSection) {
		abs := filepath.Clean(s.File)
		if _, ok := absPaths[abs]; ok {
			sectByFile[abs] = append(sectByFile[abs], s)
		}
	}

	result := make(map[string][]parser.EntityCandidate, len(filePaths))
	for abs, sections := range sectByFile {
		unresolved := resolveNLCore(g, sections, codeNames, codeNamesLower, existingKnowledge)
		if len(unresolved) > 0 {
			result[absPaths[abs]] = unresolved
		}
	}
	return result
}

// resolveNLForSections is the shared implementation for single-graph-scope calls.
// It builds the lookup maps internally and delegates to resolveNLCore.
func resolveNLForSections(g *graph.Graph, sections []*graph.Node) []parser.EntityCandidate {
	if len(sections) == 0 {
		return nil
	}
	codeNames := buildCodeNames(g)
	codeNamesLower := buildCodeNamesLower(codeNames)
	existingKnowledge := buildKnowledgeNames(g)
	return resolveNLCore(g, sections, codeNames, codeNamesLower, existingKnowledge)
}

// resolveNLCore is the extraction kernel. It accepts pre-built lookup maps so
// ResolveNLEntitiesForFiles can reuse them across multiple file passes without
// redundant full-graph scans.
//
// existingKnowledge is mutated in-place (new node IDs are added as they are
// created) — callers that share it across multiple calls benefit from the
// cross-call dedup automatically.
func resolveNLCore(
	g *graph.Graph,
	sections []*graph.Node,
	codeNames map[string][]*graph.Node,
	codeNamesLower map[string]bool,
	existingKnowledge map[string]bool,
) []parser.EntityCandidate {
	if len(sections) == 0 {
		return nil
	}

	var unresolved []parser.EntityCandidate

	for _, sec := range sections {
		body := sec.Metadata["body"]
		if body == "" {
			continue
		}

		candidates := parser.ExtractEntityCandidates(body)
		if len(candidates) == 0 {
			continue
		}

		// For each candidate: check if it resolves to a code entity.
		// If yes — skip (docedges.go handles it).
		// If no — create a knowledge node and a RELATES_TO edge.
		for _, c := range candidates {
			norm := normalizeKnowledgeName(c.Name)
			if norm == "" {
				continue
			}

			// Skip if already a known code entity (exact or case-insensitive).
			// buildCodeNames keys are original-case ("TokenBucket"); codeNamesLower
			// handles docs that use lowercase references (`` `tokenbucket` ``).
			if _, isCode := codeNames[c.Name]; isCode {
				continue
			}
			if codeNamesLower[norm] {
				continue
			}

			// Create or reuse knowledge node.
			nodeID := makeKnowledgeNodeID(g, sec.File, norm)
			if !existingKnowledge[string(nodeID)] {
				g.AddNode(&graph.Node{
					ID:     nodeID,
					Type:   graph.NodeConcept, // default type; upgraded by Tier 2
					Name:   norm,
					File:   sec.File,
					Line:   c.SourceLine,
					Domain: graph.DomainKnowledge,
					Metadata: map[string]string{
						"context":    truncateContext(c.Context, 200),
						"confidence": fmt.Sprintf("%.2f", c.Confidence),
						"tier":       "0",
					},
				})
				existingKnowledge[string(nodeID)] = true
			}

			// RELATES_TO from section → knowledge node.
			// AddEdge is idempotent (graph ignores duplicate From+To+Type).
			g.AddEdge(&graph.Edge{
				From: sec.ID,
				To:   nodeID,
				Type: graph.EdgeRelatesTo,
			})

			unresolved = append(unresolved, c)
		}

		// Also extract relationship signals between known candidates and wire them.
		signals := parser.ExtractRelationshipSignals(body, candidates)
		applyRelationshipSignals(g, sec, signals, existingKnowledge)
	}

	return unresolved
}

// applyRelationshipSignals creates typed edges based on relationship keyword signals.
// The signal keyword determines the edge type; falls back to RELATES_TO.
func applyRelationshipSignals(g *graph.Graph, sec *graph.Node, signals []parser.RelationshipSignal, existing map[string]bool) {
	for _, sig := range signals {
		fromNorm := normalizeKnowledgeName(sig.From)
		toNorm := normalizeKnowledgeName(sig.To)
		if fromNorm == "" || toNorm == "" || fromNorm == toNorm {
			continue
		}

		fromID := makeKnowledgeNodeID(g, sec.File, fromNorm)
		toID := makeKnowledgeNodeID(g, sec.File, toNorm)

		// Only wire edges between nodes that exist in the graph.
		if !existing[string(fromID)] || !existing[string(toID)] {
			continue
		}

		et := signalToEdgeType(sig.Signal)
		g.AddEdge(&graph.Edge{From: fromID, To: toID, Type: et})
	}
}

// signalToEdgeType maps a relationship keyword to a graph EdgeType.
func signalToEdgeType(signal string) graph.EdgeType {
	lower := strings.ToLower(signal)
	switch {
	case strings.Contains(lower, "caused by") || strings.Contains(lower, "causes"):
		return graph.EdgeCausedBy
	case strings.Contains(lower, "instance of") || strings.Contains(lower, "type of"):
		return graph.EdgeInstanceOf
	case strings.Contains(lower, "contradicts") || strings.Contains(lower, "conflicts"):
		return graph.EdgeContradicts
	default:
		return graph.EdgeRelatesTo
	}
}

// buildCodeNamesLower returns a lowercase key set from a buildCodeNames map.
// Used so `` `tokenbucket` `` matches a code entity named "TokenBucket".
func buildCodeNamesLower(codeNames map[string][]*graph.Node) map[string]bool {
	m := make(map[string]bool, len(codeNames))
	for k := range codeNames {
		m[strings.ToLower(k)] = true
	}
	return m
}

// buildKnowledgeNames returns a set of existing knowledge node IDs.
func buildKnowledgeNames(g *graph.Graph) map[string]bool {
	knowledgeTypes := []graph.NodeType{
		graph.NodeConcept, graph.NodeEntity,
		graph.NodeArtifact, graph.NodeDecision,
	}
	m := make(map[string]bool)
	for _, nt := range knowledgeTypes {
		for _, n := range g.FindByType(nt) {
			m[string(n.ID)] = true
		}
	}
	return m
}

// makeKnowledgeNodeID creates a stable NodeID for a knowledge node.
// Format: MakeNodeID(filePath, "knowledge:"+normalizedName)
// File-scoped so the same concept name in different docs doesn't collide.
func makeKnowledgeNodeID(g *graph.Graph, filePath, name string) graph.NodeID {
	return g.MakeNodeID(filePath, "knowledge:"+name)
}

// NormalizeKnowledgeName returns the canonical form of a knowledge entity name:
// lowercase and trimmed. This is the key component used in knowledge NodeIDs.
// Exported so callers (e.g. the watcher) can reconstruct NodeIDs for Tier 2
// without duplicating the normalisation logic.
// Returns "" if the result is shorter than 3 characters.
func NormalizeKnowledgeName(name string) string {
	return normalizeKnowledgeName(name)
}

// normalizeKnowledgeName is the unexported implementation.
func normalizeKnowledgeName(name string) string {
	n := strings.TrimSpace(strings.ToLower(name))
	if len(n) < 3 {
		return ""
	}
	return n
}

// truncateContext truncates s to at most maxLen bytes at a word boundary.
func truncateContext(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	cut := s[:maxLen]
	if idx := strings.LastIndexByte(cut, ' '); idx > 0 {
		return cut[:idx]
	}
	return cut
}
