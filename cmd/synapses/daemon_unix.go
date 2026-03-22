//go:build !windows

package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

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
// or 0 if it cannot be determined. Uses ps on macOS/Linux.
func processStartTime(pid int) int64 {
	// ps -o lstart= gives the process start time in a parseable format.
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return 0
	}
	// macOS/Linux ps lstart format: "Mon Jan  2 15:04:05 2006"
	for _, layout := range []string{
		"Mon Jan  2 15:04:05 2006",
		"Mon Jan 2 15:04:05 2006",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixNano()
		}
	}
	return 0
}
