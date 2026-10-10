#!/usr/bin/env bash
# Start the built image and exercise the real binary: health, first-run setup, login, authed API.
set -euo pipefail
IMAGE="${1:-consolidarr:ci}"
NAME="consolidarr-smoke-$$"
PORT="${PORT:-17480}"
DATA="$(mktemp -d)"; chmod 777 "$DATA"
cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; rm -rf "$DATA"; }
trap cleanup EXIT
docker run -d --name "$NAME" -p "127.0.0.1:$PORT:7480" -v "$DATA:/data" "$IMAGE" >/dev/null
for i in $(seq 1 30); do curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break; sleep 1; done
B="http://127.0.0.1:$PORT"
curl -fsS "$B/healthz" | grep -q ok
curl -fsS "$B/api/auth/me" | grep -q '"setupRequired":true'
curl -fsS -c "$DATA/cj" -X POST "$B/api/auth/setup" -H 'content-type: application/json' -d '{"username":"admin","password":"smoke-test-pw"}' | grep -q ok
test "$(curl -s -o /dev/null -w '%{http_code}' "$B/api/settings")" = 401           # anonymous is rejected
curl -fsS -b "$DATA/cj" "$B/api/settings" | grep -q '"dryRun":true'                 # safe default
curl -fsS "$B/" | grep -qi '<div id="root">'                                        # UI is embedded
ffprobe -version >/dev/null 2>&1 || docker exec "$NAME" ffprobe -version >/dev/null  # verification dependency present
KEY=$(curl -fsS -b "$DATA/cj" -X POST "$B/api/keys" -H 'content-type: application/json' -d '{"name":"smoke"}' | sed -n 's/.*"key":"\([^"]*\)".*/\1/p')
test -n "$KEY"
curl -fsS -H "X-API-Key: $KEY" "$B/api/status" | grep -q flaggedTV                   # API-key auth works
test "$(curl -s -o /dev/null -w '%{http_code}' -H 'X-API-Key: csl_wrong' "$B/api/status")" = 401
curl -fsS "$B/api/openapi.json" | grep -q '"openapi"'                                # spec is served
echo "smoke ok"
