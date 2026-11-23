# ADR 0043: Eigenes schlankes Probe-Binary und -Image (`pulsequorum-probe`)

|                 |                                                             |
|-----------------|-------------------------------------------------------------|
| **Status**      | Accepted                                                    |
| **Datum**       | 2026-11-22                                                  |
| **Entscheider** | Projektinhaber                                              |
| **Bezug**       | Z-M-04, FA-NODE-01, FA-PRB-01..07, FA-SEC-SSRF, NFA-PERF-02, NFA-PERF-07, NFA-PORT-01, ADR 0001, ADR 0009, ADR 0030, ADR 0034, ADR 0035, ADR 0036, ADR 0040 |

## Kontext

Gemäß Z-M-04 („Ein Image/Binary für alle Rollen“) und FA-NODE-01 gibt es genau ein Go-Binary (`cmd/pulsequorum`) und ein
Container-Image (`ghcr.io/joinsider/pulsequorum` auf `gcr.io/distroless/static-debian12:nonroot`). Die Rolle wählt
`NODE_ROLE=voter|probe`.

Seit AP-5.2 (ADR 0035) betreiben Mandanten **private Probes** in ihren eigenen Netzen. Dort fallen Image-Größe und vor
allem die Meldungen von Container-Scannern bei der Sicherheitsfreigabe des Kunden stärker ins Gewicht: Das Probe-Image
enthält heute auch die SPA, die REST-/OpenAPI-Schicht, OIDC und Shoutrrr samt Abhängigkeiten, obwohl die Probe-Rolle sie
nie ausführt.

### Ist-Stand der Abhängigkeiten (gemessen am 2026-11-22)

Die Probe-Rolle ist **nicht** nur über `cmd/pulsequorum` mit dem Voter-Code verbunden. Die Probe-Pakete selbst ziehen
Cluster-Code:

| Probe-Paket            | importiert                                                                 | Grund                                                    |
|------------------------|----------------------------------------------------------------------------|----------------------------------------------------------|
| `internal/probe`       | `internal/cluster`, `membership`, `clusterpki`, `clusternet`, `store`      | Join, Zertifikatserneuerung, Dial, `cluster.ErrorClassToProto` |
| `internal/membership`  | `internal/cluster`, `clusterpki`, `clusternet`, `selfcheck`, `store`       | Join-Client (`Join`, `RenewViaLeader`) liegt im selben Paket wie der Voter-seitige Manager |
| `internal/clusterpki`  | `internal/cluster`, `store`                                                | `NodeCert` liegt im selben Paket wie die CA (`Authority`) |
| `internal/clusternet`  | `github.com/hashicorp/raft`                                                | `Mux.RaftLayer()` liegt im selben Paket wie `DialProbe`/`Security` |

Dadurch landen `hashicorp/raft`, `raft-boltdb`, `go.etcd.io/bbolt`, `internal/cluster` und `statedb` auch in einem
reinen Probe-Binary. Ein Versuchsbau (Probe-Startlogik allein, `CGO_ENABLED=0 -trimpath -ldflags='-s -w'`, per
`go build -overlay`, ohne Repo-Änderung) ergab:

| Binary                              | Größe     | Go-Module |
|-------------------------------------|-----------|-----------|
| `cmd/pulsequorum` (ohne SPA)        | 31,6 MB   | 59        |
| nur Probe-Startlogik                | 21,6 MB   | 40        |

Die SPA (`web/dist`, ca. 2,4 MB) kommt beim Haupt-Image noch hinzu. Im Probe-Binary bleiben Raft, bbolt, gRPC und
SQLite. Es entfallen vor allem `internal/webui`, `internal/api`, `internal/auth`, `internal/alert`, Shoutrrr,
`go-oidc` und `kin-openapi`.

### Sicherheitliche Einordnung

Private Probes stehen im Netz des Kunden und sollen dort weniger Angriffsfläche haben als der Cluster. Dazu gehört eine
nüchterne Abgrenzung, was ein schlankeres Image leistet und was nicht:

- **Eingehende Angriffsfläche:** Die Probe öffnet keinen Cluster-Port. Erreichbar sind nur `/healthz` und `/readyz`
  (`HTTP_ADDR`, Standard `:8080`) und optional `/metrics` (`METRICS_ADDR`, Standard aus). Daran ändert ein eigenes
  Image nichts.
- **Mitgelinkter, nie ausgeführter Code:** In Go ist Code, den die Probe-Rolle nicht aufruft, nicht über das Netz
  erreichbar. Er erhöht vor allem die Zahl der Scanner-Treffer und den Update-Druck (jede CVE in Shoutrrr, OIDC oder
  OpenAPI erzwingt ein neues Probe-Image), nicht die tatsächliche Ausnutzbarkeit.
- **Eigentliches Risiko im Kundennetz:** Eine private Probe führt Prüfaufträge aus, die der Cluster ihr schickt. Nach
  ADR 0035 ist die lokale SSRF-Sperre für private Probes standardmäßig aus, und die Prüfung beim Speichern lässt
  interne Ziele zu, wenn ein Monitor nur eigenen privaten Probes zugewiesen ist. Wer im Mandanten Monitore anlegen darf
  oder einen Voter kontrolliert, kann die Probe damit beliebige interne Ziele per HTTP (mit frei wählbaren Headern und
  Body), TCP, DNS und ICMP ansprechen lassen und erhält Ergebnisse und Fehlermeldungen zurück. Die Probe ist so ein
  möglicher Einstiegspunkt in das Kundennetz. **Dieses Risiko adressiert dieses ADR nicht.** Dafür braucht es eine
  vom Kunden lokal gesetzte Zielrichtlinie auf der Probe und gehärtete Betriebsvorgaben (siehe Folgearbeiten).

Option A weicht von **Z-M-04** ab. Das ist eine Abweichung vom Pflichtenheft und Teil dieser Entscheidung, nicht nur
eine Doku-Folgearbeit (siehe Entscheidung).

## Optionen

### Option A: Zusätzliches Probe-Binary und -Image, Trennung nur auf `cmd`-Ebene (vorgeschlagen)

Zusätzlich zum unveränderten Haupt-Image wird `cmd/pulsequorum-probe` gebaut und als
`ghcr.io/joinsider/pulsequorum-probe` (Multi-Arch) veröffentlicht. Die Pakete `internal/probe`, `membership`,
`clusterpki` und `clusternet` bleiben unverändert.

1. **Gemeinsamer Probe-Runner `internal/proberun`:** Die Startlogik aus `cmd/pulsequorum/probe.go` und die dort
   genutzten Hilfsfunktionen aus `main.go` (`readJoinToken`, `dbFileSizes`, `checkpointInterval`) sowie der
   Blank-Import `internal/checker/builtin` wandern nach `internal/proberun`. `cmd/pulsequorum` ruft bei
   `NODE_ROLE=probe` `proberun` auf (FA-NODE-01 bleibt erfüllt). `cmd/pulsequorum-probe` kennt nur `proberun` sowie
   die Subcommands `healthcheck` und `version`, nimmt `probe` als Standardrolle und beendet sich mit Exit-Code ≠ 0 und
   klarer Meldung bei `NODE_ROLE=voter`.
2. **Dockerfiles:**
   - `Dockerfile.probe` (lokaler Build): ohne `node:24-alpine`-Stage, baut nur `./cmd/pulsequorum-probe` mit
     `CGO_ENABLED=0` auf `gcr.io/distroless/static-debian12:nonroot`.
   - `Dockerfile.probe.release`: verpackt das von GoReleaser gebaute `$TARGETPLATFORM/pulsequorum-probe` mit
     `ENV DATA_DIR=/data`, `ENV NODE_ROLE=probe`, `EXPOSE 8080` (ohne `9443`), `USER nonroot:nonroot`,
     `HEALTHCHECK CMD ["/pulsequorum-probe", "healthcheck"]`, `ENTRYPOINT ["/pulsequorum-probe"]` und kopiert wie
     `Dockerfile.release` die Lizenztexte nach `/licenses`.
3. **Release (`.goreleaser.yaml`, `release.yml`):**
   - Zweiter Build `id: pulsequorum-probe` (`main: ./cmd/pulsequorum-probe`, `linux/amd64` und `linux/arm64`).
     Der globale `before`-Hook `make web-embed` bleibt für das Haupt-Binary nötig. Der Release-Lauf braucht also
     weiterhin Node, nur das Probe-Binary bettet nichts ein.
   - Eigenes Archiv `pulsequorum-probe_<ver>_<os>_<arch>.tar.gz` (`ids: [pulsequorum-probe]`) mit `LICENSE`,
     `LICENSING.md` und `THIRD_PARTY_NOTICES.md`. Die Syft-SBOM entsteht über `sboms: artifacts: archive` automatisch.
   - Zweiter `dockers_v2`-Eintrag (`ids: [pulsequorum-probe]`, `extra_files: [LICENSE]`) mit denselben Tag-Regeln wie
     das Haupt-Image (`{{ .Version }}`, ohne Prerelease `{{ .Major }}.{{ .Minor }}`, `latest` erst ab `1.0.0`).
     `docker_signs` mit `artifacts: all` signiert auch dieses Image.
   - `release.yml` löst für beide Images den Digest auf, erstellt je eine Build-Provenance und prüft vor Abschluss des
     Draft-Releases Cosign-Signatur und Attestation beider Images.
   - `THIRD_PARTY_NOTICES.md` wird weiterhin für das ganze Modul erzeugt. Das ist für das Probe-Archiv eine
     Obermenge und lizenzrechtlich ausreichend. Getrennte Hinweise je Binary sind nicht Teil dieses ADR.
4. **CI und Makefile:** `make go-build-probe` baut `bin/pulsequorum-probe`. `ci.yml` baut das Probe-Binary und prüft
   eine feste Größenschwelle von **25 MB** für das Probe-Binary (gemessen 21,6 MB). Für das Image gilt weiter
   NFA-PERF-07 (< 40 MB). Dependabot erfasst die neuen Dockerfiles über den vorhandenen `docker`-Eintrag in `/`.
5. **Architekturtest:** Ein Go-Test prüft per `go list -deps ./cmd/pulsequorum-probe`, dass `internal/webui`,
   `internal/api`, `internal/auth`, `internal/alert`, `github.com/nicholas-fedor/shoutrrr`,
   `github.com/coreos/go-oidc/v3` und `github.com/getkin/kin-openapi` nicht im Abhängigkeitsgraph landen.
   `internal/cluster`, `hashicorp/raft` und `go.etcd.io/bbolt` stehen bewusst **nicht** auf der Liste (siehe Option B).
6. **Doku und UI:** `deploy/probe/compose.yaml` (Image und Healthcheck-Pfad `/pulsequorum-probe`),
   `docs/betrieb/probe.md` und der Quick-Start im `probe-token-dialog` zeigen auf `ghcr.io/joinsider/pulsequorum-probe`.
   Der Dialog nennt dabei ein Versions-Tag statt `:latest`, weil `latest` nach ADR 0040 erst ab `1.0.0` existiert.
   Der bisherige Text `pulsequorum:latest` ist heute schon falsch.

Keine Änderung an FSM-Befehlen (Befehlsstufe bleibt `10`), Protobuf-Verträgen oder OpenAPI.

- Vorteile:
  - Probe-Binary rund ein Drittel kleiner (21,6 statt 31,6 MB, dazu entfällt die SPA), 19 Go-Module weniger.
  - Shoutrrr, OIDC, OpenAPI und SPA fallen aus Image und SBOM. CVEs in diesen Teilen erzwingen kein neues
    Probe-Image mehr, und die Sicherheitsfreigabe beim Kunden hat weniger Fundstellen zu bewerten.
  - Kein Eingriff in Sicherheitscode (mTLS/PKI, Join), kein Breaking Change, `NODE_ROLE=probe` im Haupt-Image bleibt.
- Nachteile:
  - Zweites Multi-Arch-Image in GoReleaser, Release-Workflow und Doku (mit AP-5.3 `pulsequorum-web` sind es drei).
  - Raft, bbolt und `internal/cluster` bleiben im Probe-Image. Das ist für Scanner sichtbar, aber nicht ausführbar.
  - Abweichung von Z-M-04.

### Option B: Wie A, zusätzlich Entkopplung der Probe-Pfade vom Cluster-Code

Wie Option A. Zusätzlich werden die Probe-seitigen Teile aus den Cluster-Paketen gelöst, damit `internal/cluster`,
`hashicorp/raft`, `raft-boltdb` und `go.etcd.io/bbolt` aus dem Probe-Binary verschwinden:

- `clusternet`: `RaftLayer` in ein eigenes Paket, `DialProbe`/`Security` bleiben ohne Raft-Import.
- `clusterpki`: `NodeCert` (Knotenseite) von `Authority` (CA, Raft-Befehle) trennen.
- `membership`: Join-Client (`Join`, `RenewViaLeader`) vom Voter-seitigen `Manager` trennen.
- `cluster.ErrorClassToProto` in ein neutrales Paket verschieben.
- Architekturtest zusätzlich mit `internal/cluster`, `hashicorp/raft` und `go.etcd.io/bbolt`.

- Vorteile:
  - Probe-SBOM ohne Raft und bbolt. Die Probe kann nachweislich keinen Raft- oder Zustandscode enthalten.
  - Klarere Paketgrenzen zwischen Knoten- und Cluster-Seite.
- Nachteile:
  - Umbau in sicherheitsrelevantem Code (mTLS/PKI, Join, Zertifikatserneuerung). Den schreibt nach CLAUDE.md Regel 1
    der Hauptagent, mit eigenem Review und vollständigen Regressionstests von Join, Erneuerung, Widerruf und Failover.
  - Zusatznutzen gegenüber A klein. Raft und bbolt haben wenig CVE-Historie und sind in der Probe-Rolle nicht
    erreichbar. Der Größengewinn gegenüber A wurde nicht gemessen und dürfte deutlich kleiner ausfallen.

### Option C: Weiterhin nur ein gemeinsames Image

- Vorteile:
  - Kein zusätzlicher Release- und Wartungsaufwand. Z-M-04 bleibt wörtlich erfüllt.
- Nachteile:
  - Jede Probe enthält SPA, API, OIDC und Shoutrrr. Mehr Scanner-Treffer und häufigere Pflicht-Updates im Kundennetz.

### Option D: Harte Trennung (`NODE_ROLE=probe` aus `cmd/pulsequorum` entfernen)

- Vorteile:
  - Eindeutige Rollen im Code.
- Nachteile:
  - Bricht FA-NODE-01 und bestehende Probe-Setups mit dem Haupt-Image ohne Not. Verworfen.

## Entscheidung

**Option A** wird vorgeschlagen. Sie liefert den größten Teil des messbaren Gewinns (Größe, SBOM, Update-Druck) ohne
Eingriff in Sicherheitscode. **Option B** wird ausdrücklich nicht jetzt umgesetzt. Sie wird neu bewertet, wenn
Kunden Raft oder bbolt im Probe-Image konkret beanstanden oder die Paketgrenzen aus anderen Gründen ohnehin geschnitten
werden.

Das Pflichtenheft wird mit diesem ADR angepasst:

- **Z-M-04** neu: „Ein Binary/Image (`pulsequorum`) für alle Rollen; zusätzlich ein schlankes, optionales Probe-Image
  (`pulsequorum-probe`); Wachstum von 1 auf n Voter ohne Datenmigration.“
- **FA-NODE-01** bleibt unverändert und gilt für `pulsequorum`. Für `pulsequorum-probe` ergänzt: `NODE_ROLE` leer oder
  `probe` startet die Probe, jeder andere Wert endet mit Exit-Code ≠ 0 und verständlicher Meldung.

Dieses ADR ist **kein** Sicherheitsgewinn für die Probe im Kundennetz im engeren Sinn. Die wirksamen Maßnahmen dafür
stehen unter Folgearbeiten und sind vorrangig.

## Konsequenzen

- **Positiv:** Kleineres Probe-Image, deutlich weniger Fundstellen bei Container-Scans im Kundennetz, seltener
  Pflicht-Updates der Probe, kein `EXPOSE 9443`, Architekturtest gegen unbeabsichtigte Voter-Abhängigkeiten.
- **Negativ / Risiken:** Zwei (mit AP-5.3 drei) Images müssen im Release gemeinsam auf Digest, Cosign-Signatur und
  Provenance geprüft werden. Raft und bbolt bleiben in der Probe-SBOM. Wer das Image als Sicherheitsmaßnahme
  missversteht, überschätzt den Gewinn. Doku und Release-Notes beschreiben den Nutzen deshalb nüchtern.
- **Folgearbeiten:**
  - Nach `Accepted`: Umsetzung als **AP-5.2b** (Liste aus Option A, Pflichtenheft Z-M-04/FA-NODE-01, Roadmap).
  - **Vorrangig und unabhängig von diesem ADR** (eigenes ADR, da Sicherheitsmodell): Härtung der privaten Probe im
    Kundennetz, insbesondere
    - eine **lokale Zielrichtlinie** auf der Probe (z. B. erlaubte CIDRs, Hosts, Ports), die nur der Kunde über die
      Probe-Konfiguration setzt, die der Dialer durchsetzt und die der Cluster nicht lockern kann;
    - gehärtete Vorgaben in `deploy/probe/compose.yaml` und `docs/betrieb/probe.md`: `read_only: true` mit `tmpfs`
      wo nötig, `cap_drop: [ALL]`, `security_opt: [no-new-privileges:true]`, Egress nur zu den Voter-Ports und den
      erlaubten Zielen.

## Approval

| Datum      | Status   | Durch  | Anmerkung |
|------------|----------|--------|-----------|
| 2026-11-22 | Proposed | Claude | Wartet auf Review durch Projektinhaber |
| 2026-11-22 | Proposed | Claude | Überarbeitet: Abhängigkeiten gemessen, Option B (Entkopplung) ergänzt und zurückgestellt, Z-M-04-Abweichung, Lizenzen, Tag im UI, sicherheitliche Einordnung |
| 2026-11-22 | Accepted | Projektinhaber | im Chat bestätigt |
