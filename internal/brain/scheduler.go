// Package brain — scheduler.go provides priority-aware dispatch for brain tasks.
//
// The Scheduler routes background work (P1/P2) through a bounded, deduped queue
// executed by a single drain goroutine. This prevents concurrent Ollama requests
// and ensures low-priority background work (file-save ingestion, archivist) does
// not contend with user-waiting operations.
//
// Priority model:
//
//	P0 — NOW: user is waiting (enrich, guardian, HyDE).
//	     Bypass the scheduler entirely. Use ShouldDegrade() to skip the LLM call
//	     when system is Red or Yellow+no-model.
//
//	P1 — SOON: background tasks that should complete this session (archivist, navigator).
//	     Deferred up to 5 min under Yellow/Red health; degraded/dropped after TTL.
//
//	P2 — IDLE: best-effort work that piggybacks on model residency (ingest, bulk).
//	     Deferred up to 15 min under Yellow/Red; silently dropped after TTL.
//
// Queue invariants:
//   - Bounded: at most 100 items. When full, oldest P2 task is evicted first.
//   - Dedup: same key → keep the latest fn only (e.g. file saved 5× → 1 ingest).
//   - TTL: P1 tasks expire after 5 min, P2 after 15 min.
//   - Serial: the single drain goroutine executes one task at a time.
//
// Lifecycle: NewScheduler → Start → (Submit calls) → Stop.
// If pulse is nil, Submit runs fn immediately in a new goroutine (NullBrain / test path).
package brain

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/SynapsesOS/synapses/internal/logutil"
)

// TaskPriority classifies urgency of a brain task relative to system load.
type TaskPriority int

const (
	// PriorityP0 is reserved for user-waiting tasks (enrich, guardian, HyDE).
	// P0 tasks do NOT go through the scheduler queue — they call the brain directly.
	// Use ShouldDegrade() to decide whether to skip the LLM call when under pressure.
	PriorityP0 TaskPriority = 0

	// PriorityP1 tasks run when health is Green, or are deferred up to 5 min
	// under Yellow/Red. Examples: archivist (session end), navigator background.
	PriorityP1 TaskPriority = 1

	// PriorityP2 tasks run when health is Green, or are deferred up to 15 min
	// under Yellow/Red. Examples: ingest (file save), bulk descriptions.
	// P2 tasks are evicted first when the queue is full.
	PriorityP2 TaskPriority = 2
)

const (
	// schedulerQueueMax is the maximum number of deferred tasks in the queue.
	// When full, the oldest P2 task is evicted. If no P2 exists, new tasks are dropped.
	schedulerQueueMax = 100

	// schedulerP1TTL is the maximum deferral window for P1 tasks.
	schedulerP1TTL = 5 * time.Minute

	// schedulerP2TTL is the maximum deferral window for P2 tasks.
	schedulerP2TTL = 15 * time.Minute

	// schedulerDrainInterval is how often the drain goroutine wakes to check
	// health and run eligible tasks, even without an explicit signal.
	schedulerDrainInterval = 10 * time.Second
)

// brainTask is a single deferred unit of brain work.
type brainTask struct {
	key        string       // dedup key; same key keeps the latest fn only
	priority   TaskPriority
	fn         func()       // the work closure; must not be nil
	enqueuedAt time.Time
	ttl        time.Duration
}

// isExpired returns true if the task has outlived its TTL.
func (t *brainTask) isExpired() bool {
	return time.Since(t.enqueuedAt) > t.ttl
}

// deferredQueue is a bounded, priority-ordered task queue with TTL and dedup.
// All exported methods are safe for concurrent use.
type deferredQueue struct {
	mu      sync.Mutex
	tasks   []*brainTask
	maxSize int
}

func newDeferredQueue(maxSize int) *deferredQueue {
	return &deferredQueue{
		tasks:   make([]*brainTask, 0, 16),
		maxSize: maxSize,
	}
}

// add inserts or replaces a task by key (dedup). If the queue is full, the
// oldest P2 task is evicted to make room. If no P2 exists to evict and the
// queue is full, the incoming task is silently dropped and false is returned.
func (q *deferredQueue) add(t *brainTask) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Dedup: replace an existing task with the same key (keep latest fn).
	for i, existing := range q.tasks {
		if existing.key == t.key {
			q.tasks[i] = t
			return true
		}
	}

	// Queue is full: evict the oldest P2 task to make room.
	if len(q.tasks) >= q.maxSize {
		evicted := false
		for i, existing := range q.tasks {
			if existing.priority == PriorityP2 {
				q.tasks = append(q.tasks[:i], q.tasks[i+1:]...)
				evicted = true
				break
			}
		}
		if !evicted {
			// All slots are held by P1 tasks. Drop the incoming task.
			logutil.Warn("brain: scheduler: queue full (max=%d), dropping task %q (priority=%d)\n",
				q.maxSize, t.key, t.priority)
			return false
		}
	}

	q.tasks = append(q.tasks, t)
	return true
}

// drain selects all tasks eligible to run given the current health state,
// removes expired tasks, and returns the eligible tasks sorted by priority
// (P1 before P2) then FIFO within each priority.
//
// Eligibility rules:
//
//	HealthGreen  → all priorities
//	HealthYellow → P1 only
//	HealthRed    → none (all tasks stay in queue)
//
// Expired tasks are silently removed regardless of health.
func (q *deferredQueue) drain(health HealthLevel) []*brainTask {
	q.mu.Lock()
	defer q.mu.Unlock()

	var run []*brainTask
	var keep []*brainTask

	for _, t := range q.tasks {
		if t.isExpired() {
			continue // silently drop expired tasks
		}
		switch health {
		case HealthGreen:
			run = append(run, t)
		case HealthYellow:
			if t.priority <= PriorityP1 {
				run = append(run, t)
			} else {
				keep = append(keep, t)
			}
		case HealthRed:
			keep = append(keep, t)
		}
	}

	// Sort run: P1 before P2, FIFO within same priority.
	sort.Slice(run, func(i, j int) bool {
		if run[i].priority != run[j].priority {
			return run[i].priority < run[j].priority
		}
		return run[i].enqueuedAt.Before(run[j].enqueuedAt)
	})

	q.tasks = keep
	return run
}

// size returns the current number of tasks in the queue.
func (q *deferredQueue) size() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.tasks)
}

// Scheduler routes brain tasks by priority and system health.
//
// P0 tasks (user-waiting) bypass the scheduler — callers use ShouldDegrade()
// to decide whether to skip the LLM call and return a heuristic response.
//
// P1 and P2 tasks go through Submit() and are executed serially by the internal
// drain goroutine. Only one task runs at a time — no concurrent Ollama requests.
//
// When a ModelManager is attached via WithModelManager, the drain goroutine calls
// EnsureModel before executing each batch. If EnsureModel returns "" (insufficient
// RAM to load any model), the batch is skipped and retried on the next tick.
// Tasks remain in the deferred queue — they are not dropped.
//
// Lifecycle: NewScheduler → (optional WithModelManager) → Start → (Submit / ShouldDegrade calls) → Stop.
type Scheduler struct {
	pulse    *SystemPulse
	modelMgr *ModelManager // optional; nil = no RAM gate on drain loop

	queue   *deferredQueue
	drainCh chan struct{} // buffered(1): wakes drain goroutine immediately on Submit when Green

	done      chan struct{}
	wg        sync.WaitGroup
	startOnce sync.Once
	stopOnce  sync.Once

	// prevHealth tracks the last observed health level so drainLoop can detect
	// Yellow/Red → Green transitions and trigger an immediate drain.
	prevHealth HealthLevel
}

// NewScheduler creates a Scheduler. Call Start() before submitting tasks.
//
// If pulse is nil, Submit() runs fn immediately in a new goroutine (useful
// for NullBrain / testing paths where system monitoring is not available).
func NewScheduler(pulse *SystemPulse) *Scheduler {
	return &Scheduler{
		pulse:   pulse,
		queue:   newDeferredQueue(schedulerQueueMax),
		drainCh: make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
}

// WithModelManager attaches a ModelManager to the Scheduler. The drain goroutine
// will call ModelManager.EnsureModel before executing each eligible task batch,
// skipping the batch when no model can be loaded.
//
// Call before Start() to avoid a data race. Returns s for optional chaining:
//
//	sched := NewScheduler(pulse).WithModelManager(mgr)
//	sched.Start()
func (s *Scheduler) WithModelManager(mgr *ModelManager) *Scheduler {
	s.modelMgr = mgr
	return s
}

// Start launches the background drain goroutine. Safe to call multiple times
// — subsequent calls after the first are no-ops (sync.Once).
func (s *Scheduler) Start() {
	s.startOnce.Do(func() {
		s.wg.Add(1)
		go s.drainLoop()
	})
}

// Stop signals the drain goroutine to exit and waits for it to finish.
// Any tasks still in the deferred queue at shutdown time are silently dropped.
// Safe to call multiple times and safe to call without a prior Start().
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() {
		close(s.done)
	})
	s.wg.Wait()
}

// Submit enqueues a P1 or P2 task for deferred execution by the drain goroutine.
//
// If pulse is nil (no system monitoring), fn is run immediately in a new
// goroutine — preserving the fire-and-forget contract for NullBrain / test paths.
//
// Submit never blocks. It always returns immediately, regardless of queue state.
// If the queue is full and cannot evict, the task is silently dropped.
// If Stop() has already been called, the task is silently dropped (drain goroutine
// has exited; enqueuing would orphan the task permanently).
func (s *Scheduler) Submit(key string, priority TaskPriority, fn func()) {
	if s.pulse == nil {
		// No system monitoring — run immediately (NullBrain / test fallback).
		go safeSchedulerRun(key, fn)
		return
	}

	// Guard: if the scheduler has been stopped, the drain goroutine has exited.
	// Enqueuing would silently orphan the task. Drop it instead.
	select {
	case <-s.done:
		return
	default:
	}

	ttl := schedulerP1TTL
	if priority >= PriorityP2 {
		ttl = schedulerP2TTL
	}

	t := &brainTask{
		key:        key,
		priority:   priority,
		fn:         fn,
		enqueuedAt: time.Now(),
		ttl:        ttl,
	}

	if !s.queue.add(t) {
		return // queue full and nothing to evict; task dropped
	}

	// If system is currently Green, signal the drain goroutine to wake immediately.
	if s.pulse.Current().Health == HealthGreen {
		select {
		case s.drainCh <- struct{}{}:
		default: // already signaled; drain goroutine will wake on its next iteration
		}
	}
}

// ShouldDegrade reports whether the current system state warrants skipping
// an LLM call for a P0 (user-waiting) task.
//
// Returns true when:
//   - Health is Red (RAM < 1.5 GB or CPU > 0.9): loading a model would risk OOM.
//   - Health is Yellow AND no model is currently loaded: loading would consume RAM
//     that is already under pressure.
//
// When ShouldDegrade returns true, callers should return a heuristic/template
// response immediately rather than issuing an Ollama request.
func (s *Scheduler) ShouldDegrade() bool {
	if s.pulse == nil {
		return false
	}
	state := s.pulse.Current()
	switch state.Health {
	case HealthRed:
		return true
	case HealthYellow:
		// Only degrade if no model is loaded. When a model is already resident,
		// it costs nothing extra to use it — the RAM pressure is already accepted.
		return state.OllamaModelLoaded == ""
	default:
		return false
	}
}

// QueueSize returns the current number of pending deferred tasks.
// Intended for observability and testing.
func (s *Scheduler) QueueSize() int {
	return s.queue.size()
}

// drainLoop is the single background goroutine. It wakes on an explicit signal
// (s.drainCh), on a 10-second poll interval, or on a Yellow/Red → Green health
// transition, then runs all eligible tasks serially.
func (s *Scheduler) drainLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(schedulerDrainInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-s.drainCh:
			s.runEligible()
		case <-ticker.C:
			// Detect health transitions: Yellow/Red → Green triggers immediate
			// drain of all queued tasks (including P2 piggyback).
			if s.pulse != nil {
				cur := s.pulse.Current().Health
				if s.prevHealth > HealthGreen && cur == HealthGreen && s.queue.size() > 0 {
					logutil.Debug("brain: scheduler: health recovered to Green, draining queued tasks\n")
				}
				s.prevHealth = cur
			}
			s.runEligible()
		}
	}
}

// runEligible drains all eligible tasks from the queue (based on current health)
// and executes them one at a time. After running, if more tasks arrived while
// tasks were executing (concurrent Submit calls), the drain goroutine is
// re-signaled to run immediately rather than waiting for the poll ticker.
// Panics in task fns are recovered so the drain goroutine never dies.
//
// When a ModelManager is attached, runEligible calls EnsureModel before draining.
// If EnsureModel returns "" (insufficient RAM), the drain cycle is skipped —
// eligible tasks remain in the queue and will be retried on the next tick.
func (s *Scheduler) runEligible() {
	health := HealthGreen
	if s.pulse != nil {
		health = s.pulse.Current().Health
	}

	// RAM gate: before draining, verify a model can be loaded.
	// Skipped when health is Red (drain() would return nothing anyway) or
	// when no tasks are queued (avoid unnecessary HTTP warmup calls).
	if s.modelMgr != nil && health != HealthRed && s.queue.size() > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), modelManagerWarmupTimeout)
		selectedModel := s.modelMgr.EnsureModel(ctx)
		cancel() // release context resources immediately; ctx is only needed for the warmup HTTP call
		if selectedModel == "" {
			// Insufficient RAM to load any model — skip this cycle.
			// Tasks stay in the queue; they will be retried on the next tick
			// or expire naturally at their TTL.
			return
		}
		// selectedModel is the model name EnsureModel chose (primary or 2B fallback).
		// Sprint 17 #4 (fallback chains) will thread this value into task dispatch
		// so that task closures use the correct OllamaClient tier.
	}

	tasks := s.queue.drain(health)
	for _, t := range tasks {
		safeSchedulerRun(t.key, t.fn)
	}

	// P2 piggyback: when we loaded a model for P1 tasks under Yellow health,
	// the model is now resident. Drain P2 tasks immediately while the model is
	// still warm — this avoids waiting for the next Green tick or keep_alive expiry.
	if health == HealthYellow && len(tasks) > 0 && s.queue.size() > 0 {
		piggyback := s.queue.drain(HealthGreen) // Green eligibility → includes P2
		for _, t := range piggyback {
			safeSchedulerRun(t.key, t.fn)
		}
	}

	// If more tasks appeared during task execution (e.g. concurrent file saves),
	// and health is Green, re-signal immediately rather than waiting for the ticker.
	// This avoids a 10-second delay when bursts of work arrive concurrently.
	// Guard on Green only: under Yellow/Red, the tasks will drain on next health
	// improvement (via ticker). A spin loop under Red/Yellow would waste CPU.
	if health == HealthGreen && s.queue.size() > 0 {
		select {
		case s.drainCh <- struct{}{}:
		default: // signal already pending; drain goroutine will pick it up
		}
	}
}

// safeSchedulerRun executes fn, recovering from any panic so the drain
// goroutine continues running. Panics are logged as errors.
func safeSchedulerRun(key string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			logutil.Error("brain: scheduler: panic in task %q: %v\n", key, r)
		}
	}()
	fn()
}
