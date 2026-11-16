# ADR 0023: Alert-Drosselung, Bündelung, dauerhafte Fehler und Flapping

| | |
|---|---|
| **Status** | Proposed |
| **Datum** | 2026-11-16 |
| **Entscheider** | Projektinhaber |
| **Bezug** | FA-ALR-04/05/06, PF-25, PF-26, ADR 0014 (Outbox), 0018 (Befehlsversionen) |

## Kontext

Die Outbox aus ADR 0014 stellt jeden Übergang einzeln zu, mit Backoff 30 s · 2ⁿ. Für P3 fehlen:
- **FA-ALR-04:** Rate-Limit je Ziel (Token Bucket), Beachtung von HTTP 429 mit `Retry-After`, Bündelung gleichzeitiger Wechsel eines Mandanten (30 s, abschaltbar).
- **FA-ALR-05:** Dauerhafte Fehler stoppen bereits und setzen den Kanal auf `disabled`. Die UI zeigt das schon an. Es fehlt die Information des `tenant-admin` über einen anderen Kanal.
- **FA-ALR-06:** Mehr als N Wechsel in T (6 in 30 min) → „flapping“, Einzelalerts unterdrückt, eine Zusammenfassung.

Randbedingung: Was die FSM tut, muss auf allen Votern gleich sein. Neues FSM-Verhalten braucht deshalb das Feature-Gate aus ADR 0018. Was nur der Dispatcher auf dem Leader tut, braucht es nicht.

## Optionen

### Option A: Alles im Dispatcher auf dem Leader
- Vorteile: Keine FSM-Änderung.
- Nachteile: Flapping-Zustand und Unterdrückung gingen beim Leaderwechsel verloren; der Status „flapping“ wäre nicht replizierter Zustand.

### Option B: Drosselung und Bündelung im Dispatcher, Flapping und Kanal-Ausfall-Alert in der FSM
- Vorteile: Was flüchtig sein darf (Token, Bündelfenster), bleibt im Speicher. Was dauerhaft und für alle sichtbar sein muss (Flapping, Ausfall-Alert), liegt im Raft-Zustand.
- Nachteile: Befehlsversion 4 und eine Migration.

### Option C: Bündelung ebenfalls in der FSM (Sammel-Outbox-Einträge)
- Vorteile: Bündel überleben Leaderwechsel.
- Nachteile: Die FSM bräuchte Zeitfenster. Bündel sind Komfort; ein Leaderwechsel im Fenster erzeugt schlimmstenfalls Einzelnachrichten.

## Entscheidung

**Option B.**

**1. Rate-Limit (Dispatcher, im Speicher).**
- Je Kanal ein Token Bucket: 1 Nachricht/s, Burst 5, für alle Arten gleich. Telegram-Gruppen erlauben 20/min; dafür reichen Burst und Bündelung.
- Ohne Token bleibt der Eintrag fällig und wird in einem späteren Durchlauf versendet. Das zählt nicht als Versuch.
- Ein neuer Leader beginnt mit vollen Buckets.

**2. HTTP 429.**
- Webhook: `Retry-After` in Sekunden oder als HTTP-Datum, begrenzt auf 1 h.
- Shoutrrr: Die Bibliothek liefert den Header nicht. Erkannt wird `429` im Fehlertext, dann gelten 60 s.
- Der Versuch wird wie jeder vorübergehende Fehler mit `AckAlertDelivery` festgehalten, aber mit `next_attempt_ms = max(Backoff, Retry-After)`. Das ändert die FSM nicht.
- Zusätzlich sperrt der Dispatcher den Kanal im Speicher bis dahin, damit auch andere Einträge desselben Kanals warten.

**3. Bündelung (Dispatcher, im Speicher, nur Shoutrrr-Kanäle).**
- Steuerung über `ALERT_BUNDLE_WINDOW` (Standard 30 s, `0` = aus). Eine Einstellung je Mandant folgt mit P4.
- Ablauf je Kanal: Der erste fällige Eintrag geht sofort raus. Weitere Einträge, die innerhalb des Fensters nach diesem Versand fällig werden, sammelt der Dispatcher. Am Fensterende verschickt er sie als **eine** Nachricht („3 Statuswechsel: …“, je Zeile Monitor, Status, Zeit) und bestätigt jeden Eintrag einzeln per `AckAlertDelivery`.
- Ein Bündel aus nur einem Eintrag wird normal formatiert. Webhooks werden nicht gebündelt: Maschinen erwarten ein Ereignis je Aufruf und deduplizieren über `alertId`.

**4. Flapping (FSM, ab Befehlsversion 4).**
- `SetMonitorState` zählt die Wechsel zwischen UP, DOWN, DEGRADED und PARTIAL in `status_history` der letzten 30 min (Zeit aus `issued_at_ms`).
- Erreicht die Zahl einschließlich des neuen Wechsels **6**, setzt die FSM `flapping = true` und `flapping_since_ms`. Sie legt **einen** Outbox-Eintrag mit Ereignis `flapping` an (`alert_key = <monitor>:<since>:FLAPPING`).
- Solange `flapping` gilt, legt sie für Wechsel dieses Monitors keine Outbox-Einträge an. Der Status selbst wird weiter geschrieben, UI und Uptime bleiben korrekt.
- Ende: Der Leader schlägt `ClearMonitorFlapping{monitor_id, expected_since_ms}` vor, sobald 30 min lang kein Wechsel kam. Die FSM legt dann eine Zusammenfassung an (Ereignis `flapping`, Text „stabil seit …, aktueller Status X, n Wechsel“).
- Die Schwellen sind vorerst Konstanten (6 / 30 min), damit alle Voter gleich rechnen. Je Monitor einstellbar werden sie in P6.
- Unterhalb von Level 4 verhält sich die FSM wie bisher.

**5. Kanal ausgefallen (FA-ALR-05, FSM, ab Befehlsversion 4).**
- Setzt `AckAlertDelivery` einen Kanal auf `disabled` oder wechselt er auf `failing`, legt die FSM je **anderem** aktivem Standardkanal (`is_default`) des Mandanten einen Eintrag mit Ereignis `channel_failed` an (Kanalname, Grund ohne Geheimnisse, gleiche Maskierung wie `last_error`).
- Gibt es keinen solchen Kanal, bleibt es bei der Anzeige in der UI.

**Datenmodell.**
- Migration: `monitor_state.flapping`, `monitor_state.flapping_since_ms`.
- Ereignisse `flapping` und `channel_failed` im Kanal-Ereignisfilter. Bestehende Kanäle erhalten beide; neue standardmäßig auch.
- API: `MonitorState.flapping` (additiv), Badge in Dashboard und Monitor-Detail.
- Neuer Befehl `ClearMonitorFlapping`; die neuen FSM-Pfade sind an `command_level ≥ 4` gebunden.

## Konsequenzen

- Positiv: Gesperrte Plattform-IPs und Alarmfluten werden unwahrscheinlicher (PF-25/26). Flapping und Kanal-Ausfall sind replizierter Zustand und überleben Leaderwechsel. Rate-Limit und Bündelung kommen ohne FSM-Änderung aus.
- Negativ / Risiken:
  - Ein Leaderwechsel im Bündelfenster schickt die offenen Einträge einzeln (at least once bleibt erhalten).
  - Die festen Flapping-Schwellen passen nicht für jeden Monitor; ein Monitor mit kurzem Intervall erreicht sie schneller.
  - 429 zählt als Versuch. Bei sehr langem `Retry-After` und `ALERT_MAX_ATTEMPTS` 10 kann ein Eintrag nach über 10 h `failed` werden.
- Folgearbeiten: Schwellen und Bündelung je Mandant/Monitor (P4/P6). Systemkanal für Plattform-Admins (AP-3.6). Tests: Token Bucket, 429 je Senderart, Bündel inkl. Leaderwechsel, FSM-Tabellentest Flapping (Grenze 5/6, Ende, Level 3 vs. 4), Kanal-Ausfall-Alert.

## Approval

| Datum | Status | Durch | Anmerkung |
|-------|--------|-------|-----------|
| 2026-11-16 | Proposed | Claude | |
