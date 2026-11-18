#!/usr/bin/env bash
# Starts the SSRF test environment for AS-07 (ADR 0030) with Podman.
#
#   setup.sh [on|off]    default: on
#
# Builds the node and the test helper for linux/<host arch>, (re)creates the
# containers and a fresh single-voter node with SSRF_PROTECTION=<mode>, and
# creates the first admin. Credentials go to .generated/admin.env (chmod 600);
# the password is never printed.
#
# Topology: see deploy/ssrf-dev/README.md. Development use only.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
GEN="$HERE/.generated"
BIN="$GEN/bin"
ADMIN_ENV="$GEN/admin.env"

MODE="${1:-on}"
IMAGE="docker.io/library/alpine:3.20"
PUB_NET="ssrf-pub"
PUB_SUBNET="203.0.113.0/24"
INT_NET="ssrf-int"
INT_SUBNET="10.99.0.0/24"
NODE_PORT=18080
BASE="http://127.0.0.1:${NODE_PORT}"
READY_TIMEOUT_S=120
TOKEN_TIMEOUT_S=60

die() { echo "setup.sh: $*" >&2; exit 1; }
log() { echo "setup.sh: $*" >&2; }

case "$MODE" in
  on | off) ;;
  *) die "usage: setup.sh [on|off], got '$MODE'" ;;
esac

for tool in podman go curl jq openssl; do
  command -v "$tool" >/dev/null 2>&1 || die "required tool not found: $tool"
done
podman info >/dev/null 2>&1 || die "podman is not reachable; start the machine with: podman machine start"

umask 077
mkdir -p "$BIN"
chmod 700 "$GEN"

case "$(podman info --format '{{.Host.Arch}}')" in
  aarch64 | arm64) GOARCH=arm64 ;;
  x86_64 | amd64) GOARCH=amd64 ;;
  *) die "unsupported host architecture: $(podman info --format '{{.Host.Arch}}')" ;;
esac

# 1. Binaries: the node (embeds whatever SPA build exists) and the test helper.
log "building linux/$GOARCH binaries"
(cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -trimpath \
  -o "$BIN/ssrfenv" ./test/ssrfenv/cmd/ssrfenv)
(cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -trimpath \
  -o "$BIN/uptimecluster" ./cmd/uptimecluster)

# 2. Networks. --disable-dns: the test DNS server at 203.0.113.53 is the only resolver.
ensure_network() {
  local name="$1" subnet="$2"
  if podman network exists "$name"; then
    log "network $name exists"
  else
    log "creating network $name ($subnet)"
    podman network create --subnet "$subnet" --disable-dns "$name" >/dev/null
  fi
}
ensure_network "$PUB_NET" "$PUB_SUBNET"
ensure_network "$INT_NET" "$INT_SUBNET"

# 3. Containers. Recreated on every run so the canary log and the node state start empty.
for c in ssrf-node ssrf-target ssrf-redirect ssrf-dns; do
  if podman container exists "$c"; then
    podman rm -f "$c" >/dev/null
  fi
done

log "starting ssrf-dns (203.0.113.53)"
podman run -d --name ssrf-dns \
  --network "$PUB_NET:ip=203.0.113.53" \
  -v "$BIN/ssrfenv:/usr/local/bin/ssrfenv:ro" \
  "$IMAGE" /usr/local/bin/ssrfenv -mode dns >/dev/null

log "starting ssrf-redirect (203.0.113.10)"
podman run -d --name ssrf-redirect \
  --network "$PUB_NET:ip=203.0.113.10" \
  -v "$BIN/ssrfenv:/usr/local/bin/ssrfenv:ro" \
  "$IMAGE" /usr/local/bin/ssrfenv -mode redirect >/dev/null

log "starting ssrf-target (10.99.0.10, canary)"
podman run -d --name ssrf-target \
  --network "$INT_NET:ip=10.99.0.10" \
  -v "$BIN/ssrfenv:/usr/local/bin/ssrfenv:ro" \
  "$IMAGE" /usr/local/bin/ssrfenv -mode target -log /var/log/canary.log >/dev/null

log "starting ssrf-node (203.0.113.20 / 10.99.0.20, SSRF_PROTECTION=$MODE)"
podman run -d --name ssrf-node \
  --network "$PUB_NET:ip=203.0.113.20" \
  --network "$INT_NET:ip=10.99.0.20" \
  --dns 203.0.113.53 \
  -p "127.0.0.1:${NODE_PORT}:8080" \
  --tmpfs /data \
  -e NODE_NAME=ssrf1 \
  -e NODE_ROLE=voter \
  -e AUTH_MODE=local \
  -e SSRF_PROTECTION="$MODE" \
  -e LOG_FORMAT=text \
  -e LOG_LEVEL=info \
  -e DATA_DIR=/data \
  -e HTTP_ADDR=:8080 \
  -e ICMP_MODE=off \
  -v "$BIN/uptimecluster:/usr/local/bin/uptimecluster:ro" \
  "$IMAGE" /usr/local/bin/uptimecluster >/dev/null

# 4. Wait for readiness.
log "waiting for the node (up to ${READY_TIMEOUT_S}s)"
ready=""
for ((i = 0; i < READY_TIMEOUT_S; i += 2)); do
  if curl -fsS -o /dev/null "$BASE/readyz" 2>/dev/null; then
    ready=1
    break
  fi
  if [[ "$(podman inspect -f '{{.State.Running}}' ssrf-node 2>/dev/null || true)" != "true" ]]; then
    podman logs --tail 40 ssrf-node >&2 || true
    die "ssrf-node stopped during startup"
  fi
  sleep 2
done
if [[ -z "$ready" ]]; then
  podman logs --tail 40 ssrf-node >&2 || true
  die "node not ready after ${READY_TIMEOUT_S}s"
fi

# 5. First admin. The one-time setup token is logged by the node (FA-AUTH-01).
token=""
for ((i = 0; i < TOKEN_TIMEOUT_S; i += 2)); do
  token="$(podman logs ssrf-node 2>&1 | sed -nE 's/.*setup_token[=":]+([^[:space:]"]+).*/\1/p' | tail -n 1 || true)"
  [[ -n "$token" ]] && break
  sleep 2
done
[[ -n "$token" ]] || die "node logged no setup token within ${TOKEN_TIMEOUT_S}s"

password="$(openssl rand -hex 24)"
body="$(jq -n --arg t "$token" --arg p "$password" \
  '{setupToken: $t, username: "admin", displayName: "SSRF test admin", password: $p}')"
code="$(printf '%s' "$body" | curl -sS -o /dev/null -w '%{http_code}' -X POST "$BASE/api/v1/auth/setup" \
  -H 'Content-Type: application/json' --data-binary @- || true)"
[[ "$code" == "201" ]] || die "admin setup failed with HTTP $code"

{
  printf 'SSRF_ADMIN_USER=%s\n' admin
  printf 'SSRF_ADMIN_PASSWORD=%s\n' "$password"
} > "$ADMIN_ENV"
chmod 600 "$ADMIN_ENV"
unset token body password

log "environment ready: SSRF_PROTECTION=$MODE, node at $BASE"
log "next: $HERE/check.sh $MODE"
