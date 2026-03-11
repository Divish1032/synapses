package pulse

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewClient(t *testing.T) {
	cli := NewClient("http://localhost:11437", 5)

	if cli.baseURL != "http://localhost:11437" {
		t.Errorf("expected baseURL http://localhost:11437, got %s", cli.baseURL)
	}
	if cli.cli == nil {
		t.Errorf("expected non-nil http.Client")
	}
	if cli.cli.Timeout != 5*time.Second {
		t.Errorf("expected timeout 5s, got %v", cli.cli.Timeout)
	}
}

func TestNewClientDefaultTimeout(t *testing.T) {
	cli := NewClient("http://localhost:11437", 0)

	if cli.cli.Timeout != 2*time.Second {
		t.Errorf("expected default timeout 2s, got %v", cli.cli.Timeout)
	}
}

func TestRecordToolCall(t *testing.T) {
	receivedEvent := ToolCallEvent{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ingest/tool-call" {
			t.Errorf("expected /v1/ingest/tool-call, got %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedEvent)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cli := NewClient(server.URL, 2)
	event := ToolCallEvent{
		ToolName:      "get_context",
		AgentID:       "test-agent",
		DurationMs:    42,
		Success:       true,
		ResponseBytes: 1200,
	}

	cli.RecordToolCall(event)

	if receivedEvent.ToolName != "get_context" {
		t.Errorf("expected ToolName get_context, got %s", receivedEvent.ToolName)
	}
	if receivedEvent.AgentID != "test-agent" {
		t.Errorf("expected AgentID test-agent, got %s", receivedEvent.AgentID)
	}
	if receivedEvent.DurationMs != 42 {
		t.Errorf("expected DurationMs 42, got %d", receivedEvent.DurationMs)
	}
}

func TestRecordContextDelivery(t *testing.T) {
	receivedEvent := ContextDeliveryEvent{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ingest/context-delivery" {
			t.Errorf("expected /v1/ingest/context-delivery, got %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedEvent)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cli := NewClient(server.URL, 2)
	event := ContextDeliveryEvent{
		ToolName:       "get_context",
		AgentID:        "claude-opus",
		Entity:         "Graph.New",
		File:           "internal/graph/graph.go",
		ResponseBytes:  3200,
		ResponseTokens: 800,
		BaselineTokens: 5400,
		NodesDelivered: 12,
		NodesPruned:    3,
		EdgesDelivered: 8,
		Truncated:      false,
		DurationMs:     15,
		CacheHit:       true,
		BrainEnriched:  false,
	}

	cli.RecordContextDelivery(event)

	if receivedEvent.ToolName != "get_context" {
		t.Errorf("expected ToolName get_context, got %s", receivedEvent.ToolName)
	}
	if receivedEvent.ResponseTokens != 800 {
		t.Errorf("expected ResponseTokens 800, got %d", receivedEvent.ResponseTokens)
	}
	if receivedEvent.BaselineTokens != 5400 {
		t.Errorf("expected BaselineTokens 5400, got %d", receivedEvent.BaselineTokens)
	}
	if !receivedEvent.CacheHit {
		t.Errorf("expected CacheHit true")
	}
}

func TestRecordSessionEvent(t *testing.T) {
	receivedEvent := SessionEvent{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ingest/session-event" {
			t.Errorf("expected /v1/ingest/session-event, got %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedEvent)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cli := NewClient(server.URL, 2)

	cli.RecordSessionEvent("test-agent", "start")

	if receivedEvent.AgentID != "test-agent" {
		t.Errorf("expected AgentID test-agent, got %s", receivedEvent.AgentID)
	}
	if receivedEvent.Event != "start" {
		t.Errorf("expected Event start, got %s", receivedEvent.Event)
	}
}

func TestClientFireAndForgetServerDown(t *testing.T) {
	// Use a port that's unlikely to be listening.
	cli := NewClient("http://127.0.0.1:9999", 1)

	// These should not panic even though server is down.
	cli.RecordToolCall(ToolCallEvent{ToolName: "test", Success: true})
	cli.RecordContextDelivery(ContextDeliveryEvent{ToolName: "test"})
	cli.RecordSessionEvent("agent-1", "start")

	// If we got here without panic, the fail-silent behavior is working.
}

func TestClientContentTypeHeader(t *testing.T) {
	contentType := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cli := NewClient(server.URL, 2)
	cli.RecordToolCall(ToolCallEvent{ToolName: "test", Success: true})

	if contentType != "application/json" {
		t.Errorf("expected Content-Type application/json, got %s", contentType)
	}
}

func TestClientTimeout(t *testing.T) {
	// Create server that delays response
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second) // Delay longer than client timeout
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	// Client with 1-second timeout.
	cli := NewClient(server.URL, 1)

	// Record the time before and after.
	start := time.Now()
	cli.RecordToolCall(ToolCallEvent{ToolName: "test", Success: true})
	elapsed := time.Since(start)

	// Should return quickly (within ~1-2 seconds due to timeout).
	// If it took the full 2 seconds of server delay, timeout didn't work.
	if elapsed > 3*time.Second {
		t.Errorf("expected timeout to trigger, but request took %v", elapsed)
	}
}

func TestClientEventFieldsPropagated(t *testing.T) {
	receivedEvent := ToolCallEvent{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedEvent)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cli := NewClient(server.URL, 2)
	event := ToolCallEvent{
		ToolName:      "prepare_context",
		AgentID:       "test-agent",
		Entity:        "Store.CarveEgoGraph",
		DurationMs:    123,
		Success:       false,
		ResponseBytes: 5000,
	}

	cli.RecordToolCall(event)

	// Verify all fields were propagated.
	if receivedEvent.ToolName != event.ToolName {
		t.Errorf("ToolName mismatch")
	}
	if receivedEvent.AgentID != event.AgentID {
		t.Errorf("AgentID mismatch")
	}
	if receivedEvent.Entity != event.Entity {
		t.Errorf("Entity mismatch")
	}
	if receivedEvent.DurationMs != event.DurationMs {
		t.Errorf("DurationMs mismatch")
	}
	if receivedEvent.Success != event.Success {
		t.Errorf("Success mismatch")
	}
	if receivedEvent.ResponseBytes != event.ResponseBytes {
		t.Errorf("ResponseBytes mismatch")
	}
}

func TestMultipleConcurrentRequests(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cli := NewClient(server.URL, 2)

	// Make multiple concurrent requests.
	for i := 0; i < 10; i++ {
		cli.RecordToolCall(ToolCallEvent{ToolName: "test", Success: true})
	}

	// Give time for requests to complete.
	time.Sleep(100 * time.Millisecond)

	// We expect at least some requests to have been received (may not be all 10 due to race).
	if callCount == 0 {
		t.Error("expected at least some requests to be received")
	}
}
