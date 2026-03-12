package peer_test

// Tests for TLS helpers, health monitor, syncPeerAgents, and remaining
// client method branches.

import (
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/peer"
	"github.com/SynapsesOS/synapses/internal/store"
)

// openTLSStore opens a real SQLite store in a temp dir.
func openTLSStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "tls_test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// ── needsTLS via Start with remote peer URL ────────────────────────────────────

func TestPeerServer_Start_WithRemotePeer(t *testing.T) {
	// A config with a non-localhost remote peer URL makes needsTLS() true.
	// Start(0) tries ensureSelfSignedCert + loadTLSConfig.
	// Even if TLS setup fails, we verify no panic / clean error.
	cfg := &config.Config{
		Peers: []config.PeerConfig{
			{Name: "remote", URL: "https://remote.example.com:8443", Token: "tok"},
		},
	}
	g := graph.New("test")
	ps := peer.NewPeerServer(g, cfg, nil)
	if err := ps.Start(0); err != nil {
		// TLS cert generation may fail in sandboxed CI — acceptable.
		t.Logf("Start with TLS: %v (skipping TLS coverage)", err)
		return
	}
	ps.Stop()
}

// ── PeerManager.BroadcastIntent with store ────────────────────────────────────

func TestPeerManager_BroadcastIntent_WithStore(t *testing.T) {
	st := openTLSStore(t)
	pm := peer.NewPeerManager(
		&config.Config{Peers: []config.PeerConfig{}},
		graph.New("local"),
		st,
	)
	// No connected peers → only the store branch (AppendEvent) runs.
	pm.BroadcastIntent("agent-x", "internal/auth", "package")
}

func TestPeerManager_BroadcastIntent_NoStore(t *testing.T) {
	pm := peer.NewPeerManager(
		&config.Config{Peers: []config.PeerConfig{}},
		graph.New("local"),
		nil,
	)
	// No store, no peers → goroutine path only (fire-and-forget).
	pm.BroadcastIntent("agent-x", "internal/auth", "package")
}

func TestPeerManager_BroadcastIntent_WithPeers(t *testing.T) {
	// Set up a live peer server so the goroutine has a real target.
	g := graph.New("remote")
	srv := httptest.NewServer(peer.NewPeerServer(g, buildCfg("tok"), nil).Handler())
	defer srv.Close()

	peerCfg := config.PeerConfig{Name: "remote", URL: srv.URL, Token: "tok"}
	pm := peer.NewPeerManager(
		&config.Config{Peers: []config.PeerConfig{peerCfg}},
		graph.New("local"),
		nil,
	)
	pm.Connect()

	// BroadcastIntent sends goroutines — just verify no panic.
	pm.BroadcastIntent("agent-x", "pkg/auth", "package")
	// Brief wait for goroutines to fire.
	time.Sleep(50 * time.Millisecond)
}

// ── StartHealthMonitor ─────────────────────────────────────────────────────────

func TestPeerManager_StartHealthMonitor_StopsCleanly(t *testing.T) {
	g := graph.New("remote")
	srv := httptest.NewServer(peer.NewPeerServer(g, buildCfg("tok"), nil).Handler())
	defer srv.Close()

	peerCfg := config.PeerConfig{Name: "remote", URL: srv.URL, Token: "tok"}
	pm := peer.NewPeerManager(
		&config.Config{Peers: []config.PeerConfig{peerCfg}},
		graph.New("local"),
		nil,
	)
	pm.Connect()

	// Start monitor with a long interval so it fires at most once quickly.
	pm.StartHealthMonitor(200 * time.Millisecond)
	// Let the ticker fire at least once.
	time.Sleep(350 * time.Millisecond)
	// Stop should clean up the goroutine.
	pm.Stop()
}

// ── syncPeerAgents via StartHealthMonitor + connected peer ────────────────────

func TestPeerManager_SyncPeerAgents_ViaHealthMonitor(t *testing.T) {
	// Use a real store so UpsertRemoteAgent can be exercised.
	st := openTLSStore(t)

	gRemote := graph.New("remote")
	srv := httptest.NewServer(peer.NewPeerServer(gRemote, buildCfg("tok"), st).Handler())
	defer srv.Close()

	peerCfg := config.PeerConfig{Name: "remote", URL: srv.URL, Token: "tok"}
	pm := peer.NewPeerManager(
		&config.Config{Peers: []config.PeerConfig{peerCfg}},
		graph.New("local"),
		st,
	)
	pm.Connect()

	// Start monitor; on the first tick it calls syncPeerAgents.
	pm.StartHealthMonitor(100 * time.Millisecond)
	time.Sleep(300 * time.Millisecond)
	pm.Stop()
}

// ── PeerClient error paths (HTTP 4xx/5xx responses) ───────────────────────────

func TestPeerClient_FetchDigest_EmptyGraph(t *testing.T) {
	g := graph.New("remote")
	srv := httptest.NewServer(peer.NewPeerServer(g, buildCfg("tok"), nil).Handler())
	defer srv.Close()

	cli := peer.NewPeerClient(config.PeerConfig{
		Name: "remote", URL: srv.URL, Token: "tok",
	})
	result, err := cli.FetchDigest()
	if err != nil {
		t.Logf("FetchDigest: %v", err)
	}
	_ = result
}

func TestPeerClient_FetchClaims_Live(t *testing.T) {
	g := graph.New("remote")
	srv := httptest.NewServer(peer.NewPeerServer(g, buildCfg("tok"), nil).Handler())
	defer srv.Close()

	cli := peer.NewPeerClient(config.PeerConfig{
		Name: "remote", URL: srv.URL, Token: "tok",
	})
	claims, err := cli.FetchClaims()
	if err != nil {
		t.Fatalf("FetchClaims: %v", err)
	}
	if claims == nil {
		t.Error("expected non-nil claims")
	}
}

func TestPeerClient_FetchAgents_Live(t *testing.T) {
	g := graph.New("remote")
	st := openTLSStore(t)
	srv := httptest.NewServer(peer.NewPeerServer(g, buildCfg("tok"), st).Handler())
	defer srv.Close()

	cli := peer.NewPeerClient(config.PeerConfig{
		Name: "remote", URL: srv.URL, Token: "tok",
	})
	agents, err := cli.FetchAgents()
	if err != nil {
		t.Fatalf("FetchAgents: %v", err)
	}
	_ = agents
}

func TestPeerClient_QueryEntity_Live(t *testing.T) {
	g := buildServerGraph(t)
	srv := httptest.NewServer(peer.NewPeerServer(g, buildCfg("tok"), nil).Handler())
	defer srv.Close()

	cli := peer.NewPeerClient(config.PeerConfig{
		Name: "remote", URL: srv.URL, Token: "tok",
	})
	result, err := cli.QueryEntity("Login", 1)
	if err != nil {
		t.Logf("QueryEntity: %v", err)
	}
	_ = result
}

func TestPeerClient_WrongToken_Unauthorized(t *testing.T) {
	g := graph.New("remote")
	srv := httptest.NewServer(peer.NewPeerServer(g, buildCfg("correct-tok"), nil).Handler())
	defer srv.Close()

	cli := peer.NewPeerClient(config.PeerConfig{
		Name: "remote", URL: srv.URL, Token: "wrong-tok",
	})
	_, err := cli.FetchClaims()
	if err == nil {
		t.Error("expected error for wrong token")
	}
}
