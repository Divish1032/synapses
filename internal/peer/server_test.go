package peer_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/peer"
)

// buildServerGraph creates a small graph for server handler tests.
func buildServerGraph(t *testing.T) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	fnID := g.MakeNodeID("svc.go", "Serve")
	g.AddNode(&graph.Node{
		ID: fnID, Type: graph.NodeFunction, Name: "Serve", Package: "svc",
		File: "svc.go", Exported: true,
		Metadata: map[string]string{"signature": "func(w http.ResponseWriter, r *http.Request)"},
	})
	fnID2 := g.MakeNodeID("svc.go", "internal")
	g.AddNode(&graph.Node{
		ID: fnID2, Type: graph.NodeFunction, Name: "internal", Package: "svc",
		File: "svc.go", Exported: false,
	})
	return g
}

func buildCfg(token string) *config.Config {
	c := &config.Config{PeerAPIToken: token}
	return c
}

func TestHandleIdentity_NoAuth(t *testing.T) {
	g := buildServerGraph(t)
	ps := peer.NewPeerServer(g, buildCfg("secret"), nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/identity", nil)
	w := httptest.NewRecorder()
	ps.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("identity status = %d, want 200", w.Code)
	}
	var resp peer.IdentityResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode identity: %v", err)
	}
	if resp.Project != "testrepo" {
		t.Errorf("Project = %q, want %q", resp.Project, "testrepo")
	}
	if len(resp.Capabilities) == 0 {
		t.Error("expected non-empty capabilities")
	}
}

func TestAuth_BlocksUnauthenticated(t *testing.T) {
	g := buildServerGraph(t)
	ps := peer.NewPeerServer(g, buildCfg("secret"), nil)

	for _, path := range []string{
		"/api/v1/api-digest",
		"/api/v1/api-surface",
		"/api/v1/claims",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		ps.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s without token: status = %d, want 401", path, w.Code)
		}
	}
}

func TestAuth_AllowsValidToken(t *testing.T) {
	g := buildServerGraph(t)
	ps := peer.NewPeerServer(g, buildCfg("secret"), nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/api-digest", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	ps.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("digest with valid token: status = %d, want 200", w.Code)
	}
}

func TestHandleApiDigest_OnlyExported(t *testing.T) {
	g := buildServerGraph(t)
	ps := peer.NewPeerServer(g, buildCfg("tok"), nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/api-digest", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	ps.Handler().ServeHTTP(w, req)

	var entries []peer.DigestEntry
	if err := json.NewDecoder(w.Body).Decode(&entries); err != nil {
		t.Fatalf("decode digest: %v", err)
	}
	for _, e := range entries {
		if e.Name == "internal" {
			t.Error("digest should not include unexported functions")
		}
	}
	found := false
	for _, e := range entries {
		if e.Name == "Serve" {
			found = true
			if len(e.SigHash) != 16 {
				t.Errorf("SigHash length = %d, want 16 hex chars", len(e.SigHash))
			}
		}
	}
	if !found {
		t.Error("digest should include exported 'Serve' function")
	}
}

func TestHandleQuery_KnownEntity(t *testing.T) {
	g := buildServerGraph(t)
	ps := peer.NewPeerServer(g, buildCfg("tok"), nil)

	body, _ := json.Marshal(peer.QueryRequest{Entity: "Serve", Depth: 1})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/query", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	ps.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("query status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var sub graph.SubGraph
	if err := json.NewDecoder(w.Body).Decode(&sub); err != nil {
		t.Fatalf("decode subgraph: %v", err)
	}
	if len(sub.Nodes) == 0 {
		t.Error("expected at least 1 node in subgraph")
	}
}

func TestHandleQuery_UnknownEntity(t *testing.T) {
	g := buildServerGraph(t)
	ps := peer.NewPeerServer(g, buildCfg("tok"), nil)

	body, _ := json.Marshal(peer.QueryRequest{Entity: "DoesNotExist"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/query", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	ps.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("unknown entity: status = %d, want 404", w.Code)
	}
}

func TestHandleClaims_NoStore(t *testing.T) {
	g := buildServerGraph(t)
	ps := peer.NewPeerServer(g, buildCfg("tok"), nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/claims", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	ps.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("claims status = %d, want 200", w.Code)
	}
}

func TestHandleAgents_NoStore(t *testing.T) {
	g := buildServerGraph(t)
	ps := peer.NewPeerServer(g, buildCfg("tok"), nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	ps.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("agents status = %d, want 200", w.Code)
	}
	// Response must be an empty array, never null.
	var result []interface{}
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result == nil {
		t.Errorf("response body must be [] not null")
	}
}

func TestHandleAgents_RequiresAuth(t *testing.T) {
	g := buildServerGraph(t)
	ps := peer.NewPeerServer(g, buildCfg("secret"), nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	// No auth header — must be rejected.
	w := httptest.NewRecorder()
	ps.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without auth, got %d", w.Code)
	}
}
