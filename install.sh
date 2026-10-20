#!/usr/bin/env bash
# aixodia installer — one line:
#
#   curl -fsSL https://raw.githubusercontent.com/Tulipskun/aixodia-engine/main/install.sh | sudo bash
#
# Installs to /usr/local/bin:
#   aixodia-engine  the engine binary (it dials the relay; no inbound port)
#   aixodia         the launcher (start / stop / status / logs)
#   (kgat_sync.py lives in the kaggle repo, not here)
#
# Updating is the same command again: it replaces the files and stops there.
# Restart a running engine afterwards (aixodia stop; aixodia) to pick the new
# build up.
set -euo pipefail

REPO_SLUG="${AIXODIA_REPO_SLUG:-Tulipskun/aixodia-engine}"
REF="${AIXODIA_REF:-main}"
RAW_BASE="${AIXODIA_RAW_BASE:-https://raw.githubusercontent.com/$REPO_SLUG/$REF}"
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
    Linux/aarch64 | Linux/arm64) PLATFORM=linux-arm64 ;;
    *) die "unsupported platform: $os/$arch (only Linux x86_64 and arm64 builds are published)" ;;
  esac
fi

ENGINE_URL="${AIXODIA_ENGINE_URL:-https://github.com/$REPO_SLUG/releases/download/engine-latest/aixodia-engine-$PLATFORM}"

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

fetch "$ENGINE_URL" "$PREFIX/aixodia-engine"
fetch "$RAW_BASE/aixodia" "$PREFIX/aixodia"

log "installed: $PREFIX/{aixodia-engine, aixodia}"
echo
echo "Start it with:"
echo "  export CF_TOKEN=xxxxx   # the engine joins the relay and publishes its room with this"
echo "  aixodia"
