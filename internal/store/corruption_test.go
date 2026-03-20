package store_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/store"
)

// corruptFile overwrites the first 4KB of a file with garbage bytes, which is
// enough to destroy the SQLite header and trigger quick_check to report corruption.
func corruptFile(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("corruptFile: open %s: %v", path, err)
	}
	defer f.Close()
	garbage := make([]byte, 4096)
	for i := range garbage {
		garbage[i] = 0xDE // non-zero, non-SQLite header bytes
	}
	if _, err := f.WriteAt(garbage, 0); err != nil {
		t.Fatalf("corruptFile: write %s: %v", path, err)
	}
}

// TestOpenRecoverCorruptGraphDB verifies that store.Open() recovers from a
// corrupt graph.db by deleting and recreating it, returning a functional store.
// This proves the attack vector (corrupt graph → daemon refuses to start) is closed.
func TestOpenRecoverCorruptGraphDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "graph.db")

	// Step 1: create a valid store so the DB file exists.
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("initial Open: %v", err)
	}
	st.Close()

	// Step 2: corrupt the graph.db file.
	corruptFile(t, dbPath)

	// Step 3: Open() must succeed despite corruption (recovery path).
	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open with corrupt graph.db should recover, got error: %v", err)
	}
	defer st2.Close()

	// Step 4: the recovered store must be functional — GetPendingTasks exercises
	// the knowledge DB schema; if the schema is missing this will error.
	if _, err := st2.GetPendingTasks("", ""); err != nil {
		t.Fatalf("recovered store GetPendingTasks() failed: %v", err)
	}

	// Step 5: the corrupt backup should NOT exist (graph is deleted, not renamed).
	if _, statErr := os.Stat(dbPath + ".corrupt"); statErr == nil {
		t.Error("graph.db.corrupt should not exist — graph is deleted on corruption, not backed up")
	}
}

// TestOpenRecoverCorruptKnowledgeDB verifies that store.Open() continues in
// degraded mode when knowledge.db is corrupt, backing it up rather than losing it.
func TestOpenRecoverCorruptKnowledgeDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "graph.db")
	kPath := store.KnowledgePath(dbPath)

	// Step 1: create a valid store so both DB files exist.
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("initial Open: %v", err)
	}
	st.Close()

	// Step 2: corrupt the knowledge.db file.
	corruptFile(t, kPath)

	// Step 3: Open() must succeed (degraded mode, not a hard failure).
	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open with corrupt knowledge.db should enter degraded mode, got error: %v", err)
	}
	defer st2.Close()

	// Step 4: the corrupt knowledge.db should be backed up.
	if _, statErr := os.Stat(kPath + ".corrupt"); statErr != nil {
		t.Errorf("knowledge.db.corrupt backup should exist after corruption recovery: %v", statErr)
	}

	// Step 5: a fresh knowledge.db should be in place and functional.
	if _, err := st2.GetPendingTasks("", ""); err != nil {
		t.Fatalf("recovered store GetPendingTasks() failed: %v", err)
	}
}

// TestOpenHealthyDB verifies the happy path: a healthy DB passes quick_check
// and is opened normally without any recovery.
func TestOpenHealthyDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "graph.db")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open healthy db: %v", err)
	}
	defer st.Close()

	// No .corrupt backup files should exist.
	if _, statErr := os.Stat(dbPath + ".corrupt"); statErr == nil {
		t.Error("graph.db.corrupt should not exist for a healthy DB")
	}
	kPath := store.KnowledgePath(dbPath)
	if _, statErr := os.Stat(kPath + ".corrupt"); statErr == nil {
		t.Error("knowledge.db.corrupt should not exist for a healthy DB")
	}
}
