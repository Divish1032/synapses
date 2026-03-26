//go:build !windows

package parser

import (
	"syscall"

	"github.com/SynapsesOS/synapses/internal/logutil"
)

// lowerProcessPriority sets the current process to nice +10 so the initial
// full-index yields CPU time to foreground applications. Returns a function
// that restores the original priority. Best-effort — failure (e.g.
// unprivileged container) is logged and ignored.
func lowerProcessPriority() func() {
	// Get current priority so we can restore it.
	orig, err := syscall.Getpriority(syscall.PRIO_PROCESS, 0)
	if err != nil {
		logutil.Warn("synapses: could not read process priority: %v\n", err)
		return func() {}
	}
	if err := syscall.Setpriority(syscall.PRIO_PROCESS, 0, 10); err != nil {
		logutil.Warn("synapses: could not lower process priority: %v\n", err)
		return func() {}
	}
	logutil.Info("synapses: lowered process priority (nice +10) for initial index\n")
	return func() {
		if err := syscall.Setpriority(syscall.PRIO_PROCESS, 0, orig); err != nil {
			logutil.Warn("synapses: could not restore process priority: %v\n", err)
		} else {
			logutil.Info("synapses: restored process priority (nice %d)\n", orig)
		}
	}
}
