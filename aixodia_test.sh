#!/usr/bin/env bash
# Tests for the aixodia launcher. Run: bash aixodia_test.sh
# No framework: a stub engine stands in for the real binary so the launcher's
# own logic (env check, pidfile, wait-for-url, stop/status) is what is tested.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
LAUNCHER="$HERE/aixodia"

fails=0
pass() { echo "ok   - $1"; }
fail() { echo "FAIL - $1"; fails=$((fails + 1)); }

tmp_home() { mktemp -d; }

# Stub engine: prints a tunnel URL and stays alive until it is killed.
make_stub_engine() {
  cat > "$1" <<'STUB'
#!/usr/bin/env bash
echo "engine started"
echo "tunnel url: https://stub-tunnel.trycloudflare.com"
while true; do sleep 0.5; done
STUB
  chmod +x "$1"
}

# --- missing CF_TOKEN: refuse before starting anything ------------------------
home=$(tmp_home); bin=$(tmp_home)/ai-engine; make_stub_engine "$bin"
out=$(env -u CF_TOKEN AIXODIA_HOME="$home" AIXODIA_ENGINE_BIN="$bin" bash "$LAUNCHER" 2>&1); rc=$?
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -qi "CF_TOKEN"; then
  pass "start without CF_TOKEN fails with a message"
else
  fail "start without CF_TOKEN (rc=$rc, out=$out)"
fi
if [ ! -f "$home/aixodia.pid" ]; then
  pass "no pidfile written when CF_TOKEN is missing"
else
  fail "pidfile written without CF_TOKEN"
fi

# --- status when not running -------------------------------------------------
home=$(tmp_home)
out=$(AIXODIA_HOME="$home" bash "$LAUNCHER" status 2>&1); rc=$?
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -qi "not running"; then
  pass "status reports not running"
else
  fail "status not-running (rc=$rc, out=$out)"
fi

# --- already running: do not start a second engine ---------------------------
home=$(tmp_home); bin=$(tmp_home)/ai-engine; make_stub_engine "$bin"
echo "$$" > "$home/aixodia.pid"
out=$(CF_TOKEN=x AIXODIA_HOME="$home" AIXODIA_ENGINE_BIN="$bin" bash "$LAUNCHER" 2>&1); rc=$?
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -qi "already running"; then
  pass "second start reports already running"
else
  fail "already-running (rc=$rc, out=$out)"
fi
if [ "$(cat "$home/aixodia.pid")" = "$$" ]; then
  pass "already-running keeps the existing pid"
else
  fail "already-running overwrote the pid"
fi

# --- start, then status, stop, status ---------------------------------------
home=$(tmp_home); fakebin=$(tmp_home); bin=$fakebin/ai-engine
make_stub_engine "$bin"
printf '#!/usr/bin/env bash\nexec sleep 100000\n' > "$fakebin/cloudflared"
chmod +x "$fakebin/cloudflared"
out=$(CF_TOKEN=x AIXODIA_HOME="$home" AIXODIA_ENGINE_BIN="$bin" AIXODIA_TUNNEL_WAIT=15 PATH="$fakebin:$PATH" bash "$LAUNCHER" 2>&1); rc=$?
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q "https://stub-tunnel.trycloudflare.com"; then
  pass "start prints the tunnel url and returns"
else
  fail "start (rc=$rc, out=$out)"
fi
pid=$(cat "$home/aixodia.pid" 2>/dev/null || echo "")
if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
  pass "engine is left running in the background"
else
  fail "engine not running after start (pid=$pid)"
fi

out=$(AIXODIA_HOME="$home" bash "$LAUNCHER" status 2>&1); rc=$?
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q "https://stub-tunnel.trycloudflare.com"; then
  pass "status reports the running url"
else
  fail "status running (rc=$rc, out=$out)"
fi

AIXODIA_HOME="$home" bash "$LAUNCHER" stop >/dev/null 2>&1
sleep 0.5
if ! kill -0 "$pid" 2>/dev/null; then
  pass "stop terminates the engine"
else
  fail "stop left the engine alive (pid=$pid)"
fi
if [ ! -f "$home/aixodia.pid" ]; then
  pass "stop removes the pidfile"
else
  fail "stop left the pidfile"
fi

echo
if [ "$fails" -eq 0 ]; then
  echo "all launcher tests passed"
else
  echo "$fails launcher test(s) failed"
  exit 1
fi
