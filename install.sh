#!/bin/sh
# Synapses installer
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/Divish1032/synapses/main/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/Divish1032/synapses/main/install.sh | sh -s -- --full
#
# Flags:
#   (default)  Install synapses only (Tier 1 — code graph + MCP)
#   --full     Install synapses + brain sidecar, run brain setup (Tier 2)
#   --core     Same as default (explicit)

set -e

SYNAPSES_PKG="github.com/Divish1032/synapses/cmd/synapses@latest"
BRAIN_PKG="github.com/Divish1032/synapses-intelligence/cmd/brain@latest"

TIER="core"
for arg in "$@"; do
  case "$arg" in
    --full) TIER="full" ;;
    --core) TIER="core" ;;
  esac
done

# ── helpers ──────────────────────────────────────────────────────────────────

ok()   { printf "  \033[32m✓\033[0m %s\n" "$*"; }
info() { printf "  \033[1m→\033[0m %s\n" "$*"; }
die()  { printf "\n  \033[31m✗\033[0m %s\n\n" "$*" >&2; exit 1; }

# ── preflight ────────────────────────────────────────────────────────────────

if ! command -v go >/dev/null 2>&1; then
  die "Go is not installed. Install it from https://go.dev/dl/ then re-run this script."
fi

GO_VERSION=$(go version | awk '{print $3}' | sed 's/go//')
info "Using Go $GO_VERSION"

# ── main ──────────────────────────────────────────────────────────────────────

echo ""
printf "  \033[1mSynapses installer\033[0m"
if [ "$TIER" = "full" ]; then
  printf " (Tier 2 — full stack)\n"
else
  printf " (Tier 1 — core)\n"
fi
echo "  ──────────────────────────────────────"
echo ""

# Always install synapses
info "Installing synapses..."
go install "$SYNAPSES_PKG"
ok "synapses installed  ($(which synapses 2>/dev/null || echo '~/.go/bin/synapses'))"

if [ "$TIER" = "full" ]; then
  echo ""
  info "Installing brain..."
  go install "$BRAIN_PKG"
  ok "brain installed  ($(which brain 2>/dev/null || echo '~/.go/bin/brain'))"

  echo ""
  printf "  \033[1mRunning brain setup...\033[0m\n"
  echo "  (detects your RAM, picks the right model, pulls it)"
  echo ""

  # brain setup is interactive; exit code non-zero means Ollama missing
  if ! brain setup; then
    echo ""
    printf "  \033[33m!\033[0m brain setup hit an issue (see above).\n"
    printf "  \033[33m!\033[0m Fix it, then run:  brain setup\n"
    echo ""
  fi
fi

# ── path hint ────────────────────────────────────────────────────────────────

GOBIN=$(go env GOBIN)
GOPATH=$(go env GOPATH)
EFFECTIVE_BIN="${GOBIN:-${GOPATH}/bin}"

if ! echo "$PATH" | grep -q "$EFFECTIVE_BIN"; then
  echo ""
  printf "  \033[33m!\033[0m $EFFECTIVE_BIN is not in your PATH.\n"
  printf "  \033[33m!\033[0m Add this to ~/.zshrc or ~/.profile:\n"
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
  echo "  1. Start brain in the background:"
  echo "       brain serve &"
  echo "     (add to ~/.zshrc or ~/.profile to start automatically)"
  echo ""
  echo "  2. Wire synapses into Claude Code (run in your project):"
  echo "       claude mcp add synapses -- synapses start --path ."
  echo ""
  echo "  To change model:  brain config model qwen3:4b --pull"
else
  echo "  Wire synapses into Claude Code (run in your project):"
  echo "       claude mcp add synapses -- synapses start --path ."
  echo ""
  echo "  To add the AI brain later:"
  echo "       curl -fsSL https://raw.githubusercontent.com/Divish1032/synapses/main/install.sh | sh -s -- --full"
fi

echo ""
