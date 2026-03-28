// Package resolver — nl_embed_doccode.go bridges documentation and code using
// embedding similarity.
//
// Problem: Name-matching (docedges.go) only links docs to code when the exact
// entity name appears in text. A "Deployment Guide" section that discusses
// running servers relates to Flask.run() but never mentions it by name.
//
// Solution: For each doc Section node, embed its title+body, search HNSW for
// similar code entities (functions, structs, etc.), and create EXPLAINS edges
// when similarity exceeds a threshold. This catches implicit doc↔code links.
//
// Cascade strategy (most specific → broadest):
//  1. Function/class level — exact name matches (handled by docedges.go)
//  2. Function/class level — embedding similarity (this file, high threshold)
//  3. File level — file path references in text (handled by docedges.go linkSectionsToFiles)
//  4. File level — embedding similarity against file nodes (this file, medium threshold)
//  5. Module/package level — embedding similarity (this file, lower threshold)
//
// Only levels 2, 4, 5 are implemented here. Levels 1, 3 are in docedges.go.
// The cascade logic: if a section already has specific edges (function-level),
// skip broader fallbacks to avoid noise. If no function-level links exist,
// try file-level, then module-level.
//
// Pipeline position: runs AFTER ResolveDocEdges (name-match) and AFTER the
// node embedding pass has populated HNSW vectors.
package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// docCodeEmbedThreshold is the minimum cosine similarity for an embedding-based
// doc→code EXPLAINS edge. Higher than knowledge↔knowledge (0.55) because
// cross-domain matching needs more confidence to avoid noise.
const docCodeEmbedThreshold = 0.60

// docCodeFileThreshold is the threshold for file-level fallback linking.
const docCodeFileThreshold = 0.55

// docCodeModuleThreshold is the threshold for module/package-level fallback.
const docCodeModuleThreshold = 0.50

// docCodeTimeout is the per-section budget for embedding calls.
const docCodeTimeout = 3 * time.Second

// DiscoverDocCodeRelations finds code entities that are semantically similar
// to doc sections and creates EXPLAINS/DOCUMENTED_BY edges.
//
// For each Section node that lacks function/class-level doc edges, it:
//  1. Embeds the section title + body preview
//  2. Searches HNSW for similar code entities
//  3. Creates edges at the most specific level available (function > file > module)
//
// Returns the number of EXPLAINS edges created.
// er must be non-nil; callers should guard before calling.
func DiscoverDocCodeRelations(g *graph.Graph, er EmbedResolver, threshold float64) int {
	if er == nil {
		return 0
	}
	if threshold <= 0 {
		threshold = docCodeEmbedThreshold
	}

	sections := g.FindByType(graph.NodeSection)
	if len(sections) == 0 {
		return 0
	}

	// Build sets of code node IDs by specificity level.
	codeEntityIDs := make(map[string]bool)  // functions, methods, structs, classes, etc.
	codeFileIDs := make(map[string]bool)    // NodeFile in DomainCode
	codePackageIDs := make(map[string]bool) // NodePackage

	for _, n := range g.AllNodes() {
		switch {
		case n.Type == graph.NodeFile && n.Domain != graph.DomainDocs:
			codeFileIDs[string(n.ID)] = true
		case n.Type == graph.NodePackage:
			codePackageIDs[string(n.ID)] = true
		case n.Domain != graph.DomainDocs && n.Type != graph.NodeSection &&
			n.Type != graph.NodeFile && n.Type != graph.NodePackage:
			codeEntityIDs[string(n.ID)] = true
		}
	}

	created := 0

	for _, sec := range sections {
		if sec.Domain != graph.DomainDocs {
			continue
		}

		// Check if this section already has function/class-level EXPLAINS edges
		// from name matching (docedges.go). If so, skip — we already have the
		// most specific link.
		existingLevel := existingDocEdgeLevel(g, sec, codeEntityIDs, codeFileIDs, codePackageIDs)

		// Build embed text from section title + body preview.
		title := sec.Metadata["title"]
		body := sec.Metadata["body"]
		if body == "" {
			body = sec.Metadata["body_preview"]
		}
		codeBlocksJSON := sec.Metadata["code_blocks"]
		embedText := buildSectionEmbedText(title, body, codeBlocksJSON)
		if len(embedText) < 10 {
			continue // too short to embed meaningfully
		}

		ctx, cancel := context.WithTimeout(context.Background(), docCodeTimeout)
		vec, err := er.EmbedText(ctx, embedText)
		cancel()
		if err != nil || len(vec) == 0 {
			continue
		}

		matches := er.SearchByVector(vec, 20)

		// Cascade: try most specific level first, stop when we find matches.
		if existingLevel < levelEntity {
			n := linkMatches(g, sec, matches, codeEntityIDs, threshold)
			created += n
			if n > 0 {
				continue // found function-level links, skip broader
			}
		}

		if existingLevel < levelFile {
			n := linkMatches(g, sec, matches, codeFileIDs, docCodeFileThreshold)
			created += n
			if n > 0 {
				continue // found file-level links, skip broader
			}
		}

		if existingLevel < levelModule {
			n := linkMatches(g, sec, matches, codePackageIDs, docCodeModuleThreshold)
			created += n
		}
	}

	return created
}

// specificity levels for cascade logic
const (
	levelNone   = 0
	levelModule = 1
	levelFile   = 2
	levelEntity = 3
)

// existingDocEdgeLevel checks what level of doc→code edges a section already has.
func existingDocEdgeLevel(g *graph.Graph, sec *graph.Node, entityIDs, fileIDs, packageIDs map[string]bool) int {
	level := levelNone
	for _, e := range g.OutEdges(sec.ID) {
		if e.Type != graph.EdgeExplains {
			continue
		}
		tid := string(e.To)
		if entityIDs[tid] && level < levelEntity {
			level = levelEntity
		} else if fileIDs[tid] && level < levelFile {
			level = levelFile
		} else if packageIDs[tid] && level < levelModule {
			level = levelModule
		}
	}
	return level
}

// linkMatches creates EXPLAINS/DOCUMENTED_BY edges between a section and matching
// code nodes from the HNSW search results. Only matches within the targetIDs set
// and above the threshold are linked. Returns count of edges created.
func linkMatches(g *graph.Graph, sec *graph.Node, matches []EmbedMatch, targetIDs map[string]bool, threshold float64) int {
	created := 0
	seen := make(map[string]bool)

	for _, m := range matches {
		if !targetIDs[m.NodeID] {
			continue
		}
		if m.Score < threshold {
			continue
		}
		if seen[m.NodeID] {
			continue
		}
		seen[m.NodeID] = true

		targetID := graph.NodeID(m.NodeID)

		// Check if edge already exists to avoid duplicates.
		alreadyLinked := false
		for _, e := range g.OutEdges(sec.ID) {
			if e.To == targetID && e.Type == graph.EdgeExplains {
				alreadyLinked = true
				break
			}
		}
		if alreadyLinked {
			continue
		}

		g.AddEdge(&graph.Edge{
			From: sec.ID,
			To:   targetID,
			Type: graph.EdgeExplains,
		})
		g.AddEdge(&graph.Edge{
			From: targetID,
			To:   sec.ID,
			Type: graph.EdgeDocumentedBy,
		})
		sec.Metadata["doc_link_source"] = "embedding"
		sec.Metadata["doc_link_confidence"] = fmt.Sprintf("%.3f", m.Score)
		created++

		// Limit edges per section to avoid noise (max 3 per specificity level).
		if created >= 3 {
			break
		}
	}

	return created
}

// buildSectionEmbedText constructs the text to embed for a doc section.
// Combines title, truncated body, and code block identifier names.
func buildSectionEmbedText(title, body, codeBlocksJSON string) string {
	var sb strings.Builder
	if title != "" {
		sb.WriteString(title)
	}
	if body != "" {
		if sb.Len() > 0 {
			sb.WriteString(": ")
		}
		b := body
		if len(b) > 300 {
			b = b[:300]
		}
		sb.WriteString(b)
	}
	// Append code block identifiers as structured suffix.
	if codeBlocksJSON != "" {
		names := extractCodeBlockNames(codeBlocksJSON)
		if len(names) > 0 {
			suffix := " [code: " + strings.Join(names, ", ") + "]"
			if sb.Len()+len(suffix) <= 500 {
				sb.WriteString(suffix)
			}
		}
	}
	// Hard cap at 500 chars.
	s := sb.String()
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}

// extractCodeBlockNames extracts identifier names from code blocks JSON metadata.
func extractCodeBlockNames(codeBlocksJSON string) []string {
	type cb struct {
		Language string `json:"language"`
		Content  string `json:"content"`
	}
	var blocks []cb
	if err := json.Unmarshal([]byte(codeBlocksJSON), &blocks); err != nil {
		return nil
	}
	seen := make(map[string]bool)
	var names []string
	for _, block := range blocks {
		idents := extractCodeBlockIdentifiers(block.Content, block.Language)
		for _, id := range idents {
			if !seen[id] {
				seen[id] = true
				names = append(names, id)
			}
		}
	}
	return names
}
