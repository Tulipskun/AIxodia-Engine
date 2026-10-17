#!/usr/bin/env bash
# aixodia installer — one line:
#
#   curl -fsSL https://raw.githubusercontent.com/Tulipskun/ai-engine/main/install.sh | sudo bash
#
# Installs to /usr/local/bin:
#   ai-engine   the engine binary
#   aixodia     the launcher (start / stop / status / logs)
#   cloudflared the tunnel helper, if it is not already installed
#
# Updating is the same command again: it replaces the files and stops there.
# Restart a running engine afterwards (aixodia stop; aixodia) to pick the new
# build up.
set -euo pipefail

REPO_SLUG="${AIXODIA_REPO_SLUG:-Tulipskun/ai-engine}"
REF="${AIXODIA_REF:-main}"
RAW_BASE="${AIXODIA_RAW_BASE:-https://raw.githubusercontent.com/$REPO_SLUG/$REF}"
ENGINE_URL="${AIXODIA_ENGINE_URL:-https://github.com/$REPO_SLUG/releases/download/engine-latest/ai-engine}"
PREFIX="${AIXODIA_PREFIX:-/usr/local/bin}"

log() { printf '==> %s\n' "$*"; }
die() { printf 'install: %s\n' "$*" >&2; exit 1; }

command -v curl >/dev/null 2>&1 || die "curl is required"

# Writing to /usr/local/bin needs root; a writable prefix (tests, custom
# installs) does not.
if [ "$(id -u)" -ne 0 ] && [ ! -w "$PREFIX" ] && [ ! -w "$(dirname "$PREFIX")" ]; then
  die "needs root to write $PREFIX — run it as: curl -fsSL <url> | sudo bash"
fi

os="$(uname -s)"
arch="$(uname -m)"
PLATFORM="${AIXODIA_PLATFORM:-}"
if [ -z "$PLATFORM" ]; then
  case "$os/$arch" in
    Linux/x86_64) PLATFORM=linux-amd64 ;;
    *) die "unsupported platform: $os/$arch (only Linux/x86_64 builds are published)" ;;
  esac
fi

CLOUDFLARED_URL="${AIXODIA_CLOUDFLARED_URL:-https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-$PLATFORM}"

mkdir -p "$PREFIX"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# fetch <url> <dest> — downloads to a temp name and moves it into place, so a
# binary that is currently running can still be replaced.
fetch() {
  local url="$1" dest="$2"
  log "download $(basename "$dest")"
  curl -fsSL --retry 3 --retry-delay 2 -o "$TMP/download" "$url" \
    || die "cannot download $url"
  chmod +x "$TMP/download"
  mv -f "$TMP/download" "$dest"
}

fetch "$ENGINE_URL" "$PREFIX/ai-engine"
fetch "$RAW_BASE/aixodia" "$PREFIX/aixodia"

if command -v cloudflared >/dev/null 2>&1; then
  log "cloudflared already installed, keeping it"
else
  fetch "$CLOUDFLARED_URL" "$PREFIX/cloudflared"
fi

log "installed: $PREFIX/{ai-engine, aixodia}$(command -v cloudflared >/dev/null 2>&1 && echo "" || echo ", cloudflared")"
echo
echo "Start it with:"
echo "  export CF_TOKEN=xxxxx   # the engine publishes its tunnel url to D1 with this"
echo "  aixodia"
