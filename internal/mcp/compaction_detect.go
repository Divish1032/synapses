package mcp

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	mcp "github.com/mark3labs/mcp-go/mcp"
)

// compactCooldown suppresses duplicate recovery injections within a session.
// After one injection, no more are fired for this duration.
// 30 minutes allows a second post-compaction recovery if the agent works
// through one compaction cycle before hitting another.
const compactCooldown = 30 * time.Minute

// compactDetectState holds per-session compaction detection bookkeeping.
// Stored in a sync.Map on Server (key: Synapses session ID).
type compactDetectState struct {
	mu         sync.Mutex
	injectedAt time.Time          // zero-value means never injected
	explored   map[string]struct{} // entities/queries seen this session (Signal 2)
}

// shouldInject returns true when recovery should be injected for this session
// (i.e. the cooldown has expired or injection has never happened).
// Use tryMarkInjected for concurrent callers to avoid check-then-act races.
func (c *compactDetectState) shouldInject() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.injectedAt.IsZero() || time.Since(c.injectedAt) >= compactCooldown
}

// markInjected records the current time as the last injection point.
func (c *compactDetectState) markInjected() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.injectedAt = time.Now()
}

// tryMarkInjected atomically checks the cooldown and records the injection
// in a single lock acquisition. Returns true if injection should proceed,
// false if still within cooldown. Use this in concurrent call sites (e.g.
// ledgerWrapped) to prevent duplicate recovery blocks when multiple tool
// calls arrive simultaneously.
func (c *compactDetectState) tryMarkInjected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.injectedAt.IsZero() && time.Since(c.injectedAt) < compactCooldown {
		return false
	}
	c.injectedAt = time.Now()
	return true
}

// unmarkInjected resets the injection timestamp so the next call to shouldInject
// or tryMarkInjected will return true. Used when tryMarkInjected succeeded but
// the recovery packet could not be built (empty session) — we shouldn't consume
// the injection slot for a no-op.
func (c *compactDetectState) unmarkInjected() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.injectedAt = time.Time{}
}

// isReExplored returns true when entity was already seen in this session,
// indicating the agent may have lost context and is re-exploring.
// An empty entity always returns false.
func (c *compactDetectState) isReExplored(entity string) bool {
	if entity == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.explored[entity]
	return ok
}

// markExplored records entity as having been seen this session.
// No-op for empty strings.
func (c *compactDetectState) markExplored(entity string) {
	if entity == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.explored == nil {
		c.explored = make(map[string]struct{})
	}
	c.explored[entity] = struct{}{}
}

// getCompactDetectState returns the compaction detection state for sessionID,
// creating a new one if none exists. Thread-safe via sync.Map.LoadOrStore.
func (s *Server) getCompactDetectState(sessionID string) *compactDetectState {
	v, _ := s.compactDetect.LoadOrStore(sessionID, &compactDetectState{
		explored: make(map[string]struct{}),
	})
	return v.(*compactDetectState)
}

// injectCompactionRecovery appends the recovery packet as a new content block
// to result. Mirrors the injectAlerts pattern: the original response bytes are
// preserved and the recovery is tacked on as a labeled text block.
//
// signal identifies which detection heuristic fired ("re-init" | "re-exploration").
// The label helps agents and developers understand why the packet was included.
func injectCompactionRecovery(result *mcp.CallToolResult, recovery map[string]interface{}, signal string) {
	if result == nil || len(recovery) == 0 || len(result.Content) == 0 {
		return
	}
	recoveryJSON, err := json.Marshal(recovery)
	if err != nil {
		return
	}
	text := fmt.Sprintf("\n[Compaction Recovery — signal: %s]\n%s", signal, string(recoveryJSON))
	result.Content = append(result.Content, mcp.NewTextContent(text))
}

// extractQueryEntity returns the primary entity name or query string for the
// given tool call. Only meaningful for "get_context" and "search"; returns ""
// for all other tools so callers can skip compaction checks cheaply.
func extractQueryEntity(toolName string, args map[string]interface{}) string {
	switch toolName {
	case "get_context":
		entity, _ := args["entity"].(string)
		return entity
	case "search":
		query, _ := args["query"].(string)
		return query
	}
	return ""
}
