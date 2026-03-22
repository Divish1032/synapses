//go:build !windows

package main

import (
	"os"
	"syscall"
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
