package peer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

const connectTimeout = 5 * time.Second

// CrossProjectConflict is a work-claim conflict detected on a remote peer.
type CrossProjectConflict struct {
	PeerName string          `json:"peer_name"`
	Claim    store.WorkClaim `json:"claim"`
}

// ScopesOverlap reports whether work scopes a and b conflict: exact match,
// a is a child of b, or b is a child of a. Mirrors the SQL logic in store.ClaimWork.
func ScopesOverlap(a, b string) bool {
	return a == b ||
		strings.HasPrefix(a, b+"/") ||
		strings.HasPrefix(b, a+"/")
}

// PeerManager manages connections to all configured peer synapses instances.
type PeerManager struct {
	clients  []*PeerClient
	g        *graph.Graph
	cfg      *config.Config
	st       *store.Store
	mu       sync.RWMutex
	statuses []PeerStatus
	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewPeerManager creates a PeerManager from the given config. Call Connect to
// establish connections to all configured peers.
func NewPeerManager(cfg *config.Config, g *graph.Graph, st *store.Store) *PeerManager {
	pm := &PeerManager{g: g, cfg: cfg, st: st, stopCh: make(chan struct{})}
	for _, p := range cfg.Peers {
		pm.clients = append(pm.clients, NewPeerClient(p))
	}
	return pm
}

// Connect pings all configured peers concurrently and fetches their identity +
// digest. Intersection counts are computed. Returns once all peers respond or
// the 5-second timeout elapses.
func (pm *PeerManager) Connect() []PeerStatus {
	type result struct {
		status PeerStatus
		idx    int
	}

	ch := make(chan result, len(pm.clients))
	for i, cli := range pm.clients {
		go func(idx int, c *PeerClient) {
			st := PeerStatus{
				Name:       c.cfg.Name,
				URL:        c.cfg.URL,
				TrustLevel: c.cfg.TrustLevel,
			}
			id, err := c.Ping()
			if err != nil {
				st.Error = err.Error()
				ch <- result{st, idx}
				return
			}
			st.Connected = true
			st.Healthy = true
			st.LastSeenAt = time.Now()
			st.NodeCount = id.NodeCount

			// Best-effort digest + intersection.
			if digest, err := c.FetchDigest(); err == nil {
				shared := ComputeIntersection(pm.g, digest)
				st.SharedCount = len(shared)
			}
			ch <- result{st, idx}
		}(i, cli)
	}

	statuses := make([]PeerStatus, len(pm.clients))
	deadline := time.After(connectTimeout)
	received := 0
	for received < len(pm.clients) {
		select {
		case r := <-ch:
			statuses[r.idx] = r.status
			received++
		case <-deadline:
			// Fill remaining statuses as timed out.
			for i, s := range statuses {
				if !s.Connected && s.Error == "" && s.Name == "" {
					statuses[i] = PeerStatus{
						Name:       pm.clients[i].cfg.Name,
						URL:        pm.clients[i].cfg.URL,
						TrustLevel: pm.clients[i].cfg.TrustLevel,
						Error:      "connection timeout",
					}
				}
			}
			received = len(pm.clients)
		}
	}

	pm.mu.Lock()
	pm.statuses = statuses
	pm.mu.Unlock()
	return statuses
}

// GetClient returns the client for the named peer, or an error if not found.
func (pm *PeerManager) GetClient(name string) (*PeerClient, error) {
	for _, c := range pm.clients {
		if c.cfg.Name == name {
			return c, nil
		}
	}
	return nil, fmt.Errorf("peer %q not found in configuration", name)
}

// GetStatuses returns the last-known connection statuses for all peers.
func (pm *PeerManager) GetStatuses() []PeerStatus {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	out := make([]PeerStatus, len(pm.statuses))
	copy(out, pm.statuses)
	return out
}

// FetchAllPeerClaims contacts each connected peer concurrently and returns a
// map[peerName → []WorkClaim]. Unconnected peers are skipped. Each goroutine
// runs independently; the overall call completes once all goroutines finish.
func (pm *PeerManager) FetchAllPeerClaims(_ context.Context) map[string][]store.WorkClaim {
	pm.mu.RLock()
	statuses := make([]PeerStatus, len(pm.statuses))
	copy(statuses, pm.statuses)
	pm.mu.RUnlock()

	type result struct {
		name   string
		claims []store.WorkClaim
	}
	ch := make(chan result, len(pm.clients))

	for i, cli := range pm.clients {
		connected := i < len(statuses) && statuses[i].Connected
		if !connected {
			ch <- result{cli.cfg.Name, nil}
			continue
		}
		go func(c *PeerClient) {
			claims, err := c.FetchClaims()
			if err != nil {
				ch <- result{c.cfg.Name, nil}
				return
			}
			ch <- result{c.cfg.Name, claims}
		}(cli)
	}

	out := make(map[string][]store.WorkClaim)
	for range pm.clients {
		r := <-ch
		if len(r.claims) > 0 {
			out[r.name] = r.claims
		}
	}
	return out
}

// BroadcastIntent sends an IntentMessage to all connected peers concurrently.
// Each send is fire-and-forget; individual failures are silently ignored.
func (pm *PeerManager) BroadcastIntent(agentID, scope, scopeType string) {
	msg := IntentMessage{
		TraceID:    generateStableID(),
		AgentID:    agentID,
		IntentType: "claim_work",
		Scope:      scope,
		ScopeType:  scopeType,
		Timestamp:  time.Now().Unix(),
	}
	for _, cli := range pm.clients {
		go func(c *PeerClient) { _ = c.BroadcastIntent(msg) }(cli)
	}
	if pm.st != nil {
		payload, _ := json.Marshal(msg)
		_ = pm.st.AppendEvent("intent_broadcast", agentID, string(payload))
	}
}

// StartHealthMonitor starts a background goroutine that re-pings all peers
// every interval. Call Stop() to shut it down cleanly.
func (pm *PeerManager) StartHealthMonitor(interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				pm.Connect()
			case <-pm.stopCh:
				return
			}
		}
	}()
}

// Stop shuts down the health monitor goroutine (if running).
func (pm *PeerManager) Stop() {
	pm.stopOnce.Do(func() { close(pm.stopCh) })
}
