package resolver

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// EmbedResolver provides optional embedding-based entity resolution.
// When non-nil, Tier 1 uses vector similarity in addition to name-matching.
// Implementations must be safe for concurrent use.
type EmbedResolver interface {
	// EmbedText returns a vector embedding for the given text.
	// Returns (nil, nil) if embedding is intentionally disabled.
	// Returns (nil, err) on transient failure — callers fall back to name-match.
	EmbedText(ctx context.Context, text string) ([]float32, error)

	// SearchByVector finds the top-k graph nodes most similar to queryVec.
	// Returns node IDs with cosine similarity scores, descending order.
	SearchByVector(queryVec []float32, k int) []EmbedMatch
}

// EmbedMatch is a single result from EmbedResolver.SearchByVector.
type EmbedMatch struct {
	NodeID string
	Score  float64 // cosine similarity [0, 1]
}

// embedTimeout is the per-candidate budget for embedding calls in Tier 1.
// If the embedder is slow, we skip to name-match rather than blocking the pipeline.
const embedTimeout = 2 * time.Second

// embedHighThreshold is the cosine similarity above which an entity candidate
// is considered a confident match to an existing graph node.
// From iText2KG (arxiv Sept 2024): reliable entity resolution at cosine > 0.6.
const embedHighThreshold = 0.6

// embedMidThreshold is the cosine similarity below which no match is assumed
// (genuinely new concept). Between mid and high the candidate is flagged for
// Tier 2 review.
const embedMidThreshold = 0.4

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
// When er is non-nil, Tier 1 also performs embedding-based HNSW similarity
// search. Candidates with cosine > 0.6 are wired directly to an existing graph
// node via EXPLAINS (Section→CodeEntity); candidates in the 0.4–0.6 band are
// created as knowledge nodes and flagged with embed_hint metadata for Tier 2.
//
// Returns the unresolved candidates across all sections, suitable for Tier 2
// LLM classification via brain.Client.ScheduleNLClassification.
//
// Must be called after MarkdownParser.Parse (Section nodes must exist) and
// after ResolveDocEdges (so code-entity links don't get duplicated).
func ResolveNLEntities(g *graph.Graph, er EmbedResolver) []parser.EntityCandidate {
	return resolveNLForSections(g, g.FindByType(graph.NodeSection), er)
}

// ResolveNLEntitiesForFile runs the Tier 0+1 NL-to-graph pipeline scoped to
// Section nodes belonging to filePath only. Use this in the watcher when a
// single markdown file changes — avoids rescanning all sections.
//
// Returns unresolved candidates from this file for Tier 2 classification.
func ResolveNLEntitiesForFile(g *graph.Graph, filePath string, er EmbedResolver) []parser.EntityCandidate {
	abs := filepath.Clean(filePath)
	var sections []*graph.Node
	for _, s := range g.FindByType(graph.NodeSection) {
		if filepath.Clean(s.File) == abs {
			sections = append(sections, s)
		}
	}
	return resolveNLForSections(g, sections, er)
}

// ResolveNLEntitiesForFiles runs the Tier 0+1 pipeline for a set of markdown
// files in a single pass — buildCodeNames is called only once regardless of
// how many files are in the batch. Use this in the watcher for multi-file
// batches (initial index, branch switch) to avoid O(N×|graph|) redundancy.
//
// Returns a map from filePath → unresolved candidates for Tier 2 scheduling.
// Files with no unresolved candidates are omitted from the result.
func ResolveNLEntitiesForFiles(g *graph.Graph, filePaths []string, er EmbedResolver) map[string][]parser.EntityCandidate {
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
	// absPaths maps cleaned path → original path (preserves caller's path format).
	absPaths := make(map[string]string, len(filePaths))
	for _, fp := range filePaths {
		absPaths[filepath.Clean(fp)] = fp
	}
	sectByFile := make(map[string][]*graph.Node, len(filePaths))
	for _, s := range g.FindByType(graph.NodeSection) {
		abs := filepath.Clean(s.File)
		if _, ok := absPaths[abs]; ok {
			sectByFile[abs] = append(sectByFile[abs], s)
		}
	}

	result := make(map[string][]parser.EntityCandidate, len(filePaths))
	for abs, sections := range sectByFile {
		unresolved := resolveNLCore(g, sections, codeNames, codeNamesLower, existingKnowledge, er)
		if len(unresolved) > 0 {
			result[absPaths[abs]] = unresolved
		}
	}
	return result
}

// resolveNLForSections is the shared implementation for single-graph-scope calls.
// It builds the lookup maps internally and delegates to resolveNLCore.
func resolveNLForSections(g *graph.Graph, sections []*graph.Node, er EmbedResolver) []parser.EntityCandidate {
	if len(sections) == 0 {
		return nil
	}
	codeNames := buildCodeNames(g)
	codeNamesLower := buildCodeNamesLower(codeNames)
	existingKnowledge := buildKnowledgeNames(g)
	return resolveNLCore(g, sections, codeNames, codeNamesLower, existingKnowledge, er)
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
	er EmbedResolver,
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

		// Frontmatter-derived candidates: tags and category from the file node
		// metadata are high-confidence entity signals (explicitly authored).
		fileNodeID := g.MakeNodeID(sec.File, sec.File)
		if fileNode := g.GetNode(fileNodeID); fileNode != nil {
			var fmTags []string
			if raw := fileNode.Metadata["frontmatter_tags"]; raw != "" {
				fmTags = strings.Split(raw, ",")
			}
			fmCandidates := parser.ExtractFrontmatterCandidates(fmTags, fileNode.Metadata["frontmatter_category"])
			candidates = append(candidates, fmCandidates...)
		}

		if len(candidates) == 0 {
			continue
		}

		// For each candidate: check if it resolves to a code entity.
		// If yes — skip (docedges.go handles it).
		// If no — try embedding-based resolution, then create a knowledge node.
		for _, c := range candidates {
			norm := normalizeKnowledgeName(c.Name)
			if norm == "" {
				continue
			}

			// Tier 1a: name-match (fast, zero-cost).
			// buildCodeNames keys are original-case ("TokenBucket"); codeNamesLower
			// handles docs that use lowercase references (`` `tokenbucket` ``).
			if _, isCode := codeNames[c.Name]; isCode {
				continue
			}
			if codeNamesLower[norm] {
				continue
			}

			// Tier 1b: embedding-based resolution.
			// When er is available, embed the candidate name+context and search
			// the HNSW index.
			//   Score > 0.6: confident match → EXPLAINS edge to existing node, skip unresolved.
			//   Score 0.4–0.6: ambiguous → knowledge node with embed_hint, queue for Tier 2.
			//   Score < 0.4 or embed error: new concept → standard knowledge node (tier=0).
			//
			// embedResolved is set true when the embedding path fully handles the
			// candidate so the standard knowledge-node creation below is skipped.
			embedResolved := false
			if er != nil {
				embedText := c.Name
				if c.Context != "" {
					embedText = c.Name + " " + truncateContext(c.Context, 100)
				}
				ctx, cancel := context.WithTimeout(context.Background(), embedTimeout)
				vec, embedErr := er.EmbedText(ctx, embedText)
				cancel()
				if embedErr == nil && len(vec) > 0 {
					matches := er.SearchByVector(vec, 5)
					// Find the best-scoring match explicitly (SearchByVector is
					// sorted descending but we guard defensively).
					bestScore := 0.0
					bestID := ""
					for _, m := range matches {
						if m.Score > bestScore {
							bestScore = m.Score
							bestID = m.NodeID
						}
					}

					switch {
					case bestScore > embedHighThreshold:
						// Confident match. Guard against stale HNSW entries: if the
						// matched node was deleted/renamed since the last HNSW rebuild,
						// treat it as a new concept (fall through to standard creation).
						matchID := graph.NodeID(bestID)
						if g.GetNode(matchID) != nil {
							// Section --EXPLAINS--> existing code entity.
							// (EdgeExplains is Section→Code; EdgeDocumentedBy is the reverse.)
							g.AddEdge(&graph.Edge{
								From: sec.ID,
								To:   matchID,
								Type: graph.EdgeExplains,
							})
							embedResolved = true // candidate resolved; skip knowledge node
						}
						// If node is stale (nil), embedResolved stays false → fall through.

					case bestScore > embedMidThreshold:
						// Ambiguous match: create knowledge node flagged for Tier 2 review.
						nodeID := makeKnowledgeNodeID(g, sec.File, norm)
						if !existingKnowledge[string(nodeID)] {
							g.AddNode(&graph.Node{
								ID:     nodeID,
								Type:   graph.NodeConcept,
								Name:   norm,
								File:   sec.File,
								Line:   c.SourceLine,
								Domain: graph.DomainKnowledge,
								Metadata: map[string]string{
									"context":     truncateContext(c.Context, 200),
									"confidence":  fmt.Sprintf("%.2f", c.Confidence),
									"tier":        "1",
									"embed_hint":  bestID,
									"embed_score": fmt.Sprintf("%.3f", bestScore),
								},
							})
							existingKnowledge[string(nodeID)] = true
						}
						g.AddEdge(&graph.Edge{
							From: sec.ID,
							To:   nodeID,
							Type: graph.EdgeRelatesTo,
						})
						unresolved = append(unresolved, c)
						embedResolved = true

						// default: bestScore <= embedMidThreshold — genuinely new concept;
						// fall through to standard creation with tier=0.
					}
				}
				// embedErr or empty vec: fall through to standard creation.
			}

			if embedResolved {
				continue
			}

			// Standard path: create or reuse a knowledge node (tier=0).
			// Reached when: er is nil, embedding failed, or score < 0.4 (new concept).
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
// Covers all 8 signal types detected by Tier 0 regex: depends on, implements,
// uses, extends, caused by, instance of, see also/related to, contradicts.
func signalToEdgeType(signal string) graph.EdgeType {
	lower := strings.ToLower(signal)
	switch {
	case strings.Contains(lower, "caused by") || strings.Contains(lower, "causes"):
		return graph.EdgeCausedBy
	case strings.Contains(lower, "instance of") || strings.Contains(lower, "type of"):
		return graph.EdgeInstanceOf
	case strings.Contains(lower, "contradicts") || strings.Contains(lower, "conflicts"):
		return graph.EdgeContradicts
	case strings.Contains(lower, "depends on") || strings.Contains(lower, "depends_on"):
		return graph.EdgeCausedBy // dependency is a causal relationship
	case strings.Contains(lower, "implements") || strings.Contains(lower, "implemented by"):
		return graph.EdgeInstanceOf // implementation is an instance-of relationship
	case strings.Contains(lower, "extends") || strings.Contains(lower, "extended by"):
		return graph.EdgeInstanceOf // extension is specialisation
	case strings.Contains(lower, "uses") || strings.Contains(lower, "used by"):
		return graph.EdgeRelatesTo // generic usage — no stronger semantic available
	default:
		return graph.EdgeRelatesTo
	}
}

// buildCodeNamesLower returns a lowercase key set from a buildCodeNames map.
// Used so “ `tokenbucket` “ matches a code entity named "TokenBucket".
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
