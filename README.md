# UptimeCluster

Hochverfügbarer, mandantenfähiger, selbst hostbarer Uptime-Monitor. Läuft als Cluster aus 1…N Containern auf verschiedenen Hosts (Raft-Konsens, Heartbeat-Replikation, Probes hinter NAT).

> **Status:** Phase 0 – Fundament. Noch nicht einsatzbereit. Stand und nächste Schritte: [docs/roadmap.md](docs/roadmap.md).

## Dokumentation

| Dokument | Inhalt |
|----------|--------|
| [Konzept](uptime-cluster-konzept.md) | Ursprüngliches Konzept v0.3 |
| [Pflichtenheft](docs/pflichtenheft.md) | Verbindliche Anforderungen, Pitfalls, Abnahmekriterien |
| [Roadmap](docs/roadmap.md) | Phasen und Arbeitspakete |
| [ADRs](docs/adr/README.md) | Architekturentscheidungen |

## Entwicklung

Voraussetzungen: Go 1.27, Node.js 24.15+ oder 26, npm.

```bash
make help        # alle Ziele
make build       # SPA bauen und ins Binary einbetten → bin/uptimecluster
make test        # Go- und Frontend-Tests
make lint        # go vet, gofmt, prettier
make run         # lokaler Single-Voter auf http://localhost:8080
make web-dev     # Angular-Dev-Server auf :4200 (API-Proxy auf :8080)
```

Container: `docker compose -f deploy/compose.single.yaml up --build`

## Lizenz

Geplant: AGPLv3 + kommerzielle Lizenz (siehe ADR 0010, noch nicht angenommen).
