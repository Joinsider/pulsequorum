# ADR 0035: Private Probes, Mandantenbindung und Probe-Alerts

|                 |                                                                                                                                                                                                                                                                                                                                                                     |
|-----------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **Status** | Accepted |
| **Datum**       | 2026-11-19                                                                                                                                                                                                                                                                                                                                                          |
| **Entscheider** | Projektinhaber                                                                                                                                                                                                                                                                                                                                                            |
| **Bezug**       | FA-PRB-05, FA-PRB-06, FA-PKI-02/04, FA-TEN-04, FA-SEC-SSRF, FA-OBS-01, FA-UI-04/05, AS-06, PF-10; Konzept 7.1 (Private Probes), E11, E21; ADR 0006, ADR 0015, ADR 0017, ADR 0018, ADR 0023 (`channel_failed`), ADR 0025 (Systemalerts), ADR 0029 (Limits, Löschen), ADR 0030 Abschnitt 5 (Vorgabe für private Probes), ADR 0031 (Audit), ADR 0034 (Probe-Protokoll) |

## Kontext

Ein Mandant soll eigene Probes in seinem Netz betreiben können, um interne Dienste zu prüfen (Konzept 7.1). Diese
**privaten Probes** (`owner_tenant_id`) dürfen nur Monitore ihres Mandanten sehen und nur für diese Heartbeats liefern
(FA-PRB-05). Registriert werden sie durch den `tenant-admin` über ein Join-Token mit Mandantenbindung. Fällt eine Probe
aus, meldet die Plattform das: als Systemalert bei öffentlichen Probes, als Mandanten-Alert bei privaten (FA-PRB-06).

Private Probes verschieben die Vertrauensgrenze. Sie laufen in Netzen und auf Hosts, die der Kunde kontrolliert, unter
Umständen mit verändertem Binary. Ein privater Probe ist deshalb als **nicht vertrauenswürdiger Client** zu behandeln,
der ein gültiges Zertifikat besitzt. Er darf weder Daten anderer Mandanten erhalten noch fremde Ströme, Monitore oder
Mandanten beschreiben.

**Vorhanden**

- `pki.Identity.TenantID`: Die URI-SAN `uc://<cluster>/probe/<id>?tenant=<t>` erlaubt den Mandanten nur für die Rolle
  `probe`.
- `clusterpki.Authority.CreateJoinToken(role, tenantID, ttl)` und `Redeem`: Die Identität kommt aus dem gespeicherten
  Token, nicht aus CSR oder Anfrage.
- FSM `PutJoinToken`: prüft, dass der Mandant existiert, und erlaubt `tenant` nur bei `probe`.
- API `POST /api/v1/admin/join-tokens` mit optionalem `tenantId` (nur `platform-admin`).
- Limit `maxPrivateProbes` ist im Plan gespeichert, aber nicht durchgesetzt (ADR 0029).
- ADR 0030 Abschnitt 5 gibt vor: Ein privater Probe gehört genau einem Mandanten, prüft nur dessen Monitore und nutzt
  eine eigene, lokale SSRF-Richtlinie (Standard aus). Voter und öffentliche Probes lockern nie für Mandantenziele.
  Benachrichtigungen gehen immer von Votern aus.

**Fehlend**

- Register ohne `owner_tenant_id` (ADR 0034 ergänzt die Spalte und `RegisterProbe`).
- Kein Mandanten-Endpunkt für Probes.
- Keine Prüfungen im (noch nicht vorhandenen) Probe-Gateway.
- Keine Probe-Alerts.

## Optionen

### Frage 1: Wo ist die Mandantenbindung verankert?

**1A Nur im Zertifikat (URI-SAN)**

- Vorteile: Gibt es schon. Jede Prüfung hat die Bindung ohne Zustandszugriff. Sie lässt sich nicht nachträglich ändern.
- Nachteile: Löschen oder Sperren eines Mandanten wirkt erst über die Sperrliste. Admin-UI und Zuweisung brauchen die
  Bindung trotzdem im Register.

**1B Nur im Register (`nodes.owner_tenant_id`)**

- Vorteile: Änderbar, sofort clusterweit sichtbar.
- Nachteile: Jede Prüfung im Gateway braucht einen Zustandszugriff. Ein Fehler beim Registrieren könnte eine Probe
  öffentlich machen. Das Zertifikat sagt dann nichts über die Berechtigung.

**1C Zertifikat ist maßgeblich, das Register spiegelt es; der Gateway verlangt Übereinstimmung**

- Vorteile: Doppelte Sicherung. Das Register wird nur aus dem Zertifikat befüllt und kann nicht abweichen, ohne dass der
  Gateway die Sitzung ablehnt. Löschen und Sperren wirken sofort über das Register und dauerhaft über die Sperrliste.
- Nachteile: Etwas mehr Prüfcode. Eine Bindung lässt sich nicht umhängen (gewollt: neue Bindung = neue Probe).

### Frage 2: Wer erzeugt Probe-Tokens mit Mandantenbindung?

**2A Nur `platform-admin` (heute)**

- Nachteile: Verfehlt FA-PRB-05 („Registrierung durch `tenant-admin`“).

**2B Zusätzlich `tenant-admin` über einen Mandanten-Endpunkt; Mandant immer aus dem Principal**

- Vorteile: Erfüllt FA-PRB-05. Der Mandant kann nicht manipuliert werden, weil er nie aus dem Body kommt. Das Limit wird
  im FSM durchgesetzt.
- Nachteile: Neue Endpunkte und ein neuer Schreibpfad für Tokens außerhalb der Admin-API.

**2C API-Token des Mandanten dürfen ebenfalls Probe-Tokens erzeugen**

- Vorteile: Automatisierbar (Infrastructure as Code).
- Nachteile: Ein Token-Leck erlaubt das Einschleusen von Probes. Token-Rechte (ADR 0031) sehen das nicht vor.

### Frage 3: Wie werden private Probe-Offline-Alerts zugestellt?

**3A Neuer Befehl `SetProbeAlert` mit Mandanten-Routing**

- Zustand in einer mandantenbezogenen Tabelle, Outbox-Einträge für Kanäle des Mandanten mit dem neuen Ereignis
  `probe_offline` (analog `channel_failed`, ADR 0023).
- Vorteile: Saubere Mandantengrenze. Die Daten werden mit dem Mandanten gelöscht (ADR 0029). Für den `tenant-admin` gibt
  es eine eigene Sicht, ohne dass der `_system`-Bereich geöffnet wird.
- Nachteile: Neuer Befehl, neue Tabelle, Befehlsversion.

**3B `SetSystemAlert` (Art `probe_offline`) routet je nach Owner an `_system` oder an Mandantenkanäle**

- Vorteile: Ein Befehl für beide Fälle.
- Nachteile: Mandantendaten liegen in der plattformweiten Tabelle `system_alerts`. Für Mandanten-UI und Löschen muss
  diese Tabelle nach Mandant gefiltert werden, und der `_system`-Schutz aus ADR 0025 wird aufgeweicht. Andere Bedeutung
  derselben Payload je Owner verletzt den Geist von ADR 0018.

**3C Private Probes nur in der UI anzeigen, ohne Benachrichtigung**

- Nachteile: Verfehlt FA-PRB-06.

### Frage 4: SSRF-Richtlinie privater Probes

**4A Lokal auf der Probe, Standard aus (ADR 0030 Abschnitt 5), Prüfung beim Speichern gelockert, wenn nur private Probes
des Mandanten zugewiesen sind**

- Vorteile: Erfüllt den eigentlichen Zweck (interne Ziele). Voter und öffentliche Probes behalten ihre Durchsetzung beim
  Verbindungsaufbau unabhängig davon.
- Nachteile: Die Prüfung beim Speichern hängt von der Zuweisung ab. Das ist mehr Validierungslogik.

**4B Lokal aus, aber keine Lockerung beim Speichern**

- Nachteile: IP-Literale in privaten Bereichen würden bei aktivem Schutz abgelehnt. Genau der Hauptfall (z.B.
  `http://10.0.0.5/`) ginge nicht.

## Entscheidung

**1C, 2B, 3A, 4A.**

### 1. Identität und Bindung

- **Maßgeblich ist das Zertifikat.** `Identity.TenantID` aus der URI-SAN, verifiziert beim Handshake (Kette, Ablauf,
  Sperrliste, Cluster-ID).
- **Register:** `RegisterProbe` (ADR 0034) setzt `owner_tenant_id` ausschließlich aus dem Zertifikat des Aufrufers bzw.
  beim Join aus dem eingelösten Token. Das FSM prüft:
    - Der Mandant existiert und ist nicht gelöscht.
    - Ein vorhandener Eintrag ändert seinen Owner nie (`CodeConflict`).
    - Die Rolle ist `probe`.
- **Gateway bei jedem Stream-Start:** Er lehnt ab (`PermissionDenied`, Warn-Log mit Node-ID und Mandant, Metrik
  `uptimecluster_probe_rejected_total{reason}`), wenn:
    - die Rolle ≠ `probe` ist,
    - die Node-ID widerrufen ist,
    - der Registereintrag fehlt (Level ≥ 10),
    - oder `register.owner_tenant_id ≠ cert.TenantID` gilt.

  Eine Probe des Mandanten `default` im lokalen bzw. generischen Modus ist eine gewöhnliche private Probe dieses
  Mandanten.
- **Öffentlich** ist eine Probe genau dann, wenn ihr Zertifikat keinen Mandanten trägt. Solche Tokens erzeugt nur der
  `platform-admin`.

### 2. Join-Token mit Mandantenbindung

- **Neue Mandanten-Endpunkte**, Rolle `tenant-admin` (Mandant immer aus dem Principal, nie aus Pfad oder Body):
    - `POST /api/v1/probes/join-tokens` `{ttlMinutes?}` → Token (einmalig angezeigt, `Cache-Control: no-store`, nie
      geloggt)
    - `GET /api/v1/probes` → private Probes des Mandanten (ID, Name, Region, Version, Gesundheit, letzter Kontakt,
      Pufferstand; **keine** Adressen anderer Nodes)
    - `DELETE /api/v1/probes/{id}` → `RevokeNode`, nur wenn `owner_tenant_id` = Mandant, sonst **404** (wie AS-06)
    - `GET /api/v1/nodes` → für die Zuweisung wählbare Nodes: Voter und öffentliche Probes (nur ID, Name, Region,
      Fähigkeiten) und die eigenen privaten Probes
- **API-Token** (ADR 0031) dürfen diese Endpunkte nicht nutzen (2C abgelehnt; offene Frage F2). Das FSM lässt
  `PutJoinToken` mit Mandant nur von `USER`-Akteuren zu.
- **Limit `maxPrivateProbes`** (ADR 0029, 2B: FSM verbindlich, API vorab):
    - Gezählt werden registrierte private Probes des Mandanten plus offene (unbenutzte, nicht abgelaufene) Tokens des
      Mandanten.
    - `PutJoinToken` mit Mandant lehnt bei Erreichen mit `CodeLimit` ab → **403** mit verständlicher Meldung.
    - `RegisterProbe` prüft das Limit nicht erneut, weil das Token schon gezählt wurde und eine bestehende Probe sonst
      bei Planänderung ausgesperrt würde. Eine Herabstufung des Plans entfernt keine Probes.
- **Audit** (ADR 0031): `probe.token.create`, `probe.revoke`, `probe.register`. Das Token selbst wird nie protokolliert.
- Der `platform-admin` behält `POST /api/v1/admin/join-tokens` (öffentliche und gebundene Probes, Voter).

### 3. Durchsetzung der Mandantengrenze

Jede Schicht prüft für sich. Keine verlässt sich auf eine andere.

| #  | Schicht                      | Regel                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
|----|------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| D1 | `scheduler.Assigner`         | Eine private Probe ist Kandidat **nur** für Monitore mit `TenantID = owner_tenant_id`. Sie ist kein Kandidat für `all`/`count` (siehe F1), nur für `nodes: [...]`. Öffentliche Probes und Voter gelten nach ADR 0034 Abschnitt 7.                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| D2 | API Monitor-Validierung      | `assignment.nodes` darf nur Voter, öffentliche Probes und eigene private Probes enthalten. Eine fremde private Probe ergibt **dieselbe** Meldung wie eine unbekannte („unknown node“), also kein Orakel über fremde IDs.                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| D3 | Gateway `Subscribe`          | Die Zuweisung wird aus dem lokalen Zustand für die Zertifikats-Identität berechnet und danach noch einmal gefiltert: Bei privater Probe wird jeder Monitor mit fremdem `TenantID` verworfen (Fehler-Log, Metrik). Gesperrte oder gelöschte Mandanten ergeben eine leere Zuweisung (vorhandener `TenantGate`).                                                                                                                                                                                                                                                                                                                                                             |
| D4 | Gateway `UploadHeartbeats`   | Je Heartbeat: `node_id` = Zertifikat. `tenant_id`/`monitor_id` müssen einen existierenden Monitor desselben Mandanten bezeichnen. Bei privater Probe gilt `tenant_id = cert.TenantID`. `ts_ms` liegt höchstens 5 min in der Zukunft (Serverzeit) und höchstens `PROBE_BUFFER_MAX_AGE` + 1 h in der Vergangenheit. Größen wie in der Replikation. Ein Verstoß gegen die Identitäts- oder Mandantenregel verwirft **den ganzen Batch** und beendet den Stream mit `PermissionDenied`. Heartbeats gelöschter Monitore werden still verworfen und zählen trotzdem zur Position, damit der Strom nicht hängen bleibt. `lost_below` wirkt nur auf den eigenen Strom (ADR 0034). |
| D5 | Gateway Rate-Limit           | Je Probe höchstens ein `Subscribe`- und ein Upload-Stream (eine neue Sitzung ersetzt die alte). Je Probe ein Token-Bucket für Heartbeats: Erlaubt sind das 2-Fache der erwarteten Rate aus der Zuweisung, mindestens 10/s, plus Burst für die Nachlieferung (Standard 2.000/s je Probe, global 20.000/s). Bei Überschreitung wird gedrosselt (Stream-Flusssteuerung), nicht verworfen.                                                                                                                                                                                                                                                                                    |
| D6 | Probe (Komfort, kein Schutz) | Die private Probe verwirft empfangene Monitore anderer Mandanten und protokolliert das.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| D7 | Konsens                      | Zählt nur Heartbeats zugewiesener Nodes (ADR 0021). Ein Heartbeat, der D4 übersteht, aber nicht zugewiesen ist, ändert keinen Status.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| D8 | Lebenszyklus                 | Mandant gesperrt → leere Zuweisung, Verbindung bleibt. Mandant gelöscht → `DeleteTenant` widerruft im selben FSM-Schritt alle privaten Probes des Mandanten (Register weg, `revoked_nodes`), und offene Tokens werden gelöscht (ADR 0029 nennt die Tokens schon). Laufende Sitzungen schließt der Gateway beim Anwenden.                                                                                                                                                                                                                                                                                                                                                  |
| D9 | Sichtbarkeit                 | Mandanten-API und WebSocket zeigen fremde private Probes nie, auch nicht als Melder eines Monitors (es gibt keine, siehe D1). Die Admin-API zeigt alle Probes mit Owner.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |

**Geheimnisse:** Eine private Probe erhält die aufgelöste Konfiguration (Klartext-Geheimnisse, ADR 0015) nur für
Monitore ihres Mandanten, und nur, solange sie ihnen zugewiesen ist. Es sind die eigenen Geheimnisse des Mandanten.
DEKs, KEK und Daten anderer Mandanten erreichen eine Probe nie. `JoinResponse.kek` bleibt für Probes leer (heute schon
so).

### 4. SSRF bei privaten Probes (ADR 0030 Abschnitt 5)

- **Private Probe:** nutzt die lokale Variable `SSRF_PROTECTION`. Für `NODE_ROLE=probe` mit Mandanten-Zertifikat gilt
  `auto` = **aus**. `ProbeMonitor.ssrf_protection` wird ignoriert, der Kunde entscheidet in seinem Netz.
- **Öffentliche Probe:** Geschützt wird, wenn `ProbeMonitor.ssrf_protection` **oder** die lokale Richtlinie es verlangt.
  Keine Seite kann den Schutz lockern. Der Voter setzt das Feld nach seiner Richtlinie.
- **Prüfung beim Speichern:** IP-Literale in gesperrten Bereichen sind bei aktivem Schutz erlaubt, **wenn**
  `assignment.mode = nodes` gilt und **alle** genannten Nodes private Probes des Mandanten sind. Sonst gilt die
  bisherige Ablehnung. Ändert sich die Zuweisung später, wird erneut geprüft (gleiche Validierung bei `PUT`).
- Voter prüfen zugewiesene Monitore weiter mit ihrer Richtlinie beim Verbindungsaufbau (sie bekommen solche Monitore
  wegen D1/D2 aber gar nicht zugewiesen).
- Benachrichtigungen gehen weiter nur von Votern aus (FA-ALR-12). Eine private Probe versendet nichts.

### 5. Probe-Offline-Alerts (FA-PRB-06)

- **Grundlage:** die Probe-Gesundheit im Register (ADR 0034 Abschnitt 7). Ein Alert entsteht, wenn eine Probe seit **
  `PROBE_OFFLINE_ALERT` (Standard 5 min)** ungesund ist. Das unterdrückt Wechsel durch `MaxConnectionAge`, Voter-Updates
  und kurze Ausfälle. Er endet nach 2 Bewertungen ohne Bedingung, mit Entwarnung (wie ADR 0025).
- **Öffentliche Probe → Systemalert:** neue Art `probe_offline` (Betreff: Node-ID) im vorhandenen `sysalert.Collector`.
  Das ist ohne neuen Befehl möglich (`SetSystemAlert`). Der Collector bewertet bisher nur Voter. Er nimmt öffentliche
  Probes für genau diese Art auf. Folgearten für Probes (Zertifikatsablauf aus `cert_not_after_ms`, Inkarnationswechsel,
  Uhrversatz aus `wall_clock_unix_ms`, Puffer-Verluste aus `dropped_total`) laufen über die Felder aus ADR 0034
  Abschnitt 4, ebenfalls als Systemalerts. Bei privaten Probes sieht sie nur der `platform-admin` in der Admin-UI, ohne
  Benachrichtigung (siehe F4).
- **Private Probe → Mandanten-Alert (3A):**
    - Neuer Befehl `SetProbeAlert{probe_id, active}` mit `Command.tenant_id` = Owner, Mindestversion **10**, nur vom
      Leader und nur bei Wechseln.
    - Das FSM prüft, dass der Owner der Probe dem Mandanten entspricht.
    - Tabelle `probe_alerts(tenant_id, probe_id, active, since_ms, updated_ms)`, Primärschlüssel
      `(tenant_id, probe_id)`. Sie ist Teil des Fingerprints und wird mit dem Mandanten gelöscht.
    - Outbox-Einträge für aktive Kanäle des Mandanten, die das neue Ereignis **`probe_offline`** abonnieren.
      `alert_key = probe:<probe_id>:<since>:<active|clear>`. Nutzlast: Probe-Name und -ID, seit wann, Absender-Sicht
      (ADR 0024). Keine Adressen, keine Fehlertexte.
    - Drosselung und Bündelung wie ADR 0023. Für neue Kanäle ist `probe_offline` ab Level 10 in den Standard-Ereignissen
      enthalten (wie `channel_failed`).
- **Ohne Quorum** entstehen keine neuen Probe-Alerts (Bewertung nur auf dem Leader). Probe-Alerts sind keine
  Monitor-Alerts und fallen nicht unter den Degraded-Versand (ADR 0024).
- Entfernte Probes (`RevokeNode`) beenden einen offenen Alert still (Entwarnung ohne Benachrichtigung).

### 6. UI (FA-UI-04/05)

- Mandant „Private Probes“: Liste, Token erzeugen (mit Compose-Schnipsel), entfernen, Gesundheit und Pufferstand.
- Monitor-Editor: Auswahl für `nodes` aus `GET /api/v1/nodes`. Hinweis bei internen Zielen: „nur mit privaten Probes“.
- Kanal-Editor: Ereignis `probe_offline`.
- Admin: Probes in der Cluster-Ansicht mit Owner.
- Systemkanäle: Art `probe_offline`.

### 7. Tests

- **Isolation (Erweiterung AS-06-Treiber und Tabellentests):**
    - Mandant A listet, löscht und referenziert private Probes von B → 404 bzw. „unknown node“.
    - Probe von A bekommt im Snapshot nie Monitore von B, auch nach `nodes`-Manipulation im Raft-Zustand (FSM-Fixture),
      denn D3 greift unabhängig von D1.
    - Upload mit fremder `tenant_id`, fremder `node_id` oder fremdem `monitor_id` → `PermissionDenied`, nichts
      gespeichert, Position unverändert.
    - `lost_below` einer Probe ändert keinen fremden Strom.
- **Zertifikat:**
    - Ein Zertifikat ohne bzw. mit anderem Mandanten als im Register wird abgelehnt.
    - Nach `RevokeNode` scheitert der Handshake, und laufende Sitzungen enden.
    - Nach `DeleteTenant` sind alle Probes des Mandanten widerrufen.
- **Limit:** Token N+1 → 403. Gleichzeitige Token-Anfragen über zwei Voter → genau eines scheitert (FSM).
- **SSRF:** private Probe prüft `10.0.0.5` (lokale Richtlinie aus). Öffentliche Probe mit `ssrf_protection=true`
  blockiert trotz lokal `off`. Speichern mit IP-Literal: erlaubt nur bei `nodes` = ausschließlich eigene private Probes.
- **Alerts:**
    - Public offline → Systemkanal.
    - Private offline → Kanal des Mandanten mit `probe_offline`, nicht an andere Mandanten und nicht an `_system`.
    - Ein Wechsel durch `MaxConnectionAge` erzeugt keinen Alert.
    - Entwarnung nach Rückkehr.
    - Determinismus-Test für `SetProbeAlert`/`RegisterProbe`.

## Konsequenzen

- **Positiv:**
    - Die Mandantengrenze ist an neun Stellen unabhängig gesichert. Zertifikat und Register können nicht unbemerkt
      auseinanderlaufen.
    - Der Hauptanwendungsfall „interne Ziele eines Kunden“ funktioniert trotz SSRF-Schutz der Plattform.
    - Probe-Alerts folgen der vorhandenen Outbox-, Drosselungs- und Kanal-Logik.
- **Negativ / Risiken:**
    - Mehr Validierungscode im Gateway und in der Monitor-API, alles sicherheitsrelevant (Hauptagent).
    - Neue Befehle (`RegisterProbe`, `SetProbeAlert`), neue Tabelle und Spalte: Level 10 und Feature-Gate.
    - Eine kompromittierte private Probe kann für die Monitore ihres Mandanten falsche Ergebnisse liefern. Das liegt in
      der Verantwortung des Mandanten. Für andere Mandanten bleibt es ohne Wirkung.
    - Die Lockerung der Speicherprüfung koppelt Validierung an die Zuweisung. Ein Fehler dort könnte interne Ziele auf
      Votern zulassen. Abgefedert ist das durch die unveränderte Dial-Zeit-Prüfung auf Votern.
- **Folgearbeiten:**
    - OpenAPI: `/probes`, `/probes/join-tokens`, `/probes/{id}`, `/nodes`, Ereignis `probe_offline`, Systemalert-Art
      `probe_offline`.
    - Pflichtenheft: FA-PRB-05 (API-Pfade, Limitzählung inkl. offener Tokens), FA-PRB-06 (Schwelle 5 min, Ereignis
      `probe_offline`), FA-SEC-SSRF (Speicherprüfung bei privaten Probes).
    - ADR 0029: Zählweise von `maxPrivateProbes`. ADR 0030: Abschnitt 5 umgesetzt.
    - Betriebsdoku „Private Probe im Kundennetz“.

### Offene Fragen an Projektinhaber

- **F1 – Private Probes in `all`/`count`:** Vorschlag: nur über `nodes: [...]` (später Labels). Sonst würde eine neue
  private Probe alle `all`-Monitore des Mandanten mitprüfen und deren Konsens verändern, auch öffentliche Ziele.
  Alternative: private Probes nehmen an `all` und `count` ihres Mandanten teil.
- **F2 – API-Token:** Sollen Mandanten private Probes per API-Token (mit neuem Recht `probes:write`) registrieren dürfen
  (Automatisierung)? Vorschlag: nicht in AP-5.2.
- **F3 – Limitzählung:** Offene Tokens mitzählen (Vorschlag, verhindert Token-Vorrat über dem Limit) oder nur
  registrierte Probes?
- **F4 – Plattform bei privaten Probes:** Soll der `platform-admin` zusätzlich einen Systemalert erhalten, wenn eine
  private Probe offline geht? Vorschlag: nein, nur Anzeige. Das ist Sache des Kunden und erzeugt sonst Alarmlärm.
- **F5 – Bestehende Kanäle:** Sollen bestehende Mandantenkanäle `probe_offline` automatisch abonnieren (Migration als
  Raft-Befehl) oder nur neue Kanäle? Vorschlag: nur neue, plus UI-Hinweis.
- **F6 – Schwelle `PROBE_OFFLINE_ALERT` = 5 min:** einverstanden, oder je Mandant einstellbar?

## Approval

| Datum      | Status   | Durch  | Anmerkung                                |
|------------|----------|--------|------------------------------------------|
| 2026-11-19 | Proposed | Claude | Vorbereitung AP-5.2; offene Fragen F1–F6 |
| 2026-11-20 | Accepted | Projektinhaber | im Chat; offene Fragen wie vorgeschlagen |
