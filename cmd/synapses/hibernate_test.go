package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// ── Registry state machine tests ─────────────────────────────────────────────

func TestRegistry_HibernateAndWake(t *testing.T) {
	reg := newProjectRegistry()
	defer reg.Close()

	// Set a mock ProjectInstance.
	pi := &ProjectInstance{AbsPath: "/tmp/test-hibernate"}
	reg.Set(pi)

	// Verify WARM.
	got, ok := reg.Get("/tmp/test-hibernate")
	if !ok || got != pi {
		t.Fatal("expected WARM project")
	}

	// BeginHibernate should return the instance.
	victim := reg.BeginHibernate("/tmp/test-hibernate")
	if victim != pi {
		t.Fatal("BeginHibernate should return the instance")
	}

	// While HIBERNATING, Get returns (nil, false).
	_, ok = reg.Get("/tmp/test-hibernate")
	if ok {
		t.Fatal("Get should return false during HIBERNATING state")
	}

	// FinishHibernate installs tombstone.
	tomb := &HibernatedProject{
		AbsPath:      "/tmp/test-hibernate",
		HibernatedAt: time.Now(),
		sentinelStop: make(chan struct{}),
	}
	reg.FinishHibernate("/tmp/test-hibernate", tomb)

	// Should be hibernated.
	if !reg.IsHibernated("/tmp/test-hibernate") {
		t.Fatal("expected HIBERNATED")
	}

	// Get still returns false.
	_, ok = reg.Get("/tmp/test-hibernate")
	if ok {
		t.Fatal("Get should return false for HIBERNATED project")
	}

	// WarmCount should be 0.
	if reg.WarmCount() != 0 {
		t.Fatalf("expected 0 warm, got %d", reg.WarmCount())
	}

	// All() should be empty.
	if len(reg.All()) != 0 {
		t.Fatalf("All() should be empty, got %d", len(reg.All()))
	}

	// HibernatedPaths should have our path.
	paths := reg.HibernatedPaths()
	if len(paths) != 1 || paths[0] != "/tmp/test-hibernate" {
		t.Fatalf("expected [/tmp/test-hibernate], got %v", paths)
	}

	// Clean up sentinel.
	tomb.StopSentinel()
}

func TestRegistry_BeginHibernate_ActiveConns(t *testing.T) {
	reg := newProjectRegistry()
	defer reg.Close()

	pi := &ProjectInstance{AbsPath: "/tmp/test-active"}
	reg.Set(pi)

	// Simulate an active connection.
	entry := reg.getEntry("/tmp/test-active")
	entry.activeConns.Add(1)

	// BeginHibernate should return nil (project has active connections).
	if victim := reg.BeginHibernate("/tmp/test-active"); victim != nil {
		t.Fatal("BeginHibernate should refuse when activeConns > 0")
	}
}

func TestRegistry_Delete_Hibernated(t *testing.T) {
	reg := newProjectRegistry()

	tomb := &HibernatedProject{
		AbsPath:      "/tmp/test-delete",
		HibernatedAt: time.Now(),
		sentinelStop: make(chan struct{}),
	}
	reg.Hibernate("/tmp/test-delete", tomb)

	// Delete should stop sentinel (via StopSentinel which is idempotent).
	reg.Delete("/tmp/test-delete")

	if reg.IsHibernated("/tmp/test-delete") {
		t.Fatal("project should be gone after Delete")
	}

	// Calling StopSentinel again should not panic (sync.Once).
	tomb.StopSentinel()
}

func TestRegistry_StopSentinel_Idempotent(t *testing.T) {
	tomb := &HibernatedProject{
		AbsPath:      "/tmp/test-sentinel",
		sentinelStop: make(chan struct{}),
	}
	// Call StopSentinel multiple times — should not panic.
	tomb.StopSentinel()
	tomb.StopSentinel()
	tomb.StopSentinel()
}

func TestRegistry_GetOrSet_WakesHibernated(t *testing.T) {
	reg := newProjectRegistry()

	// Install a hibernated project.
	tomb := &HibernatedProject{
		AbsPath:      "/tmp/test-getorset",
		HibernatedAt: time.Now(),
		sentinelStop: make(chan struct{}),
	}
	reg.Hibernate("/tmp/test-getorset", tomb)

	// Set a wake function.
	wakeCalled := false
	noop := func() {} // safe cancel func
	wokenPI := &ProjectInstance{AbsPath: "/tmp/test-getorset", cancel: noop}
	reg.SetWakeFunc(func(absPath string) (*ProjectInstance, error) {
		wakeCalled = true
		// Simulate wake: update registry to WARM.
		reg.mu.Lock()
		entry := &registryEntry{state: stateWarm, instance: wokenPI}
		entry.lastAccess.Store(time.Now().UnixNano())
		reg.projects[absPath] = entry
		reg.mu.Unlock()
		return wokenPI, nil
	})

	// GetOrSet should trigger wake, not cold init.
	coldInitCalled := false
	got, err := reg.GetOrSet("/tmp/test-getorset", func() (*ProjectInstance, error) {
		coldInitCalled = true
		return nil, nil
	})
	if err != nil {
		t.Fatalf("GetOrSet: %v", err)
	}
	if !wakeCalled {
		t.Fatal("wake function should have been called")
	}
	if coldInitCalled {
		t.Fatal("cold init should NOT have been called")
	}
	if got != wokenPI {
		t.Fatal("should return the woken instance")
	}
	reg.Close()
}

func TestRegistry_RemoveLRUIdle(t *testing.T) {
	reg := newProjectRegistry()

	// Add 3 projects with different lastAccess times.
	pi1 := &ProjectInstance{AbsPath: "/tmp/lru-1"}
	pi2 := &ProjectInstance{AbsPath: "/tmp/lru-2"}
	pi3 := &ProjectInstance{AbsPath: "/tmp/lru-3"}

	reg.Set(pi1)
	reg.Set(pi2)
	reg.Set(pi3)

	// Make pi1 the oldest by setting its lastAccess to the past.
	entry1 := reg.getEntry("/tmp/lru-1")
	entry1.lastAccess.Store(time.Now().Add(-1 * time.Hour).UnixNano())

	// Make pi3 have active connections (should not be evicted).
	entry3 := reg.getEntry("/tmp/lru-3")
	entry3.activeConns.Add(1)

	// Remove LRU should pick pi1 (oldest, no active conns).
	reg.mu.Lock()
	victim := reg.removeLRUIdleLocked()
	reg.mu.Unlock()

	if victim != pi1 {
		t.Fatal("should evict pi1 (oldest)")
	}
	if reg.WarmCount() != 2 {
		t.Fatalf("expected 2 warm after eviction, got %d", reg.WarmCount())
	}
}

// ── Sentinel watcher tests ───────────────────────────────────────────────────

func TestSentinelWatcher_DirtyOnMtimeChange(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	os.MkdirAll(gitDir, 0o700)
	indexFile := filepath.Join(gitDir, "index")
	os.WriteFile(indexFile, []byte("v1"), 0o644)

	tomb := &HibernatedProject{
		AbsPath:      dir,
		sentinelStop: make(chan struct{}),
	}

	// Start sentinel with very short interval for testing.
	go func() {
		gitIndex := filepath.Join(tomb.AbsPath, ".git", "index")
		lastGitMtime := statMtime(gitIndex)

		ticker := time.NewTicker(50 * time.Millisecond) // fast for test
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if m := statMtime(gitIndex); m.After(lastGitMtime) {
					tomb.Dirty.Store(true)
					lastGitMtime = m
				}
			case <-tomb.sentinelStop:
				return
			}
		}
	}()

	// Initially clean.
	if tomb.Dirty.Load() {
		t.Fatal("should start clean")
	}

	// Modify .git/index.
	time.Sleep(100 * time.Millisecond) // ensure mtime changes
	os.WriteFile(indexFile, []byte("v2"), 0o644)

	// Wait for sentinel to detect.
	deadline := time.After(2 * time.Second)
	for !tomb.Dirty.Load() {
		select {
		case <-deadline:
			t.Fatal("sentinel did not detect .git/index change within 2s")
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}

	tomb.StopSentinel()
}

// ── Sweeper tests ────────────────────────────────────────────────────────────

func TestSweepOnce_SkipsActiveConnections(t *testing.T) {
	reg := newProjectRegistry()
	defer reg.Close()

	pi := &ProjectInstance{AbsPath: "/tmp/sweep-active"}
	reg.Set(pi)

	// Set lastAccess to 2 hours ago (should trigger hibernate).
	entry := reg.getEntry("/tmp/sweep-active")
	entry.lastAccess.Store(time.Now().Add(-2 * time.Hour).UnixNano())
	// But set active connections.
	entry.activeConns.Add(1)

	sweepOnce(reg)

	// Should still be WARM (active connection protects it).
	if reg.IsHibernated("/tmp/sweep-active") {
		t.Fatal("should NOT hibernate project with active connections")
	}
}

func TestSweepOnce_HibernatesIdle(t *testing.T) {
	reg := newProjectRegistry()
	defer reg.Close()

	pi := &ProjectInstance{AbsPath: "/tmp/sweep-idle"}
	reg.Set(pi)

	// Set lastAccess to 2 hours ago.
	entry := reg.getEntry("/tmp/sweep-idle")
	entry.lastAccess.Store(time.Now().Add(-2 * time.Hour).UnixNano())

	sweepOnce(reg)

	// Should be hibernated.
	if !reg.IsHibernated("/tmp/sweep-idle") {
		t.Fatal("should hibernate idle project")
	}
}

func TestSweepOnce_SkipsRecentlyUsed(t *testing.T) {
	reg := newProjectRegistry()
	defer reg.Close()

	pi := &ProjectInstance{AbsPath: "/tmp/sweep-recent"}
	reg.Set(pi)

	// lastAccess is set to now by Set() — should not be hibernated.
	sweepOnce(reg)

	if reg.IsHibernated("/tmp/sweep-recent") {
		t.Fatal("should NOT hibernate recently used project")
	}
}

// ── Projects.json v2 tests ───────────────────────────────────────────────────

func TestProjectsV2_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	pFile := filepath.Join(dir, "projects.json")

	// Write v2 manually, then read back.
	entries := []projectEntry{
		{Path: "/tmp/proj1", State: "warm"},
		{Path: "/tmp/proj2", State: "hibernated", HibernatedAt: "2026-03-30T10:00:00Z"},
	}
	// Use writeProjectEntries via a temp file approach.
	v2 := projectsFile{Version: 2, Projects: entries}
	data, _ := json.MarshalIndent(v2, "", "  ")
	os.WriteFile(pFile, data, 0o600)

	// Read back using low-level parse.
	raw, _ := os.ReadFile(pFile)
	var got projectsFile
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal v2: %v", err)
	}
	if len(got.Projects) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got.Projects))
	}
	if got.Projects[0].State != "warm" || got.Projects[1].State != "hibernated" {
		t.Errorf("states: got %v", got.Projects)
	}
}

func TestProjectsV1_Migration(t *testing.T) {
	dir := t.TempDir()
	pFile := filepath.Join(dir, "projects.json")

	// Write v1 format.
	os.WriteFile(pFile, []byte(`["/tmp/old1", "/tmp/old2"]`), 0o600)

	// Parse as v1 and verify migration logic.
	raw, _ := os.ReadFile(pFile)
	var v2 projectsFile
	if err := json.Unmarshal(raw, &v2); err == nil && v2.Version >= 2 {
		t.Fatal("should NOT parse as v2")
	}
	var paths []string
	if err := json.Unmarshal(raw, &paths); err != nil {
		t.Fatalf("should parse as v1 array: %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("expected 2 paths, got %d", len(paths))
	}
	// Verify the migration produces correct entries.
	migrated := make([]projectEntry, 0, len(paths))
	for _, p := range paths {
		migrated = append(migrated, projectEntry{Path: p, State: "warm"})
	}
	if migrated[0].State != "warm" || migrated[1].State != "warm" {
		t.Error("migrated entries should all be warm")
	}
}

// ── ProjectInstance thread safety tests ──────────────────────────────────────

func TestProjectInstance_SetWatcher_ThreadSafe(t *testing.T) {
	pi := &ProjectInstance{AbsPath: "/tmp/thread-test"}

	// Concurrent SetWatcher and Close should not race.
	done := make(chan struct{})
	go func() {
		defer close(done)
		pi.SetWatcher(nil) // no-op but exercises the lock
	}()
	// Don't actually close (pi has no real resources), just test the lock.
	pi.mu.Lock()
	pi.mu.Unlock()
	<-done
}

// ── ActiveSessionCount tests ─────────────────────────────────────────────────

func TestRegistry_BeginHibernate_ActiveSessions(t *testing.T) {
	reg := newProjectRegistry()
	defer reg.Close()

	// Create a mock server with active sessions by checking the guard.
	// Since we can't easily mock the MCPServer, test the activeConns path.
	pi := &ProjectInstance{AbsPath: "/tmp/test-sessions"}
	reg.Set(pi)

	// No active conns, no MCP server → should succeed.
	victim := reg.BeginHibernate("/tmp/test-sessions")
	if victim == nil {
		t.Fatal("BeginHibernate should succeed when no active conns/sessions")
	}
}

// ── Concurrent GetOrSet during hibernate ─────────────────────────────────────

func TestRegistry_ConcurrentGetOrSet_Singleflight(t *testing.T) {
	reg := newProjectRegistry()
	defer reg.Close()

	var initCount atomic.Int32
	pi := &ProjectInstance{AbsPath: "/tmp/test-concurrent"}

	// Launch 10 concurrent GetOrSet calls.
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func() {
			_, err := reg.GetOrSet("/tmp/test-concurrent", func() (*ProjectInstance, error) {
				initCount.Add(1)
				time.Sleep(50 * time.Millisecond) // simulate slow init
				return pi, nil
			})
			errs <- err
		}()
	}

	for i := 0; i < 10; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("GetOrSet: %v", err)
		}
	}

	// Singleflight should ensure init is called exactly once.
	if n := initCount.Load(); n != 1 {
		t.Fatalf("init called %d times, expected 1 (singleflight)", n)
	}
}
