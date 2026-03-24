// Package webcache provides a Go-native HTTP fetch + SQLite cache for web
// documentation. It replaces the synapses-scout Python sidecar for the core
// use cases that have no built-in equivalent in AI agent runtimes:
//
//   - Fetching and caching Go package documentation at a pinned version
//     (invalidated only when go.mod changes, never on a timer)
//   - Caching arbitrary URLs for cross-session reuse (24h TTL)
//
// All network fetches are performed by the caller's goroutine or the background
// indexer — never on the MCP hot path.
package webcache

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/SynapsesOS/synapses/internal/store"
)

const (
	// PkgDocTTL is 0 — version-pinned package docs never expire on their own.
	// They are invalidated only when go.mod bumps a package version.
	PkgDocTTL = 0

	// URLCacheTTL is 24 hours for arbitrary URL content.
	URLCacheTTL = 24

	// pkgPrefix is the cache key prefix for Go package docs.
	pkgPrefix = "pkg:"

	// pkgDocBaseURL is the base URL for Go package documentation.
	pkgDocBaseURL = "https://pkg.go.dev/"

	// maxDocBytes caps the content stored per entry to avoid ballooning the DB.
	maxDocBytes = 8 * 1024 // 8 KB

	httpTimeout = 15 * time.Second
)

// Cache is the Go-native web documentation cache backed by the main store DB.
// It is safe for concurrent use.
type Cache struct {
	store      *store.Store
	httpClient *http.Client
}

// New creates a Cache backed by the given store with SSRF protection enabled.
func New(s *store.Store) *Cache {
	// SSRF Protection: custom Dialer that rejects private/loopback/metadata IPs.
	dialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
					ip.IsUnspecified() || ip.IsMulticast() {
					return nil, fmt.Errorf("SSRF prevention blocked access to %s (%s)", host, ip.String())
				}
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("no IPs resolved for %s", host)
			}
			var lastErr error
			for _, ip := range ips {
				conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if dialErr == nil {
					return conn, nil
				}
				lastErr = dialErr
			}
			return nil, fmt.Errorf("all %d IPs for %s failed: %w", len(ips), host, lastErr)
		},
	}

	return &Cache{
		store: s,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   httpTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("too many redirects")
				}
				host := req.URL.Hostname()
				// Case 1: redirect target is an IP literal — check directly.
				if ip := net.ParseIP(host); ip != nil {
					if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
						return fmt.Errorf("redirect to private IP blocked: %s", host)
					}
					return nil
				}
				// Case 2: hostname-based redirect — resolve DNS with a short
				// timeout and reject if any resolved IP is private/internal.
				// Fail-closed on DNS errors: an unresolvable redirect target is blocked.
				dnsCtx, dnsCancel := context.WithTimeout(req.Context(), 500*time.Millisecond)
				defer dnsCancel()
				addrs, lookupErr := net.DefaultResolver.LookupIPAddr(dnsCtx, host)
				if lookupErr != nil {
					return fmt.Errorf("DNS lookup failed for redirect target %s: %w", host, lookupErr)
				}
				if len(addrs) == 0 {
					return fmt.Errorf("no IPs resolved for redirect target %s", host)
				}
				for _, addr := range addrs {
					ip := addr.IP
					if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
						return fmt.Errorf("redirect to internal hostname blocked: %s resolves to %s", host, ip.String())
					}
				}
				// Pin the subsequent dial to the verified IP to prevent DNS rebinding.
				port := req.URL.Port()
				if port == "" {
					if req.URL.Scheme == "https" {
						port = "443"
					} else {
						port = "80"
					}
				}
				req.URL.Host = net.JoinHostPort(addrs[0].IP.String(), port)
				return nil
			},
		},
	}
}

// PackageCacheKey returns the web_cache URL key for a Go package at a version.
// Example: "pkg:github.com/mark3labs/mcp-go@v0.32.0"
func PackageCacheKey(importPath, version string) string {
	if version == "" {
		return pkgPrefix + importPath
	}
	return pkgPrefix + importPath + "@" + version
}

// IsStdlib returns true if importPath is a Go standard library package.
// Stdlib packages never contain a dot in the first path segment.
func IsStdlib(importPath string) bool {
	first := strings.SplitN(importPath, "/", 2)[0]
	return !strings.Contains(first, ".")
}

// FetchPackageDocs returns the cached docs for a Go package at the given version.
// If not cached (or version changed), it fetches from pkg.go.dev and caches with
// TTL=0 (version-pinned — valid until go.mod changes).
//
// Returns (content, fromCache, error).
func (c *Cache) FetchPackageDocs(ctx context.Context, importPath, version string) (string, bool, error) {
	key := PackageCacheKey(importPath, version)
	if entry, ok := c.store.GetWebCache(key); ok {
		return entry.Content, true, nil
	}

	docURL := pkgDocBaseURL + importPath
	if version != "" {
		docURL += "@" + version
	}

	content, err := c.fetchAndStrip(ctx, docURL)
	if err != nil {
		return "", false, fmt.Errorf("fetch pkg docs %s: %w", importPath, err)
	}

	if err := c.store.UpsertWebCache(key, content, PkgDocTTL); err != nil {
		// Cache write failure is non-fatal — return content anyway.
		_ = err
	}
	return content, false, nil
}

// Fetch returns content for a URL, using the cache when available.
// ttlHours controls expiry: 0 = never expire, >0 = expire after N hours.
//
// Returns (content, fromCache, error).
func (c *Cache) Fetch(ctx context.Context, rawURL string, ttlHours int) (string, bool, error) {
	cacheKey := rawURL
	if u, err := url.Parse(rawURL); err == nil && u.User != nil {
		h := sha256.Sum256([]byte(u.User.String()))
		u.User = nil
		cacheKey = u.String() + "|userinfo=" + hex.EncodeToString(h[:8])
	}
	if entry, ok := c.store.GetWebCache(cacheKey); ok {
		return entry.Content, true, nil
	}

	content, err := c.fetchAndStrip(ctx, rawURL)
	if err != nil {
		return "", false, fmt.Errorf("fetch %s: %w", cacheKey, err)
	}

	if err := c.store.UpsertWebCache(cacheKey, content, ttlHours); err != nil {
		_ = err
	}
	return content, false, nil
}

// FetchFresh fetches url from the network without reading or writing the cache.
// Use this when the user has disabled web doc caching (cache_web_searches=false).
// Returns (content, false, error) — fromCache is always false.
func (c *Cache) FetchFresh(ctx context.Context, url string) (string, error) {
	return c.fetchAndStrip(ctx, url)
}

// FetchPackageDocsFresh fetches package docs without reading or writing the cache.
func (c *Cache) FetchPackageDocsFresh(ctx context.Context, importPath, version string) (string, error) {
	docURL := pkgDocBaseURL + importPath
	if version != "" {
		docURL += "@" + version
	}
	return c.fetchAndStrip(ctx, docURL)
}

// InvalidatePackage removes cached docs for importPath at oldVersion so they
// will be re-fetched at the new version. Called when go.mod bumps a dependency.
func (c *Cache) InvalidatePackage(importPath, oldVersion string) {
	prefix := pkgPrefix + importPath
	_ = c.store.DeleteWebCachePrefix(prefix)
}

// ParseGoMod parses the go.mod file at projectPath and returns a map of
// module import path → version for all direct and indirect dependencies.
func ParseGoMod(projectPath string) (map[string]string, error) {
	gomodPath := filepath.Join(projectPath, "go.mod")
	f, err := os.Open(gomodPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // not a Go module — not an error
		}
		return nil, fmt.Errorf("open go.mod: %w", err)
	}
	defer f.Close()

	versions := make(map[string]string)
	scanner := bufio.NewScanner(f)
	inRequire := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "require (" {
			inRequire = true
			continue
		}
		if inRequire && line == ")" {
			inRequire = false
			continue
		}
		// Single-line require: require github.com/foo/bar v1.2.3
		isSingleRequire := false
		if strings.HasPrefix(line, "require ") && !strings.HasSuffix(line, "(") {
			line = strings.TrimPrefix(line, "require ")
			line = strings.TrimSpace(line)
			isSingleRequire = true
		}
		if inRequire || isSingleRequire {
			// Strip inline comment
			if idx := strings.Index(line, "//"); idx >= 0 {
				line = strings.TrimSpace(line[:idx])
			}
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				versions[parts[0]] = parts[1]
			}
		}
	}
	return versions, scanner.Err()
}

// fetchAndStrip performs an HTTP GET and returns HTML-stripped plain text,
// capped at maxDocBytes.
func (c *Cache) fetchAndStrip(ctx context.Context, url string) (string, error) {
	// Validate URL scheme — only allow http and https.
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return "", fmt.Errorf("unsupported URL scheme (only http/https allowed): %s", url)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "synapses-webcache/1.0 (+https://github.com/SynapsesOS/synapses)")
	req.Header.Set("Accept", "text/html,text/plain")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}

	// Cap read size to limit allocation. The final output is truncated to
	// maxDocBytes (8KB) of stripped text, so we only need enough raw HTML to
	// produce that. Real-world measurements:
	//   - Typical docs page: ~30KB HTML → ~5KB text  (6:1 ratio)
	//   - Script-heavy SPA:  ~80KB HTML → ~3KB text  (27:1 ratio)
	//   - Worst case (ads):  ~120KB HTML → ~4KB text (30:1 ratio)
	// 128KB covers all realistic pages with a 16:1 safety margin over the
	// 8KB target, saving ~384KB vs the original 512KB limit per fetch.
	limited := io.LimitReader(resp.Body, 128*1024)
	raw, err := io.ReadAll(limited)
	// Drain remaining body so the HTTP transport can reuse the connection.
	_, _ = io.Copy(io.Discard, resp.Body)
	if err != nil {
		return "", err
	}

	text := StripHTML(string(raw))
	if len(text) > maxDocBytes {
		// Truncate at rune boundary to avoid splitting multi-byte UTF-8 chars.
		// Walk backwards from the byte limit to find a valid rune start.
		truncated := text[:maxDocBytes]
		for i := len(truncated) - 1; i >= len(truncated)-utf8.UTFMax && i >= 0; i-- {
			if utf8.RuneStart(truncated[i]) {
				truncated = truncated[:i]
				break
			}
		}
		text = truncated
	}
	return strings.TrimSpace(text), nil
}
