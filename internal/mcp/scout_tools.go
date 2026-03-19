package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/webcache"
)

// handleWebAnnotate persists web findings as a graph node annotation so they
// survive across sessions and appear in get_context for that node.
// This is the "context sharing" pattern — web findings become first-class
// data objects attached to code entities.
func (s *Server) handleWebAnnotate(
	ctx context.Context,
	req mcpgo.CallToolRequest,
) (*mcpgo.CallToolResult, error) {
	if s.store == nil {
		return mcpgo.NewToolResultError("store not available (run synapses start, not synapses index)"), nil
	}

	nodeID, _ := req.GetArguments()["node_id"].(string)
	if nodeID == "" {
		return mcpgo.NewToolResultError("node_id is required"), nil
	}
	agentID, _ := req.GetArguments()["agent_id"].(string)
	note, noteErr := stringArgLimited(req, "note", maxArgLengthNote)
	if noteErr != nil {
		return mcpgo.NewToolResultError(noteErr.Error()), nil
	}

	// Optional: structured hits JSON to format as a readable annotation.
	type searchHit struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Snippet string `json:"snippet"`
	}
	if hitsJSON, ok := req.GetArguments()["hits"].(string); ok && hitsJSON != "" {
		var hits []searchHit
		if err := json.Unmarshal([]byte(hitsJSON), &hits); err == nil && len(hits) > 0 {
			var sb strings.Builder
			sb.WriteString("[web findings]")
			if note != "" {
				sb.WriteString(" ")
				sb.WriteString(note)
			}
			for i, h := range hits {
				if i >= 5 {
					break
				}
				fmt.Fprintf(&sb, "\n  - [%s](%s)", h.Title, h.URL)
				if h.Snippet != "" {
					sb.WriteString(": ")
					sb.WriteString(h.Snippet)
				}
			}
			note = sb.String()
		}
	}

	if note == "" {
		return mcpgo.NewToolResultError("note or hits is required"), nil
	}

	id, err := s.store.AddAnnotation(nodeID, agentID, note)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("store annotation failed: %v", err)), nil
	}
	_ = ctx
	return jsonResult(map[string]interface{}{
		"id":      id,
		"node_id": nodeID,
		"note":    note,
		"status":  "annotated — visible in get_context for this node",
	})
}

// handleLookupDocs returns cached Go package documentation or arbitrary URL
// content. Accepts one of:
//   - package= (Go import path, e.g. "github.com/mark3labs/mcp-go")
//   - url=     (arbitrary URL, cached for 24 hours)
//   - entity=  (code entity name — returns docs for all packages it imports)
//
// Package docs are version-pinned from go.mod and never expire unless go.mod
// changes. Results are cached cross-session in the local SQLite store.
func (s *Server) handleLookupDocs(
	ctx context.Context,
	req mcpgo.CallToolRequest,
) (*mcpgo.CallToolResult, error) {
	if s.webCache == nil {
		return mcpgo.NewToolResultError("doc cache not available"), nil
	}

	args := req.GetArguments()
	pkgParam, _ := args["package"].(string)
	urlParam, _ := args["url"].(string)
	entityParam, _ := args["entity"].(string)

	switch {
	case pkgParam != "":
		return s.lookupPackageDocs(ctx, pkgParam)
	case urlParam != "":
		return s.lookupURL(ctx, urlParam)
	case entityParam != "":
		return s.lookupEntityDocs(ctx, entityParam)
	default:
		return mcpgo.NewToolResultError("one of package, url, or entity is required"), nil
	}
}

func (s *Server) lookupPackageDocs(ctx context.Context, importPath string) (*mcpgo.CallToolResult, error) {
	version := ""
	if s.projectPath != "" {
		if versions, err := webcache.ParseGoMod(s.projectPath); err == nil && versions != nil {
			version = versions[importPath]
		}
	}

	var content string
	var fromCache bool
	if s.cacheWebSearches {
		var err error
		content, fromCache, err = s.webCache.FetchPackageDocs(ctx, importPath, version)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("lookup_docs: %v", err)), nil
		}
	} else {
		// Cache disabled — fetch fresh, skip read/write
		var err error
		content, err = s.webCache.FetchPackageDocsFresh(ctx, importPath, version)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("lookup_docs: %v", err)), nil
		}
	}

	result := map[string]interface{}{
		"import_path": importPath,
		"content":     content,
		"from_cache":  fromCache,
		"fetched_at":  time.Now().UTC().Format(time.RFC3339),
	}
	if version != "" {
		result["version"] = version
		result["note"] = fmt.Sprintf("docs pinned to go.mod version %s — re-fetched only on version bump", version)
	}
	if !s.cacheWebSearches {
		result["cache_disabled"] = true
	}
	return jsonResult(result)
}

func (s *Server) lookupURL(ctx context.Context, url string) (*mcpgo.CallToolResult, error) {
	var content string
	var fromCache bool
	if s.cacheWebSearches {
		var err error
		content, fromCache, err = s.webCache.Fetch(ctx, url, webcache.URLCacheTTL)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("lookup_docs: %v", err)), nil
		}
	} else {
		var err error
		content, err = s.webCache.FetchFresh(ctx, url)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("lookup_docs: %v", err)), nil
		}
	}
	result := map[string]interface{}{
		"url":        url,
		"content":    content,
		"from_cache": fromCache,
		"fetched_at": time.Now().UTC().Format(time.RFC3339),
		"ttl_hours":  webcache.URLCacheTTL,
	}
	if !s.cacheWebSearches {
		result["cache_disabled"] = true
	}
	return jsonResult(result)
}

func (s *Server) lookupEntityDocs(ctx context.Context, entityName string) (*mcpgo.CallToolResult, error) {
	if s.graph == nil {
		return mcpgo.NewToolResultError("graph not available"), nil
	}

	// Find the entity node by exact name or suffix match.
	var entityNode *graph.Node
	for _, n := range s.graph.AllNodes() {
		if n.Name == entityName || strings.HasSuffix(n.Name, "/"+entityName) {
			entityNode = n
			break
		}
	}
	if entityNode == nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("entity %q not found in graph", entityName)), nil
	}

	// Collect external package nodes reachable via IMPORTS edges from this entity.
	seen := make(map[string]bool)
	var packages []string
	for _, e := range s.graph.OutEdges(entityNode.ID) {
		if e.Type != graph.EdgeImports {
			continue
		}
		target := s.graph.GetNode(e.To)
		if target == nil || target.Type != graph.NodePackage {
			continue
		}
		if webcache.IsStdlib(target.Name) || seen[target.Name] {
			continue
		}
		seen[target.Name] = true
		packages = append(packages, target.Name)
	}

	if len(packages) == 0 {
		return mcpgo.NewToolResultError(fmt.Sprintf("no external package imports found for entity %q", entityName)), nil
	}

	// Parse go.mod versions once for all packages.
	versions := map[string]string{}
	if s.projectPath != "" {
		if v, err := webcache.ParseGoMod(s.projectPath); err == nil && v != nil {
			versions = v
		}
	}

	type pkgDoc struct {
		ImportPath string `json:"import_path"`
		Version    string `json:"version,omitempty"`
		Content    string `json:"content"`
		FromCache  bool   `json:"from_cache"`
	}
	var docs []pkgDoc
	for _, pkg := range packages {
		if len(docs) >= 5 {
			break
		}
		ver := versions[pkg]
		content, fromCache, err := s.webCache.FetchPackageDocs(ctx, pkg, ver)
		if err != nil {
			continue
		}
		docs = append(docs, pkgDoc{
			ImportPath: pkg,
			Version:    ver,
			Content:    content,
			FromCache:  fromCache,
		})
	}

	if len(docs) == 0 {
		return mcpgo.NewToolResultError("failed to fetch docs for any imported packages"), nil
	}

	return jsonResult(map[string]interface{}{
		"entity":   entityName,
		"packages": docs,
	})
}
