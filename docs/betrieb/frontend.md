# Externes Frontend und Client-Failover

Dieses Dokument beschreibt, wie die Oberfläche (SPA) getrennt von den Votern ausgeliefert wird und wie der Browser bei Ausfall eines Voters auf einen anderen wechselt. Grundlage sind [ADR 0036](../adr/0036-externes-frontend-und-client-failover.md) (FA-UI-02, FA-UI-03), [ADR 0008](../adr/0008-lokale-authentifizierung.md) (Cookies, Refresh) und [ADR 0027](../adr/0027-oidc-anbindung.md) (OIDC). Die Variablen entsprechen `internal/frontendcfg/frontendcfg.go` und `internal/config/config.go`.

Verwandte Dokumente: [compose.md](compose.md) (Einzelknoten), [cluster.md](cluster.md) (Voter, Update, Befehlsstufen), [oidc.md](oidc.md) (Anbieter und Redirect-URIs), [release.md](release.md) (Artefakte).

## 1. Überblick

Es gibt zwei Wege, die SPA auszuliefern:

- **Eingebettet:** Jeder Voter liefert die SPA selbst aus (`internal/webui`). Die SPA ist also unter der Adresse jedes Voters erreichbar.
- **Extern:** Die SPA läuft auf einem eigenen Host (Container, CDN oder statischer Webserver). Die Liste der Voter steht in ihrer eigenen `config.json`. Damit legt der Betreiber fest, welche Voter die SPA überhaupt ansprechen darf.

**Was der Client-Failover leistet:**

- Beim Start prüft die SPA alle konfigurierten Voter parallel über `GET /api/v1/health` (Timeout 2 s) und wählt den schnellsten gesunden Node. Gesund heißt: HTTP 200, nicht `degraded` und nicht `stale`.
- Bei einem Netzwerkfehler, einem Timeout oder den Antworten 502, 503 und 504 sperrt die SPA den Node für 30 s. Jede weitere Sperre verdoppelt die Dauer bis höchstens 5 min, mit ±20 % Jitter. Danach wechselt sie auf den nächsten gesunden Node.
- Nodes, deren Antwortzeit höchstens 20 % über der des schnellsten liegt, gelten als gleich schnell. Dann entscheidet die Reihenfolge der Liste.
- Alle 60 s prüft sie bei sichtbarem Tab erneut. Sie wechselt dabei nur, wenn der aktuelle Node ungesund ist. Auf einen schnelleren Node wechselt sie nicht von selbst, nur nach einem Fehler (ADR 0036, 2B).
- Sie wiederholt nur Anfragen, die sicher sind (Abschnitt 7).
- Die Node-Liste lädt sie nach der Auswahl und alle 5 min von `GET /api/v1/cluster/nodes` nach. Das Nachladen kann die Liste nur einengen (Abschnitt 7).

**Was der Client-Failover nicht leistet:**

- **Kein Proxy.** Der Browser spricht die Voter direkt an. Es gibt keine Lastverteilung je Anfrage.
- **Keine Garantie für frische Lesezugriffe.** Ein Follower kann beim Lesen kurz hinter dem Leader liegen. Der Health-Check meldet `stale: true`, wenn der letzte Kontakt zum Leader länger als 10 s her ist, der Voter keinen Leader kennt oder mehr als 1 000 committete Einträge noch nicht angewendet hat (`internal/cluster/stale.go`). Die SPA meidet solche Nodes. Das mildert das Problem, verhindert es aber nicht.
- **Keine stille Wiederholung unsicherer Schreibzugriffe** und **keine Wiederholung der Anmeldung** (Abschnitt 7).
- **Keine Bindung der Anmeldung an einen Node.** Das Access-Token gilt auf allen Votern. Der Refresh-Cookie (Abschnitt 5) bestimmt, ob ein Wechsel eine Sitzung überlebt.
- **WebSocket-Verbindungen** bleiben an den Node gebunden, auf dem das Ticket ausgestellt wurde. Ein Ticket gilt nur dort.

## 2. Voraussetzungen am Cluster

### 2.1 PUBLIC_URL je Voter

Jeder Voter setzt `PUBLIC_URL` auf die Adresse, unter der Browser ihn erreichen, zum Beispiel `https://n1.example.com`. Der Voter übernimmt diese Adresse ins Node-Register, und die anderen Voter nehmen sie in ihre Failover-Liste auf.

Regeln (`internal/publicorigin/publicorigin.go`, dieselben Regeln gelten für `FRONTEND_NODES`):

- absolute URL mit Schema `https`, oder `http` nur für `localhost`, `127.0.0.1` und `[::1]`;
- kein Benutzername, kein Pfad (ein einzelnes `/` ist erlaubt), keine Query, kein Fragment, auch nicht leer (`?` oder `#`);
- Port 1 bis 65535, höchstens 256 Zeichen, nur druckbares ASCII ohne Leerzeichen.

Gespeichert wird der normalisierte Origin: Kleinbuchstaben, Standardport entfernt. Beispiel: `HTTPS://N1.example.com:443/` wird zu `https://n1.example.com`.

Erfüllt die Adresse die Regeln nicht, startet der Voter trotzdem. Er schreibt die Warnung `PUBLIC_URL is not a plain origin; client failover will not list this voter` ins Log (`internal/clusterhost/clusterhost.go`). Das Register nimmt die Adresse nicht auf. Der Voter fehlt dann in der Failover-Liste der anderen Voter und in der Liste eines externen Frontends, das seine Liste nachlädt.

Ohne gültige eigene `PUBLIC_URL` liefert ein Voter außerdem eine leere Node-Liste in seiner `config.json`. Seine eingebettete SPA bleibt dann beim Origin der Seite und wechselt nicht (ADR 0036, 1B). Die CORS-Freigabe für die anderen Voter gilt trotzdem.

### 2.2 Befehlsstufe 11

Ein Voter speichert `PUBLIC_URL` erst ab Befehlsstufe 11 im Register (`internal/cluster/nodes.go`). Solange der Cluster darunter liegt, wird die Adresse stillschweigend nicht übernommen. Es gibt dann keine Fehlermeldung.

- Der Leader hebt die Befehlsstufe an, sobald alle Voter eine Version mit Stufe 11 melden. Den Ablauf eines Updates beschreibt [cluster.md](cluster.md).
- Bis dahin funktioniert ein externes Frontend mit der eigenen `config.json` trotzdem. Es fehlt nur das Nachladen der Liste, weil die älteren Voter den Endpunkt `/api/v1/cluster/nodes` nicht haben. Die SPA behält dann die konfigurierte Liste.
- Der Failover zwischen Votern über das Register ist erst nach dem Anheben der Stufe vollständig.

### 2.3 CORS

Ein Voter beantwortet CORS-Anfragen nur für Origins auf seiner Freigabeliste. Diese besteht aus drei Quellen (`internal/server/security.go`, ADR 0036):

- `CORS_ORIGINS` der eigenen Konfiguration,
- der eigene `PUBLIC_URL`,
- die `PUBLIC_URL` aller anderen Voter aus dem Register. Diese Freigabe läuft automatisch, die Voter müssen sie nicht selbst pflegen.

Für externe Frontends gilt:

- **`CORS_ORIGINS` auf jedem Voter** muss den Origin jedes externen Frontends enthalten, also Schema, Host und Port, ohne Pfad. Beispiel: `CORS_ORIGINS=https://uptime.example.com`. Nicht erlaubt sind `*` und `null`.
- Ohne diesen Eintrag schlagen `/api/v1/health` und `/api/v1/cluster/nodes` im Browser fehl. Die Gesundheitsprüfung meldet dann Status 0, und die SPA findet keinen Node.
- Die Freigabe gilt für Bearer-Anfragen. Cookies werden nur auf `/api/v1/auth/*` und `/api/v1/me/password` mitgeschickt (Abschnitt 5).
- Die Änderung von `CORS_ORIGINS` wirkt erst nach einem Neustart des Voters.

## 3. Varianten der Auslieferung

| Variante | Was läuft | Konfiguration | Geeignet für |
| --- | --- | --- | --- |
| Eingebettet im Voter | die SPA des Voters (`internal/webui`) | `PUBLIC_URL` je Voter, Register | Standardbetrieb, ein Host je Voter |
| `pulsequorum frontend` | Unterbefehl im Image `ghcr.io/joinsider/pulsequorum:<version>` (distroless, nonroot) | Umgebungsvariablen | Frontend getrennt von den Votern, ohne eigenen Webserver |
| nginx-Image `pulsequorum-web` | `ghcr.io/joinsider/pulsequorum-web:<version>` (nginx-unprivileged, UID 101, Port 8080) | Umgebungsvariablen, beim Start erzeugt | Container-Betrieb mit Standard-Webserver |
| Statisches Archiv `pulsequorum-web_<version>.tar.gz` | nur Dateien, für CDN oder statischen Host | `config.json` von Hand, `_headers` per Befehl | Hosts ohne Container, zum Beispiel Cloudflare Pages oder Netlify |

Die Images sind mehrarchitektur (`linux/amd64`, `linux/arm64`). Eine lokale Variante des nginx-Images entsteht mit `make docker-web` als `pulsequorum-web:dev`.

### 3.1 pulsequorum frontend

Das Image `ghcr.io/joinsider/pulsequorum` startet mit `/pulsequorum` als Einstiegspunkt. Der Unterbefehl `frontend` liefert die SPA, eine aus den Umgebungsvariablen erzeugte `config.json` und die Sicherheits-Header aus. Er spricht keine API an und hat keinen Raft-Zustand.

```sh
docker run --rm -p 8080:8080 \
  -e FRONTEND_NODES=https://n1.example.com,https://n2.example.com,https://n3.example.com \
  -e AUTH_MODE=local \
  ghcr.io/joinsider/pulsequorum:<version> frontend
```

Der Healthcheck des Images ruft `pulsequorum healthcheck` auf. Der liest `HTTP_ADDR` (Standard `:8080`) und fragt dort `/healthz` ab. Ändern Sie `FRONTEND_LISTEN`, setzen Sie `HTTP_ADDR` auf denselben Wert, sonst meldet Docker den Container als ungesund.

### 3.2 nginx-Image pulsequorum-web

Das Image basiert auf `nginxinc/nginx-unprivileged` (Alpine, digest-gepinnt) und enthält die SPA, den Helfer `pulsequorum-webconfig` und `deploy/web/nginx.conf`. Beim Start schreibt das Skript `10-pulsequorum-webconfig.sh` die `config.json` und die Header-Datei nach `/tmp/pulsequorum`. Ungültige Umgebungsvariablen lassen den Start scheitern.

- `/api/` antwortet mit 404. nginx leitet nie an die Voter weiter.
- `index.html` und `config.json` sind `no-store`, gehashte Bundles `immutable`.
- `/healthz` liefert den Container-Healthcheck.

Das Image ist für ein Root-Dateisystem nur zum Lesen gedacht. Der Container schreibt ausschließlich nach `/tmp`:

```sh
docker run --rm -p 8080:8080 \
  --read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges:true \
  -e FRONTEND_NODES=https://n1.example.com,https://n2.example.com \
  -e AUTH_MODE=local \
  ghcr.io/joinsider/pulsequorum-web:<version>
```

### 3.3 Statisches Archiv

Das Archiv `pulsequorum-web_<version>.tar.gz` (GoReleaser, `.goreleaser.yaml`) enthält die SPA unter einem Verzeichnis `pulsequorum-web/`, dazu `config.example.json`, `LICENSE` und `THIRD_PARTY_NOTICES.md`. Die `config.json` und die Header muss der Betreiber selbst erzeugen:

1. `config.example.json` nach `config.json` kopieren und die Liste `nodes` sowie `auth` anpassen (Abschnitt 4.2).
2. Die Header-Datei mit demselben Befehl und denselben Umgebungsvariablen erzeugen, die auch der Liste entsprechen:

   ```sh
   docker run --rm \
     -e FRONTEND_NODES=https://n1.example.com,https://n2.example.com \
     -e AUTH_MODE=local \
     ghcr.io/joinsider/pulsequorum:<version> frontend headers --format netlify > _headers
   ```

   Die Datei `_headers` gilt für Cloudflare Pages und Netlify. Mit `--format nginx` entsteht stattdessen eine `include`-Datei für nginx.

Die `config.json` und die CSP in `_headers` müssen dieselbe Node-Liste enthalten. Sonst blockiert der Browser entweder den Failover, oder die CSP erlaubt mehr, als die SPA nutzt. Beim Ändern der Liste werden beide Dateien neu erzeugt.

Auf Hosts ohne Header-Unterstützung fehlen die Header. GitHub Pages erlaubt keine eigenen Response-Header. Dort fehlen damit `frame-ancestors` (Schutz vor Einbettung), `X-Frame-Options` und HSTS. Ein `<meta>`-Ersatz für die CSP erzeugt das Projekt nicht. Für Produktivbetrieb ist ein Host mit Header-Unterstützung zu bevorzugen.

## 4. Konfiguration

### 4.1 Umgebungsvariablen

Diese Variablen gelten für `pulsequorum frontend`, `pulsequorum frontend headers` und das nginx-Image. Alle Werte werden beim Start geprüft. Alle Fehler erscheinen gemeinsam, eingeleitet mit `invalid configuration:`.

| Variable | Standard | Prüfregel |
| --- | --- | --- |
| `FRONTEND_NODES` | – (Pflicht) | Kommagetrennte Origins der Voter, mindestens einer. Jeder Eintrag muss die Regeln aus Abschnitt 2.1 erfüllen. Doppelte Einträge entfallen, die Reihenfolge bleibt erhalten. |
| `AUTH_MODE` | `local` | `local` oder `oidc`. `zitadel` gibt es hier nicht. |
| `OIDC_ISSUER` | – | Nur bei `oidc`, Pflicht. Absolute `http(s)`-URL mit Host. Muss exakt dem Claim `iss` entsprechen. |
| `OIDC_CLIENT_ID` | – | Nur bei `oidc`, Pflicht. Client-ID der SPA. Muss in `OIDC_CLIENT_IDS` jedes Voters stehen (Abschnitt 6). |
| `OIDC_SCOPES` | `openid profile offline_access` | Nur bei `oidc`. Komma oder Leerzeichen als Trennzeichen. Muss `openid` enthalten. `offline_access` ist für den Refresh nötig. |
| `FRONTEND_LISTEN` | `:8080` | Listen-Adresse des Frontend-Hosts. |
| `FRONTEND_TLS_CERT` | leer | Zertifikatsdatei. Nur zusammen mit `FRONTEND_TLS_KEY`. |
| `FRONTEND_TLS_KEY` | leer | Schlüsseldatei. Nur zusammen mit `FRONTEND_TLS_CERT`. |
| `FRONTEND_HSTS` | `auto` | `auto` (nur mit TLS), `on` oder `off`. HSTS setzt `max-age=31536000`. |

Bei `AUTH_MODE=local` sind `OIDC_ISSUER`, `OIDC_CLIENT_ID` und `OIDC_SCOPES` verboten, wenn sie gesetzt sind. Die Meldung lautet dann etwa `OIDC_ISSUER: must be empty when AUTH_MODE is local`.

Das nginx-Image erhält die Werte über dieselben Variablen. Der Helfer `pulsequorum-webconfig` beendet sich bei ungültiger Konfiguration mit Code 1, der Container startet dann nicht.

### 4.2 config.json

Die `config.json` ist die Vertrauensgrenze der SPA (Abschnitt 7). Der Frontend-Host erzeugt sie aus den Umgebungsvariablen. Im statischen Archiv schreibt der Betreiber sie selbst. Beispiel nach `web/public/config.example.json`:

```json
{
  "nodes": ["https://n1.example.com", "https://n2.example.com", "https://n3.example.com"],
  "auth": {
    "mode": "oidc",
    "issuer": "https://id.example.com",
    "clientId": "pulsequorum-spa",
    "scopes": ["openid", "profile", "email", "offline_access"]
  }
}
```

- `nodes`: Reihenfolge ist wichtig. Der erste nicht gesperrte Node ist der Rückfall, wenn kein Node die Gesundheitsprüfung besteht. Beim Wechsel nach einem Fehler gilt ebenfalls die Reihenfolge.
- `auth.mode`: `local` oder `oidc`. Bei `local` fehlen `issuer`, `clientId` und `scopes`.
- Ungültige Einträge in `nodes` verwirft die SPA still. Sie zeigt keine Meldung an. Prüfen Sie die Liste daher nach jeder Änderung mit der Gesundheitsprüfung (Abschnitt 9).
- Meldet `/api/v1/cluster/nodes` einen Voter, der nicht in `nodes` steht, ignoriert die SPA ihn und schreibt einmal eine Warnung in die Browser-Konsole.
- Ein Frontend-Host liest die Datei nur beim Start. Nach einer Änderung der Umgebungsvariablen den Container neu starten.

## 5. Domains und Cookies

Das Refresh-Token wird als Cookie gespeichert (`internal/api/auth.go`):

- `HttpOnly; Secure; SameSite=Strict; Path=/api/v1/auth`
- `Domain` aus `AUTH_COOKIE_DOMAIN` (Standard leer: nur für den Host, der das Cookie gesetzt hat).

`SameSite=Strict` bedeutet: Der Browser schickt das Cookie nur bei Anfragen innerhalb derselben **Site**. Zwei Origins sind dieselbe Site, wenn sie dieselbe registrierbare Domain (eTLD+1 nach der Public Suffix List) und dasselbe Schema haben. Ports zählen nicht.

- `status.example.com` und `mon1.example.com` sind same-site.
- `https://uptime.example.com` und `http://uptime.example.com` sind cross-site, weil das Schema abweicht.
- `*.pages.dev`, `*.github.io` und `*.netlify.app` stehen auf der Public Suffix List. Jedes Projekt dort ist eine eigene Site. Ein Frontend dort ist gegenüber den Votern immer cross-site.

**Matrix Host mal Auth-Modus:**

| Lage | `AUTH_MODE=local` | `AUTH_MODE=oidc` |
| --- | --- | --- |
| Eingebettete SPA, Voter unter eigener Adresse | ja. Mit gemeinsamer `AUTH_COOKIE_DOMAIN` Failover ohne Neu-Login | ja |
| Frontend `uptime.example.com`, Voter `n1.uptime.example.com` usw. (same-site) | ja, mit `AUTH_COOKIE_DOMAIN=uptime.example.com` auf jedem Voter | ja |
| Frontend `status.example.com`, Voter `mon1.example.com` (same-site, andere Subdomain) | ja, wie oben | ja |
| Frontend `http://localhost:8080`, Voter `http://localhost:8081` bis `8083` (Entwicklung) | ja, Ports zählen nicht | ja |
| Frontend unter fremder Domain (`*.pages.dev`, `*.netlify.app`, andere Domain) | nein. Das Cookie wird nicht gesendet, die Anmeldung scheitert | ja |
| Frontend `https`, Voter `http` (Schema abweichend) | nein, cross-site | ja |

**Folgen für den Failover im Modus `local`:**

- **Mit gemeinsamer Cookie-Domain** kann jeder Voter den Refresh übernehmen. Ein Wechsel beendet die Sitzung nicht.
- **Ohne gemeinsame Domain** gilt das Cookie nur für den Voter, der es gesetzt hat. Nach einem Wechsel schlägt der Refresh fehl. Die SPA versucht es beim nächsten Anlass erneut, solange das Access-Token noch gültig ist. Danach endet die Sitzung, und der Nutzer muss sich neu anmelden.
- **Verlorene Antwort beim Refresh:** Der Server rotiert das Refresh-Token. Kommt die neue Cookie-Antwort wegen eines Ausfalls nicht an, verwendet der Browser beim nächsten Versuch das alte Token. Der Server wertet das als Wiederverwendung und widerruft die ganze Sitzungsfamilie (FA-AUTH-03). Folge: Neu-Login. Eine Kulanzfrist ist als offene Frage O3 in ADR 0036 vermerkt und nicht umgesetzt.

**Warnung im Log.** Jeder Voter mit `AUTH_MODE=local` prüft beim Start jeden Eintrag in `CORS_ORIGINS` gegen seine eigene `PUBLIC_URL`. Liegt ein Eintrag cross-site, schreibt er die Warnung `CORS origin is cross-site to PUBLIC_URL; local login will not work there`. Die Prüfung fehlt, wenn `PUBLIC_URL` leer ist. Eine Warnung zeigt also an, dass die lokale Anmeldung von diesem Origin nicht funktioniert. Setzen Sie in diesem Fall `AUTH_MODE=oidc` oder ein Frontend unter einer Domain derselben Site.

**Empfehlung.** Verwenden Sie für den Betrieb mit lokaler Anmeldung eine eigene Subdomain für das Frontend und eine gemeinsame Domain für alle Voter, zum Beispiel `uptime.example.com` für die SPA und `n1.uptime.example.com` bis `n3.uptime.example.com` für die Voter. Setzen Sie `AUTH_COOKIE_DOMAIN=uptime.example.com` auf jedem Voter. Das Cookie gilt dann für alle Subdomains, aber nur unter `Path=/api/v1/auth`. Die Angriffsfläche wächst dadurch auf alle Hosts unter dieser Domain.

## 6. OIDC

Ein Access-Token ist an Issuer und Audience gebunden. Alle Voter mit derselben OIDC-Konfiguration akzeptieren es. Den Refresh übernimmt die SPA direkt beim Identity Provider. Der Failover zwischen Votern berührt die Anmeldung daher nicht (ADR 0036, Frage 5). Für externe Frontends gilt außerdem:

- **Redirect-URIs** hängen am **Origin der SPA**, nicht am Voter. Tragen Sie beim IdP für jeden Origin, unter dem die SPA läuft, die Redirect-URI `https://<host>/auth/callback` und die Post-Logout-URI `https://<host>/login` ein (siehe [oidc.md](oidc.md), Abschnitt 2). Ein externes Frontend auf `https://uptime.example.com` braucht genau diese Einträge, auch wenn es die Voter unter anderen Adressen anspricht.
- **Web Origins beim IdP.** Der Token-Endpunkt muss den Origin der SPA für CORS erlauben. Keycloak nennt das „Web Origins“. Bei anderen Anbietern prüfen Sie die Einstellung selbst.
- **Issuer in der CSP.** Die `connect-src`-Richtlinie erhält den Origin von `OIDC_ISSUER` automatisch (`webfront.SPACSP`). Eine manuelle Pflege entfällt.
- **Client-ID auf den Votern.** Die `OIDC_CLIENT_ID` des Frontends muss auf jedem Voter in `OIDC_CLIENT_IDS` stehen, sonst lehnt der Voter das Token ab (siehe [oidc.md](oidc.md)).
- **Scopes.** `offline_access` ist für den Refresh nötig. Ohne diesen Scope läuft die Sitzung nach dem Ablauf des Access-Tokens ab.

## 7. Sicherheit

**Vertrauensgrenze.** Die `config.json` des Betreibers ist die einzige Quelle für die Liste der Voter, die ein externes Frontend anspricht (ADR 0036, Entscheidung 1B). Ein Voter kann die Liste nur einengen, nie erweitern: Die SPA übernimmt aus `/api/v1/cluster/nodes` nur Einträge, die bereits in der eigenen `config.json` stehen (`trustedNodes` in `web/src/app/core/failover/node-origins.ts`). Ein neuer Voter muss deshalb in der `config.json` des Frontends eingetragen werden, bevor die SPA ihn nutzt. Die Liste einer Anfrage ist also nicht vertrauenswürdig.

**Content-Security-Policy.** Die SPA setzt `connect-src 'self'` und erweitert es um genau die Origins der Liste, jeweils als `https://` und `wss://` (bei `http` für Loopback als `http://` und `ws://`), und um den Issuer. Ein Platzhalter `*` gibt es nicht. Der Browser setzt damit dieselbe Grenze durch, die die SPA in JavaScript prüft. Ein manipulierter Voter-Eintrag oder eine manipulierte Antwort von `/api/v1/cluster/nodes` kann so keine Anfragen mit Bearer-Token an Hosts außerhalb der eigenen `config.json` lenken.

**Anmeldedaten.** Cookies werden nur auf `/api/v1/auth/*` und `/api/v1/me/password` mitgeschickt (`credentials: include` nur dort). Bearer-Anfragen laufen ohne Cookies. Die Voter beantworten `Access-Control-Allow-Credentials` nur auf diesen Pfaden.

**Wiederholung.** Die SPA wiederholt nur, was sicher ist (`web/src/app/core/failover/retry-policy.ts`):

- GET, HEAD, PUT und DELETE, jeweils höchstens einmal auf einem anderen Node;
- POST und PATCH nur mit `Idempotency-Key`, und zwar mit demselben Schlüssel;
- `POST /api/v1/stream/ticket` auch ohne Schlüssel, weil ein Ticket einmalig und kurzlebig ist;
- nie `/api/v1/auth/*` und nie `/api/v1/me/password`, weil ein erneuter Refresh das Token rotieren und die Sitzung widerrufen könnte;
- nie bei 4xx. Auch 401 und 403 lösen keinen Wechsel aus.

Bei einem Fehler ohne sichere Wiederholung meldet die SPA den Ausfall dem Auswahlmechanismus und gibt den Fehler an den Aufrufer weiter. Dann sollte der Nutzer den Vorgang prüfen, bevor er wiederholt.

**Header.** Jede Antwort des Frontend-Hosts trägt `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, `X-Frame-Options: DENY` und `Cross-Origin-Opener-Policy: same-origin` sowie die CSP mit `frame-ancestors 'none'`. HSTS kommt nur mit TLS (Standard `auto`).

**Chrome Local Network Access.** Ruft eine öffentlich ausgelieferte SPA Adressen im privaten Netz oder auf Loopback auf, fragt Chrome (ab Version 142, laut Herstellerangaben) den Nutzer um Erlaubnis. Ohne Zustimmung schlagen die Aufrufe fehl, und die SPA findet keinen gesunden Node. Lösung: Voter unter öffentlichen Adressen betreiben, oder das Frontend im selben privaten Netz ausliefern.

**Kein Proxy.** Der Frontend-Host antwortet auf `/api/` mit 404 und leitet nichts weiter. Der Verkehr zur API geht immer direkt vom Browser zum Voter.

## 8. Beispiel-Compose

[deploy/frontend/compose.yaml](../../deploy/frontend/compose.yaml) zeigt drei Voter und ein Frontend auf einem Host. Starten aus dem Wurzelverzeichnis des Repositorys:

```sh
docker compose -f deploy/frontend/compose.yaml up -d
```

Die Datei ist eine lokale Vorlage. Die Voter sind unter `localhost` erreichbar, und `http` ist für Loopback-Adressen erlaubt. Für den Betrieb ersetzen Sie die Platzhalter:

| Platzhalter | Ersetzen durch |
| --- | --- |
| `PUBLIC_URL` (je Voter) | die öffentliche `https`-Adresse des Voters, ohne Pfad |
| `CORS_ORIGINS` (je Voter) | den Origin des Frontends, zum Beispiel `https://uptime.example.com` |
| `FRONTEND_NODES` (Frontend) | die `PUBLIC_URL`-Adressen aller Voter, in der gewünschten Reihenfolge |
| `OIDC_ISSUER` | die Issuer-URL des Identity Providers, exakt wie im Claim `iss` |
| `OIDC_CLIENT_IDS` (Voter) und `OIDC_CLIENT_ID` (Frontend) | dieselbe Client-ID der SPA |
| `OIDC_SCOPES` | Scopes mit `openid` und `offline_access` |
| `FRONTEND_HSTS` | `auto` bei `https`, sonst `off` |

Das Frontend setzt `read_only: true`, `tmpfs: /tmp`, `cap_drop: ALL` und `no-new-privileges`. Die Voter laufen wie in [compose.md](compose.md) beschrieben. Die Voter-Blöcke enthalten `CLUSTER_JOIN` nur bei den Folgeknoten, der erste Voter bootstrappt den Cluster.

## 9. Fehlersuche

| Symptom | Ursache | Abhilfe |
| --- | --- | --- |
| In der Browser-Konsole steht „blocked by CORS policy“. Die SPA wechselt ständig oder findet keinen Node. | Der Origin des Frontends fehlt in `CORS_ORIGINS` auf dem Voter. Die Antwort kommt ohne `Access-Control-Allow-Origin`, der Browser meldet Status 0. | Origin mit Schema und Port in `CORS_ORIGINS` jedes Voters eintragen und die Voter neu starten. |
| Nach einem Failover endet die Sitzung, obwohl der Nutzer angemeldet war. | Das Refresh-Cookie gilt nicht für den Ziel-Voter, weil `AUTH_COOKIE_DOMAIN` fehlt oder das Frontend cross-site ist. Alternativ wurde die Sitzungsfamilie durch eine verlorene Refresh-Antwort widerrufen. | Gemeinsame `AUTH_COOKIE_DOMAIN` setzen (Abschnitt 5) oder `AUTH_MODE=oidc` verwenden. Bei Widerruf erneut anmelden. |
| Die Warnung `CORS origin is cross-site to PUBLIC_URL` erscheint im Log. | Ein `CORS_ORIGINS`-Eintrag liegt auf einer anderen Site als die `PUBLIC_URL`, `AUTH_MODE=local`. | Frontend auf eine Subdomain derselben Domain legen oder `AUTH_MODE=oidc` verwenden. |
| Ein Voter wird nie gewählt. | `/api/v1/health` liefert `degraded: true` oder `stale: true`, oder der Voter antwortet nicht. | Health-Antwort des Voters im Browser oder mit `curl https://n1.example.com/api/v1/health` prüfen. `stale` deutet auf fehlenden Leader oder Rückstand hin, siehe [cluster.md](cluster.md). |
| Der Container startet nicht. Das Log nennt `FRONTEND_NODES: node "…": invalid public node URL: …`. | Ein Eintrag verletzt die Regeln aus Abschnitt 2.1, zum Beispiel `http` für eine öffentliche Adresse, ein Pfad oder ein Port 0. | Eintrag korrigieren. Die Meldung nennt die verletzte Regel. |
| Der Container startet nicht. Das Log nennt `invalid configuration:` mit mehreren Zeilen. | Mehrere Variablen sind ungültig, etwa `OIDC_CLIENT_ID` fehlt bei `AUTH_MODE=oidc`, oder TLS ist nur halb gesetzt. | Alle genannten Variablen prüfen (Abschnitt 4.1). |
| Der Container ist im Docker-Status „unhealthy“, die SPA läuft aber. | `FRONTEND_LISTEN` weicht von `HTTP_ADDR` ab, der Healthcheck prüft den falschen Port. | `HTTP_ADDR` auf denselben Wert wie `FRONTEND_LISTEN` setzen. |
| Ein Voter fehlt in der Failover-Liste. | Seine `PUBLIC_URL` ist ungültig oder fehlt. Die Befehlsstufe liegt noch unter 11. | Log auf `PUBLIC_URL is not a plain origin` prüfen. Bei Stufe 11 kann der Cluster noch im Update stecken, siehe [cluster.md](cluster.md). |
| Die Liste im Frontend ist veraltet, nachdem ein Voter umgezogen ist. | Die `config.json` des Frontends steht noch auf der alten Adresse. Nachladen kann nur einengen. | `config.json` oder `FRONTEND_NODES` anpassen und den Host neu starten. |
| Auf einer öffentlichen Seite erscheint eine Chrome-Abfrage zum lokalen Netz. | Die SPA ruft Voter unter privaten Adressen auf (Abschnitt 7). | Voter unter öffentlichen Adressen betreiben oder Frontend im selben Netz ausliefern. |
| Anfragen an `/api/...` auf dem Frontend-Host liefern 404. | Gewollt: Der Frontend-Host leitet keine API-Anfragen weiter. | Die SPA direkt mit den Votern sprechen lassen. Ein Proxy im Frontend-Host ist nicht vorgesehen. |
