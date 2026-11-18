#!/usr/bin/env bash
# Removes the SSRF test environment: the ssrf-* containers, the networks
# ssrf-pub and ssrf-int, and deploy/ssrf-dev/.generated/. Nothing else is touched.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GEN="$HERE/.generated"

die() { echo "down.sh: $*" >&2; exit 1; }
log() { echo "down.sh: $*" >&2; }

command -v podman >/dev/null 2>&1 || die "required tool not found: podman"
podman info >/dev/null 2>&1 || die "podman is not reachable; start the machine with: podman machine start"

for c in ssrf-node ssrf-target ssrf-redirect ssrf-dns; do
  if podman container exists "$c"; then
    log "removing container $c"
    podman rm -f "$c" >/dev/null
  fi
done

for n in ssrf-pub ssrf-int; do
  if podman network exists "$n"; then
    log "removing network $n"
    podman network rm -f "$n" >/dev/null
  fi
done

if [[ -d "$GEN" ]]; then
  log "removing $GEN"
  rm -rf "$GEN"
fi

log "done"
