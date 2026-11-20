#!/usr/bin/env bash
# Gibt ein Access-Token eines Testnutzers auf stdout aus (Password Grant mit pulsequorum-cli).
# Nur fuer Entwicklungsskripte. Passwoerter kommen aus .generated/credentials.env und werden nie ausgegeben.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CRED="$HERE/.generated/credentials.env"
TOKEN_URL="http://localhost:8081/realms/uptime/protocol/openid-connect/token"

usage() { echo "usage: token.sh admin@example.test|editor@example.test|nobody@example.test" >&2; exit 2; }

user="${1:-}"
case "$user" in
  admin@example.test) var=USER_ADMIN_PASSWORD ;;
  editor@example.test) var=USER_EDITOR_PASSWORD ;;
  nobody@example.test) var=USER_NOBODY_PASSWORD ;;
  *) usage ;;
esac

[[ -f "$CRED" ]] || { echo "token.sh: $CRED fehlt, zuerst setup.sh ausfuehren" >&2; exit 1; }

set -a
# shellcheck disable=SC1090
source "$CRED"
set +a
password="${!var:-}"
[[ -n "$password" ]] || { echo "token.sh: kein Passwort fuer $user in $CRED" >&2; exit 1; }

resp="$(curl -sS -X POST "$TOKEN_URL" \
  --data-urlencode "grant_type=password" \
  --data-urlencode "client_id=pulsequorum-cli" \
  --data-urlencode "scope=openid" \
  --data-urlencode "username=$user" \
  --data-urlencode "password=$password")"

printf '%s' "$resp" | python3 -c '
import json,sys
d=json.load(sys.stdin)
t=d.get("access_token")
if not t:
    sys.stderr.write("token.sh: Token-Anfrage fehlgeschlagen: %s\n" % d.get("error_description", d.get("error", "unbekannt")))
    sys.exit(1)
sys.stdout.write(t + "\n")
'
