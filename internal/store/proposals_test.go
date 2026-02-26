package store_test

import (
	"testing"
)

func TestCreateProposal(t *testing.T) {
	st := openTestStore(t)

	id, err := st.CreateProposal("agent-1", "Merge handlers", "Combine into one package", nil, 2)
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty proposal ID")
	}

	proposals, err := st.GetProposals("")
	if err != nil {
		t.Fatalf("GetProposals: %v", err)
	}
	if len(proposals) != 1 {
		t.Fatalf("expected 1 proposal, got %d", len(proposals))
	}
	p := proposals[0]
	if p.Title != "Merge handlers" {
		t.Errorf("title = %q, want %q", p.Title, "Merge handlers")
	}
	if p.Status != "open" {
		t.Errorf("status = %q, want %q", p.Status, "open")
	}
	if p.VoteThreshold != 2 {
		t.Errorf("vote_threshold = %d, want 2", p.VoteThreshold)
	}
}

func TestVoteOnProposal_ApproveThreshold(t *testing.T) {
	st := openTestStore(t)

	id, err := st.CreateProposal("proposer", "Change X", "", nil, 2)
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}

	// First approve vote — should not resolve yet.
	p, err := st.VoteOnProposal(id, "agent-1", "approve", "looks good")
	if err != nil {
		t.Fatalf("VoteOnProposal: %v", err)
	}
	if p.Status != "open" {
		t.Errorf("status after 1 approve = %q, want open", p.Status)
	}
	if p.ApproveCount != 1 {
		t.Errorf("approve_count = %d, want 1", p.ApproveCount)
	}

	// Second approve vote — should auto-resolve to accepted.
	p, err = st.VoteOnProposal(id, "agent-2", "approve", "agree")
	if err != nil {
		t.Fatalf("VoteOnProposal: %v", err)
	}
	if p.Status != "accepted" {
		t.Errorf("status after 2 approves = %q, want accepted", p.Status)
	}
}

func TestVoteOnProposal_RejectThreshold(t *testing.T) {
	st := openTestStore(t)

	id, err := st.CreateProposal("proposer", "Bad idea", "", nil, 2)
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}

	st.VoteOnProposal(id, "agent-1", "reject", "nope")
	p, err := st.VoteOnProposal(id, "agent-2", "reject", "disagree")
	if err != nil {
		t.Fatalf("VoteOnProposal: %v", err)
	}
	if p.Status != "rejected" {
		t.Errorf("status = %q, want rejected", p.Status)
	}
}

func TestVoteOnProposal_ChangeVote(t *testing.T) {
	st := openTestStore(t)

	id, _ := st.CreateProposal("proposer", "Test", "", nil, 2)

	// Agent votes approve, then changes to reject.
	st.VoteOnProposal(id, "agent-1", "approve", "")
	p, err := st.VoteOnProposal(id, "agent-1", "reject", "changed mind")
	if err != nil {
		t.Fatalf("VoteOnProposal: %v", err)
	}
	if p.ApproveCount != 0 {
		t.Errorf("approve_count = %d, want 0 after vote change", p.ApproveCount)
	}
	if p.RejectCount != 1 {
		t.Errorf("reject_count = %d, want 1 after vote change", p.RejectCount)
	}
}

func TestVoteOnProposal_ClosedProposalError(t *testing.T) {
	st := openTestStore(t)

	id, _ := st.CreateProposal("proposer", "Test", "", nil, 1)

	// Resolve immediately with threshold=1.
	st.VoteOnProposal(id, "agent-1", "approve", "")

	// Voting on a closed proposal should fail.
	_, err := st.VoteOnProposal(id, "agent-2", "approve", "late")
	if err == nil {
		t.Fatal("expected error voting on closed proposal")
	}
}

func TestWithdrawProposal(t *testing.T) {
	st := openTestStore(t)

	id, _ := st.CreateProposal("proposer", "Test", "", nil, 2)

	p, err := st.WithdrawProposal(id, "proposer")
	if err != nil {
		t.Fatalf("WithdrawProposal: %v", err)
	}
	if p.Status != "withdrawn" {
		t.Errorf("status = %q, want withdrawn", p.Status)
	}
}

func TestWithdrawProposal_WrongAgent(t *testing.T) {
	st := openTestStore(t)

	id, _ := st.CreateProposal("proposer", "Test", "", nil, 2)

	_, err := st.WithdrawProposal(id, "other-agent")
	if err == nil {
		t.Fatal("expected error when non-proposer tries to withdraw")
	}
}

func TestGetProposals_StatusFilter(t *testing.T) {
	st := openTestStore(t)

	id1, _ := st.CreateProposal("a", "Open one", "", nil, 2)
	id2, _ := st.CreateProposal("a", "Will close", "", nil, 1)
	st.VoteOnProposal(id2, "agent", "approve", "")
	_ = id1

	open, _ := st.GetProposals("open")
	accepted, _ := st.GetProposals("accepted")

	if len(open) != 1 {
		t.Errorf("open proposals = %d, want 1", len(open))
	}
	if len(accepted) != 1 {
		t.Errorf("accepted proposals = %d, want 1", len(accepted))
	}
}
