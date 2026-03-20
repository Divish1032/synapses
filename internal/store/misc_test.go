package store_test

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/store"
)

// ── WAL concurrency: concurrent reads and writes on the primary store ─────────
//
// This test directly validates the MaxOpenConns(2) setting in store.Open().
// With MaxOpenConns(1), all reads are serialized behind writes. With
// MaxOpenConns(2) and WAL mode, readers and writers proceed concurrently.
// If the concurrency setting is broken (e.g. reverted to 1 with a slow write
// path), this test becomes flaky under load. With a data-race or corruption
// bug it will either report t.Error or be caught by -race.

func TestWALConcurrency_ConcurrentReadWrite(t *testing.T) {
	st := openTestStore(t)

	const writers = 5
	const readers = 5

	// Seed one memory so readers have something to query from the start.
	seed := store.Memory{
		Tier:    store.TierProject,
		Content: "seed memory for wal concurrency test validation",
		AgentID: "test-agent",
		Source:  store.SourceManual,
	}
	if _, err := st.InsertMemory(seed); err != nil {
		t.Fatalf("seed InsertMemory: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, writers+readers)

	// Writers: insert distinct memories concurrently.
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m := store.Memory{
				Tier:    store.TierProject,
				Content: fmt.Sprintf("wal concurrency concurrent write number %d proof", i),
				AgentID: "test-agent",
				Source:  store.SourceManual,
			}
			if _, err := st.InsertMemory(m); err != nil {
				errs <- fmt.Errorf("writer %d: %w", i, err)
			}
		}(i)
	}

	// Readers: search memories concurrently with the writes.
	for i := range readers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results, err := st.SearchMemories("wal concurrency seed memory", 10)
			if err != nil {
				errs <- fmt.Errorf("reader %d: %w", i, err)
				return
			}
			_ = results // correctness: no panic, no error
		}(i)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}

	// After all goroutines finish, verify all written memories are readable.
	all, err := st.SearchMemories("wal concurrency concurrent write number", writers*2)
	if err != nil {
		t.Fatalf("post-concurrency recall: %v", err)
	}
	if len(all) < writers {
		t.Errorf("expected at least %d memories after concurrent writes, got %d", writers, len(all))
	}
}

// ── OpenReadOnly ──────────────────────────────────────────────────────────────

func TestOpenReadOnly_ExistingDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	// Create a regular store first.
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	st.Close()

	// Now open read-only.
	ro, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()

	// Reads should work.
	agents, err := ro.GetAgents()
	if err != nil {
		t.Fatalf("GetAgents on read-only: %v", err)
	}
	_ = agents
}

func TestOpenReadOnly_MissingDB_ReturnsError(t *testing.T) {
	_, err := store.OpenReadOnly("/nonexistent/path/test.db")
	if err == nil {
		t.Error("expected error opening nonexistent read-only DB")
	}
}

// ── AddAnnotationIfNew ────────────────────────────────────────────────────────

func TestAddAnnotationIfNew_Inserts(t *testing.T) {
	st := openTestStore(t)

	id, inserted, err := st.AddAnnotationIfNew("node-123", "agent-1", "this is a note", time.Hour)
	if err != nil {
		t.Fatalf("AddAnnotationIfNew: %v", err)
	}
	if !inserted {
		t.Error("expected inserted=true on first call")
	}
	if id == "" {
		t.Error("expected non-empty annotation ID")
	}
}

func TestAddAnnotationIfNew_DeduplicatesWithinWindow(t *testing.T) {
	st := openTestStore(t)

	_, inserted1, err := st.AddAnnotationIfNew("node-123", "agent-1", "same note", time.Hour)
	if err != nil || !inserted1 {
		t.Fatalf("first insert: err=%v inserted=%v", err, inserted1)
	}

	// Same note again within 1-hour window — should NOT insert.
	_, inserted2, err := st.AddAnnotationIfNew("node-123", "agent-1", "same note", time.Hour)
	if err != nil {
		t.Fatalf("second AddAnnotationIfNew: %v", err)
	}
	if inserted2 {
		t.Error("expected inserted=false for duplicate within dedup window")
	}
}

func TestAddAnnotationIfNew_DifferentNoteInserts(t *testing.T) {
	st := openTestStore(t)

	_, _, _ = st.AddAnnotationIfNew("node-123", "agent-1", "note A", time.Hour)

	_, inserted, err := st.AddAnnotationIfNew("node-123", "agent-1", "note B", time.Hour)
	if err != nil {
		t.Fatalf("AddAnnotationIfNew different note: %v", err)
	}
	if !inserted {
		t.Error("expected inserted=true for different note text")
	}
}

// ── TaskInput.UnmarshalJSON ───────────────────────────────────────────────────

func TestTaskInput_UnmarshalJSON_StringPriority(t *testing.T) {
	data := `{"title":"fix bug","priority":"p1","assigned_to":"agent-x"}`
	var ti store.TaskInput
	if err := json.Unmarshal([]byte(data), &ti); err != nil {
		t.Fatalf("UnmarshalJSON string priority: %v", err)
	}
	if ti.Priority != "p1" {
		t.Errorf("expected priority p1, got %q", ti.Priority)
	}
	if ti.Title != "fix bug" {
		t.Errorf("expected title 'fix bug', got %q", ti.Title)
	}
}

func TestTaskInput_UnmarshalJSON_NumericPriority(t *testing.T) {
	data := `{"title":"refactor","priority":2}`
	var ti store.TaskInput
	if err := json.Unmarshal([]byte(data), &ti); err != nil {
		t.Fatalf("UnmarshalJSON numeric priority: %v", err)
	}
	if ti.Priority != "p2" {
		t.Errorf("expected priority p2, got %q", ti.Priority)
	}
}

func TestTaskInput_UnmarshalJSON_NoPriority(t *testing.T) {
	data := `{"title":"some task"}`
	var ti store.TaskInput
	if err := json.Unmarshal([]byte(data), &ti); err != nil {
		t.Fatalf("UnmarshalJSON no priority: %v", err)
	}
	// Priority should be empty — no default applied.
	if ti.Title != "some task" {
		t.Errorf("expected title, got %q", ti.Title)
	}
}

func TestTaskInput_UnmarshalJSON_InvalidPriority(t *testing.T) {
	data := `{"title":"t","priority":{"bad":"value"}}`
	var ti store.TaskInput
	if err := json.Unmarshal([]byte(data), &ti); err == nil {
		t.Error("expected error for object priority")
	}
}
