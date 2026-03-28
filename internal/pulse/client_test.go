package pulse

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "pulse.sqlite")

	cli, err := New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cli.Close()

	if cli.store == nil {
		t.Error("expected non-nil store")
	}
	if cli.coll == nil {
		t.Error("expected non-nil collector")
	}
}

func TestNewClient_BackwardsCompat(t *testing.T) {
	// NewClient should not panic; it ignores its arguments and uses DefaultDBPath.
	// We can't control the DB path here, so just verify it doesn't crash.
	// (In CI the home dir is writable; worst case it returns nil, which is fine.)
	_ = NewClient("http://ignored", 5)
}

func TestClose_Nil(t *testing.T) {
	var cli *Client
	cli.Close() // must not panic
}

func TestRecordMethods_NilSafe(t *testing.T) {
	var cli *Client
	// All Record* methods must be nil-safe (fire-and-forget contract).
	cli.RecordToolCall(ToolCallEvent{ToolName: "test"})
	cli.RecordContextDelivery(ContextDeliveryEvent{ToolName: "test"})
	cli.RecordSessionEvent("agent", "proj", "start")
	cli.RecordOutcomeSignal(OutcomeSignalEvent{SignalType: "task_done"})
}

func TestRecordAndFlush(t *testing.T) {
	dir := t.TempDir()
	cli, err := New(filepath.Join(dir, "pulse.sqlite"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Fire several events — should not panic.
	cli.RecordToolCall(ToolCallEvent{
		ToolName:      "get_context",
		AgentID:       "test-agent",
		ProjectID:     "test-proj",
		DurationMs:    42,
		Success:       true,
		ResponseBytes: 1200,
	})
	cli.RecordContextDelivery(ContextDeliveryEvent{
		ToolName:       "get_context",
		AgentID:        "test-agent",
		ResponseTokens: 800,
		BaselineTokens: 5400,
	})
	cli.RecordSessionEvent("test-agent", "test-proj", "start")
	cli.RecordOutcomeSignal(OutcomeSignalEvent{
		ProjectID:  "test-proj",
		Entity:     "Graph.New",
		SignalType: "task_done",
	})

	// Close flushes the collector before closing the store.
	cli.Close()
}

func TestFetchEffectiveness_Empty(t *testing.T) {
	dir := t.TempDir()
	cli, err := New(filepath.Join(dir, "pulse.sqlite"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cli.Close()

	// No signals yet — should return nil, not panic.
	result := cli.FetchEffectiveness("test-proj", 1)
	if result != nil {
		t.Errorf("expected nil effectiveness for empty store, got %v", result)
	}
}

func TestDBFileCreated(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "subdir", "pulse.sqlite")

	cli, err := New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cli.Close()

	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Error("expected DB file to be created")
	}
}

func TestFireAndForgetDoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	cli, err := New(filepath.Join(dir, "pulse.sqlite"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cli.Close()

	start := time.Now()
	for i := 0; i < 100; i++ {
		cli.RecordToolCall(ToolCallEvent{ToolName: "test", Success: true})
	}
	elapsed := time.Since(start)

	// 100 enqueues should be essentially instantaneous (< 50ms).
	if elapsed > 50*time.Millisecond {
		t.Errorf("RecordToolCall is blocking: 100 calls took %v", elapsed)
	}
}
