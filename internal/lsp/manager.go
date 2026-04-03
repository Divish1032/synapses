package lsp

import (
	"context"
	"sync"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
)

const (
	// DefaultIdleTimeout is the duration an LSP process may sit idle before
	// Manager shuts it down to reclaim resources. It restarts lazily on the next
	// ResolveEdge call.
	DefaultIdleTimeout = 5 * time.Minute

	// DefaultCacheTTL is the time-to-live for cached VerifiedEdge results.
	// Call sites are stable within a file version, so a relatively long TTL is safe.
	DefaultCacheTTL = 10 * time.Minute

	// DefaultMaxCacheEntries limits the resolved-edge cache to avoid unbounded growth.
	DefaultMaxCacheEntries = 10_000
)

// Options configures a Manager.
type Options struct {
	// IdleTimeout is how long an LSP verifier process may be idle before Manager
	// stops it. Defaults to DefaultIdleTimeout.
	IdleTimeout time.Duration

	// CacheTTL is the time-to-live for cached VerifiedEdge entries.
	// Defaults to DefaultCacheTTL.
	CacheTTL time.Duration

	// MaxCacheEntries caps the number of cached resolutions per Manager.
	// Defaults to DefaultMaxCacheEntries.
	MaxCacheEntries int
}

func (o *Options) withDefaults() Options {
	out := *o
	if out.IdleTimeout <= 0 {
		out.IdleTimeout = DefaultIdleTimeout
	}
	if out.CacheTTL <= 0 {
		out.CacheTTL = DefaultCacheTTL
	}
	if out.MaxCacheEntries <= 0 {
		out.MaxCacheEntries = DefaultMaxCacheEntries
	}
	return out
}

// cacheKey uniquely identifies a resolution query. File:line:col is sufficient
// because LSP resolution is deterministic for a given source position.
type cacheKey struct {
	file string
	line int
	col  int
}

// cacheEntry holds a cached VerifiedEdge and its expiry time.
type cacheEntry struct {
	edge    *VerifiedEdge
	expiresAt time.Time
}

// Manager holds per-language EdgeVerifiers and a shared result cache.
// It is the primary entry point for LSP edge resolution in Synapses.
//
// Usage:
//
//	m := lsp.NewManager(lsp.Options{})
//	m.Register(goplsVerifier)           // called by Sprint 28.2
//
//	edge, err := m.ResolveEdge(ctx, from, to, pos)
//
// When no verifier is registered for a language, Get returns a NoOpVerifier.
// Manager is safe for concurrent use after construction.
//
// Lock ordering: mu → lastUsedMu. Never take lastUsedMu while holding mu's
// read lock from a concurrent path — take mu first, then lastUsedMu.
type Manager struct {
	opts      Options
	mu        sync.RWMutex
	verifiers map[Language]EdgeVerifier

	cacheMu sync.Mutex
	cache   map[cacheKey]cacheEntry

	// lastUsedMu protects lastUsed. Lock ordering: mu → lastUsedMu.
	lastUsedMu sync.Mutex
	// lastUsed records the most recent ResolveEdge call time per language.
	// Used by TrimIdle to close verifiers that have sat idle longer than
	// opts.IdleTimeout without being queried.
	lastUsed map[Language]time.Time
}

// NewManager constructs a Manager with the given options.
// Call Register to add language-specific verifiers.
func NewManager(opts Options) *Manager {
	o := opts.withDefaults()
	return &Manager{
		opts:      o,
		verifiers: make(map[Language]EdgeVerifier),
		cache:     make(map[cacheKey]cacheEntry),
		lastUsed:  make(map[Language]time.Time),
	}
}

// Register adds or replaces the EdgeVerifier for a language.
// If a previous verifier was registered for the same language, it is closed
// after the new verifier is installed. Close is called outside the lock so
// that a slow LSP process shutdown does not block concurrent Get calls.
func (m *Manager) Register(v EdgeVerifier) {
	m.mu.Lock()
	prev := m.verifiers[v.Language()] // may be nil
	m.verifiers[v.Language()] = v
	m.mu.Unlock()
	if prev != nil {
		_ = prev.Close() // best-effort; ignore error on replacement
	}
}

// Get returns the EdgeVerifier registered for the given language.
// If no verifier has been registered, it returns NoOpVerifier(lang).
func (m *Manager) Get(lang Language) EdgeVerifier {
	m.mu.RLock()
	v, ok := m.verifiers[lang]
	m.mu.RUnlock()
	if !ok {
		return NoOpVerifier(lang)
	}
	return v
}

// ResolveEdge is a convenience method that selects the correct verifier for
// the given file's language and resolves the edge, checking the cache first.
//
// lang must be set by the caller; Manager does not infer language from the
// file extension. (Language detection belongs in the parser layer.)
func (m *Manager) ResolveEdge(ctx context.Context, lang Language, from, to graph.NodeID, pos CallPosition) (*VerifiedEdge, error) {
	// Check cache before dispatching to a verifier.
	if pos.File != "" {
		if cached := m.fromCache(pos); cached != nil {
			return cached, nil
		}
	}

	edge, err := m.Get(lang).ResolveEdge(ctx, from, to, pos)
	if err != nil {
		return nil, err
	}

	// Record activity time for this language so TrimIdle can enforce IdleTimeout.
	// Updated even for NoOp results — the intent is to track when this language
	// was last queried, regardless of whether LSP is actually running.
	m.lastUsedMu.Lock()
	m.lastUsed[lang] = time.Now()
	m.lastUsedMu.Unlock()

	// Cache results from real verifiers (not no-ops — ConfidenceNone is not
	// worth caching since the no-op always returns the same cheap result).
	if pos.File != "" && edge.Confidence > ConfidenceNone {
		m.toCache(pos, edge)
	}

	return edge, nil
}

// Close shuts down all registered verifiers. Idempotent.
// Should be called when the Manager is no longer needed (e.g. daemon shutdown).
// Close calls each verifier's Close after releasing the write lock to prevent
// a slow LSP process shutdown from blocking concurrent Get calls.
func (m *Manager) Close() {
	m.mu.Lock()
	verifiers := make([]EdgeVerifier, 0, len(m.verifiers))
	for lang, v := range m.verifiers {
		verifiers = append(verifiers, v)
		delete(m.verifiers, lang)
	}
	m.mu.Unlock()
	for _, v := range verifiers {
		_ = v.Close()
	}
}

// CacheSize returns the current number of entries in the resolution cache.
// Primarily useful for testing and observability.
func (m *Manager) CacheSize() int {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	return len(m.cache)
}

// fromCache returns a cached VerifiedEdge for pos, or nil if not found or expired.
func (m *Manager) fromCache(pos CallPosition) *VerifiedEdge {
	k := cacheKey{file: pos.File, line: pos.Line, col: pos.Col}
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	entry, ok := m.cache[k]
	if !ok {
		return nil
	}
	if time.Now().After(entry.expiresAt) {
		delete(m.cache, k)
		return nil
	}
	return entry.edge
}

// toCache stores a VerifiedEdge in the cache for pos.
// If the cache has reached MaxCacheEntries, the insertion is skipped rather
// than evicting — eviction is deferred to periodic expiry cleanup.
func (m *Manager) toCache(pos CallPosition, edge *VerifiedEdge) {
	k := cacheKey{file: pos.File, line: pos.Line, col: pos.Col}
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	if len(m.cache) >= m.opts.MaxCacheEntries {
		return // capacity guard — skip rather than evict
	}
	m.cache[k] = cacheEntry{
		edge:    edge,
		expiresAt: time.Now().Add(m.opts.CacheTTL),
	}
}

// VerifySymbol asks the LSP to resolve the symbol at the given source position
// and reports whether the definition is confirmed to be at that same location.
//
// This is the primary entry point for Sprint 28.5 LSP-triggered re-verification.
// The security enricher calls it to upgrade MEDIUM-confidence findings (name-based
// tree-sitter matches) to HIGH when LSP confirms the entity identity via the type
// system, and to upgrade LOW-confidence findings (heuristic BFS) to MEDIUM.
//
// lang is the language string ("go", "typescript"). file is an absolute path.
// line and col are zero-indexed (LSP specification). A col of 0 is safe for
// most definitions — gopls and tsserver resolve function names from the start of
// the function keyword line.
//
// Returns (true, nil) when LSP resolves the symbol and the definition points to
// the same file and line as queried. Returns (false, nil) when LSP resolves to a
// different location, cannot resolve, or no verifier is registered for lang.
// Returns (false, err) only for transient LSP failures (process crash, timeout).
func (m *Manager) VerifySymbol(ctx context.Context, lang string, file string, line, col int) (bool, error) {
	if file == "" {
		return false, nil
	}
	pos := CallPosition{File: file, Line: line, Col: col}
	// Use empty NodeIDs: we are not resolving a named graph edge, just checking
	// whether LSP agrees a symbol exists at this exact source position.
	edge, err := m.Get(Language(lang)).ResolveEdge(ctx, graph.NodeID(""), graph.NodeID(""), pos)
	if err != nil {
		return false, err
	}

	// Update idle-timeout tracking so TrimIdle does not reclaim a verifier that is
	// actively serving VerifySymbol calls. This mirrors the tracking done by
	// Manager.ResolveEdge, which VerifySymbol bypasses to avoid cache pollution
	// (the empty NodeIDs would corrupt cached VerifiedEdge entries used by real
	// edge-resolution callers).
	m.lastUsedMu.Lock()
	m.lastUsed[Language(lang)] = time.Now()
	m.lastUsedMu.Unlock()

	if edge.Confidence < ConfidenceMedium {
		return false, nil // LSP unavailable or could not resolve
	}
	// Confirmed: LSP returned a definition at the same file and zero-indexed line.
	return edge.Callee.File == file && edge.Callee.Line == line, nil
}

// PurgeExpired removes all expired cache entries. Can be called periodically
// by background goroutines (e.g. watcher's maintenance tick). Safe to call
// at any time; if never called, expired entries are lazily evicted on read.
func (m *Manager) PurgeExpired() {
	now := time.Now()
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	for k, entry := range m.cache {
		if now.After(entry.expiresAt) {
			delete(m.cache, k)
		}
	}
}

// TrimIdle closes verifiers that have not been used for longer than
// opts.IdleTimeout, freeing the underlying LSP process (gopls, tsserver, etc.)
// to reclaim system resources. The verifier restarts lazily on the next
// ResolveEdge call.
//
// TrimIdle is designed to be called periodically — for example, once per
// minute by the daemon's maintenance goroutine — rather than on every query.
// It is safe to call from multiple goroutines concurrently.
//
// Lock ordering: mu → lastUsedMu. Both are acquired briefly, then released
// before calling v.Close() to avoid blocking concurrent Get calls.
func (m *Manager) TrimIdle() {
	now := time.Now()

	// Collect idle verifiers under both locks, then close outside all locks.
	m.mu.Lock()
	m.lastUsedMu.Lock()
	var toClose []EdgeVerifier
	for lang, v := range m.verifiers {
		last, ok := m.lastUsed[lang]
		if !ok || now.Sub(last) >= m.opts.IdleTimeout {
			toClose = append(toClose, v)
			delete(m.verifiers, lang)
			delete(m.lastUsed, lang)
		}
	}
	m.lastUsedMu.Unlock()
	m.mu.Unlock()

	for _, v := range toClose {
		_ = v.Close()
	}
}
