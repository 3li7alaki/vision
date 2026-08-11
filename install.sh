#!/bin/sh
# vision installer: slim, no runtime deps, no root, idempotent (also acts as updater).
#   curl -fsSL https://raw.githubusercontent.com/3li7alaki/vision/main/install.sh | sh
#   curl -fsSL .../install.sh | sh -s -- --uninstall
#
# vision is a single self-contained Go binary, distributed as a rolling `latest` release
# (no version numbers: every push to main republishes the current build). This script
# downloads the right one for your platform, stores it under XDG data, and symlinks it
# onto your PATH at ~/.local/bin/vision. Re-run it any time to update in place.
set -eu

TOOL="vision"
REPO="3li7alaki/vision"
RELEASE="latest"          # rolling tag; there is no semver
INSTALL_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/$TOOL"
BIN_DIR="$INSTALL_DIR/bin"
LINK_DIR="$HOME/.local/bin"

NO_PATH=false
UNINSTALL=false
for arg in "$@"; do
  case "$arg" in
    --no-path)   NO_PATH=true ;;
    --uninstall) UNINSTALL=true ;;
    *) echo "unknown flag: $arg" >&2; exit 1 ;;
  esac
done

say()  { printf '%s\n' "$*"; }
err()  { printf 'error: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || err "missing required tool: $1"; }

if [ "$UNINSTALL" = true ]; then
  "$BIN_DIR/$TOOL" off >/dev/null 2>&1 || true
  rm -rf "$INSTALL_DIR"
  rm -f "$LINK_DIR/$TOOL"
  say "uninstalled $TOOL (your captures under XDG data are left alone)"
  exit 0
fi

need curl
need uname

# Platform to Go's GOOS-GOARCH naming (matches the release asset names).
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$OS" in
  linux|darwin) ;;
  *) err "unsupported OS: $OS (vision ships linux + darwin)" ;;
esac
ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64)  ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) err "unsupported architecture: $ARCH (vision ships amd64 + arm64)" ;;
esac
TARGET="${OS}-${ARCH}"

# Rolling release: the asset lives under the fixed `latest` tag, so no API call is needed.
URL="https://github.com/$REPO/releases/download/$RELEASE/${TOOL}-${TARGET}"
say "installing $TOOL ($TARGET) ..."

# Download to a temp file, then atomically move into place (a failed download never
# leaves a half-written binary on PATH).
mkdir -p "$BIN_DIR"
TMP=$(mktemp "$BIN_DIR/.$TOOL.XXXXXX")
trap 'rm -f "$TMP"' EXIT INT TERM
curl -fSL --progress-bar "$URL" -o "$TMP" || err "download failed: $URL"
chmod +x "$TMP"
mv -f "$TMP" "$BIN_DIR/$TOOL"
trap - EXIT INT TERM

# Symlink onto PATH (no root, no copy: the link tracks updates in place).
mkdir -p "$LINK_DIR"
ln -sf "$BIN_DIR/$TOOL" "$LINK_DIR/$TOOL"

# Add ~/.local/bin to PATH once, guarded against duplicates.
if [ "$NO_PATH" != true ]; then
  LINE="export PATH=\"$LINK_DIR:\$PATH\""
  for RC in "$HOME/.bashrc" "$HOME/.zshrc" "$HOME/.profile"; do
    [ -f "$RC" ] || continue
    grep -qF "$LINK_DIR" "$RC" && continue
    printf '\n# %s\n%s\n' "$TOOL" "$LINE" >> "$RC"
  done
fi

# Verify against the binary we just installed (not a stale PATH entry).
INSTALLED=$("$BIN_DIR/$TOOL" --version 2>/dev/null || echo "installed")
if command -v "$TOOL" >/dev/null 2>&1; then
  say "✓ $TOOL $INSTALLED installed. Start the daemon with: vision on"
else
  say "✓ $TOOL $INSTALLED installed to $BIN_DIR/$TOOL"
  say "  open a new shell, or run: export PATH=\"$LINK_DIR:\$PATH\""
fi

command -v pinchtab >/dev/null 2>&1 || \
  say "  note: pinchtab is not on your PATH, and vision needs it to capture. See https://pinchtab.com"
