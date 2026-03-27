#!/bin/sh
# Synapses installer
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/SynapsesOS/synapses/main/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/SynapsesOS/synapses/main/install.sh | sh -s -- --all
#   curl -fsSL https://raw.githubusercontent.com/SynapsesOS/synapses/main/install.sh | sh -s -- --full --with-scout
#
# Flags:
#   (default)     Install synapses core only (code graph + MCP server)
#   --core        Same as default (explicit)
#   --full        Install synapses + brain AI sidecar
#                 Requires: Ollama (https://ollama.com) + 4 GB+ RAM
#   --with-scout  Also install the web-intelligence sidecar (optional add-on)
#                 Requires: Python 3.11+ and pip
#   --with-pulse  Also install the analytics sidecar
#   --all         Install all four legs: core + brain + scout + pulse
#
# System Requirements:
#   Core only:         Go 1.22+, any OS/arch
#   With --full/--all: Go 1.22+, Ollama, 4 GB RAM minimum (8 GB recommended)
#   With scout:        Python 3.11+, pip

set -e

SYNAPSES_PKG="github.com/SynapsesOS/synapses/cmd/synapses@latest"
BRAIN_PKG="github.com/SynapsesOS/synapses-intelligence/cmd/brain@latest"
PULSE_PKG="github.com/SynapsesOS/synapses-pulse/cmd/pulse@latest"
SCOUT_PIP_PKG="synapses-scout"

TIER="core"
WITH_SCOUT=false
WITH_PULSE=false

for arg in "$@"; do
  case "$arg" in
    --all)         TIER="full"; WITH_SCOUT=true; WITH_PULSE=true ;;
    --full)        TIER="full" ;;
    --core)        TIER="core" ;;
    --with-scout)  WITH_SCOUT=true ;;
    --with-pulse)  WITH_PULSE=true ;;
  esac
done

# ── helpers ──────────────────────────────────────────────────────────────────

ok()   { printf "  \033[32m✓\033[0m %s\n" "$*"; }
info() { printf "  \033[1m→\033[0m %s\n" "$*"; }
warn() { printf "  \033[33m!\033[0m %s\n" "$*"; }
die()  { printf "\n  \033[31m✗\033[0m %s\n\n" "$*" >&2; exit 1; }

# ── platform detection ────────────────────────────────────────────────────────

detect_platform() {
  OS=$(uname -s | tr '[:upper:]' '[:lower:]')
  ARCH=$(uname -m)
  case "$ARCH" in
    x86_64|amd64) ARCH="x86_64" ;;
    arm64|aarch64) ARCH="arm64" ;;
    *) ARCH="$ARCH" ;;
  esac
  case "$OS" in
    darwin) PLATFORM="${OS}_${ARCH}" ;;
    linux)  PLATFORM="${OS}_${ARCH}" ;;
    mingw*|msys*|cygwin*) PLATFORM="windows_${ARCH}" ;;
    *) PLATFORM="" ;;
  esac
}

# ── try downloading pre-built binary ─────────────────────────────────────────

GITHUB_REPO="SynapsesOS/synapses"
INSTALL_DIR="$HOME/.synapses/bin"

try_download_binary() {
  detect_platform
  if [ -z "$PLATFORM" ]; then
    return 1
  fi

  # Get latest release tag
  LATEST_URL="https://api.github.com/repos/$GITHUB_REPO/releases/latest"
  if command -v curl >/dev/null 2>&1; then
    RELEASE_JSON=$(curl -fsSL "$LATEST_URL" 2>/dev/null) || return 1
  elif command -v wget >/dev/null 2>&1; then
    RELEASE_JSON=$(wget -qO- "$LATEST_URL" 2>/dev/null) || return 1
  else
    return 1
  fi

  # Extract download URL for our platform (e.g., synapses_darwin_arm64.tar.gz)
  ASSET_NAME="synapses_${PLATFORM}.tar.gz"
  DOWNLOAD_URL=$(echo "$RELEASE_JSON" | grep -o "\"browser_download_url\"[[:space:]]*:[[:space:]]*\"[^\"]*${ASSET_NAME}\"" | head -1 | grep -o 'https://[^"]*')

  if [ -z "$DOWNLOAD_URL" ]; then
    return 1
  fi

  info "Downloading pre-built binary ($PLATFORM)..."
  mkdir -p "$INSTALL_DIR"
  TMPDIR=$(mktemp -d)
  trap "rm -rf $TMPDIR" EXIT

  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$DOWNLOAD_URL" -o "$TMPDIR/$ASSET_NAME" || return 1
  else
    wget -q "$DOWNLOAD_URL" -O "$TMPDIR/$ASSET_NAME" || return 1
  fi

  tar -xzf "$TMPDIR/$ASSET_NAME" -C "$TMPDIR" 2>/dev/null || return 1

  # Find the binary in extracted files
  if [ -f "$TMPDIR/synapses" ]; then
    mv "$TMPDIR/synapses" "$INSTALL_DIR/synapses"
  elif [ -f "$TMPDIR/synapses.exe" ]; then
    mv "$TMPDIR/synapses.exe" "$INSTALL_DIR/synapses.exe"
  else
    return 1
  fi

  chmod +x "$INSTALL_DIR/synapses" 2>/dev/null
  ok "synapses installed to $INSTALL_DIR/synapses"
  BINARY_INSTALLED=true
  return 0
}

BINARY_INSTALLED=false

# ── preflight ────────────────────────────────────────────────────────────────

# Try pre-built binary first (no Go required)
if try_download_binary; then
  info "Installed pre-built binary (no Go required)"
elif command -v go >/dev/null 2>&1; then
  GO_VERSION=$(go version | awk '{print $3}' | sed 's/go//')
  info "No pre-built binary available; using Go $GO_VERSION"
else
  die "Could not download pre-built binary and Go is not installed.\n  Install Go from https://go.dev/dl/ or download the desktop app from https://github.com/$GITHUB_REPO/releases"
fi

if [ "$WITH_SCOUT" = true ] && ! command -v python3 >/dev/null 2>&1; then
  warn "--with-scout requires Python 3.11+. Python not found — skipping scout."
  WITH_SCOUT=false
fi

# ── header ────────────────────────────────────────────────────────────────────

echo ""
printf "  \033[1mSynapses installer\033[0m"
if [ "$TIER" = "full" ] && [ "$WITH_SCOUT" = true ] && [ "$WITH_PULSE" = true ]; then
  printf " (full stack: core + brain + scout + pulse)\n"
elif [ "$TIER" = "full" ] && [ "$WITH_SCOUT" = true ]; then
  printf " (core + brain + scout)\n"
elif [ "$TIER" = "full" ] && [ "$WITH_PULSE" = true ]; then
  printf " (core + brain + pulse)\n"
elif [ "$TIER" = "full" ]; then
  printf " (core + brain AI sidecar)\n"
else
  printf " (core: code graph + MCP server)\n"
fi
echo "  ──────────────────────────────────────"
echo ""

# ── install core ──────────────────────────────────────────────────────────────

if [ "$BINARY_INSTALLED" = true ]; then
  info "Synapses core already installed via download"
  EFFECTIVE_BIN="$INSTALL_DIR"
else
  info "Installing synapses core via go install..."
  go install "$SYNAPSES_PKG"
  GOBIN_DIR=$(go env GOBIN); GOPATH_DIR=$(go env GOPATH); EFFECTIVE_BIN="${GOBIN_DIR:-${GOPATH_DIR}/bin}"
  ok "synapses installed  ($(command -v synapses 2>/dev/null || echo "$EFFECTIVE_BIN/synapses"))"
fi

# ── install brain ─────────────────────────────────────────────────────────────

if [ "$TIER" = "full" ]; then
  echo ""
  info "Installing brain (AI enrichment sidecar)..."
  go install "$BRAIN_PKG"
  ok "brain installed  ($(command -v brain 2>/dev/null || echo '~/.go/bin/brain'))"

  echo ""
  printf "  \033[1mRunning brain setup...\033[0m\n"
  echo "  (detects your GPU/CPU, benchmarks installed Ollama models, picks the fastest)"
  echo ""

  if ! brain setup; then
    echo ""
    warn "brain setup hit an issue (see above)."
    warn "Install Ollama from https://ollama.com, then run:  brain setup"
    echo ""
  fi
fi

# ── install scout ─────────────────────────────────────────────────────────────

if [ "$WITH_SCOUT" = true ]; then
  echo ""
  info "Installing scout (web intelligence sidecar)..."
  if command -v pip3 >/dev/null 2>&1; then
    pip3 install "$SCOUT_PIP_PKG" --quiet
    ok "scout installed  ($(command -v scout 2>/dev/null || echo '~/.local/bin/scout'))"
  elif command -v pip >/dev/null 2>&1; then
    pip install "$SCOUT_PIP_PKG" --quiet
    ok "scout installed  ($(command -v scout 2>/dev/null || echo '~/.local/bin/scout'))"
  else
    warn "pip not found — skipping scout. Install pip then run:  pip install synapses-scout"
  fi
fi

# ── install pulse ─────────────────────────────────────────────────────────────

if [ "$WITH_PULSE" = true ]; then
  echo ""
  info "Installing pulse (analytics sidecar)..."
  go install "$PULSE_PKG"
  ok "pulse installed  ($(command -v pulse 2>/dev/null || echo '~/.go/bin/pulse'))"
fi

# ── path verification ────────────────────────────────────────────────────────

if command -v synapses >/dev/null 2>&1; then
  ok "synapses is in PATH — ready to use"
else
  # If we downloaded to ~/.synapses/bin, that's our effective bin
  if [ "$BINARY_INSTALLED" = true ]; then
    EFFECTIVE_BIN="$INSTALL_DIR"
  elif command -v go >/dev/null 2>&1; then
    GOBIN_DIR=$(go env GOBIN)
    GOPATH_DIR=$(go env GOPATH)
    EFFECTIVE_BIN="${GOBIN_DIR:-${GOPATH_DIR}/bin}"
  fi

  echo ""
  warn "$EFFECTIVE_BIN is not in your PATH."
  echo ""
  echo "       Activate now:"
  echo "         export PATH=\"$EFFECTIVE_BIN:\$PATH\""
  echo ""
  echo "       Persist for future sessions:"

  SHELL_NAME=$(basename "$SHELL" 2>/dev/null)
  case "$SHELL_NAME" in
    zsh)  echo "         echo 'export PATH=\"$EFFECTIVE_BIN:\$PATH\"' >> ~/.zshrc" ;;
    bash)
      if [ -f "$HOME/.bash_profile" ]; then
        echo "         echo 'export PATH=\"$EFFECTIVE_BIN:\$PATH\"' >> ~/.bash_profile"
      else
        echo "         echo 'export PATH=\"$EFFECTIVE_BIN:\$PATH\"' >> ~/.bashrc"
      fi ;;
    fish) echo "         fish_add_path $EFFECTIVE_BIN" ;;
    *)    echo "         echo 'export PATH=\"$EFFECTIVE_BIN:\$PATH\"' >> ~/.profile" ;;
  esac
  echo ""
fi

# ── start daemon ─────────────────────────────────────────────────────────────

echo ""
echo "  ──────────────────────────────────────"

# Resolve binary: prefer PATH, fall back to GOBIN location from earlier.
SYNAPSES_BIN=$(command -v synapses 2>/dev/null || echo "${EFFECTIVE_BIN}/synapses")

if [ -x "$SYNAPSES_BIN" ]; then
  info "Starting daemon..."
  "$SYNAPSES_BIN" daemon start || warn "Daemon start failed — run 'synapses daemon start' later."
else
  warn "Skipping daemon start — synapses not in PATH yet."
  warn "After updating PATH (see above), run: synapses daemon start"
fi

# ── next steps ────────────────────────────────────────────────────────────────

echo "  ──────────────────────────────────────"
printf "  \033[1mDone! Next step:\033[0m\n"
echo ""
echo "    cd /your/project"
echo "    synapses init"
echo ""
echo "  This indexes your project, starts the daemon, and"
echo "  connects your AI agents — all in one command."
echo ""
echo "  Optional — start daemon at login:"
echo "       synapses daemon install"
echo ""
echo "  Useful commands:"
echo "    synapses daemon status        — see what's running"
echo "    synapses doctor               — full health check"
echo ""
