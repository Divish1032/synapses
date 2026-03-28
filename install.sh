#!/bin/sh
# Synapses installer
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/SynapsesOS/synapses/main/install.sh | sh
#
# Installs synapses — a local-first, graph-based context manager for AI coding agents.
# No Go required if a pre-built binary is available for your platform.
#
# System Requirements:
#   macOS (arm64/x86_64), Linux (amd64/arm64)
#   From source: Go 1.22+

set -e

SYNAPSES_PKG="github.com/SynapsesOS/synapses/cmd/synapses@latest"

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
  else
    return 1
  fi

  chmod +x "$INSTALL_DIR/synapses" 2>/dev/null
  # Clear macOS quarantine and re-sign binary (cp strips ad-hoc signature)
  xattr -d com.apple.quarantine "$INSTALL_DIR/synapses" 2>/dev/null || true
  codesign --force --sign - "$INSTALL_DIR/synapses" 2>/dev/null || true
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

# ── header ────────────────────────────────────────────────────────────────────

echo ""
printf "  \033[1mSynapses installer\033[0m (code graph + MCP server)\n"
echo "  ──────────────────────────────────────"
echo ""

# ── install core ──────────────────────────────────────────────────────────────

if [ "$BINARY_INSTALLED" = true ]; then
  info "Synapses already installed via download"
  EFFECTIVE_BIN="$INSTALL_DIR"
else
  info "Installing synapses via go install..."
  go install "$SYNAPSES_PKG"
  GOBIN_DIR=$(go env GOBIN); GOPATH_DIR=$(go env GOPATH); EFFECTIVE_BIN="${GOBIN_DIR:-${GOPATH_DIR}/bin}"
  ok "synapses installed  ($(command -v synapses 2>/dev/null || echo "$EFFECTIVE_BIN/synapses"))"
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

# Resolve binary: prefer PATH, fall back to install location.
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
