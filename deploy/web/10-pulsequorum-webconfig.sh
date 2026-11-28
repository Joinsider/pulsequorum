#!/bin/sh
# Runs before nginx starts (the nginx image runs /docker-entrypoint.d/*.sh
# under "set -e", so a failing step aborts the container start).
#
# pulsequorum-webconfig reads its settings from the environment (FRONTEND_NODES,
# AUTH_MODE, OIDC_*, FRONTEND_HSTS). This script passes no values, so it does not
# template anything. It writes DIR/config.json and the nginx include with the
# security headers (ADR 0036, 6B).
set -eu

mkdir -p /tmp/pulsequorum/web
/usr/local/bin/pulsequorum-webconfig --web-root /tmp/pulsequorum/web --headers /tmp/pulsequorum/headers.conf
