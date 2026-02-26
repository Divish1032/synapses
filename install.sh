#!/bin/sh
# Synapses installer
# Usage: curl -fsSL https://raw.githubusercontent.com/synapses/synapses/main/install.sh | sh

set -e

REPO="synapses/synapses"
BINARY="synapses"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

# Detect OS
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$OS" in
  linux)  OS="linux" ;;
  darwin) OS="darwin" ;;
  *)
    echo "Unsupported OS: $OS"
    echo "Please download manually from https://github.com/${REPO}/releases"
    exit 1
    ;;
esac

# Detect architecture
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)          ARCH="x86_64" ;;
  amd64)           ARCH="x86_64" ;;
  arm64|aarch64)   ARCH="arm64" ;;
  *)
    echo "Unsupported architecture: $ARCH"
    echo "Please download manually from https://github.com/${REPO}/releases"
    exit 1
    ;;
esac

# Fetch latest version tag
echo "Fetching latest release..."
VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
  | grep '"tag_name"' \
  | sed -E 's/.*"([^"]+)".*/\1/')

if [ -z "$VERSION" ]; then
  echo "Could not determine latest version. Check your internet connection."
  exit 1
fi

echo "Installing ${BINARY} ${VERSION} (${OS}/${ARCH})..."

# Build download URL
ARCHIVE="${BINARY}_${OS}_${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/download/${VERSION}/${ARCHIVE}"

# Download to temp dir
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

curl -fsSL "$URL" -o "${TMP}/${ARCHIVE}"

# Extract
tar -xzf "${TMP}/${ARCHIVE}" -C "$TMP"

# Install
if [ ! -w "$INSTALL_DIR" ]; then
  echo "Installing to ${INSTALL_DIR} (requires sudo)..."
  sudo install -m 755 "${TMP}/${BINARY}" "${INSTALL_DIR}/${BINARY}"
else
  install -m 755 "${TMP}/${BINARY}" "${INSTALL_DIR}/${BINARY}"
fi

echo ""
echo "synapses ${VERSION} installed to ${INSTALL_DIR}/${BINARY}"
echo ""
echo "Quick start:"
echo "  cd /your/project"
echo "  synapses init"
