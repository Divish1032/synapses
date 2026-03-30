//go:build loadtest

package loadtest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// HTTPLoadConfig controls HTTP load testing parameters.
type HTTPLoadConfig struct {
	// Endpoint is the full URL to test (e.g. "http://localhost:7437/mcp").
	Endpoint string

	// Method is the HTTP method (default POST).
	Method string

	// Body is the request body template. Sent for every request.
	Body string

	// RPSTiers are the RPS levels to test sequentially (e.g. [50, 100, 200]).
	RPSTiers []int

	// Duration is how long to sustain each RPS tier.
	Duration time.Duration

	// WarmupDuration is how long to warm up before measuring at each tier.
	WarmupDuration time.Duration

	// Timeout per individual request.
	Timeout time.Duration
}

// DefaultHTTPLoadConfig returns sensible defaults for MCP endpoint testing.
func DefaultHTTPLoadConfig(endpoint string) HTTPLoadConfig {
	return HTTPLoadConfig{
		Endpoint:       endpoint,
		Method:         "POST",
		Body:           `{"jsonrpc":"2.0","method":"tools/list","id":1}`,
		RPSTiers:       []int{50, 100, 200},
		Duration:       30 * time.Second,
		WarmupDuration: 5 * time.Second,
		Timeout:        10 * time.Second,
	}
}

// RunHTTPLoad executes the HTTP load test across all configured RPS tiers.
func RunHTTPLoad(cfg HTTPLoadConfig) ([]HTTPResult, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.Method == "" {
		cfg.Method = "POST"
	}

	client := &http.Client{
		Timeout: cfg.Timeout,
		Transport: &http.Transport{
			MaxIdleConns:        500,
			MaxIdleConnsPerHost: 500,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	results := make([]HTTPResult, 0, len(cfg.RPSTiers))

	for _, rps := range cfg.RPSTiers {
		result, err := runTier(client, cfg, rps)
		if err != nil {
			return results, fmt.Errorf("rps=%d: %w", rps, err)
		}
		results = append(results, result)
	}

	return results, nil
}

func runTier(client *http.Client, cfg HTTPLoadConfig, rps int) (HTTPResult, error) {
	totalDur := cfg.WarmupDuration + cfg.Duration
	ctx, cancel := context.WithTimeout(context.Background(), totalDur+5*time.Second)
	defer cancel()

	var (
		mu        sync.Mutex
		latencies []time.Duration
		errCount  atomic.Int64
	)

	warmupEnd := time.Now().Add(cfg.WarmupDuration)
	deadline := time.Now().Add(totalDur)

	// Use a ticker-based rate limiter: one tick per request at the target RPS.
	// A central goroutine dispatches work tokens on a buffered channel.
	interval := time.Duration(float64(time.Second) / float64(rps))
	tokens := make(chan struct{}, rps) // buffer up to 1 second of burst

	// Ticker goroutine.
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				select {
				case tokens <- struct{}{}:
				default: // drop if workers are saturated
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	var wg sync.WaitGroup

	// Worker count: enough to handle concurrent in-flight requests.
	workers := rps
	if workers > 500 {
		workers = 500
	}
	if workers < 1 {
		workers = 1
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if time.Now().After(deadline) {
					return
				}
				select {
				case <-tokens:
				case <-ctx.Done():
					return
				}

				start := time.Now()
				req, err := http.NewRequestWithContext(ctx, cfg.Method, cfg.Endpoint,
					strings.NewReader(cfg.Body))
				if err != nil {
					errCount.Add(1)
					continue
				}
				req.Header.Set("Content-Type", "application/json")

				resp, err := client.Do(req)
				elapsed := time.Since(start)

				if err != nil {
					errCount.Add(1)
					continue
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()

				isError := resp.StatusCode >= 400

				// Only record measurements after warmup.
				if time.Now().After(warmupEnd) {
					mu.Lock()
					latencies = append(latencies, elapsed)
					if isError {
						errCount.Add(1)
					}
					mu.Unlock()
				}
			}
		}()
	}

	wg.Wait()

	mu.Lock()
	defer mu.Unlock()

	SortDurations(latencies)

	result := HTTPResult{
		RPS:      rps,
		Duration: cfg.Duration,
		Samples:  len(latencies),
	}

	if len(latencies) > 0 {
		result.P50 = Percentile(latencies, 0.50)
		result.P95 = Percentile(latencies, 0.95)
		result.P99 = Percentile(latencies, 0.99)
		result.Max = latencies[len(latencies)-1]
	}

	total := int64(len(latencies)) + errCount.Load()
	if total > 0 {
		result.ErrorRate = float64(errCount.Load()) / float64(total)
	}

	return result, nil
}
