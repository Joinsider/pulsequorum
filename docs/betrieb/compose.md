# Single-Node-Betrieb mit Docker Compose

Dieses Dokument beschreibt die Inbetriebnahme eines einzelnen Voters mit lokalen Benutzern über [deploy/compose.single.yaml](../../deploy/compose.single.yaml) (Pflichtenheft NFA-USE-01: Inbetriebnahme in unter 5 Minuten). Die Konfiguration entspricht `internal/config/config.go`; Standardwerte und Bedeutung der Variablen stehen in der Tabelle unten.

## Voraussetzungen

- Docker Compose v2 oder `podman-compose` (siehe Abschnitt Podman)
- Ein Host mit Zugriff auf Port 8080 (TCP)
- Für ICMP-Monitore: `net.ipv4.ping_group_range` im Container erlaubt (ist in der Compose-Datei gesetzt, siehe [icmp.md](icmp.md))
- Ausgehender Netzwerkzugriff zu den überwachten Zielen

## Schnellstart

1. Repository holen und das Image starten (Build aus dem Quellcode; ein fertiges Image gibt es unter `ghcr.io/joinsider/uptimecluster:latest`):

   ```bash
   docker compose -f deploy/compose.single.yaml up -d --build
   ```

2. Das Einmal-Setup-Token aus dem Log holen:

   ```bash
   docker compose -f deploy/compose.single.yaml logs uptime | grep setup_token
   ```

   Die Zeile lautet im Textformat `no user exists yet: create the first admin within 1 hour via POST /api/v1/auth/setup (or the web UI) with this one-time token` mit dem Feld `setup_token=…`. Bei `LOG_FORMAT=json` (Standard) steht dort `"setup_token":"…"`.

3. Im Browser `http://localhost:8080/setup` öffnen, Token, Benutzername und Passwort eingeben. Der erste Benutzer ist Plattform-Administrator. Danach ist das Token verbraucht, Setup ist nicht mehr möglich.

4. Ersten Monitor anlegen (Monitor-Editor, z. B. Typ HTTP) und die Prüfung über die Testfunktion bzw. den Live-Status kontrollieren.

5. Benachrichtigungskanal anlegen. Der Typ `shoutrrr` nimmt eine Shoutrrr-URL entgegen, z. B. für ntfy `ntfy://ntfy.sh/<thema>` (eigener Server: `ntfy://<host>/<thema>`). Alternativ ist ein Webhook möglich. Kanal dem Monitor zuordnen; über die Kanal-Testfunktion lässt sich die Zustellung prüfen.

## Konfiguration

Alle Variablen werden beim Start gelesen. Ungültige Werte beenden den Start mit einer Fehlermeldung, die alle Fehler auf einmal nennt.

| Variable                               | Standard                              | Bedeutung                                                                                                                                                                                                                                                             |
| -------------------------------------- | ------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `NODE_NAME`                            | Hostname des Containers               | Name des Knotens. In der Compose-Datei gesetzt (`vps1`).                                                                                                                                                                                                              |
| `NODE_ROLE`                            | `voter`                               | `voter` (Raft-Teilnehmer) oder `probe` (nur Prüfungen; noch nicht implementiert).                                                                                                                                                                                     |
| `AUTH_MODE`                            | `local`                               | Anmeldung mit lokalen Benutzern, oder `oidc` für einen externen OIDC-Anbieter (Keycloak, Authentik, Entra ID, Zitadel u. a.). Details: [oidc.md](oidc.md).                                                                                                            |
| `OIDC_*`, `ZITADEL_PLATFORM_ORG_ID`    | leer                                  | Nur mit `AUTH_MODE=oidc` (oder dem Alias `zitadel`): Issuer, Audience, Client-IDs, Rollen-Claim, Mandantenmodus u. a. Vollständige Liste mit Standardwerten: [oidc.md](oidc.md#3-variablen).                                                                          |
| `DATA_DIR`                             | `/data`                               | Datenverzeichnis (Volume). Das Image setzt den Wert bereits.                                                                                                                                                                                                          |
| `HTTP_ADDR`                            | `:8080`                               | Listen-Adresse für API und Oberfläche.                                                                                                                                                                                                                                |
| `CLUSTER_ADDR`                         | `:9443`                               | Listen-Adresse für die Cluster-Kommunikation (nur mit weiteren Votern nötig).                                                                                                                                                                                         |
| `CLUSTER_ADVERTISE`                    | `<NODE_NAME>:<Port aus CLUSTER_ADDR>` | Unter dieser Adresse erreichen andere Knoten diesen Knoten.                                                                                                                                                                                                           |
| `RAFT_TIMEOUT`                         | `1s`                                  | Heartbeat- und Election-Timeout von Raft; Minimum `100ms`.                                                                                                                                                                                                            |
| `PUBLIC_URL`                           | leer                                  | Öffentliche URL, z. B. `https://mon1.example.com` (nur Schema und Host, kein Pfad). Wird auch als CORS-Ursprung erlaubt.                                                                                                                                              |
| `CORS_ORIGINS`                         | leer                                  | Weitere Ursprünge externer Frontends, durch Komma getrennt; jeweils nur `scheme://host[:port]`.                                                                                                                                                                       |
| `MASTER_KEY`                           | leer                                  | Master-Schlüssel (Base64) für die Verschlüsselung von Geheimnissen. Nicht zusammen mit `MASTER_KEY_FILE`.                                                                                                                                                             |
| `MASTER_KEY_FILE`                      | leer                                  | Pfad zu einer Datei mit dem Master-Schlüssel (Base64 oder 32 Rohbytes). Empfohlen, siehe Datenhaltung.                                                                                                                                                                |
| `ICMP_MODE`                            | `auto`                                | `auto`, `privileged`, `unprivileged` oder `off`. Details: [icmp.md](icmp.md).                                                                                                                                                                                         |
| `SSRF_PROTECTION`                      | `auto`                                | `auto`, `on` oder `off`. `auto` ist im lokalen Modus **aus**, im Zitadel-Modus an. `on` sperrt Prüfziele und Benachrichtigungsziele in privaten Netzen. Details: [ssrf.md](ssrf.md).                                                                                  |
| `RAW_COMPACTION`                       | `true`                                | Verdichtet Heartbeats, die älter als 48 h sind.                                                                                                                                                                                                                       |
| `RETENTION_RAW_DAYS`                   | `30`                                  | Aufbewahrung der Rohdaten in Tagen; Minimum `3`.                                                                                                                                                                                                                      |
| `RETENTION_HOURLY_DAYS`                | `365`                                 | Aufbewahrung der Stundenaggregate in Tagen; Minimum `1`.                                                                                                                                                                                                              |
| `RETENTION_DAILY_DAYS`                 | `0`                                   | Aufbewahrung der Tagesaggregate in Tagen; `0` = unbegrenzt.                                                                                                                                                                                                           |
| `AUTH_COOKIE_DOMAIN`                   | leer                                  | `Domain` des Refresh-Cookies. Leer = nur für den aufgerufenen Host. Nur bei Subdomain-Setups nötig.                                                                                                                                                                   |
| `ALERT_MAX_ATTEMPTS`                   | `10`                                  | Zustellversuche je Alarm und Kanal, von 1 bis 100.                                                                                                                                                                                                                    |
| `ALERT_BUNDLE_WINDOW`                  | `30s`                                 | Bündelungsfenster je Shoutrrr-Kanal: Alarme, die in diesem Fenster fällig werden, gehen als eine Nachricht raus. `0` schaltet die Bündelung aus. Webhooks werden nie gebündelt.                                                                                       |
| `SELF_CHECK_TARGETS`                   | leer                                  | Referenzziele der Selbstprüfung des Nodes, kommagetrennt: `host:port` (TCP) oder `https://…` (HTTP-HEAD; jede Antwort zählt). Schlagen alle fehl, gilt der Node als gestört, sofern die anderen Nodes gesund sind. Empfehlung: 2 bis 3 unabhängige Ziele. Leer = aus. |
| `SELF_HEALTH_FAIL_RATIO`               | `0.5`                                 | Anteil fehlgeschlagener Checks, ab dem ein Node als gestört gilt (Wert über 0 bis 1).                                                                                                                                                                                 |
| `DEGRADED_AFTER`                       | `15s`                                 | Dauer ohne bekannten Leader, ab der ein Voter ohne Quorum unbestätigt alarmiert. `0` schaltet das aus.                                                                                                                                                                |
| `LOG_LEVEL`                            | `info`                                | `debug`, `info`, `warn` oder `error`.                                                                                                                                                                                                                                 |
| `LOG_FORMAT`                           | `json`                                | `json` oder `text`.                                                                                                                                                                                                                                                   |
| `SHUTDOWN_TIMEOUT`                     | `10s`                                 | Wartezeit beim Herunterfahren für laufende Hintergrundarbeit.                                                                                                                                                                                                         |
| `METRICS_ADDR`                         | leer                                  | Listen-Adresse `host:port` für Prometheus unter `/metrics`, eigener Port, nur diese Route. Leer = aus.                                                                                                                                                                |
| `METRICS_TOKEN` / `METRICS_TOKEN_FILE` | leer                                  | Bearer-Token für `/metrics`. Nicht zusammen setzen. Ohne Token nur über das Netzwerk absichern.                                                                                                                                                                       |

Die Zeitzone ist nicht relevant: Intern wird UTC verwendet, `TZ` muss nicht gesetzt werden. Wartungsfenster mit eigener Zeitzone werden unabhängig davon ausgewertet.

Die Compose-Datei enthält alle wichtigen optionalen Variablen auskommentiert. Zum Aktivieren die Kommentarzeichen entfernen und `docker compose … up -d` erneut ausführen.

## Datenhaltung

Alle Daten liegen im Volume `uptime-data`, das auf `/data` eingebunden ist:

- `state.db`: Zustand des Clusters (Monitore, Benutzer, Kanäle, Wartungsfenster, verschlüsselte Geheimnisse). Die Schema-Migrationen laufen beim Start automatisch.
- `metrics.db`: Heartbeats und Verfügbarkeitsaggregate.
- `master.key`: Master-Schlüssel, wenn weder `MASTER_KEY` noch `MASTER_KEY_FILE` gesetzt ist. Er wird beim ersten Start mit Rechten 0600 erzeugt (ADR 0011).

**Sicherung:** Das Volume regelmäßig sichern, `master.key` eingeschlossen. Geht der Master-Schlüssel verloren, sind die gespeicherten Geheimnisse (Kanal-URLs, Webhook-Header) nicht mehr lesbar; eine Wiederherstellung ist dann nicht möglich. Eine Sicherung ohne den dazugehörigen Schlüssel hilft für Geheimnisse daher nicht.

**Empfehlung: externer Master-Schlüssel.** Liegt der Schlüssel nicht im Volume, geben gestohlene Volume- oder Datenbank-Sicherungen allein keine Geheimnisse preis (ADR 0011, Option A). Dafür als Compose-Secret einbinden:

```bash
mkdir -p secrets
openssl rand -base64 32 > secrets/master.key
chmod 600 secrets/master.key
```

In `deploy/compose.single.yaml` (oder einer eigenen Override-Datei) die auskommentierten Abschnitte aktivieren:

```yaml
services:
  uptime:
    environment:
      MASTER_KEY_FILE: /run/secrets/master_key
    secrets:
      - master_key
secrets:
  master_key:
    file: ./secrets/master.key
```

Der Schlüssel muss außerhalb des Repositorys aufbewahrt und gesichert werden. Ein Wechsel auf einen anderen Schlüssel, nachdem Daten existieren, führt beim Start zu einem Fehler (der Fingerabdruck des Schlüssels wird im Zustand gespeichert); der bisherige Schlüssel muss dann wieder verwendet werden.

## Hinter einem Reverse Proxy

Für den Betrieb mit HTTPS wird ein Reverse Proxy davorgeschaltet. Dabei gilt:

- `PUBLIC_URL` auf die öffentliche Adresse setzen, z. B. `PUBLIC_URL: https://mon1.example.com`.
- Der Port 8080 sollte dann nur lokal erreichbar sein: `"127.0.0.1:8080:8080"` in der Compose-Datei.
- Der Refresh-Token liegt in einem Cookie mit den Attributen `HttpOnly`, `Secure` und `SameSite=Strict` (Pfad `/api/v1/auth`). Browser senden `Secure`-Cookies nur über HTTPS oder auf `localhost`. Ohne HTTPS bleibt man nach einem Neuladen der Seite nicht angemeldet.
- Der Live-Status nutzt eine WebSocket-Verbindung unter `/api/v1/stream`. Der Proxy muss den Upgrade-Header durchreichen.

Beispiel für Caddy (übernimmt HTTPS und WebSocket-Upgrade automatisch):

```caddyfile
mon1.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

Beispiel für nginx (mit HTTPS-Konfiguration und Zertifikaten, hier nur der relevante Teil):

```nginx
map $http_upgrade $connection_upgrade {
    default upgrade;
    ""      close;
}

server {
    listen 443 ssl;
    server_name mon1.example.com;
    # ssl_certificate / ssl_certificate_key hier eintragen

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_read_timeout 3600s;
    }
}
```

Die Anwendung wertet `X-Forwarded-*`-Header nicht aus. Die Client-IP für Drosselung ist daher die TCP-Adresse des Proxys.

## Update

```bash
docker compose -f deploy/compose.single.yaml pull
docker compose -f deploy/compose.single.yaml up -d --build
```

Mit einem selbst gebauten Image entfällt `pull`; `--build` baut dann neu. Die Daten bleiben im Volume erhalten; Schema-Migrationen laufen beim Start automatisch. Vor einem Update empfiehlt sich eine Sicherung des Volumes.

## Podman

`podman-compose` liest dieselbe Datei:

```bash
podman-compose -f deploy/compose.single.yaml up -d --build
```

- Die Ports 8080 und 9443 sind im rootless-Betrieb unproblematisch. Ports unter 1024 benötigen `net.ipv4.ip_unprivileged_port_start`.
- Podman baut standardmäßig OCI-Images, die das `HEALTHCHECK` aus dem Dockerfile verwerfen. Die Compose-Datei setzt deshalb einen eigenen `healthcheck` auf Dienstebene; er funktioniert in Docker und Podman gleich.
- `net.ipv4.ping_group_range` setzt Podman standardmäßig auf `0 0`. Die Angabe in der Compose-Datei hebt das auf; Details und ein `podman run`-Beispiel stehen in [icmp.md](icmp.md). Ob die Einstellung im rootless-Betrieb überall greift, ist noch nicht praktisch verifiziert (AP-1.5).

## Fehlerbehebung

**Setup-Token abgelaufen oder ungültig.** Das Token ist eine Stunde gültig, solange noch kein Benutzer existiert. Danach ist es ungültig. Ein Neustart erzeugt ein neues Token, das im Log erscheint:

```bash
docker compose -f deploy/compose.single.yaml restart uptime
docker compose -f deploy/compose.single.yaml logs uptime | grep setup_token
```

**Kein Token im Log.** Es wird nur erzeugt, solange kein Benutzer existiert. Wurde der Admin bereits angelegt, ist das erwartet.

**Status prüfen.** `GET /healthz` antwortet, sobald der Prozess läuft (Liveness). `GET /readyz` antwortet mit `200` erst, wenn der Knoten bereit ist, und sonst mit `503` und einem Grund:

```bash
curl -s http://localhost:8080/healthz
curl -s -w '\n%{http_code}\n' http://localhost:8080/readyz
```

Der Container-Healthcheck ruft denselben lokalen Liveness-Endpunkt über `/uptimecluster healthcheck` auf. Ein Status `unhealthy` deutet auf einen nicht startenden Prozess hin; die Ursache steht im Log.

**Fehlermeldung beim Start (`invalid configuration`).** Die Meldung nennt jede ungültige Variable mit dem erwarteten Wert. Die Tabelle oben zeigt die zulässigen Werte.

**Geheimnisse nicht mehr lesbar.** Der Master-Schlüssel passt nicht zu den gespeicherten Daten. Den ursprünglichen Schlüssel aus der Sicherung wiederherstellen (siehe Datenhaltung).

**ICMP-Monitore melden DOWN.** Siehe [icmp.md](icmp.md), Abschnitt „Prüfen“.
