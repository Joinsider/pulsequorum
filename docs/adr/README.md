# Architecture Decision Records

Ablauf und Regeln: siehe [CLAUDE.md – Regel 2](../../CLAUDE.md#regel-2-adr-vor-jeder-großen-änderung). Vorlage: [0000-template.md](0000-template.md).

Nur der Projektinhaber setzt einen ADR auf **Accepted**.

| Nr. | Titel | Status | Datum |
|-----|-------|--------|-------|
| [0001](0001-repository-struktur-und-build.md) | Repository-Struktur, Build und Einbettung der SPA | Proposed | 2026-10-19 |
| [0002](0002-go-http-stack.md) | Go-HTTP-Stack, Konfiguration und Logging | Proposed | 2026-10-19 |
| [0003](0003-api-vertrag-und-codegenerierung.md) | API-Vertrag (OpenAPI) und Codegenerierung | Proposed | 2026-10-19 |
| [0004](0004-persistenz.md) | Persistenzschicht (SQLite, Schema, Migrationen, Repository) | Proposed | 2026-10-19 |
| [0005](0005-konsens-raft.md) | Konsens mit hashicorp/raft und FSM-Design | Proposed | 2026-10-19 |
| [0006](0006-inter-node-kommunikation.md) | Inter-Node-Kommunikation, mTLS und Heartbeat-Replikation | Proposed | 2026-10-19 |
| [0007](0007-frontend-stack.md) | Frontend-Stack | Proposed | 2026-10-19 |
| [0008](0008-lokale-authentifizierung.md) | Lokale Authentifizierung und Token-Handling in der SPA | Proposed | 2026-10-19 |
| [0009](0009-ci-cd-und-release.md) | CI/CD, Qualitäts-Gates und Release | Proposed | 2026-10-19 |
| [0010](0010-lizenzierung.md) | Lizenzierung, CLA und Abhängigkeitslizenzen | Proposed | 2026-10-19 |
| [0011](0011-geheimnisverwaltung.md) | Verschlüsselung von Geheimnissen (Envelope Encryption) | Proposed | 2026-10-19 |
