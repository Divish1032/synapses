package peer_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/peer"
)

// ── ScopesOverlap ─────────────────────────────────────────────────────────────

func TestScopesOverlap_Exact(t *testing.T) {
	if !peer.ScopesOverlap("pkg/auth", "pkg/auth") {
		t.Error("exact match should overlap")
	}
}

func TestScopesOverlap_AisChildOfB(t *testing.T) {
	if !peer.ScopesOverlap("pkg/auth/middleware", "pkg/auth") {
		t.Error("child of b should overlap with b")
	}
}

func TestScopesOverlap_BisChildOfA(t *testing.T) {
	if !peer.ScopesOverlap("pkg/auth", "pkg/auth/middleware") {
		t.Error("a should overlap with child of a")
	}
}

func TestScopesOverlap_NoOverlap(t *testing.T) {
	if peer.ScopesOverlap("pkg/auth", "pkg/billing") {
		t.Error("unrelated scopes should not overlap")
	}
}

func TestScopesOverlap_PrefixButNotChild(t *testing.T) {
	// "pkg/auth-extra" must NOT match "pkg/auth" — no "/" separator after prefix.
	if peer.ScopesOverlap("pkg/auth-extra", "pkg/auth") {
		t.Error("prefix-only match without path separator should not overlap")
	}
}

func TestScopesOverlap_Empty(t *testing.T) {
	// Two empty strings are equal → overlap.
	if !peer.ScopesOverlap("", "") {
		t.Error("two empty scopes should overlap (both equal)")
	}
}

// ── PeerManager lifecycle ─────────────────────────────────────────────────────

func TestNewPeerManager_NoPeers(t *testing.T) {
	cfg := &config.Config{}
	g := graph.New("test")
	pm := peer.NewPeerManager(cfg, g, nil)
	if pm == nil {
		t.Fatal("expected non-nil PeerManager")
	}
	statuses := pm.GetStatuses()
	if len(statuses) != 0 {
		t.Errorf("expected 0 statuses for empty peer config, got %d", len(statuses))
	}
}

func TestPeerManager_GetClient_NotFound(t *testing.T) {
	cfg := &config.Config{}
	g := graph.New("test")
	pm := peer.NewPeerManager(cfg, g, nil)
	_, err := pm.GetClient("ghost")
	if err == nil {
		t.Error("expected error for unknown peer name")
	}
}

func TestPeerManager_GetClient_Found(t *testing.T) {
	peerCfg := config.PeerConfig{
		Name:  "known-peer",
		URL:   "http://localhost:12345",
		Token: "tok",
	}
	g := graph.New("test")
	pm := peer.NewPeerManager(&config.Config{Peers: []config.PeerConfig{peerCfg}}, g, nil)
	cli, err := pm.GetClient("known-peer")
	if err != nil {
		t.Fatalf("GetClient: %v", err)
	}
	if cli.Name() != "known-peer" {
		t.Errorf("expected Name='known-peer', got %q", cli.Name())
	}
}

func TestPeerManager_Stop_Idempotent(t *testing.T) {
	cfg := &config.Config{}
	g := graph.New("test")
	pm := peer.NewPeerManager(cfg, g, nil)
	pm.Stop()
	pm.Stop() // second call must not panic (uses sync.Once)
}

func TestPeerManager_Connect_WithRealServer(t *testing.T) {
	gA := graph.New("remote-proj")
	cfgA := &config.Config{PeerAPIToken: "tok"}
	psA := peer.NewPeerServer(gA, cfgA, nil)

	srv := httptest.NewServer(psA.Handler())
	defer srv.Close()

	peerCfg := config.PeerConfig{
		Name:       "remote-proj",
		URL:        srv.URL,
		Token:      "tok",
		TrustLevel: "full",
	}
	gLocal := graph.New("local")
	pm := peer.NewPeerManager(&config.Config{Peers: []config.PeerConfig{peerCfg}}, gLocal, nil)

	statuses := pm.Connect()
	if len(statuses) != 1 {
		t.Fatalf("expected 1 status, got %d", len(statuses))
	}
	if !statuses[0].Connected {
		t.Errorf("expected connected=true, got error: %q", statuses[0].Error)
	}

	// GetStatuses should return the cached result.
	cached := pm.GetStatuses()
	if len(cached) != 1 || !cached[0].Connected {
		t.Error("GetStatuses should return cached connected status")
	}
}

func TestPeerManager_Connect_UnreachablePeer(t *testing.T) {
	peerCfg := config.PeerConfig{
		Name:  "dead-peer",
		URL:   "http://127.0.0.1:19999", // nothing listening
		Token: "tok",
	}
	g := graph.New("local")
	pm := peer.NewPeerManager(&config.Config{Peers: []config.PeerConfig{peerCfg}}, g, nil)
	statuses := pm.Connect()
	if len(statuses) != 1 {
		t.Fatalf("expected 1 status")
	}
	if statuses[0].Connected {
		t.Error("expected not connected for unreachable peer")
	}
	if statuses[0].Error == "" {
		t.Error("expected non-empty error for unreachable peer")
	}
}

func TestPeerManager_FetchAllPeerClaims_NotConnected(t *testing.T) {
	peerCfg := config.PeerConfig{
		Name:  "offline",
		URL:   "http://127.0.0.1:19998",
		Token: "tok",
	}
	g := graph.New("test")
	pm := peer.NewPeerManager(&config.Config{Peers: []config.PeerConfig{peerCfg}}, g, nil)
	// Don't call Connect — statuses empty → peer treated as not connected.
	result := pm.FetchAllPeerClaims(nil)
	if result == nil {
		t.Error("expected non-nil empty map (not nil)")
	}
	if len(result) != 0 {
		t.Errorf("expected empty map for unconnected peer, got %d entries", len(result))
	}
}

func TestPeerManager_BroadcastIntent_NoPeers_NoStore(t *testing.T) {
	cfg := &config.Config{}
	g := graph.New("test")
	pm := peer.NewPeerManager(cfg, g, nil)
	// Fire-and-forget with no peers and no store — must not panic.
	pm.BroadcastIntent("agent-a", "pkg/auth", "directory")
	// Brief wait for any goroutines to drain.
	time.Sleep(10 * time.Millisecond)
}

// ── PeerServer — handleApiSurface ─────────────────────────────────────────────

func TestHandleApiSurface_ReturnsHTTPEndpoints(t *testing.T) {
	g := buildServerGraph(t) // Serve has http.ResponseWriter signature
	ps := peer.NewPeerServer(g, buildCfg("tok"), nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/api-surface", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	ps.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("api-surface status = %d, want 200", w.Code)
	}
	var endpoints []map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&endpoints); err != nil {
		t.Fatalf("decode endpoints: %v", err)
	}
	found := false
	for _, ep := range endpoints {
		if ep["name"] == "Serve" {
			found = true
			if ep["framework"] != "net/http" {
				t.Errorf("framework = %v, want net/http", ep["framework"])
			}
		}
	}
	if !found {
		t.Error("expected Serve (net/http) in api-surface endpoints")
	}
}

func TestHandleApiSurface_GinEndpoint(t *testing.T) {
	g := graph.New("testrepo")
	ginID := g.MakeNodeID("router.go", "GetUser")
	g.AddNode(&graph.Node{
		ID:       ginID,
		Type:     graph.NodeFunction,
		Name:     "GetUser",
		Package:  "api",
		File:     "router.go",
		Exported: true,
		Metadata: map[string]string{"signature": "func(c *gin.Context)"},
	})
	ps := peer.NewPeerServer(g, buildCfg("tok"), nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/api-surface", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	ps.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("api-surface status = %d, want 200", w.Code)
	}
	var endpoints []map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&endpoints); err != nil {
		t.Fatalf("decode endpoints: %v", err)
	}
	found := false
	for _, ep := range endpoints {
		if ep["name"] == "GetUser" && ep["framework"] == "gin" {
			found = true
		}
	}
	if !found {
		t.Error("expected GetUser (gin) in api-surface endpoints")
	}
}

func TestHandleApiSurface_EmptyGraph(t *testing.T) {
	g := graph.New("empty")
	ps := peer.NewPeerServer(g, buildCfg("tok"), nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/api-surface", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	ps.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("empty graph api-surface status = %d, want 200", w.Code)
	}
}

// ── PeerServer — handleIntents ────────────────────────────────────────────────

func TestHandleIntents_AcceptsValidPost(t *testing.T) {
	g := buildServerGraph(t)
	ps := peer.NewPeerServer(g, buildCfg("tok"), nil)

	intent := peer.IntentMessage{
		TraceID:    "trace-abc-001",
		AgentID:    "agent-a",
		IntentType: "claim_work",
		Scope:      "pkg/auth",
		ScopeType:  "directory",
		Timestamp:  time.Now().Unix(),
	}
	body, _ := json.Marshal(intent)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/intents", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	ps.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("intents POST status = %d, want 200", w.Code)
	}
}

func TestHandleIntents_DeduplicatesTraceID(t *testing.T) {
	g := buildServerGraph(t)
	ps := peer.NewPeerServer(g, buildCfg("tok"), nil)

	intent := peer.IntentMessage{
		TraceID:    "trace-dedup-unique-xyz",
		AgentID:    "agent-a",
		IntentType: "claim_work",
		Scope:      "pkg/auth",
	}
	body, _ := json.Marshal(intent)

	// First send — accepted and cached.
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/intents", bytes.NewReader(body))
	req1.Header.Set("Authorization", "Bearer tok")
	w1 := httptest.NewRecorder()
	ps.Handler().ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Errorf("first intent: status = %d, want 200", w1.Code)
	}

	// Second send with same trace_id — silently deduped (still 200).
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/intents", bytes.NewReader(body))
	req2.Header.Set("Authorization", "Bearer tok")
	w2 := httptest.NewRecorder()
	ps.Handler().ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Errorf("duplicate intent: status = %d, want 200", w2.Code)
	}
}

func TestHandleIntents_RejectsGET(t *testing.T) {
	g := buildServerGraph(t)
	ps := peer.NewPeerServer(g, buildCfg("tok"), nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/intents", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	ps.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /intents: status = %d, want 405", w.Code)
	}
}

func TestHandleIntents_InvalidBody(t *testing.T) {
	g := buildServerGraph(t)
	ps := peer.NewPeerServer(g, buildCfg("tok"), nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/intents", bytes.NewReader([]byte("{not valid json")))
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	ps.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON body: status = %d, want 400", w.Code)
	}
}

func TestHandleIntents_EmptyTraceID(t *testing.T) {
	g := buildServerGraph(t)
	ps := peer.NewPeerServer(g, buildCfg("tok"), nil)

	// Empty trace_id → dedup skipped, event recorded.
	intent := peer.IntentMessage{
		TraceID:    "", // no trace id — skip dedup
		AgentID:    "agent-x",
		IntentType: "claim_work",
		Scope:      "pkg/x",
	}
	body, _ := json.Marshal(intent)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/intents", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	ps.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("empty trace_id intent: status = %d, want 200", w.Code)
	}
}

// ── PeerServer — handleQuery edge cases ───────────────────────────────────────

func TestHandleQuery_MissingEntity(t *testing.T) {
	g := buildServerGraph(t)
	ps := peer.NewPeerServer(g, buildCfg("tok"), nil)

	body, _ := json.Marshal(peer.QueryRequest{Entity: ""})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/query", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	ps.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("empty entity: status = %d, want 400", w.Code)
	}
}

func TestHandleQuery_RejectsGET(t *testing.T) {
	g := buildServerGraph(t)
	ps := peer.NewPeerServer(g, buildCfg("tok"), nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/query", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	ps.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /query: status = %d, want 405", w.Code)
	}
}

func TestHandleQuery_InvalidJSON(t *testing.T) {
	g := buildServerGraph(t)
	ps := peer.NewPeerServer(g, buildCfg("tok"), nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/query", bytes.NewReader([]byte("{invalid")))
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	ps.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON body: status = %d, want 400", w.Code)
	}
}

// ── ComputeIntersection edge cases ────────────────────────────────────────────

func TestComputeIntersection_NoMatch(t *testing.T) {
	g := graph.New("local")
	fnID := g.MakeNodeID("svc.go", "LocalFunc")
	g.AddNode(&graph.Node{
		ID: fnID, Type: graph.NodeFunction, Name: "LocalFunc",
		Package: "svc", File: "svc.go", Exported: true,
		Metadata: map[string]string{"signature": "func() string"},
	})
	// Digest entry with a hash that won't match local.
	digest := []peer.DigestEntry{{Name: "RemoteFunc", SigHash: "0000000000000000"}}
	shared := peer.ComputeIntersection(g, digest)
	if len(shared) != 0 {
		t.Errorf("expected no intersection, got %v", shared)
	}
}

func TestComputeIntersection_UnexportedNodeIgnored(t *testing.T) {
	g := graph.New("local")
	fnID := g.MakeNodeID("svc.go", "localHelper")
	g.AddNode(&graph.Node{
		ID: fnID, Type: graph.NodeFunction, Name: "localHelper",
		File: "svc.go", Exported: false, // not exported
	})
	digest := []peer.DigestEntry{{Name: "localHelper", SigHash: "abcd1234abcd1234"}}
	shared := peer.ComputeIntersection(g, digest)
	if len(shared) != 0 {
		t.Errorf("unexported node should not appear in intersection, got %v", shared)
	}
}

func TestComputeIntersection_EmptyDigest(t *testing.T) {
	g := buildServerGraph(t)
	shared := peer.ComputeIntersection(g, nil)
	if len(shared) != 0 {
		t.Errorf("expected empty intersection for nil digest, got %v", shared)
	}
}

// ── PeerClient — Name ─────────────────────────────────────────────────────────

func TestPeerClient_Name(t *testing.T) {
	cli := peer.NewPeerClient(config.PeerConfig{Name: "my-peer", URL: "http://localhost"})
	if cli.Name() != "my-peer" {
		t.Errorf("expected Name='my-peer', got %q", cli.Name())
	}
}

// ── Full integration — FetchClaims + FetchAgents ──────────────────────────────

func TestIntegration_FetchAgents_NoStore(t *testing.T) {
	g := graph.New("proj")
	cfgA := &config.Config{PeerAPIToken: "tok"}
	ps := peer.NewPeerServer(g, cfgA, nil)
	srv := httptest.NewServer(ps.Handler())
	defer srv.Close()

	cli := peer.NewPeerClient(config.PeerConfig{
		Name: "proj", URL: srv.URL, Token: "tok",
	})
	agents, err := cli.FetchAgents()
	if err != nil {
		t.Fatalf("FetchAgents: %v", err)
	}
	// No store → empty list.
	if agents == nil {
		t.Error("expected non-nil agents slice")
	}
}

func TestIntegration_QueryEntity_DefaultDepth(t *testing.T) {
	g := graph.New("proj")
	fID := g.MakeNodeID("svc.go", "Bootstrap")
	g.AddNode(&graph.Node{
		ID: fID, Type: graph.NodeFunction, Name: "Bootstrap",
		Package: "svc", File: "svc.go", Exported: true,
	})
	cfgA := &config.Config{PeerAPIToken: "tok"}
	ps := peer.NewPeerServer(g, cfgA, nil)
	srv := httptest.NewServer(ps.Handler())
	defer srv.Close()

	cli := peer.NewPeerClient(config.PeerConfig{Name: "proj", URL: srv.URL, Token: "tok"})
	// Depth 0 → defaults to 2 inside handler.
	sub, err := cli.QueryEntity("Bootstrap", 0)
	if err != nil {
		t.Fatalf("QueryEntity: %v", err)
	}
	if len(sub.Nodes) == 0 {
		t.Error("expected at least 1 node in subgraph")
	}
}
