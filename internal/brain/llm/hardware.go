package llm

// hardware.go — hardware capability detection for the local LLM backend.
//
// Detects Apple Metal (M-series), NVIDIA CUDA, and CPU-only configurations
// so the LocalClient can choose the right llama.cpp execution backend and
// set an appropriate GPU layer count at startup.

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// HardwareConfig describes the host machine's LLM-relevant capabilities.
type HardwareConfig struct {
	// HasMetal is true on Apple Silicon (M1/M2/M3/M4) Macs.
	// llama.cpp uses the Metal framework for GPU acceleration on these devices.
	HasMetal bool

	// HasCUDA is true when an NVIDIA GPU with CUDA support is detected.
	HasCUDA bool

	// GPULayers is the number of transformer layers to offload to the GPU.
	// 0 = CPU-only. Auto-tuned based on detected VRAM.
	GPULayers int

	// AvailableRAMGB is the approximate amount of free system RAM in GB.
	// Used as an anti-OOM guard: if too low the local backend is skipped.
	AvailableRAMGB float64
}

// DetectHardware probes the current machine and returns a HardwareConfig.
// It is safe to call multiple times; results are not cached (cheap probes).
func DetectHardware() HardwareConfig {
	cfg := HardwareConfig{}

	switch runtime.GOOS {
	case "darwin":
		cfg.HasMetal = isAppleSilicon()
		if cfg.HasMetal {
			// Apple Silicon unified memory — offload all layers.
			// A conservative default; can be overridden via SYNAPSES_GPU_LAYERS env.
			cfg.GPULayers = gpuLayersFromEnv(99)
		}

	case "linux", "windows":
		cfg.HasCUDA = hasCUDA()
		if cfg.HasCUDA {
			vram := detectNvidiaVRAMGB()
			// Rough heuristic: 7B 4-bit ≈ 4GB; each GB above that allows ~8 more layers.
			if vram >= 4 {
				cfg.GPULayers = gpuLayersFromEnv(int((vram - 3) * 8))
			} else {
				cfg.GPULayers = gpuLayersFromEnv(0) // CPU fallback
			}
		}
	}

	cfg.AvailableRAMGB = availableRAMGB()
	return cfg
}

// isAppleSilicon returns true if running on an Apple Silicon Mac.
func isAppleSilicon() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	out, err := exec.Command("sysctl", "-n", "hw.optional.arm64").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "1"
}

// hasCUDA returns true if nvidia-smi is available, indicating an NVIDIA GPU.
func hasCUDA() bool {
	_, err := exec.LookPath("nvidia-smi")
	return err == nil
}

// detectNvidiaVRAMGB returns total VRAM in GB from nvidia-smi, or 0 on failure.
func detectNvidiaVRAMGB() float64 {
	out, err := exec.Command(
		"nvidia-smi", "--query-gpu=memory.total", "--format=csv,noheader,nounits",
	).Output()
	if err != nil {
		return 0
	}
	// Output is in MB; take the first GPU.
	line := strings.Split(strings.TrimSpace(string(out)), "\n")[0]
	mb, err := strconv.ParseFloat(strings.TrimSpace(line), 64)
	if err != nil {
		return 0
	}
	return mb / 1024.0
}

// availableRAMGB returns available system RAM in GB.
// Uses /proc/meminfo on Linux; vm_stat on macOS; approximates on Windows.
func availableRAMGB() float64 {
	switch runtime.GOOS {
	case "linux":
		data, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			return 0
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "MemAvailable:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					kb, _ := strconv.ParseFloat(fields[1], 64)
					return kb / (1024 * 1024)
				}
			}
		}

	case "darwin":
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err != nil {
			return 0
		}
		bytes, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
		return bytes / (1024 * 1024 * 1024)
	}
	return 0
}

// gpuLayersFromEnv returns the SYNAPSES_GPU_LAYERS env var if set, otherwise defaultVal.
func gpuLayersFromEnv(defaultVal int) int {
	if v := os.Getenv("SYNAPSES_GPU_LAYERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultVal
}
