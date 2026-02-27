package peer

import (
	"crypto/rand"
	"fmt"
)

// IntentMessage is the payload sent to peers when local scope is claimed.
type IntentMessage struct {
	TraceID    string `json:"trace_id"`    // UUID v4; used for dedup by receiver
	AgentID    string `json:"agent_id"`
	IntentType string `json:"intent_type"` // "claim_work" | "release_work"
	Scope      string `json:"scope"`
	ScopeType  string `json:"scope_type"`
	Timestamp  int64  `json:"timestamp"` // Unix seconds
}

// generateStableID returns a random UUID v4 string.
// Used for trace IDs in intent messages and stable node IDs.
func generateStableID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant bits
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
