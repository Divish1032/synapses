// registry.go — per-project instance registry for the singleton daemon.
//
// The singleton daemon serves multiple projects within one process.
// Each project gets its own ProjectInstance: graph, store, MCP server,
// file watcher, brain client, and a per-project Unix socket listener.
package main

import (
	"context"
	"sync"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/SynapsesOS/synapses/internal/brain"
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
	BrainClient *brain.Client
	Watcher     *watcher.Watcher
	cancel      context.CancelFunc // cancels the project context (stops watcher, socket listener)
}

// Close shuts down all resources owned by this instance.
// Called when the project is deregistered or the daemon exits.
func (pi *ProjectInstance) Close() {
	if pi.cancel != nil {
		pi.cancel()
	}
	if pi.Watcher != nil {
		pi.Watcher.Stop()
	}
	if pi.MCPServer != nil {
		pi.MCPServer.Close()
	}
	if pi.HTTPHandler != nil {
		pi.HTTPHandler.Shutdown(context.Background()) //nolint:errcheck
	}
	if pi.Store != nil {
		pi.Store.Close()
	}
	if pi.BrainClient != nil {
		pi.BrainClient.Close()
	}
}

// projectRegistry is a thread-safe map of canonicalAbsPath → ProjectInstance.
// The daemon holds a single registry shared across all HTTP handlers.
type projectRegistry struct {
	mu       sync.RWMutex
	projects map[string]*ProjectInstance
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
// The init function is called WITHOUT the registry lock held, so concurrent
// callers for the same path may call init concurrently. The winner's instance
// is stored; the loser discards its instance. This is safe because init()
// produces equivalent instances and the store/graph are idempotent.
func (r *projectRegistry) GetOrSet(absPath string, init func() (*ProjectInstance, error)) (*ProjectInstance, error) {
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
	if existing, ok := r.projects[absPath]; ok {
		// Another goroutine won the race — discard ours.
		r.mu.Unlock()
		pi.Close()
		return existing, nil
	}
	r.projects[absPath] = pi
	r.mu.Unlock()
	return pi, nil
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
