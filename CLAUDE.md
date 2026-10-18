# CLAUDE.md – UptimeCluster

Hochverfügbarer, mandantenfähiger, selbst hostbarer Uptime-Monitor (Go-Backend, Angular-SPA, Raft-Cluster).
Ein Binary/Image für alle Rollen (`NODE_ROLE=voter|probe`).

## Maßgebliche Dokumente

| Dokument | Zweck |
|----------|-------|
| [uptime-cluster-konzept.md](uptime-cluster-konzept.md) | Ursprüngliches Konzept (v0.3) inkl. Entscheidungslog E1–E21 |
| [docs/pflichtenheft.md](docs/pflichtenheft.md) | Verbindliche Anforderungen (FA-/NFA-IDs). Bei Widerspruch zum Konzept gilt das Pflichtenheft. |
| [docs/roadmap.md](docs/roadmap.md) | Phasen, Arbeitspakete, Status. **Vor jeder Arbeit lesen, nach jeder Arbeit aktualisieren.** |
| [docs/adr/](docs/adr/) | Architecture Decision Records. Akzeptierte ADRs sind bindend. |

## Regel 1: Delegation an Haiku-Agenten

Einfache, klar abgegrenzte Aufgaben werden an Subagenten mit **Haiku 5.5** (`claude-haiku-5-5`, im Agent-Tool `model: "haiku"`) ausgelagert. Der Hauptagent plant, prüft die Ergebnisse und trägt die Verantwortung.

**An Haiku auslagern:**
- Recherche: Web-Recherche, Doku/Changelogs/Versionen nachschlagen, GitHub-Issues durchsuchen, Lizenzen prüfen
- Codebase-Suche und Zusammenfassungen („wo wird X benutzt?“)
- Einfache Programmieraufgaben mit klarer Vorgabe: Boilerplate, DTOs, Mapper, einfache CRUD-Handler nach vorhandenem Muster, Tabellentests für reine Funktionen, Angular-Komponenten nach vorhandenem Muster, Umbenennungen, Formatierungs- und Lint-Fixes
- Doku-Pflege nach Vorgabe (README-Abschnitte, Kommentare, Changelog)

**Nicht an Haiku auslagern (bleibt beim Hauptagenten):**
- Architektur- und Designentscheidungen, ADRs, Pflichtenheft, Roadmap
- Cluster-Layer (Raft-FSM, Replikation, Anti-Entropie, Leader-Logik), Konsens- und Uptime-Berechnung
- Sicherheitsrelevanter Code (Auth, mTLS/PKI, Mandantentrennung, SSRF-Schutz, Verschlüsselung)
- Datenbankschema und Migrationen
- Fehlersuche mit unklarer Ursache

**Vorgehen:**
- Prompts für Haiku-Agenten sind selbsterklärend: Kontext, betroffene Dateien, erwartetes Ergebnis, Akzeptanzkriterium (z.B. „`go test ./internal/checker/...` grün“).
- Unabhängige Aufgaben parallel starten (mehrere Agent-Aufrufe in einer Nachricht).
- Ergebnisse von Haiku-Agenten werden vor dem Commit vom Hauptagenten gesichtet (Diff lesen, Tests laufen lassen).

## Regel 2: ADR vor jeder großen Änderung

Vor jeder **großen Änderung** wird ein ADR angelegt. Umgesetzt wird erst, wenn der Status **Accepted** ist. **Nur der Projektinhaber (Projektinhaber) setzt einen ADR auf `Accepted`** – Claude setzt niemals selbst Accepted.

**Große Änderung** ist insbesondere:
- neue oder ausgetauschte Abhängigkeit mit Architektur-Einfluss (Framework, DB, Raft-Bibliothek, Auth-Bibliothek, Codegen-Tool)
- Änderungen an Datenmodell/Schema, Raft-FSM-Befehlen, Protobuf- oder OpenAPI-Verträgen (nicht rückwärtskompatibel)
- neue Komponente/Paket mit eigener Verantwortung, neue Monitor-Typen-Kategorie, neues Storage-Backend
- Sicherheits- oder Mandantenmodell, Lizenz, Deployment-/Release-Modell
- Abweichung vom Pflichtenheft oder von einem akzeptierten ADR

Keine ADR nötig: Bugfixes, Refactorings ohne Verhaltensänderung, Tests, Doku, Umsetzung innerhalb eines akzeptierten ADRs.

**Ablauf:**
1. ADR aus [docs/adr/0000-template.md](docs/adr/0000-template.md) als `docs/adr/NNNN-kurzer-titel.md` anlegen (fortlaufende Nummer), Status `Proposed`, in [docs/adr/README.md](docs/adr/README.md) eintragen.
2. Committen (`docs(adr): propose NNNN …`) und den Nutzer um Review bitten.
3. Warten. Arbeitspakete, die vom ADR abhängen, bleiben in der Roadmap auf `blockiert (ADR NNNN)`. Unabhängige Arbeit darf weiterlaufen.
4. Nach Approval durch den Nutzer: Status `Accepted` mit Datum (setzt der Nutzer oder Claude auf dessen ausdrückliche Anweisung im Chat), dann umsetzen.
5. Abgelehnt → `Rejected` mit Begründung. Ersetzt → alter ADR `Superseded by NNNN`, neuer ADR verweist zurück.

**Status:** `Proposed` → `Accepted` | `Rejected`; später `Deprecated` oder `Superseded by NNNN`.

Wegwerf-Prototypen (Spikes) liegen unter `spikes/`, dürfen ohne Approval entstehen und werden nie direkt in Produktcode übernommen.

## Regel 3: Git

- Commits bei jeder abgeschlossenen, logisch zusammenhängenden Änderung – nicht erst am Ende. Ein Commit = ein nachvollziehbarer Schritt (z.B. „Backend-Skelett“, „Frontend-Skelett“, „ADR 0003 vorgeschlagen“). Keine Sammel-Commits über unabhängige Themen.
- [Conventional Commits](https://www.conventionalcommits.org/) auf Englisch: `feat(checker): add TCP checker`, `docs(adr): propose 0004 storage layer`, `chore(build): …`. Body erklärt das *Warum*.
- Vor dem Commit: Build und Tests der betroffenen Teile grün.
- Kein Push, kein Force-Push, kein Umschreiben veröffentlichter Historie ohne ausdrückliche Anweisung.
- Hauptbranch: `main` (lokal derzeit `master`).

## Sprache

- Dokumentation (Pflichtenheft, Roadmap, ADRs, README): **Deutsch**.
- Code, Bezeichner, Code-Kommentare, Commit-Messages, API-Felder, Log-Meldungen: **Englisch**.

## Projektstruktur

```
cmd/uptimecluster/   main – startet Voter oder Probe
internal/            Go-Pakete (api, auth, checker, scheduler, aggregator, alert, cluster, store, tenant, config, …)
proto/               Protobuf-Definitionen (Inter-Node, Probe-Service)
api/openapi.yaml     REST-Vertrag zwischen Backend und Frontend
web/                 Angular-SPA (Build wird per go:embed eingebettet)
deploy/              Compose-Beispiele
docs/                Pflichtenheft, Roadmap, ADRs
spikes/              Wegwerf-Prototypen
```

## Befehle

```bash
make help            # Übersicht
make build           # Frontend bauen + Go-Binary mit eingebetteter SPA
make test            # Go- und Frontend-Tests
make lint            # go vet + gofmt-Check + Frontend-Lint
make run             # Single-Voter lokal starten (Port 8080)
make web-dev         # Angular-Dev-Server mit Proxy auf :8080
```

## Konventionen

**Go**
- Standardbibliothek zuerst; neue Abhängigkeiten nur mit Begründung (ggf. ADR) und kompatibler Lizenz (MIT, BSD, Apache-2.0, MPL-2.0, ISC).
- Explizite Abhängigkeiten über Konstruktoren, kein globaler Zustand außer der Checker-Registry.
- `context.Context` als erster Parameter bei I/O; Fehler mit `fmt.Errorf("…: %w", err)` wrappen.
- Logging nur über `log/slog`, strukturiert.
- Jede mandantenbezogene Repository-Methode erhält die Tenant-ID explizit; Tests prüfen die Trennung.
- Raft-FSM-`Apply` ist deterministisch: keine Uhrzeit, kein Zufall, kein I/O außer dem lokalen Store.
- Tests neben dem Code (`_test.go`), Tabellentests bevorzugt.

**Angular**
- Standalone-Komponenten, Signals, neue Control-Flow-Syntax, Lazy-Loading pro Feature.
- API-Zugriffe nur über Services; Backend-Basis-URL kommt aus der Laufzeit-`config.json`, nie hart codiert.

## Arbeitsweise

- Vor Beginn einer Aufgabe: [docs/roadmap.md](docs/roadmap.md) und relevante ADRs lesen.
- Nach Abschluss eines Arbeitspakets: Status in der Roadmap aktualisieren und im selben Commit mitführen.
- Anforderungs-IDs aus dem Pflichtenheft (z.B. `FA-CHK-03`) in Commit-Bodies und Tests referenzieren, wo sinnvoll.
