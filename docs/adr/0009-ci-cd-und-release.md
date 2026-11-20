# ADR 0009: CI/CD, Qualitäts-Gates und Release

|                 |                                                                                                       |
|-----------------|-------------------------------------------------------------------------------------------------------|
| **Status**      | Accepted                                                                                              |
| **Datum**       | 2026-10-19                                                                                            |
| **Entscheider** | Projektinhaber                                                                                              |
| **Bezug**       | NFA-MNT-01/02, NFA-SEC-06, NFA-PORT-01, RB-LIC-03/05, Konzept 18.1 (GitHub Actions, GHCR, distroless) |

> Teilweise ersetzt durch [ADR 0040](0040-ci-cd-haertung-und-manuelle-releases.md) (Trigger, Release-Ablauf, Image-Tag `latest`, cosign).

## Kontext

Das Konzept sieht GitHub Actions, Multi-Arch-Images (amd64/arm64) in GHCR und `distroless/static` vor. Dazu kommen
Qualitäts-Gates (Lint, Tests, Schwachstellen, Lizenzen) und ein reproduzierbarer Release-Prozess. Das Binary ist
CGO-frei und lässt sich ohne QEMU für beide Architekturen cross-kompilieren.

## Optionen (Release-Werkzeug)

- **R1: GoReleaser (OSS, MIT)** – baut Binaries, Multi-Arch-Images, Checksummen, SBOM, Changelog aus einem Tag.
- **R2: Eigene Workflows mit `docker buildx`** – volle Kontrolle, mehr YAML; Multi-Arch-Build des Frontends unnötig
  doppelt.
- **R3: `ko`** – sehr schlanke Go-Images, aber Frontend-Build und Zusatzdateien umständlich.

## Entscheidung

**Pipeline (GitHub Actions)**

- Trigger: Push auf `main`, Pull Requests, Tags `v*`.
- Job `go`: `gofmt`-Check, `go vet`, `go test -race -cover`, `govulncheck`, `golangci-lint` v2 (über
  `golangci/golangci-lint-action@v9`, Version gepinnt; golangci-lint ist GPL-3.0, aber nur Werkzeug, nicht Teil des
  Produkts).
- Job `web`: `npm ci`, Lint, Vitest, Produktions-Build, `npm audit --audit-level=high`.
- Job `licenses`: `github.com/google/go-licenses/v2 check` gegen die Allow-List aus ADR 0010; für npm ein Lizenzprüfer
  mit derselben Allow-List.
- Job `generate`: generierter Code (sqlc, oapi-codegen, orval, protobuf) ist aktuell (`git diff --exit-code`).
- Job `build`: `make build`; das Image wird bei jedem PR gebaut (nicht gepusht).
- Actions werden per Commit-SHA gepinnt; Dependabot für Actions, Go-Module und npm.

**Release: R1 GoReleaser**

- Tag `vX.Y.Z` → Frontend-Build, Cross-Compile `linux/amd64`, `linux/arm64` (zusätzlich `darwin/arm64` als Binary),
  Images `ghcr.io/<org>/pulsequorum:{X.Y.Z, X.Y, latest}` auf Basis `gcr.io/distroless/static-debian12:nonroot`, SBOM
  (Syft), Checksummen, Changelog aus Conventional Commits.
- Image-Signatur mit cosign (keyless) – Kann, P5.

**Lokal**

- `Makefile` ist die Schnittstelle (`build`, `test`, `lint`, `generate`, `run`, `docker`); CI ruft dieselben Ziele auf.
- Versionierung: SemVer; Version, Commit und Build-Datum per `-ldflags`.

## Konsequenzen

- Positiv: Einheitliche Gates lokal und in CI, reproduzierbare Releases, Multi-Arch ohne QEMU.
- Negativ: GoReleaser-Konfiguration als weiteres Werkzeug.
- Folgearbeiten: AP-0.9 (Basis-Pipeline, ohne Release) jetzt; AP-0.10 (Lint-Ausbau, Lizenzen, Release) nach Accept.

## Approval

| Datum      | Status   | Durch    | Anmerkung                                 |
|------------|----------|----------|-------------------------------------------|
| 2026-10-19 | Proposed | Claude   |                                           |
| 2026-10-19 | Accepted | Projektinhaber | im Chat für alle ADRs 0001–0011 bestätigt |
