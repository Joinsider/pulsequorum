#!/usr/bin/env bash
# Startet den lokalen Zitadel und provisioniert ihn für Pulsequorum (OIDC, Profil zitadel, Tenancy multi).
# NUR ENTWICKLUNG. Idempotent: vorhandene Ressourcen werden wiederverwendet. Passwörter und Bot-Secrets
# werden bei jedem Lauf neu erzeugt und in .generated/ geschrieben (chmod 600).
#
# Voraussetzungen: podman-compose, curl, jq, openssl.
set -euo pipefail

cd "$(dirname "$0")"
umask 077

readonly GEN=".generated"
readonly BOOT="$GEN/bootstrap"
readonly COMPOSE_ENV="$GEN/compose.env"
readonly PAT_FILE="$BOOT/iam-admin.pat"
readonly BASE="http://localhost:8082"
readonly LOGIN_HEALTH="http://localhost:3000/ui/v2/login/healthy"
readonly PROJECT_NAME="pulsequorum"
readonly SPA_NAME="pulsequorum-spa"
readonly SPA_REDIRECT="http://localhost:8080/auth/callback"
readonly SPA_POST_LOGOUT="http://localhost:8080/login"
readonly SPA_ORIGIN="http://localhost:8080"
readonly ROLES=(platform-admin tenant-admin tenant-editor tenant-viewer)
readonly GRANT_ROLES_JSON='["tenant-admin","tenant-editor","tenant-viewer"]'

COMPOSE=(podman-compose -f compose.yaml --env-file "$COMPOSE_ENV")

log() { printf '==> %s\n' "$*"; }
die() { printf 'FEHLER: %s\n' "$*" >&2; exit 1; }
gen_password() { printf 'Aa1!%s' "$(openssl rand -hex 12)"; }

wait_for() { # URL TIMEOUT_SEKUNDEN NAME
  local url="$1" timeout="$2" name="$3" i
  for ((i = 0; i < timeout; i += 2)); do
    if curl -fsS -m 5 -o /dev/null "$url" 2>/dev/null; then return 0; fi
    sleep 2
  done
  die "$name antwortet nach ${timeout}s nicht ($url). Logs: podman-compose -f compose.yaml logs"
}

# --- Geheimnisse (nur beim ersten Lauf erzeugt; der Masterkey muss zur Datenbank passen) ---
mkdir -p "$BOOT"
chmod 700 "$GEN" "$BOOT"
if [[ ! -f "$COMPOSE_ENV" ]]; then
  log "erzeuge Masterkey und Dev-Geheimnisse in $COMPOSE_ENV"
  (
    echo "ZITADEL_MASTERKEY=$(openssl rand -hex 16)"
    echo "PG_PASSWORD=$(openssl rand -hex 24)"
    echo "DEV_ADMIN_PASSWORD=$(gen_password)"
    echo "LOGIN_SESSION_COOKIE_SECRET=$(openssl rand -hex 32)"
  ) > "$COMPOSE_ENV"
  chmod 600 "$COMPOSE_ENV"
fi

# --- Container ---
log "starte Container (podman-compose up -d)"
"${COMPOSE[@]}" up -d
wait_for "$BASE/debug/ready" 240 "Zitadel"
wait_for "$LOGIN_HEALTH" 120 "Login V2"
for _ in $(seq 1 30); do
  [[ -s "$PAT_FILE" ]] && break
  sleep 1
done
[[ -s "$PAT_FILE" ]] || die "PAT fehlt: $PAT_FILE"
PAT_VALUE="$(cat "$PAT_FILE")"

# --- API-Helfer: RESP und HTTP_CODE sind globale Variablen (kein Subshell) ---
RESP=""
HTTP_CODE=""
ORG_HDR=() # optional: ORG_HDR=(-H "x-zitadel-orgid: ID") für v1-Aufrufe im Kontext einer Organisation
api_call() { # METHOD PATH [JSON]; Rückgabe 0 bei 2xx
  local method="$1" path="$2" data="${3:-}" out code
  out="$(mktemp)"
  local -a args=(-sS -m 30 -o "$out" -w '%{http_code}' -X "$method"
    -H "Authorization: Bearer $PAT_VALUE" -H 'Content-Type: application/json' ${ORG_HDR[@]+"${ORG_HDR[@]}"})
  if [[ -n "$data" ]]; then args+=(--data-binary "$data"); fi
  if ! code="$(curl "${args[@]}" "$BASE$path")"; then
    rm -f "$out"; HTTP_CODE="000"; RESP=""; return 1
  fi
  RESP="$(cat "$out")"
  rm -f "$out"
  HTTP_CODE="$code"
  [[ "$code" =~ ^2 ]]
}
must() { # METHOD PATH [JSON]; bricht bei Fehler ab
  if ! api_call "$@"; then
    die "API $1 $2 -> HTTP $HTTP_CODE: $RESP"
  fi
}
must_upd() { # wie must, aber "No changes" (Zitadel: failed_precondition) gilt als Erfolg
  if ! api_call "$@"; then
    if grep -q "No changes" <<<"$RESP"; then return 0; fi
    die "API $1 $2 -> HTTP $HTTP_CODE: $RESP"
  fi
}
jqr() { jq -r "$1" <<<"$RESP"; } # Filter auf RESP; für Filter mit Argumenten jq direkt aufrufen

# --- Instanz und Plattform-Organisation ---
must GET /auth/v1/users/me
PLATFORM_ORG_ID="$(jqr '.user.details.resourceOwner')"
[[ -n "$PLATFORM_ORG_ID" && "$PLATFORM_ORG_ID" != null ]] || die "Plattform-Organisation nicht ermittelbar"
log "Plattform-Organisation: $PLATFORM_ORG_ID"

ensure_org() { # NAME -> ORG_ID
  must POST /v2/organizations/_search "$(jq -nc --arg n "$1" \
    '{queries:[{nameQuery:{name:$n,method:"TEXT_QUERY_METHOD_EQUALS"}}]}')"
  ORG_ID="$(jqr '.result[0].id // empty')"
  if [[ -z "$ORG_ID" ]]; then
    must POST /v2/organizations "$(jq -nc --arg n "$1" '{name:$n}')"
    ORG_ID="$(jqr '.organizationId')"
    log "Organisation $1 angelegt"
  fi
}
ensure_org acme
ACME_ORG_ID="$ORG_ID"
ensure_org globex
GLOBEX_ORG_ID="$ORG_ID"
ensure_org initech
INITECH_ORG_ID="$ORG_ID"

# --- Projekt, Rollen, Assert Roles on Authentication ---
must POST /zitadel.project.v2.ProjectService/ListProjects "$(jq -nc --arg n "$PROJECT_NAME" \
  '{filters:[{projectNameFilter:{projectName:$n,method:"TEXT_FILTER_METHOD_EQUALS"}}]}')"
PROJECT_ID="$(jqr '.projects[0].projectId // empty')"
if [[ -z "$PROJECT_ID" ]]; then
  must POST /zitadel.project.v2.ProjectService/CreateProject "$(jq -nc --arg o "$PLATFORM_ORG_ID" --arg n "$PROJECT_NAME" \
    '{organizationId:$o,name:$n,projectRoleAssertion:true}')"
  PROJECT_ID="$(jqr '.projectId')"
  log "Projekt $PROJECT_NAME angelegt"
fi
must_upd POST /zitadel.project.v2.ProjectService/UpdateProject \
  "$(jq -nc --arg p "$PROJECT_ID" '{projectId:$p,projectRoleAssertion:true}')"

must POST /zitadel.project.v2.ProjectService/ListProjectRoles "$(jq -nc --arg p "$PROJECT_ID" '{projectId:$p}')"
EXISTING_ROLES="$(jqr '.projectRoles[]?.key')"
for role in "${ROLES[@]}"; do
  if grep -qx "$role" <<<"$EXISTING_ROLES"; then continue; fi
  must POST /zitadel.project.v2.ProjectService/AddProjectRole \
    "$(jq -nc --arg p "$PROJECT_ID" --arg r "$role" '{projectId:$p,roleKey:$r,displayName:$r}')"
  log "Rolle $role angelegt"
done

# --- OIDC-Anwendung (SPA, PKCE, kein Client-Secret) ---
SPA_CFG="$(jq -nc --arg r "$SPA_REDIRECT" --arg l "$SPA_POST_LOGOUT" --arg o "$SPA_ORIGIN" '{
  redirectUris:[$r], postLogoutRedirectUris:[$l], additionalOrigins:[$o],
  responseTypes:["OIDC_RESPONSE_TYPE_CODE"],
  grantTypes:["OIDC_GRANT_TYPE_AUTHORIZATION_CODE","OIDC_GRANT_TYPE_REFRESH_TOKEN"],
  applicationType:"OIDC_APP_TYPE_USER_AGENT", authMethodType:"OIDC_AUTH_METHOD_TYPE_NONE",
  version:"OIDC_VERSION_1_0", developmentMode:true,
  accessTokenType:"OIDC_TOKEN_TYPE_JWT", accessTokenRoleAssertion:true }')"

must POST /zitadel.application.v2.ApplicationService/ListApplications \
  "$(jq -nc --arg p "$PROJECT_ID" '{filters:[{projectIdFilter:{projectId:$p}}]}')"
SPA_APP_ID="$(jq -r --arg n "$SPA_NAME" '[.applications[]? | select(.name==$n) | .applicationId][0] // empty' <<<"$RESP")"
if [[ -z "$SPA_APP_ID" ]]; then
  must POST /zitadel.application.v2.ApplicationService/CreateApplication "$(jq -nc --arg p "$PROJECT_ID" \
    --arg n "$SPA_NAME" --argjson c "$SPA_CFG" '{projectId:$p,name:$n,oidcConfiguration:$c}')"
  log "Anwendung $SPA_NAME angelegt"
else
  must_upd POST /zitadel.application.v2.ApplicationService/UpdateApplication "$(jq -nc --arg p "$PROJECT_ID" \
    --arg a "$SPA_APP_ID" --argjson c "$SPA_CFG" '{projectId:$p,applicationId:$a,oidcConfiguration:$c}')"
  log "Anwendung $SPA_NAME aktualisiert"
fi
must POST /zitadel.application.v2.ApplicationService/ListApplications \
  "$(jq -nc --arg p "$PROJECT_ID" '{filters:[{projectIdFilter:{projectId:$p}}]}')"
SPA_CLIENT_ID="$(jq -r --arg n "$SPA_NAME" '[.applications[]? | select(.name==$n) | .oidcConfiguration.clientId][0] // empty' <<<"$RESP")"
[[ -n "$SPA_CLIENT_ID" ]] || die "Client-ID der SPA nicht gefunden"

# --- Instanz-Standard: Access-Token-Laufzeit 15 min ---
must GET /admin/v1/settings/oidc
CUR_OIDC="$RESP"
must_upd PUT /admin/v1/settings/oidc "$(jq -nc --argjson c "$CUR_OIDC" '{
  accessTokenLifetime:"900s",
  idTokenLifetime:$c.settings.idTokenLifetime,
  refreshTokenIdleExpiration:$c.settings.refreshTokenIdleExpiration,
  refreshTokenExpiration:$c.settings.refreshTokenExpiration }')"
log "Instanz: accessTokenLifetime = 900s"

# --- Projekt-Grants an Organisationen (ohne platform-admin) ---
# ensure_project_grant ORG_ID: legt den Grant an oder aktualisiert ihn (409 = bereits vorhanden)
ensure_project_grant() {
  local org="$1" body
  body="$(jq -nc --arg p "$PROJECT_ID" --arg o "$org" --argjson r "$GRANT_ROLES_JSON" \
    '{projectId:$p,grantedOrganizationId:$o,roleKeys:$r}')"
  if api_call POST /zitadel.project.v2.ProjectService/CreateProjectGrant "$body"; then
    log "Projekt-Grant an $org angelegt"
  elif [[ "$HTTP_CODE" == "409" ]]; then
    must_upd POST /zitadel.project.v2.ProjectService/UpdateProjectGrant "$body"
    log "Projekt-Grant an $org aktualisiert"
  else
    die "CreateProjectGrant -> HTTP $HTTP_CODE: $RESP"
  fi
}
ensure_project_grant "$ACME_ORG_ID"
ensure_project_grant "$GLOBEX_ORG_ID"
# initech bekommt bewusst KEINEN Grant (Negativtest: Token ohne Projektrolle)

# --- Menschen: ensure_human BENUTZERNAME ORG_ID VORNAME NACHNAME E-MAIL -> USER_ID, USER_PASSWORD ---
ensure_human() {
  local username="$1" org="$2" given="$3" family="$4" email="$5"
  USER_PASSWORD="$(gen_password)"
  must POST /v2/users "$(jq -nc --arg u "$username" \
    '{queries:[{userNameQuery:{userName:$u,method:"TEXT_QUERY_METHOD_EQUALS"}}]}')"
  USER_ID="$(jqr '.result[0].userId // empty')"
  if [[ -z "$USER_ID" ]]; then
    must POST /v2/users/human "$(jq -nc --arg u "$username" --arg o "$org" --arg g "$given" --arg f "$family" \
      --arg e "$email" --arg pw "$USER_PASSWORD" '{
        username:$u, organization:{orgId:$o},
        profile:{givenName:$g, familyName:$f, displayName:($g+" "+$f)},
        email:{email:$e, isVerified:true},
        password:{password:$pw, changeRequired:false} }')"
    USER_ID="$(jqr '.userId')"
    log "Mensch $username angelegt"
  else
    must POST "/v2/users/$USER_ID/password" "$(jq -nc --arg pw "$USER_PASSWORD" \
      '{newPassword:{password:$pw, changeRequired:false}}')"
    log "Passwort für $username neu gesetzt"
  fi
}

ADMIN_EMAIL="admin@platform.example.test"
EDITOR_EMAIL="editor@acme.example.test"
ensure_human "$ADMIN_EMAIL" "$PLATFORM_ORG_ID" Platform Admin "$ADMIN_EMAIL"
ADMIN_ID="$USER_ID"
ADMIN_PW="$USER_PASSWORD"
ensure_human "$EDITOR_EMAIL" "$ACME_ORG_ID" Acme Editor "$EDITOR_EMAIL"
EDITOR_ID="$USER_ID"
EDITOR_PW="$USER_PASSWORD"

# --- Maschinenbenutzer: ensure_machine BENUTZERNAME ORG_ID -> MACHINE_ID (Token-Typ JWT) ---
ensure_machine() {
  local username="$1" org="$2"
  must POST /v2/users "$(jq -nc --arg u "$username" \
    '{queries:[{userNameQuery:{userName:$u,method:"TEXT_QUERY_METHOD_EQUALS"}}]}')"
  MACHINE_ID="$(jqr '.result[0].userId // empty')"
  if [[ -z "$MACHINE_ID" ]]; then
    must POST /v2/users/new "$(jq -nc --arg u "$username" --arg o "$org" '{
      username:$u, organizationId:$o,
      machine:{name:$u, accessTokenType:"ACCESS_TOKEN_TYPE_JWT"} }')"
    MACHINE_ID="$(jqr '.id')" # CreateUserResponse liefert "id", AddHumanUser liefert "userId"
    log "Maschinenbenutzer $username angelegt"
  else
    must_upd PATCH "/v2/users/$MACHINE_ID" '{"machine":{"accessTokenType":"ACCESS_TOKEN_TYPE_JWT"}}'
    log "Maschinenbenutzer $username vorhanden, Token-Typ JWT gesetzt"
  fi
}

ensure_machine platform-bot "$PLATFORM_ORG_ID"
PLATFORM_BOT_ID="$MACHINE_ID"
ensure_machine acme-bot "$ACME_ORG_ID"
ACME_BOT_ID="$MACHINE_ID"
ensure_machine platform-admin-bot "$PLATFORM_ORG_ID"
PLATFORM_ADMIN_BOT_ID="$MACHINE_ID"
ensure_machine acme-admin-bot "$ACME_ORG_ID"
ACME_ADMIN_BOT_ID="$MACHINE_ID"
ensure_machine globex-admin-bot "$GLOBEX_ORG_ID"
GLOBEX_ADMIN_BOT_ID="$MACHINE_ID"
ensure_machine initech-bot "$INITECH_ORG_ID"
INITECH_BOT_ID="$MACHINE_ID"

# bot_secret MASCHINEN_ID [ORG_ID] -> BOT_CLIENT_ID, BOT_SECRET (Client Credentials, neu bei jedem Lauf).
# Die v2-API liefert keine Client-ID; v1 PUT /management/v1/users/{id}/secret liefert clientId
# (bei Maschinenbenutzern der Benutzername, nicht die Benutzer-ID) und clientSecret.
# Liegt der Maschinenbenutzer nicht in der Standard-Organisation, wird x-zitadel-orgid gesetzt.
bot_secret() {
  if [[ -n "${2:-}" ]]; then ORG_HDR=(-H "x-zitadel-orgid: $2"); fi
  must PUT "/management/v1/users/$1/secret" '{}'
  ORG_HDR=()
  BOT_CLIENT_ID="$(jqr '.clientId')"
  BOT_SECRET="$(jqr '.clientSecret')"
  [[ -n "$BOT_CLIENT_ID" ]] || die "Client-ID des Bots fehlt"
}
bot_secret "$PLATFORM_BOT_ID"
PLATFORM_BOT_CLIENT_ID="$BOT_CLIENT_ID"; PLATFORM_BOT_SECRET="$BOT_SECRET"
bot_secret "$ACME_BOT_ID" "$ACME_ORG_ID"
ACME_BOT_CLIENT_ID="$BOT_CLIENT_ID"; ACME_BOT_SECRET="$BOT_SECRET"
bot_secret "$PLATFORM_ADMIN_BOT_ID"
PLATFORM_ADMIN_BOT_CLIENT_ID="$BOT_CLIENT_ID"; PLATFORM_ADMIN_BOT_SECRET="$BOT_SECRET"
bot_secret "$ACME_ADMIN_BOT_ID" "$ACME_ORG_ID"
ACME_ADMIN_BOT_CLIENT_ID="$BOT_CLIENT_ID"; ACME_ADMIN_BOT_SECRET="$BOT_SECRET"
bot_secret "$GLOBEX_ADMIN_BOT_ID" "$GLOBEX_ORG_ID"
GLOBEX_ADMIN_BOT_CLIENT_ID="$BOT_CLIENT_ID"; GLOBEX_ADMIN_BOT_SECRET="$BOT_SECRET"
bot_secret "$INITECH_BOT_ID" "$INITECH_ORG_ID"
INITECH_BOT_CLIENT_ID="$BOT_CLIENT_ID"; INITECH_BOT_SECRET="$BOT_SECRET"

# --- Berechtigungen (Authorizations) ---
# grant NUTZER_ID ORG_ID ROLLEN_JSON
grant() {
  local body
  body="$(jq -nc --arg u "$1" --arg p "$PROJECT_ID" --arg o "$2" --argjson r "$3" \
    '{userId:$u, projectId:$p, organizationId:$o, roleKeys:$r}')"
  if api_call POST /zitadel.authorization.v2.AuthorizationService/CreateAuthorization "$body"; then
    log "Berechtigung $3 für Nutzer $1 angelegt"
  elif [[ "$HTTP_CODE" == "409" ]]; then
    log "Berechtigung $3 für Nutzer $1 besteht bereits"
  else
    die "CreateAuthorization -> HTTP $HTTP_CODE: $RESP"
  fi
}
grant "$ADMIN_ID" "$PLATFORM_ORG_ID" '["platform-admin"]'
grant "$EDITOR_ID" "$ACME_ORG_ID" '["tenant-editor"]'
grant "$PLATFORM_BOT_ID" "$PLATFORM_ORG_ID" '["tenant-admin"]'
grant "$ACME_BOT_ID" "$ACME_ORG_ID" '["tenant-viewer"]'
grant "$PLATFORM_ADMIN_BOT_ID" "$PLATFORM_ORG_ID" '["platform-admin"]'
grant "$ACME_ADMIN_BOT_ID" "$ACME_ORG_ID" '["tenant-admin"]'
grant "$GLOBEX_ADMIN_BOT_ID" "$GLOBEX_ORG_ID" '["tenant-admin"]'
# initech-bot: bewusst ohne Berechtigung (Negativtest)

# --- Ausgabe-Dateien (Geheimnisse nur hier, chmod 600) ---
# write_env DATEI: liest KEY=VALUE-Zeilen von stdin
write_env() {
  local file="$1" tmp
  tmp="$(mktemp "$GEN/.tmp.XXXXXX")"
  cat > "$tmp"
  chmod 600 "$tmp"
  mv "$tmp" "$file"
}

ISSUER="$(curl -fsS -m 10 "$BASE/.well-known/openid-configuration" | jq -r '.issuer')"
[[ -n "$ISSUER" && "$ISSUER" != null ]] || die "issuer nicht in der Discovery gefunden"

write_env "$GEN/ids.env" <<IDS
ACME_ORG_ID=$ACME_ORG_ID
GLOBEX_ORG_ID=$GLOBEX_ORG_ID
INITECH_ORG_ID=$INITECH_ORG_ID
PLATFORM_ORG_ID=$PLATFORM_ORG_ID
PROJECT_ID=$PROJECT_ID
SPA_CLIENT_ID=$SPA_CLIENT_ID
IDS

write_env "$GEN/bots.env" <<BOTS
PLATFORM_BOT_CLIENT_ID=$PLATFORM_BOT_CLIENT_ID
PLATFORM_BOT_CLIENT_SECRET=$PLATFORM_BOT_SECRET
ACME_BOT_CLIENT_ID=$ACME_BOT_CLIENT_ID
ACME_BOT_CLIENT_SECRET=$ACME_BOT_SECRET
PLATFORM_ADMIN_BOT_CLIENT_ID=$PLATFORM_ADMIN_BOT_CLIENT_ID
PLATFORM_ADMIN_BOT_CLIENT_SECRET=$PLATFORM_ADMIN_BOT_SECRET
ACME_ADMIN_BOT_CLIENT_ID=$ACME_ADMIN_BOT_CLIENT_ID
ACME_ADMIN_BOT_CLIENT_SECRET=$ACME_ADMIN_BOT_SECRET
GLOBEX_ADMIN_BOT_CLIENT_ID=$GLOBEX_ADMIN_BOT_CLIENT_ID
GLOBEX_ADMIN_BOT_CLIENT_SECRET=$GLOBEX_ADMIN_BOT_SECRET
INITECH_BOT_CLIENT_ID=$INITECH_BOT_CLIENT_ID
INITECH_BOT_CLIENT_SECRET=$INITECH_BOT_SECRET
BOTS

write_env "$GEN/users.env" <<USERS
ADMIN_EMAIL=$ADMIN_EMAIL
ADMIN_PASSWORD=$ADMIN_PW
EDITOR_EMAIL=$EDITOR_EMAIL
EDITOR_PASSWORD=$EDITOR_PW
USERS

write_env "$GEN/pulsequorum.env" <<APP
AUTH_MODE=oidc
OIDC_PROFILE=zitadel
OIDC_TENANCY=multi
OIDC_ISSUER=$ISSUER
OIDC_AUDIENCE=$PROJECT_ID
OIDC_CLIENT_IDS=$SPA_CLIENT_ID,$PLATFORM_BOT_CLIENT_ID,$ACME_BOT_CLIENT_ID,$PLATFORM_ADMIN_BOT_CLIENT_ID,$ACME_ADMIN_BOT_CLIENT_ID,$GLOBEX_ADMIN_BOT_CLIENT_ID,$INITECH_BOT_CLIENT_ID
ZITADEL_PLATFORM_ORG_ID=$PLATFORM_ORG_ID
OIDC_INSECURE_HTTP=true
OIDC_MAX_TOKEN_TTL=1h
APP

log "fertig. Issuer: $ISSUER"
log "Konsole: $BASE/ui/console (Benutzer zitadel-admin, Passwort: DEV_ADMIN_PASSWORD in $COMPOSE_ENV)"
log "Umgebung für Pulsequorum: $GEN/pulsequorum.env"
log "Bot-Tokens: ./token.sh platform-bot|acme-bot|platform-admin-bot|acme-admin-bot|globex-admin-bot|initech-bot"
