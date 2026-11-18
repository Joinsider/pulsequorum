#!/usr/bin/env bash
# Gibt ein Access-Token eines Maschinenbenutzers (Client Credentials) auf stdout aus.
# Verwendung: ./token.sh <bot>, bot = platform-bot|acme-bot|platform-admin-bot|acme-admin-bot|globex-admin-bot|initech-bot
# Geheimnisse werden nie ausgegeben. Fehler gehen nach stderr ohne Antwortkörper mit Geheimnissen.
set -euo pipefail

cd "$(dirname "$0")"

usage() {
  echo "Verwendung: $0 platform-bot|acme-bot|platform-admin-bot|acme-admin-bot|globex-admin-bot|initech-bot" >&2
  exit 2
}
[[ $# -eq 1 ]] || usage
case "$1" in
  platform-bot) prefix="PLATFORM_BOT" ;;
  acme-bot) prefix="ACME_BOT" ;;
  platform-admin-bot) prefix="PLATFORM_ADMIN_BOT" ;;
  acme-admin-bot) prefix="ACME_ADMIN_BOT" ;;
  globex-admin-bot) prefix="GLOBEX_ADMIN_BOT" ;;
  initech-bot) prefix="INITECH_BOT" ;;
  *) usage ;;
esac

[[ -f .generated/ids.env && -f .generated/bots.env ]] || {
  echo "FEHLER: .generated/ fehlt. Zuerst ./setup.sh ausführen." >&2
  exit 1
}
# shellcheck disable=SC1091
source .generated/ids.env
# shellcheck disable=SC1091
source .generated/bots.env

client_id_var="${prefix}_CLIENT_ID"
client_secret_var="${prefix}_CLIENT_SECRET"
client_id="${!client_id_var}"
client_secret="${!client_secret_var}"

scope="openid urn:zitadel:iam:user:resourceowner urn:zitadel:iam:org:project:id:${PROJECT_ID}:aud urn:zitadel:iam:org:projects:roles"

# Die Zugangsdaten gehen über eine curl-Konfiguration (-K), damit sie nicht in der Prozessliste stehen.
tmp_body="$(mktemp)"
trap 'rm -f "$tmp_body"' EXIT
code="$(curl -sS -m 30 -o "$tmp_body" -w '%{http_code}' -K <(printf 'user = "%s:%s"\n' "$client_id" "$client_secret") \
  --data-urlencode "grant_type=client_credentials" \
  --data-urlencode "scope=${scope}" \
  "http://localhost:8082/oauth/v2/token")"

if [[ "$code" != "200" ]]; then
  echo "FEHLER: Token-Endpunkt antwortete mit HTTP $code" >&2
  jq -r '.error // .message // "keine Fehlerdetails"' "$tmp_body" >&2 || true
  exit 1
fi

jq -er '.access_token' "$tmp_body"
