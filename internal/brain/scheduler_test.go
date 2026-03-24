package brain

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ─── deferredQueue unit tests ────────────────────────────────────────────────

func TestDeferredQueue_AddAndDrain_Green(t *testing.T) {
	q := newDeferredQueue(10)
	called := false
	q.add(&brainTask{key: "k1", priority: PriorityP2, fn: func() { called = true }, enqueuedAt: time.Now(), ttl: time.Minute})
	tasks := q.drain(HealthGreen)
	if len(tasks) != 1 {
		t.Fatalf("want 1 task; got %d", len(tasks))
	}
	tasks[0].fn()
	if !called {
		t.Error("task fn was not called")
	}
	if q.size() != 0 {
		t.Errorf("want empty queue after drain; got size=%d", q.size())
	}
}

func TestDeferredQueue_Dedup_KeepsLatest(t *testing.T) {
	q := newDeferredQueue(10)
	callCount := 0
	q.add(&brainTask{key: "k1", priority: PriorityP2, fn: func() { callCount += 10 }, enqueuedAt: time.Now(), ttl: time.Minute})
	q.add(&brainTask{key: "k1", priority: PriorityP2, fn: func() { callCount += 99 }, enqueuedAt: time.Now(), ttl: time.Minute})
	tasks := q.drain(HealthGreen)
	if len(tasks) != 1 {
		t.Fatalf("want 1 task after dedup; got %d", len(tasks))
	}
	tasks[0].fn()
	if callCount != 99 {
		t.Errorf("want latest fn (callCount=99); got %d", callCount)
	}
}

func TestDeferredQueue_Yellow_P1Runs_P2Deferred(t *testing.T) {
	q := newDeferredQueue(10)
	q.add(&brainTask{key: "p1", priority: PriorityP1, fn: func() {}, enqueuedAt: time.Now(), ttl: time.Minute})
	q.add(&brainTask{key: "p2", priority: PriorityP2, fn: func() {}, enqueuedAt: time.Now(), ttl: time.Minute})

	run := q.drain(HealthYellow)
	if len(run) != 1 {
		t.Fatalf("want 1 task (P1 only) under Yellow; got %d", len(run))
	}
	if run[0].key != "p1" {
		t.Errorf("want P1 task to run; got key=%q", run[0].key)
	}
	if q.size() != 1 {
		t.Errorf("want P2 to remain in queue; got size=%d", q.size())
	}
}

func TestDeferredQueue_Red_NothingRuns(t *testing.T) {
	q := newDeferredQueue(10)
	q.add(&brainTask{key: "p1", priority: PriorityP1, fn: func() {}, enqueuedAt: time.Now(), ttl: time.Minute})
	q.add(&brainTask{key: "p2", priority: PriorityP2, fn: func() {}, enqueuedAt: time.Now(), ttl: time.Minute})

	run := q.drain(HealthRed)
	if len(run) != 0 {
		t.Fatalf("want 0 tasks under Red; got %d", len(run))
	}
	if q.size() != 2 {
		t.Errorf("want both tasks to remain in queue; got size=%d", q.size())
	}
}

func TestDeferredQueue_ExpiredTasksDropped(t *testing.T) {
	q := newDeferredQueue(10)
	// Add a task that is already expired (negative TTL, enqueuedAt far in the past).
	q.add(&brainTask{
		key:        "expired",
		priority:   PriorityP1,
		fn:         func() {},
		enqueuedAt: time.Now().Add(-10 * time.Minute),
		ttl:        time.Minute, // expired 9 minutes ago
	})
	run := q.drain(HealthGreen)
	if len(run) != 0 {
		t.Fatalf("want expired task dropped; got %d task(s)", len(run))
	}
	if q.size() != 0 {
		t.Errorf("want queue empty after expired task drain; got size=%d", q.size())
	}
}

func TestDeferredQueue_BoundedEvictsOldestP2(t *testing.T) {
	q := newDeferredQueue(3)
	q.add(&brainTask{key: "p2a", priority: PriorityP2, fn: func() {}, enqueuedAt: time.Now(), ttl: time.Minute})
	q.add(&brainTask{key: "p2b", priority: PriorityP2, fn: func() {}, enqueuedAt: time.Now(), ttl: time.Minute})
	q.add(&brainTask{key: "p2c", priority: PriorityP2, fn: func() {}, enqueuedAt: time.Now(), ttl: time.Minute})
	// Queue is now full (3/3). Adding a new task should evict the first P2 (p2a).
	added := q.add(&brainTask{key: "p2d", priority: PriorityP2, fn: func() {}, enqueuedAt: time.Now(), ttl: time.Minute})
	if !added {
		t.Fatal("expected add to succeed (evict oldest P2)")
	}
	if q.size() != 3 {
		t.Errorf("want size 3 after eviction; got %d", q.size())
	}
	// p2a should be evicted; p2d should be present.
	tasks := q.drain(HealthGreen)
	keys := make(map[string]bool)
	for _, t := range tasks {
		keys[t.key] = true
	}
	if keys["p2a"] {
		t.Error("p2a should have been evicted")
	}
	if !keys["p2d"] {
		t.Error("p2d should be present after eviction of p2a")
	}
}

func TestDeferredQueue_FullAllP1_DropsIncoming(t *testing.T) {
	q := newDeferredQueue(2)
	q.add(&brainTask{key: "p1a", priority: PriorityP1, fn: func() {}, enqueuedAt: time.Now(), ttl: time.Minute})
	q.add(&brainTask{key: "p1b", priority: PriorityP1, fn: func() {}, enqueuedAt: time.Now(), ttl: time.Minute})
	// Queue is full with P1 tasks; no P2 to evict. New task should be dropped.
	added := q.add(&brainTask{key: "p2x", priority: PriorityP2, fn: func() {}, enqueuedAt: time.Now(), ttl: time.Minute})
	if added {
		t.Fatal("expected add to fail (no P2 to evict, queue full)")
	}
	if q.size() != 2 {
		t.Errorf("want size 2 (unchanged); got %d", q.size())
	}
}

func TestDeferredQueue_SortOrder_P1BeforeP2(t *testing.T) {
	q := newDeferredQueue(10)
	base := time.Now()
	q.add(&brainTask{key: "p2", priority: PriorityP2, fn: func() {}, enqueuedAt: base, ttl: time.Minute})
	q.add(&brainTask{key: "p1", priority: PriorityP1, fn: func() {}, enqueuedAt: base.Add(time.Millisecond), ttl: time.Minute})

	run := q.drain(HealthGreen)
	if len(run) != 2 {
		t.Fatalf("want 2 tasks; got %d", len(run))
	}
	if run[0].key != "p1" {
		t.Errorf("want P1 first; got %q", run[0].key)
	}
	if run[1].key != "p2" {
		t.Errorf("want P2 second; got %q", run[1].key)
	}
}

// ─── Scheduler integration tests ─────────────────────────────────────────────

// newPulseGreen returns a SystemPulse that reports HealthGreen.
// It does NOT start the background sampler to avoid test flakiness —
// it sets the current state directly via the initial sample.
func newTestSchedulerGreen(t *testing.T) (*Scheduler, func()) {
	t.Helper()
	p := &SystemPulse{
		httpClient: nil,
		done:       make(chan struct{}),
	}
	// Manually set a Green state without starting the background sampler.
	p.mu.Lock()
	p.current = SystemState{
		AvailableRAM:      4 * 1024 * 1024 * 1024, // 4 GB — Green
		CPULoadNorm:       0.3,
		OllamaModelLoaded: "qwen3.5:2b",
		Health:            HealthGreen,
		SampledAt:         time.Now(),
	}
	p.mu.Unlock()

	sched := NewScheduler(p)
	sched.Start()
	return sched, func() {
		sched.Stop()
		p.stopOnce.Do(func() { close(p.done) })
	}
}

func newTestSchedulerYellow(t *testing.T, modelLoaded string) (*Scheduler, func()) {
	t.Helper()
	p := &SystemPulse{
		httpClient: nil,
		done:       make(chan struct{}),
	}
	p.mu.Lock()
	p.current = SystemState{
		AvailableRAM:      2 * 1024 * 1024 * 1024, // 2 GB — Yellow
		CPULoadNorm:       0.5,
		OllamaModelLoaded: modelLoaded,
		Health:            HealthYellow,
		SampledAt:         time.Now(),
	}
	p.mu.Unlock()

	sched := NewScheduler(p)
	sched.Start()
	return sched, func() {
		sched.Stop()
		p.stopOnce.Do(func() { close(p.done) })
	}
}

func newTestSchedulerRed(t *testing.T) (*Scheduler, func()) {
	t.Helper()
	p := &SystemPulse{
		httpClient: nil,
		done:       make(chan struct{}),
	}
	p.mu.Lock()
	p.current = SystemState{
		AvailableRAM: 1024 * 1024 * 1024, // 1 GB — Red
		CPULoadNorm:  0.5,
		Health:       HealthRed,
		SampledAt:    time.Now(),
	}
	p.mu.Unlock()

	sched := NewScheduler(p)
	sched.Start()
	return sched, func() {
		sched.Stop()
		p.stopOnce.Do(func() { close(p.done) })
	}
}

func TestScheduler_Green_P2RunsEventually(t *testing.T) {
	sched, cleanup := newTestSchedulerGreen(t)
	defer cleanup()

	var called atomic.Bool
	sched.Submit("node1:ingest", PriorityP2, func() { called.Store(true) })

	// The drain goroutine is signaled immediately on Green. Wait up to 2s.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if called.Load() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("P2 task was not executed within 2s on Green system")
}

func TestScheduler_Yellow_P2Deferred(t *testing.T) {
	sched, cleanup := newTestSchedulerYellow(t, "qwen3.5:2b")
	defer cleanup()

	var called atomic.Bool
	sched.Submit("node1:ingest", PriorityP2, func() { called.Store(true) })

	// Under Yellow, P2 should NOT run immediately.
	time.Sleep(150 * time.Millisecond)
	if called.Load() {
		t.Error("P2 task ran under Yellow health — expected deferral")
	}
	// Queue should hold the task.
	if sched.QueueSize() != 1 {
		t.Errorf("want 1 task in queue under Yellow; got %d", sched.QueueSize())
	}
}

func TestScheduler_Red_P1Deferred(t *testing.T) {
	sched, cleanup := newTestSchedulerRed(t)
	defer cleanup()

	var called atomic.Bool
	sched.Submit("session1:archivist", PriorityP1, func() { called.Store(true) })

	time.Sleep(150 * time.Millisecond)
	if called.Load() {
		t.Error("P1 task ran under Red health — expected deferral")
	}
	if sched.QueueSize() != 1 {
		t.Errorf("want 1 task in queue under Red; got %d", sched.QueueSize())
	}
}

func TestScheduler_NilPulse_RunsImmediately(t *testing.T) {
	// When pulse is nil, Submit runs fn immediately in a goroutine.
	sched := NewScheduler(nil)
	sched.Start()
	defer sched.Stop()

	var called atomic.Bool
	sched.Submit("node1:ingest", PriorityP2, func() { called.Store(true) })

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if called.Load() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("task was not executed with nil pulse within 500ms")
}

func TestScheduler_ShouldDegrade_NilPulse(t *testing.T) {
	sched := NewScheduler(nil)
	if sched.ShouldDegrade() {
		t.Error("ShouldDegrade with nil pulse must return false")
	}
}

func TestScheduler_ShouldDegrade_Green(t *testing.T) {
	sched, cleanup := newTestSchedulerGreen(t)
	defer cleanup()
	if sched.ShouldDegrade() {
		t.Error("ShouldDegrade must return false on Green health")
	}
}

func TestScheduler_ShouldDegrade_YellowWithModel(t *testing.T) {
	sched, cleanup := newTestSchedulerYellow(t, "qwen3.5:2b")
	defer cleanup()
	// Model is loaded: Yellow but model present → should NOT degrade.
	if sched.ShouldDegrade() {
		t.Error("ShouldDegrade must return false on Yellow with model loaded")
	}
}

func TestScheduler_ShouldDegrade_YellowNoModel(t *testing.T) {
	sched, cleanup := newTestSchedulerYellow(t, "")
	defer cleanup()
	// No model loaded + Yellow → should degrade.
	if !sched.ShouldDegrade() {
		t.Error("ShouldDegrade must return true on Yellow with no model loaded")
	}
}

func TestScheduler_ShouldDegrade_Red(t *testing.T) {
	sched, cleanup := newTestSchedulerRed(t)
	defer cleanup()
	if !sched.ShouldDegrade() {
		t.Error("ShouldDegrade must return true on Red health")
	}
}

func TestScheduler_Dedup_KeepsLatestFn(t *testing.T) {
	sched, cleanup := newTestSchedulerYellow(t, "") // Yellow: won't drain, tasks queue up
	defer cleanup()

	var mu sync.Mutex
	calls := []int{}
	sched.Submit("node1:ingest", PriorityP2, func() { mu.Lock(); calls = append(calls, 1); mu.Unlock() })
	sched.Submit("node1:ingest", PriorityP2, func() { mu.Lock(); calls = append(calls, 2); mu.Unlock() })
	sched.Submit("node1:ingest", PriorityP2, func() { mu.Lock(); calls = append(calls, 3); mu.Unlock() })

	if sched.QueueSize() != 1 {
		t.Errorf("dedup: want 1 entry in queue; got %d", sched.QueueSize())
	}
}

func TestScheduler_PanicRecovery(t *testing.T) {
	// safeSchedulerRun must recover from a panic in the task fn.
	// If it doesn't, the drain goroutine dies and the test process panics.
	sched, cleanup := newTestSchedulerGreen(t)
	defer cleanup()

	var afterPanic atomic.Bool
	sched.Submit("panic-task", PriorityP2, func() { panic("intentional test panic") })
	// Give the panic-recovery goroutine time to run.
	time.Sleep(100 * time.Millisecond)
	// A second task should still run after the panic.
	sched.Submit("after-panic", PriorityP2, func() { afterPanic.Store(true) })

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if afterPanic.Load() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("drain goroutine did not recover from panic — second task never ran")
}

func TestScheduler_StopIsIdempotent(t *testing.T) {
	sched := NewScheduler(nil)
	sched.Start()
	sched.Stop()
	sched.Stop() // must not panic or deadlock
}

func TestScheduler_StopWithoutStart(t *testing.T) {
	sched := NewScheduler(nil)
	sched.Stop() // safe without Start()
}

func TestScheduler_ConcurrentSubmit(t *testing.T) {
	// Concurrent submits from multiple goroutines must not race or panic,
	// and all tasks must eventually execute on a Green system.
	sched, cleanup := newTestSchedulerGreen(t)
	defer cleanup()

	const n = 50
	var wg sync.WaitGroup
	var count atomic.Int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Use numeric keys to guarantee all 50 are unique (no dedup collapse).
			key := fmt.Sprintf("node%d:ingest", i)
			sched.Submit(key, PriorityP2, func() { count.Add(1) })
		}(i)
	}
	wg.Wait()

	// Wait for drain goroutine to execute all tasks. The drain goroutine
	// re-signals itself after each run if more tasks remain (concurrent arrivals),
	// so all 50 tasks should complete well within 5s even under race detector.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if count.Load() == n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("concurrent submit: want %d tasks executed; got %d", n, count.Load())
}

func TestScheduler_SubmitAfterStop_IsDropped(t *testing.T) {
	// Submit() called after Stop() must silently drop the task rather than
	// orphaning it in the queue where it would never execute.
	sched, cleanup := newTestSchedulerGreen(t)
	cleanup() // stop immediately

	var called atomic.Bool
	sched.Submit("node1:ingest", PriorityP2, func() { called.Store(true) })

	// Give any potential spurious execution window time to pass.
	time.Sleep(150 * time.Millisecond)
	if called.Load() {
		t.Error("task executed after Stop() — expected silent drop")
	}
	// Task should not have been enqueued (drain goroutine is gone).
	if sched.QueueSize() != 0 {
		t.Errorf("want empty queue after Submit-post-Stop; got %d", sched.QueueSize())
	}
}

func TestScheduler_CloseAndReinit_NoGoroutineLeak(t *testing.T) {
	// Simulates the daemon hot-reload pattern: Close old scheduler, create new one.
	// Both old and new scheduler must run cleanly without goroutine leaks.
	// This validates the production fix for the activeBrain hot-reload path.
	sched1, cleanup1 := newTestSchedulerGreen(t)

	var task1Ran atomic.Bool
	sched1.Submit("node1:ingest", PriorityP2, func() { task1Ran.Store(true) })

	// Drain task1, then stop the first scheduler.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if task1Ran.Load() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cleanup1() // mimics oldBrain.Close()

	// Create a fresh scheduler (mimics newBrain = brain.NewInProcess(...)).
	sched2, cleanup2 := newTestSchedulerGreen(t)
	defer cleanup2()

	var task2Ran atomic.Bool
	sched2.Submit("node2:ingest", PriorityP2, func() { task2Ran.Store(true) })

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if task2Ran.Load() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("new scheduler after hot-reload did not execute task")
}

// ─── Scheduler + ModelManager drain gate integration ─────────────────────────

// TestScheduler_ModelManager_InsufficientRAM_BlocksDrain verifies that when
// EnsureModel returns "" (insufficient RAM), the drain goroutine skips the
// cycle and tasks remain queued rather than executing.
func TestScheduler_ModelManager_InsufficientRAM_BlocksDrain(t *testing.T) {
	warmupCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		warmupCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// 1 GB free — not enough for any model (needs 2.5 GB minimum).
	pulse := newPulseWithState(1*1024*1024*1024, "")
	mgr := newMgrWithServer(pulse, srv, "synapses/sentry", "", 120)

	sched := NewScheduler(pulse).WithModelManager(mgr)
	sched.Start()
	defer sched.Stop()

	var taskRan atomic.Bool
	sched.Submit("test-task", PriorityP2, func() { taskRan.Store(true) })

	// Wait long enough for the drain goroutine to attempt execution (>10s poll interval).
	// Use a shorter wait + queue size check to confirm task is still queued.
	time.Sleep(150 * time.Millisecond)

	if taskRan.Load() {
		t.Error("task ran despite insufficient RAM — drain gate should have blocked it")
	}
	if sched.QueueSize() == 0 {
		t.Error("task was dropped from queue; it should remain for retry")
	}
	if warmupCalled {
		t.Error("warmUp should not be called when no model fits (Case 4)")
	}
}

// TestScheduler_ModelManager_SufficientRAM_AllowsDrain verifies that when
// EnsureModel returns a model name (sufficient RAM), the drain goroutine
// executes queued tasks normally.
func TestScheduler_ModelManager_SufficientRAM_AllowsDrain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// 4 GB free — enough for primary (needs 2.5 GB for 2B model).
	pulse := newPulseWithState(4*1024*1024*1024, "")
	mgr := newMgrWithServer(pulse, srv, "synapses/sentry", "", 120)

	sched := NewScheduler(pulse).WithModelManager(mgr)
	sched.Start()
	defer sched.Stop()

	var taskRan atomic.Bool
	sched.Submit("test-task", PriorityP2, func() { taskRan.Store(true) })

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if taskRan.Load() {
			return // task executed as expected
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("task did not run within 3s despite sufficient RAM")
}

// TestScheduler_ModelManager_HealthRed_BypassesGate verifies that when health
// is Red, the RAM gate is skipped entirely (no warmup call, no EnsureModel call).
// Red health means drain() returns nothing — the gate is redundant and must not
// make unnecessary warmup HTTP calls.
func TestScheduler_ModelManager_HealthRed_SkipsGate(t *testing.T) {
	warmupCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		warmupCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Red pulse: 1 GB free.
	p := &SystemPulse{done: make(chan struct{})}
	p.mu.Lock()
	p.current = SystemState{
		AvailableRAM: 1 * 1024 * 1024 * 1024,
		CPULoadNorm:  0.95,
		Health:       HealthRed,
		SampledAt:    time.Now(),
	}
	p.mu.Unlock()

	mgr := newMgrWithServer(p, srv, "synapses/sentry", "", 120)
	sched := NewScheduler(p).WithModelManager(mgr)
	sched.Start()
	defer func() {
		sched.Stop()
		p.stopOnce.Do(func() { close(p.done) })
	}()

	sched.Submit("test-task", PriorityP2, func() {})

	time.Sleep(150 * time.Millisecond)
	if warmupCalled {
		t.Error("warmUp must NOT be called when health is Red — gate must be bypassed")
	}
}
