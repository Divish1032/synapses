package peer

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── client.go tests ──────────────────────────────────────────────────────────

func TestNewPeerClient_LocalhostTLS(t *testing.T) {
	pc := NewPeerClient(config.PeerConfig{
		Name: "local",
		URL:  "https://localhost:9090",
	})
	if pc.Name() != "local" {
		t.Fatalf("expected name 'local', got %q", pc.Name())
	}
}

func TestNewPeerClient_NonLocalhostTLS(t *testing.T) {
	pc := NewPeerClient(config.PeerConfig{
		Name: "remote",
		URL:  "https://example.com",
	})
	if pc == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestPeerClient_Ping_Live(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/identity" {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(IdentityResponse{
			Project:   "test-repo",
			NodeCount: 42,
		})
	}))
	defer srv.Close()

	pc := NewPeerClient(config.PeerConfig{Name: "test", URL: srv.URL})
	id, err := pc.Ping()
	if err != nil {
		t.Fatal(err)
	}
	if id.Project != "test-repo" {
		t.Fatalf("unexpected project: %s", id.Project)
	}
	if id.NodeCount != 42 {
		t.Fatalf("unexpected node count: %d", id.NodeCount)
	}
}

func TestPeerClient_FetchDigest_Live(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/api-digest" {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode([]DigestEntry{
			{Name: "Foo", SigHash: "abc123"},
		})
	}))
	defer srv.Close()

	pc := NewPeerClient(config.PeerConfig{Name: "test", URL: srv.URL, Token: "tok"})
	entries, err := pc.FetchDigest()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "Foo" {
		t.Fatalf("unexpected digest: %+v", entries)
	}
}

func TestPeerClient_FetchDigest_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	pc := NewPeerClient(config.PeerConfig{Name: "test", URL: srv.URL, Token: "tok"})
	_, err := pc.FetchDigest()
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestPeerClient_QueryEntity_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()

	pc := NewPeerClient(config.PeerConfig{Name: "test", URL: srv.URL, Token: "tok"})
	_, err := pc.QueryEntity("Missing", 2)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected 'not found' error, got: %v", err)
	}
}

func TestPeerClient_QueryEntity_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()

	pc := NewPeerClient(config.PeerConfig{Name: "test", URL: srv.URL, Token: "tok"})
	_, err := pc.QueryEntity("Foo", 2)
	if err == nil {
		t.Fatal("expected error for 503")
	}
}

func TestPeerClient_FetchClaims_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]string{})
	}))
	defer srv.Close()

	pc := NewPeerClient(config.PeerConfig{Name: "test", URL: srv.URL, Token: "t"})
	claims, err := pc.FetchClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 0 {
		t.Fatalf("expected empty claims, got %d", len(claims))
	}
}

func TestPeerClient_FetchClaims_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
	}))
	defer srv.Close()

	pc := NewPeerClient(config.PeerConfig{Name: "test", URL: srv.URL, Token: "t"})
	_, err := pc.FetchClaims()
	if err == nil {
		t.Fatal("expected error for 403")
	}
}

func TestPeerClient_FetchAgents_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]interface{}{
			{"id": "agent-1", "presence": "active"},
		})
	}))
	defer srv.Close()

	pc := NewPeerClient(config.PeerConfig{Name: "test", URL: srv.URL, Token: "t"})
	agents, err := pc.FetchAgents()
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
}

func TestPeerClient_FetchAgents_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	pc := NewPeerClient(config.PeerConfig{Name: "test", URL: srv.URL, Token: "t"})
	_, err := pc.FetchAgents()
	if err == nil {
		t.Fatal("expected error for 500")
	}
}

func TestPeerClient_BroadcastIntent_Success(t *testing.T) {
	received := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = true
		w.WriteHeader(200)
	}))
	defer srv.Close()

	pc := NewPeerClient(config.PeerConfig{Name: "test", URL: srv.URL, Token: "t"})
	err := pc.BroadcastIntent(IntentMessage{AgentID: "a1", Scope: "pkg/foo"})
	if err != nil {
		t.Fatal(err)
	}
	if !received {
		t.Fatal("server should have received the intent")
	}
}

func TestPeerClient_NewRequest_WithToken(t *testing.T) {
	pc := NewPeerClient(config.PeerConfig{Name: "t", URL: "http://localhost", Token: "secret"})
	req, err := pc.newRequest(http.MethodGet, "/test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "Bearer secret" {
		t.Fatal("token should be set in Authorization header")
	}
}

func TestPeerClient_NewRequest_WithoutToken(t *testing.T) {
	pc := NewPeerClient(config.PeerConfig{Name: "t", URL: "http://localhost"})
	req, err := pc.newRequest(http.MethodGet, "/test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "" {
		t.Fatal("no token means no Authorization header")
	}
}

// ── digest.go tests ──────────────────────────────────────────────────────────

func TestComputeIntersection_WithMatch(t *testing.T) {
	g := graph.New("test")
	sig := "func Foo()"
	h := sha256.Sum256([]byte(sig))
	sigHash := fmt.Sprintf("%x", h[:8])

	g.AddNode(&graph.Node{
		ID:       "test::foo.go::Foo",
		Name:     "Foo",
		Type:     graph.NodeFunction,
		Exported: true,
		Metadata: map[string]string{"signature": sig},
	})

	digest := []DigestEntry{{Name: "Foo", SigHash: sigHash}}
	shared := ComputeIntersection(g, digest)
	if len(shared) != 1 || shared[0] != "Foo" {
		t.Fatalf("expected [Foo], got %v", shared)
	}
}

func TestComputeIntersection_SkipsNonExported(t *testing.T) {
	g := graph.New("test")
	g.AddNode(&graph.Node{
		ID:       "test::foo.go::foo",
		Name:     "foo",
		Type:     graph.NodeFunction,
		Exported: false,
		Metadata: map[string]string{"signature": "func foo()"},
	})

	h := sha256.Sum256([]byte("func foo()"))
	digest := []DigestEntry{{Name: "foo", SigHash: fmt.Sprintf("%x", h[:8])}}
	shared := ComputeIntersection(g, digest)
	if len(shared) != 0 {
		t.Fatalf("unexported nodes should not match: %v", shared)
	}
}

func TestComputeIntersection_SkipsFileAndPackageNodes(t *testing.T) {
	g := graph.New("test")
	g.AddNode(&graph.Node{
		ID:       "test::main.go",
		Name:     "main.go",
		Type:     graph.NodeFile,
		Exported: true,
	})

	digest := []DigestEntry{{Name: "main.go", SigHash: "anything"}}
	shared := ComputeIntersection(g, digest)
	if len(shared) != 0 {
		t.Fatalf("file nodes should not match: %v", shared)
	}
}

func TestComputeIntersection_DeduplicatesNames(t *testing.T) {
	g := graph.New("test")
	sig := "func Bar()"
	h := sha256.Sum256([]byte(sig))
	sigHash := fmt.Sprintf("%x", h[:8])

	g.AddNode(&graph.Node{
		ID: "test::a.go::Bar", Name: "Bar", Type: graph.NodeFunction,
		Exported: true, Metadata: map[string]string{"signature": sig},
	})

	// Two digest entries with the same hash
	digest := []DigestEntry{
		{Name: "Bar", SigHash: sigHash},
		{Name: "Bar", SigHash: sigHash},
	}
	shared := ComputeIntersection(g, digest)
	if len(shared) != 1 {
		t.Fatalf("should deduplicate: got %v", shared)
	}
}

// ── intents.go tests ─────────────────────────────────────────────────────────

func TestGenerateStableID_Format(t *testing.T) {
	id := generateStableID()
	// UUID v4 format: 8-4-4-4-12
	parts := strings.Split(id, "-")
	if len(parts) != 5 {
		t.Fatalf("expected 5 parts in UUID, got %d: %s", len(parts), id)
	}
	if len(parts[0]) != 8 || len(parts[1]) != 4 || len(parts[2]) != 4 || len(parts[3]) != 4 || len(parts[4]) != 12 {
		t.Fatalf("unexpected UUID part lengths: %s", id)
	}
}

func TestGenerateStableID_Unique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := generateStableID()
		if seen[id] {
			t.Fatalf("duplicate ID: %s", id)
		}
		seen[id] = true
	}
}

// ── manager.go tests ─────────────────────────────────────────────────────────

func TestScopesOverlap_TableDriven(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"pkg/auth", "pkg/auth", true},
		{"pkg/auth", "pkg/auth/jwt", true},
		{"pkg/auth/jwt", "pkg/auth", true},
		{"pkg/auth", "pkg/graph", false},
		{"pkg/auth", "pkg/authorize", false}, // prefix but not child
		{"", "", true},
	}
	for _, tc := range tests {
		got := ScopesOverlap(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("ScopesOverlap(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestPeerManager_GetStatuses_Empty(t *testing.T) {
	pm := NewPeerManager(&config.Config{}, graph.New("test"), nil)
	statuses := pm.GetStatuses()
	if len(statuses) != 0 {
		t.Fatalf("expected empty statuses, got %d", len(statuses))
	}
}

func TestPeerManager_GetClient_WithMultiplePeers(t *testing.T) {
	cfg := &config.Config{
		Peers: []config.PeerConfig{
			{Name: "alpha", URL: "http://localhost:1"},
			{Name: "beta", URL: "http://localhost:2"},
		},
	}
	pm := NewPeerManager(cfg, graph.New("test"), nil)

	c, err := pm.GetClient("beta")
	if err != nil {
		t.Fatal(err)
	}
	if c.Name() != "beta" {
		t.Fatalf("expected beta, got %s", c.Name())
	}
}

func TestPeerManager_FetchAllPeerClaims_NoPeers(t *testing.T) {
	pm := NewPeerManager(&config.Config{}, graph.New("test"), nil)
	claims := pm.FetchAllPeerClaims(nil)
	if len(claims) != 0 {
		t.Fatalf("expected empty, got %d", len(claims))
	}
}

func TestPeerManager_FetchAllPeerClaims_UnconnectedSkipped(t *testing.T) {
	cfg := &config.Config{
		Peers: []config.PeerConfig{{Name: "p1", URL: "http://bad:1"}},
	}
	pm := NewPeerManager(cfg, graph.New("test"), nil)
	// No Connect() called, so statuses are empty → peers treated as unconnected
	claims := pm.FetchAllPeerClaims(nil)
	if len(claims) != 0 {
		t.Fatalf("unconnected peers should be skipped, got %d entries", len(claims))
	}
}

func TestPeerManager_Connect_WithLiveServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/identity":
			json.NewEncoder(w).Encode(IdentityResponse{Project: "peer-repo", NodeCount: 5})
		case "/api/v1/api-digest":
			json.NewEncoder(w).Encode([]DigestEntry{})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		Peers: []config.PeerConfig{{Name: "live", URL: srv.URL, Token: "t"}},
	}
	pm := NewPeerManager(cfg, graph.New("test"), nil)
	statuses := pm.Connect()

	if len(statuses) != 1 {
		t.Fatalf("expected 1 status, got %d", len(statuses))
	}
	if !statuses[0].Connected {
		t.Fatalf("expected connected, got error: %s", statuses[0].Error)
	}
	if statuses[0].NodeCount != 5 {
		t.Fatalf("expected 5 nodes, got %d", statuses[0].NodeCount)
	}
}

func TestPeerManager_Stop_MultipleCalls(t *testing.T) {
	pm := NewPeerManager(&config.Config{}, graph.New("test"), nil)
	pm.Stop()
	pm.Stop() // should not panic
}

func TestPeerManager_SyncPeerAgents_NilStore(t *testing.T) {
	pm := NewPeerManager(&config.Config{}, graph.New("test"), nil)
	// Should not panic with nil store
	pc := NewPeerClient(config.PeerConfig{Name: "x", URL: "http://bad"})
	pm.syncPeerAgents("x", pc)
}
