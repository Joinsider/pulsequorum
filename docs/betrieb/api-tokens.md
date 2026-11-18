# API-Tokens, Audit-Log und Massenaktionen

Grundlage ist [ADR 0031](../adr/0031-api-tokens-audit-massenaktionen.md) (FA-AUTH-10, FA-DAT-01, FA-MON-06). Alle drei Funktionen gibt es erst, wenn alle Voter eine Version mit Befehlsstufe 9 haben und der Cluster diese Stufe erreicht hat. Bis dahin antwortet `POST /api/v1/api-tokens` mit 409, und Tokens werden nicht angenommen.

## 1. API-Tokens

Ein API-Token gehört einem Mandanten, nicht einer Person. Gedacht ist es für CI, Skripte und Automatisierung. Es funktioniert in jedem Auth-Modus (lokal und OIDC) und auf jedem Voter.

**Anlegen:** Ein `tenant-admin` oder `platform-admin` legt Tokens für den eigenen Mandanten an, und zwar nur interaktiv angemeldet, nie mit einem anderen Token:

```bash
curl -X POST https://uptime.example/api/v1/api-tokens \
  -H "Authorization: Bearer $ACCESS_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"ci","role":"tenant-editor","expiresAt":"2027-06-30T00:00:00Z"}'
```

- **Rolle:** `tenant-viewer`, `tenant-editor` oder `tenant-admin`. `platform-admin` ist für Tokens nicht möglich.
- **Ablauf:** Pflicht, mindestens 1 Stunde und höchstens 366 Tage ab jetzt.
- **Name:** 1–64 Zeichen, eindeutig im Mandanten.
- **Anzahl:** höchstens 50 Tokens je Mandant.
- **Geheimnis:** Die Antwort enthält es unter `token` **genau einmal**, mit `Cache-Control: no-store`. Es beginnt mit `uct_`, damit Secret-Scanner (z.B. GitHub Secret Scanning mit eigenem Muster) geleakte Tokens finden. Gespeichert und über Raft verteilt wird nur der SHA-256. Ein verlorenes Geheimnis lässt sich nicht wiederherstellen: Token widerrufen und neu anlegen.
- **Wiederholung:** Das Anlegen hat bewusst keinen `Idempotency-Key`. Eine Wiederholung legt ein zweites Token an. Bei einer Antwort 503 „Result Unknown“ daher zuerst die Liste prüfen.

**Benutzen:** `Authorization: Bearer uct_…` wie ein Access-Token.

**Was ein Token darf:** Monitore (auch Massenaktionen), Benachrichtigungskanäle, Wartungsfenster, Incidents, Uptime, Live-Stream, die Mandantenansicht `/tenant` und, mit der Rolle `tenant-admin`, das Audit-Log `/audit`. Alles andere beantwortet der Server mit 403:

- `/auth/*`, `/me`
- Nutzerverwaltung
- `/api-tokens`
- `/admin/*`, Cluster- und Mandantenverwaltung

Das FSM lehnt Token-Befehle an Nutzern, Mandanten und Clusterobjekten zusätzlich ab.

**Fehler:** Unbekannte, abgelaufene und widerrufene Tokens bekommen einheitlich 401 „invalid or expired API token“. Bei einem gesperrten Mandanten kommt 403.

**Widerrufen:** `DELETE /api/v1/api-tokens/{id}`. Auf dem Voter, der die Anfrage beantwortet, wirkt das sofort, auf den übrigen nach der Replikation, üblicherweise nach Millisekunden. Im Degraded-Modus ohne Quorum ist ein Widerruf nicht möglich, und alle Voter prüfen gegen den letzten bekannten Stand.

**Aufräumen:** Der Leader entfernt Tokens 30 Tage nach ihrem Ablauf. Bis dahin zeigt die Liste sie als `expired`. Eine Nutzungszeit („zuletzt benutzt“) wird bewusst nicht erfasst (ADR 0031, Option 3C). Vergessene Tokens erkennt man an Ablaufdatum und Ersteller.

## 2. Audit-Log

Das FSM schreibt einen Eintrag im selben Schritt wie die Änderung. Der Eintrag ist damit auf allen Votern gleich und kann nicht fehlen.

**Was erfasst wird:**
- Jede angewandte Änderung durch Nutzer und Tokens: Monitore, Kanäle, Wartungsfenster, Nutzer, Tokens.
- Verwaltungsbefehle: Mandanten, Pläne, Join-Tokens, Widerruf von Nodes und Zertifikaten, Registrierung von Nodes, Anheben der Befehlsstufe.

**Was nicht erfasst wird:**
- Betriebsbefehle: Statuswechsel, Alert-Zustellung, Node-Gesundheit, Checkpoints.
- Logins und Logouts.
- Abgelehnte Befehle (diese stehen im Log des Voters).

**Inhalt:** Zeit, Mandant, Akteur (Art, ID, Anzeigename zum Zeitpunkt der Aktion), Aktion (z.B. `monitor.pause`), Objekt (Art, ID) und Request-ID. Nicht gespeichert werden Konfiguration, Feldwerte, Geheimnisse und IP-Adressen. Einträge einer Massenaktion teilen sich die Request-ID.

**Lesen:**
- `GET /api/v1/audit` zeigt einem `tenant-admin` (auch einem Token mit dieser Rolle) den eigenen Mandanten.
- `GET /api/v1/admin/audit` zeigt einem interaktiv angemeldeten `platform-admin` alle Mandanten. Mit `?tenant=_system` sieht er die Plattform-Aktionen: Mandanten, Pläne, Nodes, Zertifikate, Join-Tokens und Befehlsstufe stehen unter `_system`, nicht unter dem betroffenen Mandanten.
- Filter: `from`, `to`, `action`, `objectKind`, `objectId`, `actor`. Die Liste ist neueste zuerst sortiert, `cursor` blättert weiter, `limit` erlaubt höchstens 200.

**Aufbewahrung:** `AUDIT_RETENTION`, Standard `8760h` (1 Jahr), mindestens `24h`. Der Leader löscht stündlich ältere Einträge. Wird ein Mandant gelöscht, gehen seine Einträge mit. Die Löschung selbst bleibt unter `_system` stehen.

**Größe:** Das Audit-Log liegt in `state.db` und damit in jedem Raft-Snapshot. Grob gerechnet: 1.000 Mandanten mit je 20 Änderungen am Tag ergeben etwa 7,3 Mio. Einträge und rund 1,5 GB im Jahr. Bei großen Installationen `AUDIT_RETENTION` verkürzen.

**Datenschutz:** Der Anzeigename des Nutzers bleibt bis zum Ende der Aufbewahrung gespeichert, auch wenn der Nutzer gelöscht wird. Das gehört ins Verzeichnis der Verarbeitungstätigkeiten. Aktionen, die vor dem Erreichen der Stufe 9 liefen (z.B. der Bootstrap und der erste Admin), stehen nicht im Log.

## 3. Massenaktionen

`POST /api/v1/monitors/bulk` mit `{"action": "pause|resume|delete|addTags|removeTags", "ids": [...], "tags": [...]}`:

- **Umfang:** 1 bis 500 Monitore. Erlaubt mit der Rolle `tenant-editor` oder höher, auch per Token.
- **Alles oder nichts:** Die Aktion geht als ein Raft-Befehl raus. Hat ein anderer Client einen der Monitore inzwischen geändert, wird nichts angewandt, und die Antwort ist 409 mit der betroffenen Operation. Dann neu laden und wiederholen.
- **Prüfung vorab:** Unbekannte IDs, doppelte IDs und mehr als 20 Tags ergeben 422 mit Feldzeiger (`/ids/N`). Die `ids` sind UUIDs; andere Werte lehnt schon die Schema-Prüfung mit 400 ab.
- **Ohne Änderung:** Monitore, an denen sich nichts ändert (bereits pausiert, Tag schon vorhanden), fallen aus dem Befehl heraus und melden ihre aktuelle Revision.
- **Löschen:** Die Konfiguration verschwindet sofort, Heartbeats und Rollups werden asynchron entfernt (FA-MON-07).

## 4. Testprotokoll

**2026-11-18, ein lokaler Voter** (Binary von `feat/AP4.4`, `AUTH_MODE=local`, Testnutzer aus dem Setup, nur `127.0.0.1`, Skript im Scratchpad, danach gelöscht). 31 Prüfungen, alle bestanden:

- **Stufe:** Der neue Cluster hebt die Befehlsstufe etwa 4 s nach dem Start von 2 auf 9. In dieser Zeit antwortet `POST /api-tokens` korrekt mit 409.
- **Anlegen:**
  - Das Token beginnt mit `uct_`, die Antwort trägt `Cache-Control: no-store`.
  - `platform-admin` wird mit 422 abgelehnt.
  - Die Liste enthält weder Geheimnis noch Hash.
  - In `state.db` steht nur ein 64-stelliger Hash, im Node-Log nur die Token-ID.
- **Benutzen (Editor-Token):** Monitore anlegen und auflisten, Massenaktionen `pause`, `addTags` und `delete` (Ergebnis geprüft), unbekannte UUID in `ids` → 422 `/ids/1`, nichts angewandt.
- **Grenzen:** Das Token bekommt 403 auf `/api-tokens`, `/me`, `/users`, `/admin/tenants`, `/admin/audit` und, mit Editor-Rolle, auf `/audit`.
- **Audit:**
  - Die Aktionen kommen in dieser Reihenfolge: `api_token.create` ×2, `monitor.create` ×3, `monitor.pause` ×2, `monitor.tags` ×3, `monitor.delete`.
  - Der Akteur der Token-Aktionen ist `api_token ci`.
  - Es steht keine Konfiguration im Log.
  - Setup und Bootstrap liefen vor Stufe 9 und stehen erwartungsgemäß nicht darin.
- **Widerruf:** 204, danach bekommt das Token 401. Ein erfundenes `uct_…` bekommt ebenfalls 401.
