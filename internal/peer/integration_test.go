package peer_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Divish1032/synapses/internal/config"
	"github.com/Divish1032/synapses/internal/graph"
	"github.com/Divish1032/synapses/internal/peer"
)

// TestIntegration_FullFlow wires a real PeerServer through a httptest.Server
// and drives the full peer lifecycle: identity → digest → intersection → query → claims.
func TestIntegration_FullFlow(t *testing.T) {
	// Build server-side graph (Project A).
	gA := graph.New("project-a")
	fID := gA.MakeNodeID("api.go", "CreateOrder")
	gA.AddNode(&graph.Node{
		ID: fID, Type: graph.NodeFunction, Name: "CreateOrder", Package: "api",
		File: "api.go", Exported: true,
		Metadata: map[string]string{"signature": "func(ctx context.Context, req *CreateOrderRequest) (*Order, error)"},
	})

	cfgA := &config.Config{PeerAPIToken: "secret-a"}
	psA := peer.NewPeerServer(gA, cfgA, nil)

	// Wrap handler in httptest server.
	srv := httptest.NewServer(psA.Handler())
	defer srv.Close()

	// Build client-side config (Project B connecting to A).
	peerCfg := config.PeerConfig{
		Name:       "project-a",
		URL:        srv.URL,
		Token:      "secret-a",
		TrustLevel: "full",
	}
	cli := peer.NewPeerClient(peerCfg)

	// Step 1: Ping / identity (no auth needed).
	id, err := cli.Ping()
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if id.Project != "project-a" {
		t.Errorf("identity.Project = %q, want %q", id.Project, "project-a")
	}
	if id.NodeCount != 1 {
		t.Errorf("identity.NodeCount = %d, want 1", id.NodeCount)
	}

	// Step 2: Fetch digest.
	digest, err := cli.FetchDigest()
	if err != nil {
		t.Fatalf("FetchDigest: %v", err)
	}
	if len(digest) != 1 {
		t.Fatalf("digest len = %d, want 1", len(digest))
	}
	if digest[0].Name != "CreateOrder" {
		t.Errorf("digest[0].Name = %q, want CreateOrder", digest[0].Name)
	}
	if len(digest[0].SigHash) != 16 {
		t.Errorf("SigHash len = %d, want 16", len(digest[0].SigHash))
	}

	// Step 3: Compute intersection (Project B also has CreateOrder).
	gB := graph.New("project-b")
	fbID := gB.MakeNodeID("client.go", "CreateOrder")
	gB.AddNode(&graph.Node{
		ID: fbID, Type: graph.NodeFunction, Name: "CreateOrder", Package: "client",
		File: "client.go", Exported: true,
		// Same signature → same sig_hash → intersection hit.
		Metadata: map[string]string{"signature": "func(ctx context.Context, req *CreateOrderRequest) (*Order, error)"},
	})
	shared := peer.ComputeIntersection(gB, digest)
	if len(shared) != 1 || shared[0] != "CreateOrder" {
		t.Errorf("intersection = %v, want [CreateOrder]", shared)
	}

	// Step 4: Query context for CreateOrder.
	sub, err := cli.QueryEntity("CreateOrder", 1)
	if err != nil {
		t.Fatalf("QueryEntity: %v", err)
	}
	if len(sub.Nodes) == 0 {
		t.Error("expected non-empty subgraph for CreateOrder")
	}

	// Step 5: Fetch claims (no store → empty list).
	claims, err := cli.FetchClaims()
	if err != nil {
		t.Fatalf("FetchClaims: %v", err)
	}
	if claims == nil {
		t.Error("expected non-nil claims slice")
	}
}

// TestIntegration_BadToken verifies 401 on auth-required endpoints.
func TestIntegration_BadToken(t *testing.T) {
	g := graph.New("proj")
	cfgA := &config.Config{PeerAPIToken: "correct"}
	ps := peer.NewPeerServer(g, cfgA, nil)
	srv := httptest.NewServer(ps.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/api-digest", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad token: status = %d, want 401", resp.StatusCode)
	}
}

// TestIntegration_QueryNotFound verifies 404 when entity is missing.
func TestIntegration_QueryNotFound(t *testing.T) {
	g := graph.New("proj")
	cfgA := &config.Config{PeerAPIToken: "tok"}
	ps := peer.NewPeerServer(g, cfgA, nil)
	srv := httptest.NewServer(ps.Handler())
	defer srv.Close()

	body, _ := json.Marshal(peer.QueryRequest{Entity: "Ghost"})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/query", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing entity: status = %d, want 404", resp.StatusCode)
	}
}
