// Package peer implements the inter-project HTTP API that allows multiple
// synapses instances to query each other's graph context, active work claims,
// and exported API surface. It uses stdlib net/http only (zero external deps).
package peer

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Divish1032/synapses/internal/config"
	"github.com/Divish1032/synapses/internal/graph"
	"github.com/Divish1032/synapses/internal/store"
)

const peerVersion = "0.2.0"

// IdentityResponse is the public capability card returned by GET /api/v1/identity.
// No auth is required for this endpoint — it matches the A2A agent card pattern.
type IdentityResponse struct {
	Project      string   `json:"project"`
	Version      string   `json:"version"`
	NodeCount    int      `json:"node_count"`
	Capabilities []string `json:"capabilities"`
}

// DigestEntry is a single exported entity in the API digest handshake.
// SigHash is the first 16 hex chars of SHA-256(signature), used for fast
// intersection checks without transmitting full signatures.
type DigestEntry struct {
	Name    string `json:"name"`
	SigHash string `json:"sig_hash"`
}

// QueryRequest is the body for POST /api/v1/query.
type QueryRequest struct {
	Entity string `json:"entity"`
	Depth  int    `json:"depth"` // 0 → default 2
}

// traceIDCache deduplicates incoming IntentMessages by trace_id to prevent
// notification loops in a mesh. Entries expire after ttl (default 5 min).
type traceIDCache struct {
	mu      sync.Mutex
	entries map[string]time.Time
	ttl     time.Duration
}

func newTraceIDCache() *traceIDCache {
	return &traceIDCache{entries: make(map[string]time.Time), ttl: 5 * time.Minute}
}

func (c *traceIDCache) isSeen(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, ok := c.entries[id]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(c.entries, id)
		return false
	}
	return true
}

func (c *traceIDCache) markSeen(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[id] = time.Now().Add(c.ttl)
}

// PeerServer serves the peer-to-peer HTTP API for a single synapses instance.
type PeerServer struct {
	g           *graph.Graph
	cfg         *config.Config
	st          *store.Store
	validTokens map[string]struct{}
	httpSrv     *http.Server
	traceCache  *traceIDCache
}

// NewPeerServer creates a PeerServer. st may be nil — claims endpoint will
// return an empty list in that case.
func NewPeerServer(g *graph.Graph, cfg *config.Config, st *store.Store) *PeerServer {
	ps := &PeerServer{
		g:           g,
		cfg:         cfg,
		st:          st,
		validTokens: make(map[string]struct{}),
		traceCache:  newTraceIDCache(),
	}
	if cfg != nil && cfg.PeerAPIToken != "" {
		ps.validTokens[cfg.PeerAPIToken] = struct{}{}
	}
	return ps
}

// Handler returns the HTTP mux. Exported so tests can wrap it with httptest.
func (ps *PeerServer) Handler() http.Handler {
	return ps.handler()
}

// handler returns the HTTP mux used by the server.
func (ps *PeerServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/identity", ps.handleIdentity)
	mux.HandleFunc("/api/v1/api-digest", ps.auth(ps.handleApiDigest))
	mux.HandleFunc("/api/v1/api-surface", ps.auth(ps.handleApiSurface))
	mux.HandleFunc("/api/v1/query", ps.auth(ps.handleQuery))
	mux.HandleFunc("/api/v1/claims", ps.auth(ps.handleClaims))
	mux.HandleFunc("/api/v1/intents", ps.auth(ps.handleIntents))
	return mux
}

// Start begins serving on port. It returns immediately; a goroutine drives the
// server. For non-localhost URLs the server uses a self-signed TLS certificate
// cached in ~/.synapses/peer.crt + peer.key.
func (ps *PeerServer) Start(port int) error {
	addr := fmt.Sprintf(":%d", port)
	ps.httpSrv = &http.Server{
		Addr:        addr,
		Handler:     ps.handler(),
		ReadTimeout: 30 * time.Second,
	}

	// Determine whether TLS is warranted (non-localhost peer URLs configured).
	useTLS := ps.needsTLS()

	if useTLS {
		certFile, keyFile, err := ensureSelfSignedCert()
		if err != nil {
			// TLS cert generation failed — fall back to plain HTTP and log it.
			fmt.Fprintf(os.Stderr, "synapses/peer: TLS cert: %v — serving plain HTTP\n", err)
			useTLS = false
		} else {
			tlsCfg, err := loadTLSConfig(certFile, keyFile)
			if err != nil {
				fmt.Fprintf(os.Stderr, "synapses/peer: load TLS: %v — serving plain HTTP\n", err)
				useTLS = false
			} else {
				ps.httpSrv.TLSConfig = tlsCfg
			}
		}
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("peer listen %s: %w", addr, err)
	}

	go func() {
		var serveErr error
		if useTLS {
			serveErr = ps.httpSrv.ServeTLS(ln, "", "")
		} else {
			serveErr = ps.httpSrv.Serve(ln)
		}
		if serveErr != nil && serveErr != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "synapses/peer: serve: %v\n", serveErr)
		}
	}()
	return nil
}

// Stop gracefully shuts down the HTTP server with a 5-second timeout.
func (ps *PeerServer) Stop() {
	if ps.httpSrv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = ps.httpSrv.Shutdown(ctx)
}

// needsTLS returns true if any configured peer URL is non-localhost.
func (ps *PeerServer) needsTLS() bool {
	if ps.cfg == nil {
		return false
	}
	for _, p := range ps.cfg.Peers {
		u := p.URL
		if !strings.Contains(u, "localhost") && !strings.Contains(u, "127.0.0.1") {
			return true
		}
	}
	return false
}

// auth is a middleware that requires a valid Bearer token.
func (ps *PeerServer) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// If no tokens are configured, reject all requests to auth-required endpoints.
		if len(ps.validTokens) == 0 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		tok := strings.TrimPrefix(auth, prefix)
		if _, ok := ps.validTokens[tok]; !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

// handleIdentity returns the public identity card (no auth).
func (ps *PeerServer) handleIdentity(w http.ResponseWriter, _ *http.Request) {
	resp := IdentityResponse{
		Project:   ps.g.RepoID(),
		Version:   peerVersion,
		NodeCount: ps.g.NodeCount(),
		Capabilities: []string{
			"query", "claims", "api-surface", "api-digest", "intents",
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleApiDigest returns a compact list of exported entity names + sig hashes.
// Peers use this for fast intersection checks (O(n+m)) before querying context.
func (ps *PeerServer) handleApiDigest(w http.ResponseWriter, _ *http.Request) {
	entries := collectDigest(ps.g)
	writeJSON(w, http.StatusOK, entries)
}

// handleApiSurface returns the full detected API surface (HTTP/gRPC endpoints).
func (ps *PeerServer) handleApiSurface(w http.ResponseWriter, _ *http.Request) {
	endpoints := collectAPIEndpoints(ps.g, ps.cfg)
	writeJSON(w, http.StatusOK, endpoints)
}

// handleQuery handles POST /api/v1/query — carves a subgraph for the named entity.
func (ps *PeerServer) handleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	var req QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if req.Entity == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "entity is required"})
		return
	}
	depth := req.Depth
	if depth <= 0 {
		depth = 2
	}

	node := pickBestNode(ps.g, req.Entity)
	if node == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "entity not found"})
		return
	}

	ccfg := graph.DefaultCarveConfig()
	ccfg.MaxDepth = depth
	sub, err := ps.g.CarveEgoGraph(node.ID, ccfg)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, sub)
}

// handleClaims returns all active (non-expired) work claims across all agents.
func (ps *PeerServer) handleClaims(w http.ResponseWriter, _ *http.Request) {
	if ps.st == nil {
		writeJSON(w, http.StatusOK, []store.WorkClaim{})
		return
	}
	claims, err := ps.st.GetAllClaims()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if claims == nil {
		claims = []store.WorkClaim{}
	}
	writeJSON(w, http.StatusOK, claims)
}

// handleIntents receives an IntentMessage from a peer and records it as an
// event. Duplicate trace_ids (within the 5-minute TTL) are silently dropped
// to prevent notification storms in a mesh topology.
func (ps *PeerServer) handleIntents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	var msg IntentMessage
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	// Circular dampening: drop already-seen trace IDs.
	if msg.TraceID != "" && ps.traceCache.isSeen(msg.TraceID) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	if msg.TraceID != "" {
		ps.traceCache.markSeen(msg.TraceID)
	}
	// Best-effort event recording; non-fatal if store is absent.
	if ps.st != nil {
		payload, _ := json.Marshal(msg)
		_ = ps.st.AppendEvent("intent_received", msg.AgentID, string(payload))
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// collectDigest returns DigestEntry list for all exported nodes in the graph.
func collectDigest(g *graph.Graph) []DigestEntry {
	var entries []DigestEntry
	for _, n := range g.AllNodes() {
		if !n.Exported {
			continue
		}
		switch n.Type {
		case graph.NodeFunction, graph.NodeMethod, graph.NodeStruct, graph.NodeInterface:
		default:
			continue
		}
		sig := ""
		if n.Metadata != nil {
			sig = n.Metadata["signature"]
		}
		h := sha256.Sum256([]byte(sig))
		entries = append(entries, DigestEntry{
			Name:    n.Name,
			SigHash: fmt.Sprintf("%x", h[:8]),
		})
	}
	return entries
}

// collectAPIEndpoints returns a list of detected API entry points (HTTP/gRPC).
// Reuses the same detection heuristics as the get_api_contract MCP tool.
func collectAPIEndpoints(g *graph.Graph, cfg *config.Config) []map[string]interface{} {
	var endpoints []map[string]interface{}
	for _, n := range g.AllNodes() {
		if n.Type != graph.NodeFunction && n.Type != graph.NodeMethod {
			continue
		}
		sig := ""
		if n.Metadata != nil {
			sig = n.Metadata["signature"]
		}
		framework := detectFramework(sig, n.File, string(n.Type), n.Name, cfg)
		if framework == "" {
			continue
		}
		endpoints = append(endpoints, map[string]interface{}{
			"name":      n.Name,
			"file":      n.File,
			"line":      n.Line,
			"framework": framework,
			"signature": sig,
		})
	}
	return endpoints
}

// detectFramework returns a non-empty framework name if the node looks like
// an HTTP/gRPC handler. Mirrors the heuristics in internal/mcp/tools.go.
func detectFramework(sig, file, nodeType, name string, cfg *config.Config) string {
	sigL := strings.ToLower(sig)
	fileBase := filepath.Base(file)

	// net/http pattern: (http.ResponseWriter, *http.Request)
	if strings.Contains(sig, "http.ResponseWriter") && strings.Contains(sig, "*http.Request") {
		return "net/http"
	}
	// Gin: *gin.Context
	if strings.Contains(sig, "*gin.Context") {
		return "gin"
	}
	// Echo: echo.Context
	if strings.Contains(sig, "echo.Context") {
		return "echo"
	}
	// Fiber: *fiber.Ctx
	if strings.Contains(sig, "*fiber.Ctx") {
		return "fiber"
	}
	// gRPC: method with Context + XxxRequest + XxxResponse + error
	if strings.Contains(sigL, "context") &&
		strings.Contains(sigL, "request") &&
		strings.Contains(sigL, "response") &&
		strings.HasSuffix(strings.TrimSpace(sig), "error)") {
		return "grpc"
	}
	// Proto RPC: .proto files
	if strings.HasSuffix(fileBase, ".proto") {
		return "protobuf"
	}

	// Custom api_entries from config.
	if cfg != nil {
		for _, ae := range cfg.ApiEntries {
			if ae.NodeType != "" && graph.NodeType(nodeType) != ae.NodeType {
				continue
			}
			if ae.NamePattern != "" && !strings.Contains(strings.ToLower(name), strings.ToLower(ae.NamePattern)) {
				continue
			}
			if ae.FilePattern != "" {
				matched, _ := filepath.Match(ae.FilePattern, fileBase)
				if !matched && !strings.Contains(file, ae.FilePattern) {
					continue
				}
			}
			return "custom"
		}
	}
	return ""
}

// pickBestNode finds the best node matching name in the graph, preferring
// non-test exported functions and methods over other types.
func pickBestNode(g *graph.Graph, name string) *graph.Node {
	var best *graph.Node
	bestScore := -1
	for _, n := range g.AllNodes() {
		if n.Name != name {
			continue
		}
		score := 0
		isTest := strings.HasSuffix(n.File, "_test.go")
		switch n.Type {
		case graph.NodeFunction, graph.NodeMethod:
			if !isTest {
				score = 3
			} else {
				score = 1
			}
		case graph.NodeStruct, graph.NodeInterface:
			if !isTest {
				score = 2
			}
		}
		if score > bestScore {
			best = n
			bestScore = score
		}
	}
	return best
}

// writeJSON encodes v as JSON and writes it to w.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// --- TLS helpers ---

func synapsesCacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".synapses")
	return dir, os.MkdirAll(dir, 0o700)
}

func ensureSelfSignedCert() (certFile, keyFile string, err error) {
	dir, err := synapsesCacheDir()
	if err != nil {
		return "", "", err
	}
	certFile = filepath.Join(dir, "peer.crt")
	keyFile = filepath.Join(dir, "peer.key")

	// Reuse existing cert if it has >30 days remaining.
	if certPEM, e := os.ReadFile(certFile); e == nil {
		block, _ := pem.Decode(certPEM)
		if block != nil {
			if cert, e2 := x509.ParseCertificate(block.Bytes); e2 == nil {
				if time.Until(cert.NotAfter) > 30*24*time.Hour {
					return certFile, keyFile, nil
				}
			}
		}
	}

	// Generate a new self-signed RSA-2048 cert valid for 2 years.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", fmt.Errorf("generate RSA key: %w", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "synapses-peer"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(2 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", "", fmt.Errorf("create cert: %w", err)
	}

	certOut, err := os.Create(certFile)
	if err != nil {
		return "", "", err
	}
	_ = pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	certOut.Close()

	keyOut, err := os.Create(keyFile)
	if err != nil {
		return "", "", err
	}
	_ = pem.Encode(keyOut, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	keyOut.Close()

	return certFile, keyFile, nil
}

func loadTLSConfig(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}
