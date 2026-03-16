// Package archivist synthesizes agent session transcripts into persistent
// memory entries and code annotations.
//
// This is a Tier 2 (cold standby) component — the archivist llama-server
// process is started lazily on first Memorize() call and shares the same
// LLMClient interface as all other tiers.
package archivist

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/SynapsesOS/synapses/internal/brain/llm"
)

// Archivist synthesizes session transcripts into persistent memories.
type Archivist struct {
	llm     llm.LLMClient
	timeout time.Duration
}

// New creates an Archivist backed by the given LLM client.
func New(client llm.LLMClient, timeout time.Duration) *Archivist {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Archivist{llm: client, timeout: timeout}
}

// SessionEvent is a single tool call from an agent session.
type SessionEvent struct {
	Tool   string `json:"tool"`
	Entity string `json:"entity,omitempty"`
	Result string `json:"result_summary,omitempty"`
}

// MemorizeRequest is the input for session memory synthesis.
type MemorizeRequest struct {
	SessionEvents  []SessionEvent `json:"session_events"`
	ExistingMemory []string       `json:"existing_memory,omitempty"`
}

// Memory is a synthesized persistent memory entry.
type Memory struct {
	Key      string   `json:"key"`
	Content  string   `json:"content"`
	Entities []string `json:"entities,omitempty"`
}

// Annotation is a synthesized note for a code entity.
type Annotation struct {
	Node string `json:"node"`
	Note string `json:"note"`
}

// MemorizeResponse contains synthesized memories and annotations.
type MemorizeResponse struct {
	NewMemories []Memory     `json:"new_memories"`
	Annotations []Annotation `json:"annotations"`
}

// Memorize synthesizes a session transcript into persistent memory entries and annotations.
func (a *Archivist) Memorize(ctx context.Context, req MemorizeRequest) (MemorizeResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	prompt := buildMemorizePrompt(req)
	raw, err := a.llm.Generate(ctx, prompt)
	if err != nil {
		return MemorizeResponse{}, fmt.Errorf("archivist: %w", err)
	}

	var resp MemorizeResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &resp); err != nil {
		// Return empty on parse failure — non-fatal, agent continues without new memories.
		return MemorizeResponse{}, nil
	}
	return resp, nil
}

func buildMemorizePrompt(req MemorizeRequest) string {
	eventsJSON, _ := json.Marshal(req.SessionEvents)
	memoryJSON, _ := json.Marshal(req.ExistingMemory)
	return fmt.Sprintf(`Analyze this agent session and extract what is worth remembering long-term.
Session events: %s
Existing memory: %s

Rules:
- Only save architectural discoveries, non-obvious relationships, or decisions that will matter in future sessions.
- If the session is trivial (a single lookup, no new discoveries, or only routine tool calls), return empty arrays.
- Do not duplicate entries already present in existing_memory.
- Keep each memory concise (one sentence for content).
- Only annotate entities that were meaningfully analyzed, not just mentioned.

Return JSON only: {"new_memories":[{"key":"short_snake_case_key","content":"what to remember","entities":["EntityName"]}],"annotations":[{"node":"EntityName","note":"observation"}]}
Return {"new_memories":[],"annotations":[]} if nothing is worth saving.`,
		string(eventsJSON), string(memoryJSON))
}
