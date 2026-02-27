package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// handleProposals dispatches between create and list based on the action param.
func (s *Server) handleProposals(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	action := stringArg(req, "action")
	if action == "" {
		action = "list"
	}
	switch action {
	case "create":
		return s.handleProposeChange(ctx, req)
	case "list":
		return s.handleGetProposals(ctx, req)
	default:
		return mcp.NewToolResultError("action must be 'create' or 'list'"), nil
	}
}

// handleVoteProposal dispatches between vote and withdraw based on the action param.
func (s *Server) handleVoteProposal(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	action := stringArg(req, "action")
	if action == "" {
		action = "vote"
	}
	switch action {
	case "vote":
		return s.handleVoteOnProposal(ctx, req)
	case "withdraw":
		return s.handleWithdrawProposal(ctx, req)
	default:
		return mcp.NewToolResultError("action must be 'vote' or 'withdraw'"), nil
	}
}

// handleProposeChange creates a new architectural change proposal.
func (s *Server) handleProposeChange(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("store not available"), nil
	}

	agentID, _ := req.Params.Arguments["agent_id"].(string)
	title, _ := req.Params.Arguments["title"].(string)
	description, _ := req.Params.Arguments["description"].(string)

	if title == "" {
		return mcp.NewToolResultError("title is required"), nil
	}

	// Parse optional affected_nodes JSON array.
	var affectedNodes []string
	if raw, ok := req.Params.Arguments["affected_nodes"].(string); ok && raw != "" {
		if err := json.Unmarshal([]byte(raw), &affectedNodes); err != nil {
			return mcp.NewToolResultError("affected_nodes must be a JSON array of node ID strings"), nil
		}
	}
	// Auto-detect nodes from title + description if none given.
	if len(affectedNodes) == 0 {
		affectedNodes = s.autoLinkNodes(title + " " + description)
	}

	threshold := 2
	if v, ok := req.Params.Arguments["vote_threshold"].(float64); ok && v >= 1 {
		threshold = int(v)
	}

	id, err := s.store.CreateProposal(agentID, title, description, affectedNodes, threshold)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("create proposal: %v", err)), nil
	}

	return jsonResult(map[string]interface{}{
		"proposal_id":    id,
		"status":         "open",
		"vote_threshold": threshold,
		"affected_nodes": affectedNodes,
		"message":        fmt.Sprintf("Proposal created. Other agents can now vote using vote_on_proposal with proposal_id=%q.", id),
	})
}

// handleVoteOnProposal records a vote on an existing proposal.
func (s *Server) handleVoteOnProposal(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("store not available"), nil
	}

	proposalID, _ := req.Params.Arguments["proposal_id"].(string)
	agentID, _ := req.Params.Arguments["agent_id"].(string)
	vote, _ := req.Params.Arguments["vote"].(string)
	rationale, _ := req.Params.Arguments["rationale"].(string)

	if proposalID == "" {
		return mcp.NewToolResultError("proposal_id is required"), nil
	}
	if vote == "" {
		return mcp.NewToolResultError("vote is required (approve | reject | abstain)"), nil
	}

	p, err := s.store.VoteOnProposal(proposalID, agentID, vote, rationale)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	result := map[string]interface{}{
		"proposal_id":    p.ID,
		"title":          p.Title,
		"status":         p.Status,
		"approve_count":  p.ApproveCount,
		"reject_count":   p.RejectCount,
		"vote_threshold": p.VoteThreshold,
		"votes":          p.Votes,
	}
	switch p.Status {
	case "accepted":
		result["message"] = fmt.Sprintf("Proposal ACCEPTED (%d/%d approve votes reached threshold). The change may proceed.", p.ApproveCount, p.VoteThreshold)
	case "rejected":
		result["message"] = fmt.Sprintf("Proposal REJECTED (%d/%d reject votes reached threshold). The change should not proceed.", p.RejectCount, p.VoteThreshold)
	default:
		remaining := p.VoteThreshold - p.ApproveCount
		if p.VoteThreshold-p.RejectCount < remaining {
			remaining = p.VoteThreshold - p.RejectCount
		}
		result["message"] = fmt.Sprintf("Vote recorded. %d more vote(s) needed to resolve.", remaining)
	}
	return jsonResult(result)
}

// handleWithdrawProposal lets the proposing agent cancel their proposal.
func (s *Server) handleWithdrawProposal(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("store not available"), nil
	}

	proposalID, _ := req.Params.Arguments["proposal_id"].(string)
	agentID, _ := req.Params.Arguments["agent_id"].(string)

	if proposalID == "" {
		return mcp.NewToolResultError("proposal_id is required"), nil
	}

	p, err := s.store.WithdrawProposal(proposalID, agentID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return jsonResult(map[string]interface{}{
		"proposal_id": p.ID,
		"status":      p.Status,
		"message":     "Proposal withdrawn.",
	})
}

// handleGetProposals lists proposals filtered by optional status.
func (s *Server) handleGetProposals(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.store == nil {
		return mcp.NewToolResultError("store not available"), nil
	}

	status, _ := req.Params.Arguments["status"].(string)

	if status != "" {
		valid := map[string]bool{"open": true, "accepted": true, "rejected": true, "withdrawn": true}
		if !valid[status] {
			return mcp.NewToolResultError("status must be one of: open | accepted | rejected | withdrawn"), nil
		}
	}

	proposals, err := s.store.GetProposals(status)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get proposals: %v", err)), nil
	}

	type proposalView struct {
		ID            string        `json:"id"`
		AgentID       string        `json:"agent_id"`
		Title         string        `json:"title"`
		Description   string        `json:"description"`
		Status        string        `json:"status"`
		AffectedNodes []string      `json:"affected_nodes"`
		VoteThreshold int           `json:"vote_threshold"`
		ApproveCount  int           `json:"approve_count"`
		RejectCount   int           `json:"reject_count"`
		Votes         []interface{} `json:"votes"`
		CreatedAt     string        `json:"created_at"`
	}

	views := make([]proposalView, 0, len(proposals))
	for _, p := range proposals {
		votes := make([]interface{}, 0, len(p.Votes))
		for _, v := range p.Votes {
			votes = append(votes, map[string]string{
				"agent_id":  v.AgentID,
				"vote":      v.Vote,
				"rationale": v.Rationale,
			})
		}
		views = append(views, proposalView{
			ID:            p.ID,
			AgentID:       p.AgentID,
			Title:         p.Title,
			Description:   p.Description,
			Status:        p.Status,
			AffectedNodes: p.AffectedNodes,
			VoteThreshold: p.VoteThreshold,
			ApproveCount:  p.ApproveCount,
			RejectCount:   p.RejectCount,
			Votes:         votes,
			CreatedAt:     p.CreatedAt,
		})
	}

	filterDesc := "all"
	if status != "" {
		filterDesc = status
	}

	// Surface open proposals waiting for votes.
	openCount := 0
	for _, p := range proposals {
		if p.Status == "open" {
			openCount++
		}
	}
	notice := ""
	if openCount > 0 && (status == "" || status == "open") {
		agentIDs := make(map[string]struct{})
		for _, p := range proposals {
			if p.Status == "open" {
				for _, v := range p.Votes {
					agentIDs[v.AgentID] = struct{}{}
				}
			}
		}
		voters := make([]string, 0, len(agentIDs))
		for id := range agentIDs {
			voters = append(voters, id)
		}
		if len(voters) > 0 {
			notice = fmt.Sprintf("%d open proposal(s) need votes. Agents that have already voted: %s.", openCount, strings.Join(voters, ", "))
		} else {
			notice = fmt.Sprintf("%d open proposal(s) are waiting for votes.", openCount)
		}
	}

	result := map[string]interface{}{
		"filter":    filterDesc,
		"count":     len(proposals),
		"proposals": views,
	}
	if notice != "" {
		result["notice"] = notice
	}
	return jsonResult(result)
}
