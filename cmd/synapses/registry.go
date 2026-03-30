// registry.go — per-project instance registry for the singleton daemon.
//
// The singleton daemon serves multiple projects within one process.
// Each project gets its own ProjectInstance: graph, store, MCP server,
// file watcher, brain client, and a per-project Unix socket listener.
//
// Projects have two lifecycle states:
//   - WARM:       fully loaded, serving requests
//   - HIBERNATED: snapshot on disk, tombstone in memory, sentinel watching .git/index
package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"
	"golang.org/x/sync/singleflight"

	"github.com/SynapsesOS/synapses/internal/brain"
	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/SynapsesOS/synapses/internal/federation"
	"github.com/SynapsesOS/synapses/internal/graph"
	mcpsrv "github.com/SynapsesOS/synapses/internal/mcp"
	"github.com/SynapsesOS/synapses/internal/store"
	"github.com/SynapsesOS/synapses/internal/watcher"
)

// ProjectInstance holds all resources owned by the daemon for one project.
// It is created by initProjectInstance and managed by projectRegistry.
type ProjectInstance struct {
	AbsPath            string
	Graph              *graph.Graph
	Store              *store.Store
	MCPServer          *mcpsrv.Server
	HTTPHandler        *mcpserver.StreamableHTTPServer // HTTP MCP endpoint for this project
	BrainClient        *brain.Client
	Watcher            *watcher.Watcher
	MemoryEmbedder     embed.Embedder       // closed on project shutdown, NOT via defer in init
	FederationResolver *federation.Resolver // nil when no federation configured
	cancel             context.CancelFunc   // cancels the project context (stops watcher, socket listener)

	mu sync.Mutex // guards Watcher and FederationResolver which may be set from background goroutines
}

// Close shuts down all resources owned by this instance.
// Called when the project is deregistered or the daemon exits.
// Each resource is closed via defer so a hang or panic in one closer does not
// prevent the remaining resources from being released.
func (pi *ProjectInstance) Close() {
	if pi.MemoryEmbedder != nil {
		defer pi.MemoryEmbedder.Close()
	}
	if pi.BrainClient != nil {
		defer pi.BrainClient.Close()
	}
	if pi.Store != nil {
		defer pi.Store.Close()
	}
	if pi.HTTPHandler != nil {
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			pi.HTTPHandler.Shutdown(ctx) //nolint:errcheck
		}()
	}
	if pi.MCPServer != nil {
		defer pi.MCPServer.Close()
	}
	pi.mu.Lock()
	fw := pi.Watcher
	pi.mu.Unlock()
	if fw != nil {
		defer fw.Stop()
	}
	if pi.cancel != nil {
		pi.cancel()
	}
}

// SetWatcher sets the file watcher, safe for concurrent use with Close.
func (pi *ProjectInstance) SetWatcher(fw *watcher.Watcher) {
	pi.mu.Lock()
	pi.Watcher = fw
	pi.mu.Unlock()
}

// SetFederationResolver sets the federation resolver, safe for concurrent use.
func (pi *ProjectInstance) SetFederationResolver(fr *federation.Resolver) {
	pi.mu.Lock()
	pi.FederationResolver = fr
	pi.mu.Unlock()
}

// maxActiveProjects caps the number of simultaneously loaded (WARM) projects to
// prevent unbounded memory and file descriptor growth.
const maxActiveProjects = 64

// projectState represents the lifecycle state of a project in the registry.
type projectState int

const (
	stateWarm        projectState = iota // fully loaded, serving requests
	stateHibernating                     // transitioning to hibernated (no new requests)
	stateHibernated                      // snapshot on disk, tombstone in memory
)

// HibernatedProject is the lightweight tombstone kept in memory for a
// hibernated project. Cost: ~200 bytes + one goroutine (sentinel watcher).
type HibernatedProject struct {
	AbsPath      string
	HibernatedAt time.Time
	Dirty        atomic.Bool   // set by sentinel when .git/index or root dir changes
	sentinelStop chan struct{} // close to stop sentinel goroutine
	stopOnce     sync.Once    // prevents double-close panic on sentinelStop
}

// StopSentinel safely stops the sentinel watcher goroutine. Idempotent.
func (h *HibernatedProject) StopSentinel() {
	h.stopOnce.Do(func() {
		close(h.sentinelStop)
	})
}

// registryEntry wraps either a WARM ProjectInstance or a HIBERNATED tombstone.
type registryEntry struct {
	state       projectState
	instance    *ProjectInstance    // non-nil when state == stateWarm
	hibernated  *HibernatedProject  // non-nil when state == stateHibernated
	lastAccess  atomic.Int64        // UnixNano — updated on every Get/GetOrSet
	activeConns atomic.Int64        // proxy socket connections (IDE is open if > 0)
}

// projectRegistry is a thread-safe map of canonicalAbsPath → registryEntry.
// The daemon holds a single registry shared across all HTTP handlers.
type projectRegistry struct {
	mu       sync.RWMutex
	projects map[string]*registryEntry
	sf       singleflight.Group
	wakeFunc func(absPath string) (*ProjectInstance, error) // set by hibernate system
}

func newProjectRegistry() *projectRegistry {
	return &projectRegistry{
		projects: make(map[string]*registryEntry),
	}
}

// Get returns the instance for absPath if it is WARM, or (nil, false) if not
// registered, hibernated, or hibernating.
func (r *projectRegistry) Get(absPath string) (*ProjectInstance, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.projects[absPath]
	if !ok || e.state != stateWarm {
		return nil, false
	}
	e.lastAccess.Store(time.Now().UnixNano())
	return e.instance, true
}

// getEntry returns the raw registry entry (any state) for internal use.
func (r *projectRegistry) getEntry(absPath string) *registryEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.projects[absPath]
}

// Set stores a new WARM project instance. Replaces any existing entry.
func (r *projectRegistry) Set(pi *ProjectInstance) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := &registryEntry{
		state:    stateWarm,
		instance: pi,
	}
	entry.lastAccess.Store(time.Now().UnixNano())
	r.projects[pi.AbsPath] = entry
}

// GetOrSet returns the existing WARM instance, wakes a HIBERNATED project, or
// calls init() for a brand-new project.
// Uses singleflight to ensure exactly one init()/wake per path, preventing
// duplicate resource allocation when concurrent requests arrive.
func (r *projectRegistry) GetOrSet(absPath string, init func() (*ProjectInstance, error)) (*ProjectInstance, error) {
	r.mu.RLock()
	if e, ok := r.projects[absPath]; ok && e.state == stateWarm {
		e.lastAccess.Store(time.Now().UnixNano())
		r.mu.RUnlock()
		return e.instance, nil
	}
	r.mu.RUnlock()

	// singleflight ensures only one init()/wake runs per absPath.
	v, err, _ := r.sf.Do(absPath, func() (interface{}, error) {
		// Double-check under lock — another caller may have stored/woken it.
		r.mu.RLock()
		if e, ok := r.projects[absPath]; ok && e.state == stateWarm {
			e.lastAccess.Store(time.Now().UnixNano())
			r.mu.RUnlock()
			return e.instance, nil
		}
		var wasHibernated bool
		if e, ok := r.projects[absPath]; ok && (e.state == stateHibernated || e.state == stateHibernating) {
			wasHibernated = true
		}
		r.mu.RUnlock()

		if wasHibernated && r.wakeFunc != nil {
			// If HIBERNATING, the hibernate is in progress on another goroutine.
			// wakeFunc will wait for FinishHibernate (which won't clobber us
			// because FinishHibernate checks for stateHibernating), or if
			// the project is already HIBERNATED, it wakes normally.
			pi, err := r.wakeFunc(absPath)
			if err != nil {
				return nil, err
			}
			return pi, nil
		}

		// Cold init (first time ever, or wake not available).
		pi, err := init()
		if err != nil {
			return nil, err
		}

		r.mu.Lock()
		warmCount := r.warmCountLocked()
		if warmCount >= maxActiveProjects {
			// Try to make room by closing the least-recently-used idle project.
			// We remove it from the map under lock then close outside.
			victim := r.removeLRUIdleLocked()
			r.mu.Unlock()
			if victim != nil {
				victim.Close() // close outside lock — safe, no one can Get() it
			} else {
				pi.Close()
				return nil, fmt.Errorf("max active projects (%d) reached", maxActiveProjects)
			}
			r.mu.Lock()
		}
		entry := &registryEntry{
			state:    stateWarm,
			instance: pi,
		}
		entry.lastAccess.Store(time.Now().UnixNano())
		r.projects[absPath] = entry
		r.mu.Unlock()
		return pi, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*ProjectInstance), nil
}

// SetWakeFunc registers the function that wakes hibernated projects.
// Called from GetOrSet when a HIBERNATED project is accessed.
// Must be called before the registry starts serving requests.
func (r *projectRegistry) SetWakeFunc(fn func(absPath string) (*ProjectInstance, error)) {
	r.wakeFunc = fn
}

// BeginHibernate atomically transitions a WARM project to HIBERNATING state,
// returning the ProjectInstance for the caller to close. Returns nil if the
// project is not WARM or has active connections/sessions.
// While HIBERNATING, Get() returns (nil, false) so no new requests use the instance.
func (r *projectRegistry) BeginHibernate(absPath string) *ProjectInstance {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.projects[absPath]
	if !ok || e.state != stateWarm {
		return nil
	}
	if e.activeConns.Load() > 0 {
		return nil
	}
	if e.instance.MCPServer != nil && e.instance.MCPServer.ActiveSessionCount() > 0 {
		return nil
	}
	// Atomically mark as HIBERNATING — no one can Get() this instance anymore.
	e.state = stateHibernating
	return e.instance
}

// FinishHibernate completes the transition to HIBERNATED state by installing
// the tombstone. Must be called after BeginHibernate + pi.Close().
// Only writes if the project is still in HIBERNATING state — if a concurrent
// GetOrSet already woke/recreated the project, the tombstone is discarded.
func (r *projectRegistry) FinishHibernate(absPath string, tomb *HibernatedProject) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.projects[absPath]
	if !ok || e.state != stateHibernating {
		// Someone else already changed the state (concurrent wake or cold init).
		// Stop the sentinel we were about to install — it's orphaned.
		tomb.StopSentinel()
		return
	}
	entry := &registryEntry{
		state:      stateHibernated,
		hibernated: tomb,
	}
	entry.lastAccess.Store(time.Now().UnixNano())
	r.projects[absPath] = entry
}

// Hibernate directly creates a HIBERNATED entry (for startup and tests).
// Unlike FinishHibernate, this does NOT require prior stateHibernating.
func (r *projectRegistry) Hibernate(absPath string, tomb *HibernatedProject) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := &registryEntry{
		state:      stateHibernated,
		hibernated: tomb,
	}
	entry.lastAccess.Store(time.Now().UnixNano())
	r.projects[absPath] = entry
}

// WarmCount returns the number of WARM (fully loaded) projects.
func (r *projectRegistry) WarmCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.warmCountLocked()
}

func (r *projectRegistry) warmCountLocked() int {
	n := 0
	for _, e := range r.projects {
		if e.state == stateWarm {
			n++
		}
	}
	return n
}

// IsHibernated returns true if the project is registered and in HIBERNATED state.
func (r *projectRegistry) IsHibernated(absPath string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.projects[absPath]
	return ok && e.state == stateHibernated
}

// Delete closes and removes a project instance (any state).
func (r *projectRegistry) Delete(absPath string) {
	r.mu.Lock()
	e, ok := r.projects[absPath]
	if ok {
		delete(r.projects, absPath)
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	switch e.state {
	case stateWarm:
		if e.instance != nil {
			e.instance.Close()
		}
	case stateHibernating:
		// Instance is being closed by hibernateProject; don't double-close.
	case stateHibernated:
		e.hibernated.StopSentinel()
	}
}

// All returns a snapshot of all WARM project instances.
func (r *projectRegistry) All() []*ProjectInstance {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*ProjectInstance, 0, len(r.projects))
	for _, e := range r.projects {
		if e.state == stateWarm {
			out = append(out, e.instance)
		}
	}
	return out
}

// HibernatedPaths returns the absolute paths of all HIBERNATED projects.
func (r *projectRegistry) HibernatedPaths() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var paths []string
	for path, e := range r.projects {
		if e.state == stateHibernated {
			paths = append(paths, path)
		}
	}
	return paths
}

// Len returns the number of WARM projects (backward-compatible).
func (r *projectRegistry) Len() int {
	return r.WarmCount()
}

// Close shuts down all project instances and sentinel watchers.
// Called when the daemon exits.
func (r *projectRegistry) Close() {
	r.mu.Lock()
	projects := r.projects
	r.projects = make(map[string]*registryEntry)
	r.mu.Unlock()
	for _, e := range projects {
		switch e.state {
		case stateWarm:
			if e.instance != nil {
				e.instance.Close()
			}
		case stateHibernating:
			// Instance is being closed by hibernateProject; don't double-close.
		case stateHibernated:
			e.hibernated.StopSentinel()
		}
	}
}

// removeLRUIdleLocked removes and returns the least-recently-used WARM
// ProjectInstance that has no active connections or sessions.
// Caller must hold r.mu write lock. Returns nil if no evictable project found.
// The removed project is NOT closed — caller must close it outside the lock.
func (r *projectRegistry) removeLRUIdleLocked() *ProjectInstance {
	var oldestPath string
	var oldestAccess int64 = 1<<63 - 1

	for path, e := range r.projects {
		if e.state != stateWarm {
			continue
		}
		if e.activeConns.Load() > 0 {
			continue
		}
		if e.instance.MCPServer != nil && e.instance.MCPServer.ActiveSessionCount() > 0 {
			continue
		}
		access := e.lastAccess.Load()
		if access < oldestAccess {
			oldestAccess = access
			oldestPath = path
		}
	}

	if oldestPath == "" {
		return nil
	}

	e := r.projects[oldestPath]
	delete(r.projects, oldestPath)
	return e.instance
}

// registryAdapter wraps projectRegistry to implement mcp.ProjectStoreProvider.
type registryAdapter struct {
	reg *projectRegistry
}

func projectDisplayNames(projects []*ProjectInstance) map[string]string {
	baseCount := make(map[string]int)
	for _, p := range projects {
		baseCount[filepath.Base(p.AbsPath)]++
	}
	names := make(map[string]string, len(projects))
	for _, p := range projects {
		base := filepath.Base(p.AbsPath)
		if baseCount[base] > 1 {
			parent := filepath.Base(filepath.Dir(p.AbsPath))
			names[p.AbsPath] = base + " (" + parent + ")"
		} else {
			names[p.AbsPath] = base
		}
	}
	return names
}

func (a *registryAdapter) ListProjects() []string {
	projects := a.reg.All()
	nameMap := projectDisplayNames(projects)
	names := make([]string, 0, len(projects))
	for _, p := range projects {
		names = append(names, nameMap[p.AbsPath])
	}
	return names
}

func (a *registryAdapter) GetStore(name string) *store.Store {
	projects := a.reg.All()
	nameMap := projectDisplayNames(projects)
	for _, p := range projects {
		if nameMap[p.AbsPath] == name {
			return p.Store
		}
	}
	return nil
}
