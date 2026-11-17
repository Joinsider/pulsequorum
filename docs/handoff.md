# Handoff für die nächste Claude-Code-Session

**Stand:** 2026-11-17, nach Abschluss von P3 (AP-3.1 bis AP-3.7). Diese Datei ist der Einstieg für eine neue Session. Was hier steht, ergänzt [CLAUDE.md](../CLAUDE.md), ersetzt es aber nicht. Bei Widerspruch gelten CLAUDE.md, das Pflichtenheft und die akzeptierten ADRs.

## 1. Zuerst lesen

1. [CLAUDE.md](../CLAUDE.md): Regeln für Delegation, ADRs, Git und Sprache.
2. [docs/roadmap.md](roadmap.md), Abschnitt „Aktueller Stand“ und P4.
3. [docs/adr/README.md](adr/README.md): ADRs 0001 bis 0026 sind alle Accepted.
4. [docs/abnahme/p3-ha.md](abnahme/p3-ha.md): letzter Abnahmebericht mit offenen Einschränkungen.
5. Für P4 im [Pflichtenheft](pflichtenheft.md): FA-AUTH-05..08, FA-AUTH-10, FA-TEN-03/04, FA-SEC-SSRF, FA-ALR-12, FA-DAT-01, FA-MON-06, FA-UI-05 und die Abnahmeszenarien AS-06/07.

## 2. Stand des Projekts

| Phase | Status |
|-------|--------|
| P0 Fundament | fertig bis auf AP-0.9/0.10 (brauchen GitHub-Läufe) |
| P1 Kern (Single-Voter) | abgenommen ([p1-kern.md](abnahme/p1-kern.md)) |
| P2 Cluster | abgenommen ([p2-cluster.md](abnahme/p2-cluster.md)) |
| P3 HA-Logik | abgenommen ([p3-ha.md](abnahme/p3-ha.md)) |
| **P4 Mandanten** | **aktiv, nächster Schritt AP-4.1** |
| P5 Reichweite, P6 Ausbau | offen |

**Was P3 gebracht hat:**
- Konsens-Bewertung auf dem Leader (ADR 0021).
- Node-Selbstgesundheit (ADR 0022).
- Alert-Drosselung, Bündelung und Flapping (ADR 0023). DOWN wird nie zurückgehalten (ADR 0026).
- Degraded-Modus mit unbestätigten Alerts und Korrektur nach der Heilung (ADR 0024).
- Systemalerts über den reservierten Mandanten `_system` und Prometheus-Metriken (ADR 0025).
- Chaos-Harness unter `test/chaos`, mit Podman rootless.

**Git:**
- Gearbeitet wird auf Branch `feat/AP3`, PR [Joinsider/uptime-cluster#15](https://github.com/Joinsider/uptime-cluster/pull/15).
- Drei CI-Fixes sind lokal committet, aber noch nicht gepusht: `d20c717` (Go 1.27.4), `cbc10c8` (orval 8.48.0) und `3597e86` (Lint über `./...`).
- Vor neuer Arbeit mit `git status` und `gh pr checks 15` klären, ob gepusht und gemergt ist. Projektinhaber fragen, ob P4 auf einem neuen Branch (z.B. `feat/AP4`) beginnt.
- Pushen nur auf ausdrückliche Anweisung.

## 3. Arbeitsweise (beibehalten)

### Rollen

- **Hauptagent (Opus):**
  - Planung, Zerlegung in Aufträge, ADRs, Roadmap, Abnahmeberichte, Review und Commits.
  - Schreibt **sicherheitsrelevanten Code selbst**: Authentifizierung und Autorisierung, Mandantengrenze, SSRF-Schutz, mTLS/PKI, Geheimnisse und Redaktion. Für P4 betrifft das **den größten Teil von AP-4.1, AP-4.3 und AP-4.4**.
- **Haiku-Subagenten** (`Agent` mit `model: "haiku"`):
  - Alles andere: Handler, Store, Migrationen, FSM-Befehle, Jobs, Angular-Komponenten, Verdrahtung, Tests, Lint-Fixes, Recherche, Doku nach Vorgabe.

### So sehen Aufträge an Haiku aus

- **Selbsterklärend:** Repo-Pfad, Kontext (welcher ADR, welche Anforderung), betroffene Dateien und Pakete, erwartetes Verhalten.
- **Klare Grenzen:** „nichts committen“, „nur diese Dateien anfassen“.
- **Akzeptanzkriterium als Befehl:** z.B. `go test -race ./internal/alert/...` grün und `make golangci` mit 0 Issues.
- **Zuerst die Sicherheitsfunktion:** Berührt ein Auftrag Sicherheitscode, schreibt der Hauptagent vorher die Hilfsfunktion, z.B. `auth.FromContext` oder `metrics.RequireToken`. Haiku ruft sie nur auf.
- **Parallel nur getrennt:** Unabhängige Aufträge laufen parallel in einer Nachricht, aber nur auf getrennten Dateien und Paketen. Sonst nacheinander.
- **Englische Texte:** Alert-Texte, Logs und Bezeichner ausdrücklich auf Englisch verlangen. Haiku hat sonst schon deutsche Alert-Texte geschrieben.

### Review vor jedem Commit (Hauptagent)

- Diff lesen. Berichte der Subagenten sind Aussagen, kein Beweis.
- Selbst laufen lassen: `go test -race ./...`, `make golangci`, bei Web-Änderungen `cd web && npm test -- --watch=false`.
- `make generate-check` **nach** dem Commit ausführen, weil es gegen git diffed.
- Typische Lücken bei Haiku:
  - Verdrahtung vergessen (z.B. ein neuer Aggregator war nicht als Reconcile-Ziel eingetragen).
  - Fehlende Store-Methoden umgangen statt ergänzt.
  - Fehlerwerte ignoriert.
  - Zu lockere Tests.

### ADR-Ablauf

- **Nummer:** Nächster ADR ist **0027**. Vorlage: `docs/adr/0000-template.md`, eintragen in `docs/adr/README.md`.
- **Accepted:** Setzt nur Projektinhaber, oder Claude auf seine ausdrückliche Anweisung im Chat. In der Approval-Tabelle steht dann z.B. „im Chat angenommen („Annehmen, umsetzen“), eingetragen durch Claude“.
- **Optionen anbieten:** Projektinhaber entscheidet Richtungsfragen gern über kurze Auswahlen, z.B. „DOWN nie zurückhalten“. Bei echten Zielkonflikten Optionen mit Empfehlung vorlegen, nicht selbst entscheiden.
- **Abweichungen:** Abweichungen von Pflichtenheft oder ADR immer per neuem ADR, nie stillschweigend.

### Commits und Doku

- Conventional Commits auf Englisch, der Body erklärt das Warum. Letzte Zeile: `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Ein Commit je logischem Schritt. Die Roadmap im selben Commit wie den Abschluss des Arbeitspakets aktualisieren.
- Dokumentation auf Deutsch, Code und Kommentare auf Englisch.
- Abnahmeberichte unter `docs/abnahme/pN-*.md`, nach dem Muster von `p3-ha.md`: Messumgebung, Aufruf, Tabelle der Kriterien, Befunde, Einschränkungen.

## 4. Nächster Schritt: AP-4.1 Zitadel-Anbindung

1. **Lesen:** Roadmap P4, FA-AUTH-05..08 und die bestehenden Auth-ADRs. Dazu den aktuellen Code `internal/auth` (lokale Benutzer, Sessions, Refresh-Cookie in `internal/api/auth.go`) und `auth.FromContext` (lehnt Principals aus `_system` ab).
2. **ADR 0027 entwerfen**, Status Proposed. Er soll klären:
   - JWT-Validierung und Bibliothek, mit Lizenzprüfung nach ADR 0010.
   - JWKS-Cache und Schlüsselrotation.
   - Abbildung der Claims auf Mandant und Rollen.
   - Koexistenz mit der lokalen Anmeldung bis AP-4.5.
   - Angular-OIDC (PKCE) und die Laufzeit-`config.json`.
   - Verhalten bei einem Ausfall von Zitadel, angesichts der HA-Ziele.
   - Ein Testkonzept ohne echtes Zitadel, z.B. ein lokaler OIDC-Fake im Test.
3. **Committen und warten:** Commit `docs(adr): propose 0027 …`, AP-4.1 in der Roadmap auf `blockiert (ADR 0027)` setzen, um Review bitten. Umsetzen erst nach Accepted.
4. **Umsetzen:** Validierung und Autorisierung schreibt der Hauptagent. UI, Konfiguration und Tests gehen an Haiku.

## 5. Offene Punkte aus P3

- **raft_stale-Schwellen:** 1000 Einträge / 10 s sind vorläufig, Projektinhaber hat sie noch nicht bestätigt.
- **Quorum restored:** wird als Paar aus aktiv und Entwarnung gesendet.
- **mixed_versions:** Die Karenzzeit beginnt nach einem Leaderwechsel neu.
- **Checkpoint:** Ein fehlgeschlagener Checkpoint wartet eine volle Stunde bis zum nächsten Versuch.
- **Probe-offline-Alerts:** kommen erst mit P5.
- **Runbook:** Der Abschnitt „Fingerprint weicht ab“ fehlt unter `docs/betrieb`.
- **AS-05:** erkennt Systemalerts nur über den Titel einer Einzelnachricht.
- **Quorumverlust 1|1|1:** nur im Integrationstest abgedeckt, nicht im Chaos-Harness.
- **Docker:** im Harness nicht getestet.
- **Degraded-Modus:** Anzeigen dafür fehlen in der UI.
- **npm:** `allowScripts` blockiert Install-Skripte (esbuild, @parcel/watcher, lmdb …). Der Build läuft trotzdem. Prüfen, ob das gewollt ist.

## 6. Technische Stolpersteine

- **FSM:**
  - Raft-FSM-`Apply` ist deterministisch.
  - Neue Befehle brauchen eine Befehlsversion und ein Feature-Gate nach ADR 0018. Aktuell ist `CommandVersion` 6.
- **Generierter Code:**
  - Nie von Hand ändern. Neue REST-Operationen zuerst in `api/openapi.yaml`, dann `make generate`.
  - SQL-Zugriffe über sqlc, Migrationen mit goose.
- **Mandant `_system`:** ist reserviert (`store.SystemTenantID`). FSM und Auth sperren ihn für normale Akteure. P4 darf diese Grenze nicht aufweichen. Tests dazu gibt es in `internal/auth` und in der FSM.
- **Lint:** `make golangci` und CI prüfen jetzt das ganze Modul (`./...`), auch `test/`.
- **Chaos-Harness:**
  - Aufruf: `go run ./test/chaos/cmd/chaos -scenarios as04,as04nb,as05`.
  - Braucht Podman rootless und dauert mehrere Minuten, deshalb im Hintergrund starten.
  - Logs vor dem Aufräumen sichern, sonst lassen sich Fehlschläge nicht mehr belegen.
- **Govulncheck:** prüft gegen die Go-Version aus `go.mod`. Bei neuen Stdlib-Meldungen die Zeile `go` anheben.
