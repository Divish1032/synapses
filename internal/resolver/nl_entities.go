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

// resolveNLForSections is the shared implementation used by both public funcs.
func resolveNLForSections(g *graph.Graph, sections []*graph.Node) []parser.EntityCandidate {
	if len(sections) == 0 {
		return nil
	}

	// Build code-name lookup (same function used by docedges.go).
	// Candidates matching code entity names are skipped — docedges.go already
	// handles the EXPLAINS/DOCUMENTED_BY link for those.
	codeNames := buildCodeNames(g)

	// Build a set of existing knowledge node IDs to avoid duplicates across
	// multiple calls (e.g. initial index + watcher incremental update).
	existingKnowledge := buildKnowledgeNames(g)

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

			// Skip if already a known code entity.
			if _, isCode := codeNames[c.Name]; isCode {
				continue
			}
			// Also check normalised variant.
			if _, isCode := codeNames[norm]; isCode {
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
			// Use dedup: AddEdge is idempotent (graph ignores duplicate From+To+Type).
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
