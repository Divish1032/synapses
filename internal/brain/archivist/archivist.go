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
	Entities []string `json:"-"` // populated by custom parsing, not direct unmarshal
}

// rawMemory is the flexible intermediate type for JSON unmarshaling.
// The LLM may return entities as either a comma-separated string
// (requested to avoid nested arrays that Qwen3.5 frequently malforms)
// or as a JSON array (if the model ignores the instruction).
type rawMemory struct {
	Key      string          `json:"key"`
	Content  string          `json:"content"`
	Entities json.RawMessage `json:"entities,omitempty"`
}

func parseEntities(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	// Try array first: ["A","B"]
	var arr []string
	if json.Unmarshal(raw, &arr) == nil {
		return arr
	}
	// Try string: "A,B"
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		parts := strings.Split(s, ",")
		result := make([]string, 0, len(parts))
		for _, p := range parts {
			if t := strings.TrimSpace(p); t != "" {
				result = append(result, t)
			}
		}
		return result
	}
	return nil
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

	// Strip markdown fences before parsing — some Ollama versions or fallback
	// models emit ```json ... ``` even when format:"json" is set. Without this,
	// the unmarshal fails silently and the circuit breaker never trips.
	raw = llm.ExtractJSON(strings.TrimSpace(raw))
	raw = llm.RepairJSON(raw)

	resp, parseErr := parseMemorizeResponse(raw)
	if parseErr != nil {
		return MemorizeResponse{}, fmt.Errorf("parse memorize response: %w", parseErr)
	}
	return resp, nil
}

// parseMemorizeResponse handles the flexible entities field (string or array).
func parseMemorizeResponse(raw string) (MemorizeResponse, error) {
	var intermediate struct {
		NewMemories []rawMemory  `json:"new_memories"`
		Annotations []Annotation `json:"annotations"`
	}
	if err := json.Unmarshal([]byte(raw), &intermediate); err != nil {
		return MemorizeResponse{}, err
	}
	resp := MemorizeResponse{
		Annotations: intermediate.Annotations,
		NewMemories: make([]Memory, len(intermediate.NewMemories)),
	}
	for i, rm := range intermediate.NewMemories {
		resp.NewMemories[i] = Memory{
			Key:      rm.Key,
			Content:  rm.Content,
			Entities: parseEntities(rm.Entities),
		}
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

Return JSON only: {"new_memories":[{"key":"short_snake_case_key","content":"what to remember","entities":"EntityName1,EntityName2"}],"annotations":[{"node":"EntityName","note":"observation"}]}
Return {"new_memories":[],"annotations":[]} if nothing is worth saving.`,
		string(eventsJSON), string(memoryJSON))
}
