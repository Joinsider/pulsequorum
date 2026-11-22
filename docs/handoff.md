# Handoff für den nächsten KI-Agenten

**Stand:** 2026-11-22. `origin/master` (`ee723c5`) enthält P0–P4 (inkl. AP-4.5, AP-4.6, ADR 0033), die P5-ADRs (0034–0041) sowie AP-6.5 (ADR 0042). **AP-5.1 (Probe-Rolle nach ADR 0034)** ist auf Branch `feat/phase-5.1` (`68c91e5`) abgeschlossen; auf Branch `feat/phase-5.2` (ab `104d3df` auf `feat/phase-5.1`) ist nun **AP-5.2 (Private Probes, Mandantenbindung & Probe-Alerts nach ADR 0035)** vollständig implementiert und verifiziert. Nächstes Arbeitspaket ist **AP-5.3 (Externes Frontend & Client-Failover nach ADR 0036)**. Diese Datei ergänzt [CLAUDE.md](../CLAUDE.md) und ersetzt es nicht; bei Widerspruch gelten CLAUDE.md, das Pflichtenheft und die akzeptierten ADRs.

## 1. Zuerst lesen

1. [CLAUDE.md](../CLAUDE.md): Regeln für Delegation, ADRs, Git und Sprache.
2. [docs/roadmap.md](roadmap.md): „Aktueller Stand“ und „P5 – Reichweite“ inkl. Stack-Reihenfolge und Befehlsstufen.
3. [docs/adr/README.md](adr/README.md): ADRs 0001 bis 0042 sind alle `Accepted`. Für P5 zentral:
   - [0034](adr/0034-probe-protokoll-und-puffer.md): Probe-Protokoll, Keepalive, Failover und lokaler Puffer (AP-5.1, erledigt)
   - [0035](adr/0035-private-probes-und-mandantenbindung.md): Private Probes, Mandantenbindung und Probe-Alerts (AP-5.2, erledigt)
   - [0036](adr/0036-externes-frontend-und-client-failover.md): Externes Frontend & Client-Failover (**nächster Schritt AP-5.3**, Befehlsstufe 11)
   - [0037](adr/0037-postgresql-vorarbeiten.md)–[0040](adr/0040-ci-cd-haertung-und-manuelle-releases.md): PostgreSQL-Vorarbeiten (AP-5.4), Backup/Restore (AP-5.5a), CA-Rotation (AP-5.5b), CI/CD & Release (AP-5.0)
4. [docs/betrieb/probe.md](betrieb/probe.md) und [deploy/probe/compose.yaml](../../deploy/probe/compose.yaml): Probe-Betrieb (öffentlich & privat), Probe-Alerts, Persistenzdateien, Puffer-Parameter und Failover-Verhalten.
5. [docs/pflichtenheft.md](pflichtenheft.md): insb. FA-PRB-01..07 (Kap. 6.15), FA-UI-02/03, Konfiguration (Kap. 9.5) und Risiko PF-30 (Kap. 12.1).

## 2. Stand des Projekts

| Phase | Status |
|-------|--------|
| P0 Fundament | fertig bis auf AP-0.9/0.10 (brauchen GitHub-Läufe, mit AP-5.0 zusammengefasst) |
| P1 Kern, P2 Cluster, P3 HA-Logik | abgenommen (`docs/abnahme/p1-kern.md`, `p2-cluster.md`, `p3-ha.md`) |
| P4 Mandanten | abgenommen (`docs/abnahme/p4-mandanten.md`) |
| P5 Reichweite | ADRs 0034–0041 akzeptiert; **AP-5.1 ✅**, **AP-5.2 ✅**, AP-5.0 🔄; offen: **AP-5.3**, AP-5.5a, AP-5.5b, AP-5.4, AP-5.6 |
| P6 Ausbau | AP-6.5 ✅ (ADR 0042); AP-6.1..6.4 offen |

**Was AP-5.1 gebracht hat (Branch `feat/phase-5.1`, `68c91e5`):**
- **Verträge & Datenbank:** `ProbeService` (`Subscribe`, `Upload`, `GetBufferStatus`) in `proto/pulsequorum/v1/probe.proto`, erweiterte Registry-/Command-Protos (`command.proto`, `cluster.proto`), SQLite-Migration `00019_probe_registry.sql` (`owner_tenant_id`, `incarnation`), Puffer-Queries in `internal/store/sqlite/queries/metrics.sql` (`LastSeq` mit `MAX(heartbeats, stream_positions)`, `MinSeq`, `UnackedStats`, gestaffeltes Löschen, `IncrementalVacuum`) und `ProbeMetricsCacheKiB = 2 MiB` / `ReadConns = 1`.
- **Cluster/FSM:** `RegisterProbe` mit Feature-Gate **`ProbeCommandLevel = 10`** (`CommandVersion` ist **10**), State-Fingerprint nimmt neue Felder erst ab Stufe 10 auf; Audit-Richtlinie `probe.register` in `internal/cluster/audit.go`; Event-Fan-out `FSM.SubscribeChanges()`.
- **Sicherheit & Membership:** `DialProbe` und `ProbeOnly` in `internal/clusternet`; persistiertes `ca-bundle.pem` (`Bundle()`, `IssuerFingerprint()`, `BundleFingerprints()`) in `internal/clusterpki`; Join/Renewal (inkl. Follower-Weiterleitung via `stillMember`) und `ProbeHealthWatcher` in `internal/membership`; `Assigner` filtert öffentliche und private Probes in `internal/scheduler/assign.go`.
- **Voter-Gateway (`internal/probegw`):** Bidirektionales Streaming über `Subscribe` (Snapshot, Delta, 5-s-Keepalive-Tick, `evictRevokedSessions`), `Upload` (mit Sequenz-, Mandanten-, Monitor-, Zeitstempel- und Größe-Validierung sowie `lost_below`), Rate-Limiter und In-Memory-Geheimnisauflösung (`configsecret.ResolveMonitor`).
- **Probe-Runtime (`internal/probe`, `cmd/pulsequorum/probe.go`):** Initialer Join via `CLUSTER_SEEDS` + `JOIN_TOKEN_FILE`/`CLUSTER_JOIN_TOKEN`, persistierte Voter-Liste (`voters.json`), `Buffer` (`PROBE_BUFFER_MAX_AGE`, `PROBE_BUFFER_MAX_BYTES`, `PROBE_ACK_RETAIN`), Failover über Kandidatenliste bei `PROBE_SESSION_TIMEOUT` (Standard `15s`) + Full-Jitter-Backoff, `ErrCertRejected` bei Quorum-Ablehnung (`roundRejected > len(candidates)/2`, auch bei einem offline stehenden Voter), gRPC-Keepalive (`30s`), SSRF-Policy (`probe.Policy`), `/healthz` + `/readyz` + `/metrics`.
- **Admin-API & UI:** Erweiterte `ClusterNode` / `ProbeBufferStatus` in `api/openapi.yaml` und `internal/api/cluster.go`; `clusterhost`-Verdrahtung (`ProbeGW`, `ProbeHealth`, `ProbeSessions()`); Angular-Cluster-Ansicht (`cluster-page.*`, `cluster-rules.ts`, `remove-node-dialog.ts`, `en.json`/`de.json`) zeigt Probes samt Pufferstatus getrennt von Votern und ohne Quorum-Warnungen beim Entfernen.
- **Bereits aus ADR 0035 in AP-5.1 mitgebaut (als Defense-in-Depth im Protokoll/Gateway/Assigner):**
  - `owner_tenant_id` in `nodes`, `RegisterProbe` und `ProbeNode`-Zuweisung (`Assigner` D1)
  - Gateway-Mandantengrenze (`D3` in `probegw/subscribe.go`, `D4` in `probegw/upload.go`), Client-Filter (`D6` in `probe/client.go`)
  - `probe.Policy` trennt private (`localSSRF`, Standard aus) von öffentlichen Probes (`local || remote`)
  - Limit-Feld `maxPrivateProbes` im Plan (aus AP-4.2, noch nicht durchgesetzt)
- **Tests & Doku (AP-5.1):** Multi-Node-/Failover-, Quorum-Neutralitäts- (`FA-PRB-07`) und verkürzte AS-08-Tests inkl. Overflow/`lost_below` in `internal/clusterhost/probe_test.go`; Compose-Vorlage `deploy/probe/compose.yaml`, `docs/betrieb/probe.md`, Updates in `docs/pflichtenheft.md` und `docs/roadmap.md`.

**Was AP-5.2 gebracht hat (Branch `feat/phase-5.2`, aufbauend auf `feat/phase-5.1` `68c91e5` / `104d3df`):**
- **Verträge, Datenbank, FSM & PKI (`6d3054a`):** `api/openapi.yaml` (`/probes`, `/probes/join-token`, `/probes/{id}`, `/monitors/assignable-nodes`, `NotificationEvent.probe_offline`, `TenantUsage.privateProbes`), `command.proto` & FSM-Befehl `SetProbeAlert` (`ProbeCommandLevel = 10`, Audit-Richtlinie `probe.alert` ab `effectiveLevel >= 10`), SQLite-Migration `00020_probe_alerts.sql`, `DeleteTenant` widerruft und entfernt zugehörige private Probes (`revoked_nodes` + `nodes`), atomare Planlimit-Durchsetzung `maxPrivateProbes` bei `RegisterProbe` in `internal/cluster/fsm.go`, `CreateJoinTokenFor` in `internal/clusterpki/authority.go` inkl. Mandanten-Validierung beim Membership-Join.
- **Alerting, Metriken & Gateway-Verdrahtung (`d0c9024`):** `PROBE_OFFLINE_ALERT` (`ProbeOfflineAlert`, Standard `5m`, `0` = aus) in `internal/config`, `probe_offline`-Templates und `EventProbeOffline` in `internal/alert/template.go`, `sysalert.Collector` mit Trennung zwischen Plattform-Systemalert (`SetSystemAlert`, öffentliche Probes) und mandantenbezogenem Alert (`SetProbeAlert` ab `effectiveLevel >= 10`, private Probes), Prometheus-Counter `pulsequorum_probe_rejected_total{reason}` inkl. `OnRejected`-Callback im `probegw` (`non_probe_role`, `revoked`, `unregistered`, `owner_mismatch`, `foreign_monitor`, `upload_identity`), `LastMessageMs` und Verdrahtung in `clusterhost` & `cmd/pulsequorum`.
- **Mandanten-REST-API & SSRF-Lockerung (`773ed5c`):** `internal/api/probes.go` (isolierte Verwaltung privater Probes, `maxPrivateProbes`-Vorprüfung), D2-Validierung in `monitors.go` (`validateAssignedNodes` lehnt fremde/unbekannte Nodes mit 422 ab; `assignedOnlyOwnPrivateProbes` lockert beim Speichern auf `POST`/`PUT` die SSRF-Prüfung bei rein privater Zuweisung), `channel_secrets.go` (`probe_offline` für Mandantenkanäle ab Stufe 10, im Systemkanal `_system` mit 400 abgelehnt), `tenants.go` (`TenantUsage.PrivateProbes` & `exceededLimits`).
- **Multi-Node-Integrationstests (`69727a0`):** `internal/clusterhost/private_probe_test.go` deckt Mandantenisolation (D1–D6 + `pulsequorum_probe_rejected_total`), gleichzeitige `maxPrivateProbes`-Durchsetzung, `DeleteTenant`-Widerruf & sofortige Stream-Trennung, SSRF-Split (privat vs. öffentlich) sowie `probe_offline`-Alert-Routing (Plattform vs. Mandant) Ende-zu-Ende ab.
- **Angular-UI (`27f5c8c`):** Ansicht `/probes` (`web/src/app/features/probes/`, inkl. Join-Token-Dialog und Pufferstatus), `assignable-nodes`-Node-Picker im Monitor-Editor (`monitor-editor-page.*`, `monitor-form.*`), Kanalereignis `probe_offline`, Limit-/Verbrauchsanzeige `privateProbes` in Admin-/Tenant-Ansichten, i18n (`en`/`de`) und Komponententests.
- **Doku:** Betrieb privater Probes, `PROBE_OFFLINE_ALERT`, `pulsequorum_probe_rejected_total` und AP-5.2-Abschluss in `docs/betrieb/probe.md`, `docs/pflichtenheft.md` und `docs/roadmap.md`.

**Git-Stand & Arbeitshinweise:**
- `origin/master` liegt auf `ee723c5`; parallel im Repo liegen noch weitere WIP-Branches/Worktrees (z. B. `feat/AP5.2-*`, `feat/AP5.4-*`, `ci/AP5.0-release`) — bestehende Worktrees anderer Branches nicht ungefragt verändern oder überschreiben.
- Aktueller Branch: `feat/phase-5.2` (basiert auf `feat/phase-5.1` `68c91e5`). **Nie pushen**, außer auf ausdrückliche Anweisung des Nutzers.
- Alle Verifikationen für **AP-5.2** bestanden: `make golangci` (`0 issues`), `make go-lint`, `make proto-lint`, `make api-lint`, `make web-lint`, `GIT_CONFIG_GLOBAL=/dev/null make generate-check`, `GIT_CONFIG_GLOBAL=/dev/null make go-build`, `CI=true HOME=/tmp make web-test` (65 Dateien / 895 Tests) sowie `go test -race -count=1 ./...` (`BypassSandbox: true`, alle Pakete grün).

## 3. Arbeitsweise (beibehalten)

### Rollen & Delegation

- **Hauptagent:**
  - Architektur, Planung, ADRs, Roadmap, Abnahmeberichte, Code-Reviews und Commits.
  - Schreibt **sicherheitsrelevanten Code selbst**: Authentifizierung und Autorisierung (Mandantengrenze `tenant-admin` vs. `platform-admin`), Join-Token-Verwaltung mit Mandantenbindung, mTLS/PKI, Geheimnisse und Redaktion, SSRF-Durchsetzung, Protokoll- und Gateway-Prüfungen.
- **Einfache/abgegrenzte Aufgaben delegieren** (per `swarm` an Worker-Modell bzw. gemäß [CLAUDE.md](../CLAUDE.md) an Haiku/Hydrogen-Worker, sofern Kontingent verfügbar):
  - Store-Methoden, OpenAPI-Generierung (`make generate` prüfen), einfache API-Handler auf Basis fertiger Sicherheitshelfer, Angular-Komponenten/i18n/Specs, Unit-/Tabellentests, Lint-Fixes, Doku nach Vorgabe.
  - Prompts stets selbsterklärend halten (betroffene Dateien, ADR-Bezug, Verbot von Commits/Pushes, englisches Code-/Log-Vokabular, konkreter Prüf-Befehl).

### Review vor jedem Commit

- Diff jedes Workers vollständig lesen.
- Relevante Tests und Linters selbst ausführen (`go test -race ./...`, `make golangci`, bei Web `make web-test` + `make web-lint`).
- `GIT_CONFIG_GLOBAL=/dev/null make generate-check` **nach** jedem relevanten Commit prüfen, wenn `api/`, `proto/` oder `sqlc` berührt wurden.

### ADR-, Commit- und Sprachregeln

- **Nächster freier ADR:** **0043** (Vorlage `docs/adr/0000-template.md`, Liste `docs/adr/README.md`). Status `Accepted` setzt nur der Projektinhaber oder auf ausdrückliche Anweisung.
- **Commits:** Conventional Commits auf Englisch (`feat(...)`, `fix(...)`, `test(...)`, `docs(...)`), Body erklärt das *Warum*; logische Schritte getrennt committen.
- **Sprache:** Dokumentation (Roadmap, Betriebsdoku, Handoff, ADRs) auf **Deutsch**; Code, Bezeichner, Kommentare, API-Felder, Logs und Commit-Messages auf **Englisch**.

## 4. Nächste Schritte

1. **Stacking/Merge abstimmen:** Falls `feat/phase-5.1` bzw. `feat/phase-5.2` per PR gemergt werden sollen, `origin/master` (`ee723c5`) prüfen; solange sie offen sind, auf eigenem Branch ab `feat/phase-5.2` weiterarbeiten.
2. **AP-5.3 – Externes Frontend & Client-Failover (ADR 0036, FA-UI-02/03, Befehlsstufe 11):**
   - Zuerst [ADR 0036](adr/0036-externes-frontend-und-client-failover.md) komplett lesen und gegen den Bestand abgleichen.
   - Geplante Bausteine: `public_url`-Verbreitung über Raft (Befehlsstufe `11`), `/api/v1/cluster/nodes`, CORS/CSP sowie nginx-Container/Client-Failover für die Angular-SPA.
3. **Danach in P5 (Reihenfolge laut `docs/roadmap.md`):**
   - `AP-5.5a` Backup/Export/Restore (ADR 0038, Befehlsstufe 12)
   - `AP-5.5b` CA-Rotation über Raft (ADR 0039, Befehlsstufe 13)
   - `AP-5.4` PostgreSQL-Vorarbeiten (ADR 0037)
   - `AP-5.6` Abnahme AS-08 & 24-h-NAT-Langzeittest

## 5. Offene Punkte

**Aus AP-5.1 / AP-5.2:**
- 24-h-NAT-Langzeittest (`FA-PRB-04`) steht für Abnahme AS-08 (`AP-5.6`) aus; in `probe_test.go` ist bisher die verkürzte Simulation abgedeckt.
- Überlappendes CA-Bundle (`ca-bundle.pem`) wird bei Join/Renewal/Snapshot ausgeliefert; die dreistufige CA-Rotation selbst folgt in **AP-5.5b** (ADR 0039).
- `TestResolverSSRF` (`internal/checker/dnscheck/dnscheck_test.go:143`) kann bei parallelen `./...`-Läufen sporadisch an belegtem lokalem Port scheitern (`address already in use`); Einzellauf besteht zuverlässig.

**Aus AP-4.x (unverändert offen, sofern zutreffend):**
- **AP-4.5:** Plattform-Organisation nicht wechselbar (ADR 0032 4B); Audit-Identität bricht am Auth-Wechsel; nach Rückwechsel auf `local` gelten alte Access-Tokens bis zu 15 min; `PATCH /admin/tenants/{id}` mit `orgId` liefert bei `single` 200.
- **AP-4.4:** `POST /api-tokens` ohne Idempotency-Key; Logins/Logouts nicht im Audit-Log; Befehle vor Erreichen der alten Stufe 9 nicht im Audit-Log; kein Test mit genau 500 Batch-Operationen; STRIDE-Ergänzung für Tokens fehlt.
- **AP-4.3:** Fehlermeldungen fehlgeschlagener Checks können lokale Node-IP enthalten; 422 bei Kanälen ohne Feldzeiger; MQTT meldet bei SSRF-Sperre nur Timeout; `DeliveryError` ohne `errors.Is(..., ErrSSRFBlocked)` (Ausnahme für private Probes in AP-5.2 umgesetzt).
- **AP-4.2:** Ausstehende Alerts gesperrter Mandanten in Outbox; Purge-/Snapshot-Lauf im 30-s-Takt; lokaler Login meldet Sperre erst bei richtigem Passwort; fehlendes `ng lint`; i18n-Dateien unter `/i18n/` mit `max-age=3600` ohne Hash.
- **AP-4.1:** PocketID-Claim-Fixture & STRIDE-Ergänzung für OIDC fehlen; Zitadel Login V2 ignoriert `prompt=none`; `web/scripts/audit-exceptions.json` leer (CI-Security-Regeln nicht ohne Zustimmung ändern).

**Aus P3:**
- `raft_stale`-Schwellen (1000 Einträge / 10 s) vorläufig; `mixed_versions`-Karenzzeit beginnt nach Leaderwechsel neu; fehlgeschlagener Checkpoint wartet 1 h; Runbook „Fingerprint weicht ab“ fehlt; Degraded-Modus ohne UI-Anzeige.

## 6. Technische Stolpersteine

- **FSM & Befehlsstufen:** `Apply` muss strikt deterministisch sein (keine Uhr, kein Zufall, nur Befehl + Store). Aktuelle `CommandVersion` ist **`10`** (`ProbeCommandLevel`, `internal/cluster/commands.go`). Neues Verhalten in der FSM immer mit einem Feature-Gate an die jeweilige Befehlsstufe binden, sonst divergieren gemischte Voter im Rolling Update. Jeder neue Payload-Typ braucht zwingend einen Eintrag in `internal/cluster/audit.go`, sonst schlägt `TestAuditPolicyCoversEveryPayload` fehl.
- **State-Fingerprint:** Neue Spalten/Tabellen (z. B. für AP-5.2) erst ab der passenden `effectiveLevel`-Stufe in `internal/store/sqlite/state_snapshot.go` in den Fingerprint aufnehmen; siehe `fingerprint_level_test.go`.
- **Probe-Puffer & Sequenznummern:** `metrics.LastSeq` liest `MAX` über `heartbeats` **und** `stream_positions`, damit das Löschen bereits bestätigter Zeilen (`PROBE_ACK_RETAIN`) die `seq` nach einem Neustart nie auf 0 zurücksetzt. Bei Überlauf (`PROBE_BUFFER_MAX_AGE` / `PROBE_BUFFER_MAX_BYTES`) rückt `lost_below` vor, sodass der Voter Lücken sauber überspringt.
- **Probe-Zertifikatswiderruf (`ErrCertRejected`):** `internal/probe/client.go` bricht mit `ErrCertRejected` ab, sobald eine Mehrheit der bekannten Voter (`roundRejected > len(candidates)/2`) das Zertifikat im Round-Robin ablehnt — dadurch endet eine entfernte Probe auch dann sauber, wenn ein einzelner Minderheits-Voter offline ist.
- **Sandbox & Git im Tool-Runner:**
  - In restriktiver Sandbox kann `~/.config/git/github.gitconfig` unlesbar sein und `.git/index.lock` bzw. `127.0.0.1`-TCP-Connect (`internal/api`, `internal/clusternet`, `internal/clusterhost`, `make golangci` via `go run`) blockiert werden. Nur rein lesende Sandbox-Aufrufe (z. B. `make generate-check`, `git log`/`git diff` in der Sandbox) dürfen `GIT_CONFIG_GLOBAL=/dev/null` nutzen — `git commit`, `git rebase` usw. mit `BypassSandbox: true` **immer ohne `GIT_CONFIG_GLOBAL=/dev/null`** ausführen, damit Name, E-Mail und SSH-Signatur aus `~/.config/git/github.gitconfig` greifen.
- **Generierter Code:** OpenAPI (`api/openapi.yaml`), Protobuf (`proto/`) und SQL (`queries/`) nie von Hand in den `gen`/`pb`/`*db`-Verzeichnissen ändern; nach Änderungen `make generate` und nach dem Commit `GIT_CONFIG_GLOBAL=/dev/null make generate-check` ausführen.
- **Test-Umgebungen & Container:** Laufende `sonarqube-mcp`-Container nicht stoppen. Web-Tests immer über `cd web && npx ng test --watch=false` (oder `make web-test`), nicht direkt mit `npx vitest`.
- **Bekannter flackernder Test (P3):** `TestDegradedModeAlertsAndCorrects` (`internal/clusterhost`) schlägt selten (~1/8 Läufen vor P5 beobachtet) mit `false_alarm` statt `confirmed` fehl (Vermutung: `last_issued_at` / `gapMinAge`-Timing in `internal/cluster/fsm.go`); vor Fix wäre ein ADR nötig.
