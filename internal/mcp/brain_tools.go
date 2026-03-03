package mcp

import (
	"context"

	"github.com/SynapsesOS/synapses/internal/brain"
)

// getBrainClient type-asserts the stored brainClient to *brain.Client.
// Returns nil if no brain client is configured.
func (s *Server) getBrainClient() *brain.Client {
	if s.brainClient == nil {
		return nil
	}
	bc, _ := s.brainClient.(*brain.Client)
	return bc
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
