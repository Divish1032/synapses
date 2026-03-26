// Package indexer clones and indexes GitHub repositories using the Synapses
// CLI, caching results to disk so re-runs skip already-indexed repos.
//
// Designed for RepoBench-R: clone the repos referenced in the dataset,
// index each with `synapses index --path <dir>`, then let the benchmark
// binary call tools with `?project=<dir>` per sample.
//
// Usage:
//
//	indexer.Run(indexer.Options{
//	    ReposDir:   "/tmp/repobench_repos",
//	    CacheFile:  "/tmp/repobench_index_cache.json",
//	    Repos:      []string{"sissaschool/elementpath", ...},
//	    Workers:    8,
//	    SynapsesBin: "/Users/itachi/.synapses/bin/synapses",
//	})
package indexer

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options controls the indexer.
type Options struct {
	// ReposDir is the directory where repos are cloned.
	ReposDir string
	// CacheFile is the JSON file tracking which repos have been indexed.
	CacheFile string
	// Repos is the list of "owner/repo" strings to clone and index.
	Repos []string
	// Workers is the number of parallel clone+index workers (default 8).
	Workers int
	// SynapsesBin is the path to the synapses binary (default: auto-detect).
	SynapsesBin string
	// SkipIndex skips the `synapses index` step (clone only).
	SkipIndex bool
	// TimeoutPerRepo is the max time per clone+index operation (default 3 min).
	TimeoutPerRepo time.Duration
	// Verbose prints per-repo progress.
	Verbose bool
}

// Result holds the outcome of indexing a single repo.
type Result struct {
	Repo      string `json:"repo"`
	LocalPath string `json:"local_path"`
	Indexed   bool   `json:"indexed"`
	Skipped   bool   `json:"skipped"` // already cached
	Error     string `json:"error,omitempty"`
	DurationS float64 `json:"duration_s"`
}

// Cache persists indexed repo paths across runs.
type Cache struct {
	mu      sync.Mutex
	path    string
	entries map[string]string // repo → local path
}

// LoadCache loads or creates a cache file.
func LoadCache(path string) (*Cache, error) {
	c := &Cache{path: path, entries: make(map[string]string)}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, fmt.Errorf("load cache: %w", err)
	}
	if err := json.Unmarshal(data, &c.entries); err != nil {
		return nil, fmt.Errorf("parse cache: %w", err)
	}
	return c, nil
}

// Get returns the local path for a repo, or "" if not cached.
func (c *Cache) Get(repo string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[repo]
}

// Set records a repo → local path mapping and flushes to disk.
func (c *Cache) Set(repo, localPath string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[repo] = localPath
	data, err := json.MarshalIndent(c.entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, data, 0o644)
}

// LocalPath returns the canonical local directory for a GitHub repo.
// "owner/repo" → "<reposDir>/owner/repo"
func LocalPath(reposDir, repo string) string {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return filepath.Join(reposDir, repo)
	}
	return filepath.Join(reposDir, parts[0], parts[1])
}

// Run clones and indexes all repos with parallel workers.
// Returns results for all repos (including cached ones).
func Run(opts Options) ([]Result, error) {
	if opts.Workers <= 0 {
		opts.Workers = 8
	}
	if opts.TimeoutPerRepo == 0 {
		opts.TimeoutPerRepo = 3 * time.Minute
	}
	if opts.SynapsesBin == "" {
		opts.SynapsesBin = detectSynapsesBin()
	}
	if err := os.MkdirAll(opts.ReposDir, 0o755); err != nil {
		return nil, fmt.Errorf("create repos dir: %w", err)
	}

	cache, err := LoadCache(opts.CacheFile)
	if err != nil {
		return nil, err
	}

	type job struct {
		repo string
	}

	jobs := make(chan job, len(opts.Repos))
	for _, r := range opts.Repos {
		jobs <- job{repo: r}
	}
	close(jobs)

	results := make([]Result, len(opts.Repos))
	repoIndex := make(map[string]int, len(opts.Repos))
	for i, r := range opts.Repos {
		repoIndex[r] = i
	}

	var mu sync.Mutex
	var done int64
	total := int64(len(opts.Repos))

	var wg sync.WaitGroup
	for w := 0; w < opts.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				r := processRepo(j.repo, opts, cache)
				mu.Lock()
				results[repoIndex[j.repo]] = r
				mu.Unlock()
				n := atomic.AddInt64(&done, 1)
				if opts.Verbose || n%10 == 0 || n == total {
					status := "ok"
					if r.Error != "" {
						status = "err: " + r.Error
					} else if r.Skipped {
						status = "cached"
					}
					fmt.Printf("  [%d/%d] %s — %s\n", n, total, j.repo, status)
				}
			}
		}()
	}
	wg.Wait()
	return results, nil
}

// processRepo clones and indexes one repo, using cache if available.
func processRepo(repo string, opts Options, cache *Cache) Result {
	start := time.Now()

	// Already indexed?
	if cached := cache.Get(repo); cached != "" {
		if _, err := os.Stat(cached); err == nil {
			return Result{
				Repo:      repo,
				LocalPath: cached,
				Indexed:   true,
				Skipped:   true,
				DurationS: time.Since(start).Seconds(),
			}
		}
		// Cached path gone — re-clone.
	}

	localPath := LocalPath(opts.ReposDir, repo)

	// Clone.
	if _, err := os.Stat(localPath); os.IsNotExist(err) {
		if err := cloneRepo(repo, localPath, opts.TimeoutPerRepo); err != nil {
			return Result{
				Repo:      repo,
				LocalPath: localPath,
				Error:     "clone: " + err.Error(),
				DurationS: time.Since(start).Seconds(),
			}
		}
	}

	// Index.
	if !opts.SkipIndex {
		if err := indexRepo(localPath, opts.SynapsesBin, opts.TimeoutPerRepo); err != nil {
			return Result{
				Repo:      repo,
				LocalPath: localPath,
				Error:     "index: " + err.Error(),
				DurationS: time.Since(start).Seconds(),
			}
		}
	}

	// Cache the result.
	_ = cache.Set(repo, localPath)

	return Result{
		Repo:      repo,
		LocalPath: localPath,
		Indexed:   true,
		DurationS: time.Since(start).Seconds(),
	}
}

// cloneRepo runs `git clone --depth=1` for a GitHub repo.
func cloneRepo(repo, localPath string, timeout time.Duration) error {
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return err
	}
	url := "https://github.com/" + repo + ".git"
	cmd := exec.Command("git", "clone", "--depth=1", "--quiet", url, localPath)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")

	// Timeout via context.
	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		cmd.Process.Kill() //nolint:errcheck
		return fmt.Errorf("timeout after %s", timeout)
	}
}

// indexRepo runs `synapses index --path <dir>` on the cloned repo.
func indexRepo(localPath, synapsesBin string, timeout time.Duration) error {
	cmd := exec.Command(synapsesBin, "index", "--path", localPath)
	cmd.Env = os.Environ()

	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		cmd.Process.Kill() //nolint:errcheck
		return fmt.Errorf("index timeout after %s", timeout)
	}
}

// detectSynapsesBin finds the synapses binary.
func detectSynapsesBin() string {
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

// Summary prints a summary of indexer results.
func Summary(results []Result) {
	var ok, cached, failed int
	var totalTime float64
	for _, r := range results {
		totalTime += r.DurationS
		if r.Error != "" {
			failed++
		} else if r.Skipped {
			cached++
		} else {
			ok++
		}
	}
	fmt.Printf("\nIndexer summary: %d indexed, %d cached, %d failed (%.1fs total)\n",
		ok, cached, failed, totalTime)
}
