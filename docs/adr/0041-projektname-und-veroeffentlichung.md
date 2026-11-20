# ADR 0041: Projektname Pulsequorum und Veröffentlichung als öffentliches Repository

|                 |                                                                       |
|-----------------|-----------------------------------------------------------------------|
| **Status**      | Accepted                                                              |
| **Datum**       | 2026-11-20                                                            |
| **Entscheider** | Projektinhaber                                                              |
| **Bezug**       | O-08 (finaler Projektname), RB-LIC-01..05, ADR 0009, ADR 0010, ADR 0040 |

## Kontext

- Das Pflichtenheft verlangt den finalen Namen vor dem ersten öffentlichen Release (O-08). Der Arbeitsname
  „UptimeCluster“ kollidiert mit dem bestehenden selbst gehosteten Open-Source-Monitor „ClusterUptime“ (eigenes
  Docker-Image, gleiche Kategorie).
- Modulpfad (`github.com/joinsider/uptimecluster`) und Repository (`Joinsider/uptime-cluster`) passen nicht zusammen;
  der erste Release würde daran scheitern (ADR 0040, F5).
- Das Repository soll öffentlich werden (AGPL §13, kostenlose Sicherheitsfunktionen für öffentliche Repos, ADR 0040).
- Ein Scan aller 309 Commits auf allen Refs (gitleaks und Suche nach persönlichen Zeichenketten) fand keine echten
  Geheimnisse (zwei Fehlalarme: eingebettete OpenAPI-Spezifikation, Testpasswort). Gefunden wurden personenbezogene
  Daten: eine private E-Mail-Adresse als Autor in 224 Commits, der Vorname des Projektinhabers in Doku und Commit-Texten,
  zwei Schreibweisen des Autornamens.
- GitHub hält Commits über `refs/pull/*` jedes PRs dauerhaft erreichbar. Ein Force-Push bereinigter History in das
  bestehende Repository entfernt die alten Commits deshalb nicht; das kann nur der GitHub-Support.

## Optionen

### Frage 1: Name

Geprüft per Websuche (ohne Domains und Markenregister): Pulsequorum, Assentry, Wardenly, Heartwire hatten keine
sichtbaren Kollisionen; Vigil, Vigilo, Tribune, Quorra, Kvorum sind belegt.

- **1A Pulsequorum** – beschreibt den Kern (Heartbeats und Quorum-Bewertung).
- **1B UptimeCluster behalten** – kein Umbau, aber Namensnähe zu ClusterUptime.

### Frage 2: Veröffentlichung

- **2A Neues Repository aus bereinigter History** – altes Repository bleibt privat als Archiv mit allen PRs.
  - Vorteile: Keine alten Commits über PR-Refs erreichbar; History bleibt erhalten.
  - Nachteile: PR-Diskussionen und Issues bleiben im Archiv; Repo-Einstellungen sind neu anzulegen.
- **2B Neues Repository mit einem einzigen Initial-Commit** – am saubersten, aber ohne History.
- **2C Bestehendes Repository bereinigen und öffentlich schalten** – alte Commits bleiben über PR-Refs sichtbar.

## Entscheidung

**1A + 2A** (Name und Veröffentlichungsweg von Projektinhaber am 2026-11-20 im Chat gewählt).

**Name und Bezeichner**

| Was | Neu |
|-----|-----|
| Anzeigename | Pulsequorum |
| Repository | `github.com/Joinsider/pulsequorum` |
| Go-Modul | `github.com/joinsider/pulsequorum` |
| Binary, `cmd/` | `pulsequorum`, `cmd/pulsequorum` |
| Protobuf-Paket | `pulsequorum.v1` (`proto/pulsequorum/v1`) |
| Prometheus-Präfix | `pulsequorum_` |
| Images | `ghcr.io/joinsider/pulsequorum`, `ghcr.io/joinsider/pulsequorum-web` (ADR 0036, 6B) |
| Angular-Projekt, OIDC-Client-IDs der Dev-Umgebung | `pulsequorum-web`, `pulsequorum-spa`, `pulsequorum-cli`, `pulsequorum-api` |

- Der Umbau ist bewusst **nicht rückwärtskompatibel** (gRPC-Servicenamen, Metriknamen, Pfade). Es gibt noch keinen
  Release und keine Installation außerhalb der Entwicklung; lokale Dev-Datenverzeichnisse werden neu angelegt.
- Umgebungsvariablen haben kein Projektpräfix und bleiben unverändert.
- Alte Commits behalten den Arbeitsnamen; umbenannt wird nur der aktuelle Stand.

**Bereinigung und Veröffentlichung (einmalig)**

1. Frischer Klon von `master` in ein eigenes Verzeichnis. Nur `master` wird übernommen, keine anderen Branches, keine
   PR-Refs.
2. `git filter-repo` (MIT) mit
   - `.mailmap`: private E-Mail und Schreibweise „Joinside“ → `Joinsider <…@users.noreply.github.com>`,
   - `--replace-text` und `--replace-message`: Vorname des Projektinhabers → „Projektinhaber“.
3. Erneuter Scan des Ergebnisses (gitleaks über alle Commits, Suche nach E-Mail und Vorname) muss leer sein.
4. Alle Commits werden lokal mit der bestehenden SSH-Signatur des Projektinhabers neu signiert (Skript mit
   `git commit-tree -S` in topologischer Reihenfolge, Autor, Committer und Zeitstempel bleiben erhalten). Danach
   `git verify-commit` für jeden Commit.
5. Neues Repository zunächst **privat** anlegen, bereinigten `master` pushen, Einstellungen nach ADR 0040 setzen
   (Rulesets, Immutable Releases, Environment, Actions-Policy). Der Stack der P5-Arbeit wird auf diesen `master`
   gesetzt.
6. Projektinhaber prüft das Repository und schaltet es selbst öffentlich. Danach werden CodeQL, Secret Scanning mit
   Push-Schutz, Dependency Review, Scorecard und Build-Attestations aktiviert (ADR 0040).
7. Das alte Repository `Joinsider/uptime-cluster` bleibt privat und wird archiviert.

## Konsequenzen

- Positiv: Name ohne bekannte Kollision; Modulpfad, Repository und Image-Namen passen zusammen; keine
  personenbezogenen Daten in der öffentlichen History.
- Negativ / Risiken:
  - Umbau berührt rund 870 Dateien (überwiegend generierter Code und Importpfade); mechanisch, aber groß.
  - Commit-Hashes ändern sich; lokale Branches müssen auf den neuen `master` umgesetzt werden.
  - Domains und Markenregister (EUIPO/USPTO) sind noch nicht geprüft.
- Folgearbeiten:
  - AP-5.0a: Rename (eigener Branch im Stack, an Haiku delegierbar, Akzeptanz `make build test lint generate-check`).
  - AP-5.0b: Bereinigung und neues Repository (Hauptagent; Push erst nach Freigabe durch Projektinhaber).
  - Pflichtenheft O-08 schließen, CLAUDE.md und README auf den neuen Namen umstellen.

## Approval

| Datum      | Status   | Durch  | Anmerkung |
|------------|----------|--------|-----------|
| 2026-11-20 | Proposed | Claude |           |
| 2026-11-20 | Accepted | Projektinhaber | im Chat |
