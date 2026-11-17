# Handoff für die nächste Claude-Code-Session

**Stand:** 2026-11-17, nach Abschluss von AP-4.1 (OIDC-Anbindung, ADR 0027/0028). PR [Joinsider/uptime-cluster#16](https://github.com/Joinsider/uptime-cluster/pull/16) ist gemergt. Diese Datei ist der Einstieg für eine neue Session. Sie ergänzt [CLAUDE.md](../CLAUDE.md), ersetzt es aber nicht. Bei Widerspruch gelten CLAUDE.md, das Pflichtenheft und die akzeptierten ADRs.

## 1. Zuerst lesen

1. [CLAUDE.md](../CLAUDE.md): Regeln für Delegation, ADRs, Git und Sprache.
2. [docs/roadmap.md](roadmap.md): „Aktueller Stand“ und P4. Der Eintrag zu AP-4.1 listet, was gebaut ist und was offen blieb.
3. [docs/adr/README.md](adr/README.md): ADRs 0001 bis 0028 sind alle Accepted. Für P4 zentral: [0027](adr/0027-oidc-anbindung.md) (OIDC, Profile, Mandanten-Bindung) und [0028](adr/0028-rollen-aus-userinfo-und-client-id.md) (Rollen aus `/userinfo`, `client_id`).
4. [docs/betrieb/oidc.md](betrieb/oidc.md): Betriebsdoku zu OIDC mit den Befunden aus den Tests gegen echte Anbieter.
5. Pflichtenheft (Version 1.4) für den nächsten Schritt: FA-TEN-03, FA-TEN-04, dazu FA-TEN-01/02/05, NFA-SEC-02 und das Abnahmeszenario AS-06.

## 2. Stand des Projekts

| Phase | Status |
|-------|--------|
| P0 Fundament | fertig bis auf AP-0.9/0.10 (brauchen GitHub-Läufe) |
| P1 Kern, P2 Cluster, P3 HA-Logik | abgenommen (`docs/abnahme/p1-kern.md`, `p2-cluster.md`, `p3-ha.md`) |
| **P4 Mandanten** | **AP-4.1 ✅, nächster Schritt AP-4.2** |
| P5 Reichweite, P6 Ausbau | offen |

**Was AP-4.1 gebracht hat (Kurzfassung):**
- `AUTH_MODE=oidc` mit Profilen `generic` (ein Mandant `default`, Rollen aus Claim oder `/userinfo`, `OIDC_ROLE_MAP`) und `zitadel` (Mandant = Organisation, `single`/`multi`, `platform-admin` nur aus `ZITADEL_PLATFORM_ORG_ID`).
- Verifier in `internal/auth/oidc` (go-oidc mit eigenem `KeySet`, JWKS-Cache mit Datei, Ausfallverhalten 503), Fake-IdP `internal/auth/oidctest`.
- FSM-Befehl `BindTenantOrg`, **`CommandVersion` ist jetzt 7** (`OIDCCommandLevel`, Feature-Gate nach ADR 0018). Store-Methode `GetTenantByOrg`.
- SPA mit `angular-auth-oidc-client` 22.0.3: Tokens nur im RAM, nach Reload ein stiller Login.
- Ende zu Ende geprüft gegen Keycloak 26.8.2, Zitadel v4.20.3 (beide lokal unter `deploy/oidc-dev/`) und eine echte PocketID-Instanz von Projektinhaber.

**Git:**
- `feat/AP4` ist über PR #16 in `master` gemergt. Vor neuer Arbeit: `git fetch`, `master` aktualisieren, `git status` prüfen.
- Projektinhaber fragen, auf welchem Branch AP-4.2 beginnt (Vorschlag: neuer Branch von `master`, z.B. `feat/AP4.2`).
- Pushen und PRs nur auf ausdrückliche Anweisung. Wenn Projektinhaber „Auto-fix“ für einen PR einschaltet, kommen CI-Fehler und Review-Kommentare als `<ci-monitor-event>`; dann ohne Rückfrage beheben und pushen.

## 3. Arbeitsweise (beibehalten)

### Rollen

- **Hauptagent (Opus):**
  - Planung, Zerlegung in Aufträge, ADRs, Roadmap, Abnahmeberichte, Review und Commits.
  - Schreibt **sicherheitsrelevanten Code selbst**: Authentifizierung und Autorisierung, Durchsetzung der Mandantengrenze, SSRF-Schutz, mTLS/PKI, Geheimnisse und Redaktion, Token-Behandlung in der SPA. Für AP-4.2 betrifft das vor allem: wer Mandanten anlegen, sperren und löschen darf, die Auto-Provisionierung (welche Organisation wird wann Mandant), die Validierung der Org-ID und die Löschung mit Daten.
- **Haiku-Subagenten** (`Agent` mit `model: "haiku"`):
  - Alles andere: Handler, Store, Migrationen, FSM-Befehle nach Vorgabe, Jobs, Angular-Komponenten, Konfiguration, Verdrahtung, Tests, Lint-Fixes, Recherche, Doku nach Vorgabe, Testumgebungen.

### So sehen Aufträge an Haiku aus

- **Selbsterklärend:** Repo-Pfad, Kontext (welcher ADR, welche Anforderung), betroffene Dateien und Pakete, erwartetes Verhalten, Beispiele für Fehlerfälle.
- **Klare Grenzen:** „nichts committen“, „nur diese Dateien anfassen“, „keine neue Abhängigkeit“.
- **Akzeptanzkriterium als Befehl:** z.B. `go test -race ./internal/cluster/...` grün und `make golangci` mit 0 Issues; bei Web `cd web && npx ng test --watch=false`.
- **Zuerst die Sicherheitsfunktion:** Berührt ein Auftrag Sicherheitscode, schreibt der Hauptagent vorher die Hilfsfunktion (z.B. `auth.FromContext`, `validOrgID`). Haiku ruft sie nur auf.
- **Parallel nur getrennt:** Unabhängige Aufträge laufen parallel in einer Nachricht, aber nur auf getrennten Dateien und Paketen. Sonst nacheinander.
- **Englische Texte:** Fehlermeldungen, Logs, Alert-Texte und Bezeichner ausdrücklich auf Englisch verlangen.

### Review vor jedem Commit (Hauptagent)

- Diff lesen. Berichte der Subagenten sind Aussagen, kein Beweis.
- Selbst laufen lassen: `go test -race ./...`, `make golangci`, bei Web-Änderungen `cd web && npx ng test --watch=false` und `npx prettier --check src`.
- `make generate-check` **nach** dem Commit ausführen, weil es gegen git diffed. Bei neuen Abhängigkeiten `make licenses`.
- Typische Lücken bei Haiku: Verdrahtung in `main` vergessen, fehlende Store-Methoden umgangen statt ergänzt, Fehlerwerte ignoriert, zu lockere Tests, deutsche Texte im Code.

### ADR-Ablauf

- **Nummer:** Nächster ADR ist **0029**. Vorlage `docs/adr/0000-template.md`, Eintrag in `docs/adr/README.md`.
- **Accepted:** setzt nur Projektinhaber, oder Claude auf seine ausdrückliche Anweisung im Chat. In der Approval-Tabelle dann z.B. „im Chat angenommen („Ja ADR 28 annehmen“), eingetragen durch Claude“.
- **Optionen mit Empfehlung:** Projektinhaber entscheidet Richtungsfragen über kurze Auswahlen mit Kennungen (z.B. „1A, 2B“). Bei Zielkonflikten Optionen mit Vor- und Nachteilen und einer Empfehlung vorlegen; bei Sicherheitsfragen die sicherere Option empfehlen und sagen, warum.
- **Befund aus Tests → neuer ADR:** Zeigt ein Test, dass ein akzeptierter ADR nicht trägt (so entstand ADR 0028 aus dem PocketID-Test), zuerst einen neuen ADR vorschlagen, nicht still abweichen.

### Commits und Doku

- Conventional Commits auf Englisch, der Body erklärt das Warum und nennt Anforderungs-IDs. Letzte Zeile: `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Ein Commit je logischem Schritt: ADR, Fix, Feature, Doku getrennt. Ist ein Commit versehentlich gemischt, vor dem Push mit `git reset --soft` aufteilen.
- Roadmap im selben Commit wie den Abschluss des Arbeitspakets aktualisieren.
- Doku auf Deutsch, Code und Kommentare auf Englisch.
- Abnahmeberichte unter `docs/abnahme/pN-*.md` nach dem Muster von `p3-ha.md`.

### Tests gegen echte Systeme

- Unit-Tests mit Fakes reichen nicht für Integrationen: AP-4.1 hat fünf Fehler erst gegen echte IdPs gezeigt (fehlender Mandant `default`, `client_id` statt `azp`, Anzeigename, Token-Laufzeit 3601 s, zweiter Reload). Für neue Integrationen eine reproduzierbare lokale Umgebung bauen (Muster: `deploy/oidc-dev/`) und Ende zu Ende prüfen.
- Gefundene Fehler: Regressionstest dazu, Befund in der Betriebsdoku festhalten.
- Logins im Browser macht Projektinhaber selbst (Passkey, Passwort). Nie Zugangsdaten erfragen oder eintippen. Testnutzer der lokalen Umgebungen kommen aus deren Setup-Skripten.
- **Browser:** Das eingebaute Browser-Fenster blockiert Skripte mancher externer Seiten (PocketID blieb schwarz, `ERR_BLOCKED_BY_CLIENT`). Dann Claude in Chrome verwenden, nach Rückfrage.
- **Speicher-Hygiene der SPA** nach jedem Login prüfen: kein JWT, kein Refresh-Token, keine Profildaten in `localStorage`/`sessionStorage`/IndexedDB.

## 4. Nächster Schritt: AP-4.2 Mandantenverwaltung, Pläne, Limits, Auto-Provisionierung

Anforderungen: FA-TEN-03 (Verwaltung durch `platform-admin`: anlegen, sperren, löschen mit Datenlöschung, Plan/Limits; automatische Anlage beim ersten Login einer neuen Zitadel-Organisation, abschaltbar, nur Profil `zitadel` mit `OIDC_TENANCY=multi`) und FA-TEN-04 (Limits je Plan: Monitore, kürzestes Intervall, Statusseiten, private Probes, Kanäle; Überschreitung → 403 mit verständlicher Meldung).

1. **Lesen:** ADR 0027 (Abschnitte Mandanten und Zitadel-Profil), `internal/cluster/tenants.go` (`BindTenantOrg`, `validOrgID`), `internal/cluster/fsm.go` (`PutEntity`), `internal/auth/oidc/profile.go` (`Zitadel.Map` liefert heute 403 „not provisioned“ für unbekannte Organisationen), Store-Methoden für Mandanten, die bestehenden Admin-Endpunkte in `api/openapi.yaml`.
2. **ADR 0029 entwerfen** (Proposed), weil Schema, FSM-Befehle und OpenAPI-Vertrag sich ändern. Er soll klären:
   - Datenmodell für Pläne und Limits (fest im Code, konfigurierbar oder im Raft-Zustand) und Standardplan.
   - FSM-Befehle für Anlegen, Sperren, Plan ändern, Löschen; neue `CommandVersion` 8 mit Feature-Gate.
   - Löschen mit Datenlöschung: Reihenfolge (sperren → Daten löschen → Mandant entfernen), Metrik-Daten in `metrics.db`, Geheimnisse, Wiederanlauf nach Abbruch, `_system` und `default` nie löschbar.
   - Auto-Provisionierung: wer den FSM-Befehl auslöst (Anfragepfad des Followers → Leader), Idempotenz bei parallelen ersten Logins, Schalter zum Abschalten, Missbrauchsschutz (Rate-Limit, Höchstzahl).
   - Durchsetzung der Limits: wo geprüft wird (FSM deterministisch und/oder API), Verhalten bei gesenktem Plan mit schon überschrittenem Bestand.
   - Offener Punkt aus AP-4.1: `PutEntity` für Mandanten muss die Org-ID genauso prüfen wie `BindTenantOrg` (eindeutig, gültig, `_system` nie gebunden).
3. **Committen und warten:** `docs(adr): propose 0029 …`, AP-4.2 in der Roadmap auf `blockiert (ADR 0029)`, Projektinhaber um Review bitten. Umsetzen erst nach Accepted.
4. **Umsetzen:** Autorisierung, Provisionierungsregeln, Org-ID-Validierung und Löschlogik schreibt der Hauptagent. API-Handler nach Vorgabe, Migrationen, Admin-UI, Tests und Doku gehen an Haiku. Mandantentrennung mit zwei Mandanten testen (Vorbereitung für AS-06), auch über den Zitadel-Testaufbau `deploy/oidc-dev/zitadel` (Org `acme` bekommt heute 403, nach AP-4.2 soll sie provisioniert werden).

## 5. Offene Punkte

**Aus AP-4.1:**
- Claim-Fixture aus PocketID fehlt (Claims nur aus dem Quelltext belegt; Keycloak und Zitadel liegen in `internal/auth/oidc/testdata/`). PocketID-Version für die Doku bei Projektinhaber erfragen.
- STRIDE-Ergänzung für OIDC fehlt.
- Zitadel Login V2 ignoriert `prompt=none` (zeigt die Login-Maske); dokumentiert, keine Code-Änderung.
- **npm-Audit-Ausnahme:** `web/scripts/audit-exceptions.json` erlaubt `GHSA-fc5q-39xc-vjpf` (`picomatch`, nur Build-Werkzeuge) bis **2026-12-31**. Die CI schlägt wieder an, wenn das Advisory verschwindet (dann Ausnahme löschen) oder das Datum abläuft (neu bewerten). Änderungen an CI-Sicherheitsprüfungen nur mit Projektinhaber' Zustimmung; der Auto-Modus blockiert sie sonst zu Recht.

**Aus P3 (unverändert offen):**
- `raft_stale`-Schwellen (1000 Einträge / 10 s) sind vorläufig, von Projektinhaber nicht bestätigt.
- „Quorum restored“ wird als Paar aus aktiv und Entwarnung gesendet.
- `mixed_versions`: Karenzzeit beginnt nach Leaderwechsel neu.
- Fehlgeschlagener Checkpoint wartet eine volle Stunde.
- Probe-offline-Alerts kommen mit P5.
- Runbook-Abschnitt „Fingerprint weicht ab“ fehlt unter `docs/betrieb`.
- AS-05 erkennt Systemalerts nur über den Titel einer Einzelnachricht; Quorumverlust 1|1|1 nur im Integrationstest; Docker im Chaos-Harness nicht getestet.
- Degraded-Modus hat keine Anzeige in der UI.
- npm `allowScripts` blockiert Install-Skripte (esbuild, @parcel/watcher, lmdb …); Build läuft trotzdem. Klären, ob gewollt.

## 6. Technische Stolpersteine

- **FSM:** `Apply` ist deterministisch (keine Uhr, kein Zufall, kein I/O außer dem lokalen Store). Neue Befehle brauchen eine Befehlsversion und ein Feature-Gate (ADR 0018); aktuell `CommandVersion` 7. Zeitpunkte gehören in den Befehl, nicht ins `Apply`.
- **Generierter Code:** nie von Hand ändern. Neue REST-Operationen zuerst in `api/openapi.yaml`, dann `make generate`; SQL über sqlc, Migrationen mit goose.
- **Mandant `_system`:** reserviert (`store.SystemTenantID`). FSM, Auth und `checkTenant` im OIDC-Verifier sperren ihn; nie an eine Organisation bindbar, nie löschbar. Diese Grenze darf AP-4.2 nicht aufweichen.
- **Mandant `default`:** wird in jedem Auth-Modus beim Bootstrap angelegt (`EnsureDefaultTenant`). Im Profil `zitadel` bindet der Leader ihn an `ZITADEL_PLATFORM_ORG_ID`.
- **Auth-Fehler:** `auth.ErrUnauthenticated` → 401, `ErrForbidden` → 403, `ErrAuthUnavailable` → 503, `ErrOpaqueToken` → 401 mit Hinweis (Abbildung in `internal/api/middleware.go`, `authProblem`). Im OIDC-Modus antworten lokale Anmeldung und Nutzerverwaltung 409.
- **Lokale OIDC-Testumgebungen:** `deploy/oidc-dev/keycloak` (127.0.0.1:8081) und `deploy/oidc-dev/zitadel` (127.0.0.1:8082, Login V2 auf 3000), jeweils `setup.sh`, `token.sh`, `down.sh`; Geheimnisse landen im ignorierten Ordner `.generated/`. Anleitung in `deploy/oidc-dev/README.md`. Die Container können von der letzten Session noch laufen: mit `down.sh` aufräumen, wenn sie nicht gebraucht werden.
- **Lint:** `make golangci` (golangci-lint v2.15.0 über `go run`, nicht global installiert) prüft das ganze Modul.
- **Web-Tests:** `npx vitest` direkt läuft ohne Angular-Umgebung nicht; immer `npx ng test --watch=false`.
- **Chaos-Harness:** `go run ./test/chaos/cmd/chaos -scenarios …`, braucht Podman rootless, dauert Minuten (im Hintergrund starten), Logs vor dem Aufräumen sichern.
- **Govulncheck:** prüft gegen die Go-Version aus `go.mod`; bei neuen Stdlib-Meldungen die `go`-Zeile anheben.
