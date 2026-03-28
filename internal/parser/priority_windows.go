//go:build windows

package parser

// lowerProcessPriority is a no-op on Windows. SetPriorityClass could be used
// but is not implemented — Synapses primarily targets macOS and Linux.
func lowerProcessPriority() func() { return func() {} }
