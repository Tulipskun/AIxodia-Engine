#!/usr/bin/env bash
# Tests for install.sh. Run: bash install_test.sh
# Downloads are served from file:// fixtures, so nothing touches the network
# and nothing is written outside a temp prefix.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
fails=0
pass() { echo "ok   - $1"; }
fail() { echo "FAIL - $1"; fails=$((fails + 1)); }

srv=$(mktemp -d)
printf '#!/usr/bin/env bash\necho engine\n' > "$srv/aixodia-engine"
printf '#!/usr/bin/env bash\necho launcher\n' > "$srv/aixodia"
mkdir -p "$srv/kaggle"
chmod +x "$srv"/* "$srv/kaggle"/*
missing="file://$srv/does-not-exist"

# --- fresh install drops both binaries in the prefix -------------------------
prefix=$(mktemp -d)
out=$(PATH=/usr/bin:/bin \
      AIXODIA_PREFIX="$prefix" AIXODIA_PLATFORM=linux-amd64 \
      AIXODIA_ENGINE_URL="file://$srv/aixodia-engine" \
      AIXODIA_RAW_BASE="file://$srv" \
      bash "$HERE/install.sh" 2>&1); rc=$?
if [ "$rc" -eq 0 ]; then
  pass "install exits 0"
else
  fail "install rc=$rc ($out)"
fi
for f in aixodia-engine aixodia; do
  if [ -x "$prefix/$f" ]; then
    pass "installed $f (executable)"
  else
    fail "missing or not executable: $prefix/$f"
  fi
done

# --- a failed download fails the install ------------------------------------
prefix=$(mktemp -d)
out=$(AIXODIA_PREFIX="$prefix" AIXODIA_PLATFORM=linux-amd64 \
      AIXODIA_ENGINE_URL="$missing" \
      AIXODIA_RAW_BASE="file://$srv" \
      bash "$HERE/install.sh" 2>&1); rc=$?
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -qi "cannot download"; then
  pass "a failed download fails loudly"
else
  fail "failed download (rc=$rc, out=$out)"
fi

# --- downloads match the host architecture ----------------------------------
record=$(mktemp)
fakecurl=$(mktemp -d)
cat > "$fakecurl/curl" <<'FAKE'
#!/usr/bin/env bash
# stand-in curl: records the requested url and writes a dummy file
out=""; url=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -*) shift ;;
    *) url="$1"; shift ;;
  esac
done
printf '%s\n' "$url" >> "$RECORD"
printf 'x\n' > "$out"
FAKE
chmod +x "$fakecurl/curl"
want=amd64
case "$(uname -m)" in aarch64|arm64) want=arm64 ;; esac
prefix=$(mktemp -d)
PATH="$fakecurl:/usr/bin:/bin" RECORD="$record" AIXODIA_PREFIX="$prefix" \
  bash "$HERE/install.sh" >/dev/null 2>&1
if grep -q "aixodia-engine-linux-$want" "$record"; then
  pass "engine download matches the host arch ($want)"
else
  fail "engine url for $want: $(tr '\n' ' ' < "$record")"
fi

echo
if [ "$fails" -eq 0 ]; then
  echo "all installer tests passed"
else
  echo "$fails installer test(s) failed"
  exit 1
fi
