package mcp

// finding_queue.go — Sprint 27.10: Finding queue and piggyback delivery.
//
// findingQueue is an in-memory, per-session store for CRITICAL and HIGH security
// findings detected by background components (file watcher, drift analysis).
// Any component can Enqueue a finding; the server middleware dequeues and
// piggybacks up to 3 findings onto the next non-error tool response.
//
// Findings are delivered exactly once (dequeue removes them from the queue).
// CRITICAL findings surface before HIGH. Within the same severity, earlier-
// enqueued findings come first.
//
// The same struct also tracks which PatternID+Target pairs have had an episode
// recorded this session (the "episoded" set), used by handleVerifyImplementation
// to avoid writing duplicate episodes for persistent findings on repeated calls.
//
// Thread-safety: all public methods acquire the mutex. GC runs lazily on Enqueue.

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/SynapsesOS/synapses/internal/security"
)

const (
	findingQueueGCInterval = time.Hour
	findingQueueMaxAge     = 24 * time.Hour
	// maxPiggybackFindings is the maximum number of findings injected per tool response.
	maxPiggybackFindings = 3
)

// queuedFinding wraps a security.Violation with its enqueue timestamp.
type queuedFinding struct {
	V          security.Violation
	EnqueuedAt time.Time
}

// findingQueue holds queued findings and episoded keys per Synapses session UUID.
type findingQueue struct {
	mu       sync.Mutex
	queue    map[string][]queuedFinding  // sessionID → pending findings
	episoded map[string]map[string]bool  // sessionID → "patternID:target" already episoded
	lastGC   time.Time
}

func newFindingQueue() *findingQueue {
	return &findingQueue{
		queue:    make(map[string][]queuedFinding),
		episoded: make(map[string]map[string]bool),
		lastGC:   time.Now(),
	}
}

// Enqueue adds a finding to the session queue.
// Only CRITICAL and HIGH severity findings are queued; MEDIUM is informational
// and surfaced directly in the validate response.
// Duplicate findings (same PatternID+Target already in queue) are silently skipped.
// Returns true if the finding was added to the queue.
func (q *findingQueue) Enqueue(sessionID string, v security.Violation) bool {
	if sessionID == "" {
		return false
	}
	if v.Severity != security.SeverityCritical && v.Severity != security.SeverityHigh {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.gcLocked()

	key := v.PatternID + ":" + v.Target
	for _, qf := range q.queue[sessionID] {
		if qf.V.PatternID+":"+qf.V.Target == key {
			return false // already queued
		}
	}
	q.queue[sessionID] = append(q.queue[sessionID], queuedFinding{
		V:          v,
		EnqueuedAt: time.Now(),
	})
	return true
}

// Dequeue removes and returns up to maxN findings for the session.
// CRITICAL findings are returned before HIGH; within the same severity,
// earlier-enqueued findings come first. Returned findings are removed from the queue.
func (q *findingQueue) Dequeue(sessionID string, maxN int) []security.Violation {
	if sessionID == "" || maxN <= 0 {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	pending := q.queue[sessionID]
	if len(pending) == 0 {
		return nil
	}

	// Sort a copy: CRITICAL (0) before HIGH (1), then by enqueue time.
	sorted := make([]queuedFinding, len(pending))
	copy(sorted, pending)
	sort.SliceStable(sorted, func(i, j int) bool {
		oi := findingSeverityOrder(sorted[i].V.Severity)
		oj := findingSeverityOrder(sorted[j].V.Severity)
		if oi != oj {
			return oi < oj
		}
		return sorted[i].EnqueuedAt.Before(sorted[j].EnqueuedAt)
	})

	take := maxN
	if take > len(sorted) {
		take = len(sorted)
	}

	out := make([]security.Violation, take)
	for i := range out {
		out[i] = sorted[i].V
	}

	// Build a set of PatternID:Target keys to remove from the original queue.
	taken := make(map[string]bool, take)
	for _, f := range out {
		taken[f.PatternID+":"+f.Target] = true
	}
	remaining := pending[:0]
	for _, qf := range pending {
		if !taken[qf.V.PatternID+":"+qf.V.Target] {
			remaining = append(remaining, qf)
		}
	}
	if len(remaining) == 0 {
		delete(q.queue, sessionID)
	} else {
		q.queue[sessionID] = remaining
	}
	return out
}

// CheckAndMarkEpisoded atomically checks whether an episode for patternID+target
// has already been written this session, and marks it if not.
//
// Returns true if the caller should proceed with writing the episode (the key
// was NOT previously seen). Returns false if already episoded (caller skips write).
//
// The atomic check+mark prevents TOCTOU races when concurrent
// verify_implementation calls process the same persistent finding simultaneously.
//
// When sessionID is "" (test/stdio path), always returns true so episode
// behaviour is unchanged from pre-27.10 code.
func (q *findingQueue) CheckAndMarkEpisoded(sessionID, patternID, target string) bool {
	if sessionID == "" {
		return true
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	m := q.episoded[sessionID]
	if m == nil {
		m = make(map[string]bool)
		q.episoded[sessionID] = m
	}
	key := patternID + ":" + target
	if m[key] {
		return false
	}
	m[key] = true
	return true
}

// IsEpisoded reports whether an episode for patternID+target was already written
// this session. Prefer CheckAndMarkEpisoded when both a check and a mark are needed.
func (q *findingQueue) IsEpisoded(sessionID, patternID, target string) bool {
	if sessionID == "" {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.episoded[sessionID][patternID+":"+target]
}

// MarkEpisoded records that an episode was written for patternID+target this session.
// Prefer CheckAndMarkEpisoded when both a check and a mark are needed atomically.
func (q *findingQueue) MarkEpisoded(sessionID, patternID, target string) {
	if sessionID == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	m := q.episoded[sessionID]
	if m == nil {
		m = make(map[string]bool)
		q.episoded[sessionID] = m
	}
	m[patternID+":"+target] = true
}

// Clear removes all queued findings and episoded markers for a session.
// Called when a session ends (end_session, goalReinforcer.clear path).
func (q *findingQueue) Clear(sessionID string) {
	if sessionID == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.queue, sessionID)
	delete(q.episoded, sessionID)
}

// gcLocked removes stale session entries. Must be called with q.mu held.
func (q *findingQueue) gcLocked() {
	if time.Since(q.lastGC) < findingQueueGCInterval {
		return
	}
	cutoff := time.Now().Add(-findingQueueMaxAge)
	for sid, findings := range q.queue {
		// Remove session if all queued findings are older than the cutoff.
		allOld := true
		for _, f := range findings {
			if f.EnqueuedAt.After(cutoff) {
				allOld = false
				break
			}
		}
		if allOld {
			delete(q.queue, sid)
			delete(q.episoded, sid)
		}
	}
	q.lastGC = time.Now()
}

// findingSeverityOrder maps severity to a sort priority (lower = higher priority).
func findingSeverityOrder(s security.Severity) int {
	switch s {
	case security.SeverityCritical:
		return 0
	case security.SeverityHigh:
		return 1
	default:
		return 2
	}
}

// piggybackFinding is the wire format for findings injected into tool responses.
type piggybackFinding struct {
	PatternID string `json:"pattern_id"`
	Severity  string `json:"severity"`
	Action    string `json:"action"`
	File      string `json:"file"`
	Target    string `json:"target"`
	Message   string `json:"message"`
	Evidence  string `json:"evidence,omitempty"`
}

// injectPendingFindings dequeues up to maxPiggybackFindings pending security
// findings for the session and appends them to the MCP result as a structured
// text content block. CRITICAL findings surface first.
//
// No-op when result is nil, the queue is nil, sessionID is empty, or there are
// no queued findings for this session. Follows the same append pattern as
// injectAlerts and injectCompactionRecovery — never re-serialises existing content.
func injectPendingFindings(result *mcp.CallToolResult, q *findingQueue, sessionID string) {
	if result == nil || q == nil || sessionID == "" {
		return
	}
	findings := q.Dequeue(sessionID, maxPiggybackFindings)
	if len(findings) == 0 {
		return
	}

	pf := make([]piggybackFinding, len(findings))
	for i, f := range findings {
		pf[i] = piggybackFinding{
			PatternID: f.PatternID,
			Severity:  string(f.Severity),
			Action:    f.Action,
			File:      f.File,
			Target:    f.Target,
			Message:   f.Message,
			Evidence:  f.Evidence,
		}
	}

	payload := map[string]interface{}{
		"pending_findings": pf,
		"_note": fmt.Sprintf(
			"%d security finding(s) detected by background analysis. Address before continuing.",
			len(findings),
		),
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return // silently skip on marshal error
	}
	result.Content = append(result.Content, mcp.NewTextContent("\n[Pending Security Findings]\n"+string(b)))
}
