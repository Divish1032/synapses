// Package brain — pulse.go provides cross-platform system health monitoring.
//
// SystemPulse samples RAM and CPU every 10 seconds and classifies system
// health into three levels (Green/Yellow/Red) to guide work scheduling.
// Ollama model residency is polled separately every 30 seconds via /api/ps.
//
// Usage:
//
//	p := NewSystemPulse()
//	p.Start()
//	defer p.Stop()
//	state := p.Current()
package brain

import (
	"encoding/json"
	"io"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/SynapsesOS/synapses/internal/logutil"
)

// HealthLevel classifies the current system resource state.
type HealthLevel int

const (
	// HealthGreen indicates ample resources: RAM > 3 GB free and CPU < 0.7.
	// All work can proceed normally.
	HealthGreen HealthLevel = iota

	// HealthYellow indicates moderate pressure: RAM 1.5–3 GB free or CPU 0.7–0.9.
	// Prefer P0 (critical) work; defer lower-priority tasks.
	HealthYellow

	// HealthRed indicates resource exhaustion: RAM < 1.5 GB free or CPU > 0.9.
	// Degrade all work; shed load where possible.
	HealthRed
)

// String returns a human-readable label for the health level.
func (h HealthLevel) String() string {
	switch h {
	case HealthGreen:
		return "green"
	case HealthYellow:
		return "yellow"
	case HealthRed:
		return "red"
	default:
		return "unknown"
	}
}

const (
	pulseRAMGreenThreshold  int64   = 3 * 1024 * 1024 * 1024 // 3 GB
	pulseRAMYellowThreshold int64   = 1536 * 1024 * 1024     // 1.5 GB
	pulseCPUGreenThreshold  float64 = 0.7
	pulseCPUYellowThreshold float64 = 0.9

	pulseSampleInterval = 10 * time.Second
	pulseOllamaInterval = 30 * time.Second
	pulseOllamaTimeout  = 2 * time.Second
	pulseOllamaURL      = "http://localhost:11434/api/ps"
)

// SystemState is a snapshot of system resource availability.
// All fields are safe to read without a lock (returned by value from Current()).
type SystemState struct {
	// AvailableRAM is the amount of RAM free for allocation, in bytes.
	AvailableRAM int64

	// CPULoadNorm is the normalised 1-minute CPU load average in [0.0, 1.0].
	// Computed as load1 / numCPU, clamped to 1.0.
	CPULoadNorm float64

	// OllamaModelLoaded is the name of the model currently resident in Ollama,
	// or "" if Ollama is not running or no model is loaded.
	OllamaModelLoaded string

	// Health is the derived health classification for the current state.
	Health HealthLevel

	// SampledAt is the wall-clock time when this state was last updated.
	SampledAt time.Time
}

// SystemPulse samples system resources on a background goroutine and exposes
// the latest snapshot via Current(). It is safe for concurrent use.
//
// Lifecycle: NewSystemPulse → (optional WithOllamaURL) → Start → (use Current) → Stop.
// SystemPulse is NOT restartable: once Stop() is called, Start() is a no-op
// and the pulse remains stopped. Create a new instance to restart.
type SystemPulse struct {
	mu         sync.RWMutex
	current    SystemState
	httpClient *http.Client
	ollamaURL  string // full URL to Ollama /api/ps; defaults to pulseOllamaURL
	done       chan struct{}

	// platformCPUState holds platform-specific CPU sampling state.
	// On Linux/Darwin it is an empty struct (zero cost). On Windows it holds
	// the previous GetSystemTimes values for delta computation, scoped to this
	// instance so multiple SystemPulse instances do not share state.
	platformCPUState

	// wg tracks the single background goroutine launched by Start().
	// Stop() calls wg.Wait(), which returns immediately if Start() was never
	// called (counter stays at 0). This avoids any channel reassignment and
	// the data race that would entail.
	wg sync.WaitGroup

	// startOnce ensures Start() launches at most one background goroutine.
	startOnce sync.Once
	// stopOnce ensures done is closed exactly once.
	stopOnce sync.Once
}

// NewSystemPulse creates a new SystemPulse. Call Start() to begin sampling.
// The pulse is ready for use immediately; Current() returns a zero-value
// SystemState until Start() is called.
func NewSystemPulse() *SystemPulse {
	return &SystemPulse{
		httpClient: &http.Client{Timeout: pulseOllamaTimeout},
		ollamaURL:  pulseOllamaURL,
		done:       make(chan struct{}),
	}
}

// WithOllamaURL overrides the Ollama /api/ps URL used for model residency polling.
// Call before Start(). The url must be the full URL including path, e.g.
// "http://gpu-server:11434/api/ps".
//
// This is necessary when Ollama runs on a non-default host or port so that
// OllamaModelLoaded in SystemState reflects the correct Ollama instance.
func (p *SystemPulse) WithOllamaURL(url string) *SystemPulse {
	p.ollamaURL = url
	return p
}

// Start launches the background sampling goroutine. It is safe to call
// multiple times — subsequent calls after the first are no-ops (sync.Once).
// A single initial sample is taken synchronously before the goroutine starts
// so that Current() always returns a non-zero SampledAt after Start() returns.
func (p *SystemPulse) Start() {
	p.startOnce.Do(func() {
		// Take one synchronous sample so Current() is immediately valid.
		p.sample()
		p.pollOllama()

		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.loop()
		}()
	})
}

// Stop signals the background goroutine to exit and waits for it to finish.
// It is safe to call multiple times and safe to call without a prior Start()
// — in both cases it returns promptly without blocking.
func (p *SystemPulse) Stop() {
	p.stopOnce.Do(func() {
		close(p.done)
	})
	p.wg.Wait() // no-op (counter=0) if Start() was never called
}

// Current returns a copy of the most recent SystemState snapshot.
// Safe for concurrent use; never blocks.
// Returns a zero-value SystemState (SampledAt.IsZero() == true) if called
// before Start().
func (p *SystemPulse) Current() SystemState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.current
}

// loop is the background sampling goroutine.
func (p *SystemPulse) loop() {
	sampleTick := time.NewTicker(pulseSampleInterval)
	ollamaTick := time.NewTicker(pulseOllamaInterval)
	defer sampleTick.Stop()
	defer ollamaTick.Stop()

	for {
		select {
		case <-p.done:
			return
		case <-sampleTick.C:
			p.sample()
		case <-ollamaTick.C:
			p.pollOllama()
		}
	}
}

// sample calls the platform-specific samplePlatform() and updates p.current.
// On error it logs a warning and retains the previous RAM/CPU values so that
// health classification does not oscillate due to transient read failures.
func (p *SystemPulse) sample() {
	ram, cpu, err := p.samplePlatform()
	if err != nil {
		logutil.Error("synapses: pulse: platform sample failed: %v", err)
		// Retain previous values; only update timestamp.
		p.mu.Lock()
		p.current.SampledAt = time.Now()
		p.current.Health = computeHealth(p.current.AvailableRAM, p.current.CPULoadNorm)
		p.mu.Unlock()
		return
	}

	// Clamp CPU to [0.0, 1.0] — loadavg can exceed 1.0 on heavily loaded systems.
	if cpu < 0 {
		cpu = 0
	}
	if cpu > 1.0 {
		cpu = 1.0
	}

	p.mu.Lock()
	p.current.AvailableRAM = ram
	p.current.CPULoadNorm = cpu
	p.current.SampledAt = time.Now()
	p.current.Health = computeHealth(ram, cpu)
	p.mu.Unlock()
}

// pollOllama queries the Ollama /api/ps endpoint to detect which model is
// currently loaded in RAM. On any error (Ollama not running, timeout, etc.)
// it silently sets OllamaModelLoaded to "".
func (p *SystemPulse) pollOllama() {
	model := ""
	defer func() {
		p.mu.Lock()
		p.current.OllamaModelLoaded = model
		p.mu.Unlock()
	}()

	resp, err := p.httpClient.Get(p.ollamaURL)
	if err != nil {
		// Ollama may not be running — this is expected and not an error.
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Drain body so the connection can be reused by the HTTP transport.
		_, _ = io.Copy(io.Discard, resp.Body)
		return
	}

	// Limit read to 64 KB — /api/ps responses are <1 KB in practice.
	// Guards against a rogue service on :11434 serving an arbitrarily large body.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return
	}

	var ps struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &ps); err != nil {
		return
	}
	if len(ps.Models) > 0 {
		model = ps.Models[0].Name
	}
}

// computeHealth derives a HealthLevel from RAM (bytes) and normalised CPU load.
// Thresholds per the design spec:
//
//	Green:  RAM > 3 GB AND CPU < 0.7
//	Yellow: RAM 1.5–3 GB OR CPU 0.7–0.9
//	Red:    RAM < 1.5 GB OR CPU > 0.9
func computeHealth(availableRAM int64, cpuLoadNorm float64) HealthLevel {
	ramRed := availableRAM < pulseRAMYellowThreshold
	ramYellow := availableRAM >= pulseRAMYellowThreshold && availableRAM < pulseRAMGreenThreshold
	cpuRed := cpuLoadNorm > pulseCPUYellowThreshold
	cpuYellow := cpuLoadNorm >= pulseCPUGreenThreshold && cpuLoadNorm <= pulseCPUYellowThreshold

	if ramRed || cpuRed {
		return HealthRed
	}
	if ramYellow || cpuYellow {
		return HealthYellow
	}
	return HealthGreen
}

// numCPUSafe returns runtime.NumCPU(), clamped to a minimum of 1 to avoid
// division-by-zero in platform implementations.
func numCPUSafe() float64 {
	n := runtime.NumCPU()
	if n < 1 {
		n = 1
	}
	return float64(n)
}
