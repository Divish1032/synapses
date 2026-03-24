// registry.go — per-project instance registry for the singleton daemon.
//
// The singleton daemon serves multiple projects within one process.
// Each project gets its own ProjectInstance: graph, store, MCP server,
// file watcher, brain client, and a per-project Unix socket listener.
package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"
	"golang.org/x/sync/singleflight"

	"github.com/SynapsesOS/synapses/internal/brain"
	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/SynapsesOS/synapses/internal/graph"
	mcpsrv "github.com/SynapsesOS/synapses/internal/mcp"
	"github.com/SynapsesOS/synapses/internal/store"
	"github.com/SynapsesOS/synapses/internal/watcher"
)

// ProjectInstance holds all resources owned by the daemon for one project.
// It is created by initProjectInstance and managed by projectRegistry.
type ProjectInstance struct {
	AbsPath     string
	Graph       *graph.Graph
	Store       *store.Store
	MCPServer   *mcpsrv.Server
	HTTPHandler *mcpserver.StreamableHTTPServer // HTTP MCP endpoint for this project
	BrainClient    *brain.Client
	Watcher        *watcher.Watcher
	MemoryEmbedder embed.Embedder    // closed on project shutdown, NOT via defer in init
	cancel         context.CancelFunc // cancels the project context (stops watcher, socket listener)
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
	if pi.Watcher != nil {
		defer pi.Watcher.Stop()
	}
	if pi.cancel != nil {
		pi.cancel()
	}
}

// maxActiveProjects caps the number of simultaneously loaded projects to
// prevent unbounded memory and file descriptor growth.
const maxActiveProjects = 64

// projectRegistry is a thread-safe map of canonicalAbsPath → ProjectInstance.
// The daemon holds a single registry shared across all HTTP handlers.
type projectRegistry struct {
	mu       sync.RWMutex
	projects map[string]*ProjectInstance
	sf       singleflight.Group
}

func newProjectRegistry() *projectRegistry {
	return &projectRegistry{
		projects: make(map[string]*ProjectInstance),
	}
}

// Get returns the instance for absPath, or (nil, false) if not registered.
func (r *projectRegistry) Get(absPath string) (*ProjectInstance, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	pi, ok := r.projects[absPath]
	return pi, ok
}

// Set stores a new project instance. Replaces any existing entry.
func (r *projectRegistry) Set(pi *ProjectInstance) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.projects[pi.AbsPath] = pi
}

// GetOrSet returns the existing instance, or calls init() and stores the result.
// Uses singleflight to ensure exactly one init() per path, preventing duplicate
// resource allocation when concurrent requests arrive for the same project.
func (r *projectRegistry) GetOrSet(absPath string, init func() (*ProjectInstance, error)) (*ProjectInstance, error) {
	r.mu.RLock()
	if pi, ok := r.projects[absPath]; ok {
		r.mu.RUnlock()
		return pi, nil
	}
	r.mu.RUnlock()

	// singleflight ensures only one init() runs per absPath.
	v, err, _ := r.sf.Do(absPath, func() (interface{}, error) {
		// Double-check under lock — another caller may have stored it
		// between our RUnlock above and singleflight selecting us.
		r.mu.RLock()
		if pi, ok := r.projects[absPath]; ok {
			r.mu.RUnlock()
			return pi, nil
		}
		r.mu.RUnlock()

		pi, err := init()
		if err != nil {
			return nil, err
		}

		r.mu.Lock()
		if len(r.projects) >= maxActiveProjects {
			r.mu.Unlock()
			pi.Close()
			return nil, fmt.Errorf("max active projects (%d) reached", maxActiveProjects)
		}
		r.projects[absPath] = pi
		r.mu.Unlock()
		return pi, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*ProjectInstance), nil
}

// Delete closes and removes a project instance.
func (r *projectRegistry) Delete(absPath string) {
	r.mu.Lock()
	pi, ok := r.projects[absPath]
	if ok {
		delete(r.projects, absPath)
	}
	r.mu.Unlock()
	if ok {
		pi.Close()
	}
}

// All returns a snapshot of all current project instances.
func (r *projectRegistry) All() []*ProjectInstance {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*ProjectInstance, 0, len(r.projects))
	for _, pi := range r.projects {
		out = append(out, pi)
	}
	return out
}

// Len returns the number of registered projects.
func (r *projectRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.projects)
}

// Close shuts down all project instances. Called when the daemon exits.
func (r *projectRegistry) Close() {
	r.mu.Lock()
	projects := r.projects
	r.projects = make(map[string]*ProjectInstance)
	r.mu.Unlock()
	for _, pi := range projects {
		pi.Close()
	}
}

// registryAdapter wraps projectRegistry to implement mcp.ProjectStoreProvider.
// Handles name collisions: if two projects share a basename (e.g. /code/api and
// /work/api), the second gets "api (work)" — parent dir appended for disambiguation.
type registryAdapter struct {
	reg *projectRegistry
}

// projectDisplayName returns a unique human-readable name for a project.
// Uses filepath.Base, with parent dir appended on collision.
func projectDisplayNames(projects []*ProjectInstance) map[string]string {
	// First pass: count basenames.
	baseCount := make(map[string]int)
	for _, p := range projects {
		baseCount[filepath.Base(p.AbsPath)]++
	}
	// Second pass: assign names, disambiguating collisions.
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
