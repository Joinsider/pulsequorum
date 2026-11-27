<p align="center"><img src="docs/assets/pulsequorum-mark.svg" alt="Pulsequorum" width="160"></p>

<h1 align="center">Pulsequorum</h1>

<p align="center">Hochverfügbarer, mandantenfähiger Uptime-Monitor zum Selbsthosten</p>

> **Im Aufbau.** Pulsequorum ist noch nicht für den Produktivbetrieb freigegeben. `v0.*`-Versionen sind Vorabversionen. Unterstützte Versionen und Sicherheitsmeldungen: siehe [SECURITY.md](SECURITY.md).

## Was ist Pulsequorum?

Pulsequorum prüft die Erreichbarkeit von Diensten von mehreren Knoten aus. Die Knoten bilden einen Raft-Cluster: Ein Monitor gilt erst dann als ausgefallen, wenn sich ein Quorum einig ist. Netzprobleme an einem einzelnen Standort erzeugen so keinen Fehlalarm, und der Ausfall eines Knotens unterbricht die Überwachung nicht.

- **Monitortypen:** HTTP(S) (auch mit Client-Zertifikat/mTLS), TCP, DNS und ICMP
- **Konsens-Status:** Heartbeats werden zwischen den Knoten repliziert, Statuswechsel und Incidents laufen über Raft
- **Alarme:** Benachrichtigungskanäle (Shoutrrr-URLs, Webhooks) mit Bündelung, Rate-Limits und Wartungsfenstern
- **Uptime:** Rollups, Latenzen je Knoten und Aufbewahrungsfristen
- **Mandanten:** Rollen, Limits, Anmeldung lokal oder über OIDC (z. B. Zitadel), API-Tokens und Audit-Log
- **Sicherheit:** SSRF-Schutz für alle ausgehenden Prüfungen und Benachrichtigungen, verschlüsselte Geheimnisse, die in der API maskiert werden
- **Betrieb:** Prometheus-Metriken, Selbstüberwachung des Clusters, ein Binary mit eingebetteter Web-Oberfläche

**In Arbeit / geplant (P5):** Probes hinter NAT, externes Frontend mit Failover, PostgreSQL-Backend (zunächst Vorarbeiten), Backup und Restore. Weitere Monitortypen und Statusseiten folgen in P6.

## Stand

| Phase | Inhalt | Status |
|-------|--------|--------|
| P0 – Fundament | Grundgerüst, Architekturentscheidungen, Spikes | ✅ abgeschlossen |
| P1 – Kern | Single-Voter mit Checks, Uptime, Alarmen, lokalen Nutzern | ✅ abgenommen |
| P2 – Cluster | Join/Leave, mTLS, Heartbeat-Replikation | ✅ abgenommen |
| P3 – HA-Logik | Konsens-Status, Leader-Alarme, Degraded-Modus, Selbstüberwachung | ✅ abgenommen |
| P4 – Mandanten | OIDC, Rollen, Limits, SSRF-Durchsetzung, Admin-UI | ✅ abgenommen |
| P5 – Reichweite | Probes, externes Frontend, Backup; Release 1.0 | 🔄 in Arbeit |
| P6 – Ausbau | Statusseiten, Push-Monitore, weitere Typen, Node-Labels | ⬜ geplant |

Details und Abnahmeberichte: [docs/roadmap.md](docs/roadmap.md), [docs/abnahme/](docs/abnahme/).

## Schnellstart

Ein Knoten mit Docker oder Podman Compose, Anleitung in [docs/betrieb/compose.md](docs/betrieb/compose.md):

```bash
docker compose -f deploy/compose.single.yaml up -d --build
docker compose -f deploy/compose.single.yaml logs uptime | grep setup_token
```

Danach `http://localhost:8080/setup` öffnen und den ersten Administrator anlegen.

## Dokumentation

| Dokument | Inhalt |
|----------|--------|
| [Konzept](pulsequorum-konzept.md) | Ursprüngliches Konzept v0.3 |
| [Pflichtenheft](docs/pflichtenheft.md) | Verbindliche Anforderungen und Abnahmekriterien |
| [Roadmap](docs/roadmap.md) | Phasen, Arbeitspakete und aktueller Stand |
| [ADRs](docs/adr/README.md) | Architekturentscheidungen |
| [Abnahmeberichte](docs/abnahme/) | Nachweise je Phase |
| [Single-Node mit Compose](docs/betrieb/compose.md) | Einzelknoten-Betrieb und Konfiguration |
| [Cluster erweitern und verkleinern](docs/betrieb/cluster.md) | Voter hinzufügen, entfernen und ersetzen |
| [Anmeldung über OIDC](docs/betrieb/oidc.md) | Zitadel und andere OIDC-Anbieter |
| [Externes Frontend und Client-Failover](docs/betrieb/frontend.md) | Auslieferung der SPA, Domains, Cookies, Failover |
| [API-Tokens, Audit-Log](docs/betrieb/api-tokens.md) | Tokens, Protokoll und Massenaktionen |
| [Recovery](docs/betrieb/recovery.md) | Quorumverlust und Admin-Passwort |
| [SSRF-Schutz](docs/betrieb/ssrf.md) | Zielprüfung und Ausnahmen |
| [ICMP im Container](docs/betrieb/icmp.md) | Ping-Monitore in Containern |
| [Client-Zertifikate (mTLS)](docs/betrieb/mtls-monitore.md) | HTTP-Monitore für Ziele mit mTLS |
| [Release](docs/betrieb/release.md) | Ablauf, Prüfung und Fehlerfälle |

## Entwicklung

Voraussetzungen: Go 1.27, Node.js 24.15+ oder 26, npm.

```bash
make help        # alle Ziele
make build       # SPA bauen und ins Binary einbetten → bin/pulsequorum
make test        # Go- und Frontend-Tests
make lint        # go vet, gofmt, golangci-lint, Prettier
make run         # lokaler Single-Voter auf http://localhost:8080
make web-dev     # Angular-Dev-Server auf :4200 (API-Proxy auf :8080)
```

Container: `docker compose -f deploy/compose.single.yaml up --build`

## Lizenz

[AGPL-3.0-only](LICENSE) + kommerzielle Lizenz, siehe [LICENSING.md](LICENSING.md). Externe Beiträge werden derzeit noch nicht angenommen, siehe [CONTRIBUTING.md](CONTRIBUTING.md).
