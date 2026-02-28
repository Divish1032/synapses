# Security Policy

## Reporting a Vulnerability

**Please do not report security vulnerabilities through public GitHub issues.**

Use [GitHub Security Advisories](https://github.com/Divish1032/synapses/security/advisories/new)
to report vulnerabilities privately. You can expect a response within 72 hours.

Please include:
- Description of the vulnerability and its potential impact
- Steps to reproduce
- Affected versions
- Any suggested mitigations (if known)

## Security model

Synapses is a **local-only tool** — it never sends your code or graph data to external services.

- The MCP server binds to `stdin/stdout` only (no network port by default)
- The peer federation API binds to `localhost` by default; token-authenticated
- SQLite databases are stored at `<project>/.synapses/` — local filesystem only
- The `brain` sidecar (synapses-intelligence) calls a local Ollama instance only

## Known limitations

- Peer API tokens in `synapses.json` are static strings — do not reuse them across
  untrusted networks. Federation is intended for localhost or trusted LAN use only.
- The TypeScript resolver spawns a `node` subprocess to analyse your project's code.
  Only enable `use_ts_types: true` if you trust the project's `node_modules`.
