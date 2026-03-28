//go:build windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x00000008} // DETACHED_PROCESS
}

func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Windows there's no signal 0; use OpenProcess instead.
	handle, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(proc.Pid))
	if err != nil {
		return false
	}
	syscall.CloseHandle(handle)
	return true
}

func killProcess(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}
	return proc.Signal(os.Interrupt)
}

func forceKillProcess(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	return proc.Kill()
}

// processStartTime returns the creation time of the process with the given PID
// as Unix nanoseconds. Returns 0 if the process cannot be queried (not running,
// access denied, etc.). Used for PID recycling detection: if the PID file's
// stored start time doesn't match the running process's creation time, the PID
// was recycled by the OS and the stale PID file should be cleaned up.
//
// Uses GetProcessTimes via syscall — available on all Windows versions.
// PROCESS_QUERY_LIMITED_INFORMATION (0x1000) is sufficient and works even for
// processes owned by other users (unlike PROCESS_QUERY_INFORMATION).
func processStartTime(pid int) int64 {
	const PROCESS_QUERY_LIMITED_INFORMATION = 0x1000
	handle, err := syscall.OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0
	}
	defer syscall.CloseHandle(handle)

	var creation, exit, kernel, user syscall.Filetime
	err = syscall.GetProcessTimes(handle, &creation, &exit, &kernel, &user)
	if err != nil {
		return 0
	}
	return creation.Nanoseconds()
}
