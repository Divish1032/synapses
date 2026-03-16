package types

import "testing"

func TestBrainUsageEvent_TotalTokens(t *testing.T) {
	tests := []struct {
		name   string
		event  BrainUsageEvent
		want   int
	}{
		{
			name:  "both zero",
			event: BrainUsageEvent{PromptTokens: 0, CompletionTokens: 0},
			want:  0,
		},
		{
			name:  "prompt only",
			event: BrainUsageEvent{PromptTokens: 100, CompletionTokens: 0},
			want:  100,
		},
		{
			name:  "completion only",
			event: BrainUsageEvent{PromptTokens: 0, CompletionTokens: 50},
			want:  50,
		},
		{
			name:  "both set",
			event: BrainUsageEvent{PromptTokens: 200, CompletionTokens: 300},
			want:  500,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.event.TotalTokens()
			if got != tt.want {
				t.Errorf("TotalTokens() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestToolCallEvent_Fields(t *testing.T) {
	ev := ToolCallEvent{
		ToolName:      "get_context",
		AgentID:       "agent-1",
		ProjectID:     "proj-1",
		Entity:        "MyFunc",
		DurationMs:    42,
		Success:       true,
		ResponseBytes: 1024,
	}
	if ev.ToolName != "get_context" {
		t.Error("ToolName not set")
	}
	if !ev.Success {
		t.Error("Success not set")
	}
}

func TestContextDeliveryEvent_Fields(t *testing.T) {
	ev := ContextDeliveryEvent{
		ToolName:       "get_context",
		ResponseTokens: 500,
		BaselineTokens: 2000,
		NodesDelivered: 10,
		NodesPruned:    5,
		Truncated:      false,
		CacheHit:       true,
		BrainEnriched:  false,
	}
	if ev.ResponseTokens != 500 {
		t.Error("ResponseTokens wrong")
	}
	if !ev.CacheHit {
		t.Error("CacheHit not set")
	}
}

func TestOutcomeSignalEvent_Fields(t *testing.T) {
	ev := OutcomeSignalEvent{
		ProjectID:  "proj-1",
		AgentID:    "agent-1",
		Entity:     "MyFunc",
		SignalType: "task_done",
		Count:      1,
	}
	if ev.SignalType != "task_done" {
		t.Error("SignalType wrong")
	}
}

func TestEntityEffectiveness_Fields(t *testing.T) {
	e := EntityEffectiveness{
		Entity:     "MyFunc",
		Score:      0.75,
		Signals:    10,
		Positives:  7,
		Negatives:  3,
		Suggestion: "test",
	}
	if e.Score != 0.75 {
		t.Error("Score wrong")
	}
}

func TestAgentLLMUsageEvent_Fields(t *testing.T) {
	ev := AgentLLMUsageEvent{
		SessionID:    "sess-1",
		AgentID:      "agent-1",
		Model:        "claude-sonnet-4-6",
		Provider:     "anthropic",
		InputTokens:  1000,
		OutputTokens: 500,
		CostUSD:      0.05,
	}
	if ev.Model != "claude-sonnet-4-6" {
		t.Error("Model wrong")
	}
	if ev.CostUSD != 0.05 {
		t.Error("CostUSD wrong")
	}
}

func TestSessionEvent_Fields(t *testing.T) {
	ev := SessionEvent{
		AgentID:   "agent-1",
		ProjectID: "proj-1",
		Event:     "start",
	}
	if ev.Event != "start" {
		t.Error("Event wrong")
	}
}
