package mcp

// goal_reinforcer.go — Sprint 25.6: Goal and convention reinforcement.
//
// Every N tool responses (configurable via session.reinforcement_interval, default 10),
// appends a compact reminder to the response: the current in-progress task goal (1 line)
// + top 3 active conventions. Prevents mid-session drift where the task and project rules
// decay into the "middle of context" and stop influencing the agent's behaviour.
//
// Design: server-side Tier 1 — no agent initiative required. Fires automatically on the
// Nth response in a session regardless of which tool the agent called.

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// goalReinforcerEntry holds the per-session response count and a creation timestamp
// used for GC. The count is incremented on every tool response (excluding session_init
// and end_session, which are meta tools that don't benefit from reinforcement).
type goalReinforcerEntry struct {
	responseCount int
	created       time.Time
}

// goalReinforcer tracks per-session response counts and fires goal+convention
// reminders every N responses. interval == 0 disables reinforcement entirely.
//
// Thread-safety: all public methods acquire the mutex. GC runs lazily on record()
// to avoid spawning a background goroutine.
type goalReinforcer struct {
	mu       sync.Mutex
	entries  map[string]*goalReinforcerEntry // sessionID → entry
	interval int                             // fire every N responses; 0 = disabled
	lastGC   time.Time
}

const (
	goalReinforcerGCInterval = 30 * time.Minute
	goalReinforcerMaxAge     = 2 * time.Hour
)

// newGoalReinforcer constructs a goalReinforcer with the given interval.
// interval == 0 disables reinforcement (all calls to recordAndShouldFire return false).
func newGoalReinforcer(interval int) *goalReinforcer {
	return &goalReinforcer{
		entries:  make(map[string]*goalReinforcerEntry),
		interval: interval,
		lastGC:   time.Now(),
	}
}

// recordAndShouldFire increments the response counter for sessionID and returns
// true when the counter is a non-zero multiple of the configured interval.
// Returns false immediately when interval == 0 (disabled) or sessionID == "".
func (r *goalReinforcer) recordAndShouldFire(sessionID string) bool {
	if r.interval <= 0 || sessionID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.entries[sessionID]
	if !ok {
		e = &goalReinforcerEntry{created: time.Now()}
		r.entries[sessionID] = e
	}
	e.responseCount++

	// Periodic GC: remove entries older than 2 hours to prevent unbounded growth
	// from sessions that disconnect without calling end_session.
	if time.Since(r.lastGC) > goalReinforcerGCInterval {
		r.lastGC = time.Now()
		cutoff := time.Now().Add(-goalReinforcerMaxAge)
		for sid, entry := range r.entries {
			if entry.created.Before(cutoff) {
				delete(r.entries, sid)
			}
		}
	}

	return e.responseCount%r.interval == 0
}

// clear removes the session entry, resetting the counter. Called from end_session
// so that a resumed session starts fresh rather than inheriting a stale count.
func (r *goalReinforcer) clear(sessionID string) {
	if sessionID == "" {
		return
	}
	r.mu.Lock()
	delete(r.entries, sessionID)
	r.mu.Unlock()
}

// responseCountFor returns the current response count for a session.
// Used in tests to inspect state without triggering a fire. 0 means no responses recorded.
func (r *goalReinforcer) responseCountFor(sessionID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[sessionID]; ok {
		return e.responseCount
	}
	return 0
}

// injectGoalReinforcement appends a compact goal+convention reminder to result.
// It queries the store for the first in-progress task (the "current goal") and
// takes the top 3 conventions from the project's formatter conventions + configured
// agent rules. The reminder adds ~50 tokens — lightweight by design.
//
// Errors are silently swallowed: reinforcement is advisory and must never block
// or corrupt the primary tool response.
func (s *Server) injectGoalReinforcement(result *mcp.CallToolResult, sessionID string) {
	if result == nil || len(result.Content) == 0 {
		return
	}

	goal := s.currentTaskGoal(sessionID)
	conventions := s.topConventions(3)

	// Nothing meaningful to inject — skip to avoid noise.
	if goal == "" && len(conventions) == 0 {
		return
	}

	reminder := buildReinforcementBlock(goal, conventions)
	if reminder == "" {
		return
	}

	result.Content = append(result.Content, mcp.NewTextContent(reminder))
}

// currentTaskGoal returns the title of the first in-progress task for the project,
// or "" if no in-progress task exists or the store is unavailable.
// It does not filter by agent because goal reinforcement is project-scoped.
func (s *Server) currentTaskGoal(_ string) string {
	if s.store == nil {
		return ""
	}
	tasks, err := s.store.GetPendingTasks("", "")
	if err != nil {
		return ""
	}
	for _, t := range tasks {
		if t.Status == "in_progress" {
			title := strings.TrimSpace(t.Title)
			// Normalize: use only the first line of multi-line titles.
			if nl := strings.IndexByte(title, '\n'); nl > 0 {
				title = strings.TrimRight(title[:nl], "\r")
			}
			// Cap at 100 runes so the reminder stays compact.
			if rs := []rune(title); len(rs) > 100 {
				title = string(rs[:100]) + "…"
			}
			return title
		}
	}
	return ""
}

// topConventions returns up to n conventions for the project.
// Priority: formatter conventions first, then cross-session learned conventions
// (Sprint 29.2), then configured agent-type rules.
func (s *Server) topConventions(n int) []string {
	var convs []string

	// Formatter conventions (cached after first call — free).
	convs = append(convs, s.cachedFormatterConventions()...)
	if len(convs) >= n {
		return convs[:n]
	}

	// Cross-session learned conventions (Sprint 29.2).
	// GetProjectConventions is a fast indexed SQL read; at most 1 slot here.
	if s.store != nil && s.projectID != "" {
		if learned, err := s.store.GetProjectConventions(s.projectID, 0.6); err == nil {
			for _, c := range learned {
				if len(convs) >= n || c.Text == "" {
					break
				}
				convs = append(convs, c.Text)
			}
		}
	}
	if len(convs) >= n {
		return convs[:n]
	}

	// Fill remaining slots from configured agent-type rules.
	s.rulesMu.RLock()
	rules := s.config.Rules
	s.rulesMu.RUnlock()
	for _, r := range rules {
		if len(convs) >= n {
			break
		}
		if !r.IsAgentRule() {
			continue
		}
		desc := strings.TrimSpace(r.Description)
		if desc == "" {
			continue
		}
		// Same 120-rune cap as session_init conventions.
		if rs := []rune(desc); len(rs) > 120 {
			desc = string(rs[:120]) + "…"
		}
		convs = append(convs, desc)
	}

	return convs
}

// reinforcementPayload is the JSON shape of the reminder block.
type reinforcementPayload struct {
	TaskGoal    string   `json:"task_goal,omitempty"`
	Conventions []string `json:"conventions,omitempty"`
}

// buildReinforcementBlock serialises the goal+conventions into a compact text
// annotation. Returns "" when both goal and conventions are empty.
func buildReinforcementBlock(goal string, conventions []string) string {
	if goal == "" && len(conventions) == 0 {
		return ""
	}
	p := reinforcementPayload{
		TaskGoal:    goal,
		Conventions: conventions,
	}
	b, err := json.Marshal(p)
	if err != nil {
		// Fallback to plain text if marshal fails (should never happen).
		var parts []string
		if goal != "" {
			parts = append(parts, fmt.Sprintf("Current goal: %s", goal))
		}
		for _, c := range conventions {
			parts = append(parts, c)
		}
		return "\n📌 Reminder: " + strings.Join(parts, " | ")
	}
	return "\n📌 Reminder: " + string(b)
}
