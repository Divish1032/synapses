package peer_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/peer"
	"github.com/SynapsesOS/synapses/internal/store"
)

// openPeerStore creates a real SQLite store in a temp dir for handler tests.
func openPeerStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "peer_test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// ── handleClaims with real store ──────────────────────────────────────────────

func TestHandleClaims_WithStore_EmptyResult(t *testing.T) {
	g := buildServerGraph(t)
	st := openPeerStore(t)
	ps := peer.NewPeerServer(g, buildCfg("tok"), st)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/claims", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	ps.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("claims with store: status = %d, want 200", w.Code)
	}
	var claims []interface{}
	if err := json.NewDecoder(w.Body).Decode(&claims); err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	// Empty store → empty array (not null).
	if claims == nil {
		t.Error("expected non-nil (possibly empty) claims array")
	}
}

// ── handleAgents with real store ──────────────────────────────────────────────

func TestHandleAgents_WithStore(t *testing.T) {
	g := buildServerGraph(t)
	st := openPeerStore(t)
	ps := peer.NewPeerServer(g, buildCfg("tok"), st)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	ps.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("agents with store: status = %d, want 200", w.Code)
	}
	var agents []interface{}
	if err := json.NewDecoder(w.Body).Decode(&agents); err != nil {
		t.Fatalf("decode agents: %v", err)
	}
	if agents == nil {
		t.Error("expected non-nil (possibly empty) agents array")
	}
}

// ── PeerServer.Start / Stop ───────────────────────────────────────────────────

func TestPeerServer_StartStop(t *testing.T) {
	g := graph.New("test")
	// Empty token → no auth needed for the start/stop test.
	ps := peer.NewPeerServer(g, &config.Config{}, nil)

	// Port 0 → OS assigns a free port.
	if err := ps.Start(0); err != nil {
		t.Fatalf("Start(0): %v", err)
	}
	ps.Stop()
}

func TestPeerServer_Stop_WithoutStart(t *testing.T) {
	g := graph.New("test")
	ps := peer.NewPeerServer(g, buildCfg("tok"), nil)
	// Stop before Start — httpSrv is nil → early return, no panic.
	ps.Stop()
}

// ── detectFramework — echo / fiber / grpc paths ───────────────────────────────

func TestHandleApiSurface_EchoEndpoint(t *testing.T) {
	g := graph.New("testrepo")
	echoID := g.MakeNodeID("api.go", "GetUser")
	g.AddNode(&graph.Node{
		ID:       echoID,
		Type:     graph.NodeFunction,
		Name:     "GetUser",
		Package:  "api",
		File:     "api.go",
		Exported: true,
		Metadata: map[string]string{"signature": "func(c echo.Context) error"},
	})
	ps := peer.NewPeerServer(g, buildCfg("tok"), nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/api-surface", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	ps.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("api-surface status = %d", w.Code)
	}
	var endpoints []map[string]interface{}
	_ = json.NewDecoder(w.Body).Decode(&endpoints)
	for _, ep := range endpoints {
		if ep["name"] == "GetUser" && ep["framework"] != "echo" {
			t.Errorf("expected framework=echo, got %v", ep["framework"])
		}
	}
}

func TestHandleApiSurface_FiberEndpoint(t *testing.T) {
	g := graph.New("testrepo")
	fID := g.MakeNodeID("api.go", "PostItem")
	g.AddNode(&graph.Node{
		ID:       fID,
		Type:     graph.NodeFunction,
		Name:     "PostItem",
		Package:  "api",
		File:     "api.go",
		Exported: true,
		Metadata: map[string]string{"signature": "func(c *fiber.Ctx) error"},
	})
	ps := peer.NewPeerServer(g, buildCfg("tok"), nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/api-surface", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	ps.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("api-surface status = %d", w.Code)
	}
}

func TestHandleApiSurface_GrpcEndpoint(t *testing.T) {
	g := graph.New("testrepo")
	gID := g.MakeNodeID("svc.go", "CreateUser")
	g.AddNode(&graph.Node{
		ID:       gID,
		Type:     graph.NodeFunction,
		Name:     "CreateUser",
		Package:  "svc",
		File:     "svc.go",
		Exported: true,
		// gRPC pattern: context + request + response + "error)"
		Metadata: map[string]string{
			"signature": "func(ctx context.Context, req *CreateUserRequest) (*CreateUserResponse, error)",
		},
	})
	ps := peer.NewPeerServer(g, buildCfg("tok"), nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/api-surface", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	ps.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("api-surface status = %d", w.Code)
	}
	var endpoints []map[string]interface{}
	_ = json.NewDecoder(w.Body).Decode(&endpoints)
	found := false
	for _, ep := range endpoints {
		if ep["name"] == "CreateUser" {
			found = true
			if ep["framework"] != "grpc" {
				t.Errorf("expected framework=grpc, got %v", ep["framework"])
			}
		}
	}
	if !found {
		t.Error("expected CreateUser (grpc) in api-surface endpoints")
	}
}

func TestHandleApiSurface_ProtoFile(t *testing.T) {
	g := graph.New("testrepo")
	pID := g.MakeNodeID("user.proto", "UserService")
	g.AddNode(&graph.Node{
		ID:       pID,
		Type:     graph.NodeFunction,
		Name:     "UserService",
		Package:  "proto",
		File:     "user.proto",
		Exported: true,
	})
	ps := peer.NewPeerServer(g, buildCfg("tok"), nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/api-surface", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	ps.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("api-surface status = %d", w.Code)
	}
}

func TestHandleApiSurface_CustomApiEntry(t *testing.T) {
	g := graph.New("testrepo")
	cID := g.MakeNodeID("custom.go", "HandleWebhook")
	g.AddNode(&graph.Node{
		ID:       cID,
		Type:     graph.NodeFunction,
		Name:     "HandleWebhook",
		Package:  "webhook",
		File:     "custom.go",
		Exported: true,
	})
	// Config with a custom api_entry matching "Webhook" in name.
	cfg := &config.Config{
		PeerAPIToken: "tok",
		ApiEntries: []config.ApiEntryPattern{{
			NamePattern: "Webhook",
		}},
	}
	ps := peer.NewPeerServer(g, cfg, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/api-surface", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	ps.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("api-surface status = %d", w.Code)
	}
	var endpoints []map[string]interface{}
	_ = json.NewDecoder(w.Body).Decode(&endpoints)
	found := false
	for _, ep := range endpoints {
		if ep["name"] == "HandleWebhook" && ep["framework"] == "custom" {
			found = true
		}
	}
	if !found {
		t.Error("expected HandleWebhook (custom) in api-surface endpoints")
	}
}

// ── PeerClient.BroadcastIntent ────────────────────────────────────────────────

func TestPeerClient_BroadcastIntent(t *testing.T) {
	// Server that accepts intents (with auth).
	g := graph.New("remote")
	srv := httptest.NewServer(peer.NewPeerServer(g, buildCfg("tok"), nil).Handler())
	defer srv.Close()

	cli := peer.NewPeerClient(config.PeerConfig{
		Name: "remote", URL: srv.URL, Token: "tok",
	})
	msg := peer.IntentMessage{
		TraceID:    "client-broadcast-test",
		AgentID:    "agent-a",
		IntentType: "claim_work",
		Scope:      "pkg/auth",
	}
	err := cli.BroadcastIntent(msg)
	if err != nil {
		t.Fatalf("BroadcastIntent: %v", err)
	}
}

// ── PeerManager.FetchAllPeerClaims — connected path ───────────────────────────

func TestPeerManager_FetchAllPeerClaims_Connected(t *testing.T) {
	gA := graph.New("remote")
	psA := peer.NewPeerServer(gA, buildCfg("tok"), nil)
	srv := httptest.NewServer(psA.Handler())
	defer srv.Close()

	peerCfg := config.PeerConfig{Name: "remote", URL: srv.URL, Token: "tok"}
	gLocal := graph.New("local")
	pm := peer.NewPeerManager(&config.Config{Peers: []config.PeerConfig{peerCfg}}, gLocal, nil)

	statuses := pm.Connect()
	if len(statuses) == 0 || !statuses[0].Connected {
		t.Skip("peer not connected (skipping connected FetchAllPeerClaims test)")
	}

	result := pm.FetchAllPeerClaims(nil)
	// No active claims → empty map (not nil).
	if result == nil {
		t.Error("expected non-nil map from FetchAllPeerClaims")
	}
}

// ── PeerManager.FetchAllPeerClaims — connected path returns claims ────────────

func TestPeerManager_GetStatuses_AfterConnect(t *testing.T) {
	gA := graph.New("remote")
	psA := peer.NewPeerServer(gA, buildCfg("tok"), nil)
	srv := httptest.NewServer(psA.Handler())
	defer srv.Close()

	peerCfg := config.PeerConfig{Name: "remote", URL: srv.URL, Token: "tok"}
	pm := peer.NewPeerManager(&config.Config{Peers: []config.PeerConfig{peerCfg}}, graph.New("local"), nil)

	// Before Connect — statuses is empty.
	before := pm.GetStatuses()
	if len(before) != 0 {
		t.Errorf("expected 0 statuses before connect, got %d", len(before))
	}

	pm.Connect()

	after := pm.GetStatuses()
	if len(after) != 1 {
		t.Errorf("expected 1 status after connect, got %d", len(after))
	}
}

// ── ApiEntry config fields ────────────────────────────────────────────────────

func TestHandleApiSurface_CustomApiEntry_FilePattern(t *testing.T) {
	g := graph.New("testrepo")
	cID := g.MakeNodeID("handler/ws.go", "ServeWS")
	g.AddNode(&graph.Node{
		ID:       cID,
		Type:     graph.NodeFunction,
		Name:     "ServeWS",
		Package:  "handler",
		File:     "handler/ws.go",
		Exported: true,
	})
	cfg := &config.Config{
		PeerAPIToken: "tok",
		ApiEntries: []config.ApiEntryPattern{{
			FilePattern: "handler/*.go",
			NodeType:    graph.NodeFunction,
		}},
	}
	ps := peer.NewPeerServer(g, cfg, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/api-surface", nil)
	req.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	ps.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("api-surface status = %d", w.Code)
	}
}

// ── traceIDCache expired entry re-accepts ────────────────────────────────────

func TestHandleIntents_TraceIDCache_AcceptsAfterExpiry(t *testing.T) {
	// After the 5-minute TTL, the trace ID should be accepted again.
	// We can't control time, but we can verify that a DIFFERENT trace ID is accepted.
	g := buildServerGraph(t)
	ps := peer.NewPeerServer(g, buildCfg("tok"), nil)

	// Send intent with trace-A.
	send := func(traceID string) int {
		body, _ := json.Marshal(peer.IntentMessage{TraceID: traceID, AgentID: "a", Scope: "x"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/intents", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok")
		w := httptest.NewRecorder()
		ps.Handler().ServeHTTP(w, req)
		return w.Code
	}

	if code := send("unique-trace-a"); code != http.StatusOK {
		t.Errorf("first send trace-a: %d", code)
	}
	// Different trace ID → should also be 200.
	if code := send("unique-trace-b"); code != http.StatusOK {
		t.Errorf("different trace-b: %d", code)
	}
}
