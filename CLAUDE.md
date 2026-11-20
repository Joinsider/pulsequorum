# CLAUDE.md – Pulsequorum

Hochverfügbarer, mandantenfähiger, selbst hostbarer Uptime-Monitor (Go-Backend, Angular-SPA, Raft-Cluster).
Ein Binary/Image für alle Rollen (`NODE_ROLE=voter|probe`).

## Maßgebliche Dokumente

| Dokument | Zweck |
|----------|-------|
| [pulsequorum-konzept.md](pulsequorum-konzept.md) | Ursprüngliches Konzept (v0.3) inkl. Entscheidungslog E1–E21 |
| [docs/pflichtenheft.md](docs/pflichtenheft.md) | Verbindliche Anforderungen (FA-/NFA-IDs). Bei Widerspruch zum Konzept gilt das Pflichtenheft. |
| [docs/roadmap.md](docs/roadmap.md) | Phasen, Arbeitspakete, Status. **Vor jeder Arbeit lesen, nach jeder Arbeit aktualisieren.** |
| [docs/adr/](docs/adr/) | Architecture Decision Records. Akzeptierte ADRs sind bindend. |

## Regel 1: Delegation an Haiku-Agenten

**Alle Entwicklungsarbeit** wird an Subagenten mit **Haiku 5.5** (`claude-haiku-5-5`, im Agent-Tool `model: "haiku"`) ausgelagert – **mit Ausnahme von sicherheitsrelevantem Code**. Der Hauptagent plant, schreibt die Vorgaben, prüft die Ergebnisse und trägt die Verantwortung.

**An Haiku auslagern:**
- Implementierung aller Pakete: Handler, Store, Schema und Migrationen, FSM-Befehle, Jobs, Scheduler, Checker, Angular-Komponenten, Konfiguration, Verdrahtung in `main`
- Tests aller Art (Unit, Integration, Tabellentests), Fehlersuche, Lint- und Formatierungs-Fixes, Umbenennungen
- Recherche: Web-Recherche, Doku/Changelogs/Versionen, GitHub-Issues, Lizenzen; Codebase-Suche und Zusammenfassungen
- Doku-Pflege nach Vorgabe (README-Abschnitte, Kommentare, Changelog)

**Nicht an Haiku auslagern (bleibt beim Hauptagenten):**
- Sicherheitsrelevanter Code: Authentifizierung/Autorisierung, mTLS/PKI, Mandantentrennung (Durchsetzung der Tenant-Grenze), SSRF-Schutz, Verschlüsselung und Umgang mit Klartext-Geheimnissen, Maskierung/Redaktion von Geheimnissen
- Planung und Design: Architekturentscheidungen, ADRs, Pflichtenheft, Roadmap, Zerlegung in Aufträge
- Review: Diffs der Haiku-Agenten lesen, Tests laufen lassen, Commit

**Vorgehen:**
- Prompts für Haiku-Agenten sind selbsterklärend: Kontext, betroffene Dateien, erwartetes Ergebnis, Akzeptanzkriterium (z.B. „`go test ./internal/checker/...` grün“).
- Unabhängige Aufgaben parallel starten (mehrere Agent-Aufrufe in einer Nachricht).
- Ergebnisse von Haiku-Agenten werden vor dem Commit vom Hauptagenten gesichtet (Diff lesen, Tests laufen lassen).
- Berührt eine Aufgabe Sicherheitscode, schreibt der Hauptagent diesen Teil (z.B. als Hilfsfunktion) zuerst; der Haiku-Agent nutzt ihn nur.
- Parallele Agenten arbeiten auf getrennten Dateien und Paketen; hängen Pakete voneinander ab, laufen die Aufträge nacheinander.

## Regel 2: ADR vor jeder großen Änderung

Vor jeder **großen Änderung** wird ein ADR angelegt. Umgesetzt wird erst, wenn der Status **Accepted** ist. **Nur der Projektinhaber setzt einen ADR auf `Accepted`** – Claude setzt niemals selbst Accepted.

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
cmd/pulsequorum/   main – startet Voter oder Probe
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
make lint            # go vet, gofmt, golangci-lint, Prettier, OpenAPI- und Proto-Lint
make generate        # Code aus api/openapi.yaml und proto/ neu erzeugen (nie von Hand editieren)
make generate-check  # schlägt fehl, wenn generierter Code veraltet ist
make licenses        # Lizenzprüfung Go + npm gegen die Allow-List (ADR 0010)
make run             # Single-Voter lokal starten (Port 8080)
make web-dev         # Angular-Dev-Server mit Proxy auf :8080
```

Generierter Code: `internal/api/gen`, `internal/api/unimplemented.go`, `internal/pb`, `web/src/app/api/gen`. Neue REST-Operationen zuerst in `api/openapi.yaml`, dann `make generate`; die Operation antwortet 501, bis `api.Handlers` die Methode implementiert. Generator-Werkzeuge liegen in `tools/go.mod` (`go tool -modfile=tools/go.mod …`).

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
