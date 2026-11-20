# ADR 0031: API-Tokens, Audit-Log und Massenaktionen

|                 |                                                                                                                                                                |
|-----------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **Status**      | Accepted                                                                                                                                                       |
| **Datum**       | 2026-11-18                                                                                                                                                     |
| **Entscheider** | Projektinhaber                                                                                                                                                       |
| **Bezug**       | FA-AUTH-10, FA-DAT-01, FA-MON-06, FA-MON-07, FA-TEN-02, FA-CLU-09; ADR 0008 (lokale Auth), ADR 0018 (Befehlsversionen), ADR 0019 (Recovery-CLI), ADR 0027–0029 |

## Kontext

AP-4.4 umfasst drei Anforderungen:

- **FA-AUTH-10:** API-Tokens, langlebig, an Mandant und Rolle gebunden, widerrufbar, nur gehasht gespeichert. Endpunkt
  `CRUD /api/v1/api-tokens`.
- **FA-DAT-01:** Alle schreibenden Aktionen kommen ins Audit-Log (Datenklasse A): wer, wann, Mandant, Objekt, Aktion,
  ohne Geheimnisse. Einsehbar für `tenant-admin` und `platform-admin`, Endpunkt `GET /api/v1/audit`, Aufbewahrung 1
  Jahr, konfigurierbar (Pflichtenheft 7.3).
- **FA-MON-06:** Massenaktionen über die API: pausieren, fortsetzen, löschen, Tags.

Was es schon gibt:

- `pb.Actor` mit den Arten `USER`, `API_TOKEN` und `SYSTEM` steht in jedem Befehl. Seit P1 ist er „für das Audit-Log“
  vorgesehen, ausgewertet wird er bisher nicht.
- `auth.NewOpaqueToken` liefert 256 Zufallsbits, `auth.HashToken` den SHA-256 dazu (ADR 0008, für Refresh- und
  Setup-Tokens).
- Die Middleware nimmt Bearer-Tokens an: im lokalen Modus eigene JWTs, im OIDC-Modus JWTs des IdP. Opake Tokens lehnt
  sie mit `ErrOpaqueToken` ab.
- ADR 0029 lässt Befehle der Mandantenverwaltung nur von `USER` und `SYSTEM` zu, nie von API-Tokens, „bis AP-4.4
  Token-Rechte festlegt“.
- Monitore sind JSON-Dokumente (`PutEntity`/`DeleteEntity` mit `expected_revision`) und haben `tags` (höchstens 20).
- Das FSM ist deterministisch. Neues Verhalten braucht eine Befehlsstufe (ADR 0018). Aktuell gilt `CommandVersion` 8.

Die Richtungsfragen wurden am 2026-11-18 im Chat entschieden: **1A, 2A, 3C, 4A**. Für Frage 2 hat Projektinhaber ein Ziel
genannt: Später soll ein eigenes Frontend oder Backend Mandanten automatisch anlegen, für Unternehmen wie für
Einzelpersonen. Das deckt ein eigener ADR ab. Dieser ADR hält das Modell nur dafür offen.

## Optionen

### Frage 1: Woran hängt ein Token?

**1A Dienst-Token des Mandanten.** Das Token gehört dem Mandanten, nicht einer Person. Ein `tenant-admin` legt es mit
einer festen Rolle an. Es bleibt gültig, wenn der Ersteller den Mandanten verlässt. ← gewählt

- Vorteile: passt zu CI und Automatisierung. Funktioniert in allen Auth-Modi gleich, auch mit OIDC, wo Nutzerrollen nur
  im IdP liegen und Pulsequorum sie zwischen zwei Logins nicht kennt.
- Nachteile: Ein Token überlebt seinen Ersteller. Dagegen helfen der Pflicht-Ablauf (Frage 3) und die Token-Liste mit
  Ersteller.

**1B Persönliches Token.** Gehört einem Nutzer, hat höchstens dessen Rolle und erlischt mit ihm.

- Nachteile: Im OIDC-Modus lässt sich nicht prüfen, ob der Nutzer seine Rolle noch hat. Das Token wäre dort entweder
  unsicher oder nutzlos.

**1C Beides.** Doppelte Regeln, Tests und Oberfläche für wenig Gewinn.

### Frage 2: Plattform-Rechte für Tokens

**2A Nur Mandantenrollen, Provisionierung später.** ← gewählt

- Tokens tragen `tenant-viewer`, `tenant-editor` oder `tenant-admin`, nie `platform-admin`.
- Das Datenmodell bekommt ein Feld `scope` mit dem einzigen Wert `tenant`. So kann ein späterer ADR einen engen
  Geltungsbereich `provisioning` ergänzen, ohne Migration: Mandanten anlegen, Plan setzen, sperren, aber kein Zugriff
  auf Cluster, Join-Tokens oder die CA.
- Bis dahin deckt das Zitadel-Profil Firmen und Einzelpersonen ab. Ein Backend legt über die Zitadel-API eine
  Organisation mit Nutzer an. Pulsequorum legt den Mandanten beim ersten Login an (Auto-Provisionierung, ADR 0029).
  Ein Mandant mit nur einem Nutzer ist dabei kein Sonderfall.
- Vorteile: Ein geleaktes Token gefährdet höchstens einen Mandanten.
- Nachteile: Automatisierung auf Plattform-Ebene geht vorerst nur über Zitadel oder interaktive Logins.

**2B platform-admin-Tokens:** Bei einem Leck ist die ganze Plattform betroffen. **2C Provisionierungs-Token jetzt:**
sinnvoll, aber ohne konkretes Backend noch nicht zu bemessen.

### Frage 3: Ablauf und Nutzung

**3A** Pflicht-Ablauf, „zuletzt benutzt“ grob per Raft. **3B** Ablauf optional. **3C Pflicht-Ablauf, ohne
Nutzungszeit.** ← gewählt

- Vorteile von 3C: Tokens verursachen keine zusätzlichen Raft-Schreibvorgänge. Jedes Token läuft nach höchstens einem
  Jahr ab.
- Nachteile: Ungenutzte Tokens fallen nur über Ablaufdatum und Ersteller auf.

### Frage 4: Umfang des Audit-Logs

**4A Aktionen von Nutzern und Tokens, dazu administrative Systembefehle, ohne Inhalte.** ← gewählt

- Das FSM schreibt den Eintrag im selben `Apply` wie die Änderung. Damit ist er auf allen Votern gleich und fehlt nie.
- Vorteile: lückenlos für alles, was Menschen und Automatisierung tun. Das Log bleibt klein.
- Nachteile: Ein Eintrag zeigt nicht, welche Felder sich geändert haben.

**4B Alle Befehle:** Jeder Statuswechsel und jede Alert-Zustellung käme hinzu, Log und Snapshots würden stark wachsen.
**4C Mit Änderungsdetails:** bräuchte eine Redaktion je Objekttyp und ist für P4 nicht gefordert.

### Frage 5: Massenaktionen (ohne Rückfrage entschieden, Vorschlag)

**5A Ein Batch-Befehl, alles oder nichts.** Die API liest die Monitore, baut die geänderten Dokumente und schickt
**einen** Befehl `Batch` mit bis zu 500 `PutEntity`/`DeleteEntity`, jeweils mit `expected_revision`. Das FSM wendet alle
an oder keinen. ← vorgeschlagen

- Vorteile: Das FSM bleibt generisch, ohne JSON-Änderungen im `Apply`. Es gibt keine halben Ergebnisse. Konflikte durch
  gleichzeitige Änderungen erkennt `expected_revision`.
- Nachteile: Ein einziger Konflikt lässt die ganze Aktion scheitern. Der Client muss sie wiederholen (409 mit der Liste
  der betroffenen IDs).

**5B Die API schickt einen Befehl je Monitor.** Liefert Teilergebnisse je ID, ist bei 500 Monitoren aber langsam (500
Raft-Runden) und hinterlässt bei einem Leaderwechsel unklare Zwischenstände.

## Entscheidung

### 1. API-Tokens (FA-AUTH-10)

**Datenmodell** (`state.db`, Klasse A, neue Tabelle `api_tokens`):

| Spalte                           | Inhalt                                                                                      |
|----------------------------------|---------------------------------------------------------------------------------------------|
| `id`                             | öffentliche ID, `tok_` + 16 Zufallszeichen (base32), wird in der API zum Widerrufen genutzt |
| `tenant_id`                      | Mandant, nie `_system`                                                                      |
| `scope`                          | `tenant`; andere Werte lehnt das FSM bis zu einem späteren ADR ab                           |
| `name`                           | Anzeigename, 1–64 Zeichen, eindeutig je Mandant                                             |
| `role`                           | `tenant-viewer`, `tenant-editor` oder `tenant-admin`                                        |
| `secret_hash`                    | SHA-256 (hex) des Geheimnisses, eindeutiger Index                                           |
| `created_at_ms`, `expires_at_ms` | aus dem Befehl (`issued_at_ms`), Ablauf Pflicht                                             |
| `created_by`                     | Akteur, der das Token angelegt hat (Art, ID, Name)                                          |

**Format:** `uct_` + `NewOpaqueToken()`, also 256 Bit base64url. Das Präfix lässt Secret-Scanner Tokens erkennen und
trennt sie in der Middleware von JWTs. Gespeichert wird nur `HashToken(secret)`. Bei 256 Bit Entropie reicht SHA-256,
ein langsamer Hash wäre unnötig (wie ADR 0008). Das Geheimnis erscheint genau einmal in der Antwort auf das Anlegen, mit
`Cache-Control: no-store`, und nie in Logs, Audit oder Raft-Log. Im Befehl steht nur der Hash.

**Regeln:**

- Anlegen, auflisten und widerrufen darf nur ein **interaktiv angemeldeter** `tenant-admin` oder `platform-admin`, für
  den eigenen Mandanten. Tokens verwalten keine Tokens und keine Nutzer. Sonst könnte sich ein geleaktes Token selbst
  verlängern oder Nutzer anlegen.
- Die Rolle ist höchstens `tenant-admin`. Ein `platform-admin` legt Tokens nur für seinen eigenen Mandanten an (Frage
  2).
- Ablauf: Pflicht, 1 Stunde bis 366 Tage. Das FSM prüft das gegen `issued_at_ms` des Befehls. Abgelaufene Tokens bleiben
  30 Tage sichtbar und werden dann vom Aufräumbefehl entfernt (Abschnitt 2).
- Höchstens 50 Tokens je Mandant (feste Grenze im FSM, kein Plan-Limit).
- Widerruf löscht die Zeile. Der Audit-Eintrag bleibt.
- Mandant gesperrt: Tokens werden mit 403 abgewiesen, wie Logins. Mandant gelöscht: Tokens gehen mit dem Mandanten
  (`DeleteTenant` löscht alle Tabellen mit `tenant_id`).

**Prüfung bei der Anfrage** (Hauptagent, `internal/auth`):

1. Beginnt der Bearer-Wert mit `uct_`, sucht die Middleware den Hash im lokalen `state.db`, in jedem Auth-Modus und auf
   jedem Voter. Sonst läuft die bisherige JWT-Prüfung.
2. Unbekannt oder abgelaufen ergibt 401 mit einer einheitlichen Meldung, ohne Hinweis, welcher Fall vorliegt. Gesperrter
   Mandant ergibt 403.
3. Der Principal bekommt `UserID = tok_…`, den Mandanten und die Rolle des Tokens sowie ein Merkmal `ViaToken`. Befehle
   tragen `Actor{KIND_API_TOKEN, tok_…}`.
4. Ein Widerruf wirkt auf einem Follower erst, wenn der Befehl dort angewandt ist, üblicherweise nach Millisekunden. Im
   Degraded-Modus ohne Quorum gilt der letzte bekannte Stand.
5. `/admin/*`, die Nutzerverwaltung, `/api-tokens`, `/auth/*` und die Mandantenbefehle aus ADR 0029 lehnen `ViaToken`
   mit 403 ab. Das FSM lehnt `KIND_API_TOKEN` für Token-, Nutzer- und Mandantenbefehle zusätzlich ab.
6. WebSocket-Tickets (FA-AUTH-11) dürfen Tokens holen. Die Verbindung endet beim Ablauf des Tokens.

**API:** `GET/POST /api/v1/api-tokens`, `DELETE /api/v1/api-tokens/{id}`. Die Liste zeigt Name, Rolle, Ersteller, Anlage
und Ablauf, nie Hash oder Geheimnis.

### 2. Audit-Log (FA-DAT-01)

**Datenmodell** (`state.db`, neue Tabelle `audit_log`):

| Spalte                                 | Inhalt                                                                                                                             |
|----------------------------------------|------------------------------------------------------------------------------------------------------------------------------------|
| `seq`                                  | Raft-Index des Befehls, bei Batches ergänzt um eine laufende Nummer. Fortlaufend und auf allen Votern gleich.                      |
| `at_ms`                                | `issued_at_ms` des Befehls, nie die Uhr im `Apply`                                                                                 |
| `tenant_id`                            | betroffener Mandant; Plattform-Aktionen (Mandanten, Pläne, Nodes, Zertifikate) unter `_system`                                     |
| `actor_kind`, `actor_id`, `actor_name` | aus `pb.Actor`; neues Feld `Actor.name = 3` mit dem Anzeigenamen zum Zeitpunkt der Aktion                                          |
| `action`                               | z.B. `monitor.create`, `monitor.update`, `monitor.delete`, `monitor.pause`, `channel.update`, `api_token.create`, `tenant.suspend` |
| `object_kind`, `object_id`             | z.B. `monitor`, `mon_…`                                                                                                            |
| `request_id`                           | Request-ID des Befehls; verbindet die Einträge eines Batches                                                                       |

**Was erfasst wird:** jeder erfolgreich angewandte Befehl mit Akteur `USER` oder `API_TOKEN`. Dazu Befehle mit Akteur
`SYSTEM`, die eine Verwaltungsaktion sind: Recovery-CLI (ADR 0019), Auto-Provisionierung, Bindung von `default`,
Join-Token-Einlösung, Widerruf von Nodes und Zertifikaten. **Nicht** erfasst werden Betriebsbefehle: `SetMonitorState`,
Alert-Zustellung, Verlauf, Node-Gesundheit, Checkpoints, Inkarnationen, Systemalerts. Die Zuordnung steht als Tabelle je
Payload-Typ im Code. Ein Test sorgt dafür, dass jeder neue Payload-Typ dort eingetragen werden muss.

**Inhalt:** keine Dokumente, keine Feldwerte, keine Geheimnisse, keine IP-Adressen (Datenminimierung, Pflichtenheft 11).
Abgelehnte Befehle landen nicht im Audit-Log, sondern wie bisher im `slog`. Logins und Logouts (Sitzungsbefehle) sind
keine Objektänderungen und bleiben für P4 draußen.

**Aufbewahrung:** `AUDIT_RETENTION` (Standard `8760h`, mindestens `24h`). Der Leader schickt stündlich einen Befehl
`PruneAudit{before_ms}`. Die Zeit steht im Befehl. Derselbe Befehl entfernt seit 30 Tagen abgelaufene API-Tokens. Bei
`DeleteTenant` gehen die Einträge des Mandanten mit (Pflichtenheft 11). Der Löschvorgang selbst wird unter `_system`
erfasst.

**Lesen:** `GET /api/v1/audit` zeigt einem `tenant-admin` den eigenen Mandanten. `GET /api/v1/admin/audit` zeigt einem
`platform-admin` alle Mandanten und `_system`, mit Filter auf Mandant. Beide filtern nach Zeitraum, Aktion, Objekt und
Akteur und blättern per Cursor über `seq`, neueste zuerst, höchstens 200 je Seite. Tokens können das Audit-Log mit der
Rolle `tenant-admin` lesen.

### 3. Massenaktionen (FA-MON-06)

- `POST /api/v1/monitors/bulk` mit `{action: pause|resume|delete|addTags|removeTags, ids: [...] (1–500), tags: [...]}`.
- Die API prüft jeden Monitor und baut die Dokumente. Für Tags gelten dieselben Regeln wie beim Einzel-Update (höchstens
  20, je 64 Zeichen). Ein Fehler ergibt 422 mit Feldzeiger `/ids/N`. Danach geht **ein** Befehl `Batch` an das FSM
  (Option 5A).
- Das FSM erlaubt in `Batch` vorerst nur `PutEntity` und `DeleteEntity` für Monitore eines Mandanten. Jede Teiloperation
  durchläuft dieselben Prüfungen wie einzeln, auch Limits und Revisionen. Scheitert eine, wird nichts angewandt. Die API
  antwortet dann 409 mit den IDs, die den Konflikt auslösen.
- Löschen folgt FA-MON-07: Die Konfiguration ist sofort weg, Heartbeats und Rollups räumt der bestehende Job asynchron.
- Jede Teiloperation bekommt einen eigenen Audit-Eintrag mit gemeinsamer `request_id`.

### 4. Befehlsstufe

- `CommandVersion` wird **9** (`AuditCommandLevel`). Neue Befehle sind `CreateAPIToken`, `RevokeAPIToken`, `PruneAudit`
  und `Batch`, dazu das Feld `Actor.name`.
- Audit-Einträge schreibt das FSM erst ab Stufe 9. Vorher entstünden auf gemischten Votern unterschiedliche Zustände.
- `api_tokens` und `audit_log` gehen erst ab Stufe 9 in den State-Fingerprint ein (wie bei ADR 0029).
- Die Middleware nimmt `uct_`-Tokens erst an, wenn der Cluster Stufe 9 erreicht hat. Vorher antwortet `POST /api-tokens`
  mit 409 und einem Hinweis auf das ausstehende Update.

## Konsequenzen

- Positiv:
    - Automatisierung ohne Nutzerkonto, in allen Auth-Modi gleich.
    - Ein geleaktes Token betrifft höchstens einen Mandanten und läuft spätestens nach einem Jahr ab.
    - Das Audit-Log ist auf allen Votern identisch und kann nicht fehlen, weil es im selben `Apply` entsteht.
    - Massenaktionen sind atomar.
- Negativ / Risiken:
    - **Größe:** Das Audit-Log liegt in Klasse A und damit in jedem Snapshot. Grob geschätzt bei 1.000 Mandanten mit je
      20 Änderungen am Tag: 7,3 Mio. Einträge im Jahr, etwa 1,5 GB. Für große Installationen muss `AUDIT_RETENTION`
      kürzer werden, oder ein späterer ADR verlegt ältere Einträge in ein Archiv. Die Betriebsdoku nennt die Rechnung.
    - Ohne Nutzungszeit (3C) fallen vergessene Tokens nur über den Ablauf auf.
    - Ein Widerruf wirkt auf Followern mit Replikationsverzug, im Degraded-Modus erst nach Rückkehr des Quorums.
    - Ein Batch scheitert als Ganzes an einem einzigen Konflikt.
    - `actor_name` ist ein personenbezogenes Datum. Es bleibt bis zum Ende der Aufbewahrung gespeichert, auch nach dem
      Löschen des Nutzers. Die Betriebsdoku muss das nennen.
- Folgearbeiten:
    - Eigener ADR für Provisionierungs-Tokens (`scope = provisioning`), sobald das Backend konkret ist.
    - Login- und Logout-Ereignisse im Audit, falls gewünscht.
    - Oberfläche: Token-Verwaltung und Audit-Ansicht (AP-4.6), Massenaktionen in der Monitorliste.
    - STRIDE-Ergänzung für Tokens.

## Approval

| Datum      | Status   | Durch    | Anmerkung                                                                                                       |
|------------|----------|----------|-----------------------------------------------------------------------------------------------------------------|
| 2026-11-18 | Proposed | Claude   | Richtungsfragen im Chat entschieden: 1A, 2A, 3C, 4A. Frage 5 (Massenaktionen) ist ein Vorschlag ohne Rückfrage. |
| 2026-11-18 | Accepted | Projektinhaber | im Chat angenommen („Accept ADR 31“), eingetragen durch Claude                                                  |
