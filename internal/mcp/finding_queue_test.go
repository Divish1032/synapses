package mcp

import (
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/SynapsesOS/synapses/internal/security"
)

// newTestViolation builds a minimal security.Violation for testing.
func newTestViolation(patternID, target string, sev security.Severity) security.Violation {
	return security.Violation{
		PatternID:   patternID,
		PatternName: patternID,
		Severity:    sev,
		Action:      "block",
		File:        "some/file.go",
		Target:      target,
		Message:     "test finding: " + patternID,
		Evidence:    "evidence for " + patternID,
	}
}

func TestFindingQueue_EnqueueDequeue_Basic(t *testing.T) {
	q := newFindingQueue()
	const sid = "session-1"

	v := newTestViolation("pat-1", "handlerFoo", security.SeverityHigh)
	added := q.Enqueue(sid, v)
	if !added {
		t.Fatal("expected Enqueue to return true for new finding")
	}

	out := q.Dequeue(sid, 10)
	if len(out) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(out))
	}
	if out[0].PatternID != "pat-1" {
		t.Errorf("unexpected PatternID: %s", out[0].PatternID)
	}

	// Queue is now empty — second dequeue returns nil.
	out2 := q.Dequeue(sid, 10)
	if len(out2) != 0 {
		t.Errorf("expected empty dequeue after delivery, got %d", len(out2))
	}
}

func TestFindingQueue_ExactlyOnceDelivery(t *testing.T) {
	q := newFindingQueue()
	const sid = "session-once"

	q.Enqueue(sid, newTestViolation("a", "tgt", security.SeverityCritical))
	q.Dequeue(sid, 5)

	// Second dequeue must be empty.
	if got := q.Dequeue(sid, 5); len(got) != 0 {
		t.Errorf("finding delivered twice; second dequeue returned %d findings", len(got))
	}
}

func TestFindingQueue_MaxN(t *testing.T) {
	q := newFindingQueue()
	const sid = "session-max"

	for i := range 5 {
		q.Enqueue(sid, newTestViolation("pat", string(rune('A'+i)), security.SeverityHigh))
	}

	out := q.Dequeue(sid, 3)
	if len(out) != 3 {
		t.Errorf("expected 3 findings with maxN=3, got %d", len(out))
	}
	// 2 remain in queue.
	remaining := q.Dequeue(sid, 10)
	if len(remaining) != 2 {
		t.Errorf("expected 2 remaining findings, got %d", len(remaining))
	}
}

func TestFindingQueue_CriticalBeforeHigh(t *testing.T) {
	q := newFindingQueue()
	const sid = "session-order"

	// Enqueue HIGH first, then CRITICAL.
	q.Enqueue(sid, newTestViolation("high-pat", "tgt-h", security.SeverityHigh))
	q.Enqueue(sid, newTestViolation("crit-pat", "tgt-c", security.SeverityCritical))

	out := q.Dequeue(sid, 2)
	if len(out) != 2 {
		t.Fatalf("expected 2, got %d", len(out))
	}
	if out[0].Severity != security.SeverityCritical {
		t.Errorf("expected CRITICAL first, got %s", out[0].Severity)
	}
	if out[1].Severity != security.SeverityHigh {
		t.Errorf("expected HIGH second, got %s", out[1].Severity)
	}
}

func TestFindingQueue_MediumSkipped(t *testing.T) {
	q := newFindingQueue()
	const sid = "session-medium"

	added := q.Enqueue(sid, newTestViolation("med-pat", "tgt", security.SeverityMedium))
	if added {
		t.Error("expected MEDIUM finding to be rejected from queue")
	}
	if out := q.Dequeue(sid, 5); len(out) != 0 {
		t.Errorf("expected empty queue for MEDIUM finding, got %d", len(out))
	}
}

func TestFindingQueue_DuplicateEnqueueSkipped(t *testing.T) {
	q := newFindingQueue()
	const sid = "session-dup"

	v := newTestViolation("dup-pat", "tgt-dup", security.SeverityHigh)
	q.Enqueue(sid, v)

	added := q.Enqueue(sid, v) // same PatternID+Target
	if added {
		t.Error("expected duplicate enqueue to be rejected")
	}

	out := q.Dequeue(sid, 10)
	if len(out) != 1 {
		t.Errorf("expected exactly 1 finding despite double enqueue, got %d", len(out))
	}
}

func TestFindingQueue_EmptySessionID(t *testing.T) {
	q := newFindingQueue()
	v := newTestViolation("p", "t", security.SeverityCritical)

	if q.Enqueue("", v) {
		t.Error("empty sessionID should return false")
	}
	if got := q.Dequeue("", 5); len(got) != 0 {
		t.Errorf("empty sessionID dequeue should return nil, got %d", len(got))
	}
}

func TestFindingQueue_EpisodedDedup(t *testing.T) {
	q := newFindingQueue()
	const sid = "session-ep"

	// Not yet episoded.
	if q.IsEpisoded(sid, "pat", "tgt") {
		t.Error("expected IsEpisoded=false before MarkEpisoded")
	}

	q.MarkEpisoded(sid, "pat", "tgt")

	if !q.IsEpisoded(sid, "pat", "tgt") {
		t.Error("expected IsEpisoded=true after MarkEpisoded")
	}
	// Different target is independent.
	if q.IsEpisoded(sid, "pat", "other-tgt") {
		t.Error("different target should not be episoded")
	}
	// Different session is independent.
	if q.IsEpisoded("other-session", "pat", "tgt") {
		t.Error("different session should not be episoded")
	}
}

func TestFindingQueue_CheckAndMarkEpisoded_AtomicSemantics(t *testing.T) {
	q := newFindingQueue()
	const sid = "session-atomic"

	// First call: not yet marked → returns true (proceed with episode write).
	if !q.CheckAndMarkEpisoded(sid, "pat", "tgt") {
		t.Error("first CheckAndMarkEpisoded should return true")
	}
	// Second call: already marked → returns false (skip write).
	if q.CheckAndMarkEpisoded(sid, "pat", "tgt") {
		t.Error("second CheckAndMarkEpisoded should return false")
	}
	// Different key is independent.
	if !q.CheckAndMarkEpisoded(sid, "pat", "other-tgt") {
		t.Error("different target should return true")
	}
	// Empty sessionID always returns true (dedup disabled for test/stdio path).
	if !q.CheckAndMarkEpisoded("", "pat", "tgt") {
		t.Error("empty sessionID should always return true")
	}
}

func TestFindingQueue_Clear(t *testing.T) {
	q := newFindingQueue()
	const sid = "session-clear"

	q.Enqueue(sid, newTestViolation("p", "t", security.SeverityCritical))
	q.MarkEpisoded(sid, "p", "t")

	q.Clear(sid)

	if got := q.Dequeue(sid, 10); len(got) != 0 {
		t.Errorf("expected empty queue after Clear, got %d", len(got))
	}
	if q.IsEpisoded(sid, "p", "t") {
		t.Error("expected episoded cleared after Clear")
	}
}

func TestInjectPendingFindings_NilSafe(t *testing.T) {
	// All nil-safe paths must not panic.
	injectPendingFindings(nil, nil, "")
	injectPendingFindings(nil, newFindingQueue(), "sess")
	result := &mcp.CallToolResult{}
	injectPendingFindings(result, nil, "sess")
	injectPendingFindings(result, newFindingQueue(), "") // empty sessionID
}

func TestInjectPendingFindings_AppendsBlock(t *testing.T) {
	q := newFindingQueue()
	const sid = "session-inject"

	q.Enqueue(sid, newTestViolation("inject-pat", "handler", security.SeverityCritical))

	result := mcp.NewToolResultText(`{"status":"ok"}`)
	injectPendingFindings(result, q, sid)

	if len(result.Content) < 2 {
		t.Fatalf("expected a second content block to be appended, got %d blocks", len(result.Content))
	}
	// The appended block must mention the pattern.
	appended := result.Content[len(result.Content)-1]
	txt, ok := appended.(mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", appended)
	}
	if !strings.Contains(txt.Text, "inject-pat") {
		t.Errorf("appended block missing pattern_id; got: %s", txt.Text)
	}
	if !strings.Contains(txt.Text, "Pending Security Findings") {
		t.Errorf("appended block missing header; got: %s", txt.Text)
	}

	// Finding was delivered — queue is now empty.
	if got := q.Dequeue(sid, 5); len(got) != 0 {
		t.Errorf("expected empty queue after inject, got %d", len(got))
	}
}

func TestInjectPendingFindings_EmptyQueue_NoAppend(t *testing.T) {
	q := newFindingQueue()
	result := mcp.NewToolResultText(`{"status":"ok"}`)
	initialLen := len(result.Content)

	injectPendingFindings(result, q, "no-findings-session")

	if len(result.Content) != initialLen {
		t.Errorf("expected no new content block when queue is empty")
	}
}

func TestInjectPendingFindings_ConfidencePreserved(t *testing.T) {
	q := newFindingQueue()
	const sid = "session-conf"

	v := newTestViolation("conf-pat", "handler", security.SeverityCritical)
	v.Confidence = security.ConfidenceHigh
	v.ConfidenceReason = "import-path-match"
	q.Enqueue(sid, v)

	result := mcp.NewToolResultText(`{"status":"ok"}`)
	injectPendingFindings(result, q, sid)

	if len(result.Content) < 2 {
		t.Fatalf("expected injected content block, got %d blocks", len(result.Content))
	}
	appended := result.Content[len(result.Content)-1]
	txt, ok := appended.(mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", appended)
	}
	if !strings.Contains(txt.Text, "HIGH") {
		t.Errorf("injected block missing confidence HIGH; got: %s", txt.Text)
	}
	if !strings.Contains(txt.Text, "import-path-match") {
		t.Errorf("injected block missing confidence_reason; got: %s", txt.Text)
	}
}

func TestFindingQueue_MultiSession_Isolation(t *testing.T) {
	q := newFindingQueue()

	q.Enqueue("sess-A", newTestViolation("pat-a", "tgt", security.SeverityCritical))
	q.Enqueue("sess-B", newTestViolation("pat-b", "tgt", security.SeverityHigh))

	outA := q.Dequeue("sess-A", 10)
	outB := q.Dequeue("sess-B", 10)

	if len(outA) != 1 || outA[0].PatternID != "pat-a" {
		t.Errorf("sess-A: unexpected result %+v", outA)
	}
	if len(outB) != 1 || outB[0].PatternID != "pat-b" {
		t.Errorf("sess-B: unexpected result %+v", outB)
	}
}
