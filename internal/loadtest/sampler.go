//go:build loadtest

package loadtest

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

const defaultSampleInterval = 10 * time.Millisecond

// Snapshot holds a point-in-time memory and goroutine reading.
type Snapshot struct {
	HeapInuse  int64
	TotalAlloc int64
	Mallocs    int64
	NumGC      uint32
	PauseTotal time.Duration
	Goroutines int
}

// readSnapshot fills a Snapshot from current runtime state.
func readSnapshot() Snapshot {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return Snapshot{
		HeapInuse:  int64(ms.HeapInuse),
		TotalAlloc: int64(ms.TotalAlloc),
		Mallocs:    int64(ms.Mallocs),
		NumGC:      ms.NumGC,
		PauseTotal: time.Duration(ms.PauseTotalNs),
		Goroutines: runtime.NumGoroutine(),
	}
}

// Sampler polls runtime memory stats at a fixed interval and tracks
// peak heap usage. It is goroutine-safe.
type Sampler struct {
	interval time.Duration
	peakHeap atomic.Int64
	stop     chan struct{}
	wg       sync.WaitGroup
}

// NewSampler creates a sampler with the given polling interval.
// Pass 0 to use the default 10ms interval.
func NewSampler(interval time.Duration) *Sampler {
	if interval <= 0 {
		interval = defaultSampleInterval
	}
	return &Sampler{interval: interval}
}

// Start begins background sampling. It is safe to call Start on an
// already-running sampler (it will be a no-op).
func (s *Sampler) Start() {
	if s.stop != nil {
		return // already running
	}
	s.stop = make(chan struct{})
	s.peakHeap.Store(0)
	s.wg.Add(1)
	go s.loop()
}

// Stop halts background sampling and blocks until the sampling goroutine exits.
func (s *Sampler) Stop() {
	if s.stop == nil {
		return
	}
	close(s.stop)
	s.wg.Wait()
	s.stop = nil
}

// PeakHeapInuse returns the maximum HeapInuse observed during sampling.
func (s *Sampler) PeakHeapInuse() int64 {
	return s.peakHeap.Load()
}

func (s *Sampler) loop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	var ms runtime.MemStats
	for {
		select {
		case <-ticker.C:
			runtime.ReadMemStats(&ms)
			heap := int64(ms.HeapInuse)
			for {
				cur := s.peakHeap.Load()
				if heap <= cur {
					break
				}
				if s.peakHeap.CompareAndSwap(cur, heap) {
					break
				}
			}
		case <-s.stop:
			return
		}
	}
}
