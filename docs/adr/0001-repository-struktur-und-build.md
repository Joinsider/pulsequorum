# ADR 0001: Repository-Struktur, Build und Einbettung der SPA

| | |
|---|---|
| **Status** | Accepted |
| **Datum** | 2026-10-19 |
| **Entscheider** | Projektinhaber |
| **Bezug** | FA-UI-01, FA-UI-02, NFA-PERF-07, PF-38, PF-39, Konzept 18.2 |

## Kontext

Backend (Go) und Frontend (Angular) werden gemeinsam versioniert und als ein Binary ausgeliefert. Die SPA wird per `go:embed` eingebettet und soll zusätzlich extern gehostet werden können. Bekannte Fallstricke: `go:embed` schlägt bei leerem Verzeichnis fehl, die Build-Reihenfolge Frontend → Backend muss erzwungen werden, und falsche Cache-Header führen nach Updates zu veralteten Bundles. Außerdem darf `go build ./...` nicht über `web/node_modules` stolpern (npm-Pakete können `.go`-Dateien enthalten).

## Optionen

### Option A: Monorepo, ein Go-Modul im Root, Angular-Workspace unter `web/`, Einbettung aus `internal/webui/dist`
- Vorteile: Ein Commit ändert Vertrag, Backend und Frontend gemeinsam; einfache CI; Konzept-Struktur bleibt erhalten.
- Nachteile: Build-Artefakt muss vom Angular-Output in das Embed-Verzeichnis kopiert werden.

### Option B: Getrennte Repositories für Backend und Frontend
- Vorteile: Unabhängige Release-Zyklen.
- Nachteile: Vertragsänderungen über zwei Repos, aufwendigere Einbettung, mehr Overhead für ein Ein-Personen-Projekt.

### Option C: Angular direkt in ein Go-Paket bauen (`outputPath` zeigt in `internal/…`)
- Vorteile: Kein Kopierschritt.
- Nachteile: Angular löscht das Ausgabeverzeichnis inkl. Platzhalter; generierte Dateien vermischen sich mit Go-Code.

## Entscheidung

**Option A.**

- Struktur gemäß Konzept 18.2 (`cmd/`, `internal/`, `proto/`, `api/`, `web/`, `deploy/`, `docs/`, zusätzlich `spikes/`).
- Go-Modulpfad vorläufig `github.com/joinsider/uptimecluster` (Änderung bei finalem Namen, O-08).
- `internal/webui` bettet `all:dist` ein. `dist/.gitkeep` ist eingecheckt, damit der Go-Build ohne Frontend-Build funktioniert; fehlt `index.html`, liefert der Server eine Hinweisseite „UI nicht gebaut“.
- `make build` baut zuerst die SPA (`web/dist/…/browser`), synchronisiert sie nach `internal/webui/dist` und baut dann das Binary. CI nutzt dieselben Make-Ziele.
- Auslieferung: Dateien, die existieren, werden ausgeliefert; alle anderen Pfade außer `/api/*`, `/healthz`, `/readyz`, `/metrics` fallen auf `index.html` zurück (SPA-Routing). Unter `/api/*` gibt es echte 404.
- Cache-Header: gehashte Assets `public, max-age=31536000, immutable`; `index.html` und `config.json` `no-cache`.
- `config.json` wird vom Server dynamisch aus der Node-Konfiguration erzeugt (FA-UI-02); für den Dev-Server und externes Hosting liegt eine statische Variante in `web/public/`.
- Go-Befehle im Makefile nutzen `./cmd/... ./internal/...` statt `./...`.

## Konsequenzen

- Positiv: Ein Artefakt, eine Version, reproduzierbarer Build; Go-Entwicklung ohne Node-Toolchain möglich.
- Negativ: Kopierschritt im Build; Entwickler müssen `make build` statt `go build` nutzen, um die UI zu sehen.
- Folgearbeiten: Vorkomprimierung (gzip/brotli) der Assets beim Build (P1, optional).

## Approval

| Datum | Status | Durch | Anmerkung |
|-------|--------|-------|-----------|
| 2026-10-19 | Proposed | Claude | |
| 2026-10-19 | Accepted | Projektinhaber | im Chat für alle ADRs 0001–0011 bestätigt |
