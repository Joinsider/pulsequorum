# ADR 0029: Mandantenverwaltung, Pläne, Limits und Auto-Provisionierung

| | |
| --- | --- |
| **Status** | Accepted |
| **Datum** | 2026-11-17 |
| **Entscheider** | Projektinhaber |
| **Bezug** | FA-TEN-01..05, NFA-SEC-02, AS-06, Konzept Kap. 12.5 und E21, ADR 0004, 0011, 0016, 0018, 0025, 0027 |

## Kontext

Mit AP-4.1 (ADR 0027) gibt es mehrere Mandanten im Profil `zitadel` mit `OIDC_TENANCY=multi`. Mandanten lassen sich aber noch nicht verwalten:

- **FA-TEN-03:** Ein `platform-admin` legt Mandanten an, sperrt und löscht sie (mit Datenlöschung) und setzt Plan und Limits. Beim ersten Login einer neuen Zitadel-Organisation wird der Mandant automatisch angelegt. Das ist abschaltbar und gilt nur für `zitadel` mit `multi`.
- **FA-TEN-04:** Limits je Plan für Monitore, kürzestes Intervall, Statusseiten, private Probes und Kanäle. Eine Überschreitung ergibt `403` mit verständlicher Meldung.

Was es heute gibt:

- Tabelle `tenants` mit `plan`, `limits_json` und `suspended` seit der ersten Migration. Ausgewertet wird davon nur `suspended` (OIDC-Verifier und WebSocket).
- Mandanten entstehen nur über `PutEntity` (Bootstrap von `default` und `_system`). `PutEntity` prüft die Org-ID nicht wie `BindTenantOrg` und überschreibt bei jedem Aufruf `created_ms`.
- `Zitadel.Map` antwortet bei unbekannter Organisation `403` „organization is not provisioned“. `Map` läuft auf jedem Node im Anfragepfad. Schreibbefehle eines Followers leitet `Node.Apply` an den Leader weiter.
- `state.db` wird über Raft repliziert. Fast alle mandantenbezogenen Tabellen hängen per `ON DELETE CASCADE` an `tenants`. `metrics.db` ist knotenlokal (ADR 0016) und hat keine Löschung je Mandant. Daten gelöschter Monitore altern dort heute über die Aufbewahrung aus.
- Statusseiten und private Probes gibt es noch nicht (P5/P6).
- Raft behält nach einem Snapshot `TrailingLogs` = 20000 Einträge.

Randbedingungen: `Apply` ist deterministisch (keine Uhr, kein Zufall, kein I/O außer `state.db`). Neue Befehle brauchen eine Befehlsversion mit Feature-Gate (ADR 0018). Die Mandanten `_system` und `default` dürfen nie gelöscht, gesperrt oder umgebunden werden.

## Optionen

### Frage 1: Wo liegen die Pläne?

- **1A: im Raft-Zustand.** Pläne sind Entitäten. Ein `platform-admin` pflegt sie über API und UI.
  - Vorteile: Auf allen Nodes gleich, damit darf das FSM Limits prüfen. Änderungen ohne Release.
  - Nachteile: Neue Tabelle, neue Befehle, Admin-UI für Pläne.
- **1B: fest im Code.** Wenige feste Pläne, dazu Overrides je Mandant.
  - Vorteile: Am einfachsten.
  - Nachteile: Ein neuer Plan braucht ein Release. Bei gemischten Versionen sehen Nodes unterschiedliche Pläne.
- **1C: Konfigurationsdatei je Node.**
  - Vorteile: Flexibel ohne UI.
  - Nachteile: Nodes können auseinanderlaufen. Das FSM darf sich darauf nicht stützen, also nur API-Prüfung.

### Frage 2: Wo werden Limits durchgesetzt?

- **2A: nur in der API** vor dem Schreiben.
  - Vorteile: Einfach, beste Fehlermeldungen.
  - Nachteile: Parallele Anfragen über mehrere Nodes überschreiten das Limit (Zählen und Schreiben sind getrennt).
- **2B: im FSM**, zusätzlich eine Vorprüfung in der API.
  - Vorteile: Verbindlich auch bei parallelen Anfragen, Klonen und Massenaktionen. Deterministisch, weil Pläne im Raft-Zustand liegen (1A).
  - Nachteile: Zählen im `Apply` kostet eine Abfrage je Anlage.

### Frage 3: Was passiert bei einem kleineren Plan, wenn der Bestand schon darüber liegt?

- **3A:** Bestand bleibt, Neues wird abgelehnt.
- **3B:** Überzählige Monitore automatisch pausieren.
- **3C:** Plan-Wechsel ablehnen, bis der Bestand passt.

### Frage 4: Auto-Provisionierung standardmäßig an oder aus?

- **4A: aus**, Betreiber schaltet bewusst ein.
- **4B: an**, abschaltbar.

### Frage 5: Löschen in einem oder mehreren Schritten?

- **5A: ein FSM-Befehl** löscht alles in `state.db`. Jeder Node räumt `metrics.db` danach selbst auf.
- **5B: FSM-Befehle in Stapeln** (Monitor für Monitor), Mandant zum Schluss.
  - Nachteile: Zwischenzustände über viele Raft-Einträge, Wiederanlauf über mehrere Befehle.

## Entscheidung

Im Chat vorgegeben: **1A, 3A, 4A**. Vorgeschlagen: **2B, 5A**. Dazu die folgenden Regeln.

### Datenmodell

**Neue Tabelle `plans`** in `state.db` (Migration):

```
plans(id TEXT PRIMARY KEY, name TEXT NOT NULL, limits_json TEXT NOT NULL,
      created_ms INTEGER NOT NULL, updated_ms INTEGER NOT NULL)
```

**Limits** (`limits_json`), alle optional. Fehlt ein Feld oder ist es `null`, gilt es als unbegrenzt.

| Feld | Bedeutung | Durchsetzung ab |
| ------ | ----------- | ----------------- |
| `maxMonitors` | Anzahl Monitore | AP-4.2 |
| `minIntervalSeconds` | kürzestes Prüfintervall | AP-4.2 |
| `maxChannels` | Anzahl Benachrichtigungskanäle | AP-4.2 |
| `maxStatusPages` | Anzahl Statusseiten | mit dem Feature (P5/P6) |
| `maxPrivateProbes` | Anzahl privater Probes | mit dem Feature (P5) |

- Werte sind ganze Zahlen ≥ 0 (`minIntervalSeconds` ≥ 1). Unbekannte Felder werden abgelehnt, damit Tippfehler nicht still „unbegrenzt“ bedeuten.
- **Mandanten:** `tenants.plan` verweist auf `plans.id`. `tenants.limits_json` enthält Overrides je Mandant mit denselben Feldern.
- **Wirksame Limits:** feldweise Override vor Plan. Ein Override `null` hebt das Limit für den Mandanten auf.
- **Mitgelieferte Pläne:** Die Migration legt `default` (ohne Limits) und `system` (für `_system`) an. Beide sind nicht löschbar, `system` ist auch nicht änderbar und keinem anderen Mandanten zuweisbar.
- **Neue Spalten in `tenants`:** `source` (`admin` | `auto` | `bootstrap`) für die Höchstzahl der Auto-Provisionierung.

**Neue Tabelle `deleted_tenants`** (Grabsteine):

```
deleted_tenants(id TEXT PRIMARY KEY, deleted_ms INTEGER NOT NULL)
```

Eine gelöschte Mandanten-ID wird nie wieder vergeben. Die Org-Bindung wird beim Löschen frei.

### FSM-Befehle (Befehlsversion 8)

Neue Konstante `TenantAdminCommandLevel = 8`. `CommandVersion` steigt auf 8. Jeder neue Befehl läuft erst, wenn alle Voter Version 8 können (ADR 0018).

| Befehl | Inhalt | Regeln |
| -------- | -------- | -------- |
| `CreateTenant` | `tenant_id`, `name`, `plan_id`, `org_id?`, `limits?`, `source` | ID neu und nicht in `deleted_tenants`; Name 1–100 Zeichen ohne Steuerzeichen; Plan existiert und ist nicht `system`; Org-ID nach `validOrgID` und frei; bei `source=auto` siehe unten |
| `UpdateTenant` | `tenant_id`, Felder als optionale Änderungen: `name`, `plan_id`, `limits`, `org_id` | nicht `_system`; für `default` nur `name`, `plan_id`, `limits`; Org-ID wie oben; Umbinden ist hier erlaubt (offener Punkt aus ADR 0027) |
| `SetTenantSuspended` | `tenant_id`, `suspended` | nicht `_system`, nicht `default` |
| `DeleteTenant` | `tenant_id` | nicht `_system`, nicht `default`; Mandant muss gesperrt sein |
| `PutPlan` | `plan_id`, `name`, `limits` | nicht `system`; Limits validiert |
| `DeletePlan` | `plan_id` | nicht `default`, nicht `system`; kein Mandant nutzt ihn |

- Zeitpunkte kommen aus `issued_at_ms` des Befehls. `UpdateTenant` lässt `created_ms` unverändert.
- **`PutEntity` für Mandanten** bleibt nur für den Bootstrap mit `Actor_KIND_SYSTEM` und prüft die Org-ID mit denselben Regeln wie `BindTenantOrg` (gültig, eindeutig, nie für `_system`). Für andere Akteure wird es abgelehnt. `created_ms` eines bestehenden Mandanten bleibt erhalten.
- Die Prüfregeln für Org-IDs liegen in einer gemeinsamen Funktion, die `BindTenantOrg`, `CreateTenant`, `UpdateTenant` und `PutEntity` aufrufen.

### Autorisierung

- Alle Befehle oben außer der Auto-Provisionierung löst nur ein `platform-admin` über `/api/v1/admin/tenants` und `/api/v1/admin/plans` aus. Die Rolle prüft die API, denn `pb.Actor` trägt keine Rollen. Das FSM lässt diese Befehle nur von Akteuren der Art `USER` oder `SYSTEM` zu, nie von API-Tokens (bis AP-4.4 Token-Rechte festlegt).
- `tenant-admin` und darunter sehen über `GET /api/v1/tenant` nur den eigenen Mandanten mit wirksamen Limits und Nutzung, ohne Org-ID und ohne fremde Pläne.
- Der `platform-admin` handelt in der Admin-API mandantenübergreifend über die Pfad-ID. Das ist die einzige Stelle, an der eine Mandanten-ID aus der URL kommt (FA-TEN-02 betrifft Mandantendaten, nicht die Plattformverwaltung). Monitore, Kanäle und andere Daten fremder Mandanten bleiben auch für ihn unsichtbar.

### Sperren

- Gesperrter Mandant: Anmeldung und API antworten `403` „tenant is suspended“, in beiden Auth-Modi. Lokale Refresh-Sitzungen des Mandanten werden beim Sperren widerrufen. Offene WebSocket-Verbindungen schließt die bestehende Revalidierung.
- Checks und Alerts des Mandanten ruhen, solange er gesperrt ist. Der Scheduler überspringt seine Monitore, die Alert-Engine verwirft seine Ausgänge. Daten bleiben erhalten.
- Entsperren stellt alles wieder her, Monitore starten im Zustand `pending`.

### Löschen mit Datenlöschung (5A)

Reihenfolge: **sperren → löschen → aufräumen.**

1. **Sperren** mit `SetTenantSuspended` (eigener Schritt, damit nichts im Flug ist und ein Fehlklick folgenlos bleibt).
2. **`DELETE /admin/tenants/{id}`** verlangt im Body den Mandantennamen zur Bestätigung (`confirmName`). Danach `DeleteTenant` in **einem** `Apply`, also in einer SQLite-Transaktion:
   - alle Zeilen des Mandanten in `state.db` löschen (über `ON DELETE CASCADE` und ausdrücklich für Tabellen ohne Fremdschlüssel, z.B. Probe-Tokens in `join_tokens`, `alert_outbox`, Status-Historie);
   - den Mandanten aus `tenants` entfernen (die Org-Bindung wird damit frei);
   - `deleted_tenants` eintragen.
   - Ein Abbruch mitten im `Apply` hinterlässt dank Transaktion nichts Halbes. Nach einem Neustart wird der Befehl aus dem Log erneut angewandt.
3. **Aufräumen von `metrics.db` auf jedem Node:** Ein Job findet Grabsteine, die lokal noch nicht erledigt sind (lokale Tabelle `purged_tenants` in `metrics.db`), und löscht Serien, Heartbeats, Minuten-Latenzen, Rollups und Degraded-Einträge des Mandanten in Stapeln. Er läuft nach einem Neustart weiter, wo er war. Der Ingest verwirft Heartbeats für Mandanten mit Grabstein.
4. **Im Speicher:** Scheduler, Alert-Engine und Stream verwerfen alles zum Mandanten, sobald der Befehl angewandt ist.
5. **Geheimnisse:** Geheimnisse liegen verschlüsselt in den Zeilen von Monitoren und Kanälen (ADR 0011, 0015) und verschwinden mit ihnen. Die DEKs sind nicht mandantenbezogen und bleiben.
6. **Grenze:** Raft-Log und ältere Snapshots enthalten die gelöschten Konfigurationen noch, bis sie überschrieben sind (bis zu 20000 Einträge nach dem nächsten Snapshot), Backups bis zu ihrer Aufbewahrung. Nach `DeleteTenant` stößt jeder Node einen Snapshot an. Die Betriebsdoku nennt diese Grenze ausdrücklich.

### Auto-Provisionierung (4A)

- **Nur** mit `OIDC_PROFILE=zitadel`, `OIDC_TENANCY=multi` und `OIDC_AUTO_PROVISION=true`. Standard ist `false`.
- **Wann:** `Zitadel.Map` findet für eine Organisation keinen Mandanten. Das Token ist zu diesem Zeitpunkt vollständig geprüft (Signatur, Issuer, Audience, `azp`, Laufzeit).
- **Voraussetzung:** Das Token trägt mindestens eine gültige Projektrolle **in dieser Organisation**. Rollen in einer Organisation entstehen nur, wenn der Zitadel-Betreiber dem Projekt eine Berechtigung für sie gibt (Project Grant). Damit legt eine Selbstregistrierung in Zitadel allein noch keinen Mandanten an. `platform-admin` aus einer Kundenorganisation bleibt verworfen (ADR 0027).
- **Ablauf:** Der Node erzeugt eine neue Mandanten-ID (UUID) und sendet `CreateTenant{source=auto, plan_id=OIDC_AUTO_PROVISION_PLAN}` über `Node.Apply` (Follower leiten an den Leader weiter). Der Name kommt aus dem Claim `urn:zitadel:iam:user:resourceowner:name`, bereinigt und gekürzt, sonst die Org-ID.
- **Idempotenz:** Request-ID `provision-org:<org_id>`. Ist die Organisation beim `Apply` schon gebunden, liefert das FSM den vorhandenen Mandanten als Ergebnis statt eines Fehlers. Parallele erste Logins über mehrere Nodes enden so beim selben Mandanten.
- **Missbrauchsschutz:**
  - Höchstzahl `OIDC_AUTO_PROVISION_MAX` (Standard 100) automatisch angelegter Mandanten, im FSM über `source=auto` gezählt und damit clusterweit verbindlich.
  - Rate-Limit je Node (Standard 10 Anlagen je Minute) und eine Negativ-Sperre je Organisation für 60 s nach einer Ablehnung.
  - Bei Ablehnung bleibt die Antwort `403` „organization is not provisioned“. Das Log nennt den Grund.
- `OIDC_AUTO_PROVISION_PLAN` (Standard `default`) muss beim Start existieren, sonst schlägt die Anlage mit klarer Meldung fehl.

### Limits durchsetzen (2B, 3A)

- **Im FSM** bei `PutEntity` für Monitore und Kanäle:
  - Anlage: Anzahl der Monitore bzw. Kanäle des Mandanten muss unter `maxMonitors` bzw. `maxChannels` liegen.
  - Anlage und Änderung: Intervall ≥ `minIntervalSeconds`. Eine Änderung, die das Intervall unverändert lässt, ist erlaubt, auch wenn es nach einem Plan-Wechsel unter dem Minimum liegt (3A).
  - Fehler: neuer Code `CodeLimitExceeded`, die API antwortet `403` als Problem-Detail mit Limit, Wert und Plan, z.B. „monitor limit reached: plan 'starter' allows 10 monitors“.
- **In der API** dieselbe Prüfung vorab, damit Formulare Feldfehler zeigen. Das FSM bleibt verbindlich.
- **Klonen, Import und Massenaktionen** laufen über dieselben Befehle und damit dieselbe Prüfung.
- **Kleinerer Plan (3A):** Bestand bleibt und läuft weiter. Neues wird abgelehnt. `GET /tenant` und die Admin-Ansicht zeigen Nutzung und Überschreitung.
- `_system` ist von Limits ausgenommen.

### API (OpenAPI, rückwärtskompatibel ergänzt)

- `GET/POST /admin/plans`, `GET/PUT/DELETE /admin/plans/{planId}`
- `GET/POST /admin/tenants`, `GET/PATCH/DELETE /admin/tenants/{tenantId}`, `POST /admin/tenants/{tenantId}/suspend`, `POST /admin/tenants/{tenantId}/resume`
- `GET /tenant` für den eigenen Mandanten
- Antworten enthalten Plan, Overrides, wirksame Limits und Nutzung (Anzahl Monitore, Kanäle, kürzestes Intervall).

## Konsequenzen

- **Positiv:**
  - FA-TEN-03 und FA-TEN-04 sind erfüllt. Limits gelten clusterweit verbindlich, auch bei parallelen Anfragen.
  - Löschen ist atomar in `state.db` und wiederanlauffähig in `metrics.db`.
  - Auto-Provisionierung ist standardmäßig aus und hängt zusätzlich an einer Projektberechtigung in Zitadel.
  - Der offene Punkt aus AP-4.1 (`PutEntity` prüft die Org-ID nicht) ist geschlossen.
- **Negativ / Risiken:**
  - Gelöschte Daten bleiben im Raft-Log und in Backups, bis sie überschrieben sind. Das wird dokumentiert, nicht technisch verhindert.
  - Ein großer Mandant erzeugt beim Löschen eine lange Transaktion auf jedem Voter. Für die Zielgröße (ADR 0004) ist das tragbar. Sollte es nicht reichen, kommt ein neuer ADR für Option 5B.
  - Limits für Statusseiten und private Probes sind gespeichert, aber erst mit den Features durchgesetzt.
  - Zählen im `Apply` kostet je Anlage eine Abfrage.
- **Folgearbeiten:**
  - Migration (`plans`, `deleted_tenants`, `tenants.source`), Store-Methoden mit Isolationstests, `metrics.db`-Tabelle `purged_tenants` und Lösch-Job.
  - FSM-Befehle mit Tabellentests, Feature-Gate und Determinismus-Test.
  - Admin-API, `GET /tenant`, Admin-UI „Mandanten & Pläne“, Anzeige der Limits für Mandanten.
  - Auto-Provisionierung im Profil `zitadel`, Ende zu Ende gegen `deploy/oidc-dev/zitadel` (Org `acme`).
  - Test, dass nach `DeleteTenant` in keiner Tabelle von `state.db` und `metrics.db` eine Zeile des Mandanten bleibt.
  - Isolationstest mit zwei Mandanten als Vorbereitung für AS-06.
  - Betriebsdoku: Sperren, Löschen mit seinen Grenzen, Auto-Provisionierung, Konfiguration.
  - Konfiguration `OIDC_AUTO_PROVISION`, `OIDC_AUTO_PROVISION_PLAN` und `OIDC_AUTO_PROVISION_MAX` in `docs/betrieb/oidc.md` beschreiben (die OIDC-Variablen stehen dort, nicht in der Konfigurationstabelle des Pflichtenhefts).

## Approval

| Datum | Status | Durch | Anmerkung |
|-------|--------|-------|-----------|
| 2026-11-17 | Proposed | Claude | Richtung 1A, 3A, 4A im Chat vorgegeben; 2B und 5A vorgeschlagen |
| 2026-11-17 | Accepted | Projektinhaber | im Chat angenommen („Ja ADR 29 annehmen“), eingetragen durch Claude |
