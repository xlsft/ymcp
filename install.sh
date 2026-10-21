#!/bin/sh
# ymcp installer for macOS and Linux.
#
#   curl -fsSL https://raw.githubusercontent.com/xlsft/ymcp/main/install.sh | sh
#
# Environment:
#   YMCP_VERSION      release tag to install (default: latest), e.g. v0.1.0
#   YMCP_INSTALL_DIR  where to put the binary (default: ~/.local/bin)
#   YMCP_NO_SETUP=1   skip `ymcp setup` (credentials and MCP clients)
#   YMCP_DOWNLOAD_URL base URL to download release assets from (mirrors, testing)
set -eu

REPO="xlsft/ymcp"
INSTALL_DIR="${YMCP_INSTALL_DIR:-$HOME/.local/bin}"
VERSION="${YMCP_VERSION:-latest}"

if [ -t 1 ]; then
  RED="$(printf '\033[1;97;41m')"; BOLD="$(printf '\033[1m')"; DIM="$(printf '\033[2m')"
  GREEN="$(printf '\033[1;32m')"; YELLOW="$(printf '\033[1;33m')"; RESET="$(printf '\033[0m')"
else
  RED=""; BOLD=""; DIM=""; GREEN=""; YELLOW=""; RESET=""
fi

say()  { printf '%s\n' "$*"; }
ok()   { say "${GREEN}✓${RESET} $*"; }
warn() { say "${YELLOW}!${RESET} $*"; }
die()  { say "${YELLOW}✗${RESET} $*" >&2; exit 1; }

say ""
say "${RED} ymcp ${RESET} ${BOLD}YouTrack MCP installer${RESET}"
say "${DIM}It's fun to fetch with the YMCP${RESET}"
say ""

case "$(uname -s)" in
  Darwin) OS=darwin ;;
  Linux)  OS=linux ;;
  *) die "Unsupported OS $(uname -s). On Windows run: irm https://raw.githubusercontent.com/$REPO/main/install.ps1 | iex" ;;
esac
case "$(uname -m)" in
  x86_64|amd64)  ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) die "Unsupported architecture $(uname -m)" ;;
esac

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL --retry 3 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q -O "$2" "$1"; }
else
  die "curl or wget is required"
fi

if [ -n "${YMCP_DOWNLOAD_URL:-}" ]; then
  BASE="$YMCP_DOWNLOAD_URL"
elif [ "$VERSION" = "latest" ]; then
  BASE="https://github.com/$REPO/releases/latest/download"
else
  BASE="https://github.com/$REPO/releases/download/$VERSION"
fi
ARCHIVE="ymcp_${OS}_${ARCH}.tar.gz"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT INT TERM

say "${DIM}Downloading $ARCHIVE ($VERSION)…${RESET}"
fetch "$BASE/$ARCHIVE" "$TMP/$ARCHIVE" || die "Download failed: $BASE/$ARCHIVE"
fetch "$BASE/checksums.txt" "$TMP/checksums.txt" || die "Download failed: $BASE/checksums.txt"

EXPECTED="$(awk -v f="$ARCHIVE" '$2 == f { print $1 }' "$TMP/checksums.txt")"
[ -n "$EXPECTED" ] || die "No checksum for $ARCHIVE"
if command -v sha256sum >/dev/null 2>&1; then
  ACTUAL="$(sha256sum "$TMP/$ARCHIVE" | awk '{ print $1 }')"
else
  ACTUAL="$(shasum -a 256 "$TMP/$ARCHIVE" | awk '{ print $1 }')"
fi
[ "$EXPECTED" = "$ACTUAL" ] || die "Checksum mismatch for $ARCHIVE"
ok "Checksum verified"

tar -xzf "$TMP/$ARCHIVE" -C "$TMP" ymcp
mkdir -p "$INSTALL_DIR"
install -m 755 "$TMP/ymcp" "$INSTALL_DIR/ymcp"
ok "Installed $("$INSTALL_DIR/ymcp" version) to ${BOLD}$INSTALL_DIR/ymcp${RESET}"

case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *)
    case "${SHELL:-}" in
      */zsh)  RC="~/.zshrc" ;;
      */bash) RC="~/.bashrc" ;;
      */fish) RC="~/.config/fish/config.fish" ;;
      *)      RC="your shell profile" ;;
    esac
    warn "$INSTALL_DIR is not on your PATH. Add it to $RC:"
    if [ "$RC" = "~/.config/fish/config.fish" ]; then
      say "    fish_add_path $INSTALL_DIR"
    else
      say "    export PATH=\"$INSTALL_DIR:\$PATH\""
    fi
    ;;
esac

if [ "${YMCP_NO_SETUP:-}" = "1" ]; then
  say ""
  say "Next: run ${BOLD}ymcp setup${RESET} to sign in and register ymcp in your MCP clients."
  exit 0
fi
if [ -r /dev/tty ] && [ -w /dev/tty ]; then
  "$INSTALL_DIR/ymcp" setup </dev/tty >/dev/tty 2>&1
else
  say ""
  say "Next: run ${BOLD}ymcp setup${RESET} to sign in and register ymcp in your MCP clients."
fi
