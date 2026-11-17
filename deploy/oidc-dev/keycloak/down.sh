#!/usr/bin/env bash
# Stoppt Keycloak und entfernt Container und Volumes. Die Datei .generated/ bleibt erhalten,
# ein erneutes setup.sh verwendet dieselben Zugangsdaten.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$HERE"

set -a
# Variablen fuer die Compose-Substitution, falls vorhanden.
[[ -f .generated/credentials.env ]] && source .generated/credentials.env
set +a

podman compose -f "$HERE/compose.yaml" down --volumes
