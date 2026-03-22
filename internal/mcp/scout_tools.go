package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/pulse"
	"github.com/SynapsesOS/synapses/internal/webcache"
)

// allowedDocsDomains is the set of domains permitted for lookup_docs.
// Prevents data exfiltration via URL query parameters to arbitrary hosts.
// Add domains as needed for documentation sources agents legitimately fetch.
var allowedDocsDomains = map[string]bool{
	// Language/framework docs
	"pkg.go.dev": true, "go.dev": true, "golang.org": true,
	"docs.python.org": true, "pypi.org": true,
	"developer.mozilla.org": true, "nodejs.org": true,
	"docs.rs": true, "crates.io": true,
	"docs.oracle.com": true, "kotlinlang.org": true,
	"learn.microsoft.com": true, "docs.microsoft.com": true,
	"developer.apple.com": true, "swift.org": true,
	"dart.dev": true, "api.dart.dev": true, "pub.dev": true, "api.flutter.dev": true,
	"typescriptlang.org": true, "www.typescriptlang.org": true,
	"react.dev": true, "vuejs.org": true, "angular.io": true, "svelte.dev": true,
	// Infrastructure/cloud
	"docs.docker.com": true, "kubernetes.io": true,
	"docs.aws.amazon.com": true, "cloud.google.com": true,
	"registry.terraform.io": true,
	// General reference
	"en.wikipedia.org": true, "stackoverflow.com": true,
	"github.com": true, "raw.githubusercontent.com": true,
	// Localhost (dev servers)
	"localhost": true, "127.0.0.1": true, "0.0.0.0": true,
}

// isAllowedDocsURL checks whether the URL's host is in the documentation allowlist.
// Matches exact domains and subdomains of allowed domains (e.g. "docs.python.org"
// matches because "python.org" is allowed). Does NOT match domains that merely
// contain an allowed domain as a suffix (e.g. "evil-github.com" does not match).
func isAllowedDocsURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if allowedDocsDomains[host] {
		return true
	}
	// Check if host is a subdomain of any allowed domain.
	// "docs.python.org" → check ".python.org" suffix against allowlist.
	// This correctly rejects "attacker.google.com" only if "attacker.google.com"
	// is checked as a subdomain — it matches "google.com" which IS in the list.
	// But "evil-github.com" does NOT match "github.com" because the suffix
	// check requires a dot boundary.
	for domain := range allowedDocsDomains {
		if strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

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
		return mcpgo.NewToolResultError("node_id is required (use find_entity or search to get node IDs)"), nil
	}
	agentID, _ := req.GetArguments()["agent_id"].(string)
	note, noteErr := stringArgLimited(req, "note", maxArgLengthNote)
	if noteErr != nil {
		return mcpgo.NewToolResultError(stripInternalPaths(noteErr.Error())), nil
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

	// OF-S2: scan note content for prompt injection patterns.
	// web_annotate is highest risk — content originates from web pages.
	var injectionWarning string
	if scanResult, scanErr := s.scanContent("note", note); scanErr != nil {
		return mcpgo.NewToolResultError(stripInternalPaths(scanErr.Error())), nil
	} else {
		note = scanResult.sanitized
		if scanResult.warning != "" {
			injectionWarning = scanResult.warning
			// P7-1: emit guard event for injection scan trigger.
			if pc := s.getPulseClient(); pc != nil {
				pc.RecordGuardEvent(pulse.GuardEvent{
					GuardType: "injection_scan", ToolName: "web_annotate",
					Category: "warn", AgentID: agentID, ProjectID: s.projectID,
				})
			}
		}
	}

	id, err := s.store.AddAnnotation(nodeID, agentID, note)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("store annotation failed: %v", stripInternalPaths(err.Error()))), nil
	}
	// P7-12: emit memory op for web annotation write.
	if pc := s.getPulseClient(); pc != nil {
		pc.RecordMemoryOp(pulse.MemoryOperationEvent{
			Operation: "web_annotation_write", Tier: "entity",
			ResultCount: 1, AgentID: agentID, ProjectID: s.projectID,
		})
	}
	_ = ctx
	resp := map[string]interface{}{
		"id":      id,
		"node_id": nodeID,
		"note":    note,
		"status":  "annotated — visible in get_context for this node",
	}
	if injectionWarning != "" {
		resp["injection_warning"] = injectionWarning
	}
	return jsonResult(resp)
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
			return mcpgo.NewToolResultError(fmt.Sprintf("lookup_docs: %v", stripInternalPaths(err.Error()))), nil
		}
	} else {
		// Cache disabled — fetch fresh, skip read/write
		var err error
		content, err = s.webCache.FetchPackageDocsFresh(ctx, importPath, version)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("lookup_docs: %v", stripInternalPaths(err.Error()))), nil
		}
	}

	// P7-13: emit search event for doc lookup.
	if pc := s.getPulseClient(); pc != nil {
		rc := 0
		if content != "" {
			rc = 1
		}
		pc.RecordSearchEvent(pulse.SearchEvent{
			Mode: "doc_lookup", Query: importPath,
			ResultCount: rc, CacheHit: fromCache, ProjectID: s.projectID,
		})
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
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return mcpgo.NewToolResultError("url must use http:// or https:// scheme"), nil
	}
	if !isAllowedDocsURL(url) {
		return mcpgo.NewToolResultError("url domain not in allowlist — lookup_docs is restricted to known documentation sites to prevent data exfiltration. Use web_search for general queries."), nil
	}
	var content string
	var fromCache bool
	if s.cacheWebSearches {
		var err error
		content, fromCache, err = s.webCache.Fetch(ctx, url, webcache.URLCacheTTL)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("lookup_docs: %v", stripInternalPaths(err.Error()))), nil
		}
	} else {
		var err error
		content, err = s.webCache.FetchFresh(ctx, url)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("lookup_docs: %v", stripInternalPaths(err.Error()))), nil
		}
	}
	// P7-13: emit search event for URL lookup.
	if pc := s.getPulseClient(); pc != nil {
		rc := 0
		if content != "" {
			rc = 1
		}
		pc.RecordSearchEvent(pulse.SearchEvent{
			Mode: "doc_lookup", Query: url,
			ResultCount: rc, CacheHit: fromCache, ProjectID: s.projectID,
		})
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
