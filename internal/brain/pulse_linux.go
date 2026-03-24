//go:build linux

package brain

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// samplePlatform reads available RAM from /proc/meminfo and the 1-minute CPU
// load average from /proc/loadavg, then normalises CPU by the number of cores.
//
// Returns (availableRAM bytes, cpuLoadNorm [0,∞), error).
// cpuLoadNorm is NOT clamped here — the caller in pulse.go clamps to [0,1].
func samplePlatform() (int64, float64, error) {
	ram, err := readMemAvailable()
	if err != nil {
		return 0, 0, fmt.Errorf("pulse/linux: meminfo: %w", err)
	}

	cpu, err := readLoadAvg()
	if err != nil {
		return 0, 0, fmt.Errorf("pulse/linux: loadavg: %w", err)
	}

	return ram, cpu / numCPUSafe(), nil
}

// readMemAvailable parses /proc/meminfo and returns the MemAvailable value
// in bytes. The kernel guarantees "MemAvailable:" is present on Linux ≥ 3.14.
func readMemAvailable() (int64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		// Format: "MemAvailable:   12345678 kB"
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("unexpected MemAvailable line: %q", line)
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse MemAvailable value %q: %w", fields[1], err)
		}
		return kb * 1024, nil // convert kibibytes → bytes
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("MemAvailable not found in /proc/meminfo")
}

// readLoadAvg parses /proc/loadavg and returns the 1-minute load average.
// Format: "load1 load5 load15 running/total lastpid"
func readLoadAvg() (float64, error) {
	f, err := os.Open("/proc/loadavg")
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var load1 float64
	_, err = fmt.Fscanf(f, "%f", &load1)
	if err != nil {
		return 0, fmt.Errorf("parse /proc/loadavg: %w", err)
	}
	return load1, nil
}
