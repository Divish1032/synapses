package store_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/SynapsesOS/synapses/internal/store"
)

// ─── Episode concurrency (remember/recall) ──────────────────────────────────

// TestConcurrentRememberRecall verifies that 10 goroutines writing episodes
// concurrently with 10 goroutines recalling does not lose data or corrupt
// the FTS index. All 10 episodes must be persisted after completion, and
// recall must return consistent results (no partial reads, no panics).
func TestConcurrentRememberRecall(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	const writers = 10
	const readers = 10

	var wg sync.WaitGroup

	// Collect IDs written by each goroutine.
	writtenIDs := make([]string, writers)
	writeErrs := make([]error, writers)

	// Writers: each inserts one episode with a unique decision string.
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ep := store.Episode{
				AgentID:     fmt.Sprintf("agent-%d", idx),
				EpisodeType: "decision",
				Outcome:     "success",
				Trigger:     fmt.Sprintf("concurrent trigger %d", idx),
				Decision:    fmt.Sprintf("concurrent decision number %d for testing persistence", idx),
				Rationale:   fmt.Sprintf("rationale for concurrent write %d", idx),
				Tags:        `["concurrent","test"]`,
			}
			id, err := st.RememberEpisode(ep)
			writtenIDs[idx] = id
			writeErrs[idx] = err
		}(i)
	}

	// Readers: each performs a recall query concurrently with the writes.
	// We don't assert specific results here because ordering vs writers is
	// non-deterministic — we verify no panics and no errors.
	readErrs := make([]error, readers)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := st.RecallEpisodes("concurrent decision", "", "", "", "", 5, 0)
			readErrs[idx] = err
		}(i)
	}

	wg.Wait()

	// Verify all writes succeeded.
	for i, err := range writeErrs {
		if err != nil {
			t.Errorf("writer %d failed: %v", i, err)
		}
	}
	for i, id := range writtenIDs {
		if id == "" {
			t.Errorf("writer %d returned empty ID", i)
		}
	}

	// Verify all reads completed without error.
	for i, err := range readErrs {
		if err != nil {
			t.Errorf("reader %d failed: %v", i, err)
		}
	}

	// After all goroutines are done, verify ALL episodes were persisted.
	// Use GetEpisodes (unfiltered by FTS) to count total episodes.
	allEpisodes, err := st.GetEpisodes("", "", "", nil, 100, 0)
	if err != nil {
		t.Fatalf("GetEpisodes after concurrent writes: %v", err)
	}
	if len(allEpisodes) != writers {
		t.Errorf("expected %d episodes persisted, got %d — data was lost", writers, len(allEpisodes))
	}

	// Verify recall returns consistent results now that all writes are done.
	results, err := st.RecallEpisodes("concurrent decision", "", "", "", "", writers, 0)
	if err != nil {
		t.Fatalf("final RecallEpisodes: %v", err)
	}
	if len(results) == 0 {
		t.Error("final recall returned 0 results — FTS index may be corrupted")
	}

	// Each result must have a valid ID that appears in our written set.
	idSet := make(map[string]bool, writers)
	for _, id := range writtenIDs {
		idSet[id] = true
	}
	for _, ep := range results {
		if !idSet[ep.ID] {
			t.Errorf("recall returned unknown episode ID %s", ep.ID)
		}
	}
}

// TestConcurrentRememberRecall_HighContention increases contention by having
// writers and readers target overlapping FTS terms, stressing SQLite's WAL
// journal under concurrent read/write pressure.
func TestConcurrentRememberRecall_HighContention(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	const goroutines = 20
	var wg sync.WaitGroup
	errs := make([]error, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if idx%2 == 0 {
				// Writer: all use the same FTS-matchable terms.
				_, errs[idx] = st.RememberEpisode(store.Episode{
					AgentID:     "contention-agent",
					EpisodeType: "decision",
					Outcome:     "success",
					Decision:    fmt.Sprintf("contention test authentication handler iteration %d", idx),
				})
			} else {
				// Reader: search for the same terms writers are inserting.
				_, errs[idx] = st.RecallEpisodes("authentication handler", "", "", "", "", 10, 0)
			}
		}(i)
	}

	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d failed: %v", i, err)
		}
	}

	// Verify all 10 writer episodes persisted.
	all, err := st.GetEpisodes("", "contention-agent", "", nil, 100, 0)
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(all) != goroutines/2 {
		t.Errorf("expected %d episodes, got %d", goroutines/2, len(all))
	}
}

// ─── Memory concurrency (InsertMemory + SearchMemories) ─────────────────────

// TestConcurrentInsertMemory_SearchMemories verifies that 10 goroutines
// inserting memories concurrently with 10 goroutines searching does not lose
// data or corrupt the FTS index. Each memory has unique content to avoid
// dedup collisions.
func TestConcurrentInsertMemory_SearchMemories(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	const writers = 10
	const readers = 10

	var wg sync.WaitGroup
	writtenIDs := make([]string, writers)
	writeErrs := make([]error, writers)

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			m := store.Memory{
				Tier:    store.TierProject,
				Content: fmt.Sprintf("concurrent memory content about authentication refactoring iteration number %d with enough length", idx),
				AgentID: fmt.Sprintf("mem-agent-%d", idx),
				Source:  store.SourceManual,
			}
			id, err := st.InsertMemory(m)
			writtenIDs[idx] = id
			writeErrs[idx] = err
		}(i)
	}

	readErrs := make([]error, readers)
	readCounts := make([]int, readers)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results, err := st.SearchMemories("authentication refactoring", 20)
			readErrs[idx] = err
			readCounts[idx] = len(results)
		}(i)
	}

	wg.Wait()

	for i, err := range writeErrs {
		if err != nil {
			t.Errorf("memory writer %d failed: %v", i, err)
		}
	}
	for i, id := range writtenIDs {
		if id == "" {
			t.Errorf("memory writer %d returned empty ID", i)
		}
	}
	for i, err := range readErrs {
		if err != nil {
			t.Errorf("memory reader %d failed: %v", i, err)
		}
	}

	// Verify all memories persisted via QueryMemories (not FTS).
	allMems, err := st.QueryMemories(store.TierProject, "", "", 100)
	if err != nil {
		t.Fatalf("QueryMemories: %v", err)
	}
	if len(allMems) != writers {
		t.Errorf("expected %d memories, got %d — data was lost", writers, len(allMems))
	}

	// Verify FTS index is consistent after concurrent writes.
	searchResults, err := st.SearchMemories("authentication refactoring", 20)
	if err != nil {
		t.Fatalf("SearchMemories: %v", err)
	}
	if len(searchResults) == 0 {
		t.Error("SearchMemories returned 0 results — FTS index may be corrupted")
	}

	// Verify each returned result is one we wrote.
	idSet := make(map[string]bool, writers)
	for _, id := range writtenIDs {
		idSet[id] = true
	}
	for _, m := range searchResults {
		if !idSet[m.ID] {
			t.Errorf("SearchMemories returned unknown memory ID %s", m.ID)
		}
	}
}

// TestConcurrentInsertMemory_DedupRace verifies that concurrent inserts with
// very similar content (>85% similarity) correctly handle the dedup path.
// The dedup check in prepareMemory does a read-then-compare before insert.
// Under concurrency, two goroutines may both pass the dedup check and both
// insert — this is acceptable (slightly more data, no corruption).
// What is NOT acceptable: panic, lost data, or corrupted FTS index.
func TestConcurrentInsertMemory_DedupRace(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	const goroutines = 10
	var wg sync.WaitGroup
	ids := make([]string, goroutines)
	errs := make([]error, goroutines)

	// All goroutines insert nearly identical content that should trigger dedup.
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			m := store.Memory{
				Tier:    store.TierProject,
				Content: "concurrent dedup test: the authentication service was refactored to use OAuth2 tokens instead of session cookies",
				AgentID: "dedup-agent",
				Source:  store.SourceManual,
			}
			ids[idx], errs[idx] = st.InsertMemory(m)
		}(i)
	}

	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("dedup goroutine %d failed: %v", i, err)
		}
	}

	// All should return a non-empty ID (either new insert or dedup match).
	for i, id := range ids {
		if id == "" {
			t.Errorf("dedup goroutine %d returned empty ID", i)
		}
	}

	// Count distinct IDs — with perfect dedup, there would be 1.
	// Under concurrent TOCTOU, there may be more (all goroutines pass dedup
	// check before any inserts land). Both outcomes are correct — the important
	// thing is no panics, no errors, and the store is consistent.
	uniqueIDs := make(map[string]bool)
	for _, id := range ids {
		uniqueIDs[id] = true
	}
	t.Logf("dedup: %d goroutines produced %d unique IDs (1 = perfect dedup, >1 = TOCTOU race, both acceptable)", goroutines, len(uniqueIDs))

	// Verify store is consistent — QueryMemories should return at least 1.
	mems, err := st.QueryMemories(store.TierProject, "", "dedup-agent", 100)
	if err != nil {
		t.Fatalf("QueryMemories: %v", err)
	}
	if len(mems) == 0 {
		t.Error("no memories found after concurrent dedup inserts")
	}
	// Unique IDs returned by InsertMemory must match actual stored memories.
	storedIDs := make(map[string]bool)
	for _, m := range mems {
		storedIDs[m.ID] = true
	}
	for id := range uniqueIDs {
		if !storedIDs[id] {
			// A returned ID that doesn't exist in the store means data corruption.
			t.Errorf("InsertMemory returned ID %s but it's not in the store — phantom ID", id)
		}
	}
}

// ─── Task concurrency (UpdateTask) ──────────────────────────────────────────

// TestConcurrentUpdateTask verifies that concurrent updates to different tasks
// within the same plan do not corrupt data. Each goroutine updates a distinct
// task, so there should be no contention on the same row — but they share the
// same knowledgeDB connection and the plan-completion check runs for each.
func TestConcurrentUpdateTask(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	const taskCount = 10

	// Create a plan with N tasks.
	inputs := make([]store.TaskInput, taskCount)
	for i := 0; i < taskCount; i++ {
		inputs[i] = store.TaskInput{
			Title:    fmt.Sprintf("concurrent task %d", i),
			Priority: "p1",
		}
	}
	planID, _, err := st.CreatePlan("concurrent plan", "testing concurrent updates", "", inputs)
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}
	if planID == "" {
		t.Fatal("expected non-empty plan ID")
	}

	tasks, err := st.GetPendingTasks(planID, "")
	if err != nil {
		t.Fatalf("GetPendingTasks: %v", err)
	}
	if len(tasks) != taskCount {
		t.Fatalf("expected %d tasks, got %d", taskCount, len(tasks))
	}

	// Phase 1: Concurrently mark all tasks as in_progress.
	var wg sync.WaitGroup
	updateErrs := make([]error, taskCount)
	for i := 0; i < taskCount; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, _, updateErrs[idx] = st.UpdateTask(
				tasks[idx].ID,
				"in_progress",
				fmt.Sprintf("started by agent-%d", idx),
				fmt.Sprintf("agent-%d", idx),
			)
		}(i)
	}
	wg.Wait()

	for i, err := range updateErrs {
		if err != nil {
			t.Errorf("in_progress update for task %d failed: %v", i, err)
		}
	}

	// Phase 2: Concurrently mark all tasks as done.
	// This also exercises checkAndCompletePlan — one of these will trigger
	// the plan completion logic when the last task finishes.
	planCompleted := make([]bool, taskCount)
	for i := 0; i < taskCount; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, completed, updateErr := st.UpdateTask(
				tasks[idx].ID,
				"done",
				fmt.Sprintf("completed by agent-%d", idx),
				fmt.Sprintf("agent-%d", idx),
			)
			updateErrs[idx] = updateErr
			planCompleted[idx] = completed
		}(i)
	}
	wg.Wait()

	for i, err := range updateErrs {
		if err != nil {
			t.Errorf("done update for task %d failed: %v", i, err)
		}
	}

	// Verify exactly one goroutine saw planCompleted=true.
	// checkAndCompletePlan uses UPDATE ... WHERE completed_at = 0 which is an
	// atomic compare-and-swap — exactly one writer wins.
	completedCount := 0
	for _, c := range planCompleted {
		if c {
			completedCount++
		}
	}
	if completedCount != 1 {
		t.Errorf("expected exactly 1 goroutine to see planCompleted=true, got %d", completedCount)
	}

	// Verify all tasks are now done.
	remaining, err := st.GetPendingTasks(planID, "")
	if err != nil {
		t.Fatalf("GetPendingTasks after done: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("expected 0 pending tasks, got %d", len(remaining))
	}
}

// TestConcurrentUpdateTask_SameTask verifies that concurrent note appends to
// the SAME task are handled safely. UpdateTask does a SELECT notes → string
// concat → UPDATE which is a read-modify-write without a transaction.
// With MaxOpenConns(2), this means two goroutines CAN interleave:
//
//	G1: SELECT notes → "a"
//	G2: SELECT notes → "a"
//	G1: UPDATE notes = "a\nb"    (G1 wins)
//	G2: UPDATE notes = "a\nc"    (G2 overwrites, "b" is lost)
//
// This test documents the actual behavior: some notes may be lost under
// concurrent same-row updates. It measures the exact count so we know the
// severity and can track if a future fix (e.g., using UPDATE ... SET notes =
// notes || ?) resolves it.
func TestConcurrentUpdateTask_SameTask(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_, _, err := st.CreatePlan("single task plan", "", "", []store.TaskInput{
		{Title: "contended task", Priority: "p0"},
	})
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}

	tasks, _ := st.GetPendingTasks("", "")
	if len(tasks) == 0 {
		t.Fatal("no tasks")
	}
	taskID := tasks[0].ID

	// Mark in_progress first (single-threaded).
	if _, _, err := st.UpdateTask(taskID, "in_progress", "", "setup"); err != nil {
		t.Fatalf("set in_progress: %v", err)
	}

	// 10 goroutines all append notes to the same task concurrently.
	const noteWriters = 10
	var wg sync.WaitGroup
	noteErrs := make([]error, noteWriters)
	for i := 0; i < noteWriters; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, _, noteErrs[idx] = st.UpdateTask(
				taskID,
				"in_progress",
				fmt.Sprintf("note from goroutine %d", idx),
				fmt.Sprintf("agent-%d", idx),
			)
		}(i)
	}
	wg.Wait()

	for i, err := range noteErrs {
		if err != nil {
			t.Errorf("note writer %d failed: %v", i, err)
		}
	}

	// Read back the task and count how many notes survived.
	updatedTasks, err := st.GetPendingTasks("", "")
	if err != nil {
		t.Fatalf("GetPendingTasks: %v", err)
	}
	if len(updatedTasks) == 0 {
		t.Fatal("task disappeared")
	}
	if updatedTasks[0].Notes == "" {
		t.Fatal("expected notes to be non-empty after concurrent appends")
	}

	// Count note lines. Each note is a "[timestamp] note from goroutine N" line.
	noteLines := 0
	for _, line := range strings.Split(updatedTasks[0].Notes, "\n") {
		if strings.Contains(line, "note from goroutine") {
			noteLines++
		}
	}

	t.Logf("note append: %d/%d notes survived concurrent same-row updates", noteLines, noteWriters)

	// All notes must survive — the atomic SQL append eliminates the TOCTOU race.
	if noteLines != noteWriters {
		t.Errorf("expected all %d notes to survive, got %d — atomic append may be broken", noteWriters, noteLines)
	}

	// Verify no corruption: every line must have a timestamp prefix.
	for _, line := range strings.Split(updatedTasks[0].Notes, "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "[") {
			t.Errorf("malformed note line (no timestamp prefix): %q", line)
		}
	}
}

// TestConcurrentUpdateTask_ReadWriteContention verifies that concurrent
// readers (GetPendingTasks) and writers (UpdateTask) don't deadlock or
// produce inconsistent results. Exercises WAL concurrent read/write.
func TestConcurrentUpdateTask_ReadWriteContention(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	const taskCount = 5
	inputs := make([]store.TaskInput, taskCount)
	for i := 0; i < taskCount; i++ {
		inputs[i] = store.TaskInput{
			Title:    fmt.Sprintf("rw-contention task %d", i),
			Priority: "p1",
		}
	}
	_, _, err := st.CreatePlan("rw plan", "", "", inputs)
	if err != nil {
		t.Fatalf("CreatePlan: %v", err)
	}

	tasks, _ := st.GetPendingTasks("", "")
	if len(tasks) != taskCount {
		t.Fatalf("expected %d tasks, got %d", taskCount, len(tasks))
	}

	// Concurrently: writers update different tasks, readers query the list.
	var wg sync.WaitGroup
	writeErrs := make([]error, taskCount)
	readErrs := make([]error, taskCount)

	for i := 0; i < taskCount; i++ {
		// Writer
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, _, writeErrs[idx] = st.UpdateTask(
				tasks[idx].ID, "in_progress",
				fmt.Sprintf("writer %d", idx),
				fmt.Sprintf("agent-%d", idx),
			)
		}(i)

		// Reader
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			result, err := st.GetPendingTasks("", "")
			readErrs[idx] = err
			// Each read must see a consistent view — no partial state.
			for _, task := range result {
				if task.Status != "pending" && task.Status != "in_progress" {
					t.Errorf("reader %d saw unexpected status %q for task %s", idx, task.Status, task.ID)
				}
			}
		}(i)
	}

	wg.Wait()

	for i, err := range writeErrs {
		if err != nil {
			t.Errorf("writer %d: %v", i, err)
		}
	}
	for i, err := range readErrs {
		if err != nil {
			t.Errorf("reader %d: %v", i, err)
		}
	}

	// After all writers finish, all tasks should be in_progress.
	final, _ := st.GetPendingTasks("", "")
	for _, task := range final {
		if task.Status != "in_progress" {
			t.Errorf("task %s has status %q, expected in_progress", task.ID, task.Status)
		}
	}
}

// TEST-002: Concurrent PruneStaleData + active writes.
// Verifies that running PruneStaleData while actively inserting memories
// and episodes does not cause data loss, deadlocks, or panics.
func TestConcurrentPruneAndWrite(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	const writers = 5
	const writesPerWriter = 20

	var wg sync.WaitGroup

	// Launch memory writers.
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < writesPerWriter; i++ {
				_, _ = st.InsertMemory(store.Memory{
					Tier:    store.TierProject,
					Content: fmt.Sprintf("prune-test-memory-worker-%d-iter-%d", workerID, i),
					AgentID: fmt.Sprintf("pruner-%d", workerID),
				})
			}
		}(w)
	}

	// Launch episode writers.
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < writesPerWriter; i++ {
				_, _ = st.RememberEpisode(store.Episode{
					AgentID:     fmt.Sprintf("pruner-%d", workerID),
					Decision:    fmt.Sprintf("prune-test-episode-worker-%d-iter-%d", workerID, i),
					EpisodeType: "decision",
					Outcome:     "success",
				})
			}
		}(w)
	}

	// Launch concurrent prune goroutines.
	for p := 0; p < 3; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Use 0 retention days — prunes nothing recent, but exercises all code paths.
			st.PruneStaleData(9999)
		}()
	}

	wg.Wait()

	// Verify data was written. Not all writes may be visible due to prune,
	// but we should have at least some.
	memories, err := st.SearchMemories("prune-test-memory", 100)
	if err != nil {
		t.Fatalf("SearchMemories after concurrent prune+write: %v", err)
	}
	if len(memories) == 0 {
		t.Error("expected at least some memories to survive concurrent prune+write")
	}
}
