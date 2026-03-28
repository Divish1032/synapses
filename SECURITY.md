# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in Synapses, please report it responsibly to the maintainers **before** disclosing it publicly.

### How to Report

**Email**: security@synapsesos.dev (or open a GitHub private security advisory)

**What to include**:
- Description of the vulnerability
- Steps to reproduce
- Potential impact
- Suggested fix (if available)

### Response SLA

- **Acknowledgment**: Within 24 hours
- **Initial assessment**: Within 3 business days
- **Fix timeline**: Severity-dependent (see below)

### Severity Levels

| Severity | Definition | Fix Timeline |
|----------|-----------|--------------|
| Critical | Remote code execution, arbitrary file access, credential leakage | 24 hours (emergency patch) |
| High | Local privilege escalation, data exposure via graph queries, injection attacks | 1 week |
| Medium | Information disclosure, DoS conditions, default configurations | 2 weeks |
| Low | Minor issues, non-exploitable edge cases | Next release |

---

## In-Scope Vulnerabilities

The following components are in scope for security reports:

✅ **Daemon HTTP Server** — Auth bypass, CSRF, DNS rebinding, port binding, socket activation
✅ **MCP Protocol Implementation** — Tool inputs, parameter validation, HTTP + stdio transport
✅ **SQLite Store** — Query injection, data integrity, file permissions
✅ **Rate Limiter & Loop Guard** — Bypass via reconnection, cycle detection evasion
✅ **Sidecar HTTP Clients** — Brain and Scout API communication, timeout handling
✅ **File Watcher** — Path traversal, symlink handling
✅ **Parser** — Malformed input handling, resource exhaustion
✅ **Graph Engine** — Memory leaks, unbounded recursion

---

## Out-of-Scope

The following are **not** in scope for security reports (but may be reported as bugs):

❌ **Dependency Vulnerabilities** — Report upstream; we'll update when available
❌ **Sidecar Security** — Report to synapses-intelligence or synapses-scout repositories
❌ **IDE-Specific Issues** — Report to the IDE vendor
❌ **Local Privilege Escalation (general)** — OS-level issues

---

## Security Design Decisions

### No Cloud, No Data Transmission

- All inference is **local** (brain sidecar)
- No code or context leaves your machine
- SQLite cache stored at `~/.cache/synapses/` (user-readable only)

### No CGo at Runtime

- Pure Go implementation (except parser)
- Reduces attack surface
- No native code execution vulnerabilities

### Fail-Silent Pattern

- MCP tools **never panic** — `WithRecovery()` middleware catches panics in tool handlers
- Brain sidecar unavailable? Graph queries still work
- Scout down? Web tools return "unavailable"
- Reduces denial-of-service risk

### Daemon Resilience

The singleton daemon runs on `127.0.0.1:11435` with multiple layers of protection:

- **Socket activation** — launchd (macOS) / systemd (Linux) holds the port. During daemon restart, connections queue in kernel backlog instead of getting "connection refused"
- **Panic recovery** — 4 layers: mcp-go `WithRecovery()` for tools, `defer recover()` in HTTP handler, Go stdlib per-connection recovery, process supervisor auto-restart
- **Per-project isolation** — A panic in one project's tool handler is caught and logged; other projects continue serving
- **Agent-scoped rate limiting** — Rate limit buckets keyed by agent identity, persist across reconnections. Cannot be bypassed by reconnecting
- **Cycle detection** — Loop guard detects both repeated identical calls AND alternating patterns (A-B-A-B, A-B-C-A-B-C) to prevent agent abuse
- **Loopback-only binding** — Daemon binds to `127.0.0.1`, not `0.0.0.0`. DNS rebinding protection via Host header validation
- **Bearer token auth** — Remote connections require `Authorization: Bearer <token>` (token stored at `~/.synapses/auth_token`, mode 0600). Loopback connections are trusted without a token
- **CSRF protection** — All mutation endpoints require `X-CSRF-Token` header

### SQLite Permissions

- Cache file at `~/.cache/synapses/cache/<hash>.db`
- Created with `0600` permissions (user read/write only)
- No multi-user access to codebase data

---

## Best Practices for Users

1. **Keep Synapses updated** — Regularly update to the latest version
2. **Use proper file permissions** — Don't share codebase cache across untrusted users
3. **Protect your project root** — Synapses indexes your entire codebase; restrict access to project directories
4. **Run brain sidecar locally** — Never expose `brain` sidecar to the network (`localhost:11435` only)
5. **Run scout sidecar locally** — Never expose `scout` sidecar to the network (`localhost:11436` only)

---

## Third-Party Dependency Policy

Synapses uses **minimal external dependencies**:

- `modernc.org/sqlite` — Pure-Go SQLite (no CGo)
- `smacker/go-tree-sitter` — Tree-sitter bindings (static linking only)
- `mark3labs/mcp-go` — MCP protocol reference implementation
- `fsnotify/fsnotify` — File watching

All dependencies are vendored and regularly audited. Run `go mod audit` to check for known vulnerabilities:

```bash
go mod audit
```

---

## Security Testing

The project includes tests for:

- ✅ MCP tool input validation (no SQL injection, command injection)
- ✅ File path handling (no symlink traversal)
- ✅ Parser resource limits (bounded memory)
- ✅ Graph carving depth limits (no infinite loops)

---

## Disclosure Timeline

Once a fix is ready:

1. **Patch released** as a point release (e.g., v0.7.1)
2. **Advisory issued** to GitHub Security Advisories
3. **Public disclosure** details announced with CVE (if applicable)

---

## Contact

- **Security**: security@synapsesos.dev
- **Issues**: https://github.com/SynapsesOS/synapses/issues
- **Discussions**: https://github.com/SynapsesOS/synapses/discussions

Thank you for helping keep Synapses secure! 🔒
