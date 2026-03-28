package store_test

import (
	"os"
	"path/filepath"
	"strings"
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
func TestOpenRecoverCorruptGraphDB(t *testing.T) {
	t.Parallel()
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

	// Step 4: the recovered store must be functional.
	if _, err := st2.GetPendingTasks("", ""); err != nil {
		t.Fatalf("recovered store GetPendingTasks() failed: %v", err)
	}

	// Step 5: graph is deleted (not renamed) on corruption recovery.
	if _, statErr := os.Stat(dbPath + ".corrupt"); statErr == nil {
		t.Error("graph.db.corrupt should not exist — graph is deleted on corruption, not backed up")
	}
}

// TestOpenRecoverCorruptKnowledgeDB verifies that store.Open() continues in
// degraded mode when knowledge.db is corrupt, backing it up rather than losing it.
func TestOpenRecoverCorruptKnowledgeDB(t *testing.T) {
	t.Parallel()
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


// TestOpenRecoverCorruptKnowledgeDB_WithWALSidecar verifies that a leftover
// WAL file from a corrupt knowledge.db is cleaned up during recovery.
// This is the critical production scenario: WAL checkpoint replay against
// a fresh empty DB would corrupt it on first open.
func TestOpenRecoverCorruptKnowledgeDB_WithWALSidecar(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "graph.db")
	kPath := store.KnowledgePath(dbPath)

	// Create a valid store.
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("initial Open: %v", err)
	}
	st.Close()

	// Corrupt knowledge.db and plant a fake WAL sidecar.
	corruptFile(t, kPath)
	walPath := kPath + "-wal"
	if err := os.WriteFile(walPath, []byte("fake wal data from corrupt session"), 0o644); err != nil {
		t.Fatalf("write fake WAL: %v", err)
	}

	// Open must succeed: store is functional after recovery.
	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open with corrupt knowledge.db + WAL should recover, got error: %v", err)
	}
	defer st2.Close()

	if _, err := st2.GetPendingTasks("", ""); err != nil {
		t.Fatalf("store after WAL sidecar recovery: GetPendingTasks() failed: %v", err)
	}

	// The old corrupt WAL content must not be present. If recoverKnowledgeDB
	// didn't delete it and SQLite replayed it against the fresh DB, the store
	// would be corrupt and GetPendingTasks above would have failed. We also
	// verify directly: if the file still exists, its content must differ
	// from the fake data we planted (i.e. SQLite replaced it with a valid WAL).
	if content, readErr := os.ReadFile(walPath); readErr == nil {
		if string(content) == "fake wal data from corrupt session" {
			t.Error("old corrupt WAL content is still present — recovery did not clean up the sidecar")
		}
	}
}

// TestOpenGraphDBDeletionFailure verifies that when graph.db cannot be deleted
// during corruption recovery, Open() returns a clear actionable error rather
// than a confusing schema error from reopening the corrupt file.
func TestOpenGraphDBDeletionFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "graph.db")

	// Create a valid store.
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("initial Open: %v", err)
	}
	st.Close()

	// Corrupt graph.db, then make it read-only so deletion fails.
	corruptFile(t, dbPath)
	if err := os.Chmod(dbPath, 0o444); err != nil {
		t.Skip("cannot chmod to test deletion failure:", err)
	}
	// Also make the directory read-only so os.Remove can't unlink the file.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Skip("cannot chmod dir to test deletion failure:", err)
	}
	t.Cleanup(func() {
		// Restore permissions so TempDir cleanup works.
		_ = os.Chmod(dir, 0o755)
		_ = os.Chmod(dbPath, 0o644)
	})

	_, openErr := store.Open(dbPath)
	if openErr == nil {
		t.Fatal("Open() with undeletable corrupt graph.db should return an error")
	}
	// Error must be actionable — tell the user to delete the file manually.
	if !strings.Contains(openErr.Error(), "manually") {
		t.Errorf("error message should tell user to delete manually, got: %v", openErr)
	}
}

// TestOpenHealthyDB verifies the happy path: a healthy DB passes quick_check
// and is opened normally without any recovery side effects.
func TestOpenHealthyDB(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "graph.db")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open healthy db: %v", err)
	}
	defer st.Close()

	// No .corrupt backup files should exist on a healthy open.
	if _, statErr := os.Stat(dbPath + ".corrupt"); statErr == nil {
		t.Error("graph.db.corrupt should not exist for a healthy DB")
	}
	kPath := store.KnowledgePath(dbPath)
	if _, statErr := os.Stat(kPath + ".corrupt"); statErr == nil {
		t.Error("knowledge.db.corrupt should not exist for a healthy DB")
	}
}

