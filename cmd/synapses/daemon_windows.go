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

// processStartTime returns 0 on Windows — PID recycling detection
// relies solely on the PID file timestamp heuristic.
func processStartTime(_ int) int64 {
	return 0
}
