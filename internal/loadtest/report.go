//go:build loadtest

package loadtest

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// FullReport is the top-level JSON output for a load test run.
type FullReport struct {
	Timestamp  time.Time      `json:"timestamp"`
	RepoRoot   string         `json:"repo_root"`
	Size       string         `json:"size"`
	Stages     []*StageReport `json:"stages"`
	LeakResult *LeakResult    `json:"leak_result,omitempty"`
	HTTPResult []HTTPResult   `json:"http_result,omitempty"`

	// Retained state (only populated when Config.RetainState is true).
	Graph   *graph.Graph `json:"-"`
	DBPath  string       `json:"-"`
	cleanUp func()
}

// Close releases retained resources. Safe to call on nil or non-retained reports.
func (r *FullReport) Close() {
	if r != nil && r.cleanUp != nil {
		r.cleanUp()
		r.cleanUp = nil
	}
}

// LeakResult captures the outcome of steady-state leak detection.
type LeakResult struct {
	Passed       bool          `json:"passed"`
	Duration     time.Duration `json:"duration_ns"`
	BaselineHeap int64         `json:"baseline_heap"`
	FinalHeap    int64         `json:"final_heap"`
	GrowthBytes  int64         `json:"growth_bytes"`
	GrowthPct    float64       `json:"growth_pct"`
	Reason       string        `json:"reason,omitempty"`
}

// HTTPResult stores latency statistics for one RPS tier.
type HTTPResult struct {
	RPS       int           `json:"rps"`
	Duration  time.Duration `json:"duration_ns"`
	P50       time.Duration `json:"p50_ns"`
	P95       time.Duration `json:"p95_ns"`
	P99       time.Duration `json:"p99_ns"`
	Max       time.Duration `json:"max_ns"`
	ErrorRate float64       `json:"error_rate"`
	Samples   int           `json:"samples"`
}

// WriteConsoleTable writes a human-readable table to w.
func WriteConsoleTable(w io.Writer, stages []*StageReport) {
	const header = "%-22s %10s %10s %10s %12s %12s %12s %12s %6s %4s"
	const row = "%-22s %10s %10s %10s %12s %12s %12s %12s %6s %4d"

	fmt.Fprintf(w, header+"\n",
		"STAGE", "WALL", "CPU_USER", "CPU_SYS",
		"HEAP_PEAK", "HEAP_DELTA", "ALLOCS", "PEAK_RSS",
		"GORO", "GC")
	fmt.Fprintln(w, strings.Repeat("─", 130))

	for _, s := range stages {
		heapDelta := s.HeapInuseAfter - s.HeapInuseBefore
		goroDelta := s.GoroutinesAfter - s.GoroutinesBefore

		goroStr := fmt.Sprintf("%+d", goroDelta)
		if goroDelta == 0 {
			goroStr = "0"
		}

		fmt.Fprintf(w, row+"\n",
			truncate(s.Name, 22),
			fmtDuration(s.WallTime),
			fmtDuration(s.CPUUserTime),
			fmtDuration(s.CPUSysTime),
			fmtBytes(s.HeapInusePeak),
			fmtBytesDelta(heapDelta),
			fmtCount(s.MallocsDelta),
			fmtBytes(s.PeakRSSAfter),
			goroStr,
			s.GCPauses,
		)
	}

	// Totals row.
	var totalWall, totalCPUUser, totalCPUSys time.Duration
	var totalAllocs int64
	for _, s := range stages {
		totalWall += s.WallTime
		totalCPUUser += s.CPUUserTime
		totalCPUSys += s.CPUSysTime
		totalAllocs += s.MallocsDelta
	}
	fmt.Fprintln(w, strings.Repeat("─", 130))
	fmt.Fprintf(w, "%-22s %10s %10s %10s %12s %12s %12s %12s %6s %4s\n",
		"TOTAL",
		fmtDuration(totalWall),
		fmtDuration(totalCPUUser),
		fmtDuration(totalCPUSys),
		"", "", fmtCount(totalAllocs), "",
		"", "",
	)
}

// WriteJSON writes the full report as indented JSON to w.
func WriteJSON(w io.Writer, report *FullReport) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

// WriteBenchstat writes benchstat-compatible output to w.
// Format: BenchmarkStage_<Name> <N> <wall_ns> ns/op <heap_peak> B/op <mallocs> allocs/op
func WriteBenchstat(w io.Writer, stages []*StageReport) {
	for _, s := range stages {
		name := "BenchmarkStage_" + sanitizeBenchName(s.Name)
		fmt.Fprintf(w, "%s 1 %d ns/op %d B/op %d allocs/op\n",
			name,
			s.WallTime.Nanoseconds(),
			s.HeapInusePeak,
			s.MallocsDelta,
		)
	}
}

// WriteHTTPTable writes HTTP load results as a table.
func WriteHTTPTable(w io.Writer, results []HTTPResult) {
	fmt.Fprintf(w, "\n%-8s %10s %10s %10s %10s %10s %8s\n",
		"RPS", "P50", "P95", "P99", "MAX", "ERRORS", "SAMPLES")
	fmt.Fprintln(w, strings.Repeat("─", 76))
	for _, r := range results {
		fmt.Fprintf(w, "%-8d %10s %10s %10s %10s %9.1f%% %8d\n",
			r.RPS,
			fmtDuration(r.P50),
			fmtDuration(r.P95),
			fmtDuration(r.P99),
			fmtDuration(r.Max),
			r.ErrorRate*100,
			r.Samples,
		)
	}
}

// Percentile computes the p-th percentile from a sorted slice of durations.
// p is in [0,1] (e.g. 0.95 for p95).
func Percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// SortDurations sorts a slice of durations in ascending order.
func SortDurations(d []time.Duration) {
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
}

// ── helpers ──────────────────────────────────────────────────────────────────

func fmtDuration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%.0fµs", float64(d.Microseconds()))
	case d < time.Second:
		return fmt.Sprintf("%.1fms", float64(d.Nanoseconds())/1e6)
	default:
		return fmt.Sprintf("%.2fs", d.Seconds())
	}
}

func fmtBytes(b int64) string {
	switch {
	case b < 1024:
		return fmt.Sprintf("%dB", b)
	case b < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(b)/1024)
	case b < 1024*1024*1024:
		return fmt.Sprintf("%.1fMB", float64(b)/(1024*1024))
	default:
		return fmt.Sprintf("%.2fGB", float64(b)/(1024*1024*1024))
	}
}

func fmtBytesDelta(b int64) string {
	sign := "+"
	if b < 0 {
		sign = "-"
		b = -b
	}
	return sign + fmtBytes(b)
}

func fmtCount(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

func sanitizeBenchName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('_')
		default:
			// skip
		}
	}
	return b.String()
}
