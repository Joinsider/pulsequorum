# Architecture Decision Records

Ablauf und Regeln: siehe [CLAUDE.md – Regel 2](../../CLAUDE.md#regel-2-adr-vor-jeder-großen-änderung). Vorlage: [0000-template.md](0000-template.md).

Nur der Projektinhaber setzt einen ADR auf **Accepted**.

| Nr. | Titel | Status | Datum |
|-----|-------|--------|-------|
| [0001](0001-repository-struktur-und-build.md) | Repository-Struktur, Build und Einbettung der SPA | Accepted | 2026-10-19 |
| [0002](0002-go-http-stack.md) | Go-HTTP-Stack, Konfiguration und Logging | Accepted | 2026-10-19 |
| [0003](0003-api-vertrag-und-codegenerierung.md) | API-Vertrag (OpenAPI) und Codegenerierung | Accepted | 2026-10-19 |
| [0004](0004-persistenz.md) | Persistenzschicht (SQLite, Schema, Migrationen, Repository) | Accepted | 2026-10-19 |
| [0005](0005-konsens-raft.md) | Konsens mit hashicorp/raft und FSM-Design | Accepted | 2026-10-19 |
| [0006](0006-inter-node-kommunikation.md) | Inter-Node-Kommunikation, mTLS und Heartbeat-Replikation | Accepted | 2026-10-19 |
| [0007](0007-frontend-stack.md) | Frontend-Stack | Accepted | 2026-10-19 |
| [0008](0008-lokale-authentifizierung.md) | Lokale Authentifizierung und Token-Handling in der SPA | Accepted | 2026-10-19 |
| [0009](0009-ci-cd-und-release.md) | CI/CD, Qualitäts-Gates und Release | Accepted | 2026-10-19 |
| [0010](0010-lizenzierung.md) | Lizenzierung, CLA und Abhängigkeitslizenzen | Accepted | 2026-10-19 |
| [0011](0011-geheimnisverwaltung.md) | Verschlüsselung von Geheimnissen (Envelope Encryption) | Accepted | 2026-10-19 |
| [0012](0012-praezisierungen-aus-spikes.md) | Präzisierungen aus den Spikes (Raft, SQLite) | Accepted | 2026-10-19 |
| [0013](0013-persistenz-ergebnisse-sqlite-spike.md) | Persistenz: Ergebnisse des SQLite-Spikes | Accepted | 2026-10-19 |
| [0014](0014-benachrichtigungen.md) | Benachrichtigungen: Kanäle, Outbox und Versand (Shoutrrr) | Accepted | 2026-10-19 |
| [0015](0015-geheimnisse-in-monitor-konfigurationen.md) | Geheimnisse in Monitor-Konfigurationen | Accepted | 2026-10-22 |
| [0016](0016-kompaktes-metrik-schema.md) | Kompaktes Schema für Latenz-Minuten und Rollups | Accepted | 2026-10-22 |
| [0017](0017-zuweisung-von-monitoren.md) | Zuweisung von Monitoren an Nodes, Node-Register und Node-Gesundheit | Accepted | 2026-10-23 |
| [0018](0018-rolling-updates-und-feature-gates.md) | Rolling Updates: Befehlsversionen, Feature-Gate und Expand/Contract | Accepted | 2026-10-23 |
| [0019](0019-recovery-cli.md) | Recovery-CLI: Quorumverlust und Admin-Passwort | Accepted | 2026-10-25 |
| [0020](0020-uhrversatz-messen-und-testen.md) | Uhrversatz zwischen Nodes messen und testen | Accepted | 2026-11-15 |
| [0021](0021-konsens-bewertung-auf-dem-leader.md) | Konsens-Bewertung auf dem Leader in Bewertungsrunden | Accepted | 2026-11-16 |
| [0022](0022-node-selbstgesundheit.md) | Node-Selbstgesundheit (Konnektivität) und Ausschluss aus der Bewertung | Accepted | 2026-11-16 |
| [0023](0023-alert-drosselung-buendelung-flapping.md) | Alert-Drosselung, Bündelung, dauerhafte Fehler und Flapping | Accepted | 2026-11-16 |
