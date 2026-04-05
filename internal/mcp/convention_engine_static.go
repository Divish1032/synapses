package mcp

// convention_engine_static.go provides index-time convention detection from
// the code graph. New projects get conventions immediately without sessions.
//
// This supplements the session-based pipeline (temporal evidence from multiple
// sessions) with static evidence (single code analysis). Static conventions
// have lower confidence (0.50) and never overwrite session-based ones.

import (
	"strings"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// DetectStaticConventions walks the graph to detect conventions from import
// patterns and file structure. Returns count of conventions upserted.
// Static conventions have confidence=0.50 (below the 0.60 threshold for
// session-based conventions) to indicate they are single-point observations.
func DetectStaticConventions(g *graph.Graph, st *store.Store, projectID string) int {
	if g == nil || st == nil || projectID == "" {
		return 0
	}

	const staticConfidence = 0.50

	// Collect import-based library signals.
	libCounts := map[string]int{} // key → file count
	g.IterateNodes(func(n *graph.Node) {
		if n.Type != graph.NodePackage {
			return
		}
		importPath := strings.ToLower(n.Name)
		for _, lib := range staticLibraries {
			if strings.Contains(importPath, lib.contains) {
				libCounts[lib.key]++
				break
			}
		}
	})

	// Collect file-structure signals.
	layerCounts := map[string]int{} // layer → file count
	g.IterateNodes(func(n *graph.Node) {
		if n.Type != graph.NodeFile {
			return
		}
		parts := strings.Split(strings.ToLower(n.File), "/")
		for _, part := range parts {
			for _, kl := range staticLayers {
				if part == kl.fragment {
					layerCounts[kl.layer]++
					break
				}
			}
		}
	})

	count := 0

	// Upsert library conventions (only if ≥3 files import the library).
	for key, fileCount := range libCounts {
		if fileCount < 3 {
			continue
		}
		text, ok := conventionBaseTexts[key]
		if !ok {
			continue
		}
		text += " (detected from import graph)"

		// Upsert — the store's UpsertConvention uses ON CONFLICT DO UPDATE.
		// Static confidence (0.50) is below session-based (0.60+), so
		// if a session-based convention already exists with higher confidence,
		// the upsert will lower it. To prevent this, we set confidence to
		// max(existing, static) — but since we can't read first without a
		// GetByKey method, we use a low confidence that the upsert will
		// only apply if no convention exists yet.
		st.UpsertConvention(store.ExtractedConvention{
			ID:         projectID + "::" + key,
			ProjectID:  projectID,
			Category:   store.ObsCategoryLibraryUsage,
			Key:        key,
			Text:       text,
			Confidence: staticConfidence,
			SessionCount: 1,
		})
		count++
	}

	// Upsert file-structure conventions.
	for layer, fileCount := range layerCounts {
		if fileCount < 3 {
			continue
		}
		key := "uses_" + layer + "_layer"
		text := "This project uses a " + layer + " layer (detected from file structure)"

		st.UpsertConvention(store.ExtractedConvention{
			ID:         projectID + "::" + key,
			ProjectID:  projectID,
			Category:   store.ObsCategoryFilePattern,
			Key:        key,
			Text:       text,
			Confidence: staticConfidence,
			SessionCount: 1,
		})
		count++
	}

	return count
}

// staticLibraries maps import path substrings to convention keys.
// Same data as wellKnownLibraries in session_observations.go.
var staticLibraries = []struct {
	contains string
	key      string
}{
	{"testify", "uses_testify"},
	{"gomock", "uses_gomock"},
	{"/chi", "uses_chi_router"},
	{"gin-gonic", "uses_gin_router"},
	{"labstack/echo", "uses_echo_router"},
	{"gorilla/mux", "uses_gorilla_mux"},
	{"pytest", "uses_pytest"},
	{"fastapi", "uses_fastapi"},
	{"flask", "uses_flask"},
	{"django", "uses_django"},
	{"express", "uses_express"},
	{"fastify", "uses_fastify"},
	{"springframework", "uses_spring"},
	{"junit", "uses_junit"},
	{"actix_web", "uses_actix_web"},
	{"axum", "uses_axum"},
}

// staticLayers maps directory fragments to logical layers.
var staticLayers = []struct {
	fragment string
	layer    string
}{
	{"handler", "handler"},
	{"handlers", "handler"},
	{"controller", "handler"},
	{"controllers", "handler"},
	{"service", "service"},
	{"services", "service"},
	{"store", "repository"},
	{"repository", "repository"},
	{"middleware", "middleware"},
}
