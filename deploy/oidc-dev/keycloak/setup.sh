#!/usr/bin/env bash
# Startet die lokale Keycloak-Entwicklungsinstanz, legt die Testnutzer an und schreibt
# .generated/uptimecluster.env fuer den Cluster. Idempotent: mehrfacher Aufruf ist sicher.
# NUR ENTWICKLUNG. Zugangsdaten landen in .generated/credentials.env (chmod 600), nie in der Ausgabe.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$HERE"

GEN="$HERE/.generated"
CRED="$GEN/credentials.env"
UPT="$GEN/uptimecluster.env"
BASE="http://localhost:8081"
REALM="uptime"
EXPECTED_ISSUER="$BASE/realms/$REALM"
TIMEOUT_S=180

die() { echo "setup.sh: $*" >&2; exit 1; }

for tool in podman curl openssl python3; do
  command -v "$tool" >/dev/null 2>&1 || die "required tool not found: $tool"
done

# json_get <key>: reads JSON from stdin, prints top-level string/number value of <key>.
json_get() {
  python3 -c 'import json,sys; v=json.load(sys.stdin)[sys.argv[1]]; print(v if not isinstance(v,(dict,list)) else json.dumps(v))' "$1"
}

umask 077
mkdir -p "$GEN"
chmod 700 "$GEN"

# Zugangsdaten erzeugen, falls noch nicht vorhanden. Vorhandene Werte bleiben unveraendert.
touch "$CRED"
chmod 600 "$CRED"
ensure_key() {
  local key="$1" value="$2"
  if ! grep -q "^${key}=" "$CRED"; then
    printf '%s=%s\n' "$key" "$value" >> "$CRED"
  fi
}
ensure_key KC_BOOTSTRAP_ADMIN_USERNAME "admin"
ensure_key KC_BOOTSTRAP_ADMIN_PASSWORD "$(openssl rand -hex 24)"
ensure_key USER_ADMIN_PASSWORD "$(openssl rand -hex 24)"
ensure_key USER_EDITOR_PASSWORD "$(openssl rand -hex 24)"
ensure_key USER_NOBODY_PASSWORD "$(openssl rand -hex 24)"

set -a
# shellcheck disable=SC1090
source "$CRED"
set +a

# Stack starten (idempotent: laeuft der Container schon, passiert nichts).
podman compose -f "$HERE/compose.yaml" up -d

echo "Warte auf Keycloak (Realm $REALM) ..." >&2
ready=""
for ((i = 0; i < TIMEOUT_S; i += 2)); do
  if curl -fsS -o /dev/null "$EXPECTED_ISSUER/.well-known/openid-configuration" 2>/dev/null; then
    ready=1
    break
  fi
  sleep 2
done
[[ -n "$ready" ]] || die "Keycloak not ready after ${TIMEOUT_S}s (see: podman logs uptime-oidc-dev-keycloak)"

# Issuer aus der Discovery muss exakt OIDC_ISSUER entsprechen.
issuer="$(curl -fsS "$EXPECTED_ISSUER/.well-known/openid-configuration" | json_get issuer)"
[[ "$issuer" == "$EXPECTED_ISSUER" ]] || die "discovery issuer '$issuer' != expected '$EXPECTED_ISSUER'"

# Admin-Token fuer die Admin-REST-API des Realms master.
admin_token="$(curl -fsS -X POST "$BASE/realms/master/protocol/openid-connect/token" \
  --data-urlencode "grant_type=password" \
  --data-urlencode "client_id=admin-cli" \
  --data-urlencode "username=$KC_BOOTSTRAP_ADMIN_USERNAME" \
  --data-urlencode "password=$KC_BOOTSTRAP_ADMIN_PASSWORD" | json_get access_token)"
[[ -n "$admin_token" ]] || die "could not obtain admin token"

api() { # api METHOD PATH [JSON-BODY]
  local method="$1" path="$2" body="${3:-}"
  if [[ -n "$body" ]]; then
    curl -fsS -X "$method" -H "Authorization: Bearer $admin_token" \
      -H "Content-Type: application/json" --data "$body" "$BASE/admin/realms/$REALM$path"
  else
    curl -fsS -X "$method" -H "Authorization: Bearer $admin_token" \
      "$BASE/admin/realms/$REALM$path"
  fi
}

# Realm-Rolle als Representation holen (id und name).
role_rep() {
  api GET "/roles/$1"
}

# ensure_user <email> <first> <last> <password-var-value> <role>
ensure_user() {
  local email="$1" first="$2" last="$3" password="$4" role="$5"
  local rep existing id

  rep="$(python3 -c '
import json,sys
email,first,last=sys.argv[1:4]
print(json.dumps({"username": email, "email": email, "emailVerified": True, "enabled": True,
  "firstName": first, "lastName": last, "requiredActions": []}))' "$email" "$first" "$last")"

  existing="$(api GET "/users?username=$email&exact=true")"
  id="$(printf '%s' "$existing" | python3 -c 'import json,sys; l=json.load(sys.stdin); print(l[0]["id"] if l else "")')"

  if [[ -z "$id" ]]; then
    api POST "/users" "$rep" >/dev/null
    existing="$(api GET "/users?username=$email&exact=true")"
    id="$(printf '%s' "$existing" | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["id"])')"
  else
    # Bestehenden Nutzer auf den Sollzustand bringen (verifiziert, keine Pflichtaktionen).
    api PUT "/users/$id" "$rep" >/dev/null
  fi

  # Passwort aus credentials.env setzen (nicht temporaer).
  local pw_body
  pw_body="$(python3 -c 'import json,sys; print(json.dumps({"type":"password","value":sys.argv[1],"temporary":False}))' "$password")"
  api PUT "/users/$id/reset-password" "$pw_body" >/dev/null

  # Rolle zuweisen (wiederholte Zuweisung ist ein No-op).
  local role_body
  role_body="$(role_rep "$role" | python3 -c 'import json,sys; print(json.dumps([json.load(sys.stdin)]))')"
  api POST "/users/$id/role-mappings/realm" "$role_body" >/dev/null

  echo "Nutzer bereit: $email (Rolle: $role)" >&2
}

ensure_user "admin@example.test" "Test" "Admin" "$USER_ADMIN_PASSWORD" "platform-admin"
ensure_user "editor@example.test" "Test" "Editor" "$USER_EDITOR_PASSWORD" "tenant-editor"
ensure_user "nobody@example.test" "Test" "Nobody" "$USER_NOBODY_PASSWORD" "staff"

# Cluster-Umgebung schreiben (atomar).
tmp="$(mktemp "$GEN/uptimecluster.XXXXXX")"
cat > "$tmp" <<ENV
AUTH_MODE=oidc
OIDC_PROFILE=generic
OIDC_ISSUER=$issuer
OIDC_AUDIENCE=uptimecluster-api
OIDC_CLIENT_IDS=uptimecluster-spa,uptimecluster-cli
OIDC_ROLES_CLAIM=realm_access.roles
OIDC_INSECURE_HTTP=true
OIDC_MAX_TOKEN_TTL=1h
ENV
chmod 600 "$tmp"
mv "$tmp" "$UPT"

echo "Keycloak laeuft: $BASE (Realm $REALM, Issuer $issuer)" >&2
echo "Cluster-Umgebung: $UPT" >&2
echo "Zugangsdaten: $CRED (nicht ausgegeben)" >&2
