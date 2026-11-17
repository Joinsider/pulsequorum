#!/usr/bin/env bash
# Entfernt die Container und Volumes des lokalen Zitadel (Datenbank wird gelöscht).
# .generated/ bleibt erhalten; ein neuer ./setup.sh legt die Instanz neu an und überschreibt
# die Bootstrap-Dateien und Zugangsdaten. Nur für die Entwicklung.
set -euo pipefail

cd "$(dirname "$0")"

if [[ -f .generated/compose.env ]]; then
  podman-compose -f compose.yaml --env-file .generated/compose.env down --volumes
else
  podman-compose -f compose.yaml down --volumes
fi
