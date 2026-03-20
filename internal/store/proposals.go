package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/SynapsesOS/synapses/internal/logutil"
)

// Proposal is an architectural change proposal that agents can vote on.
// Status flows: open → accepted | rejected | withdrawn.
type Proposal struct {
	ID            string   `json:"id"`
	AgentID       string   `json:"agent_id"`
	Title         string   `json:"title"`
	Description   string   `json:"description"`
	AffectedNodes []string `json:"affected_nodes"`
	Status        string   `json:"status"` // open | accepted | rejected | withdrawn
	VoteThreshold int      `json:"vote_threshold"`
	Votes         []Vote   `json:"votes"`
	ApproveCount  int      `json:"approve_count"`
	RejectCount   int      `json:"reject_count"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
}

// Vote represents a single agent's vote on a proposal.
type Vote struct {
	AgentID   string `json:"agent_id"`
	Vote      string `json:"vote"` // approve | reject | abstain
	Rationale string `json:"rationale,omitempty"`
	CreatedAt string `json:"created_at"`
}

// CreateProposal persists a new proposal and returns its ID.
// affectedNodes is a list of node IDs impacted by the change.
// voteThreshold is the number of approve (or reject) votes required to close the proposal.
// If voteThreshold < 1 it defaults to 2.
func (s *Store) CreateProposal(agentID, title, description string, affectedNodes []string, voteThreshold int) (string, error) {
	if voteThreshold < 1 {
		voteThreshold = 2
	}
	if affectedNodes == nil {
		affectedNodes = []string{}
	}
	id := newID()
	now := time.Now().UTC().Format(time.RFC3339)
	nodesJSON, _ := json.Marshal(affectedNodes)

	_, err := s.knowledgeDB.Exec(
		`INSERT INTO proposals (id, agent_id, title, description, affected_nodes, status, vote_threshold, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, 'open', ?, ?, ?)`,
		id, agentID, title, description, string(nodesJSON), voteThreshold, now, now,
	)
	if err != nil {
		return "", fmt.Errorf("create proposal: %w", err)
	}
	if payload, err2 := json.Marshal(map[string]string{"proposal_id": id, "title": title}); err2 == nil {
		if err2 = s.AppendEvent("proposal_created", agentID, string(payload)); err2 != nil {
			logutil.Debug("synapses: proposals: append proposal_created event for %q: %v\n", id, err2)
		}
	}
	return id, nil
}

// VoteOnProposal records (or updates) an agent's vote on a proposal.
// Returns the updated Proposal (with all votes and current tally) so callers
// can see whether the vote tipped the proposal to accepted/rejected.
// Votes on closed proposals (accepted/rejected/withdrawn) return an error.
func (s *Store) VoteOnProposal(proposalID, agentID, vote, rationale string) (*Proposal, error) {
	// Validate vote value.
	switch vote {
	case "approve", "reject", "abstain":
	default:
		return nil, fmt.Errorf("invalid vote %q: must be approve | reject | abstain", vote)
	}

	// Load the proposal to check it exists and is open.
	p, err := s.getProposal(proposalID)
	if err != nil {
		return nil, err
	}
	if p.Status != "open" {
		return nil, fmt.Errorf("proposal %q is already %s", proposalID, p.Status)
	}

	now := time.Now().UTC().Format(time.RFC3339)

	// Upsert the vote (each agent can change their mind until the proposal closes).
	_, err = s.knowledgeDB.Exec(
		`INSERT INTO proposal_votes (proposal_id, agent_id, vote, rationale, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(proposal_id, agent_id) DO UPDATE SET
		   vote = excluded.vote,
		   rationale = excluded.rationale`,
		proposalID, agentID, vote, rationale, now,
	)
	if err != nil {
		return nil, fmt.Errorf("record vote: %w", err)
	}

	// Re-load with fresh vote counts to check threshold.
	p, err = s.getProposal(proposalID)
	if err != nil {
		return nil, err
	}

	// Auto-resolve when a side reaches the threshold.
	newStatus := ""
	if p.ApproveCount >= p.VoteThreshold {
		newStatus = "accepted"
	} else if p.RejectCount >= p.VoteThreshold {
		newStatus = "rejected"
	}

	if newStatus != "" {
		if _, err := s.knowledgeDB.Exec(
			`UPDATE proposals SET status = ?, updated_at = ? WHERE id = ?`,
			newStatus, now, proposalID,
		); err != nil {
			return nil, fmt.Errorf("close proposal: %w", err)
		}
		p.Status = newStatus
		if payload, err2 := json.Marshal(map[string]string{"proposal_id": proposalID, "resolution": newStatus}); err2 == nil {
			if err2 = s.AppendEvent("proposal_resolved", agentID, string(payload)); err2 != nil {
				logutil.Debug("synapses: proposals: append proposal_resolved event for %q: %v\n", proposalID, err2)
			}
		}
	} else {
		if payload, err2 := json.Marshal(map[string]string{"proposal_id": proposalID, "vote": vote}); err2 == nil {
			if err2 = s.AppendEvent("proposal_voted", agentID, string(payload)); err2 != nil {
				logutil.Debug("synapses: proposals: append proposal_voted event for %q: %v\n", proposalID, err2)
			}
		}
	}

	return p, nil
}

// WithdrawProposal allows the creating agent to withdraw their own proposal.
func (s *Store) WithdrawProposal(proposalID, agentID string) (*Proposal, error) {
	p, err := s.getProposal(proposalID)
	if err != nil {
		return nil, err
	}
	if p.AgentID != agentID {
		return nil, fmt.Errorf("only the proposing agent (%s) can withdraw proposal %s", p.AgentID, proposalID)
	}
	if p.Status != "open" {
		return nil, fmt.Errorf("proposal %q is already %s", proposalID, p.Status)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.knowledgeDB.Exec(
		`UPDATE proposals SET status = 'withdrawn', updated_at = ? WHERE id = ?`, now, proposalID,
	); err != nil {
		return nil, fmt.Errorf("withdraw proposal: %w", err)
	}
	p.Status = "withdrawn"
	return p, nil
}

// GetProposals returns proposals filtered by status.
// Pass "" to return all proposals regardless of status.
func (s *Store) GetProposals(status string) ([]Proposal, error) {
	q := `SELECT id, agent_id, title, description, affected_nodes, status, vote_threshold, created_at, updated_at
	      FROM proposals`
	args := []interface{}{}
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC`

	rows, err := s.knowledgeDB.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("query proposals: %w", err)
	}
	defer rows.Close()

	var proposals []Proposal
	for rows.Next() {
		var p Proposal
		var nodesJSON string
		if err := rows.Scan(
			&p.ID, &p.AgentID, &p.Title, &p.Description, &nodesJSON,
			&p.Status, &p.VoteThreshold, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(nodesJSON), &p.AffectedNodes); err != nil {
			logutil.Debug("synapses: proposals: unmarshal affected_nodes for proposal %q: %v\n", p.ID, err)
		}
		if p.AffectedNodes == nil {
			p.AffectedNodes = []string{}
		}
		proposals = append(proposals, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Load votes for each proposal.
	for i := range proposals {
		votes, approve, reject, err := s.loadVotes(proposals[i].ID)
		if err != nil {
			return nil, err
		}
		proposals[i].Votes = votes
		proposals[i].ApproveCount = approve
		proposals[i].RejectCount = reject
	}
	return proposals, nil
}

// getProposal loads a single proposal with its votes by ID.
func (s *Store) getProposal(id string) (*Proposal, error) {
	row := s.knowledgeDB.QueryRow(
		`SELECT id, agent_id, title, description, affected_nodes, status, vote_threshold, created_at, updated_at
		 FROM proposals WHERE id = ?`, id,
	)
	var p Proposal
	var nodesJSON string
	if err := row.Scan(
		&p.ID, &p.AgentID, &p.Title, &p.Description, &nodesJSON,
		&p.Status, &p.VoteThreshold, &p.CreatedAt, &p.UpdatedAt,
	); err != nil {
		return nil, fmt.Errorf("get proposal %q: %w", id, err)
	}
	if err := json.Unmarshal([]byte(nodesJSON), &p.AffectedNodes); err != nil {
		logutil.Debug("synapses: proposals: unmarshal affected_nodes for proposal %q: %v\n", p.ID, err)
	}
	if p.AffectedNodes == nil {
		p.AffectedNodes = []string{}
	}
	votes, approve, reject, err := s.loadVotes(id)
	if err != nil {
		return nil, err
	}
	p.Votes = votes
	p.ApproveCount = approve
	p.RejectCount = reject
	return &p, nil
}

// loadVotes returns all votes for a proposal plus approve/reject tallies.
func (s *Store) loadVotes(proposalID string) ([]Vote, int, int, error) {
	rows, err := s.knowledgeDB.Query(
		`SELECT agent_id, vote, rationale, created_at FROM proposal_votes WHERE proposal_id = ? ORDER BY created_at ASC`,
		proposalID,
	)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()

	var votes []Vote
	approve, reject := 0, 0
	for rows.Next() {
		var v Vote
		if err := rows.Scan(&v.AgentID, &v.Vote, &v.Rationale, &v.CreatedAt); err != nil {
			return nil, 0, 0, err
		}
		votes = append(votes, v)
		switch v.Vote {
		case "approve":
			approve++
		case "reject":
			reject++
		}
	}
	if votes == nil {
		votes = []Vote{}
	}
	return votes, approve, reject, rows.Err()
}
