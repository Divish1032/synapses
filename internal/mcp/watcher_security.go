package mcp

// watcher_security.go — Sprint 27.8: File watcher security integration.
//
// onWatcherFileChanged is the security scan callback registered on the file watcher
// via SetChangeSource → SetSecurityScanCb. It runs asynchronously (via watcher.trackGo)
// after every successful file reparse and performs three per-file checks:
//
//   - CheckFile: framework patterns, auth middleware, rate limiting, etc.
//   - CheckNorms: structural norms observed from the graph (e.g., "8/8 handlers have auth")
//   - CheckImports: unknown package detection (slopsquatting)
//
// NOTE: CheckProject (cross-transport auth, layer violations) is deliberately NOT called
// here — it is O(N×E) and unsuitable for per-keystroke execution. It fires only in
// handleVerifyImplementation.
//
// For each CRITICAL or HIGH finding that has not been delivered to this session yet:
//  1. It is enqueued in the in-memory findingQueue for piggyback delivery on the next
//     tool response (Tier 2 universal mechanism — works with any editor/agent).
//  2. An episode of type "watcher_security_finding" is written to the store for
//     cross-session persistence. The next session_init call surfaces these episodes so
//     agents starting a new session are immediately aware of outstanding issues.
//
// Both operations are gated by findingQueue.CheckAndMarkEpisoded to ensure
// exactly-once delivery per session per PatternID+Target pair.

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/SynapsesOS/synapses/internal/security"
	"github.com/SynapsesOS/synapses/internal/store"
)

// onWatcherFileChanged is the security scan callback. It is called by the file
// watcher after each successful reparse (asynchronously, non-blocking).
// filePath is the absolute path of the reparsed file.
func (s *Server) onWatcherFileChanged(filePath string) {
	if s.patternEngine == nil || s.graph == nil {
		return
	}

	// Read the file so content-based checks (hardcoded secrets) can run.
	// A read error is non-fatal — we pass nil content and skip content checks.
	content, _ := os.ReadFile(filePath)

	// Collect per-file security violations from all applicable checks.
	var findings []security.Violation
	findings = append(findings, s.patternEngine.CheckFile(s.graph, filePath, content)...)
	findings = append(findings, s.patternEngine.CheckNorms(s.graph, filePath)...)
	findings = append(findings, s.patternEngine.CheckImports(s.graph, filePath)...)

	if len(findings) == 0 {
		return
	}

	// Resolve the active session for in-memory queue delivery.
	// Empty sessID means no agent has called a tool yet this invocation;
	// we still persist the episode for cross-session delivery.
	sessID := s.getLastSynapseSessionID()

	for _, f := range findings {
		if f.Severity != security.SeverityCritical && f.Severity != security.SeverityHigh {
			continue
		}
		// Atomic check-and-mark: skip if already delivered/episoded this session.
		if !s.findingQueue.CheckAndMarkEpisoded(sessID, f.PatternID, f.Target) {
			continue
		}

		// 1. Enqueue for same-session piggyback delivery (Tier 2 universal).
		s.findingQueue.Enqueue(sessID, f)

		// 2. Write episode for cross-session persistence at next session_init.
		s.persistWatcherFindingEpisode(f, filePath)
	}
}

// persistWatcherFindingEpisode writes a store.Episode for a watcher-detected security
// finding so it surfaces at the next session_init even if the current session ends
// before the finding is delivered.
func (s *Server) persistWatcherFindingEpisode(f security.Violation, filePath string) {
	if s.store == nil {
		return
	}
	relPath := filePath
	if root := s.graph.Root(); root != "" && strings.HasPrefix(filePath, root) {
		relPath = strings.TrimPrefix(filePath, root+"/")
	}

	tags := `["auto","watcher","security"]`
	if conf := string(f.Confidence); conf != "" {
		tags = fmt.Sprintf(`["auto","watcher","security","confidence:%s"]`, conf)
	}
	ep := store.Episode{
		EpisodeType: "watcher_security_finding",
		Outcome:     "failure",
		Trigger:     fmt.Sprintf("file watcher detected security issue after editing %s", relPath),
		Decision:    fmt.Sprintf("%s: %s", f.PatternName, f.Target),
		Rationale:   f.Message,
		Tags:        tags,
		Importance:  0.85,
	}
	if _, err := s.store.RememberEpisode(ep); err != nil {
		log.Printf("mcp/watcher: persist security finding episode: %v", err)
	}
}

// watcherSecurityFindingHint is the wire format for watcher findings surfaced at session_init.
type watcherSecurityFindingHint struct {
	PatternName string `json:"pattern_name"`
	Target      string `json:"target"`
	Message     string `json:"message"`
	At          int64  `json:"detected_at"` // Unix seconds
	Confidence  string `json:"confidence,omitempty"`
}

// getWatcherSecurityFindings queries the store for recent watcher-detected security
// findings (last 24 h) and returns them formatted for session_init injection.
// Returns nil if the store is unavailable or no recent findings exist.
func (s *Server) getWatcherSecurityFindings() []watcherSecurityFindingHint {
	if s.store == nil {
		return nil
	}
	episodes, err := s.store.GetEpisodes("", "", "watcher_security_finding", nil, 10, 1)
	if err != nil || len(episodes) == 0 {
		return nil
	}

	out := make([]watcherSecurityFindingHint, 0, len(episodes))
	for _, ep := range episodes {
		// Decision is stored as "PatternName: Target" by persistWatcherFindingEpisode.
		// Split to populate both fields separately so the agent sees structured data.
		patternName, target := ep.Decision, ""
		if idx := strings.Index(ep.Decision, ": "); idx >= 0 {
			patternName = ep.Decision[:idx]
			target = ep.Decision[idx+2:]
		}
		// Extract confidence from tags: ["auto","watcher","security","confidence:HIGH"]
		// Tags is a JSON array; scan for the "confidence:<LEVEL>" entry without a
		// full JSON parse to avoid an encoding/json import in this file.
		confidence := ""
		if idx := strings.Index(ep.Tags, "confidence:"); idx >= 0 {
			rest := ep.Tags[idx+len("confidence:"):]
			if end := strings.IndexAny(rest, `"]`); end >= 0 {
				confidence = rest[:end]
			}
		}
		out = append(out, watcherSecurityFindingHint{
			PatternName: patternName,
			Target:      target,
			Message:     ep.Rationale,
			At:          ep.CreatedAt,
			Confidence:  confidence,
		})
	}
	return out
}
