# ADR 0014: Benachrichtigungen – Kanäle, Outbox und Versand

| | |
|---|---|
| **Status** | Accepted |
| **Datum** | 2026-10-19 |
| **Entscheider** | Projektinhaber |
| **Bezug** | FA-ALR-01..03/05/07..09/12/13, NFA-REL-03, Konzept E7/E8 und Kap. 9, ADR 0005, 0011, 0012 |

## Kontext

AP-1.10 führt Benachrichtigungen ein. Drei Dinge sind dabei architekturrelevant:

1. **Versandbibliothek:** Das Konzept (E7) nennt Shoutrrr-URLs und `nicholas-fedor/shoutrrr`, ein ADR dazu fehlt. Die Bibliothek ist eine neue Abhängigkeit mit vielen Diensten (SMTP, Telegram, Discord, ntfy, …) und damit Angriffsfläche, Lizenz- und Wartungsthema.
2. **Zustellgarantie (FA-ALR-03):** Alerts sollen *at least once* zugestellt werden, auch über Neustarts und (ab P3) Leaderwechsel hinweg. Der Alert-Zustand gehört deshalb in den Raft-Zustand. Der Protobuf-Entwurf enthält `EnqueueAlert` und `AckAlertDelivery`; das Zusammenspiel mit `SetMonitorState` ist offen.
3. **Webhook-Templates (FA-ALR-07, PF-27):** Templates müssen immer gültiges JSON erzeugen, auch wenn Monitornamen `"`, `\` oder Zeilenumbrüche enthalten.

Randbedingungen: FSM-`Apply` ist deterministisch (keine Uhr, kein Zufall, kein I/O). Geheimnisse in Kanal-URLs und Webhook-Headern werden nach ADR 0011 verschlüsselt und nie geloggt. Lizenzen nach ADR 0010.

## Optionen

### 1. Versandbibliothek

**A: `github.com/nicholas-fedor/shoutrrr`, Version gepinnt, hinter einer eigenen Schnittstelle.**
Recherche (Haiku-Agent) und lokale Prüfung am 2026-10-19:
- Aktuell v0.22.0 (2026-10-13), MIT, Releases alle 1–3 Wochen; `go 1.27`.
- `SendContext(ctx, …)` mit Kontext (Abbruch, Deadline), Standardbudget 10 s je Versand.
- `CreateSenderWithOptions(types.SenderOptions{HTTPClient, DialContext}, …)` reicht einen eigenen HTTP-Client bzw. Dialer an alle Dienste weiter, die das unterstützen (`HTTPClientSetter`, `DialContextSetter`) – damit greift der zentrale SSRF-Dialer (FA-ALR-12). Pro genutztem Dienst per Test zu prüfen.
- Seit v0.22.0 enthalten Fehler und Logs laut Changelog keine Dienst-URLs/Zugangsdaten mehr; Fehler sind `*types.TargetError`. Logging über einen globalen Logger (`SetLogger`).
- Abhängigkeiten im Build (lokal mit `go list -deps` gemessen): 13 Module – `eclipse/paho.golang` (MQTT), `gorilla/websocket`, `mellium.im/xmpp` u. a. (BSD-2-Clause), `mattn/go-colorable`/`go-isatty` (MIT), `golang.org/x/*` (BSD). `eclipse/paho.golang` ist **dual lizenziert EPL-2.0 oder EDL-1.0**; EDL-1.0 ist eine BSD-3-Clause-Lizenz.
- Vorteile: Shoutrrr-URL-Kompatibilität (E7) für ~20 Dienste ohne eigenen Code; aktive Pflege; Client-Injektion für SSRF-Schutz und Proxys.
- Nachteile: Fremdcode für viele Protokolle (MQTT, XMPP, SMTP) im Binary; einzelner Fork-Maintainer; `SendContext` ist neu (v0.22.0), die API kann sich noch bewegen; keine eingebauten Retries/429-Behandlung (machen wir selbst).

**B: Eigene Implementierung weniger Dienste (ntfy, Telegram, Discord, Gotify, SMTP).**
- Vorteile: volle Kontrolle über HTTP-Client, Timeouts, SSRF-Dialer und Fehlermeldungen; keine Fremdabhängigkeit.
- Nachteile: deutlich mehr Code und Pflege; Nutzer verlieren die Shoutrrr-URL-Kompatibilität (E7), die in Uptime Kuma, Watchtower u. a. verbreitet ist.

**C: Ursprüngliches `containrrr/shoutrrr`.**
- Nachteil: kaum noch gepflegt; der Fork ist der aktive Zweig.

### 2. Entstehung der Alerts

**A: Die FSM legt Outbox-Einträge beim Anwenden von `SetMonitorState` an** (im selben Transaktionsschritt wie Statuswechsel und Incident).
- Vorteile: Statuswechsel und Alert sind atomar – kein Wechsel ohne Alert, kein Alert ohne Wechsel, auch wenn der Leader direkt nach dem Commit abstürzt. Deterministisch, weil alle Eingaben (Kanalzuordnung, Ereignisfilter, Zeit aus dem Befehl) repliziert sind.
- Nachteile: Die FSM kennt Kanalzuordnung und Ereignisfilter (etwas mehr Logik in `Apply`).

**B: Der Leader schlägt nach dem Statuswechsel einen eigenen `EnqueueAlert`-Befehl vor.**
- Nachteil: Zwischen beiden Befehlen kann der Leader ausfallen → verlorener Alert, genau der Fall, den FA-ALR-03 ausschließen soll.

### 3. Webhook-Templates

**A: JSON-Baum-Ersetzung:** Das Template ist selbst JSON; Platzhalter `{{monitor.name}}` stehen nur in String-Literalen. Ein String, der nur aus einem Platzhalter besteht, wird durch den JSON-Wert ersetzt (auch Zahl/Bool/null); Platzhalter innerhalb längerer Strings werden als Text eingesetzt und vom JSON-Encoder maskiert. Feste Variablenliste, keine Funktionen.
- Vorteile: Ergebnis ist per Konstruktion gültiges JSON; Validierung beim Speichern = JSON parsen + unbekannte Platzhalter ablehnen. Kein Template-Interpreter mit Schleifen oder Funktionsaufrufen.
- Nachteile: Keine Bedingungen/Schleifen im Template.

**B: `text/template` mit `json`-Funktion.**
- Nachteile: Ungültiges JSON bleibt möglich (vergessene Funktion, Kommas in Bereichen); Validierung erst bei der Ausführung; mächtige Sprache für mandantenseitige Eingaben.

## Entscheidung

**1A, 2A, 3A.**

**Bibliothek.** `nicholas-fedor/shoutrrr` in einer festen Version, ausschließlich über das Paket `internal/alert` angesprochen (Schnittstelle `Sender`), damit sie austauschbar bleibt. Versand immer über `CreateSenderWithOptions` mit eigenem HTTP-Client und Dialer, `SendContext` mit 10-s-Deadline, Shoutrrr-Logger auf `slog` (Level debug) umgeleitet. Fehlermeldungen werden zusätzlich selbst bereinigt (siehe Maskierung), unabhängig von der Zusage der Bibliothek.

**Lizenz-Ausnahme (ADR 0010):** `github.com/eclipse/paho.golang` wird unter der **Eclipse Distribution License 1.0 (BSD-3-Clause)** genutzt, nicht unter EPL-2.0. `make licenses` erhält dafür eine dokumentierte Ausnahme für genau dieses Modul; `NOTICE`/Drittlizenzen führen die EDL-1.0.

**Datenmodell (Datenklasse A, über Raft):**
- `notification_channels(tenant_id, id, name, kind, url_enc, webhook_json, events, is_default, state, last_error, created_ms, updated_ms)` als `PutEntity`/`DeleteEntity` mit `ENTITY_KIND_NOTIFICATION_CHANNEL`. URL und Webhook-Header werden **vom Leader vor dem Vorschlag** verschlüsselt (ADR 0011, AAD `notification_channels/<id>/url` bzw. `…/header/<name>`); die FSM sieht nur Chiffrat.
- `monitor_channels(tenant_id, monitor_id, channel_id)` – n:m-Zuordnung, gepflegt über das neue Feld `notificationChannelIds` im Monitor-Dokument. Standardkanäle (`is_default`) setzt die API beim Anlegen eines Monitors ein, nicht die FSM.
- `alert_outbox(tenant_id, alert_key, channel_id, monitor_id, payload_json, attempts, next_attempt_ms, state, last_error, created_ms)` mit `state ∈ {pending, delivered, failed}`.

**Ablauf:**
1. `SetMonitorState` (AP-1.8) legt in derselben Transaktion je zugeordnetem Kanal, dessen Ereignisfilter den Übergang umfasst (`down`, `up`, `degraded`, `partial`), einen Outbox-Eintrag an. `alert_key = <monitor>:<since_ms>:<status>`; zusammen mit dem Kanal eindeutig (Deduplizierung nach FA-ALR-03). Die Nutzlast (Monitorname, Status, vorheriger Status, Zeitpunkt, Ursache, Incident-ID) wird als JSON-Schnappschuss gespeichert, damit spätere Umbenennungen den Alert nicht verändern. Kein Alert für PENDING, PAUSED und (ab AP-1.11) MAINTENANCE; UP nur nach einem DOWN (Rückkehr), nicht nach dem initialen PENDING.
2. Ein **Dispatcher auf dem Leader** liest fällige Einträge aus `state.db`, entschlüsselt die Kanal-URL, versendet (Timeout 10 s) und schlägt `AckAlertDelivery` vor: zugestellt, oder fehlgeschlagen mit `next_attempt_ms` (exponentielles Backoff 30 s · 2ⁿ, max. 1 h, ±20 % Jitter – vom Leader berechnet, damit die FSM deterministisch bleibt), oder dauerhaft fehlgeschlagen (FA-ALR-05: HTTP 401/403/404, ungültige URL → Kanal `disabled`).
3. Nach `ALERT_MAX_ATTEMPTS` (Standard 10) wird ein Eintrag `failed`; der Kanal wechselt auf `failing`.
4. Ein Absturz zwischen Versand und Ack führt zu erneutem Versand (*at least once*); Empfänger können über die stabile Alert-ID deduplizieren (Webhook-Feld `alertId`).
5. Erledigte Einträge löscht die FSM deterministisch nach 7 Tagen (Zeit aus dem Befehl, wie beim Request-Dedup).
6. Wiederholung bei andauerndem DOWN (`resendIntervalMinutes`, FA-ALR-02): der Dispatcher schlägt `EnqueueAlert` mit `alert_key = <monitor>:<since_ms>:DOWN:reminder:<n>` vor; die FSM legt den Eintrag nur an, wenn der Monitor noch im selben DOWN ist.

**Testversand** (`POST /notifications/{id}/test`) geht direkt, ohne Outbox, mit derselben Fehler-Maskierung.

**Maskierung:** Shoutrrr-URLs werden für Anzeige und Logs auf Schema, Host und unkritische Query-Parameter reduziert (`telegram://***@telegram?chats=@ops`). Fehlermeldungen der Bibliothek werden vor dem Speichern/Loggen um alle Teile der URL bereinigt (Userinfo, Pfad, Query-Werte).

**SSRF:** Webhook und Shoutrrr nutzen den zentralen `checker.Dialer` mit derselben Richtlinie wie Checks (`SSRF_PROTECTION`). Ein Shoutrrr-Dienst, der weder HTTP-Client noch Dialer übernimmt, wird bei aktivem SSRF-Schutz beim Speichern des Kanals abgelehnt (Tests je unterstütztem Dienst).

## Konsequenzen

- Positiv: Kein Statuswechsel ohne Alert; Alerts überleben Neustarts und (ab P3) Leaderwechsel; Webhook-JSON ist immer gültig; Geheimnisse liegen nur verschlüsselt im Raft-Zustand.
- Negativ / Risiken: Die FSM wächst um Outbox-Logik; der Raft-Log enthält pro Alert mindestens zwei Einträge (Statuswechsel, Ack). Bei Nennlast (seltene Statuswechsel) unkritisch. Shoutrrr bringt MQTT-, XMPP- und SMTP-Code ins Binary; Updates der Bibliothek werden wie alle Abhängigkeiten über Dependabot und CI geprüft (ADR 0009) und bei API-Brüchen hinter `internal/alert` abgefangen.
- Folgearbeiten: Rate-Limit je Ziel, Bündelung und 429-Behandlung (FA-ALR-04, P3); Flapping (FA-ALR-06, P3); Degraded-Modus (FA-ALR-10, P3); Wartungsunterdrückung mit AP-1.11 (FA-ALR-13); Neuverschlüsselung bei DEK-Rotation (AP-1.3-Folgearbeit) erfasst die Kanal-Geheimnisse.

## Approval

| Datum | Status | Durch | Anmerkung |
|-------|--------|-------|-----------|
| 2026-10-19 | Proposed | Claude | |
| 2026-10-19 | Accepted | Projektinhaber | Freigabe im Chat |
