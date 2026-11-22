# ADR 0043: Eigenes schlankes Probe-Binary und -Image (`pulsequorum-probe`)

|                 |                                                             |
|-----------------|-------------------------------------------------------------|
| **Status**      | Proposed                                                    |
| **Datum**       | 2026-11-22                                                  |
| **Entscheider** | Projektinhaber                                              |
| **Bezug**       | Z-M-04, FA-NODE-01, FA-PRB-01..07, NFA-PERF-02, NFA-PERF-07, NFA-PORT-01, ADR 0001, ADR 0009, ADR 0034, ADR 0035, ADR 0040 |

## Kontext

Bisher existiert gemäß Z-M-04 / FA-NODE-01 ausschließlich ein gemeinsames Go-Binary (`cmd/pulsequorum`) und ein einziges Container-Image (`ghcr.io/joinsider/pulsequorum`, ca. 30 MB statisches Binary auf `gcr.io/distroless/static-debian12:nonroot`). Die Rolle (`voter` oder `probe`) wird über `NODE_ROLE` gesteuert.

Obwohl die Probe zur Laufzeit im Prozess weder Raft noch `state.db`, API oder UI initialisiert, bringt das gemeinsame Binary sämtliche Voter-only-Abhängigkeiten in Image und SBOM mit:
- die eingebettete Angular-SPA (`internal/webui/dist/`),
- Raft, `bbolt` und Migrations-/Snapshot-Logik (`internal/cluster`, `hashicorp/raft`, `go.etcd.io/bbolt`),
- die REST-API-Server-/OpenAPI-Schicht (`internal/api`, `kin-openapi`),
- Authentifizierung/OIDC (`internal/auth`, `go-oidc`) sowie Alert-Templates und Shoutrrr-Integration (`internal/alert`, `shoutrrr`).

Seit AP-5.2 (ADR 0035) betreiben Mandanten **private Probes** in eigenen Kundennetzen und auf ressourcenarmen Edge-Nodes. Dort fallen Image-Größe, Download-/Build-Zeit sowie Scanner-Meldungen (CVEs in für Probes irrelevanten Voter-Paketen) und ein unnötiges `EXPOSE 9443` im Dockerfile stärker ins Gewicht. Gleichzeitig soll `FA-NODE-01` (Single-Binary-Betrieb des Haupt-Images mit `NODE_ROLE=probe`) aus Kompatibilitätsgründen nicht gebrochen werden.

## Optionen

### Option A: Zusätzliches schlankes `pulsequorum-probe`-Binary & -Image bei erhaltener Kompatibilität (vorgeschlagen)

Zusätzlich zum unveränderten Haupt-Image (`ghcr.io/joinsider/pulsequorum`, weiterhin `voter|probe`) wird ein dediziertes Probe-Binary (`cmd/pulsequorum-probe`) samt Multi-Arch-Container-Image (`ghcr.io/joinsider/pulsequorum-probe`) gebaut, signiert und veröffentlicht:

1. **Gemeinsamer Probe-Runner (`internal/proberun`):**
   - Die Probe-Start-, Readiness-, Healthcheck- und Capability-Logik aus `cmd/pulsequorum/probe.go` wird in ein internes Paket `internal/proberun` (inkl. Subcommand `healthcheck`) extrahiert, ohne `internal/webui`, `internal/api`, `internal/auth`, `internal/cluster`, `internal/alert` oder Shoutrrr zu importieren.
   - `cmd/pulsequorum` ruft `proberun` unverändert bei `NODE_ROLE=probe` auf (volle Rückwärtskompatibilität für `FA-NODE-01` und bestehende Compose-Setups).
   - `cmd/pulsequorum-probe` bindet ausschließlich `proberun` (inkl. `healthcheck` und `version`), setzt `probe` als Standardrolle und bricht mit klarer Fehlermeldung ab, falls `NODE_ROLE=voter` gesetzt wird.
2. **Dockerfiles (`Dockerfile.probe`, `Dockerfile.probe.release`):**
   - `Dockerfile.probe` kommt ohne `node:24-alpine`-/Angular-Stage aus und baut nur `./cmd/pulsequorum-probe` mit `CGO_ENABLED=0` auf `gcr.io/distroless/static-debian12:nonroot`.
   - `Dockerfile.probe.release` verpackt das von GoReleaser gebaute `$TARGETPLATFORM/pulsequorum-probe` mit `ENV DATA_DIR=/data`, `ENV NODE_ROLE=probe`, `EXPOSE 8080` (ohne `9443`), `USER nonroot:nonroot`, `HEALTHCHECK ["/pulsequorum-probe", "healthcheck"]` und `ENTRYPOINT ["/pulsequorum-probe"]`.
3. **Build, CI/CD & Release (`.goreleaser.yaml`, `release.yml`, `ci.yml`, `Makefile`, `dependabot.yml`):**
   - `.goreleaser.yaml`: zweiter Build (`id: pulsequorum-probe`, `main: ./cmd/pulsequorum-probe`, `linux/amd64` + `linux/arm64`, kein Node/Web-Embedding-Bedarf für das Probe-Binary), eigenes Tarball-Archiv (`pulsequorum-probe_<ver>_<os>_<arch>.tar.gz`) mit Syft-SBOM, zweiter `dockers_v2`-Eintrag für `ghcr.io/joinsider/pulsequorum-probe` mit identischen Tag-Regeln (`{{ .Version }}`, `{{ .Major }}.{{ .Minor }}`, ab `1.0.0` `latest`). `docker_signs` gilt über `artifacts: all` automatisch auch für das Probe-Image.
   - `.github/workflows/release.yml`: löst für **beide** Images (`pulsequorum` und `pulsequorum-probe`) den Digest auf, erstellt jeweils Build-Provenance (`actions/attest-build-provenance`) und verifiziert Cosign-Signatur sowie Attestation vor Abschluss des Draft-Releases.
   - `Makefile`: `go-build-probe` baut `bin/pulsequorum-probe`; `ci.yml` prüft den Build des Probe-Binaries (und die Einhaltung der Image-Schlankheit); `.github/dependabot.yml` hält auch `Dockerfile.probe` und `Dockerfile.probe.release` aktuell.
4. **Architekturtest & Doku:**
   - Ein Go-Test prüft via `go list -deps ./cmd/pulsequorum-probe`, dass keine Voter-only-Pakete (`internal/webui`, `internal/api`, `internal/auth`, `internal/cluster`, `internal/alert`, `github.com/hashicorp/raft`, `go.etcd.io/bbolt`, `github.com/nicholas-fedor/shoutrrr`, `github.com/coreos/go-oidc/v3`) im Abhängigkeitsgraph landen.
   - `deploy/probe/compose.yaml`, `docs/betrieb/probe.md` und der Quick-Start-Hinweis im UI-Dialog `probe-token-dialog` werden auf `ghcr.io/joinsider/pulsequorum-probe` umgestellt.
   - Keine neue FSM-Befehlsstufe nötig (bleibt `10`).

- Vorteile:
  - Spürbar kleineres Image und kompakteres Binary ohne Frontend-Assets, Raft, bbolt, OIDC, OpenAPI-Server oder Shoutrrr.
  - Deutlich reduzierte SBOM- und CVE-Fläche in Kundennetzen (wichtig für private Probes nach AP-5.2).
  - Keine API-/Protokoll- oder Breaking Changes: `ghcr.io/joinsider/pulsequorum` mit `NODE_ROLE=probe` bleibt vollständig lauffähig.
- Nachteile:
  - Zweites Multi-Arch-Image in GHCR/GoReleaser/Release-Workflow zu bauen, signieren und dokumentieren.
  - Einmalige Extraktion der Probe-Startlogik aus `cmd/pulsequorum/probe.go` nach `internal/proberun`.

### Option B: Weiterhin nur ein gemeinsames Image (`ghcr.io/joinsider/pulsequorum`)

- Vorteile:
  - Geringster Release- und Wartungsaufwand (nur ein Binary, ein Dockerfile, ein GHCR-Paket).
- Nachteile:
  - Jede Probe schleppt die Angular-SPA, Raft, bbolt, OIDC und Shoutrrr mit – sowohl im Download-Traffic als auch in jeder Container-/CVE-Prüfung beim Kunden.

### Option C: Harte Trennung (`NODE_ROLE=probe` aus `cmd/pulsequorum` entfernen)

- Vorteile:
  - Klare Rollen-Trennung im Code.
- Nachteile:
  - Bricht `FA-NODE-01` und bestehende `NODE_ROLE=probe`-Konfigurationen auf dem Haupt-Image ohne Not.

## Entscheidung

**Option A** wird vorgeschlagen: `ghcr.io/joinsider/pulsequorum-probe` als schlankes, zweites Multi-Arch-Image und Binary (`cmd/pulsequorum-probe` über gemeinsames `internal/proberun`) einzuführen, während `cmd/pulsequorum` (`NODE_ROLE=voter|probe`) vollständig kompatibel bleibt.

## Konsequenzen

- **Positiv:** Kleineres Probe-Image, saubere SBOM/kleinere Angriffsfläche für Edge- und private mandanteneigene Probes, kein `EXPOSE 9443`, automatisch erzwungener Architekturtest gegen versehentliche Voter-Abhängigkeiten.
- **Negativ / Risiken:** Release-Pipeline (`.goreleaser.yaml`, `release.yml`) verwaltet künftig zwei Container-Images (mit AP-5.3 `pulsequorum-web` drei); beide müssen im Release-Check gemeinsam auf Digest, Cosign-Signatur und Provenance geprüft werden.
- **Folgearbeiten:** Nach Approval (`Accepted`) Umsetzung als **AP-5.2b** direkt nach AP-5.2: `internal/proberun`, `cmd/pulsequorum-probe`, Architekturtest, `Dockerfile.probe` + `Dockerfile.probe.release`, `.goreleaser.yaml`, `.github/workflows/{ci,release}.yml`, `.github/dependabot.yml`, `Makefile`, `deploy/probe/compose.yaml`, `docs/betrieb/probe.md`, UI-Snippet in `web/src/app/features/probes/` sowie Update von `docs/pflichtenheft.md` (Z-M-04 / Kap. 4) und `docs/roadmap.md`.

## Approval

| Datum      | Status   | Durch  | Anmerkung |
|------------|----------|--------|-----------|
| 2026-11-22 | Proposed | Claude | Wartet auf Review durch Projektinhaber |
