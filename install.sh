#!/bin/sh
# Synapses installer
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/SynapsesOS/synapses/main/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/SynapsesOS/synapses/main/install.sh | sh -s -- --full
#   curl -fsSL https://raw.githubusercontent.com/SynapsesOS/synapses/main/install.sh | sh -s -- --full --with-scout
#
# Flags:
#   (default)     Install synapses core only (code graph + MCP server)
#   --core        Same as default (explicit)
#   --full        Install synapses + brain AI sidecar
#                 Requires: Ollama (https://ollama.com) + 4 GB+ RAM
#   --with-scout  Also install the web-intelligence sidecar (optional add-on)
#                 Requires: Python 3.11+ and pip
#
# System Requirements:
#   Core only:       Go 1.22+, any OS/arch
#   With --full:     Go 1.22+, Ollama, 4 GB RAM minimum (8 GB recommended)
#   With --with-scout: Python 3.11+, pip

set -e

SYNAPSES_PKG="github.com/SynapsesOS/synapses/cmd/synapses@latest"
BRAIN_PKG="github.com/SynapsesOS/synapses-intelligence/cmd/brain@latest"
SCOUT_PIP_PKG="synapses-scout"

TIER="core"
WITH_SCOUT=false
for arg in "$@"; do
  case "$arg" in
    --full)        TIER="full" ;;
    --core)        TIER="core" ;;
    --with-scout)  WITH_SCOUT=true ;;
  esac
done

# ── helpers ──────────────────────────────────────────────────────────────────

ok()   { printf "  \033[32m✓\033[0m %s\n" "$*"; }
info() { printf "  \033[1m→\033[0m %s\n" "$*"; }
warn() { printf "  \033[33m!\033[0m %s\n" "$*"; }
die()  { printf "\n  \033[31m✗\033[0m %s\n\n" "$*" >&2; exit 1; }

# ── preflight ────────────────────────────────────────────────────────────────

if ! command -v go >/dev/null 2>&1; then
  die "Go is not installed. Install it from https://go.dev/dl/ then re-run this script."
fi

GO_VERSION=$(go version | awk '{print $3}' | sed 's/go//')
info "Using Go $GO_VERSION"

if [ "$WITH_SCOUT" = true ] && ! command -v python3 >/dev/null 2>&1; then
  warn "--with-scout requires Python 3.11+. Python not found — skipping scout."
  WITH_SCOUT=false
fi

# ── main ──────────────────────────────────────────────────────────────────────

echo ""
printf "  \033[1mSynapses installer\033[0m"
if [ "$TIER" = "full" ] && [ "$WITH_SCOUT" = true ]; then
  printf " (full stack: core + brain + scout)\n"
elif [ "$TIER" = "full" ]; then
  printf " (core + brain AI sidecar)\n"
else
  printf " (core: code graph + MCP server)\n"
fi
echo "  ──────────────────────────────────────"
echo ""

# Always install synapses core
info "Installing synapses..."
go install "$SYNAPSES_PKG"
ok "synapses installed  ($(which synapses 2>/dev/null || echo '~/.go/bin/synapses'))"

if [ "$TIER" = "full" ]; then
  echo ""
  info "Installing brain (AI sidecar)..."
  go install "$BRAIN_PKG"
  ok "brain installed  ($(which brain 2>/dev/null || echo '~/.go/bin/brain'))"

  echo ""
  printf "  \033[1mRunning brain setup...\033[0m\n"
  echo "  (detects your GPU/CPU, benchmarks installed Ollama models, picks the fastest)"
  echo ""

  # brain setup is interactive; exit code non-zero means Ollama missing or no models
  if ! brain setup; then
    echo ""
    warn "brain setup hit an issue (see above)."
    warn "Install Ollama from https://ollama.com, then run:  brain setup"
    echo ""
  fi
fi

if [ "$WITH_SCOUT" = true ]; then
  echo ""
  info "Installing scout (web intelligence sidecar)..."
  if command -v pip3 >/dev/null 2>&1; then
    pip3 install "$SCOUT_PIP_PKG" --quiet
    ok "scout installed  ($(which scout 2>/dev/null || echo '~/.local/bin/scout'))"
  elif command -v pip >/dev/null 2>&1; then
    pip install "$SCOUT_PIP_PKG" --quiet
    ok "scout installed  ($(which scout 2>/dev/null || echo '~/.local/bin/scout'))"
  else
    warn "pip not found — skipping scout. Install pip then run:  pip install synapses-scout"
  fi
fi

# ── path hint ────────────────────────────────────────────────────────────────

GOBIN=$(go env GOBIN)
GOPATH=$(go env GOPATH)
EFFECTIVE_BIN="${GOBIN:-${GOPATH}/bin}"

if ! echo "$PATH" | grep -q "$EFFECTIVE_BIN"; then
  echo ""
  warn "$EFFECTIVE_BIN is not in your PATH."
  warn "Add this to ~/.zshrc or ~/.profile:"
  echo ""
  echo "       export PATH=\"\$PATH:$EFFECTIVE_BIN\""
  echo ""
fi

# ── next steps ────────────────────────────────────────────────────────────────

echo ""
echo "  ──────────────────────────────────────"
printf "  \033[1mDone. Next steps:\033[0m\n"
echo ""

if [ "$TIER" = "full" ]; then
  echo "  1. Start the brain sidecar in the background:"
  echo "       brain serve &"
  echo "     (add to ~/.zshrc or ~/.profile to start automatically)"
  echo ""
  if [ "$WITH_SCOUT" = true ]; then
    echo "  2. Start the scout sidecar in the background:"
    echo "       scout serve &"
    echo ""
    echo "  3. Wire synapses into your AI agent (Cursor, Gemini, Zed, Windsurf, Claude):"
    echo "       synapses mcp-setup --agent all"
  else
    echo "  2. Wire synapses into your AI agent (Cursor, Gemini, Zed, Windsurf, Claude):"
    echo "       synapses mcp-setup --agent all"
    echo ""
    echo "  To add web search/fetch later:"
    echo "       pip install synapses-scout && scout serve &"
  fi
else
  echo "  Wire synapses into your AI agent (Cursor, Gemini, Zed, Windsurf, Claude):"
  echo "       synapses mcp-setup --agent all"
  echo ""
  echo "  To add the AI brain (local LLM enrichment):"
  echo "       curl -fsSL https://raw.githubusercontent.com/SynapsesOS/synapses/main/install.sh | sh -s -- --full"
fi

echo ""
