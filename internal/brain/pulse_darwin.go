//go:build darwin

package brain

import (
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// samplePlatform reads available RAM via vm_stat and the 1-minute load average
// via sysctl on macOS.
//
// Returns (availableRAM bytes, cpuLoadNorm [0,∞), error).
// cpuLoadNorm is NOT clamped here — the caller in pulse.go clamps to [0,1].
func samplePlatform() (int64, float64, error) {
	ram, err := readRAMDarwin()
	if err != nil {
		return 0, 0, fmt.Errorf("pulse/darwin: vm_stat: %w", err)
	}

	cpu, err := readLoadAvgDarwin()
	if err != nil {
		return 0, 0, fmt.Errorf("pulse/darwin: sysctl loadavg: %w", err)
	}

	return ram, cpu / numCPUSafe(), nil
}

// readRAMDarwin runs `vm_stat` and computes free RAM as:
//
//	(Pages free + Pages speculative) × page_size
//
// The vm_stat header line contains the page size, e.g.:
//
//	"Mach Virtual Memory Statistics: (page size of 4096 bytes)"
func readRAMDarwin() (int64, error) {
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return 0, err
	}

	var pageSize int64 = 4096 // fallback default
	var freePages, speculativePages int64

	for _, line := range bytes.Split(out, []byte("\n")) {
		s := strings.TrimSpace(string(line))

		// Parse page size from header: "page size of 4096 bytes"
		if strings.Contains(s, "page size of") {
			fields := strings.Fields(s)
			for i, f := range fields {
				if f == "of" && i+1 < len(fields) {
					if ps, err := strconv.ParseInt(fields[i+1], 10, 64); err == nil {
						pageSize = ps
					}
					break
				}
			}
			continue
		}

		// "Pages free:                               123456."
		if strings.HasPrefix(s, "Pages free:") {
			freePages = parseVMStatValue(s)
		} else if strings.HasPrefix(s, "Pages speculative:") {
			speculativePages = parseVMStatValue(s)
		}
	}

	return (freePages + speculativePages) * pageSize, nil
}

// parseVMStatValue extracts the integer from a vm_stat line like "Pages free: 12345."
func parseVMStatValue(line string) int64 {
	parts := strings.SplitN(line, ":", 2)
	if len(parts) != 2 {
		return 0
	}
	s := strings.TrimSpace(parts[1])
	s = strings.TrimRight(s, ".") // vm_stat values end with a period
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

// readLoadAvgDarwin runs `sysctl -n vm.loadavg` and returns the 1-minute load.
// Output format: "{ 1.23 4.56 7.89 }"
func readLoadAvgDarwin() (float64, error) {
	out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return 0, err
	}
	// Strip braces and split: "{ 1.23 4.56 7.89 }" → ["1.23", "4.56", "7.89"]
	s := strings.TrimSpace(string(out))
	s = strings.Trim(s, "{}")
	fields := strings.Fields(s)
	if len(fields) < 1 {
		return 0, fmt.Errorf("unexpected sysctl vm.loadavg output: %q", string(out))
	}
	load1, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("parse load1 %q: %w", fields[0], err)
	}
	return load1, nil
}
