# ADR 0036: Externes Frontend und Client-Failover

|                 |                                                                                                                                                                                                                                        |
|-----------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **Status**      | Proposed                                                                                                                                                                                                                               |
| **Datum**       | 2026-11-19                                                                                                                                                                                                                             |
| **Entscheider** | Projektinhaber                                                                                                                                                                                                                               |
| **Bezug**       | FA-UI-02, FA-UI-03, FA-API-05, FA-API-07, FA-AUTH-03, FA-AUTH-11, FA-CLU-06, NFA-SEC-04, NFA-SEC-05, PF-34, O-04, E15, Z7, ADR 0001, ADR 0002, ADR 0003, ADR 0007, ADR 0008, ADR 0012 (A8, A9), ADR 0018, ADR 0027, ADR 0028, ADR 0031 |

## Kontext

AP-5.3 soll zwei Dinge liefern:

- **FA-UI-02 (extern):** Die SPA läuft auch außerhalb der Voter, als statisches Bundle mit eigener `config.json`.
- **FA-UI-03:** Die SPA prüft beim Start alle Nodes, wählt den schnellsten gesunden, wechselt bei Fehlern und lädt die
  Node-Liste von `/api/v1/cluster/nodes` nach. Abnahme: E2E-Test mit gestopptem Voter.

**Stand im Code (Branch `master`, nach AP-4.6):**

- `ConfigService` lädt `/config.json` vom Origin der Seite. `apiBaseUrl()` nimmt immer `nodes[0]`. Es gibt keinen
  Health-Check, keine Auswahl und keinen Wechsel.
- Der Voter erzeugt `/config.json` dynamisch, aber nur mit seiner eigenen `PUBLIC_URL` (oder leer = gleicher Origin).
  Die `PUBLIC_URL` der anderen Voter kennt er nicht. Das Node-Register im Raft-Zustand (`NodeInfo`) hat nur
  `advertise_addr`, die interne mTLS-Adresse.
- `/api/v1/cluster/nodes` gibt es nicht. Es gibt `/api/v1/admin/nodes`, nur für `platform-admin`, mit vielen internen
  Feldern. Für die Node-Auswahl vor dem Login taugt das nicht.
- `authInterceptor` hängt das Bearer-Token nur an relative `/api/`-URLs, danach setzt `apiBaseUrlInterceptor` die Basis
  davor. Diese Reihenfolge ist die Sicherung gegen Token-Abfluss an fremde Hosts.
- CORS (`internal/server/security.go`): exakte Allow-List aus `PUBLIC_URL`-Origin und `CORS_ORIGINS`, `Vary: Origin`,
  nie `*`, `Allow-Credentials: true` auf **allen** API-Pfaden, Preflight-Cache 600 s.
- CSP der SPA: `connect-src 'self'` (plus Issuer-Origin bei OIDC). **Damit blockiert der Browser heute jeden Failover
  der eingebetteten SPA auf einen anderen Voter.**
- Refresh-Cookie (ADR 0008): `HttpOnly; Secure; SameSite=Strict; Path=/api/v1/auth`, Domain aus `AUTH_COOKIE_DOMAIN`
  (leer = Host-Cookie). Refresh-Tokens werden rotiert. Wiederverwendung widerruft die Familie (FA-AUTH-03). Der Zustand
  liegt im Raft, gilt also auf allen Votern.
- WebSocket-Tickets (FA-AUTH-11) gelten nur auf dem ausstellenden Node.
- Idempotenz: Das FSM merkt sich Request-IDs 24 h clusterweit (`dedupRetention`). 8 von 39 schreibenden Operationen
  nehmen einen `Idempotency-Key` an, darunter Monitor anlegen, Klonen und Massenaktionen. ADR 0012 A8: Bei 503 „Ergebnis
  unbekannt“ ist eine Wiederholung mit gleichem Key sicher.
- `/api/v1/health` ist öffentlich, liegt unter `/api` (also mit CORS-Headern) und meldet `degraded`. `/readyz` liegt
  außerhalb von `/api`, bekommt keine CORS-Header und ist im Browser cross-origin nicht lesbar.
- Release-Image: `gcr.io/distroless/static-debian12:nonroot`, ein Binary mit Unterbefehlen (`version`, `healthcheck`,
  `admin`).

**Ergebnisse der Recherche (Stand 2026-11):**

- **Site und Origin:** Cookies mit `SameSite=Strict` gehen auch bei Cross-Origin-Fetches mit, solange beide Seiten
  *same-site* sind, also dieselbe registrierbare Domain (eTLD+1 nach Public Suffix List) und dasselbe Schema haben.
  Ports zählen nicht. `status.example.com` → `mon1.example.com` ist same-site. `*.pages.dev`, `*.github.io` und
  `*.netlify.app` stehen auf der PSL: jedes Projekt ist eine eigene Site, also immer cross-site zu den Votern.
- **Third-Party-Cookies:** Chrome hat im April 2025 den Ausstieg aus Third-Party-Cookies abgesagt und im Oktober 2025
  die meisten Privacy-Sandbox-APIs eingestellt. CHIPS (`Partitioned`) bleibt. Safari (ITP) blockiert Third-Party-Cookies
  vollständig, Firefox partitioniert sie standardmäßig. Cross-Site-Cookies sind deshalb browserübergreifend nicht
  verlässlich. Unabhängig davon verhindert `SameSite=Strict` sie ohnehin.
- **Local Network Access (Chrome ≥ 142):** Ruft eine Seite auf einer öffentlichen Adresse Ziele im privaten Netz oder
  auf Loopback auf (fetch, Subresourcen, laut Herstellerangaben später auch WebSocket und WebRTC), fragt Chrome den
  Nutzer um Erlaubnis. Ein öffentlich gehostetes Frontend mit Votern unter privaten IPs löst diese Abfrage aus. Ohne
  Erlaubnis schlagen die Aufrufe fehl.
- **CORS:** Gängige Empfehlung (OWASP, MDN, Fetch-Standard): Origin exakt aus einer Allow-List spiegeln, `Vary: Origin`,
  nie `*` zusammen mit Credentials, nie `null` erlauben, Credentials nur dort, wo Cookies gebraucht werden. Jeder
  Request mit `Authorization` löst einen Preflight aus. Der Preflight-Cache gilt je URL, Methode und Header. Chrome
  deckelt `Access-Control-Max-Age` auf 7200 s, Firefox auf 86400 s.
- **OIDC:** Das Access-Token wird an den Issuer und die Audience gebunden, nicht an einen Voter. Jeder Voter mit
  derselben `OIDC_*`-Konfiguration prüft es gleich (ADR 0027). Den Refresh macht die SPA direkt beim IdP. Eine
  Redirect-URI hängt am **Origin der SPA**, nicht am API-Node. Redirect-URIs und CORS-Origins sind bei den meisten IdPs
  getrennte Einstellungen. Keycloak nennt sie „Web Origins“. Zitadel antwortet laut Community-Berichten auf den
  OIDC-Endpunkten mit Wildcard-CORS. Die offizielle Doku bestätigt das nicht, es ist also nicht geprüft.
- **Retry:** Nach RFC 9110 §9.2.2 sind GET, HEAD, OPTIONS, PUT und DELETE idempotent, POST und PATCH nicht. Verbreitete
  Clients (gRPC-Retry-Policies, AWS-SDKs, Stripe) wiederholen automatisch nur idempotente Aufrufe oder solche mit
  Idempotency-Key. Der IETF-Entwurf `draft-ietf-httpapi-idempotency-key-header` beschreibt genau das Muster, das wir mit
  `Idempotency-Key` schon haben.
- **Statische Hosts und Header:** GitHub Pages erlaubt keine eigenen Response-Header. Cloudflare Pages und Netlify
  erlauben sie über eine `_headers`-Datei. CSP per `<meta http-equiv>` kennt `frame-ancestors`, `report-uri` und
  `sandbox` nicht. Auch `X-Frame-Options` geht nicht per Meta-Tag, Schutz gegen Clickjacking also nur über echte Header.

## Optionen

### Frage 1: Woher kommt die Node-Liste, und wem vertraut die SPA?

Die Node-Liste bestimmt, wohin die SPA das Bearer-Token schickt. Damit ist sie sicherheitsrelevant.

**Option 1A: nur statisch aus `config.json`.**

- Vorteile: Einfach. Einzige Vertrauensquelle ist die Konfiguration des Frontends.
- Nachteile: Neue oder ersetzte Voter erscheinen erst nach einer Änderung von `config.json`. FA-UI-03 („Nachladen“) ist
  nicht erfüllt. In der eingebetteten Variante müsste jeder Voter die anderen per Umgebungsvariable kennen.

**Option 1B: `config.json` ist die Allow-List, das Nachladen sortiert und filtert nur.**

- Jeder Voter trägt seine `PUBLIC_URL` ins Node-Register ein (`RegisterNode`, neues Feld). Ein neuer öffentlicher
  Endpunkt `GET /api/v1/cluster/nodes` liefert nur `{id, url, healthy, voter}`.
- Die eingebettete `config.json` nimmt diese URLs der Voter in die Liste auf. Weil der ausliefernde Voter auch das
  JavaScript liefert, kommt dadurch keine neue Vertrauensquelle hinzu.
- Die externe SPA übernimmt aus der nachgeladenen Liste nur Origins, die schon in ihrer eigenen `config.json` stehen
  (Schnittmenge). Neue Einträge werden ignoriert und einmal geloggt.
- Vorteile: Ein kompromittierter oder falsch konfigurierter Voter kann die externe SPA nicht auf einen fremden Host
  lenken. Die CSP (`connect-src`) erzwingt im Browser dieselbe Grenze.
- Nachteile: Neue Voter brauchen extern weiterhin einen Eintrag in `config.json`. Das Nachladen bringt dort vor allem
  Gesundheit und Reihenfolge.

**Option 1C: Die nachgeladene Liste ersetzt die Konfiguration.**

- Vorteile: Neue Voter sind sofort nutzbar.
- Nachteile: Wer `/cluster/nodes` manipulieren kann (kompromittierter Voter, falsch gesetzte `PUBLIC_URL`, DNS), lenkt
  Tokens auf einen fremden Host. Die CSP müsste dafür ohnehin weit offen sein (`connect-src https:`). Abgelehnt.

### Frage 2: Auswahl des Nodes und Wechsel

**Option 2A: erster erreichbarer Node in der Reihenfolge der Konfiguration.**

- Vorteile: Deterministisch, einfach zu testen.
- Nachteile: Nicht „schnellster“, wie FA-UI-03 verlangt. Langsamer Start, wenn der erste Node hängt.

**Option 2B: paralleler Health-Check, schnellster gesunder Node, dann klebend.**

- Beim Start parallel `GET {node}/api/v1/health` mit `credentials: 'omit'`, `cache: 'no-store'` und 2 s Timeout.
  Gemessen wird die Zeit mit `performance.now()`.
- Gesund ist ein Node mit Status 200, `degraded=false` und (neu, siehe unten) `stale=false`. Gewählt wird die kürzeste
  Antwortzeit. Bei weniger als 20 % Unterschied entscheidet die Reihenfolge der Konfiguration.
- Der gewählte Node bleibt aktiv, bis ein Fehler auftritt (Frage 3). Dann wird er für 30 s gesperrt (verdoppelnd bis 5
  min, mit Jitter), und der nächstbeste gesunde Node übernimmt.
- Im Hintergrund prüft die SPA alle 60 s, solange der Tab sichtbar ist. Zurück zu einem schnelleren Node wechselt sie
  nicht automatisch, nur nach einem Fehler.
- Vorteile: Erfüllt FA-UI-03. Kein Flattern. Wenige zusätzliche Requests.
- Nachteile: Etwas Zustand in der SPA (Sperrliste, Backoff).

**Option 2C: Lastverteilung je Request oder Hedging (gleichzeitig an zwei Nodes).**

- Vorteile: Bestmögliche Latenz.
- Nachteile: WebSocket-Tickets gelten nur auf einem Node, Lesezugriffe auf verschiedenen Followern sehen
  unterschiedliche Stände (kein Read-your-writes), doppelte Schreibzugriffe brauchen überall Idempotenz.
  Unverhältnismäßig.

### Frage 3: Welche Requests werden automatisch auf einem anderen Node wiederholt?

Auslöser für einen Wechsel sind: Netzwerkfehler (Status 0, das umfasst auch eine CORS-Ablehnung), Timeout, 502, 503 und
504. Statuscodes 4xx lösen nie einen Wechsel aus, auch 401 und 403 nicht. Bei 503 mit `Retry-After` wird höchstens bis
zu dieser Frist gewartet.

**Option 3A: alles einmal wiederholen.**

- Nachteile: Ein POST ohne Idempotency-Key kann doppelt ausgeführt werden (ADR 0012 A8: Ausgang unbekannt). Abgelehnt.

**Option 3B: nur sichere Requests wiederholen.**

- Automatisch auf dem nächsten Node wiederholt werden:
    - GET und HEAD,
    - PUT und DELETE (idempotent nach RFC 9110; ein DELETE kann beim zweiten Mal 404 liefern, was die SPA als Erfolg
      wertet),
    - POST und PATCH nur mit `Idempotency-Key`, und zwar immer mit **demselben** Key.
- Nie automatisch wiederholt wird `POST /auth/refresh` (siehe Frage 4). Die übrigen Auth-Endpunkte werden nicht über
  einen anderen Node wiederholt.
- `POST /stream/ticket` wird wiederholt, aber der WebSocket geht danach an denselben Node wie der Ticket-Abruf.
- Höchstens ein Wechsel pro Request und höchstens so viele Versuche, wie es gesunde Nodes gibt.
- Alles andere: Die SPA wechselt den Node für **künftige** Requests, gibt den Fehler aber an den Aufrufer weiter. Die
  Meldung lautet „Ergebnis unbekannt, bitte prüfen“.
- Vorteile: Keine doppelten Objekte. Folgt RFC 9110 und dem bestehenden Dedup im FSM.
- Nachteile: POST- und PATCH-Operationen ohne Key wiederholen nicht. Ein Teil der Bedienung bleibt also sichtbar
  fehlerhaft, bis der Nutzer selbst wiederholt.

**Option 3C: nie automatisch wiederholen, nur den Node wechseln.**

- Vorteile: Am einfachsten.
- Nachteile: Jeder Ausfall erscheint als Fehler in der Oberfläche, auch beim Lesen. Das erfüllt den Zweck von FA-UI-03
  kaum.

### Frage 4: Lokale Anmeldung über mehrere Nodes und mit externem Frontend

Das Access-Token (JWT, 15 min) gilt auf allen Votern. Problematisch ist nur der Refresh-Cookie.

**Option 4A: Bei ADR 0008 bleiben, externes Frontend im Modus `local` nur same-site.**

- Empfohlener Aufbau: Frontend und Voter unter einer eigenen Subdomain, z.B. `uptime.example.com` (SPA) und
  `n1.uptime.example.com` … `n3.uptime.example.com`, mit `AUTH_COOKIE_DOMAIN=uptime.example.com`. Dann kann jeder Voter
  refreshen.
- Ohne gemeinsame Cookie-Domain gilt der Cookie nur für den Voter, der ihn gesetzt hat. Nach einem Failover funktioniert
  der nächste Refresh nicht. Die SPA muss dann zum Login, spätestens nach Ablauf des Access-Tokens.
- Beim Start prüft jeder Voter mit `golang.org/x/net/publicsuffix` (schon im Modul vorhanden), ob jeder Eintrag in
  `CORS_ORIGINS` same-site zur `PUBLIC_URL` ist. Ist er cross-site und `AUTH_MODE=local`, gibt es eine Warnung im Log:
  lokale Anmeldung von diesem Origin aus ist nicht möglich.
- Vorteile: Keine Änderung am Sicherheitsmodell, OWASP-konform, nichts Neues im JavaScript-Speicher.
- Nachteile: Cloudflare Pages, GitHub Pages und Netlify unter ihren eigenen Domains gehen im Modus `local` nicht. Dort
  braucht es eine eigene Domain oder OIDC. Ein Domain-Cookie geht an alle Hosts unter dieser Domain (nur für den Pfad
  `/api/v1/auth`), deshalb die Empfehlung einer eigenen Subdomain.

**Option 4B: Für cross-site ein partitionierter Cookie (`SameSite=None; Partitioned`, CHIPS).**

- Vorteile: Funktioniert in aktuellen Browsern auch cross-site, partitioniert nach der Site des Frontends.
- Nachteile: `SameSite=None` öffnet den Refresh-Endpunkt für Cross-Site-Requests. Schutz bleibt dann nur über CORS und
  den Header `X-Requested-With`. Das Verhalten unterscheidet sich zwischen den Browsern und lässt sich schwer testen.
  Weicht von ADR 0008 ab.

**Option 4C: Refresh-Token im Speicher der SPA für cross-site.**

- Vorteile: Failover überall.
- Nachteile: Neu-Login bei jedem Reload. Ein XSS kann das Refresh-Token lesen, solange der Tab offen ist. Weicht von ADR
  0008 Option A ab.

**Option 4D: Backend-for-Frontend, der Frontend-Host leitet `/api` an die Voter weiter.**

- Vorteile: Same-Origin, keine CORS- und Cookie-Probleme.
- Nachteile: Der Proxy wird zum Single Point of Failure. Das widerspricht E15 und Z7 („Failover im Client“). Der Proxy
  müsste selbst Failover machen.

**Zusatz (gilt für alle Optionen): Wettlauf bei der Rotation.** Erreicht ein Refresh den Server, aber die Antwort mit
dem neuen Cookie geht wegen eines Ausfalls verloren, hat der Browser noch den alten Cookie. Jeder weitere Refresh, egal
auf welchem Voter, gilt dann als Wiederverwendung und widerruft die Familie (FA-AUTH-03). Folge: Neu-Login. Das ist
selten und sicher, aber lästig. Abhilfe wäre eine kurze Kulanzfrist, siehe offene Frage O3.

### Frage 5: OIDC

Das Token ist issuer-gebunden. Mit identischer `OIDC_*`-Konfiguration auf allen Votern ist es überall gültig. Den
Refresh macht die SPA beim IdP, unabhängig vom Voter. Der Failover zwischen API-Nodes berührt OIDC also nicht. Offen ist
nur, unter welchen Origins die SPA selbst läuft.

**Option 5A: Jeder Origin, unter dem die SPA erreichbar ist, wird als Redirect-URI und Post-Logout-URI beim IdP
eingetragen** (alle Voter-URLs für die eingebettete SPA, dazu das externe Frontend).

- Vorteile: Nutzer können jede Voter-URL öffnen.
- Nachteile: Mehr Pflege. Jede vergessene URI führt zu einem Login-Fehler auf diesem Node.

**Option 5B: ein kanonischer Origin.** Mit `FRONTEND_URL` gesetzt, leiten Voter `/` und alle SPA-Routen per 302 auf das
externe Frontend um, `/api` bleibt.

- Vorteile: Eine Redirect-URI, ein Origin für Cookies, CORS und CSP.
- Nachteile: Fällt der Host des externen Frontends aus, ist die SPA auch über die Voter nicht mehr erreichbar. Deshalb
  opt-in.

### Frage 6: Auslieferung des externen Frontends und CSP

**Option 6A: nur ein Release-Asset** `uptimecluster-web-<version>.tar.gz` (Inhalt von `web/dist` plus
`config.example.json` und eine generierte `_headers`-Vorlage).

- Vorteile: Läuft auf jedem statischen Host, keine Laufzeit-Komponente von uns.
- Nachteile: CSP und `frame-ancestors` hängen am Host. Auf GitHub Pages fehlen sie.

**Option 6B: eigenes Image auf Basis von `nginxinc/nginx-unprivileged`**, mit `config.json` und CSP per `envsubst` aus
Umgebungsvariablen.

- Vorteile: Bekanntes Werkzeug.
- Nachteile: Zweites Image mit eigener Paketbasis (Debian oder Alpine), eigene CVE-Pflege, Header-Logik doppelt (Go und
  nginx-Konfiguration), Lizenzprüfung für ein weiteres Image.

**Option 6C: Unterbefehl `uptimecluster frontend` im bestehenden Binary und Image (distroless).**

- Liefert nur die eingebettete SPA, eine aus Umgebungsvariablen erzeugte `config.json` und dieselben Security-Header wie
  der Voter (`internal/webui` und `securityHeaders` werden wiederverwendet). Kein Raft, kein Datenverzeichnis, keine
  API.
- Vorteile: Ein Image für alle Rollen (CLAUDE.md), dieselbe CSP-Logik, keine neue Abhängigkeit, distroless und nonroot.
- Nachteile: Das Binary ist größer als für eine reine Auslieferung nötig. Weil es ein Unterbefehl ist, bleibt
  `NODE_ROLE` unverändert (FA-NODE-01).

**CSP (für alle Optionen):**

- **Eingebettet:** `connect-src 'self'` plus die Origins aller Nodes aus der `config.json` desselben Voters, jeweils als
  `https://…` und `wss://…`, dazu der Issuer. Heute fehlt das, und der Failover ist damit blockiert.
- **Extern:** dieselbe Regel, berechnet aus der `config.json` des Frontends. Bei 6C macht das der Server. Bei 6A erzeugt
  ein Skript aus `config.json` eine `_headers`-Datei. Für Hosts ohne Header-Unterstützung gibt es nur eine Meta-CSP,
  ohne `frame-ancestors`. Das wird als „nicht empfohlen“ dokumentiert.

## Entscheidung

Vorschlag: **1B, 2B, 3B, 4A, 5A (5B als Opt-in), 6A + 6C.**

**Begründung:**

- **1B:** Die Node-Liste lenkt Bearer-Tokens. Deshalb bleibt die vom Betreiber kontrollierte `config.json` die einzige
  Vertrauensquelle des externen Frontends. Die CSP setzt dieselbe Grenze im Browser durch. In der eingebetteten Variante
  ist der ausliefernde Voter ohnehin voll vertraut, dort darf die Liste aus dem Register kommen.
- **2B:** Erfüllt FA-UI-03 wörtlich und vermeidet Flattern. Die Bindung an einen Node passt zu den WebSocket-Tickets und
  zum Lesen auf einem Follower.
- **3B:** Wiederholt nur, was nach HTTP-Semantik oder dank FSM-Dedup sicher ist. Folgearbeit: `Idempotency-Key` für alle
  übrigen anlegenden POSTs im Vertrag (rückwärtskompatibel, weil optional).
- **4A:** Ändert das Sicherheitsmodell aus ADR 0008 nicht. Externe Hosts unter fremder Domain nutzen OIDC oder eine
  eigene Domain. 4B bleibt als spätere Erweiterung offen.
- **5A:** Standard ohne Abhängigkeit vom Frontend-Host. 5B nur, wer ausdrücklich einen kanonischen Origin will.
- **6A + 6C:** Das Asset deckt CDN-Hosts ab, der Unterbefehl den Selbstbetrieb mit korrekten Headern. Kein zweites
  Image.

### Vertrag und Backend

- **Node-Register:** `NodeInfo.public_url` (neues Proto-Feld) und eine Spalte im Register. `RegisterNode` setzt das Feld
  erst ab Befehlsstufe **11** (Feature-Gate nach ADR 0018; Vergabe der P5-Stufen siehe Roadmap P5). Ältere Voter würden
  das Feld nicht speichern, und der Fingerprint wiche ab.
- **Prüfung von `public_url` im FSM:** absolute URL, `https` (oder `http` nur für Loopback), ohne Pfad, Query, Fragment
  und Userinfo, höchstens 256 Zeichen. Gespeichert wird der normalisierte Origin.
- **`GET /api/v1/cluster/nodes`:** öffentlich (`security: []`), Rate-Limit wie `/health`. Antwort
  `{nodes: [{id, url, voter, healthy}]}`, nur Voter mit `public_url`. Keine Namen, Versionen oder Adressen.
  `/admin/nodes` bleibt unverändert.
- **`Health`:** neues optionales Feld `stale` (ADR 0012 A9), additiv.
- **Eingebettete `config.json`:** eigene `PUBLIC_URL` zuerst, danach die `public_url` der übrigen Voter aus dem
  Register. Ohne `PUBLIC_URL` bleibt die Liste leer (gleicher Origin, kein Failover). `Cache-Control: no-cache` bleibt
  (ADR 0001).
- **CSP der SPA:** `connect-src` wird je Antwort aus derselben Node-Liste wie `config.json` berechnet (https und wss je
  Origin, dazu der Issuer). Die Werte werden gegen Header-Injection geprüft, wie heute bei `spaCSP`.
- **CORS:**
    - `Access-Control-Allow-Credentials: true` nur noch auf `/api/v1/auth/*`. Bearer-Requests laufen ohne Credentials
      (`withCredentials: false`).
    - `Access-Control-Max-Age` steigt auf 7200 s.
    - `Origin: null` wird nie gespiegelt (Test).
    - Allow-List, exakte Spiegelung und `Vary` bleiben.
    - `Idempotency-Key` ist schon erlaubt. `Retry-After` und `Location` bleiben die exponierten Header.
- **Startprüfung:** Warnung bei cross-site-Einträgen in `CORS_ORIGINS` im Modus `local` (4A). Hinweis im Log, wenn
  `CORS_ORIGINS` leer ist, obwohl andere Voter eine `public_url` haben. Dann hat die eingebettete SPA nach dem Failover
  keinen CORS-Zugriff, sofern nicht jeder Voter-Origin in `CORS_ORIGINS` aller Voter steht.
- **CORS zwischen Votern:** Die eingebettete SPA auf `n1` ruft `n2` cross-origin auf. Die Origins aller Voter müssen
  also in der Allow-List aller Voter stehen. Vorschlag: Jeder Voter nimmt die `public_url` der Voter aus dem Register
  automatisch in seine CORS-Allow-List auf. Der Inhalt des Registers ist über mTLS und Raft geschützt, eine neue
  Vertrauensquelle entsteht damit nicht (siehe O2).

### SPA

- **`NodeOrigins` (Hilfsfunktion, Sicherheitscode):** normalisiert und prüft Origins (nur `https`, `http` nur für
  `localhost`/`127.0.0.1`/`[::1]`; kein Pfad, keine Userinfo) und bildet die Schnittmenge mit der Liste aus der
  Konfiguration. Ungültige Einträge werden verworfen.
- **`NodeSelector`:** Health-Check, Auswahl, Sperrliste mit Backoff, Hintergrundprüfung alle 60 s bei sichtbarem Tab,
  Nachladen von `/cluster/nodes` nach der Auswahl und alle 5 min. Zustand in Signals.
- **`apiBaseUrlInterceptor`** nimmt die Basis-URL aus dem `NodeSelector`. Die Reihenfolge `authInterceptor` vor
  `apiBaseUrlInterceptor` bleibt, sie ist die Sicherung gegen Token-Abfluss.
- **`failoverInterceptor`** (nach dem Base-URL-Interceptor): erkennt Fehler nach Frage 3, meldet sie dem `NodeSelector`
  und wiederholt nur, was `isRetrySafe(req)` erlaubt. Die Funktion ist Sicherheitscode: Auth-Pfade sind ausgeschlossen.
  Ein `HttpContextToken` begrenzt auf einen Wechsel. Wiederholt wird mit relativer URL, damit Token und Basis neu
  gesetzt werden.
- **Auth `local`:** Ein fehlgeschlagener Refresh wird nicht auf einem anderen Node wiederholt. Bei einem Netzwerkfehler
  wechselt der `NodeSelector` den Node. Der **nächste** Refresh-Anlass (Timer oder 401) versucht es dort. Ohne
  gemeinsame Cookie-Domain endet die Sitzung dann mit Hinweis.
- **WebSocket:** Ticket-Abruf und Verbindung laufen über denselben, beim Start des Versuchs festgehaltenen Node. Bei
  Abbruch oder Close-Code ≠ 4001/4003 meldet die SPA den Fehler dem `NodeSelector`, der Reconnect wählt neu. Die
  Neusynchronisierung (FA-UI-06) bleibt unverändert.
- **Versionsabgleich:** `/health.version` des gewählten Nodes wird mit der Build-Version der SPA verglichen. Bei anderem
  Major oder Minor zeigt die SPA einen Hinweis (externes Frontend und Rolling Update, ADR 0018).
- **Anzeige:** Ein kleines Statussymbol in der Kopfzeile zeigt den aktuellen Node und den Wechsel an (Komfort, i18n).

### Auslieferung

- **Release-Asset** `uptimecluster-web-<version>.tar.gz` über GoReleaser. Inhalt: SPA, `config.example.json`, Skript
  `uptimecluster frontend headers --config config.json > _headers` (Go-Unterbefehl, damit die CSP-Logik nur einmal
  existiert).
- **Unterbefehl `uptimecluster frontend`:** liest `FRONTEND_NODES`, `AUTH_MODE` und die `OIDC_*`-Werte für die SPA,
  `FRONTEND_LISTEN` (Standard `:8080`) und optional TLS-Dateien. Liefert die SPA mit `config.json` und Security-Headern
  wie der Voter, `/healthz` für den Container-Healthcheck. Gleiches Image, anderer `CMD`.
- **Doku `docs/betrieb/frontend.md`:** „Domains und Cookies“ (Folgearbeit aus ADR 0008), Matrix Host × Auth-Modus,
  Redirect-URIs beim IdP, Hinweis zu Chrome Local Network Access, Beispiel-Compose mit drei Votern und Frontend.

## Konsequenzen

- **Positiv:**
    - Die SPA übersteht den Ausfall einzelner Voter beim Lesen ohne Fehler und beim Schreiben ohne Duplikate.
    - Das externe Frontend ist für OIDC und same-site-`local` vollständig nutzbar.
    - Die Sicherheitsgrenzen (Allow-List in `config.json`, CSP, CORS, Interceptor-Reihenfolge) sind ausdrücklich
      festgelegt und testbar.
    - Kein neues Image, keine neue Abhängigkeit.
- **Negativ / Risiken:**
    - Neue Befehlsstufe für `public_url`. Bis zur Freigabe der Stufe gibt es keinen Failover über das Register.
    - `local` auf fremden Hosts (`*.pages.dev` u.ä.) geht nicht. Nutzer müssen das wissen, Doku und Startwarnung sind
      Pflicht.
    - Domain-Cookies verbreitern die Angriffsfläche auf alle Hosts unter der Cookie-Domain (nur Pfad `/api/v1/auth`).
      Empfehlung: eigene Subdomain.
    - Verlorene Refresh-Antwort beim Failover → Widerruf der Familie → Neu-Login (O3).
    - Lesen auf einem Follower kann kurzzeitig veraltet sein. `stale` im Health-Check mildert das, verhindert es aber
      nicht.
    - Öffentliches Frontend mit privaten Voter-Adressen: Chrome fragt nach Zugriff aufs lokale Netz. Ohne Zustimmung
      gibt es keinen Zugriff.
    - Der öffentliche Endpunkt `/cluster/nodes` macht die Liste der Voter-URLs sichtbar. Die sind ohnehin öffentlich,
      Namen und interne Adressen bleiben verborgen.
    - Zusätzliche Preflights bei externem Frontend (je URL, gemildert durch 7200 s Cache).
- **Folgearbeiten:**
    - Optionaler `Idempotency-Key` für die übrigen anlegenden POSTs (Vertrag, rückwärtskompatibel).
    - E2E-Test mit drei Votern und gestopptem Voter (Abnahme FA-UI-03), dazu ein E2E-Test externes Frontend auf anderem
      Port (same-site `localhost`).
    - Doku `docs/betrieb/frontend.md`, STRIDE-Ergänzung für Node-Liste und CORS.
    - Pflichtenheft: `/api/v1/cluster/nodes` als öffentlicher Endpunkt, `FRONTEND_*`-Variablen, Unterbefehl `frontend`.

### Offene Fragen an Projektinhaber

- **O1 – Neue Voter im externen Frontend:** Reicht die strenge Schnittmenge (1B), oder soll `config.json` zusätzlich ein
  Suffix-Muster erlauben (z.B. `nodeOriginSuffix: ".uptime.example.com"`), damit neue Voter ohne Änderung der
  Konfiguration genutzt werden? Das Muster müsste dann auch in der CSP stehen (`https://*.uptime.example.com`).
- **O2 – Automatische CORS-Freigabe der Voter-Origins aus dem Register:** einverstanden, oder sollen Betreiber
  `CORS_ORIGINS` auf jedem Voter selbst pflegen?
- **O3 – Kulanzfrist bei der Refresh-Rotation:** Soll ein gerade rotiertes Refresh-Token 10–30 s lang noch einmal
  angenommen werden und dasselbe Nachfolge-Token liefern, statt die Familie zu widerrufen (wie bei Auth0 und Okta)? Das
  ändert FA-AUTH-03 und bräuchte einen eigenen kleinen ADR. Ohne Kulanzfrist bleibt es beim Neu-Login in diesem seltenen
  Fall.
- **O4 – Cross-site mit `local` (4B, CHIPS):** jetzt verwerfen oder für später vormerken?
- **O5 – Kanonischer Origin (5B, `FRONTEND_URL`-Umleitung):** in AP-5.3 mitnehmen oder später?
- **O6 – Auslieferung:** Reicht 6A + 6C, oder wird ausdrücklich ein nginx-Image (6B) gewünscht?

## Approval

| Datum      | Status   | Durch  | Anmerkung                                |
|------------|----------|--------|------------------------------------------|
| 2026-11-19 | Proposed | Claude | Vorbereitung AP-5.3; offene Fragen O1–O6 |
