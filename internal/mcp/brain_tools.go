package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/brain"
)

// errJSON returns a tool result with a properly JSON-encoded error message.
// Internal file paths are stripped to avoid leaking system information.
func errJSON(err error) *mcp.CallToolResult {
	msg := stripInternalPaths(err.Error())
	b, _ := json.Marshal(map[string]string{"error": msg})
	return mcp.NewToolResultText(string(b))
}

// getBrainClient returns the stored brain client, or nil if not configured.
func (s *Server) getBrainClient() *brain.Client {
	return s.brainClient
}

// handleUpsertADR creates or updates an Architectural Decision Record in the brain.
func (s *Server) handleUpsertADR(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	bc := s.getBrainClient()
	if bc == nil {
		return mcp.NewToolResultText(`{"error": "brain not configured — add brain.url to synapses.json"}`), nil
	}

	id, _ := req.GetArguments()["id"].(string)
	title, _ := req.GetArguments()["title"].(string)
	decision, _ := req.GetArguments()["decision"].(string)
	if id == "" || title == "" || decision == "" {
		return mcp.NewToolResultText(`{"error": "id, title, and decision are required"}`), nil
	}

	_, statusProvided := req.GetArguments()["status"]
	status, _ := req.GetArguments()["status"].(string)
	if status == "" {
		status = "proposed"
	}
	contextText, _ := req.GetArguments()["context"].(string)
	consequences, _ := req.GetArguments()["consequences"].(string)

	var linkedFiles []string
	if lf, ok := req.GetArguments()["linked_files"].([]interface{}); ok {
		root := s.graph.Root()
		for _, f := range lf {
			if p, ok := f.(string); ok && p != "" {
				// Resolve relative paths against root; reject paths that escape.
				if root != "" {
					if !filepath.IsAbs(p) {
						p = filepath.Join(root, p)
					}
					if !pathWithinRoot(root, p) {
						continue
					}
				}
				linkedFiles = append(linkedFiles, p)
			}
		}
	}
	// If linked_files are specified but status was not explicitly set,
	// default to "accepted" so get_adrs(file=) surfaces this ADR immediately.
	if !statusProvided && len(linkedFiles) > 0 {
		status = "accepted"
	}

	adr, err := bc.UpsertADR(ctx, brain.ADRRequest{
		ID:           id,
		Title:        title,
		Status:       status,
		ContextText:  contextText,
		Decision:     decision,
		Consequences: consequences,
		LinkedFiles:  linkedFiles,
	})
	if err != nil {
		return errJSON(err), nil
	}
	return jsonResult(adr)
}

// handleGetADRs retrieves ADRs from the brain, optionally filtered by file path.
func (s *Server) handleGetADRs(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	bc := s.getBrainClient()
	if bc == nil {
		return mcp.NewToolResultText(`{"error": "brain not configured — add brain.url to synapses.json"}`), nil
	}

	fileFilter, _ := req.GetArguments()["file"].(string)
	adrs, err := bc.GetADRs(ctx, fileFilter)
	if err != nil {
		return errJSON(err), nil
	}
	if adrs == nil {
		adrs = []brain.ADR{}
	}
	result := map[string]interface{}{
		"adrs":  adrs,
		"count": len(adrs),
	}
	if fileFilter != "" {
		result["file_filter"] = fileFilter
		if len(adrs) == 0 {
			result["hint"] = "No ADRs linked to this file. When creating ADRs with upsert_adr, pass linked_files=[\"" + fileFilter + "\"] to enable file-based filtering."
		}
	}
	return jsonResult(result)
}

// ingestWebContent sends fetched web content to the intelligence sidecar as a
// fire-and-forget ingest. Used by handleWebFetch to enrich brain with web articles.
// No-op if brain is not configured or content is too short.
func (s *Server) ingestWebContent(url, title, contentMD string) {
	bc := s.getBrainClient()
	if bc == nil || len(contentMD) < 200 {
		return
	}
	// Truncate to 2000 chars — enough for the LLM to summarise without blowing budget.
	code := contentMD
	if len(code) > 2000 {
		code = code[:2000]
	}
	bc.Ingest(context.Background(), brain.IngestRequest{
		NodeID:   "web:" + url,
		NodeName: title,
		NodeType: "web_article",
		Package:  "web",
		Code:     code,
	})
}
