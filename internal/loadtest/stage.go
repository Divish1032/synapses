//go:build loadtest

package loadtest

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"syscall"
	"time"
)

// StageReport captures resource consumption for a single pipeline stage.
type StageReport struct {
	Name             string            `json:"name"`
	WallTime         time.Duration     `json:"wall_time_ns"`
	CPUUserTime      time.Duration     `json:"cpu_user_ns"`
	CPUSysTime       time.Duration     `json:"cpu_sys_ns"`
	HeapInuseBefore  int64             `json:"heap_inuse_before"`
	HeapInuseAfter   int64             `json:"heap_inuse_after"`
	HeapInusePeak    int64             `json:"heap_inuse_peak"`
	TotalAllocDelta  int64             `json:"total_alloc_delta"`
	MallocsDelta     int64             `json:"mallocs_delta"`
	PeakRSSBefore    int64             `json:"peak_rss_before"`
	PeakRSSAfter     int64             `json:"peak_rss_after"`
	GoroutinesBefore int               `json:"goroutines_before"`
	GoroutinesAfter  int               `json:"goroutines_after"`
	GCPauses         int               `json:"gc_pauses"`
	GCPauseTotal     time.Duration     `json:"gc_pause_total_ns"`
	GCPctOfWall      float64           `json:"gc_pct_of_wall"`
	CustomCounters   map[string]int64  `json:"custom_counters,omitempty"`
}

// StageOption configures optional behaviour for MeasureStage.
type StageOption func(*stageConfig)

type stageConfig struct {
	cpuProfilePath  string
	allocProfilePath string
	sampleInterval  time.Duration
}

// WithCPUProfile writes a CPU profile to path during stage execution.
func WithCPUProfile(path string) StageOption {
	return func(c *stageConfig) { c.cpuProfilePath = path }
}

// WithAllocProfile writes a heap/alloc profile to path after stage execution.
func WithAllocProfile(path string) StageOption {
	return func(c *stageConfig) { c.allocProfilePath = path }
}

// WithSampleInterval overrides the sampler's polling interval.
func WithSampleInterval(d time.Duration) StageOption {
	return func(c *stageConfig) { c.sampleInterval = d }
}

// MeasureStage executes fn and returns a StageReport with resource
// consumption metrics. It forces a GC before the stage to establish
// a clean baseline, then samples heap at 10ms during execution.
//
// The returned StageReport.CustomCounters map is nil; callers should
// populate it after measuring.
func MeasureStage(name string, fn func() error, opts ...StageOption) (*StageReport, error) {
	cfg := stageConfig{sampleInterval: defaultSampleInterval}
	for _, o := range opts {
		o(&cfg)
	}

	// Clean baseline: collect garbage and release memory to OS.
	runtime.GC()
	debug.FreeOSMemory()
	time.Sleep(100 * time.Millisecond)

	// Pre-stage snapshots.
	before := readSnapshot()
	rssBefore := peakRSS()
	cpuUserBefore, cpuSysBefore := cpuTime()

	// Start heap peak sampler.
	sampler := NewSampler(cfg.sampleInterval)
	sampler.Start()

	// Optional CPU profile.
	var cpuFile *os.File
	if cfg.cpuProfilePath != "" {
		var err error
		cpuFile, err = os.Create(cfg.cpuProfilePath)
		if err != nil {
			sampler.Stop()
			return nil, fmt.Errorf("loadtest: create cpu profile %s: %w", cfg.cpuProfilePath, err)
		}
		if err := pprof.StartCPUProfile(cpuFile); err != nil {
			cpuFile.Close()
			sampler.Stop()
			return nil, fmt.Errorf("loadtest: start cpu profile: %w", err)
		}
	}

	// Execute stage.
	start := time.Now()
	fnErr := fn()
	wall := time.Since(start)

	// Stop profiling.
	if cpuFile != nil {
		pprof.StopCPUProfile()
		cpuFile.Close()
	}
	sampler.Stop()

	// Post-stage snapshots.
	after := readSnapshot()
	rssAfter := peakRSS()
	cpuUserAfter, cpuSysAfter := cpuTime()

	report := &StageReport{
		Name:             name,
		WallTime:         wall,
		CPUUserTime:      cpuUserAfter - cpuUserBefore,
		CPUSysTime:       cpuSysAfter - cpuSysBefore,
		HeapInuseBefore:  before.HeapInuse,
		HeapInuseAfter:   after.HeapInuse,
		HeapInusePeak:    sampler.PeakHeapInuse(),
		TotalAllocDelta:  after.TotalAlloc - before.TotalAlloc,
		MallocsDelta:     after.Mallocs - before.Mallocs,
		PeakRSSBefore:    rssBefore,
		PeakRSSAfter:     rssAfter,
		GoroutinesBefore: before.Goroutines,
		GoroutinesAfter:  after.Goroutines,
		GCPauses:         int(after.NumGC - before.NumGC),
		GCPauseTotal:     after.PauseTotal - before.PauseTotal,
	}

	// Ensure peak is at least as high as before/after.
	if report.HeapInusePeak < report.HeapInuseBefore {
		report.HeapInusePeak = report.HeapInuseBefore
	}
	if report.HeapInusePeak < report.HeapInuseAfter {
		report.HeapInusePeak = report.HeapInuseAfter
	}

	// GC time as percentage of wall time.
	if wall > 0 {
		report.GCPctOfWall = float64(report.GCPauseTotal) / float64(wall) * 100
	}

	// Optional alloc profile dump.
	if cfg.allocProfilePath != "" {
		if allocErr := DumpAllocProfile(cfg.allocProfilePath); allocErr != nil {
			fmt.Fprintf(os.Stderr, "loadtest: alloc profile %s: %v\n", cfg.allocProfilePath, allocErr)
		}
	}

	return report, fnErr
}

// cpuTime returns process-wide user and system CPU time via Getrusage.
func cpuTime() (user, sys time.Duration) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, 0
	}
	user = time.Duration(ru.Utime.Nano())
	sys = time.Duration(ru.Stime.Nano())
	return
}

// peakRSS returns the peak resident set size in bytes.
// On darwin Maxrss is already in bytes; on linux it is in KB.
func peakRSS() int64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	rss := int64(ru.Maxrss)
	if runtime.GOOS == "linux" {
		rss *= 1024 // linux reports KB
	}
	return rss
}

// DumpHeapProfile writes a heap profile to the given path.
func DumpHeapProfile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return pprof.WriteHeapProfile(f)
}

// DumpAllocProfile writes an alloc profile (all allocations, not just live)
// to the given path. Use `go tool pprof -alloc_space <file>` to analyze.
func DumpAllocProfile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return pprof.Lookup("allocs").WriteTo(f, 0)
}
