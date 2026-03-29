#!/bin/sh
# Synapses installer — thin wrapper
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/SynapsesOS/synapses/main/install.sh | sh
#
# This script delegates to the canonical installer at synapsesos.com, which
# handles all installation paths:
#   - macOS/Linux GUI: downloads the desktop app (includes CLI)
#   - Linux headless:  downloads the standalone CLI binary
#
# Environment variables:
#   SYNAPSES_DOWNLOAD_URL  — override download base URL (for mirrors/corporate networks)
#   GITHUB_TOKEN           — pass to GitHub API for higher rate limits in CI
#   SYNAPSES_CLI_ONLY=1    — force CLI-only install (skip app, even with GUI)
#
# For more information: https://synapsesos.com/docs/install

set -e

CANONICAL_INSTALLER="https://synapsesos.com/install.sh"

# ── helpers ──────────────────────────────────────────────────────────────────

ok()   { printf "  \033[32m✓\033[0m %s\n" "$*"; }
info() { printf "  \033[1m→\033[0m %s\n" "$*"; }
warn() { printf "  \033[33m!\033[0m %s\n" "$*"; }
die()  { printf "\n  \033[31m✗\033[0m %s\n\n" "$*" >&2; exit 1; }

# ── detect GUI ───────────────────────────────────────────────────────────────

has_gui() {
  # macOS always has GUI (unless explicitly overridden)
  [ "$(uname -s)" = "Darwin" ] && return 0
  # Linux: check for display server
  [ -n "${DISPLAY:-}" ] || [ -n "${WAYLAND_DISPLAY:-}" ] && return 0
  return 1
}

is_headless() {
  [ "${SYNAPSES_CLI_ONLY:-}" = "1" ] && return 0
  ! has_gui
}

# ── download helper ──────────────────────────────────────────────────────────

fetch() {
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$1"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO- "$1"
  else
    die "Neither curl nor wget found — install one and retry."
  fi
}

# ── try canonical installer first ────────────────────────────────────────────

info "Fetching installer from synapsesos.com..."

INSTALLER_SCRIPT=$(fetch "$CANONICAL_INSTALLER" 2>/dev/null) || INSTALLER_SCRIPT=""

if [ -n "$INSTALLER_SCRIPT" ]; then
  # Run the canonical installer
  echo "$INSTALLER_SCRIPT" | sh
  exit $?
fi

# ── fallback: direct binary install ──────────────────────────────────────────
# If synapsesos.com is unreachable, fall back to installing CLI from GitHub.

warn "Could not reach synapsesos.com — falling back to CLI-only install from GitHub"
echo ""

GITHUB_REPO="SynapsesOS/synapses"
INSTALL_DIR="$HOME/.synapses/bin"

detect_platform() {
  OS=$(uname -s | tr '[:upper:]' '[:lower:]')
  ARCH=$(uname -m)
  case "$ARCH" in
    x86_64|amd64) ARCH="x86_64" ;;
    arm64|aarch64) ARCH="arm64" ;;
  esac
  PLATFORM="${OS}_${ARCH}"
}

detect_platform

# Get latest release tag
LATEST_URL="https://api.github.com/repos/$GITHUB_REPO/releases/latest"
AUTH_HEADER=""
if [ -n "${GITHUB_TOKEN:-}" ]; then
  AUTH_HEADER="-H \"Authorization: token $GITHUB_TOKEN\""
fi

RELEASE_JSON=$(fetch "$LATEST_URL") || die "Could not fetch latest release from GitHub — check your network connection."

ASSET_NAME="synapses_${PLATFORM}.tar.gz"
DOWNLOAD_URL=$(echo "$RELEASE_JSON" | grep -o "\"browser_download_url\"[[:space:]]*:[[:space:]]*\"[^\"]*${ASSET_NAME}\"" | head -1 | grep -o 'https://[^"]*')

if [ -z "$DOWNLOAD_URL" ]; then
  die "No pre-built binary found for $PLATFORM. Build from source: https://github.com/$GITHUB_REPO#from-source"
fi

info "Downloading synapses ($PLATFORM)..."
mkdir -p "$INSTALL_DIR"
TMPDIR_INSTALL=$(mktemp -d)
trap "rm -rf $TMPDIR_INSTALL" EXIT

fetch "$DOWNLOAD_URL" > "$TMPDIR_INSTALL/$ASSET_NAME" || die "Download failed"

# Verify checksum if available
CHECKSUM_URL=$(echo "$RELEASE_JSON" | grep -o '"browser_download_url"[[:space:]]*:[[:space:]]*"[^"]*checksums.txt"' | head -1 | grep -o 'https://[^"]*')
if [ -n "$CHECKSUM_URL" ]; then
  fetch "$CHECKSUM_URL" > "$TMPDIR_INSTALL/checksums.txt" 2>/dev/null || true
  if [ -f "$TMPDIR_INSTALL/checksums.txt" ]; then
    EXPECTED_SHA=$(grep "$ASSET_NAME" "$TMPDIR_INSTALL/checksums.txt" | awk '{print $1}')
    if [ -n "$EXPECTED_SHA" ]; then
      if command -v sha256sum >/dev/null 2>&1; then
        ACTUAL_SHA=$(sha256sum "$TMPDIR_INSTALL/$ASSET_NAME" | awk '{print $1}')
      elif command -v shasum >/dev/null 2>&1; then
        ACTUAL_SHA=$(shasum -a 256 "$TMPDIR_INSTALL/$ASSET_NAME" | awk '{print $1}')
      fi
      if [ -n "${ACTUAL_SHA:-}" ] && [ "$EXPECTED_SHA" != "$ACTUAL_SHA" ]; then
        die "Checksum verification failed! Expected: $EXPECTED_SHA, Got: $ACTUAL_SHA"
      fi
      ok "Checksum verified"
    fi
  fi
fi

tar -xzf "$TMPDIR_INSTALL/$ASSET_NAME" -C "$TMPDIR_INSTALL" 2>/dev/null || die "Failed to extract archive"

if [ -f "$TMPDIR_INSTALL/synapses" ]; then
  mv "$TMPDIR_INSTALL/synapses" "$INSTALL_DIR/synapses"
else
  die "Binary not found in archive"
fi

chmod +x "$INSTALL_DIR/synapses"
# macOS: clear quarantine and re-sign
xattr -d com.apple.quarantine "$INSTALL_DIR/synapses" 2>/dev/null || true
codesign --force --sign - "$INSTALL_DIR/synapses" 2>/dev/null || true

# Create /usr/local/bin symlink if writable
if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
  ln -sf "$INSTALL_DIR/synapses" /usr/local/bin/synapses 2>/dev/null || true
  ok "CLI symlink created at /usr/local/bin/synapses"
fi

ok "Synapses CLI installed to $INSTALL_DIR/synapses"

# Check PATH
if ! command -v synapses >/dev/null 2>&1; then
  echo ""
  warn "$INSTALL_DIR is not in your PATH."
  SHELL_NAME=$(basename "$SHELL" 2>/dev/null)
  case "$SHELL_NAME" in
    zsh)  echo "    echo 'export PATH=\"\$HOME/.synapses/bin:\$PATH\"' >> ~/.zshrc" ;;
    bash) echo "    echo 'export PATH=\"\$HOME/.synapses/bin:\$PATH\"' >> ~/.bashrc" ;;
    fish) echo "    fish_add_path ~/.synapses/bin" ;;
    *)    echo "    echo 'export PATH=\"\$HOME/.synapses/bin:\$PATH\"' >> ~/.profile" ;;
  esac
  echo ""
fi

echo ""
echo "  Next: cd /your/project && synapses init"
echo ""
