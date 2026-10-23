# Roadmap – UptimeCluster

Arbeitsgrundlage für die Umsetzung. Anforderungen: [Pflichtenheft](pflichtenheft.md). Entscheidungen: [ADRs](adr/README.md). Regeln: [CLAUDE.md](../CLAUDE.md).

**Nutzung:** Vor jeder Arbeit „Aktueller Stand“ lesen. Arbeitspakete (AP) werden in Reihenfolge bearbeitet, sofern nicht blockiert. Nach Abschluss Status setzen und im selben Commit mitführen. Ein AP mit ADR-Abhängigkeit startet erst, wenn der ADR **Accepted** ist.

**Status:** ⬜ offen · 🔄 in Arbeit · ⏳ wartet auf Review · ⛔ blockiert · ✅ erledigt

---

## Aktueller Stand

| | |
|---|---|
| **Aktive Phase** | P2 – Cluster (P1 abgenommen; P0 offen nur für Läufe, die GitHub brauchen: AP-0.9/0.10) |
| **Wartet auf Projektinhaber** | – |
| **Nächster Schritt (Claude)** | AP-2.5 Zuweisung `all`/`count`/`nodes`, Consistent Hashing |

---

## Phasenüberblick

| Phase | Ziel | Exit-Kriterium |
|-------|------|----------------|
| **P0 – Fundament** | Prozess, Anforderungen, Architekturentscheidungen, lauffähiges Grundgerüst, Spikes für die größten Risiken | ADRs 0001–0011 entschieden; `make build test lint` grün; Raft-Spike zeigt Failover mit 3 Nodes |
| **P1 – Kern** | Vollständiger Single-Voter (Raft mit 1 Mitglied) inkl. Checks, Uptime, Alerts, lokale Nutzer, UI | Abnahmeszenarien AS-01, AS-10 |
| **P2 – Cluster** | Join/Leave, mTLS/CA, Heartbeat-Replikation, Write-Forwarding, Zuweisung | AS-02, AS-03, AS-09 |
| **P3 – HA-Logik** | Konsens-Status, Leader-Alerting, Degraded-Modus, Selbstüberwachung | AS-04, AS-05 |
| **P4 – Mandanten** | Zitadel, Rollen, Limits, SSRF-Durchsetzung, Admin-Mandanten | AS-06, AS-07 |
| **P5 – Reichweite** | Probes, externes Frontend mit Failover, PostgreSQL, Backup | AS-08 → **Release 1.0** |
| **P6 – Ausbau** | Statusseiten, Push-Monitore, weitere Typen, Node-Labels | je Feature |

---

## P0 – Fundament

| AP | Inhalt | Anforderungen | Abhängig von | Status |
|----|--------|---------------|--------------|--------|
| AP-0.1 | CLAUDE.md, ADR-Prozess und -Vorlage | NFA-MNT-05 | – | ✅ |
| AP-0.2 | Pflichtenheft inkl. Pitfall-Recherche | – | – | ✅ |
| AP-0.3 | Roadmap | – | – | ✅ |
| AP-0.4 | ADRs 0001–0011 vorschlagen | – | – | ✅ |
| AP-0.5 | Review der ADRs durch Projektinhaber | – | AP-0.4 | ✅ alle Accepted (2026-10-19) |
| AP-0.6 | Backend-Grundgerüst: Go-Modul, `main`, Konfiguration (Env), `slog`, HTTP-Server, `/healthz`, `/readyz`, `/api/v1/health`, Graceful Shutdown, Version per ldflags | FA-NODE-01/02/05/07 | ADR 0001, 0002 (Annahme: Vorschlag) | ✅ |
| AP-0.7 | Frontend-Grundgerüst: Angular 22 (standalone, zoneless, Vitest), Laufzeit-`config.json`, Health-Ansicht, Dev-Proxy | FA-UI-01/02 | ADR 0001, 0007 (Annahme: Vorschlag) | ✅ |
| AP-0.8 | Build: Makefile, SPA-Einbettung per `go:embed` mit Platzhalter, Cache-Header, SPA-Fallback, Dockerfile (distroless, nonroot), Compose Single | FA-UI-01, NFA-PERF-07, PF-38, PF-39 | ADR 0001 | ✅ Image mit Podman gebaut (31,4 MB) und per Compose betrieben (AP-1.15) |
| AP-0.9 | CI-Grundpipeline: Build, Tests, `go vet`, `gofmt`, `govulncheck`, Frontend-Build/-Test | NFA-MNT-02, NFA-SEC-06 | ADR 0009 (Basis) | 🔄 Workflow angelegt, noch nicht auf GitHub gelaufen (kein Remote) |
| AP-0.10 | CI-Ausbau: `golangci-lint`, Lizenzprüfung, Multi-Arch-Image nach GHCR, Release-Workflow | NFA-PORT-01, RB-LIC-03 | ADR 0009 | 🔄 Lokal umgesetzt (`make lint`, `make licenses`, `make notices`, `make generate-check`, `goreleaser check` grün); CI- und Release-Lauf auf GitHub noch ausstehend (kein Remote) |
| AP-0.11 | Lizenz: `LICENSE` (AGPLv3), SPDX-Header, `LICENSING.md`, `CONTRIBUTING.md` | RB-LIC-01/02 | ADR 0010 | ✅ CLA-Assistant-Einrichtung erst mit GitHub-Remote + anwaltlich geprüftem CLA-Text; `THIRD_PARTY_NOTICES` → AP-0.10; Quellcode-Link in der UI (RB-LIC-04) → AP-1.14 |
| AP-0.12 | OpenAPI-Entwurf für alle P1-Endpunkte + Codegenerierung (Server/Client) | FA-API-01..07 | ADR 0003 | ✅ `make generate`, `make api-lint`; alle Operationen außer Health antworten 501 bis zur Umsetzung in P1 |
| AP-0.13 | Protobuf-Entwurf: Cluster-, Replikations-, Probe-Service | Kap. 9.3 | ADR 0006 | ✅ `proto/uptimecluster/v1`, `buf lint` (STANDARD), Codegen nach `internal/pb`; FSM-Payloads werden in AP-1.2 verfeinert |
| AP-0.14 | **Spike Raft** (`spikes/raft`): 3 Container, Konfiguration per Raft, Heartbeats per Replikation inkl. Inkarnation, Leader-Kill, Partition 2\|1, Bootstrap-Erkennung. Ergebnisse und Empfehlungen zu ADR 0005/0006: [spikes/raft/README.md](../spikes/raft/README.md) | FA-CLU-01..07, FA-REP-01..03 | – (Spike) | ✅ |
| AP-0.15 | **Spike SQLite**: Benchmark `modernc.org/sqlite` vs. `ncruces/go-sqlite3` mit Heartbeat-Last (Batch-Insert, Range-Query, Retention-Delete) | PF-22..24, O-07 | – (Spike) | ✅ Ergebnisse: [spikes/sqlite/README.md](../spikes/sqlite/README.md); Entscheidung in ADR 0013 |
| AP-0.16 | Folgearbeiten ADR 0012: `ClusterService.GetStatus` im Proto, Kommentar `StreamPosition` (lückenlose Hochwassermarke), Pflichtenheft FA-CLU-03/06/08/10, FA-NODE-05 anpassen | – | ADR 0012 | ✅ Proto `GetStatus` + Hochwassermarke; Pflichtenheft v1.1 |

**Hinweis zu AP-0.6–0.9:** Das Grundgerüst wurde vor dem ADR-Review nach den damals vorgeschlagenen ADRs aufgesetzt; alle ADRs wurden unverändert angenommen, es ist keine Anpassung nötig.

---

## P1 – Kern (Single-Voter)

| AP | Inhalt | Anforderungen | ADR | Status |
|----|--------|---------------|-----|--------|
| AP-1.1 | Storage-Schicht: Repository-Interfaces mit Tenant-Scope, SQLite (Schreib-/Lese-Pool, Pragmas + Selbsttest), Migrationen (`auto_vacuum=INCREMENTAL`), Mandanten-Isolationstests | FA-TEN-01, FA-NODE-06, NFA-SEC-02, PF-22..24 | 0004, 0013 | ✅ Basis mit Tenants, Monitoren, Heartbeats; weitere Entitäten (Kanäle, Wartung, Nutzer, Incidents, Rollups) folgen in ihren APs. WAL-Alert → AP-3.6 |
| AP-1.2 | Node-Identität (Node-ID, Inkarnation), Raft mit einem Mitglied, FSM-Befehle, Request-Dedup, Snapshots per Backup-API, Determinismus-Test | FA-NODE-03, FA-CLU-01/02/06/07/09/13 | 0005, 0012 | ✅ In-Memory-Transport auf der Advertise-Adresse bis zum mTLS-Transport (AP-2.1); FSM-Fingerprint-Vergleich zwischen Votern → AP-3.6 |
| AP-1.3 | Geheimnisverschlüsselung (Envelope, KEK/DEK, Rotation) | FA-SEC-SEC | 0011 | ✅ KEK (Env/Datei/generiert), DEK-Rotation (jährlich automatisch), Keyring mit AAD-Bindung; Neuverschlüsselungs-Job und Stilllegung alter DEKs folgen mit den ersten verschlüsselten Objekten in AP-1.10; KEK-Rotation per Admin-CLI in P2 |
| AP-1.4 | Checker-Registry, zentraler Dialer mit SSRF-Hook, HTTP-Checker (frische Verbindungen, Phasen-Timeouts, TLS-Prüfung, Fehlerklassen) | FA-CHK-01..04, 10..15 | – | ✅ Registry, Dialer mit SSRF-Policy (Durchsetzung per `SSRF_PROTECTION` wird mit dem Scheduler in AP-1.6 verdrahtet), HTTP-Checker; Lizenzprüfung erfasst die neuen Abhängigkeiten erst, sobald der Scheduler sie ins Binary holt |
| AP-1.5 | TCP-, DNS- (TC→TCP, Mengenvergleich; `miekg/dns` v1 – v2 auf Codeberg noch pre-1.0) und ICMP-Checker (Modi, Selbsttest) inkl. Container-Doku | FA-CHK-20, 30..32, 40/41 | – | ✅ Echte Pings in der Entwicklungs-Sandbox nicht ausführbar (Tests überspringen dann); Container-Doku `docs/betrieb/icmp.md` noch nicht in Docker/Podman/Kubernetes praktisch verifiziert |
| AP-1.6 | Scheduler: Jitter inkl. Erstlauf, keine Selbstüberlappung, Nebenläufigkeitslimit, Goroutine-Leak-Tests | FA-SCH-01/03/04/05/07 | – | ✅ inkl. Heartbeat-Writer (vorgezogen aus AP-1.9), Reconciler über FSM-Änderungssignal, `SSRF_PROTECTION` verdrahtet; End-to-End-Test Raft → Scheduler → Heartbeat |
| AP-1.7 | Monitor-API (CRUD, Pause, Klonen, Typen mit JSON-Schema), Problem Details, Paginierung | FA-MON-*, FA-API-01..03/06 | 0003 | ✅ CRUD, Pause/Resume, Klonen, Test-Lauf, Heartbeats, Typen; Idempotency-Key mandantenbezogen. Geschützte Endpunkte liefern 401 bis AP-1.12; Status/Uptime-Felder folgen mit AP-1.8/1.9; Uptime-Endpunkt bleibt 501 bis AP-1.9 |
| AP-1.8 | Statuslogik Single-Node (Retries, PENDING, DEGRADED), Incidents | FA-CON-03, FA-INC-01/02 | – | ✅ Aggregator (`internal/aggregator`) bewertet die eigenen Ergebnisse und schreibt nur Statuswechsel per Raft (`SetMonitorState`, Incident atomar im selben Befehl, ersetzt `OpenIncident`/`ResolveIncident`); FSM hält PAUSED/PENDING bei Pause/Fortsetzen konsistent und beendet offene Incidents; Retry-Zähler wird nach Neustart aus eigenen Heartbeats rekonstruiert; Incident-API und Statusfilter in SQL. Incidents werden mit ihrem Monitor gelöscht |
| AP-1.9 | Uptime-Berechnung (Heartbeat-Speicherung bereits in AP-1.6), Rollups, Verdichtung, Retention in Batches | FA-UPT-01..06 | 0004 | ✅ Paket `internal/uptime`: Node-Uptime aus dem eigenen Heartbeat-Strom (Heartbeat gilt bis zum nächsten, höchstens 2 × Intervall), Konsens-Uptime aus der replizierten Statushistorie (`monitor_status_history`, 30 Tage, FSM prunt deterministisch); Stunden-Rollups nach 2 min Karenz, Tages-Rollups als Summe der Stunden; Verdichtung nach 48 h zu Latenz-Minuten, Statuswechsel bleiben; Retention in Batches à 5.000 mit `incremental_vacuum`; Uptime-Endpunkt und `uptime24h`; `RETENTION_*_DAYS`. Grenzen: p95 über Tage/Bereiche ist der gewichtete Mittelwert der Stunden-p95; Wartungszeiten folgen mit AP-1.11; verspätet replizierte Heartbeats (P2) erfordern eine Neuberechnung betroffener Rollups; Rollups gelöschter Monitore laufen nur über die Retention aus |
| AP-1.10 | Benachrichtigungen: Outbox, Shoutrrr-Kapselung, Webhook mit JSON-sicheren Templates, Testversand, Maskierung | FA-ALR-01..03/07..09/13 | 0014 | ✅ Kanäle (Shoutrrr v0.22.0, Webhook) mit verschlüsselten Geheimnissen und maskierter Ausgabe; FSM legt Outbox-Einträge atomar mit `SetMonitorState` an; Leader-Dispatcher mit Backoff + Jitter, `ALERT_MAX_ATTEMPTS`, Kanalstatus `failing`/`disabled`; Erinnerungen bei andauerndem DOWN; Testversand; Monitor↔Kanal inkl. Standardkanäle; SSRF-Dialer für alle Ziele. Offen: Wartungsunterdrückung (FA-ALR-13) mit AP-1.11; Neuverschlüsselung bei DEK-Rotation; PUT eines Monitors ohne `notificationChannelIds` entfernt die Zuordnung (PUT-Semantik) |
| AP-1.11 | Wartungsfenster (einmalig, wiederkehrend, Zeitzonen) | FA-MNT-01/02 | – | ✅ Einmalige und wiederkehrende Fenster (eigener 5-Feld-Cron-Parser, Zeitzone, DST: Lücke → erster gültiger Zeitpunkt, Doppelstunde → erstes Auftreten; `time/tzdata` eingebettet); Leader-Controller setzt MAINTENANCE/PENDING exakt zu Fensterbeginn/-ende, Aggregator ignoriert Ergebnisse im Fenster (keine Alerts, FA-ALR-13), offene Incidents werden beendet; Wartung zählt je Konsens und je Node nicht in die Uptime; API mit `nextStart`/`inProgress`. Umgesetzt durch Haiku-Agenten, Mandantenprüfung vom Hauptagenten |
| AP-1.12 | Lokale Authentifizierung: Setup-Flow, Argon2id, JWT (EdDSA), Refresh-Rotation, Drosselung, Rollenprüfung | FA-AUTH-01..04/09, NFA-SEC-03/04 | 0008 | ✅ Setup-Token (einmalig im Log, 1 h), Argon2id, EdDSA-JWT (15 min, Schlüssel im Raft-Zustand verschlüsselt, Rotation 30 Tage + 1 Tag Überlappung), Refresh-Rotation mit Familien-Widerruf bei Wiederverwendung (FSM), Drosselung je Konto (ab 5. Fehlversuch) und IP, auch für Passwortwechsel; Cookie HttpOnly/Secure/SameSite=Strict, `X-Requested-With`; Nutzerverwaltung mit Rollenregeln, Schutz des letzten Platform-Admins in der FSM; Berechtigungsmatrix-Test. Abweichung ADR 0008: `iss` = `uptimecluster` statt Cluster-ID. Offen: Proxy-Liste für Client-IP, Drosselung je Node (P2) |
| AP-1.13 | WebSocket-Stream mit Ticket, Mandantenfilter, CORS, Rate-Limiting, Security-Header | FA-API-04/05/07, FA-AUTH-11, FA-TEN-05, NFA-SEC-05 | 0002 | ✅ `GET /api/v1/stream?ticket=…` (`coder/websocket`): Ticket 30 s, einmalig, nur als SHA-256 im Speicher des ausstellenden Nodes; exakte Origin-Prüfung; Stream endet mit Ablauf des Access-Tokens (Close 4001), bei Entzug des Zugriffs (minütliche Prüfung, 4003), bei Überlauf (4008) und beim Shutdown; Hub filtert jedes Event zuerst nach dem Mandanten des Tickets, Client kann per `monitorId`/Nachricht auf Monitore einschränken. Events: Heartbeats (Scheduler-Sink), Status und Incidents (FSM-Observer nach Commit, entkoppelt über Queue). CORS mit Allow-List (`PUBLIC_URL`-Origin + `CORS_ORIGINS`), CSP ohne `unsafe-eval` (SPA) bzw. `default-src 'none'` (API); Rate-Limit je Client-IP für Auth-POSTs (Burst 10, 1/s) und öffentliche Endpunkte (Burst 50, 10/s). Umsetzung Rate-Limiter, FSM-Observer, Event-Erzeuger und Verdrahtung durch Haiku-Agenten; Ticket, Hub, WebSocket-Handler, CORS/CSP vom Hauptagenten. Offen: Node-Events (`type: node`) folgen mit der Cluster-Ansicht (AP-2.8/AP-3.6); Tickets gelten nur auf dem ausstellenden Node (Client-Failover P5); Proxy-Liste für Client-IP |
| AP-1.14 | Frontend: Shell, Login/Setup, Dashboard, Monitor-Detail (ECharts), Monitor-Editor (JSON-Schema), Kanäle, Wartung, Incidents, Live-Updates | FA-UI-04/06..09 | 0007 | ✅ Angular Material 3 mit Hell-/Dunkelmodus, responsiv bis 360 px, Laufzeit-i18n (transloco, de/en, ein Scope je Feature), eingebettete SVG-Icons statt Webfont (CSP, Größe), Quellcode-Link (RB-LIC-04). Sitzung: Access-Token nur im Speicher, Wiederaufnahme über Refresh-Cookie mit einem gemeinsamen Refresh, Bearer nur an relative `/api/`-URLs, 401 → ein Refresh + ein Retry, Return-URL nur intern. Live-Stream mit Ticket, Backoff + Jitter, Close-Codes 4001/4003/4008, Resync nach Reconnect. Ansichten: Dashboard, Monitor-Detail (ECharts tree-shaken, Latenz je Node), Monitor-Editor aus JSON-Schema, Kanäle mit geführten Shoutrrr-Formularen, Wartung, Incidents, Konto, Nutzerverwaltung. Kritisches CSS-Inlining abgeschaltet (Inline-Script verstößt gegen CSP). Ende-zu-Ende im eingebetteten Binary geprüft (Setup, Login, Monitor anlegen, Live-Wechsel PENDING → UP). Sicherheitskern vom Hauptagenten, Ansichten durch Haiku-Agenten. Lizenz-Ausnahme `argparse` (Python-2.0, nur Build-Zeit von transloco). Offen: E2E-Tests mit Playwright (→ AP-1.15); relative Zeitangaben wechseln die Sprache erst nach Neuladen; Filter „nur beendete Incidents“ clientseitig (API kennt keinen Parameter); Shoutrrr-URL-Formate nicht gegen echte Dienste geprüft |
| AP-1.16 | Geheimnisse in Monitor-Konfigurationen: Verschlüsselung und Maskierung der `writeOnly`-Felder, Behalten-Semantik bei PUT, Klonen, Bestandsmigration, Editor-Anpassung | FA-SEC-SEC, FA-API-06 | 0015 | ✅ Paket `internal/configsecret`: `writeOnly`-Felder des Typ-Schemas (auch Map-Werte/Array-Elemente) werden vor dem Raft-Vorschlag einzeln verschlüsselt (AAD `monitors/<id>/config<pointer>`) und in `monitors.secrets_json` abgelegt, `config` enthält `***`; FSM lehnt Klartext ab; API maskiert für alle Rollen (auch noch nicht migrierte Altbestände); PUT behält/ersetzt/entfernt, Typwechsel verwirft, Klonen verschlüsselt neu; Scheduler und Test-Endpunkt entschlüsseln nur im Speicher; Leader-Job migriert Altbestände. HTTP-Header-Werte jetzt `writeOnly`; Test erzwingt `writeOnly` für Feldnamen wie password/token/secret/apiKey. Editor: „gespeichert – verborgen“ mit Ersetzen/Entfernen. Ende-zu-Ende geprüft: Klartext weder in `state.db`, WAL, Raft-Log noch Log. Pflichtenheft v1.2. Offen: Neuverschlüsselung bei DEK-Rotation (gemeinsam mit Kanälen, AP-1.3-Folgearbeit); Tokens in URL-Query-Parametern bleiben ungeschützt (dokumentiert) |
| AP-1.15 | Lasttest (1.000 Monitore), Abnahme AS-01, AS-10, Compose-Doku | NFA-PERF-01..04, NFA-USE-01 | – | ✅ Abgenommen, Bericht [docs/abnahme/p1-kern.md](abnahme/p1-kern.md): 1.000 Monitore à 60 s im Container mit 2 vCPU/1 GB: CPU Ø 3,6 %, RAM 68 MiB, Monitorliste p95 66,9 ms, Uptime p95 1,6 ms, Statuswechsel p95 17 ms; AS-01 per Compose bestanden (Alert nach 40,9 s, Grenze 70 s); Compose-Inbetriebnahme 7 s bis bereit, Anleitung `docs/betrieb/compose.md`; Playwright-E2E 6/6. Dabei behoben: N+1-Abfragen der Monitorliste, Abfrageplan der Heartbeat-Seiten. Werkzeuge: `test/load/loadtest`, `test/load/gendata`, `test/acceptance/as01`, `make web-e2e`. AS-10 nach AP-1.17 bestanden |
| AP-1.17 | Kompaktes Metrik-Schema (Serien-Schlüssel, WITHOUT ROWID, Ganzzahl-Latenzen), Migration, AS-10 erneut messen | AS-10, FA-UPT-03..06 | 0016 | ✅ Tabelle `series` (Mandant, Monitor, Node → lokale Ganzzahl-ID), `rollups`/`minutes` als WITHOUT ROWID mit Ganzzahl-Latenzen (µs), Migration 00003 kopiert Bestandsdaten; Lesezugriffe filtern den Mandanten in SQL; Retention je Serie über den Primärschlüssel in Batches. Minuten-Zeile 224 → 30,5 B. AS-10: 2,2 GB nach 30 Tagen bei 1.000 Monitoren, Monitorliste p95 96 ms, Uptime p95 2 ms |

## P2 – Cluster

| AP | Inhalt | Anforderungen | ADR | Status |
|----|--------|---------------|-----|--------|
| AP-2.1 | Cluster-PKI: CA beim Bootstrap, Join-Tokens mit CA-Fingerprint, Zertifikatsrotation, Sperrliste | FA-PKI-01..04 | 0006 | ✅ Paket `internal/pki`: ECDSA-P-256-CA, Blattzertifikate 90 Tage (−5 min, Erneuerung bei 2/3) mit Identität als URI-SAN `uc://<cluster>/<rolle>/<node>[?tenant=]`, CSR liefert nur den Schlüssel; Prüfung von Kette, Verwendung, Ablauf, Sperrliste, Cluster und Rolle in `VerifyConnection` (TLS 1.3, Session-Tickets aus); Join-Token `uc1.<id>.<secret>.<ca-fingerprint>` (SHA-256 des Geheimnisses, Pinning vor dem ersten Byte). Raft-Zustand: CA (Schlüssel verschlüsselt), Join-Tokens (≤ 24 h, einmalig, Rolle, Mandant nur für Probes und nur existierend), Sperrliste mit Pruning. `internal/clusterpki`: CA beim Bootstrap, Token-Ausgabe und -Einlösung, Node-Zertifikat in `DATA_DIR/tls` mit Erneuerung, Sperrliste alle 10 s neu geladen. Offen → AP-2.2: Raft-Transport auf mTLS (ALPN-Mux), gRPC `Join`/`RenewCertificate`, Erneuerung von Followern über den Leader, Widerruf aller Zertifikate eines entfernten Nodes (Serien beim Ausstellen erfassen) |
| AP-2.2 | Join/Leave: Nicht-Voter → Voter, serialisierte Mitgliedschaftsänderungen, manuelles Entfernen, Adresswechsel | FA-CLU-03/04/08/10/11 | 0005 | ✅ Cluster-Port mit TLS 1.3/mTLS und ALPN (`uc-raft/1` nur für Voter, `h2` für gRPC mit Berechtigung je RPC), Raft darüber statt In-Memory-Transport, Verbindungen max. 1 h. Join mit `CLUSTER_JOIN` + `JOIN_TOKEN(_FILE)`: CA-Pinning, CSR, Master-Key-Übergabe (nur erzeugter Schlüssel, O_EXCL + Fingerprint), Join über Follower wird einmal an den Leader weitergeleitet; Bootstrap nur ohne Raft-Zustand, ohne `CLUSTER_JOIN` und ohne Cluster-Zertifikat. Leader: Adressprüfung vor Token-Einlösung (409 mit alter Node-ID), Nicht-Voter → Voter erst nach Anwenden des eigenen Eintrags und < 100 Rückstand (Raft-Applied-Index, da Konfigurationseinträge die FSM nicht erreichen), serialisiert mit `prevIndex`, Fortsetzung nach Leaderwechsel; Adresswechsel durch den Node selbst per Zertifikat (FA-CLU-10); manuelles Entfernen (nicht Leader selbst, nicht letzter Voter, Warnung < 3/gerade) mit Sperre aller Zertifikate des Nodes (`RevokeNode`); Follower erneuern ihr Zertifikat über den Leader, `RenewCertificate` prüft Mitgliedschaft/Sperre gegen den Zustand. Admin-API (nur platform-admin): Join-Tokens, Node-Liste mit Warnungen, Entfernen. Betriebsdoku `docs/betrieb/cluster.md`. Integrationstest 3 Voter über mTLS inkl. Adresswechsel, Konflikt, Sperre; Zwei-Binary-Test über Admin-API. Offen: Admin-UI (AP-2.8), Systemalert bei Promotion-Timeout (AP-3.6) |
| AP-2.3 | Write-Forwarding zum Leader mit Request-ID | FA-CLU-05/06 | 0006 | ✅ `Node.Apply` leitet auf Followern per gRPC `ClusterService.Apply` (nur Voter-Zertifikate) an den Leader weiter, Request-ID unverändert; Leader wendet mit `ApplyLocal` an und leitet nie erneut weiter. Frist 3 s inkl. Warten, bis der Follower den Eintrag selbst angewendet hat (Read-after-Write); `LastContact` älter als 2 × `HeartbeatTimeout` → sofort `ErrNotApplied` (503). Fehlerabbildung: kein Leader/`FAILED_PRECONDITION` → interne Wiederholung mit Backoff (50–500 ms), Transport- und sonstige Fehler → `ErrUnknownOutcome` (503 „Result Unknown“), deterministische Ablehnungen als `error_code`/`error_message` in `ApplyResponse` → `*CommandError`. Eine gecachte gRPC-Verbindung zum aktuellen Leader. Integrationstest über mTLS: Schreiben über Follower, Dedup über anderen Follower, Ablehnung, Wiederholung nach Leaderwechsel ohne Duplikat (PF-04). Umsetzung durch Haiku-Agenten; Berechtigung von `Apply` vom Hauptagenten |
| AP-2.4 | Heartbeat-Replikation, Anti-Entropie, Backpressure, Rückstandsmessung | FA-REP-01..05 | 0006 | ✅ Paket `internal/replication`. Push: je Peer ein Stream mit eigenen Heartbeats, Best Effort (Puffer 64 Batches; voll oder Sendefehler → Batch verworfen, Stream neu, Backoff 1–30 s mit Jitter), `Publish` blockiert nie (FA-REP-04/07). Anti-Entropie je Peer sequenziell alle 30 s, beim Öffnen eines Push-Streams durch den Peer, nach eigenem Reconnect und bei Raft-Ereignissen auf dem Leader (B6): `GetWatermarks` → `FetchRange(after, until_seq, 5000)` für alle Ströme, die der Peer kennt (B3). Position je Strom in `metrics.db` (`stream_positions`): `InsertHeartbeats` schiebt sie über lückenlose Folgen, eine geholte Seite übernimmt die Position des Peers, der eigene Strom gilt bis `LastSeq` – Lücken durch Verdichtung oder verlorene eigene Batches blockieren so nicht (B2). Eingehende Heartbeats werden geprüft (Pflichtfelder, Größen, JSON), den eigenen Strom schreibt nur der Erzeuger. Alle Replikations-RPCs nur für Voter, Stream-RPCs jetzt mit eigenem Interceptor. Rollups und Verdichtung warten auf vollständige Replikation, höchstens 1 h. Rückstand je Peer (Zeit seit letztem vollständigem Abgleich) und letzter Fehler in der Node-Liste der Admin-API. `Host.Close` beendet offene Streams nach 5 s. Tests: Store-Konformität, Paket-Tests (Lücken, Paging, verworfene Batches, gedrosselter Peer), Integrationstest 3 Voter über mTLS. Offen: Systemalert bei Rückstand ≥ 5 min (FA-REP-05, AP-3.6), Prüfsummenvergleich über alle Voter (FA-REP-02-Abnahme, Chaos-Test AP-2.9), Inkarnationswechsel-Alert (FA-REP-06, P3), Probe-Heartbeats (P5), Verhalten bei 1 h Trennung unter Last (AP-2.9) |
| AP-2.5 | Zuweisung `all`/`count`/`nodes`, Consistent Hashing, Capabilities | FA-SCH-01/02/06, FA-CHK-31 | – | ⬜ |
| AP-2.6 | Rolling Updates: Feature-Gates über Raft, Expand/Contract-Migrationen | FA-CLU-12, NFA-PORT-04 | ggf. neu | ⬜ |
| AP-2.7 | Recovery-CLI und Runbook (Quorumverlust, Peers-Override) | FA-OBS-04, FA-CLU-08 | – | ⬜ |
| AP-2.8 | Admin-UI: Cluster-Ansicht, Join-Tokens, Node entfernen | FA-UI-05 | – | ⬜ |
| AP-2.9 | Chaos-Test-Harness (Compose, Partition, Kill, Clock-Skew), Abnahme AS-02/03/09 | NFA-REL-01..05 | – | ⬜ |

## P3 – HA-Logik

| AP | Inhalt | Anforderungen | Status |
|----|--------|---------------|--------|
| AP-3.1 | Konsens-Status (majority/any/all), PARTIAL, UNKNOWN, Bewertung auf dem Leader | FA-CON-01/02/05/06 | ⬜ |
| AP-3.2 | Node-Selbstgesundheit (Referenzziele, Ausschluss aus Bewertung) | FA-CON-04 | ⬜ |
| AP-3.3 | Leader-Alerting mit Dedup über Raft, Fortsetzung der Outbox nach Leaderwechsel | FA-ALR-03 | ⬜ |
| AP-3.4 | Rate-Limits, Bündelung, dauerhafte Fehler, Flapping | FA-ALR-04..06 | ⬜ |
| AP-3.5 | Degraded-Modus, Absender-Sicht, Korrektur nach Quorum-Rückkehr, Neubewertung | FA-ALR-10/11, FA-UPT-07 | ⬜ |
| AP-3.6 | Selbstüberwachung, Systemalerts, Prometheus-Metriken | FA-OBS-01/02, FA-REP-05/06, FA-PKI-06 | ⬜ |
| AP-3.7 | Abnahme AS-04, AS-05 | NFA-REL-03/06 | ⬜ |

## P4 – Mandanten

| AP | Inhalt | Anforderungen | Status |
|----|--------|---------------|--------|
| AP-4.1 | Zitadel-Anbindung (JWT-Validierung, Claims, JWKS-Cache), Angular-OIDC | FA-AUTH-05..07 | ⬜ (ADR nötig) |
| AP-4.2 | Mandantenverwaltung, Pläne, Limits, Auto-Provisionierung | FA-TEN-03/04 | ⬜ |
| AP-4.3 | SSRF-Durchsetzung inkl. Fuzz-Tests, Notification-Ziele | FA-SEC-SSRF, FA-ALR-12 | ⬜ |
| AP-4.4 | API-Tokens, Audit-Log, Massenaktionen | FA-AUTH-10, FA-DAT-01, FA-MON-06 | ⬜ |
| AP-4.5 | Migration lokal → Zitadel | FA-AUTH-08 | ⬜ |
| AP-4.6 | Admin-UI Mandanten, Abnahme AS-06/07 | FA-UI-05 | ⬜ |

## P5 – Reichweite

| AP | Inhalt | Anforderungen | Status |
|----|--------|---------------|--------|
| AP-5.1 | Probe-Rolle: Subscribe, Upload, Puffer, Keepalive, Failover | FA-PRB-01..04/07 | ⬜ |
| AP-5.2 | Private Probes pro Mandant, Probe-Alerts | FA-PRB-05/06 | ⬜ |
| AP-5.3 | Externes Frontend + Client-Failover | FA-UI-02/03 | ⬜ |
| AP-5.4 | PostgreSQL-Backend mit gemeinsamer Testsuite, Lease-basierte Leaderwahl | Z-S-01, NFA-PORT-03 | ⬜ (ADR nötig) |
| AP-5.5 | Backup/Export/Import, CA-Rotation | FA-OBS-03, FA-PKI-05 | ⬜ |
| AP-5.6 | Abnahme AS-08, Release 1.0 (Namensprüfung O-08) | – | ⬜ |

## P6 – Ausbau

| AP | Inhalt | Status |
|----|--------|--------|
| AP-6.1 | Statusseiten pro Mandant | ⬜ |
| AP-6.2 | Push-/Heartbeat-Monitore | ⬜ |
| AP-6.3 | Weitere Typen (Zertifikats-/Domain-Ablauf, gRPC-Health, SMTP/IMAP, DB, MQTT) | ⬜ |
| AP-6.4 | Node-Labels, Proxy pro Monitor, Gruppen-Monitore | ⬜ |
