# Synapses — User Onboarding Plan

Goal: developer goes from zero to getting meaningful AI agent context in under 5 minutes,
with zero manual config editing.

---

## Two tiers

| Tier | Description | Time |
|------|-------------|------|
| **Core** | Code graph + MCP only. No local LLM required. | ~1 min |
| **Full** | Core + intelligence sidecar for AI-enriched context packets. | ~4 min (mostly model download) |

Users should start at Core and optionally upgrade to Full.

---

## Tier 1 — Core (1 minute)

### Step 1: Install the binary

```bash
# macOS / Linux (Go 1.22+)
go install github.com/Divish1032/synapses/cmd/synapses@latest

# or via Homebrew (future)
brew install synapses/tap/synapses

# or via install script (future)
curl -fsSL https://synapses.dev/install.sh | sh
```

### Step 2: Wire into Claude Code

```bash
cd /your/project
claude mcp add synapses -- synapses start --path .
```

That's it. Reload Claude Code. Synapses indexes your project on first start (~2s for most
codebases) and serves 34 MCP tools to the agent.

**No config file needed.** No `synapses.json` required for basic use.

---

## Tier 2 — Full (add the AI brain)

### Step 3: Install the intelligence sidecar

```bash
go install github.com/Divish1032/synapses-intelligence/cmd/brain@latest

# or via Homebrew (future)
brew install synapses/tap/synapses-intelligence
```

### Step 4: Run setup (auto-detects RAM, picks model, pulls it)

```bash
brain setup
```

Output example:
```
synapses-intelligence setup
────────────────────────────
  System RAM:  16 GB
  Recommended: qwen3:8b  (~5GB)

  All tiers:
      qwen2.5-coder:1.5b  ~900MB   default, runs on any machine
      qwen3:1.7b          ~1.1GB   recommended — thinking mode
      qwen3:4b            ~2.5GB   power user
    → qwen3:8b            ~5GB     enterprise

  ✓ Ollama installed
  ✓ Config saved to ~/.synapses/brain.json
  Pulling qwen3:8b...
    pulling manifest                          100%
    pulling model                             100%

  ✓ Model ready

────────────────────────────
Setup complete. Next steps:

  1. Start the brain sidecar:
       brain serve

  2. Add brain URL to your project's synapses.json:
       { "brain": { "url": "http://localhost:11435", "enable_llm": true } }

  3. (Re)start synapses:
       synapses start --path .
```

### Step 5: Start the brain and add one line to synapses.json

```bash
brain serve &   # or add to your shell startup / launchd / systemd
```

Create (or add to) `synapses.json` in your project root:
```json
{
  "brain": {
    "url": "http://localhost:11435",
    "enable_llm": true
  }
}
```

Restart synapses (Claude Code will pick it up on reconnect).

---

## Model management

```bash
# Change model (writes to ~/.synapses/brain.json and pulls immediately)
brain config model qwen3:4b --pull

# Just update config, pull happens automatically on next brain serve
brain config model qwen3:1.7b

# Show current config
brain config

# Check status
brain status
```

---

## Daily workflow

```
brain serve                    # (once, keep running in background)
synapses start --path .        # (via Claude Code MCP — auto-starts)
```

Agents use `get_context`, `find_entity`, `get_call_chain` etc. as normal.
With brain running, `get_context` responses include a **Context Packet** — a
600-800 token semantic summary of what the agent needs to know about the entity,
its architectural role, active SDLC phase constraints, and any active work claims.

---

## Friction audit

| Pain point | Solution |
|------------|----------|
| User must know Go install path | Pre-built binaries / install script / Homebrew |
| User must edit JSON manually | `brain setup` writes config; synapses has sane defaults |
| Ollama not installed | `brain setup` detects and prints OS-specific install command |
| Model not pulled → silent failure | `brain serve` auto-pulls on startup; `brain setup` pulls upfront |
| Wrong model for RAM | `brain setup` detects RAM and recommends the right tier |
| MCP config in Claude Code | Single `claude mcp add` command |
| Brain URL in synapses.json | One-line JSON; `brain setup` prints the exact snippet |
| Multiple terminals needed | `brain serve &` backgrounds it; can add to shell `.profile` |

---

## Future improvements (not yet built)

- `synapses setup` — single command that does both tiers (installs brain, runs setup,
  writes synapses.json, prints `claude mcp add` command)
- Homebrew tap with formula for both binaries
- Shell completion (`synapses <tab>`, `brain <tab>`)
- Auto-start brain as a launchd/systemd service via `brain install-service`
- VS Code extension that bundles the MCP wiring and shows a "Setup" UI panel
- `synapses doctor` — checks binary versions, DB health, brain connectivity, MCP config
