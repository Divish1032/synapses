package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/peer"
)

// handleListPeers returns connection status for all configured peers.
func (s *Server) handleListPeers(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	pm := s.getPeerManager()
	if pm == nil {
		return jsonResult(map[string]interface{}{
			"peers": []interface{}{},
			"hint":  "No peers configured. Add peers to synapses.json under the 'peers' key with name, url, and token.",
		})
	}
	return jsonResult(map[string]interface{}{"peers": pm.GetStatuses()})
}

// handleGetPeerContext fetches a context subgraph for an entity in a peer project.
func (s *Server) handleGetPeerContext(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project := stringArg(req, "project")
	entity := stringArg(req, "entity")
	depth := 2
	if d, ok := req.Params.Arguments["depth"].(float64); ok && d > 0 {
		depth = int(d)
	}

	if project == "" {
		return mcp.NewToolResultError("project is required"), nil
	}
	if entity == "" {
		return mcp.NewToolResultError("entity is required"), nil
	}

	pm := s.getPeerManager()
	if pm == nil {
		return mcp.NewToolResultError("No peers configured. Add peers to synapses.json and restart."), nil
	}

	cli, err := pm.GetClient(project)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("peer %q not found: %v", project, err)), nil
	}

	sub, err := cli.QueryEntity(entity, depth)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("query peer %q: %v", project, err)), nil
	}

	return jsonResult(map[string]interface{}{
		"peer":     project,
		"entity":   entity,
		"subgraph": sub,
	})
}

// handleGetDependencyGraph returns an inter-project dependency overview with a
// Mermaid diagram visualising entity sharing across connected peers.
func (s *Server) handleGetDependencyGraph(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	localExported := 0
	for _, n := range s.graph.AllNodes() {
		if n.Exported {
			localExported++
		}
	}

	type peerEntry struct {
		Name        string `json:"name"`
		NodeCount   int    `json:"node_count"`
		SharedCount int    `json:"shared_count"`
		Connected   bool   `json:"connected"`
		Error       string `json:"error,omitempty"`
	}

	var peerEntries []peerEntry
	var mermaidLines []string
	mermaidLines = append(mermaidLines, "graph LR")
	localID := sanitizeMermaidID(s.graph.RepoID())
	mermaidLines = append(mermaidLines, fmt.Sprintf("  %s[%s]", localID, s.graph.RepoID()))

	pm := s.getPeerManager()
	if pm != nil {
		for _, st := range pm.GetStatuses() {
			peerEntries = append(peerEntries, peerEntry{
				Name:        st.Name,
				NodeCount:   st.NodeCount,
				SharedCount: st.SharedCount,
				Connected:   st.Connected,
				Error:       st.Error,
			})
			peerID := sanitizeMermaidID(st.Name)
			mermaidLines = append(mermaidLines, fmt.Sprintf("  %s[%s]", peerID, st.Name))
			if st.Connected && st.SharedCount > 0 {
				mermaidLines = append(mermaidLines, fmt.Sprintf(
					"  %s <-->|%d shared| %s", localID, st.SharedCount, peerID,
				))
			} else if st.Connected {
				mermaidLines = append(mermaidLines, fmt.Sprintf("  %s --- %s", localID, peerID))
			}
		}
	}
	if peerEntries == nil {
		peerEntries = []peerEntry{}
	}

	return jsonResult(map[string]interface{}{
		"local": map[string]interface{}{
			"project":        s.graph.RepoID(),
			"node_count":     s.graph.NodeCount(),
			"exported_count": localExported,
		},
		"peers":           peerEntries,
		"mermaid_diagram": strings.Join(mermaidLines, "\n"),
	})
}

// getPeerManager type-asserts the stored peerManager to *peer.PeerManager.
// Returns nil if no peer manager is configured.
func (s *Server) getPeerManager() *peer.PeerManager {
	if s.peerManager == nil {
		return nil
	}
	pm, _ := s.peerManager.(*peer.PeerManager)
	return pm
}

// sanitizeMermaidID strips characters that would break Mermaid node IDs.
func sanitizeMermaidID(name string) string {
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}
