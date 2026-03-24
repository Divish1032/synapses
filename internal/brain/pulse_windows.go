//go:build windows

package brain

import (
	"fmt"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// windowsCPUState stores the previous GetSystemTimes values so we can compute
// the CPU-busy delta between successive samples.
var (
	windowsCPUMu   sync.Mutex
	prevIdleTicks  uint64
	prevTotalTicks uint64
	prevSampleTime time.Time
)

// samplePlatform reads available RAM via GlobalMemoryStatusEx and CPU load
// via GetSystemTimes on Windows.
//
// Returns (availableRAM bytes, cpuLoadNorm [0,∞), error).
// cpuLoadNorm is NOT clamped here — the caller in pulse.go clamps to [0,1].
func samplePlatform() (int64, float64, error) {
	ram, err := readRAMWindows()
	if err != nil {
		return 0, 0, fmt.Errorf("pulse/windows: GlobalMemoryStatusEx: %w", err)
	}

	cpu, err := readCPUWindows()
	if err != nil {
		return 0, 0, fmt.Errorf("pulse/windows: GetSystemTimes: %w", err)
	}

	return ram, cpu, nil
}

// MEMORYSTATUSEX is the structure filled by GlobalMemoryStatusEx.
// https://docs.microsoft.com/en-us/windows/win32/api/sysinfoapi/ns-sysinfoapi-memorystatusex
type memoryStatusEx struct {
	dwLength                uint32
	dwMemoryLoad            uint32
	ullTotalPhys            uint64
	ullAvailPhys            uint64
	ullTotalPageFile        uint64
	ullAvailPageFile        uint64
	ullTotalVirtual         uint64
	ullAvailVirtual         uint64
	ullAvailExtendedVirtual uint64
}

var (
	modKernel32              = syscall.NewLazyDLL("kernel32.dll")
	procGlobalMemoryStatusEx = modKernel32.NewProc("GlobalMemoryStatusEx")
	procGetSystemTimes       = modKernel32.NewProc("GetSystemTimes")
)

// readRAMWindows calls GlobalMemoryStatusEx and returns ullAvailPhys (bytes).
func readRAMWindows() (int64, error) {
	var ms memoryStatusEx
	ms.dwLength = uint32(unsafe.Sizeof(ms))
	ret, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms)))
	if ret == 0 {
		return 0, fmt.Errorf("GlobalMemoryStatusEx failed: %w", err)
	}
	return int64(ms.ullAvailPhys), nil
}

// FILETIME holds a 64-bit value representing the number of 100-nanosecond
// intervals since January 1, 1601 (UTC).
type fileTime struct {
	LowDateTime  uint32
	HighDateTime uint32
}

func fileTimeToUint64(ft fileTime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

// readCPUWindows calls GetSystemTimes and computes the normalised CPU busy
// fraction as a delta from the previous sample.  Returns 0.0 on the very first
// call (no prior delta available).
func readCPUWindows() (float64, error) {
	var idleTime, kernelTime, userTime fileTime
	ret, _, err := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idleTime)),
		uintptr(unsafe.Pointer(&kernelTime)),
		uintptr(unsafe.Pointer(&userTime)),
	)
	if ret == 0 {
		return 0, fmt.Errorf("GetSystemTimes failed: %w", err)
	}

	idle := fileTimeToUint64(idleTime)
	// kernelTime includes idle time, so total = kernel + user (not kernel - idle + user).
	kernel := fileTimeToUint64(kernelTime)
	user := fileTimeToUint64(userTime)
	total := kernel + user

	windowsCPUMu.Lock()
	defer windowsCPUMu.Unlock()

	if prevTotalTicks == 0 {
		// First sample — store baseline, return 0 (no delta yet).
		prevIdleTicks = idle
		prevTotalTicks = total
		prevSampleTime = time.Now()
		return 0.0, nil
	}

	deltaIdle := idle - prevIdleTicks
	deltaTotal := total - prevTotalTicks

	prevIdleTicks = idle
	prevTotalTicks = total
	prevSampleTime = time.Now()

	if deltaTotal == 0 {
		return 0.0, nil
	}

	busyFraction := 1.0 - float64(deltaIdle)/float64(deltaTotal)
	return busyFraction, nil
}
