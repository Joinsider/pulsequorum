# ADR 0040: CI/CD-Härtung und manuell ausgelöste, signierte Releases

|                 |                                                                                                                                                                                                             |
|-----------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **Status** | Accepted |
| **Datum**       | 2026-11-19                                                                                                                                                                                                  |
| **Entscheider** | Projektinhaber                                                                                                                                                                                                    |
| **Bezug**       | NFA-SEC-06, NFA-PORT-01, NFA-MNT-02/03, RB-LIC-03/05, AP-0.9, AP-0.10, AP-5.6; ergänzt und ersetzt teilweise ADR 0009 (Trigger, Release-Auslösung, Image-Tags, Signatur); ADR 0010 (Lizenzen der Werkzeuge) |

## Kontext

ADR 0009 legt GitHub Actions, GoReleaser, GHCR und Multi-Arch-Images fest. Die Pipeline liegt im Repository, ein Release
ist aber noch nie gelaufen und es gibt keinen Tag. Das Repository `Joinsider/pulsequorum` ist **privat** und gehört
einem **persönlichen Konto**, nicht einer Organisation. Der Default-Branch heißt `master`.

Der Projektinhaber wünscht:

1. eine vollständige CI/CD-Pipeline mit GitHub Actions nach aktuellen Security-Best-Practices,
2. das Image vorerst auf GHCR,
3. als erste Version `v0.0.1` und SemVer `0.x` bis zur Release-Reife,
4. Releases, die manuell per `workflow_dispatch` ausgelöst werden (explizite Version oder Bump `patch`/`minor`/`major`).
   Der Workflow legt einen **signierten Tag** und einen **signierten Release** an, der Changelog umfasst alles seit dem
   letzten Release,
5. einen Changelog, der sich nachträglich überschreiben lässt.

### Ist-Zustand (Befunde vom 2026-11-19)

| #   | Befund                                                                                                                                                                                                                 | Wirkung                                                                                                                 |
|-----|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------|
| B1  | `ci.yml` triggert auf `push: branches: [main]`, der Branch heißt aber `master`.                                                                                                                                        | CI läuft nur für Pull Requests, nie nach einem Merge auf `master`. Ein Release-Commit hätte keinen eigenen grünen Lauf. |
| B2  | `govulncheck` läuft mit `@latest`.                                                                                                                                                                                     | Nicht reproduzierbar, ungepinnte Ausführung fremden Codes in CI.                                                        |
| B3  | `release.yml` vergibt `contents: write` und `packages: write` auf Workflow-Ebene.                                                                                                                                      | Jeder künftige Job erbt Schreibrechte (Least Privilege verletzt).                                                       |
| B4  | `.goreleaser.yaml` hat `release.github: joinsider/pulsequorum`, das Label `org.opencontainers.image.source` zeigt auf `https://github.com/Joinsider/pulsequorum`. Das Repository heißt `Joinsider/pulsequorum`. | Der Release-Upload würde mit 404 scheitern. Das GHCR-Paket würde nicht mit dem Repository verknüpft.                    |
| B5  | `actions/checkout` ohne `persist-credentials: false`.                                                                                                                                                                  | Das Token bleibt in `.git/config` und ist für jeden späteren Schritt lesbar.                                            |
| B6  | Repo-Einstellung `sha_pinning_required: false`, `allowed_actions: all`.                                                                                                                                                | Ungepinnte oder beliebige Actions wären erlaubt. Die Pins existieren heute nur per Konvention.                          |
| B7  | Ruleset „Protect main“ (Ziel `~DEFAULT_BRANCH`) hat die Bypass-Akteure `DeployKey: always` und `RepositoryRole 5 (Admin): always`.                                                                                     | Ein künftiger Deploy-Key mit Schreibrecht könnte direkt auf `master` pushen und PR, Checks und Signaturpflicht umgehen. |
| B8  | Kein Tag-Ruleset, keine Environments, keine Secrets, Immutable Releases deaktiviert (`GET /immutable-releases` → `enabled: false`), Code Scanning und Secret Scanning deaktiviert.                                     | –                                                                                                                       |
| B9  | Basis-Images (`node:24-alpine`, `golang:1.27-alpine`, `distroless/static-debian12:nonroot`) sind nicht per Digest gepinnt. Dependabot pflegt kein `docker`-Ökosystem und hat keinen Cooldown.                          | Unbemerkte Änderungen an den Basis-Images. Brandneue (ggf. kompromittierte) Versionen werden sofort vorgeschlagen.      |
| B10 | Der Go-Modulpfad `github.com/joinsider/pulsequorum` passt nicht zum Repository-Namen.                                                                                                                                | `go install …@vX` funktioniert nicht. Für Images und Binaries egal, siehe offene Fragen.                                |

### Einschränkungen durch privates Repository auf einem persönlichen Konto (Stand Ende 2026)

| Funktion                                                                                  | Verfügbar?                                                                                                                                                                 | Quelle/Anmerkung                                     |
|-------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------------------------------------------------------|
| GitHub Artifact Attestations (`actions/attest-build-provenance`, `gh attestation verify`) | **Nein.** Bei privaten Repos nur mit GitHub Enterprise Cloud.                                                                                                              | GitHub Docs „Artifact attestations“                  |
| Immutable Releases inkl. Release-Attestation (`gh release verify`)                        | **Ja.** Der API-Endpunkt antwortet, die Einstellung ist pro Repo schaltbar. Ob die Release-Attestation bei privaten persönlichen Repos erzeugt wird, prüft der erste Lauf. | GitHub Docs „Immutable releases“, GA seit 2025-10-28 |
| Environments                                                                              | Für private Repos nur mit **GitHub Pro** (persönliches Konto).                                                                                                             | GitHub Docs „Managing environments“                  |
| Environment-Schutzregeln „Required reviewers“ und „Wait timer“                            | **Nein.** Bei Free/Pro/Team nur für öffentliche Repos. Deployment-Branch-/Tag-Policies gehen mit Pro.                                                                      | dto.                                                 |
| CodeQL, Dependency Review, Secret Scanning mit Push Protection                            | Nur mit GitHub Code Security bzw. Secret Protection (GHAS). Für persönliche Konten nicht buchbar.                                                                          | GitHub Docs                                          |
| OpenSSF Scorecard (Action)                                                                | Ohne GHAS kein SARIF-Upload, kein Publish. Lokal als CLI möglich.                                                                                                          | scorecard-action README                              |
| StepSecurity Harden-Runner                                                                | Community Tier **nicht** für private Repos (Enterprise Tier kostenpflichtig).                                                                                              | docs.stepsecurity.io                                 |
| Sigstore (cosign, gitsign) keyless                                                        | Funktioniert. Signaturen landen aber im **öffentlichen** Rekor-Log, das Fulcio-Zertifikat enthält Repository-Name, Workflow-Pfad, Ref und Commit-SHA.                      | Sigstore-Doku                                        |
| Rulesets für Branches und Tags                                                            | Ja (auch privat, persönliches Konto).                                                                                                                                      | bestehendes Ruleset belegt es                        |

Bereits im Einsatz und weiter genutzt: SonarCloud (SAST/Qualität), Socket Security (Supply-Chain-Prüfung von
PR-Abhängigkeiten, als Pflicht-Check), `golangci-lint` mit `gosec`, `npm audit` mit Ausnahmeliste, `go-licenses`.

## Optionen

### Frage 1: Signatur des Tags

#### Option T1: gitsign (Sigstore keyless) im Workflow

- Vorteile: Kein langlebiges Geheimnis. Die Identität ist der Workflow selbst (`…/release.yml@refs/heads/master`).
  Apache-2.0.
- Nachteile: GitHub zeigt gitsign-Signaturen **nicht** als „Verified“ (Sigstore-Root nicht im Trust-Store von GitHub,
  laut gitsign-README weiterhin so). Prüfen geht nur mit `gitsign verify-tag` und erwarteter Identität. Bei privatem
  Repo landen Repository-Metadaten dauerhaft im öffentlichen Rekor-Log. Zum Pushen des Tags braucht man trotzdem eine
  Ruleset-Ausnahme.

#### Option T2: SSH-Signaturschlüssel eines dedizierten Bot-Kontos, Push per Deploy-Key

- Ablauf: Ein Maschinenkonto (z.B. `pulsequorum-release`) hinterlegt einen Ed25519- **Signing Key**. Der private
  Schlüssel liegt als Environment-Secret. Der Workflow erzeugt `git tag -s` mit `gpg.format=ssh` und dem Tagger auf der
  Noreply-Adresse des Bots. Gepusht wird über einen eigenen **Deploy-Key mit Schreibrecht**, der im Tag-Ruleset als
  einziger Bypass-Akteur steht.
- Vorteile: Badge „Verified“ in GitHub und mit `git verify-tag` (allowed signers) offline prüfbar. Kein öffentliches
  Log. Der Bot hat keine Repo-Rechte, ein kompromittierter Signierschlüssel kann also weder pushen noch als Projektinhaber
  signieren. Der Deploy-Key gilt nur für dieses Repository.
- Nachteile: Zwei langlebige Geheimnisse (Signier- und Deploy-Key) mit Rotation. Ein zusätzliches Konto (laut GitHub-ToS
  ein kostenloses Maschinenkonto pro Person erlaubt). Ohne Required Reviewers schützt das Secret nur die
  Deployment-Branch-Policy des Environments.

#### Option T3: SSH/GPG-Schlüssel auf Projektinhaber' eigenem Konto als Secret

- Vorteile: „Verified“ als `Joinsider`, kein Zusatzkonto.
- Nachteile: Ein abgeflossener Schlüssel kann **Commits als Projektinhaber** signieren und erfüllt damit die Signaturpflicht
  auf `master`. Der Schadensradius ist zu groß.

#### Option T4: Tag-Objekt über die REST-/GraphQL-API (GITHUB_TOKEN oder GitHub-App-Token)

- Vorteile: Keine eigenen Schlüssel, die Tag-Erzeugung ist trivial.
- Nachteile: Nach Recherche signiert GitHub **per API erzeugte Tag-Objekte nicht**. Nur Commits über Web/API werden mit
  dem `web-flow`-Schlüssel signiert. Die API nimmt auch keine Signatur entgegen. Der Tag bliebe „unsigned“ und erfüllt
  die Anforderung nicht. Ein Spike prüft das im ersten Probelauf (`verification.reason`).

#### Option T5: Projektinhaber signiert den Tag lokal, der Workflow reagiert auf den Tag

- Vorteile: Stärkste Bindung an eine Person, Hardware-Schlüssel möglich.
- Nachteile: Widerspricht dem Wunsch nach vollständig automatischem Ablauf per `workflow_dispatch`.

### Frage 2: Signatur und Herkunftsnachweis des Releases und Images

#### Option S1: GitHub Artifact Attestations (`actions/attest-build-provenance`, SLSA Build L2/L3)

- Vorteile: Native Prüfung mit `gh attestation verify`. SLSA Build L3 mit wiederverwendbarem Workflow.
- Nachteile: Bei privatem Repo nur mit Enterprise Cloud, also **nicht verfügbar**.

#### Option S2: Immutable Releases + cosign keyless (Checksummen-Bundle, Image-Signatur) + BuildKit-SBOM/-Provenance im Image-Index

- Vorteile: Nur Bordmittel. Die Release-Attestation von GitHub bindet Tag, Commit und alle Assets und läuft bei privaten
  Repos über GitHubs eigene Sigstore-Instanz. Tag und Assets sind nach der Veröffentlichung unveränderlich,
  Release-Notes bleiben editierbar. cosign keyless liefert eine Signatur, die man über die Workflow-Identität prüft
  (`--certificate-identity …release.yml@refs/heads/master`). Diesen Weg nutzt auch GoReleaser selbst. GoReleaser
  `dockers_v2` hängt SBOMs per Default an (`sbom: true`).
- Nachteile: cosign keyless schreibt in das öffentliche Rekor-Log (Metadaten des privaten Repos, siehe offene Frage F1).
  Das ist kein formales SLSA-Provenance-Level.

#### Option S3: cosign mit Schlüsselpaar (Secret), ohne Transparenz-Log

- Vorteile: Nichts öffentlich, unabhängig von Sigstore-Infrastruktur.
- Nachteile: Langlebiger Schlüssel, Rotation, Verteilung des öffentlichen Schlüssels. Ohne Transparenz-Log keine
  Erkennung von Missbrauch.

#### Option S4: `slsa-framework/slsa-github-generator` (SLSA Build L3)

- Vorteile: Echte L3-Provenance auch ohne Enterprise.
- Nachteile: Bei privaten Repos ist ausdrücklich `private-repository: true` nötig (öffentliches Rekor). Letztes Release
  v2.1.0 vom Februar 2025, Pflege unsicher. Die Integration mit GoReleaser ist umständlich.

### Frage 3: Changelog-Mechanik

#### Option C1: GoReleaser `changelog.use: git` mit Conventional-Commit-Gruppen, Release als
**Draft**, optionale Override-Datei

- Vorteile: Deterministisch, offline, funktioniert auch beim ersten Release ohne Vorgänger-Tag. Die Gruppen folgen den
  Conventional Commits, die das Projekt ohnehin nutzt (243 von 263 Commits konform). Projektinhaber prüft und editiert den
  Draft und veröffentlicht ihn per Klick. Notes bleiben auch nach der Veröffentlichung editierbar. Eine Datei
  `.github/release-notes/vX.Y.Z.md` überschreibt den generierten Text (`--release-notes`).
- Nachteile: Kein Autor-Login im Text (bei einem Autor irrelevant).

#### Option C2: `changelog.use: github-native` (GitHub-Generator, Kategorien über `.github/release.yml` und PR-Labels)

- Vorteile: Standard-Look, PR-Links.
- Nachteile: Gruppen und Filter von GoReleaser fallen weg. Funktioniert nur mit sauberem Labeling der PRs. Die Commits
  auf `master` folgen keinem Label-Schema.

#### Option C3: `changelog.use: github` (Compare-API)

- Vorteile: Logins der Autoren.
- Nachteile: Laut GoReleaser-Doku nur, wenn beide Tags auf GitHub existieren. Für `v0.0.1` gibt es keinen Vorgänger.

#### Option C4: release-please bzw. git-cliff mit `CHANGELOG.md` im Repository

- Vorteile: Changelog versioniert im Repository.
- Nachteile: Ein Bot müsste auf `master` committen (Signaturpflicht, PR-Pflicht). release-please ersetzt das manuelle
  Auslösen durch Release-PRs. Ein weiteres Werkzeug.

### Frage 4: Versionierungsschema und Auslösung

#### Option V1: SemVer 0.x, Start `v0.0.1`, manueller `workflow_dispatch` mit `bump` oder expliziter `version`

- Vorteile: Entspricht dem Wunsch. Die Version wird bewusst gesetzt, die Bump-Logik ist prüfbar. Für `0.x` gilt die
  SemVer-Regel „alles kann brechen“, deshalb bumpt eine Breaking Change vor 1.0 `minor`.
- Nachteile: Die Bump-Wahl ist manuell. Der Vorschlag aus den Commits ist nur eine Anzeige.

#### Option V2: Automatische Version aus Commits (semantic-release, svu, release-please)

- Vorteile: Kein Nachdenken über die Nummer.
- Nachteile: Widerspricht der manuellen Auslösung. Bei Breaking Changes in 0.x springt das Werkzeug ggf. auf 1.0.

#### Option V3: CalVer (`2026.11.0`)

- Vorteile: Einfach.
- Nachteile: Widerspricht ADR 0009 (SemVer), ADR 0018 (Rolling Updates, Versionsvergleich) und dem Wunsch.

## Entscheidung

**T2 + S2 + C1 + V1**, dazu die Workflow-Härtung unten. Im Einzelnen:

### 1. Versionierung (V1) und Verhältnis zu AP-5.6

- Erste Version **`v0.0.1`** (technischer Durchstich der Release-Pipeline, nicht produktionsreif). Danach SemVer `0.x`.
  Breaking Changes erhöhen in `0.x` die **Minor**-Stelle, alles andere die Patch-Stelle.
- `workflow_dispatch`-Eingaben: `bump` (`patch` | `minor` | `major`, Default `patch`), optional `version` (explizit,
  z.B. `0.3.0` oder `0.3.0-rc.1`, hat Vorrang), `dry_run` (Boolean, Default `true`).
- `major` aus `0.x` heraus (also `1.0.0`) ist nur mit expliziter `version: 1.0.0` erlaubt. `bump: major` bricht in `0.x`
  mit Hinweis ab. So entsteht 1.0.0 nie aus Versehen.
- **Konflikt mit AP-5.6 („Release 1.0“)**, aufgelöst: `1.0.0` bleibt das Release-Reife-Ziel am Ende von P5 nach Abnahme
  AS-08 und Namensprüfung O-08. Die `0.x`-Releases davor sind Vorabversionen für Tests und Dogfooding. AP-5.6 bleibt
  inhaltlich unverändert, die Roadmap erhält (nach Accept) einen Vermerk.
- Image-Tags (ändert ADR 0009): `X.Y.Z` immer, `X.Y` nur ohne Pre-Release-Suffix, **`latest` erst ab `1.0.0`** und nur
  ohne Suffix. Ein `latest`, das auf eine `0.x` zeigt, würde Nutzer unbemerkt auf eine Version mit Breaking Changes
  bringen. GitHub-Releases mit Suffix (`-rc.N`) werden als Pre-Release markiert (`prerelease: auto`).

### 2. Signierter Tag (T2)

- Dediziertes Maschinenkonto mit Ed25519- **Signing Key**. Tagger: `<bot> <ID+bot@users.noreply.github.com>`.
- Gepusht wird über einen **Deploy-Key mit Schreibrecht**, der nur im neuen Tag-Ruleset als Bypass steht.
- **Voraussetzung:** Im Ruleset „Protect main“ wird der Bypass `DeployKey` **entfernt** (B7), bevor der Deploy-Key
  angelegt wird.
- Beide privaten Schlüssel liegen als Secrets des Environments `release`. Das Environment erlaubt nur den Branch
  `master`. Der Workflow schreibt sie nach `$RUNNER_TEMP` (Modus 0600), pinnt die SSH-Host-Keys von GitHub und löscht
  beide Dateien in einem `always()`-Schritt.
- Der Tag ist **annotiert und signiert** (`git tag -s`) und zeigt auf den geprüften Commit-SHA. Er wird vor dem Push
  lokal mit `git verify-tag` gegen eine im Repository versionierte Datei `.github/allowed_signers` geprüft. Nach dem
  Push prüft der Workflow per API `verification.verified == true`.
- Wenn ein Tag schon existiert, auf denselben SHA zeigt und gültig signiert ist, überspringt ein erneuter Lauf das
  Tagging. Sonst bricht der Lauf ab.

### 3. Signierter Release und Image (S2)

- **Immutable Releases** im Repository aktivieren. GoReleaser erzeugt den Release als **Draft** (`release.draft: true`,
  `replace_existing_draft: true`) und lädt alle Assets hoch. Projektinhaber **veröffentlicht den Draft manuell**. Erst dann
  wird der Release unveränderlich und GitHub erzeugt die Release-Attestation (`gh release verify vX.Y.Z`,
  `gh release verify-asset …`). Der manuelle Publish-Klick ersetzt die bei privaten Repos fehlenden Required Reviewers
  als zweites menschliches Gate.
- **cosign keyless** (cosign v3, Bundle-Format): `checksums.txt` wird mit
  `cosign sign-blob --bundle checksums.txt.sigstore.json` signiert, jedes Image-Manifest per Digest mit `cosign sign`.
  Bedingung: Projektinhaber akzeptiert das öffentliche Rekor-Log (offene Frage F1). Ohne diese Zustimmung entfällt cosign bis
  zur Veröffentlichung des Repos, und Immutable Release plus signierter Tag tragen die Signatur allein.
- SBOMs: Syft je Archiv (wie heute) plus BuildKit-SBOM im Image-Index (`dockers_v2.sbom: true`, Default).
  BuildKit-Provenance mit `--provenance=mode=max` wird angehängt. Sie ist unsigniert, die cosign-Signatur des Index
  deckt sie mit ab.
- **SLSA-Provenance über `actions/attest-build-provenance` folgt, sobald das Repository öffentlich ist** (dann
  kostenlos). Bis dahin weder S1 noch S4.

### 4. Changelog (C1)

- `changelog.use: git`, `sort: asc`. Gruppen: Breaking Changes (`!:` oder `BREAKING CHANGE`), Features, Bug fixes,
  Security (`fix(security)`, `sec`), Performance, Refactoring, Others. Ausgeschlossen: `docs`, `test`, `chore`, `ci`,
  `style`, Merge-Commits. Die Vorgängerversion ist der letzte `v*`-Tag, beim ersten Release die gesamte Historie.
- **Überschreiben**, in absteigender Priorität:
    1. Existiert `.github/release-notes/vX.Y.Z.md` im getaggten Commit, nutzt GoReleaser sie statt des generierten
       Textes (`--release-notes`).
    2. Der Draft ist vor dem Veröffentlichen frei editierbar.
    3. Auch nach dem Veröffentlichen bleiben Titel und Notes editierbar, Immutable Releases sperren nur Tag und Assets.
- Kein `CHANGELOG.md` im Repository. Es gibt damit keine Bot-Commits auf `master` und keinen Konflikt mit Signatur- und
  PR-Pflicht.
- PRs werden bevorzugt per Squash mit Conventional-Commit-Titel gemergt, damit jede Änderung einen Changelog-Eintrag
  ergibt (Empfehlung, kein Zwang).

### 5. Release-Workflow (`release.yml`, ersetzt den Tag-Trigger aus ADR 0009)

Trigger nur `workflow_dispatch`, kein `push: tags` mehr. Ein Tag-Push durch den Deploy-Key würde sonst einen zweiten
Lauf starten. `permissions: {}` auf Workflow-Ebene, Rechte nur je Job. `concurrency: release`,
`cancel-in-progress: false`.

| Job          | Rechte                                                  | Aufgabe                                                                                                                                                                                                                                                                                                                                                                                                                                 |
|--------------|---------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `prepare`    | `contents: read`, `checks: read`                        | Läuft nur auf `refs/heads/master`. Prüft die Eingaben per Regex (über `env`, nie `${{ }}` in `run`). Ermittelt den letzten Tag und die nächste Version. Verlangt, dass der Commit-SHA dem aktuellen `origin/master` entspricht und dass alle Pflicht-Checks (`go`, `golangci`, `web`, `licenses`, `generate`, `build`) auf diesem SHA grün sind. Gibt `version`, `tag` und `sha` aus und zeigt eine Changelog-Vorschau im Step Summary. |
| `gates`      | `contents: read`                                        | `govulncheck` (gepinnt) und `npm audit` mit Ausnahmeliste. Befunde blockieren den Release (NFA-SEC-06).                                                                                                                                                                                                                                                                                                                                 |
| `tag`        | `contents: read`, Environment `release`                 | Erzeugt den signierten Tag nach Abschnitt 2 und pusht ihn per Deploy-Key. Wird bei `dry_run` übersprungen.                                                                                                                                                                                                                                                                                                                              |
| `goreleaser` | `contents: write`, `packages: write`, `id-token: write` | Checkout des Tags. `goreleaser release --clean` als Draft, Images nach GHCR, cosign-Signaturen. Bei `dry_run`: `goreleaser release --snapshot --clean --skip=publish,sign` und Upload der Artefakte als Workflow-Artefakt.                                                                                                                                                                                                              |
| `verify`     | `contents: read`, `packages: read`                      | `git verify-tag`, `sha256sum -c`, `cosign verify-blob` und `cosign verify` mit fester Identität `https://github.com/Joinsider/pulsequorum/.github/workflows/release.yml@refs/heads/master` und Issuer `https://token.actions.githubusercontent.com`. Schreibt einen Link auf den Draft ins Step Summary.                                                                                                                             |

### 6. CI-Härtung (`ci.yml` und neue Workflows)

- Trigger `push: branches: [master]` (B1), `pull_request`, `merge_group` (für eine spätere Merge Queue). `concurrency`
  je Ref mit `cancel-in-progress` für PRs.
- `permissions: {}` auf Workflow-Ebene, je Job das Minimum (meist `contents: read`).
- Überall `actions/checkout` mit `persist-credentials: false` (B5).
- Alle Actions per vollständigem Commit-SHA mit Versionskommentar gepinnt. Die Repo-Einstellung „Require actions to be
  pinned to a full-length commit SHA“ wird aktiviert und die erlaubten Actions auf eine Liste beschränkt (B6).
- `govulncheck` über `tools/go.mod` (`go tool -modfile=tools/go.mod govulncheck`) statt `@latest` (B2).
- Neuer Job **`workflow-lint`**: `actionlint` (MIT) und `zizmor` (MIT, Offline-Audits plus `--persona=regular`). Er
  prüft unter anderem Template-Injection, `pull_request_target`, `persist-credentials`, übermäßige Rechte und ungepinnte
  Actions. Er wird Pflicht-Check.
- Neuer Job **`secrets`**: `gitleaks` (MIT) über den PR-Diff bzw. Push, als Ersatz für das bei privaten Repos fehlende
  Secret Scanning. Ausgeführt als gepinnte Binary mit Checksumme, nicht als `gitleaks-action` (diese verlangt
  Lizenzschlüssel für Organisationen).
- Verboten per zizmor-Regel und Review: `pull_request_target`, `workflow_run` mit Checkout fremder Refs,
  `${{ github.event.* }}` in `run:`-Skripten, Secrets in Jobs ohne Environment.
- Dependabot: zusätzlich `docker` (Dockerfile, Dockerfile.release), `cooldown: default-days: 7` für alle Ökosysteme,
  Gruppierung für Actions. Basis-Images werden per Digest gepinnt (`image:tag@sha256:…`, B9).
- `docker/setup-qemu-action` entfällt im Release, falls `Dockerfile.release` weiter ohne `RUN` auskommt (nur `COPY`,
  Cross-Kompilat). Das prüft der Dry-Run.
- `.goreleaser.yaml`: `release.github` auf `Joinsider/pulsequorum` und das Source-Label auf
  `https://github.com/Joinsider/pulsequorum` korrigieren (B4). Der Image-Name bleibt
  `ghcr.io/joinsider/pulsequorum` (GHCR verlangt Kleinschreibung, der Paketname darf vom Repo-Namen abweichen).

### 7. Bei Veröffentlichung des Repositorys (vorgemerkt, nicht Teil dieses ADR-Umfangs)

Dann werden aktiviert: CodeQL (Go, JavaScript/TypeScript, Actions), Dependency Review als Pflicht-Check, Secret Scanning
mit Push Protection, OpenSSF Scorecard mit `publish_results`, Harden-Runner (Community Tier, zunächst
`egress-policy: audit`, danach `block`), `actions/attest-build-provenance` für Archive und Images, Required Reviewers
für das Environment `release`. Ein eigener ADR ist dafür nicht nötig, solange er im Rahmen dieses ADRs bleibt.

### 8. Lizenzen der Werkzeuge (ADR 0010)

Alle Werkzeuge laufen nur in CI und sind nicht Teil des Produkts: GoReleaser OSS (MIT), cosign/gitsign/syft
(Apache-2.0), actionlint (MIT), zizmor (MIT), gitleaks (MIT), govulncheck (BSD-3-Clause), golangci-lint (GPL-3.0, wie in
ADR 0009), Harden-Runner (Apache-2.0, Dienst proprietär), Scorecard (Apache-2.0). CodeQL-CLI und -Abfragen stehen unter
der GitHub CodeQL Terms and Conditions (frei für öffentliche OSS-Repos) und kommen erst bei öffentlichem Repo. Keine
dieser Lizenzen berührt die Allow-List für Produktabhängigkeiten.

### Verhältnis zu ADR 0009

ADR 0009 bleibt gültig für Werkzeugwahl (GoReleaser), Jobs, Multi-Arch, distroless, SBOM und Makefile als Schnittstelle.
**Ersetzt** werden: der Trigger „Push auf `main`“ (jetzt `master`), der Release-Trigger „Tags `v*`“ (jetzt
`workflow_dispatch`), die Image-Tag-Regel für `latest` und die Einstufung der cosign-Signatur als „Kann, P5“ (jetzt
Muss, vorbehaltlich F1). Nach Accept erhält ADR 0009 den Status-Zusatz „teilweise ersetzt durch 0040“. Der Status bleibt
`Accepted`, denn ein teilweises Superseding sieht die Statusliste nicht vor.

## Konsequenzen

- Positiv:
    - Releases sind reproduzierbar ausgelöst und nur von `master` mit grünem CI-Stand möglich. Zwei menschliche Schritte
      (Dispatch, Publish) trennen Build und Veröffentlichung.
    - Tags sind „Verified“ und unveränderlich, Release-Assets nach der Veröffentlichung ebenfalls. Signaturen und
      Release-Attestation sind unabhängig von GitHub-UI-Zuständen prüfbar.
    - Die Workflows sind statisch auf typische Actions-Schwachstellen geprüft (Template-Injection, Rechte, Pins,
      Credential-Persistenz).
    - Der Changelog entsteht automatisch und lässt sich auf drei Wegen überschreiben, ohne Commits auf `master`.
    - Der Wechsel auf ein öffentliches Repository ist vorbereitet (Liste in Abschnitt 7).
- Negativ / Risiken:
    - Zwei langlebige SSH-Schlüssel und ein Bot-Konto. Rotation jährlich bzw. bei Verdacht. Ohne Required Reviewers
      schützt nur die Branch-Policy des Environments die Secrets. Jeder mit Schreibrecht auf `master` kann den
      Release-Workflow starten, derzeit nur Projektinhaber.
    - cosign keyless veröffentlicht Repository-Metadaten im öffentlichen Rekor-Log (F1).
    - Images liegen schon in GHCR, bevor der Draft veröffentlicht ist. Ein verworfener Draft hinterlässt signierte, aber
      nicht freigegebene Images. Bereinigung per Hand, `latest` ist davon in `0.x` nicht betroffen.
    - Ein gelöschter Immutable Release blockiert den Tag-Namen dauerhaft. Fehler nach dem Publish heilt nur eine neue
      Patch-Version.
    - Ohne Harden-Runner, CodeQL und Secret Scanning bleibt die Abdeckung bei privatem Repo geringer (teilweise
      ausgeglichen durch SonarCloud, gosec, gitleaks, Socket).
    - Ob Immutable Releases und Release-Attestation bei privaten persönlichen Repos voll funktionieren, ist erst nach
      dem ersten echten Release sicher.
- Folgearbeiten:
    - Umsetzung nach Accept in der Reihenfolge: Repo-Einstellungen → CI-Härtung → GoReleaser-Korrekturen →
      Release-Workflow → Dry-Run → `v0.0.1`.
    - Roadmap: AP-0.9/AP-0.10 schließen, neues AP für diesen ADR, Vermerk an AP-5.6 (`0.x` vor `1.0.0`). ADR-Index
      ergänzen. ADR 0009 mit Hinweis versehen.
    - Doku `docs/betrieb/release.md`: Ablauf, Prüfung (`git verify-tag`, `cosign verify`, `gh release verify`),
      Schlüsselrotation.
    - Bei Veröffentlichung des Repositorys: Abschnitt 7 umsetzen.

### Manuelle Einstellungen im GitHub-UI (Projektinhaber)

1. **Ruleset „Protect main“**: Bypass `DeployKey` entfernen. Den Admin-Bypass von `always` auf `pull_request` umstellen
   (nur über PR umgehbar) oder bewusst belassen. Neue Pflicht-Checks `workflow-lint` und `secrets` ergänzen.
2. **Neues Tag-Ruleset „Protect release tags“**: Ziel `refs/tags/v*`, Regeln *Restrict creations*, *Restrict updates*,
   *Restrict deletions*, *Block force pushes*. Bypass nur der Release-Deploy-Key (`always`), optional Admin.
3. **Immutable Releases** aktivieren (Settings → General → Releases → „Enable release immutability“).
4. **Actions-Einstellungen**: „Require actions to be pinned to a full-length commit SHA“ an. „Allow select actions“ mit
   Liste (`actions/*`, `docker/*`, `goreleaser/*`, `sigstore/*`, `anchore/*`, `golangci/*`, `zizmorcore/*`).
   Workflow-Default-Rechte bleiben `read`, „Allow GitHub Actions to create and approve pull requests“ bleibt aus.
5. **Maschinenkonto** anlegen (z.B. `pulsequorum-release`), 2FA, verifizierte Noreply-Adresse. Einen **SSH Signing
   Key** hinterlegen. Keine Repo-Rechte vergeben.
6. **Deploy-Key** („release-tag-push“, *Allow write access*) im Repository anlegen.
7. **Environment `release`** (benötigt GitHub Pro, F2): Deployment branches „Selected“ → `master`. Secrets
   `RELEASE_TAG_SIGNING_KEY` und `RELEASE_TAG_DEPLOY_KEY`. Variable `RELEASE_TAGGER` (Name/E-Mail des Bots).
8. **Merge-Methoden**: optional nur Squash und PR-Titel als Commit-Nachricht.
9. Nach dem ersten Image-Push: das GHCR-Paket `pulsequorum` mit dem Repository verknüpfen (geschieht per Source-Label)
   und die Sichtbarkeit prüfen (privat, solange das Repo privat ist).

### Offene Fragen

- **F1:** Ist es akzeptabel, dass cosign keyless (öffentliches Rekor) Repository-Name, Workflow-Pfad und Commit-SHAs des
  noch privaten Repos dauerhaft öffentlich macht? Falls nein: cosign bis zur Veröffentlichung aus, Signatur nur über Tag
  und Immutable Release.
- **F2:** Hat das Konto GitHub Pro? Ohne Pro gibt es in privaten Repos keine Environments, die Schlüssel wären dann
  Repository-Secrets ohne Branch-Bindung (für Fork-PRs und Dependabot weiterhin nicht sichtbar).
- **F3:** Ein Bot-Konto (T2) oder doch der Schlüssel auf dem eigenen Konto (T3, einfacher, größerer Schadensradius)?
- **F4:** Sollen `0.x`-Releases auf GitHub generell als Pre-Release markiert werden (nicht nur bei `-rc`)?
- **F5:** Wird der Go-Modulpfad auf `github.com/joinsider/pulsequorum` umgestellt oder das Repository in
  `pulsequorum` umbenannt (B10)? Das berührt alle Importpfade und ist ein eigener Schritt.
- **F6:** Wann soll das Repository öffentlich werden? AGPL §13 und RB-LIC-04 verlangen spätestens bei Weitergabe des
  Images an Dritte einen erreichbaren Quellcode. Damit hängt Abschnitt 7 zusammen.

## Approval

| Datum      | Status   | Durch  | Anmerkung |
|------------|----------|--------|-----------|
| 2026-11-19 | Proposed | Claude |           |
| 2026-11-20 | Accepted | Projektinhaber | im Chat; vorgezogen vor AP-5.1; F1–F6 werden bei der Umsetzung geklärt |
