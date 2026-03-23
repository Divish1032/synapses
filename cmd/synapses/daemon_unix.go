//go:build !windows

package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// processStartTimeCache caches PID→start-time lookups to avoid shelling
// out to `ps` on every check (10-50ms per call).
var processStartTimeCache sync.Map // int → int64

func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	defer proc.Release()
	return proc.Signal(syscall.Signal(0)) == nil
}

func killProcess(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	err = proc.Signal(syscall.SIGTERM)
	proc.Release()
	return err
}

func forceKillProcess(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	err = proc.Kill()
	proc.Release()
	return err
}

// processStartTime returns the start time of the process as Unix nanos,
// or 0 if it cannot be determined.
//
// Uses `ps -o etime=` which outputs elapsed time in [[dd-]hh:]mm:ss format.
// This is locale-independent (no month/day names), working reliably on
// macOS, Linux, and BSD regardless of LC_TIME settings.
func processStartTime(pid int) int64 {
	if v, ok := processStartTimeCache.Load(pid); ok {
		return v.(int64)
	}
	out, err := exec.Command("ps", "-o", "etime=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		// Process doesn't exist (ps failed) — evict any stale entry.
		processStartTimeCache.Delete(pid)
		return 0
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return 0
	}
	elapsed := parseEtime(s)
	if elapsed <= 0 {
		return 0
	}
	result := time.Now().Add(-elapsed).UnixNano()
	// Cap cache size to prevent unbounded growth from accumulated dead PIDs.
	// sync.Map has no Len(); count via Range. Reset when over 256 entries.
	count := 0
	processStartTimeCache.Range(func(_, _ interface{}) bool {
		count++
		return count < 257
	})
	if count >= 256 {
		processStartTimeCache.Range(func(k, _ interface{}) bool {
			processStartTimeCache.Delete(k)
			return true
		})
	}
	processStartTimeCache.Store(pid, result)
	return result
}

// parseEtime parses the `ps -o etime=` format: [[dd-]hh:]mm:ss
// Returns the duration, or 0 on parse failure.
func parseEtime(s string) time.Duration {
	var days, hours, minutes, seconds int

	// Split off days: "dd-hh:mm:ss" → days="dd", rest="hh:mm:ss"
	if idx := strings.Index(s, "-"); idx >= 0 {
		d, err := strconv.Atoi(s[:idx])
		if err != nil {
			return 0
		}
		days = d
		s = s[idx+1:]
	}

	parts := strings.Split(s, ":")
	switch len(parts) {
	case 3: // hh:mm:ss
		h, err := strconv.Atoi(parts[0])
		if err != nil {
			return 0
		}
		hours = h
		parts = parts[1:]
		fallthrough
	case 2: // mm:ss
		m, err := strconv.Atoi(parts[0])
		if err != nil {
			return 0
		}
		minutes = m
		sec, err := strconv.Atoi(parts[1])
		if err != nil {
			return 0
		}
		seconds = sec
	default:
		return 0
	}

	return time.Duration(days)*24*time.Hour +
		time.Duration(hours)*time.Hour +
		time.Duration(minutes)*time.Minute +
		time.Duration(seconds)*time.Second
}
