# Roadmap – UptimeCluster

Arbeitsgrundlage für die Umsetzung. Anforderungen: [Pflichtenheft](pflichtenheft.md). Entscheidungen: [ADRs](adr/README.md). Regeln: [CLAUDE.md](../CLAUDE.md).

**Nutzung:** Vor jeder Arbeit „Aktueller Stand“ lesen. Arbeitspakete (AP) werden in Reihenfolge bearbeitet, sofern nicht blockiert. Nach Abschluss Status setzen und im selben Commit mitführen. Ein AP mit ADR-Abhängigkeit startet erst, wenn der ADR **Accepted** ist.

**Status:** ⬜ offen · 🔄 in Arbeit · ⏳ wartet auf Review · ⛔ blockiert · ✅ erledigt

---

## Aktueller Stand

| | |
|---|---|
| **Aktive Phase** | P0 – Fundament |
| **Wartet auf Projektinhaber** | – |
| **Nächster Schritt (Claude)** | AP-0.10–0.13, AP-0.15; AP-0.14 (Raft-Spike) läuft in eigener Session |

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
| AP-0.8 | Build: Makefile, SPA-Einbettung per `go:embed` mit Platzhalter, Cache-Header, SPA-Fallback, Dockerfile (distroless, nonroot), Compose Single | FA-UI-01, NFA-PERF-07, PF-38, PF-39 | ADR 0001 | 🔄 Dockerfile noch nicht gebaut (lokal kein Docker-Daemon) |
| AP-0.9 | CI-Grundpipeline: Build, Tests, `go vet`, `gofmt`, `govulncheck`, Frontend-Build/-Test | NFA-MNT-02, NFA-SEC-06 | ADR 0009 (Basis) | 🔄 Workflow angelegt, noch nicht auf GitHub gelaufen (kein Remote) |
| AP-0.10 | CI-Ausbau: `golangci-lint`, Lizenzprüfung, Multi-Arch-Image nach GHCR, Release-Workflow | NFA-PORT-01, RB-LIC-03 | ADR 0009 | 🔄 Lokal umgesetzt (`make lint`, `make licenses`, `make notices`, `make generate-check`, `goreleaser check` grün); CI- und Release-Lauf auf GitHub noch ausstehend (kein Remote) |
| AP-0.11 | Lizenz: `LICENSE` (AGPLv3), SPDX-Header, `LICENSING.md`, `CONTRIBUTING.md` | RB-LIC-01/02 | ADR 0010 | ✅ CLA-Assistant-Einrichtung erst mit GitHub-Remote + anwaltlich geprüftem CLA-Text; `THIRD_PARTY_NOTICES` → AP-0.10; Quellcode-Link in der UI (RB-LIC-04) → AP-1.14 |
| AP-0.12 | OpenAPI-Entwurf für alle P1-Endpunkte + Codegenerierung (Server/Client) | FA-API-01..07 | ADR 0003 | ✅ `make generate`, `make api-lint`; alle Operationen außer Health antworten 501 bis zur Umsetzung in P1 |
| AP-0.13 | Protobuf-Entwurf: Cluster-, Replikations-, Probe-Service | Kap. 9.3 | ADR 0006 | ✅ `proto/uptimecluster/v1`, `buf lint` (STANDARD), Codegen nach `internal/pb`; FSM-Payloads werden in AP-1.2 verfeinert |
| AP-0.14 | **Spike Raft** (`spikes/raft`): 3 Container, Konfiguration per Raft, Heartbeats per Replikation inkl. Inkarnation, Leader-Kill, Partition 2\|1, Bootstrap-Erkennung | FA-CLU-01..07, FA-REP-01..03 | – (Spike) | ⬜ |
| AP-0.15 | **Spike SQLite**: Benchmark `modernc.org/sqlite` vs. `ncruces/go-sqlite3` mit Heartbeat-Last (Batch-Insert, Range-Query, Retention-Delete) | PF-22..24, O-07 | – (Spike) | ⬜ |

**Hinweis zu AP-0.6–0.9:** Das Grundgerüst wurde vor dem ADR-Review nach den damals vorgeschlagenen ADRs aufgesetzt; alle ADRs wurden unverändert angenommen, es ist keine Anpassung nötig.

---

## P1 – Kern (Single-Voter)

| AP | Inhalt | Anforderungen | ADR | Status |
|----|--------|---------------|-----|--------|
| AP-1.1 | Storage-Schicht: Repository-Interfaces mit Tenant-Scope, SQLite (Schreib-/Lese-Pool, Pragmas + Selbsttest), Migrationen (`auto_vacuum=INCREMENTAL`), Mandanten-Isolationstests | FA-TEN-01, FA-NODE-06, NFA-SEC-02, PF-22..24 | 0004 | ⬜ |
| AP-1.2 | Node-Identität (Node-ID, Inkarnation), Raft mit einem Mitglied, FSM-Befehle, Request-Dedup, Snapshots per Backup-API, Determinismus-Test | FA-NODE-03, FA-CLU-01/02/06/07/09 | 0005 | ⬜ |
| AP-1.3 | Geheimnisverschlüsselung (Envelope, KEK/DEK, Rotation) | FA-SEC-SEC | 0011 | ⬜ |
| AP-1.4 | Checker-Registry, zentraler Dialer mit SSRF-Hook, HTTP-Checker (frische Verbindungen, Phasen-Timeouts, TLS-Prüfung, Fehlerklassen) | FA-CHK-01..04, 10..15 | – | ⬜ |
| AP-1.5 | TCP-, DNS- (TC→TCP, Mengenvergleich; `miekg/dns` v1 – v2 auf Codeberg noch pre-1.0) und ICMP-Checker (Modi, Selbsttest) inkl. Container-Doku | FA-CHK-20, 30..32, 40/41 | – | ⬜ |
| AP-1.6 | Scheduler: Jitter inkl. Erstlauf, keine Selbstüberlappung, Nebenläufigkeitslimit, Goroutine-Leak-Tests | FA-SCH-01/03/04/05/07 | – | ⬜ |
| AP-1.7 | Monitor-API (CRUD, Pause, Klonen, Typen mit JSON-Schema), Problem Details, Paginierung | FA-MON-*, FA-API-01..03/06 | 0003 | ⬜ |
| AP-1.8 | Statuslogik Single-Node (Retries, PENDING, DEGRADED), Incidents | FA-CON-03, FA-INC-01/02 | – | ⬜ |
| AP-1.9 | Heartbeat-Speicherung, Uptime-Berechnung, Rollups, Verdichtung, Retention in Batches | FA-UPT-01..06 | 0004 | ⬜ |
| AP-1.10 | Benachrichtigungen: Outbox, Shoutrrr-Kapselung, Webhook mit JSON-sicheren Templates, Testversand, Maskierung | FA-ALR-01..03/07..09/13 | – | ⬜ |
| AP-1.11 | Wartungsfenster (einmalig, wiederkehrend, Zeitzonen) | FA-MNT-01/02 | – | ⬜ |
| AP-1.12 | Lokale Authentifizierung: Setup-Flow, Argon2id, JWT (EdDSA), Refresh-Rotation, Drosselung, Rollenprüfung | FA-AUTH-01..04/09, NFA-SEC-03/04 | 0008 | ⬜ |
| AP-1.13 | WebSocket-Stream mit Ticket, Mandantenfilter, CORS, Rate-Limiting, Security-Header | FA-API-04/05/07, FA-AUTH-11, FA-TEN-05, NFA-SEC-05 | 0002 | ⬜ |
| AP-1.14 | Frontend: Shell, Login/Setup, Dashboard, Monitor-Detail (ECharts), Monitor-Editor (JSON-Schema), Kanäle, Wartung, Incidents, Live-Updates | FA-UI-04/06..09 | 0007 | ⬜ |
| AP-1.15 | Lasttest (1.000 Monitore), Abnahme AS-01, AS-10, Compose-Doku | NFA-PERF-01..04, NFA-USE-01 | – | ⬜ |

## P2 – Cluster

| AP | Inhalt | Anforderungen | ADR | Status |
|----|--------|---------------|-----|--------|
| AP-2.1 | Cluster-PKI: CA beim Bootstrap, Join-Tokens mit CA-Fingerprint, Zertifikatsrotation, Sperrliste | FA-PKI-01..04 | 0006 | ⬜ |
| AP-2.2 | Join/Leave: Nicht-Voter → Voter, serialisierte Mitgliedschaftsänderungen, manuelles Entfernen, Adresswechsel | FA-CLU-03/04/08/10/11 | 0005 | ⬜ |
| AP-2.3 | Write-Forwarding zum Leader mit Request-ID | FA-CLU-05/06 | 0006 | ⬜ |
| AP-2.4 | Heartbeat-Replikation, Anti-Entropie, Backpressure, Rückstandsmessung | FA-REP-01..05 | 0006 | ⬜ |
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
