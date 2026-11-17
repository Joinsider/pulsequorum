# Lokale OIDC-Testumgebungen

Lokale Identity-Provider für die manuelle Ende-zu-Ende-Prüfung der OIDC-Anmeldung von UptimeCluster (siehe [docs/betrieb/oidc.md](../../docs/betrieb/oidc.md)).

> **Nur Entwicklung.** Die Instanzen laufen ohne TLS im `start-dev`-Modus, binden nur an `127.0.0.1` und enthalten feste Testrollen. Sie sind nicht für den Produktivbetrieb gedacht.

## Keycloak

Verzeichnis: `keycloak/`. Image: `quay.io/keycloak/keycloak:26.8.2`. Die Instanz lauscht auf `http://localhost:8081`, Realm `uptime`.

### Voraussetzungen

- Podman (rootless, unter macOS mit `podman machine`), `podman compose` (delegiert an `podman-compose`), `curl`, `openssl`, `python3`.

### Verwendung

```bash
cd deploy/oidc-dev/keycloak
./setup.sh                     # starten, Testnutzer anlegen, Cluster-Umgebung schreiben (idempotent)
./token.sh admin@example.test  # Access-Token auf stdout (Passwort aus .generated/credentials.env)
./down.sh                      # Container und Volumes entfernen
```

Die Skripte erzeugen `.generated/` (Verzeichnis `chmod 700`) mit:

- `credentials.env` (`chmod 600`): Admin-Zugang Keycloak und Passwörter der Testnutzer. Wird nur beim ersten Lauf erzeugt und danach nicht überschrieben. Die Datei ist per `.gitignore` ausgeschlossen.
- `uptimecluster.env`: Umgebungsvariablen für den Cluster im OIDC-Modus. `OIDC_ISSUER` entspricht exakt dem `issuer` aus der Discovery-Datei.

Die Passwörter werden nie ausgegeben.

### Testnutzer

| Nutzer | Realm-Rolle | Erwartung im Cluster |
| --- | --- | --- |
| `admin@example.test` | `platform-admin` | Vollzugriff |
| `editor@example.test` | `tenant-editor` | Bearbeitung im Mandanten |
| `nobody@example.test` | `staff` | Token wird ausgestellt, der Cluster lehnt die Anfrage mit `403` ab (keine gültige Rolle) |

### Clients

- `uptimecluster-spa`: öffentlicher Client, nur Authorization Code mit PKCE (`S256`). Redirect `http://localhost:8080/auth/callback`, Post-Logout `http://localhost:8080/login`, Web Origin `http://localhost:8080`. `offline_access` ist als optionaler Scope freigegeben.
- `uptimecluster-cli`: öffentlicher Client mit Direct Access Grants (Password Grant). Er dient nur dem Abruf von Tokens in Entwicklungsskripten (`token.sh`).
- Beide Clients enthalten einen Audience-Mapper, der `aud` = `uptimecluster-api` setzt.
- Access-Token-Laufzeit im Realm: 900 s.

Nutzer sind nicht in `realm.json` enthalten; `setup.sh` legt sie über die Admin-REST-API an.

### Hinweise

- `realm.json` wird nur beim ersten Start eines leeren Containers importiert. Änderungen daran wirken erst nach `./down.sh` und `./setup.sh`.
- Die Issuer-URL ist `http://localhost:8081/realms/uptime`. Der Cluster braucht `OIDC_INSECURE_HTTP=true`, dafür setzt `setup.sh` die Variable in `uptimecluster.env`.
- Im Cluster lädt man die Variablen mit `set -a; source deploy/oidc-dev/keycloak/.generated/uptimecluster.env; set +a`.

## Zitadel

Verzeichnis: `zitadel/`. Images (gepinnt): `ghcr.io/zitadel/zitadel:v4.20.3` (letztes stabiles v4-Release), `ghcr.io/zitadel/zitadel-login:v4.20.3` (Login V2) und `postgres:17.12-bookworm`. Zitadel läuft unter `http://localhost:8082` (`ExternalDomain` `localhost`, `ExternalPort` 8082, kein TLS), die Konsole unter `http://localhost:8082/ui/console`.

Login V2 ist in Zitadel v4 Pflicht und liefert die Anmeldeseite nicht selbst aus. Deshalb läuft es als eigener Container auf `http://localhost:3000/ui/v2/login/`. Es gibt keinen Reverse-Proxy, daher liegt die Anmeldung auf Port 3000 und nicht unter `/ui/v2/login` auf Port 8082. Die Basis-URL ist in `compose.yaml` gesetzt.

### Voraussetzungen

- Podman (rootless, unter macOS mit `podman machine`), `podman-compose`, `curl`, `jq`, `openssl`.

### Verwendung

```bash
cd deploy/oidc-dev/zitadel
./setup.sh                 # starten, provisionieren, Umgebung schreiben (idempotent)
./token.sh platform-bot    # Access-Token des Bots (Client Credentials) auf stdout
./token.sh acme-bot
./down.sh                  # Container und Volumes entfernen; .generated/ bleibt
```

`setup.sh` legt in `.generated/` (Verzeichnis `chmod 700`, Dateien `chmod 600`) an:

- `compose.env`: Masterkey, Datenbank-, Admin- und Cookie-Geheimnis. Wird nur beim ersten Lauf erzeugt. Der Masterkey muss zur Datenbank passen, deshalb nicht von Hand ändern.
- `bootstrap/`: PAT des IAM-Admins (`iam-admin.pat`) und Login-Client-PAT (`login-client.pat`), geschrieben vom Zitadel-Start.
- `ids.env`: `PROJECT_ID`, `SPA_CLIENT_ID`, `PLATFORM_ORG_ID`, `ACME_ORG_ID`.
- `bots.env`: Client-IDs und Client-Secrets der Bots. Bei jedem Lauf neu erzeugt.
- `users.env`: Passwörter der Testnutzer. Bei jedem Lauf neu gesetzt.
- `uptimecluster.env`: Umgebung für den Cluster (`AUTH_MODE=oidc`, `OIDC_PROFILE=zitadel`, `OIDC_TENANCY=multi`, Issuer, Audience, Client-IDs, Plattform-Organisation).

Passwörter und Secrets werden nie ausgegeben. Im Cluster lädt man die Variablen mit `set -a; source deploy/oidc-dev/zitadel/.generated/uptimecluster.env; set +a`.

### Aufbau

| Objekt | Wert |
| --- | --- |
| Plattform-Organisation | `platform` (Standard-Organisation der Instanz) |
| Zweite Organisation | `acme` (Kunde) |
| Projekt | `uptimecluster`, „Assert Roles on Authentication“ aktiv |
| Projektrollen | `platform-admin`, `tenant-admin`, `tenant-editor`, `tenant-viewer` |
| Projekt-Grant an `acme` | `tenant-admin`, `tenant-editor`, `tenant-viewer` (ohne `platform-admin`) |
| Anwendung `uptimecluster-spa` | öffentlich (`USER_AGENT`), `AUTH_METHOD_NONE` (PKCE), Code-Flow mit Refresh-Token, JWT-Access-Token mit Rollen-Assertion, Dev-Modus. Redirect `http://localhost:8080/auth/callback`, Post-Logout `http://localhost:8080/login` |
| Instanz | Access-Token-Laufzeit 900 s (15 min) |

Testnutzer und Bots:

| Name | Organisation | Berechtigung |
| --- | --- | --- |
| `admin@platform.example.test` | `platform` | `platform-admin` |
| `editor@acme.example.test` | `acme` | `tenant-editor` |
| `platform-bot` (Maschinenbenutzer) | `platform` | `tenant-admin` |
| `acme-bot` (Maschinenbenutzer) | `acme` | `tenant-viewer` |

Zusätzlich gibt es den Admin `zitadel-admin` in `platform` (Passwort: `DEV_ADMIN_PASSWORD` in `compose.env`). Er dient nur der Konsole.

### Hinweise

- Zitadel verwendet für Maschinenbenutzer als Client-ID den Benutzernamen (`platform-bot`, `acme-bot`), nicht die Benutzer-ID. Ihre Tokens enthalten **kein** `azp`, sondern nur `client_id`. Der Cluster prüft `azp`. Mit `OIDC_REQUIRE_AZP=true` (Standard) lehnt er diese Tokens deshalb ab. Für die Bot-Tokens muss `OIDC_REQUIRE_AZP=false` gesetzt werden. Das schwächt die Prüfung für alle Tokens ohne `azp` ab.
- `setup.sh` ist idempotent und lässt Ressourcen bestehen. Die Passwörter und Bot-Secrets werden jedes Mal neu erzeugt.
- Nach `./down.sh` ist die Datenbank leer. Ein neues `./setup.sh` legt die Instanz neu an und überschreibt die Bootstrap-Dateien.
- Nur Entwicklung: HTTP, Loopback-Ports, Passwörter in Dateien unter `.generated/`.
