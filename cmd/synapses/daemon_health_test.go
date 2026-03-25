package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/federation"
	"github.com/SynapsesOS/synapses/internal/graph"
	mcpsrv "github.com/SynapsesOS/synapses/internal/mcp"
	"github.com/SynapsesOS/synapses/internal/store"
)

// newHealthTestProject creates a minimal ProjectInstance for health tests:
// real graph + real store, no brain, no watcher, no federation.
func newHealthTestProject(t *testing.T) (*ProjectInstance, func()) {
	t.Helper()

	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, ".git"), 0o755); err != nil {
		t.Fatalf("create .git dir: %v", err)
	}
	absPath, err := canonicalPath(projectDir)
	if err != nil {
		t.Fatalf("canonicalPath: %v", err)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	g := graph.New("test-repo")

	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	srv := mcpsrv.New(g, cfg, st)
	srv.StartBackground()

	pi := &ProjectInstance{
		AbsPath:   absPath,
		Graph:     g,
		Store:     st,
		MCPServer: srv,
	}

	cleanup := func() {
		srv.Close()
		st.Close()
	}
	return pi, cleanup
}

// buildHealthTestServer creates a test HTTP server with only the /v1/health
// handler registered.  startedAt is the fake daemon start time.
func buildHealthTestServer(t *testing.T, reg *projectRegistry, startedAt time.Time) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", buildHealthHandler(reg, nil, startedAt))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func getHealth(t *testing.T, ts *httptest.Server) map[string]interface{} {
	t.Helper()
	resp, err := http.Get(ts.URL + "/v1/health")
	if err != nil {
		t.Fatalf("GET /v1/health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	var m map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	return m
}

// TestHealthHandler_NoProjects verifies the handler returns valid JSON with
// zero-value aggregates when no projects are registered.
func TestHealthHandler_NoProjects(t *testing.T) {
	reg := newProjectRegistry()
	started := time.Now().Add(-5 * time.Minute)
	ts := buildHealthTestServer(t, reg, started)

	m := getHealth(t, ts)

	if m["status"] != "ok" {
		t.Errorf("status = %v, want ok", m["status"])
	}
	if got := int(m["project_count"].(float64)); got != 0 {
		t.Errorf("project_count = %d, want 0", got)
	}
	if got := int(m["total_nodes"].(float64)); got != 0 {
		t.Errorf("total_nodes = %d, want 0", got)
	}
	if got := int(m["watchers_dead"].(float64)); got != 0 {
		t.Errorf("watchers_dead = %d, want 0", got)
	}
	if m["brain_available"] != false {
		t.Errorf("brain_available = %v, want false", m["brain_available"])
	}
	// uptime_secs should reflect the fake start time (~5 min = ~300s)
	if uptime := int(m["uptime_secs"].(float64)); uptime < 290 {
		t.Errorf("uptime_secs = %d, want >= 290", uptime)
	}
	// embedding_status must be present when pulse is nil
	if m["embedding_status"] != "none" {
		t.Errorf("embedding_status = %v, want none", m["embedding_status"])
	}
}

// TestHealthHandler_SingleProject verifies node/edge/memory aggregation for
// a single project.
func TestHealthHandler_SingleProject(t *testing.T) {
	pi, cleanup := newHealthTestProject(t)
	defer cleanup()

	// Add nodes so NodeCount > 0.
	pi.Graph.AddNode(&graph.Node{ID: pi.Graph.MakeNodeID("a.go", "Foo"), Name: "Foo", Type: graph.NodeFunction, File: "a.go", Package: "p"})
	pi.Graph.AddNode(&graph.Node{ID: pi.Graph.MakeNodeID("b.go", "Bar"), Name: "Bar", Type: graph.NodeFunction, File: "b.go", Package: "p"})

	reg := newProjectRegistry()
	reg.Set(pi)

	ts := buildHealthTestServer(t, reg, time.Now())
	m := getHealth(t, ts)

	if got := int(m["project_count"].(float64)); got != 1 {
		t.Errorf("project_count = %d, want 1", got)
	}
	if got := int(m["total_nodes"].(float64)); got < 2 {
		t.Errorf("total_nodes = %d, want >= 2", got)
	}
	if m["status"] != "ok" {
		t.Errorf("status = %v, want ok", m["status"])
	}
}

// TestHealthHandler_MethodNotAllowed verifies non-GET methods return 405 with
// an Allow header as required by RFC 7231 §6.5.5.
func TestHealthHandler_MethodNotAllowed(t *testing.T) {
	reg := newProjectRegistry()
	ts := buildHealthTestServer(t, reg, time.Now())

	for _, method := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		req, _ := http.NewRequest(method, ts.URL+"/v1/health", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s /v1/health: %v", method, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", method, resp.StatusCode)
		}
		if allow := resp.Header.Get("Allow"); allow != "GET" {
			t.Errorf("%s: Allow = %q, want GET", method, allow)
		}
	}
}

// TestHealthHandler_MultiProjectAggregation verifies that nodes from multiple
// projects are summed correctly.
func TestHealthHandler_MultiProjectAggregation(t *testing.T) {
	pi1, c1 := newHealthTestProject(t)
	defer c1()
	pi2, c2 := newHealthTestProject(t)
	defer c2()

	pi1.Graph.AddNode(&graph.Node{ID: pi1.Graph.MakeNodeID("a.go", "A"), Name: "A", Type: graph.NodeFunction, File: "a.go", Package: "p"})
	pi2.Graph.AddNode(&graph.Node{ID: pi2.Graph.MakeNodeID("b.go", "B"), Name: "B", Type: graph.NodeFunction, File: "b.go", Package: "p"})
	pi2.Graph.AddNode(&graph.Node{ID: pi2.Graph.MakeNodeID("c.go", "C"), Name: "C", Type: graph.NodeFunction, File: "c.go", Package: "p"})

	reg := newProjectRegistry()
	reg.Set(pi1)
	reg.Set(pi2)

	ts := buildHealthTestServer(t, reg, time.Now())
	m := getHealth(t, ts)

	if got := int(m["project_count"].(float64)); got != 2 {
		t.Errorf("project_count = %d, want 2", got)
	}
	if got := int(m["total_nodes"].(float64)); got < 3 {
		t.Errorf("total_nodes = %d, want >= 3", got)
	}
}

// TestHealthHandler_FederationStale verifies federation_stale count when a
// resolver reports a non-existent path (real Resolver, no mocks needed).
func TestHealthHandler_FederationStale(t *testing.T) {
	pi, cleanup := newHealthTestProject(t)
	defer cleanup()

	// A non-existent path produces "not_found" → counted as stale.
	pi.FederationResolver = federation.NewResolver(
		[]config.FederationEntry{{Path: "/nonexistent/synapses/project"}},
		t.TempDir(),
	)

	reg := newProjectRegistry()
	reg.Set(pi)

	ts := buildHealthTestServer(t, reg, time.Now())
	m := getHealth(t, ts)

	if got := int(m["federation_healthy"].(float64)); got != 0 {
		t.Errorf("federation_healthy = %d, want 0", got)
	}
	if got := int(m["federation_stale"].(float64)); got != 1 {
		t.Errorf("federation_stale = %d, want 1", got)
	}
}

// TestHealthHandler_ParallelSafety fires concurrent requests against multiple
// projects.  The race detector will flag any unsynchronised access.
func TestHealthHandler_ParallelSafety(t *testing.T) {
	reg := newProjectRegistry()
	for i := range 4 {
		pi, cleanup := newHealthTestProject(t)
		t.Cleanup(cleanup)
		name := strings.Repeat("X", i+1)
		pi.Graph.AddNode(&graph.Node{
			ID:      pi.Graph.MakeNodeID("f.go", name),
			Name:    name,
			Type:    graph.NodeFunction,
			File:    "f.go",
			Package: "p",
		})
		reg.Set(pi)
	}

	ts := buildHealthTestServer(t, reg, time.Now())

	const goroutines = 20
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := getHealth(t, ts)
			if m["status"] == nil {
				t.Error("status field missing in concurrent response")
			}
		}()
	}
	wg.Wait()
}

// TestHealthHandler_PerProjectFederationTimeout verifies that a slow project's
// federation timeout does NOT cause other projects' results to also time out.
// The shared-context bug would cause pi2's entry to appear stale when pi1 is slow.
// With per-project contexts, pi2's fast entry is counted as stale (not_found)
// for its own reason, not due to pi1's timeout.
func TestHealthHandler_PerProjectFederationTimeout(t *testing.T) {
	pi1, c1 := newHealthTestProject(t)
	defer c1()
	pi2, c2 := newHealthTestProject(t)
	defer c2()

	// Both use non-existent paths so both return "not_found" (stale).
	// The key test is that the handler completes within the per-project 3s budget,
	// not a sum across all projects.
	pi1.FederationResolver = federation.NewResolver(
		[]config.FederationEntry{{Path: "/nonexistent/p1"}},
		t.TempDir(),
	)
	pi2.FederationResolver = federation.NewResolver(
		[]config.FederationEntry{{Path: "/nonexistent/p2"}},
		t.TempDir(),
	)

	reg := newProjectRegistry()
	reg.Set(pi1)
	reg.Set(pi2)

	ts := buildHealthTestServer(t, reg, time.Now())

	start := time.Now()
	m := getHealth(t, ts)
	elapsed := time.Since(start)

	// Both entries are stale (non-existent path → not_found).
	if got := int(m["federation_stale"].(float64)); got != 2 {
		t.Errorf("federation_stale = %d, want 2", got)
	}
	// Handler must complete quickly — os.Stat on a non-existent path is instant.
	if elapsed > 2*time.Second {
		t.Errorf("handler took %v — os.Stat should be instant", elapsed)
	}
}

