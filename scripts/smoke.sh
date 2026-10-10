#!/usr/bin/env bash
# Start the built image and exercise the real binary: health, first-run setup, login, authed API, API keys, UI.
set -euo pipefail
IMAGE="${1:-consolidarr:ci}"
NAME="consolidarr-smoke-$$"
PORT="${PORT:-17480}"
DATA="$(mktemp -d)"; chmod 777 "$DATA"
cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; rm -rf "$DATA"; }
trap cleanup EXIT
docker run -d --name "$NAME" -p "127.0.0.1:$PORT:7480" -v "$DATA:/data" "$IMAGE" >/dev/null
B="http://127.0.0.1:$PORT"
for i in $(seq 1 30); do curl -fsS "$B/healthz" >/dev/null 2>&1 && break; sleep 1; done

# Capture each response first, then assert on it. (`curl | grep -q` under pipefail is flaky: grep exits at
# the first match, curl then fails writing to the closed pipe with exit 23.)
has() { grep -q -- "$2" <<<"$1" || { echo "FAIL: expected '$2' in: ${1:0:200}" >&2; exit 1; }; }
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

has "$(curl -fsS "$B/healthz")" ok
has "$(curl -fsS "$B/api/auth/me")" '"setupRequired":true'
has "$(curl -fsS -c "$DATA/cj" -X POST "$B/api/auth/setup" -H 'content-type: application/json' -d '{"username":"admin","password":"smoke-test-pw"}')" ok
test "$(code "$B/api/settings")" = 401                                              # anonymous is rejected
has "$(curl -fsS -b "$DATA/cj" "$B/api/settings")" '"dryRun":true'                  # safe default
has "$(curl -fsS "$B/")" '<div id="root">'                                          # UI is embedded
docker exec "$NAME" ffprobe -version >/dev/null                                     # verification dependency present
KEY=$(curl -fsS -b "$DATA/cj" -X POST "$B/api/keys" -H 'content-type: application/json' -d '{"name":"smoke"}' | sed -n 's/.*"key":"\([^"]*\)".*/\1/p')
test -n "$KEY"
has "$(curl -fsS -H "X-API-Key: $KEY" "$B/api/status")" flaggedTV                   # API-key auth works
test "$(code -H 'X-API-Key: csl_wrong' "$B/api/status")" = 401
has "$(curl -fsS "$B/api/openapi.json")" '"openapi"'                                # spec is served
test "$(code -X POST -H "X-API-Key: $KEY" "$B/api/engine/run")" = 409               # scheduler off is refused, not faked
echo "smoke ok"
