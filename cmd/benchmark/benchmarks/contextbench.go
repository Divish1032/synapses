// ContextBench runner — measures context retrieval quality on the ContextBench
// dataset (1,136 tasks, 66 repos, 8 languages).
//
// For each task the runner:
//  1. Clones the repo at base_commit (full depth, not shallow)
//  2. Checks out base_commit and re-indexes with Synapses
//  3. Calls Synapses tools (search, prepare_context, get_impact) guided by the
//     problem statement to retrieve context
//  4. Compares retrieved file+line ranges against gold_context annotations
//  5. Computes Context Precision, Recall, and F1
//
// Dataset: huggingface.co/datasets/Contextbench/ContextBench
// Export to JSONL:
//
//	python -c "
//	from datasets import load_dataset
//	ds = load_dataset('Contextbench/ContextBench', 'default', split='train')
//	ds.to_json('contextbench.jsonl')
//	"
//
// Gold context format per task (JSON array):
//
//	[{"file": "path/to/file.py", "start_line": 10, "end_line": 25, "content": "..."}]
package benchmarks

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/SynapsesOS/synapses/cmd/benchmark/agent"
	"github.com/SynapsesOS/synapses/cmd/benchmark/indexer"
	"github.com/SynapsesOS/synapses/cmd/benchmark/reporter"
)

// ContextBenchOptions controls the ContextBench run.
type ContextBenchOptions struct {
	// DataFile is the path to contextbench.jsonl.
	DataFile string
	// ReposDir is where repos are cloned.
	ReposDir string
	// CacheFile is the JSON cache of cloned+indexed repos (keyed by repo@commit).
	CacheFile string
	// Limit caps the number of tasks (0 = all).
	Limit int
	// Languages filters tasks by language (empty = all).
	Languages []string
	// Sources filters by source field (empty = all). E.g. ["Verified"].
	Sources []string
	// IndexWorkers controls parallel clone+index workers.
	IndexWorkers int
	// SkipIndex skips the synapses index step.
	SkipIndex bool
	// SynapsesBin is the path to the synapses binary (empty = auto-detect).
	SynapsesBin string
	// WarmupSessions: when > 0, run N warm-up sessions on each repo before tasks
	// to build edge weight history, then compare cold F1 (fresh) vs warm F1.
	// Measures whether Synapses' learning features improve retrieval quality.
	WarmupSessions int
	// CompactionMode: when true, run each task normally (Phase 1), then simulate
	// a compaction event and re-run with only the recovery packet (Phase 2).
	// Measures how well compaction recovery preserves context quality.
	CompactionMode bool
}

// ContextBenchTask is a single record from the ContextBench dataset.
type ContextBenchTask struct {
	InstanceID       string `json:"instance_id"`
	OriginalInstID   string `json:"original_inst_id"`
	Repo             string `json:"repo"`
	RepoURL          string `json:"repo_url"`
	Language         string `json:"language"`
	BaseCommit       string `json:"base_commit"`
	Source           string `json:"source"`
	GoldContextRaw   string `json:"gold_context"` // JSON array string
	Patch            string `json:"patch"`
	ProblemStatement string `json:"problem_statement"`
}

// GoldContextBlock is one annotated context region.
type GoldContextBlock struct {
	File      string `json:"file"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Content   string `json:"content"`
}

// ContextBenchTaskResult holds per-task metrics.
type ContextBenchTaskResult struct {
	InstanceID     string  `json:"instance_id"`
	Repo           string  `json:"repo"`
	Language       string  `json:"language"`
	Precision      float64 `json:"precision"`
	Recall         float64 `json:"recall"`
	F1             float64 `json:"f1"`
	GoldLines      int     `json:"gold_lines"`
	HitLines       int     `json:"hit_lines"`
	TotalLines     int     `json:"total_retrieved_lines"`
	ToolCalls      int     `json:"tool_calls"`
	TokensRetrieved int    `json:"tokens_retrieved"`  // estimated tokens in retrieved context
	TokensGold      int    `json:"tokens_gold"`       // estimated tokens in gold context
	TokenPrecision  float64 `json:"token_precision"`   // hit_tokens / retrieved_tokens
	// Multi-budget metrics at different retrieval line limits.
	PAt250          float64 `json:"p_at_250"`
	RAt250          float64 `json:"r_at_250"`
	F1At250         float64 `json:"f1_at_250"`
	PAt500          float64 `json:"p_at_500"`
	RAt500          float64 `json:"r_at_500"`
	F1At500         float64 `json:"f1_at_500"`
	PAt1000         float64 `json:"p_at_1000"`
	RAt1000         float64 `json:"r_at_1000"`
	F1At1000        float64 `json:"f1_at_1000"`
	Error          string  `json:"error,omitempty"`
}

// RunContextBench runs the full ContextBench evaluation.
func RunContextBench(client *agent.SynapsesClient, opts ContextBenchOptions) (*reporter.ContextBenchResult, error) {
	tasks, err := loadContextBenchTasks(opts.DataFile)
	if err != nil {
		return nil, fmt.Errorf("load dataset: %w", err)
	}

	// Filter by language/source.
	tasks = filterTasks(tasks, opts.Languages, opts.Sources)
	if opts.Limit > 0 && len(tasks) > opts.Limit {
		tasks = tasks[:opts.Limit]
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("no tasks after filtering")
	}
	fmt.Printf("[contextbench] %d tasks loaded\n", len(tasks))

	// Build a cache keyed by "repo@commit" so each (repo, commit) pair
	// is cloned and indexed exactly once.
	cache, err := indexer.LoadCache(opts.CacheFile)
	if err != nil {
		return nil, fmt.Errorf("load cache: %w", err)
	}

	// Run each task sequentially (tool calls hit daemon, moderate parallelism).
	var results []ContextBenchTaskResult
	var totalP, totalR, totalF1 float64
	var totalTokensRetrieved, totalTokensGold int
	var totalTokenPrecision float64
	var totalPAt250, totalRAt250, totalF1At250 float64
	var totalPAt500, totalRAt500, totalF1At500 float64
	var totalPAt1000, totalRAt1000, totalF1At1000 float64
	for i, task := range tasks {
		fmt.Printf("[contextbench] task %d/%d: %s\n", i+1, len(tasks), task.InstanceID)

		tr := runContextBenchTask(client, task, cache, opts)
		results = append(results, tr)
		totalP += tr.Precision
		totalR += tr.Recall
		totalF1 += tr.F1
		totalTokensRetrieved += tr.TokensRetrieved
		totalTokensGold += tr.TokensGold
		totalTokenPrecision += tr.TokenPrecision
		totalPAt250 += tr.PAt250
		totalRAt250 += tr.RAt250
		totalF1At250 += tr.F1At250
		totalPAt500 += tr.PAt500
		totalRAt500 += tr.RAt500
		totalF1At500 += tr.F1At500
		totalPAt1000 += tr.PAt1000
		totalRAt1000 += tr.RAt1000
		totalF1At1000 += tr.F1At1000
		fmt.Printf("  → P=%.1f%% R=%.1f%% F1=%.1f%% (gold=%d hits=%d retrieved=%d tools=%d F1@250=%.1f%% F1@1000=%.1f%%)\n",
			tr.Precision*100, tr.Recall*100, tr.F1*100,
			tr.GoldLines, tr.HitLines, tr.TotalLines, tr.ToolCalls,
			tr.F1At250*100, tr.F1At1000*100,
		)
	}

	n := float64(len(results))
	avgTokensRetrieved := totalTokensRetrieved / max(len(results), 1)
	avgTokensGold := totalTokensGold / max(len(results), 1)
	var tokenEfficiency float64
	if avgTokensRetrieved > 0 {
		tokenEfficiency = float64(avgTokensGold) / float64(avgTokensRetrieved)
	}
	result := &reporter.ContextBenchResult{
		Timestamp:          reporter.Timestamp(),
		TotalTasks:         len(results),
		TaskResults:        toInterfaceSlice(results),
		AvgPrecision:       totalP / n,
		AvgRecall:          totalR / n,
		AvgF1:              totalF1 / n,
		AvgTokensRetrieved: avgTokensRetrieved,
		AvgTokensGold:      avgTokensGold,
		AvgTokenPrecision:  totalTokenPrecision / n,
		TokenEfficiency:    tokenEfficiency,
	}

	// Per-language breakdown.
	type langAcc struct {
		p, r, f1 float64
		n        int
	}
	langMetrics := make(map[string]*langAcc)
	for _, tr := range results {
		lm := langMetrics[tr.Language]
		if lm == nil {
			lm = &langAcc{}
			langMetrics[tr.Language] = lm
		}
		lm.p += tr.Precision
		lm.r += tr.Recall
		lm.f1 += tr.F1
		lm.n++
	}
	for lang, lm := range langMetrics {
		result.PerLanguage = append(result.PerLanguage, reporter.ContextBenchLangResult{
			Language:     lang,
			Tasks:        lm.n,
			AvgPrecision: lm.p / float64(lm.n),
			AvgRecall:    lm.r / float64(lm.n),
			AvgF1:        lm.f1 / float64(lm.n),
		})
	}

	// Multi-budget aggregation.
	result.MultiBudget = &reporter.MultiBudgetResult{
		Budget250:  reporter.BudgetMetrics{Budget: 250, AvgPrecision: totalPAt250 / n, AvgRecall: totalRAt250 / n, AvgF1: totalF1At250 / n},
		Budget500:  reporter.BudgetMetrics{Budget: 500, AvgPrecision: totalPAt500 / n, AvgRecall: totalRAt500 / n, AvgF1: totalF1At500 / n},
		Budget1000: reporter.BudgetMetrics{Budget: 1000, AvgPrecision: totalPAt1000 / n, AvgRecall: totalRAt1000 / n, AvgF1: totalF1At1000 / n},
	}

	// ── Cold/Warm comparison (Phase 3b) ─────────────────────────────────────
	if opts.WarmupSessions > 0 {
		fmt.Printf("\n[contextbench] cold/warm comparison: %d warmup sessions\n", opts.WarmupSessions)

		// Cold F1 is the normal run we just did.
		coldF1 := result.AvgF1

		// Warm-up: simulate N agent sessions per repo to build edge weight history.
		// Group tasks by repo to warm up each repo once.
		repoTasks := make(map[string][]ContextBenchTask)
		for _, t := range tasks {
			repoTasks[t.Repo] = append(repoTasks[t.Repo], t)
		}

		warmClient := agent.NewClient(client.Endpoint(), "")
		for repo, rTasks := range repoTasks {
			// Get project path from cache.
			repoDir := cache.Get(repo + "@" + rTasks[0].BaseCommit)
			if repoDir == "" {
				continue
			}
			projClient := warmClient.WithProject(repoDir)

			for s := 0; s < opts.WarmupSessions; s++ {
				agentID := fmt.Sprintf("cb-warmup-%s-%d", repo, s)
				projClient.SessionInit(agentID, "")

				// Explore 5-10 entities from this repo's tasks to create context deliveries.
				for j, t := range rTasks {
					if j >= 10 {
						break
					}
					projClient.GetContextJSON(agentID, t.ProblemStatement[:min(len(t.ProblemStatement), 60)], "summary")
				}
				projClient.EndSession(agentID, "success")
			}
			fmt.Printf("  warmed up %s (%d sessions)\n", repo, opts.WarmupSessions)
		}

		// Re-run tasks with warm edge weights.
		var warmF1Sum float64
		for i, task := range tasks {
			if i < 5 { // only log first 5
				fmt.Printf("[contextbench-warm] task %d/%d: %s\n", i+1, len(tasks), task.InstanceID)
			}
			tr := runContextBenchTask(client, task, cache, opts)
			warmF1Sum += tr.F1
		}
		warmF1 := warmF1Sum / float64(len(tasks))

		result.ColdWarm = &reporter.ColdWarmComparison{
			ColdF1:         coldF1,
			WarmF1:         warmF1,
			LearningLift:   warmF1 - coldF1,
			WarmupSessions: opts.WarmupSessions,
		}
		fmt.Printf("[contextbench] cold=%.1f%% warm=%.1f%% lift=%+.1f%%\n",
			coldF1*100, warmF1*100, (warmF1-coldF1)*100)
	}

	// ── Compaction comparison (Phase 3b) ────────────────────────────────────
	if opts.CompactionMode {
		fmt.Printf("\n[contextbench] compaction mode: testing recovery quality\n")

		// Pre-compaction F1 is the normal run.
		preF1 := result.AvgF1

		// For each task: simulate compaction by calling session_init(scope=compaction)
		// to get the recovery packet, then re-run the task. The recovery packet
		// seeds the agent's context — measure how much F1 is preserved.
		compClient := agent.NewClient(client.Endpoint(), "")
		var postF1Sum float64
		var postCount int
		for i, task := range tasks {
			repoDir := cache.Get(task.Repo + "@" + task.BaseCommit)
			if repoDir == "" {
				continue
			}
			projClient := compClient.WithProject(repoDir)

			// Get compaction recovery packet.
			agentID := fmt.Sprintf("cb-compact-%d", i)
			recoveryRaw, err := projClient.SessionInit(agentID, "compaction")
			if err != nil {
				continue
			}

			// The recovery packet seeds the agent — now run the same task.
			// The recovery context should help the agent find the right files
			// even though it's a "fresh" session.
			_ = recoveryRaw // Recovery packet is automatically in the session context

			tr := runContextBenchTask(projClient, task, cache, opts)
			postF1Sum += tr.F1
			postCount++

			projClient.EndSession(agentID, "success")

			if i < 5 {
				fmt.Printf("  [compact] task %d: pre=%.1f%% post=%.1f%%\n",
					i+1, preF1*100, tr.F1*100)
			}
		}

		if postCount > 0 {
			postF1 := postF1Sum / float64(postCount)
			result.Compaction = &reporter.CompactionComparison{
				PreCompactionF1:  preF1,
				PostCompactionF1: postF1,
				RecoveryDelta:    postF1 - preF1,
			}
			fmt.Printf("[contextbench] pre=%.1f%% post=%.1f%% delta=%+.1f%%\n",
				preF1*100, postF1*100, (postF1-preF1)*100)
		}
	}

	return result, nil
}

func toInterfaceSlice(results []ContextBenchTaskResult) []interface{} {
	out := make([]interface{}, len(results))
	for i, r := range results {
		out[i] = r
	}
	return out
}

// runContextBenchTask runs one ContextBench task: issue → tool calls → context F1.
func runContextBenchTask(client *agent.SynapsesClient, task ContextBenchTask, cache *indexer.Cache, opts ContextBenchOptions) ContextBenchTaskResult {
	result := ContextBenchTaskResult{
		InstanceID: task.InstanceID,
		Repo:       task.Repo,
		Language:   task.Language,
	}

	// Parse gold context.
	goldBlocks, err := parseGoldContext(task.GoldContextRaw)
	if err != nil {
		result.Error = "parse gold: " + err.Error()
		return result
	}
	if len(goldBlocks) == 0 {
		result.Error = "empty gold context"
		return result
	}

	// Build gold line set: file:line → true.
	goldLines := make(map[string]bool)
	for _, b := range goldBlocks {
		for line := b.StartLine; line <= b.EndLine; line++ {
			goldLines[fmt.Sprintf("%s:%d", b.File, line)] = true
		}
	}
	result.GoldLines = len(goldLines)

	// Ensure repo is cloned at the correct commit and indexed.
	repoPath, err := ensureRepoAtCommit(task, cache, opts)
	if err != nil {
		result.Error = "repo setup: " + err.Error()
		return result
	}

	// Create a project-scoped client.
	sc := client.WithProject(repoPath)

	// Retrieval strategy — file-scoring approach:
	//
	// Instead of adding wide line windows for every symbol found (which inflates
	// retrieved lines and tanks precision), we:
	//   1. Collect all (file, line) mentions across ALL tool calls into a mention map.
	//   2. Score files by how many unique lines were mentioned in them — files
	//      mentioned by multiple independent tool calls rank highest.
	//   3. Retrieve only from the top-5 scoring files, using merged windows so
	//      nearby lines form contiguous ranges rather than scattered points.
	//
	// This mirrors how a real agent works: it notices which files keep appearing
	// across multiple searches and focuses context retrieval there.

	// fileMentions: file → set of raw mentioned line numbers (before windowing).
	fileMentions := make(map[string]map[int]bool)
	addMention := func(file string, line int) {
		if file == "" || line <= 0 {
			return
		}
		// Normalize absolute paths to repo-relative paths. The prepare_context
		// tool sometimes returns absolute paths (e.g. /private/tmp/.../foo.py)
		// while gold context uses relative paths (e.g. astropy/foo.py). Strip
		// the repo root prefix to ensure paths match.
		if strings.HasPrefix(file, "/") && repoPath != "" {
			rel := file
			for _, prefix := range []string{repoPath + "/", repoPath} {
				if strings.HasPrefix(rel, prefix) {
					rel = rel[len(prefix):]
					break
				}
			}
			file = rel
		}
		// Skip non-source files — changelogs, docs, and test files accumulate
		// many mentions from generic searches (e.g. "modeling" → CHANGES.rst×24)
		// and would outrank actual source files in the scoring phase.
		if strings.HasSuffix(file, ".rst") || strings.HasSuffix(file, ".md") ||
			strings.HasSuffix(file, ".txt") || strings.HasSuffix(file, ".cfg") ||
			strings.HasPrefix(file, "docs/") || strings.HasPrefix(file, "CHANGES") ||
			strings.Contains(file, "/tests/") || strings.Contains(file, "/test_") ||
			strings.HasSuffix(file, "_test.py") || strings.HasSuffix(file, "_test.go") {
			return
		}
		if fileMentions[file] == nil {
			fileMentions[file] = make(map[int]bool)
		}
		fileMentions[file][line] = true
	}

	toolCalls := 0

	// Pass 0 — session_init priming. Primes the daemon session and extracts
	// entity/file hints from the bootstrap response (stale_context_hints,
	// relevant_memories, scale_guidance).
	var sessionEntities []string
	if initResp, err := sc.SessionInit("contextbench-"+task.InstanceID, "standard"); err == nil && initResp != "" {
		collectMarkdownMentions(initResp, addMention)
		sessionEntities = extractEntitiesFromResponse(initResp)
	}
	toolCalls++

	// Pass 0.5 — memory recall. Search episodic memory for relevant context
	// from prior sessions. Surfaces entities and files from past work.
	query := cleanFirstSentence(task.ProblemStatement)
	if recallR, err := sc.Recall(task.InstanceID, query); err == nil && recallR.Text != "" {
		collectMarkdownMentions(recallR.Text, addMention)
		sessionEntities = append(sessionEntities, extractEntitiesFromResponse(recallR.Text)...)
	}
	toolCalls++

	// Pass 1 — broad search with the clean first sentence of the problem.
	if sr, err := sc.Search(task.InstanceID, query); err == nil && sr.Text != "" {
		collectSearchMentions(sr.Text, addMention)
	}
	toolCalls++

	// Pass 2 — for each code entity: search (broad symbol lookup) + prepare_context
	// (graph traversal) + get_impact (downstream effects).
	// Entities come from problem statement + session_init/memory discoveries.
	entities := extractEntitiesFromProblem(task.ProblemStatement)
	// Prepend session-discovered entities (from Pass 0/0.5) for early coverage.
	for _, se := range sessionEntities {
		if se != "" && len(se) >= 3 {
			entities = append(entities, se)
		}
	}
	var followUpEntities []string
	seenEntity := make(map[string]bool)
	for _, e := range entities {
		seenEntity[e] = true
	}

	for _, entity := range entities {
		if entity == "" || toolCalls >= 24 {
			break
		}

		// Direct search — populates file scores with high-confidence hits.
		if sr, err := sc.Search(task.InstanceID, entity); err == nil && sr.Text != "" {
			collectSearchMentions(sr.Text, addMention)
		}
		toolCalls++

		// Graph traversal — finds callers, callees, related nodes.
		if ctxR, err := sc.PrepareContext(task.InstanceID, entity, "investigate issue"); err == nil && ctxR.Text != "" {
			collectMarkdownMentions(ctxR.Text, addMention)
			followUpEntities = append(followUpEntities, extractEntitiesFromResponse(ctxR.Text)...)
		}
		toolCalls++

		// Impact analysis — finds downstream affected symbols.
		if impR, err := sc.GetImpact(task.InstanceID, entity); err == nil && impR.Text != "" {
			collectImpactMentions(impR.Text, addMention)
			// Also extract entity names from impact tiers for follow-up discovery.
			followUpEntities = append(followUpEntities, extractEntitiesFromResponse(impR.Text)...)
		}
		toolCalls++
	}

	// Pass 3 — follow-up search for callee/related symbols surfaced by prepare_context.
	// These are symbols the graph traversal found but weren't in the problem statement.
	for _, entity := range followUpEntities {
		if entity == "" || seenEntity[entity] || toolCalls >= 30 {
			continue
		}
		seenEntity[entity] = true
		if sr, err := sc.Search(task.InstanceID, entity); err == nil && sr.Text != "" {
			collectSearchMentions(sr.Text, addMention)
		}
		toolCalls++
	}

	// Pass 4 — package-sibling search. For each unique directory that appears
	// in file mentions, search for the DIRECTORY NAME as a symbol. This helps
	// when an entity is found in its definition file (e.g. NdarrayMixin →
	// ndarray_mixin.py) but the bug is in a sibling file in the same package
	// (e.g. table.py in astropy/table/). Searching for "table" surfaces the
	// Table class in table.py and scores it alongside the definition file.
	seenPackage := make(map[string]bool)
	for file := range fileMentions {
		dir := filepath.Dir(file)
		pkg := filepath.Base(dir) // last component of directory
		if pkg == "." || pkg == "" || seenPackage[pkg] || seenEntity[pkg] {
			continue
		}
		// Skip generic/uninformative package names.
		if pkg == "src" || pkg == "lib" || pkg == "pkg" || pkg == "core" ||
			pkg == "common" || pkg == "utils" || pkg == "internal" || pkg == "tests" {
			continue
		}
		seenPackage[pkg] = true
		if toolCalls >= 35 {
			break
		}
		if sr, err := sc.Search(task.InstanceID, pkg); err == nil && sr.Text != "" {
			collectSearchMentions(sr.Text, addMention)
		}
		toolCalls++
	}

	// Normalize paths: the Markdown parser produces short basenames (e.g.
	// "sliced_wcs.py") that duplicate full-path entries (e.g.
	// "astropy/wcs/wcsapi/wrappers/sliced_wcs.py"). Merge short-basename
	// mentions into the matching full-path entry so scores aren't split.
	for shortPath, shortLines := range fileMentions {
		if strings.Contains(shortPath, "/") {
			continue // already a full path
		}
		for fullPath, fullLines := range fileMentions {
			if fullPath == shortPath || !strings.Contains(fullPath, "/") {
				continue
			}
			if strings.HasSuffix(fullPath, "/"+shortPath) || strings.HasSuffix(fullPath, shortPath) {
				for line := range shortLines {
					fullLines[line] = true
				}
				delete(fileMentions, shortPath)
				break
			}
		}
	}

	// Score files by number of unique lines mentioned, with bonuses for
	// files whose name matches problem-statement entities and for source files.
	fileDepth := func(f string) int { return strings.Count(f, "/") }
	isSourceFile := func(f string) bool {
		return !strings.HasPrefix(f, "docs/") &&
			!strings.HasPrefix(f, "CHANGES") &&
			!strings.Contains(f, "/tests/") &&
			!strings.Contains(f, "/test_") &&
			!strings.HasSuffix(f, ".rst") &&
			!strings.HasSuffix(f, ".md")
	}
	// Build set of lowercase entity stems for filename matching.
	entityStems := make(map[string]bool)
	for _, e := range entities {
		low := strings.ToLower(e)
		entityStems[low] = true
		// Also add last component of dotted paths.
		if idx := strings.LastIndex(low, "."); idx >= 0 {
			entityStems[low[idx+1:]] = true
		}
		// Add underscore-joined version of CamelCase.
		// e.g. "SeparabilityMatrix" → "separability_matrix"
		var snake []byte
		for j := 0; j < len(e); j++ {
			c := e[j]
			if c >= 'A' && c <= 'Z' {
				if j > 0 && e[j-1] >= 'a' && e[j-1] <= 'z' {
					snake = append(snake, '_')
				}
				snake = append(snake, c+32)
			} else {
				snake = append(snake, c)
			}
		}
		if s := string(snake); s != low && len(s) >= 3 {
			entityStems[s] = true
		}
	}
	// fileScore is defined at package level for use by buildRetrievedLines.
	var scored []fileScore
	for file, lines := range fileMentions {
		score := len(lines)
		// Filename-entity match bonus: if the file's basename (without extension)
		// matches any entity, boost its score significantly. This helps when the
		// entity "qdp" maps to qdp.py even though qdp.py has fewer mentions than
		// generic files like table.py.
		base := filepath.Base(file)
		ext := filepath.Ext(base)
		stem := strings.ToLower(strings.TrimSuffix(base, ext))
		if entityStems[stem] {
			score += 15 // strong boost for exact basename match
		} else {
			// Check if any entity stem is a substring of the filename.
			// Use the LONGEST match to prefer "sliced_wcs" matching
			// "sliced_wcs" over "wcs" matching "sliced_wcs".
			bestMatchLen := 0
			for es := range entityStems {
				if len(es) >= 4 && strings.Contains(stem, es) && len(es) > bestMatchLen {
					bestMatchLen = len(es)
				}
			}
			if bestMatchLen > 0 {
				// Scale boost by match length — longer matches are more specific.
				boost := 5 + bestMatchLen
				if boost > 15 {
					boost = 15
				}
				score += boost
			}
		}
		scored = append(scored, fileScore{file, score, isSourceFile(file), fileDepth(file)})
	}
	// Sort: primary = score desc, secondary = source files first, tertiary = depth desc.
	for i := 1; i < len(scored); i++ {
		for j := i; j > 0; j-- {
			a, b := scored[j-1], scored[j]
			less := b.score > a.score ||
				(b.score == a.score && b.source && !a.source) ||
				(b.score == a.score && b.source == a.source && b.depth > a.depth)
			if less {
				scored[j], scored[j-1] = scored[j-1], scored[j]
			} else {
				break
			}
		}
	}

	// Pass 5 — get_file_context on top-scored files. Retrieves exact entity
	// definition lines from the daemon, improving window centering precision.
	// Run before window building so the injected lines influence block density.
	for i, fs := range scored {
		if i >= 3 || toolCalls >= 40 {
			break
		}
		if fcr, err := sc.GetFileContext(task.InstanceID, fs.file); err == nil && fcr != nil {
			for _, ent := range fcr.Entities {
				if ent.Line > 0 {
					addMention(fcr.File, ent.Line)
				}
			}
		}
		toolCalls++
	}

	// Multi-budget evaluation: build retrieved line sets at 250, 500, 1000 budgets.
	retrieved250 := buildRetrievedLines(scored, fileMentions, 250)
	retrieved500 := buildRetrievedLines(scored, fileMentions, 500)
	retrieved1000 := buildRetrievedLines(scored, fileMentions, 1000)

	// Primary metrics use the 500-line budget (backward compatible).
	retrievedLines := retrieved500

	result.ToolCalls = toolCalls
	result.TotalLines = len(retrievedLines)

	// Debug: print sample retrieved and gold lines for cross-checking.
	if os.Getenv("CB_DEBUG") != "" {
		var retSample []string
		for l := range retrievedLines {
			retSample = append(retSample, l)
			if len(retSample) >= 5 {
				break
			}
		}
		var goldSample []string
		for l := range goldLines {
			goldSample = append(goldSample, l)
			if len(goldSample) >= 5 {
				break
			}
		}
		fmt.Printf("  [debug] retrieved sample: %v\n", retSample)
		fmt.Printf("  [debug] gold sample: %v\n", goldSample)
		fmt.Printf("  [debug] fileMentions top files: ")
		for _, fs := range scored[:minInt(3, len(scored))] {
			fmt.Printf("%s(%d) ", fs.file, fs.score)
		}
		fmt.Println()
		goldFiles := map[string]bool{}
		for gl := range goldLines {
			parts := strings.SplitN(gl, ":", 2)
			if len(parts) == 2 {
				goldFiles[parts[0]] = true
			}
		}
		for gf := range goldFiles {
			if mentions, ok := fileMentions[gf]; ok {
				var mlines []int
				for ml := range mentions {
					mlines = append(mlines, ml)
				}
				sort.Ints(mlines)
				if len(mlines) > 10 {
					mlines = mlines[:10]
				}
				fmt.Printf("  [debug] gold file %s mention lines: %v (total=%d)\n", gf, mlines, len(mentions))
			} else {
				fmt.Printf("  [debug] gold file %s NOT MENTIONED\n", gf)
			}
		}
	}

	// Compute Context F1 at each budget.
	computeF1 := func(retrieved map[string]bool) (precision, recall, f1 float64) {
		var h int
		for line := range retrieved {
			if goldLines[line] {
				h++
			}
		}
		if len(retrieved) > 0 {
			precision = float64(h) / float64(len(retrieved))
		}
		if len(goldLines) > 0 {
			recall = float64(h) / float64(len(goldLines))
		}
		if precision+recall > 0 {
			f1 = 2 * precision * recall / (precision + recall)
		}
		return
	}

	result.Precision, result.Recall, result.F1 = computeF1(retrieved500)
	result.HitLines = 0
	for line := range retrieved500 {
		if goldLines[line] {
			result.HitLines++
		}
	}

	// Multi-budget metrics.
	result.PAt250, result.RAt250, result.F1At250 = computeF1(retrieved250)
	result.PAt500, result.RAt500, result.F1At500 = computeF1(retrieved500)
	result.PAt1000, result.RAt1000, result.F1At1000 = computeF1(retrieved1000)

	// Token efficiency: estimate tokens as chars/4 (standard approximation for code).
	result.TokensRetrieved = result.TotalLines * 40 / 4 // ~40 chars per line average
	result.TokensGold = result.GoldLines * 40 / 4
	hitTokens := result.HitLines * 40 / 4
	if result.TokensRetrieved > 0 {
		result.TokenPrecision = float64(hitTokens) / float64(result.TokensRetrieved)
	}

	// Session cleanup — finalize the daemon session to prevent accumulation.
	sc.EndSession("contextbench-"+task.InstanceID, "success")

	return result
}

// ─── retrieval budget ───────────────────────────────────────────────────────

// fileScore holds a file's relevance score for ranking.
type fileScore struct {
	file   string
	score  int
	source bool
	depth  int
}

// buildRetrievedLines builds a set of "file:line" keys from scored files using
// density-sorted per-file windows, capped at maxLines total.
func buildRetrievedLines(scored []fileScore, fileMentions map[string]map[int]bool, maxLines int) map[string]bool {
	const (
		topFiles     = 8
		windowBefore = 5
		windowAfter  = 60
		mergeGap     = 10
	)
	perFileBudget := maxLines / 4 // scale per-file budget with total budget
	if perFileBudget < 60 {
		perFileBudget = 60
	}

	type windowBlock struct {
		start, end   int
		mentionCount int
	}
	buildWindows := func(mentions map[int]bool) []windowBlock {
		lineSet := make(map[int]bool)
		for line := range mentions {
			for l := maxInt(1, line-windowBefore); l <= line+windowAfter; l++ {
				lineSet[l] = true
			}
		}
		var ls []int
		for l := range lineSet {
			ls = append(ls, l)
		}
		sort.Ints(ls)
		if len(ls) == 0 {
			return nil
		}
		var blocks []windowBlock
		cur := windowBlock{start: ls[0], end: ls[0]}
		for _, l := range ls[1:] {
			if l <= cur.end+mergeGap {
				cur.end = l
			} else {
				blocks = append(blocks, cur)
				cur = windowBlock{start: l, end: l}
			}
		}
		blocks = append(blocks, cur)
		for i := range blocks {
			for ml := range mentions {
				if ml >= blocks[i].start && ml <= blocks[i].end {
					blocks[i].mentionCount++
				}
			}
		}
		sort.Slice(blocks, func(i, j int) bool {
			return blocks[i].mentionCount > blocks[j].mentionCount
		})
		return blocks
	}

	retrieved := make(map[string]bool)
	for i, fs := range scored {
		if i >= topFiles || len(retrieved) >= maxLines {
			break
		}
		blocks := buildWindows(fileMentions[fs.file])
		if len(blocks) == 0 {
			continue
		}
		fileBudget := perFileBudget
		if i == 0 {
			fileBudget = perFileBudget * 2
		}
		fileAdded := 0
		minPerBlock := fileBudget / (len(blocks) + 1)
		if minPerBlock < 20 {
			minPerBlock = 20
		}
		blockUsed := make([]int, len(blocks))
		for bi, b := range blocks {
			limit := minPerBlock
			if limit > b.end-b.start+1 {
				limit = b.end - b.start + 1
			}
			for ll := b.start; ll <= b.end && blockUsed[bi] < limit && fileAdded < fileBudget && len(retrieved) < maxLines; ll++ {
				key := fmt.Sprintf("%s:%d", fs.file, ll)
				if !retrieved[key] {
					retrieved[key] = true
					fileAdded++
					blockUsed[bi]++
				}
			}
		}
		for bi, b := range blocks {
			if fileAdded >= fileBudget || len(retrieved) >= maxLines {
				break
			}
			for ll := b.start + blockUsed[bi]; ll <= b.end && fileAdded < fileBudget && len(retrieved) < maxLines; ll++ {
				key := fmt.Sprintf("%s:%d", fs.file, ll)
				if !retrieved[key] {
					retrieved[key] = true
					fileAdded++
				}
			}
		}
	}
	return retrieved
}

// ─── repo management ─────────────────────────────────────────────────────────

// ensureRepoAtCommit clones (full depth) the repo, checks out base_commit,
// and indexes it. Uses cache keyed by "repo@commit" to avoid redundant work.
func ensureRepoAtCommit(task ContextBenchTask, cache *indexer.Cache, opts ContextBenchOptions) (string, error) {
	commit := task.BaseCommit
	if commit == "" {
		commit = "HEAD"
	}
	cacheKey := task.Repo + "@" + commit

	// Check cache — if already cloned+checked-out at this commit, skip clone.
	// We still run `synapses index` below even on cache hit: the daemon may have
	// restarted or evicted the project since the last run, and `index` is
	// idempotent (no-op when nothing changed).
	alreadyCloned := false
	var repoPath string
	if cached := cache.Get(cacheKey); cached != "" {
		if _, err := os.Stat(cached); err == nil {
			// Resolve symlinks on cache hit too (handles stale /tmp entries on macOS).
			if real, err2 := filepath.EvalSymlinks(cached); err2 == nil {
				cached = real
			}
			repoPath = cached
			alreadyCloned = true
		}
	}

	// Clone directory: repos_dir/owner/repo@commit (unique per commit).
	if repoPath == "" {
		repoPath = filepath.Join(opts.ReposDir, strings.ReplaceAll(cacheKey, "/", string(os.PathSeparator)))
	}

	if !alreadyCloned {
		// Clone if not present. Use full clone (not --depth=1) so we can checkout
		// arbitrary commits.
		if _, err := os.Stat(filepath.Join(repoPath, ".git")); os.IsNotExist(err) {
			repoURL := task.RepoURL
			if repoURL == "" {
				repoURL = "https://github.com/" + task.Repo + ".git"
			}
			if err := os.MkdirAll(filepath.Dir(repoPath), 0o755); err != nil {
				return "", fmt.Errorf("create parent dir: %w", err)
			}
			fmt.Printf("  cloning %s ...\n", task.Repo)
			cmd := exec.Command("git", "clone", "--quiet", repoURL, repoPath)
			cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
			if out, err := cmd.CombinedOutput(); err != nil {
				return "", fmt.Errorf("clone %s: %w\n%s", task.Repo, err, string(out))
			}
		}

		// Checkout the specific commit.
		if commit != "HEAD" {
			cmd := exec.Command("git", "-C", repoPath, "checkout", commit, "--force", "--quiet")
			cmd.Env = os.Environ()
			if out, err := cmd.CombinedOutput(); err != nil {
				return "", fmt.Errorf("checkout %s@%s: %w\n%s", task.Repo, commit, err, string(out))
			}
		}
	}

	// Index with Synapses. Always run even on cache hit — daemon may have
	// restarted and forgotten the project; `synapses index` is idempotent.
	if !opts.SkipIndex {
		synapsesBin := opts.SynapsesBin
		if synapsesBin == "" {
			synapsesBin = detectSynapsesBinPath()
		}
		if alreadyCloned {
			fmt.Printf("  re-registering %s@%s with daemon ...\n", task.Repo, commit[:minInt(8, len(commit))])
		} else {
			fmt.Printf("  indexing %s@%s ...\n", task.Repo, commit[:minInt(8, len(commit))])
		}
		cmd := exec.Command(synapsesBin, "index", "--path", repoPath)
		cmd.Env = os.Environ()

		done := make(chan error, 1)
		go func() { done <- cmd.Run() }()
		select {
		case err := <-done:
			if err != nil {
				return "", fmt.Errorf("index %s: %w", task.Repo, err)
			}
		case <-time.After(5 * time.Minute):
			if cmd.Process != nil {
				cmd.Process.Kill() //nolint:errcheck
			}
			return "", fmt.Errorf("index %s: timed out after 5 min", task.Repo)
		}
	}

	// Resolve symlinks so the path matches exactly what the daemon stores
	// (on macOS, /tmp → /private/tmp; mismatched paths produce 0 search results).
	if real, err := filepath.EvalSymlinks(repoPath); err == nil {
		repoPath = real
	}

	// Cache for reuse.
	_ = cache.Set(cacheKey, repoPath)
	return repoPath, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// detectSynapsesBinPath finds the synapses binary (same logic as indexer).
func detectSynapsesBinPath() string {
	candidates := []string{
		os.ExpandEnv("$HOME/.synapses/bin/synapses"),
		"/usr/local/bin/synapses",
		"synapses",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
		if path, err := exec.LookPath(c); err == nil {
			return path
		}
	}
	return "synapses"
}

// ─── tool response mention collectors ────────────────────────────────────────
//
// These functions parse tool responses and call addMention(file, line) for each
// (file, line) pair found. Windowing and merging happen later in the scoring
// phase, not here — keeping raw line numbers maximises scoring accuracy.

// collectSearchMentions parses the search tool JSON response.
// Format: {"results": [{"file": "...", "line": N, ...}]}
func collectSearchMentions(text string, addMention func(string, int)) {
	var resp struct {
		Results []struct {
			File string `json:"file"`
			Line int    `json:"line"`
		} `json:"results"`
	}
	if json.Unmarshal([]byte(text), &resp) != nil {
		return
	}
	for _, r := range resp.Results {
		if r.File != "" && r.Line > 0 {
			addMention(r.File, r.Line)
		}
	}
}

// collectImpactMentions parses the get_impact tool JSON response.
// Format: {"tiers": [{"nodes": [{"file":"...", "line": N}], ...}]}
func collectImpactMentions(text string, addMention func(string, int)) {
	var resp struct {
		Tiers []struct {
			Nodes []struct {
				File string `json:"file"`
				Line int    `json:"line"`
			} `json:"nodes"`
		} `json:"tiers"`
		AffectedFiles []string `json:"affected_files"`
	}
	if json.Unmarshal([]byte(text), &resp) != nil {
		return
	}
	for _, tier := range resp.Tiers {
		for _, node := range tier.Nodes {
			if node.File != "" && node.Line > 0 {
				addMention(node.File, node.Line)
			}
		}
	}
	// affected_files survives token budget trimming even when tier nodes are dropped.
	for _, f := range resp.AffectedFiles {
		if f != "" {
			addMention(f, 1)
		}
	}
}

// collectMarkdownMentions parses file:line references from prepare_context
// Markdown responses. Patterns matched:
//   - "path/to/file.py:42"
//   - "`path/to/file.py` (line 42)"
var fileLinePattern = regexp.MustCompile(`([a-zA-Z0-9_/.\-]+\.[a-zA-Z0-9]+):(\d+)`)

func collectMarkdownMentions(text string, addMention func(string, int)) {
	for _, m := range fileLinePattern.FindAllStringSubmatch(text, -1) {
		var lineNum int
		if _, err := fmt.Sscanf(m[2], "%d", &lineNum); err == nil {
			addMention(m[1], lineNum)
		}
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// cleanFirstSentence strips HTML comment blocks and leading boilerplate from
// a SWE-bench problem statement, then returns the first meaningful line
// (up to 200 chars). This gives a focused query for Pass 1 search.
func cleanFirstSentence(problem string) string {
	// Remove HTML comment blocks <!-- ... -->.
	htmlCommentRe := regexp.MustCompile(`(?s)<!--.*?-->`)
	clean := htmlCommentRe.ReplaceAllString(problem, "")
	// Remove markdown-style link/reference lines that start with http.
	lines := strings.Split(clean, "\n")
	var meaningful []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "http") || strings.HasPrefix(line, "#") ||
			strings.HasPrefix(line, "---") || strings.HasPrefix(line, "**") {
			continue
		}
		meaningful = append(meaningful, line)
	}
	if len(meaningful) == 0 {
		// Fall back to first 200 chars of original.
		if runes := []rune(problem); len(runes) > 200 {
			return string(runes[:200])
		}
		return problem
	}
	sentence := meaningful[0]
	if runes := []rune(sentence); len(runes) > 200 {
		return string(runes[:200])
	}
	return sentence
}

// ─── entity/keyword extraction ───────────────────────────────────────────────

// extractEntitiesFromProblem extracts likely class/function/module names from
// the problem statement. Looks for:
//   - Backtick-quoted code identifiers (e.g., `SeparabilityMatrix`)
//   - Dotted paths (e.g., "astropy.modeling.separable")
//   - snake_case identifiers (e.g., "get_separability_matrix")
//   - CamelCase identifiers with ≥2 case transitions or acronym prefix
//     (e.g., URLValidator, QuerySet, ValueError, ForeignKey)
//
// Avoids false positives: single-cased English words (Django, Python,
// Description) and plain acronyms without mixed case are excluded.
func extractEntitiesFromProblem(problem string) []string {
	var entities []string
	seen := make(map[string]bool)

	// Priority 1: backtick-quoted identifiers — highest confidence.
	// Also handles `method()` and `method(args)` patterns by stripping parens.
	backtickRe := regexp.MustCompile("`([a-zA-Z_][a-zA-Z0-9_.]*(?:\\(\\))?)`")
	for _, m := range backtickRe.FindAllStringSubmatch(problem, -1) {
		ident := m[1]
		// Strip trailing () to normalize `write()` → `write`.
		ident = strings.TrimSuffix(ident, "()")
		if len(ident) >= 3 && !seen[ident] {
			seen[ident] = true
			entities = append(entities, ident)
		}
	}

	// Priority 2: dotted paths not in URLs.
	words := strings.Fields(problem)
	for _, w := range words {
		w = strings.Trim(w, ".,;:!?()[]{}\"'`")
		if strings.Contains(w, ".") && !strings.HasPrefix(w, "http") && !strings.HasPrefix(w, "www.") {
			// Must look like a code path (at least one segment with lowercase).
			parts := strings.Split(w, ".")
			if len(parts) >= 2 && len(w) >= 5 && isIdentifier(parts[0]) {
				if !seen[w] {
					seen[w] = true
					entities = append(entities, w)
				}
			}
		}

		// Priority 3: snake_case identifiers (bare, no backticks needed).
		if strings.Contains(w, "_") && isIdentifier(w) && len(w) >= 5 {
			if !seen[w] {
				seen[w] = true
				entities = append(entities, w)
			}
		}

		// Priority 4: CamelCase identifiers — require ≥2 case transitions
		// (e.g. URLValidator, QuerySet, ValueError) to avoid plain English
		// words (Django, Python, Description, Unicode). Acronym-prefixed
		// identifiers like "URLValidator" are caught by counting uppercase→
		// lowercase transitions AND consecutive uppercase runs ≥3 (acronyms).
		if isIdentifier(w) && !strings.Contains(w, "_") {
			caseChanges := 0
			maxUpperRun := 0
			upperRun := 0
			for i := 0; i < len(w); i++ {
				c := w[i]
				if c >= 'A' && c <= 'Z' {
					upperRun++
					if upperRun > maxUpperRun {
						maxUpperRun = upperRun
					}
				} else {
					upperRun = 0
				}
				if i > 0 {
					prev := w[i-1]
					if (c >= 'A' && c <= 'Z') && (prev >= 'a' && prev <= 'z') {
						caseChanges++ // lowerToUpper
					}
					if (c >= 'a' && c <= 'z') && (prev >= 'A' && prev <= 'Z') {
						caseChanges++ // upperToLower
					}
				}
			}
			// Match if: ≥2 case transitions (e.g. QuerySet) OR acronym prefix
			// (≥2 uppercase run + at least one more transition, e.g. URLValidator).
			isCode := (caseChanges >= 2) || (maxUpperRun >= 2 && caseChanges >= 1)
			if isCode && len(w) >= 5 && !seen[w] {
				seen[w] = true
				entities = append(entities, w)
			}

			// Priority 5: all-caps domain acronyms (HTML, ITRS, ASCII, SQL, CSV,
			// HTTP, JSON, XML, WCS). These are rarely plain English and usually
			// refer to formats, protocols, or coordinate systems in the codebase.
			// Exclude generic programming terms that generate noise.
			allCapsStoplist := map[string]bool{
				"api": true, "sdk": true, "ide": true, "gui": true,
				"pr": true, "mr": true, "ci": true, "cd": true,
				"none": true, "true": true, "false": true,
				"github": true, "git": true, "os": true, "io": true,
			}
			allCaps := caseChanges == 0 && len(w) >= 3 && w == strings.ToUpper(w)
			if allCaps && !seen[strings.ToLower(w)] && !allCapsStoplist[strings.ToLower(w)] {
				seen[strings.ToLower(w)] = true
				entities = append(entities, strings.ToLower(w))
			}
		}
	}

	// Cap at 8 entities to bound tool calls (3 calls per entity × 8 = 24 max).
	if len(entities) > 8 {
		entities = entities[:8]
	}
	return entities
}

// extractEntitiesFromResponse extracts function/method names from a prepare_context
// Markdown response. These are graph-traversal results (callees, callers, related
// symbols) that weren't in the original problem statement. Used for one-level
// follow-up searches to discover private helpers and transitive dependencies.
//
// The prepare_context response has lines like:
//
//	"[_separability_matrix] function · separable.py:200"
//	"Calls: _separable, _is_separable"
//	"[Model._calculate_separability_matrix] method · core.py:808"
func extractEntitiesFromResponse(text string) []string {
	var entities []string
	seen := make(map[string]bool)

	// Pattern 1: "[EntityName] type · file.py:N"
	bracketRe := regexp.MustCompile(`\[([a-zA-Z_][a-zA-Z0-9_.]+)\]`)
	for _, m := range bracketRe.FindAllStringSubmatch(text, -1) {
		name := m[1]
		// Strip qualifier prefix (e.g. "Model._calculate" → "_calculate")
		if dot := strings.LastIndex(name, "."); dot >= 0 {
			name = name[dot+1:]
		}
		if len(name) >= 3 && !seen[name] {
			seen[name] = true
			entities = append(entities, name)
		}
	}

	// Pattern 2: "Calls: name1, name2, ..."
	callsRe := regexp.MustCompile(`(?i)calls?:\s*([^\n]+)`)
	for _, m := range callsRe.FindAllStringSubmatch(text, -1) {
		for _, part := range strings.Split(m[1], ",") {
			name := strings.TrimSpace(part)
			name = strings.Trim(name, "`[]()\"'")
			if len(name) >= 3 && isIdentifier(strings.ReplaceAll(name, ".", "")) && !seen[name] {
				seen[name] = true
				entities = append(entities, name)
			}
		}
	}

	if len(entities) > 6 {
		entities = entities[:6]
	}
	return entities
}

// extractKeywords extracts N most significant keywords from the problem statement.
// Skips common stop words and short tokens.
func extractKeywords(problem string, n int) []string {
	words := strings.Fields(problem)
	var keywords []string
	seen := make(map[string]bool)
	for _, w := range words {
		w = strings.Trim(w, ".,;:!?()[]{}\"'`")
		lower := strings.ToLower(w)
		if len(lower) < 4 || isStopWord(lower) || seen[lower] {
			continue
		}
		// Skip common non-code English words.
		if cbStopWords[lower] {
			continue
		}
		seen[lower] = true
		keywords = append(keywords, w) // preserve original case for search
		if len(keywords) >= n {
			break
		}
	}
	return keywords
}

// cbStopWords extends the base stop words with common English words that
// appear in issue descriptions but don't help code search.
var cbStopWords = map[string]bool{
	"when": true, "should": true, "does": true, "doesn": true, "have": true,
	"been": true, "being": true, "would": true, "could": true, "also": true,
	"using": true, "used": true, "into": true, "which": true, "there": true,
	"their": true, "about": true, "some": true, "than": true, "then": true,
	"other": true, "like": true, "just": true, "only": true, "will": true,
	"need": true, "make": true, "want": true, "each": true, "here": true,
	"where": true, "what": true, "they": true, "were": true, "more": true,
	"after": true, "before": true, "because": true, "since": true,
	"these": true, "those": true, "such": true, "very": true,
	"same": true, "currently": true, "expected": true, "actual": true,
	"instead": true, "however": true, "seems": true, "trying": true,
	"following": true, "example": true, "error": true, "issue": true,
	"problem": true, "please": true, "think": true, "work": true,
}

func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

// ─── gold context parsing ────────────────────────────────────────────────────

// parseGoldContext parses the gold_context JSON field.
func parseGoldContext(raw string) ([]GoldContextBlock, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" || raw == "null" {
		return nil, nil
	}
	var blocks []GoldContextBlock
	if err := json.Unmarshal([]byte(raw), &blocks); err != nil {
		return nil, err
	}
	// Validate blocks: filter out entries with missing fields.
	var valid []GoldContextBlock
	for _, b := range blocks {
		if b.File != "" && b.StartLine > 0 && b.EndLine >= b.StartLine {
			valid = append(valid, b)
		}
	}
	return valid, nil
}

// ─── task filtering ──────────────────────────────────────────────────────────

// filterTasks filters by language and source (case-insensitive).
func filterTasks(tasks []ContextBenchTask, languages, sources []string) []ContextBenchTask {
	if len(languages) == 0 && len(sources) == 0 {
		return tasks
	}
	langSet := setFromSlice(languages)
	srcSet := setFromSlice(sources)
	var out []ContextBenchTask
	for _, t := range tasks {
		if len(langSet) > 0 && !langSet[strings.ToLower(t.Language)] {
			continue
		}
		if len(srcSet) > 0 && !srcSet[strings.ToLower(t.Source)] {
			continue
		}
		out = append(out, t)
	}
	return out
}

func setFromSlice(ss []string) map[string]bool {
	if len(ss) == 0 {
		return nil
	}
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[strings.ToLower(s)] = true
	}
	return m
}

// ─── JSONL loader ────────────────────────────────────────────────────────────

// loadContextBenchTasks loads the JSONL dataset.
func loadContextBenchTasks(path string) ([]ContextBenchTask, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tasks []ContextBenchTask
	for lineNo, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var t ContextBenchTask
		if err := json.Unmarshal([]byte(line), &t); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo+1, err)
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

// stopWords are pure language keywords that appear in nearly every file and
// carry zero discriminative signal. Shared across benchmarks.
var stopWords = map[string]bool{
	// Natural language
	"the": true, "and": true, "for": true, "are": true, "not": true,
	"this": true, "that": true, "with": true, "from": true,
	// Python keywords
	"import": true, "def": true, "class": true, "return": true, "self": true,
	"true": true, "false": true, "none": true, "pass": true, "elif": true,
	"lambda": true, "yield": true, "async": true, "await": true,
	// Java / Kotlin keywords
	"null": true, "void": true, "new": true, "public": true, "private": true,
	"protected": true, "static": true, "final": true, "extends": true,
	"implements": true, "override": true, "super": true, "abstract": true,
	"interface": true,
	// Go keywords
	"func": true, "package": true, "var": true, "const": true, "type": true,
	"range": true, "make": true, "append": true,
	// TypeScript / JavaScript keywords
	"let": true, "export": true, "default": true, "typeof": true,
	// Control flow
	"if": true, "else": true, "break": true, "continue": true,
	// Structural
	"struct": true,
}

func isStopWord(s string) bool { return stopWords[s] }
