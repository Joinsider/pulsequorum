# ADR 0015: Geheimnisse in Monitor-Konfigurationen

|                 |                                                                                      |
|-----------------|--------------------------------------------------------------------------------------|
| **Status**      | Accepted                                                                             |
| **Datum**       | 2026-10-21                                                                           |
| **Entscheider** | Projektinhaber                                                                             |
| **Bezug**       | FA-SEC-SEC, NFA-SEC-02, FA-API-06, FA-MON-*, FA-PRB-01, ADR 0004, ADR 0011, ADR 0014 |

## Kontext

Bei der Umsetzung des Frontends (AP-1.14) fiel auf: Zugangsdaten in Monitor-Konfigurationen werden weder verschlüsselt
noch maskiert.

- Der HTTP-Checker kennt `auth.password` und `auth.token` (im Typ-Schema als `writeOnly` markiert) und frei wählbare
  `headers`, in denen typischerweise `Authorization`, `X-Api-Key` o. Ä. stehen.
- Die Konfiguration liegt als JSON im Raft-Log, in `state.db`, in jedem Snapshot und künftig in jedem Export im
  Klartext.
- `GET /monitors`, `GET /monitors/{id}` und das Klonen liefern sie unverändert aus – an **alle** Rollen des Mandanten,
  auch `tenant-viewer`.
- FA-SEC-SEC und ADR 0011 zählen nur Kanal-URLs, Webhook-Header und Signaturschlüssel auf; für Monitore gibt es keine
  Regel. Mit den Typen aus P6 (SMTP/IMAP, Datenbanken, MQTT) kommen weitere Passwörter hinzu.

Randbedingungen:

- Jeder Voter führt Checks aus und braucht den Klartext zur Laufzeit; alle Voter haben denselben KEK und die DEKs (ADR
  0011).
- Die FSM bleibt deterministisch und kennt keinen Klartext (wie bei Kanälen, ADR 0014).
- Die Konfiguration ist typabhängig und wird über das JSON-Schema des Typs beschrieben (FA-API-06); die UI erzeugt ihre
  Formulare daraus.
- Probes (P5) erhalten Konfigurationen über mTLS (ADR 0006).

## Optionen

### Option A: Feldweise Verschlüsselung, gesteuert über das Typ-Schema

Jedes Feld, das im Schema des Typs als `writeOnly: true` markiert ist, gilt als Geheimnis. Die API zieht diese Werte vor
dem Raft-Vorschlag aus der Konfiguration heraus, verschlüsselt sie einzeln (ADR 0011, AAD
`monitors/<id>/config<JSON-Pointer>`) und legt sie in einer eigenen Map `secrets` (JSON-Pointer → Chiffrat) ab. In
`config` bleibt nur der Platzhalter `***`.

- Vorteile: Funktioniert für alle heutigen und künftigen Typen, indem das Schema Felder markiert. Nicht geheime Felder
  bleiben lesbar (Anzeige des Ziels, Filter, Suche). Gleiches Muster wie bei Kanälen (verschlüsselt + maskiert +
  „Maske = unverändert lassen“).
- Nachteile: Etwas Code für das Zerlegen/Zusammensetzen über JSON-Pointer. Geheimnisse in nicht markierten Feldern (z.
  B. Token in der Query der URL) sind nicht erfasst.

### Option B: Ganze Konfiguration verschlüsseln

`config` wird als Ganzes ein Chiffrat; die API entschlüsselt für jede Antwort und maskiert dann doch wieder nach Schema.

- Vorteile: Auch Geheimnisse in nicht markierten Feldern sind at rest geschützt.
- Nachteile: Jede Liste erfordert Entschlüsselung aller Monitore. Ziel-Anzeige, Suche und künftige SQL-Filter auf die
  Konfiguration gehen nicht ohne Entschlüsselung. Die Maskierung in der API braucht trotzdem die Schema-Markierung.
  Größere Änderung an Store und Migration.

### Option C: Nur maskieren, nicht verschlüsseln

- Vorteile: Klein.
- Nachteile: Verfehlt FA-SEC-SEC (Raft-Log, Snapshots, Exporte, gestohlene Volumes bleiben im Klartext). Nicht
  konsistent mit Kanälen.

### Option D: Konfiguration nur für `tenant-editor` und höher ausliefern

- Nachteile: Löst weder den Schutz at rest noch die Anzeige für Editoren (die sollen Passwörter auch nicht zurücklesen
  können).

## Entscheidung

**Option A**, ergänzt um zwei Festlegungen zu nicht markierten Feldern:

1. **Kennzeichnung:** Geheim ist jedes Feld, das im Typ-Schema `writeOnly: true` trägt – auch innerhalb von
   `additionalProperties` (Map-Werte) und `items` (Array-Elemente). Der HTTP-Checker markiert zusätzlich **alle
   Header-Werte** als `writeOnly`, weil dort üblicherweise Zugangsdaten stehen (wie bei Webhook-Headern, ADR 0014). Ein
   Plugin-Test prüft, dass jeder Typ mit Feldern namens `password`, `token`, `secret`, `apiKey` diese markiert.
2. **URLs mit Zugangsdaten:** Ein `userinfo`-Teil mit Passwort (`https://user:pass@host`) wird bei der Validierung mit
   422 abgelehnt und auf das `auth`-Objekt verwiesen. Tokens in Query-Parametern lassen sich nicht zuverlässig erkennen;
   die UI weist beim URL-Feld darauf hin (dokumentierte Grenze).
3. **Speicherung:** `MonitorDocument` erhält `secrets map[string]string` (JSON-Pointer → Chiffrat `v1|key_id|…`, AAD
   `monitors/<monitor-id>/config<pointer>`). Spalte `monitors.secrets_json` in `state.db` (Migration). In `config` steht
   an diesen Stellen der Platzhalter `***`. Die FSM lehnt Dokumente ab, deren `secrets` Werte ohne Chiffrat-Präfix
   enthalten (wie bei Kanälen); das Herauslösen geschieht in der API vor dem Vorschlag, der Klartext erreicht das
   Raft-Log nicht.
4. **API-Semantik (wie Kanäle):** Antworten enthalten an geheimen Stellen `***`. Bei `PUT` bedeutet ein fehlendes Feld
   oder der Wert `***` „gespeichertes Geheimnis behalten“, sofern Typ und Pfad gleich bleiben; ein anderer Wert ersetzt
   es; Typwechsel verwirft alle Geheimnisse. Ein leerer String löscht das Feld. Beim Klonen werden die Geheimnisse
   entschlüsselt und mit der AAD der neuen ID neu verschlüsselt.
5. **Laufzeit:** Scheduler und On-Demand-Test erhalten die zusammengesetzte Konfiguration über eine Funktion im Paket
   der API/Secrets-Schicht (`ResolveConfig(monitor) (json.RawMessage, error)`), die entschlüsselt und an den Pointern
   einsetzt. Der Klartext lebt nur im Speicher während des Checks; Checker-Meldungen und Logs geben ihn nicht aus
   (Test). Probes (P5) erhalten die aufgelöste Konfiguration vom Voter über mTLS und speichern sie nicht.
6. **Bestand:** Ein einmaliger Leader-Job beim Start schreibt Monitore mit Klartext-Geheimnissen über normale
   `PutEntity`-Befehle in das neue Format um (idempotent). Ältere Raft-Log-Einträge und Snapshots enthalten den
   Klartext, bis sie kompaktiert sind; die Release-Notes empfehlen, betroffene Zugangsdaten danach zu rotieren.
7. **Rotation:** Monitor-Geheimnisse werden vom Neuverschlüsselungs-Job bei DEK-Rotation (ADR 0011) mit erfasst.
8. **Pflichtenheft:** FA-SEC-SEC wird um „Zugangsdaten in Monitor-Konfigurationen (Felder mit `writeOnly` im
   Typ-Schema)“ ergänzt; FA-API-06 um die Maskierungs- und Behalten-Semantik.

## Konsequenzen

- Positiv: Zugangsdaten von Checks sind at rest verschlüsselt und werden keiner Rolle mehr angezeigt; ein Mechanismus
  für alle künftigen Typen; Verhalten konsistent mit Benachrichtigungskanälen und damit in der UI gleich bedienbar.
- Negativ / Risiken: Jeder Check-Lauf mit Geheimnissen kostet eine AES-GCM-Entschlüsselung (vernachlässigbar; bei Bedarf
  Cache je Monitor-Revision im Speicher). Schema-Pflege wird sicherheitsrelevant: ein nicht markiertes Feld bleibt
  Klartext (abgefedert durch den Plugin-Test). Geheimnisse in URL-Query-Parametern bleiben ungeschützt. Altbestand im
  Raft-Log bis zur Kompaktierung.
- Folgearbeiten (nach Annahme, als eigenes Arbeitspaket in P1 vor AP-1.15):
    - Backend: Schema-Walker (Pointer der `writeOnly`-Felder), Herauslösen/Einsetzen, `secrets` in `MonitorDocument` +
      Migration, FSM-Prüfung, PUT/Clone-Semantik, `ResolveConfig` in Scheduler und Test-Endpunkt, Bestandsmigration,
      Tests (Isolation, kein Klartext in Antworten/Logs/Raft-Log, Behalten/Ersetzen/Löschen, Typwechsel, Klonen).
      Sicherheitscode → Hauptagent.
    - HTTP-Checker: Header-Werte als `writeOnly`, Ablehnung von `userinfo` mit Passwort.
    - OpenAPI: Beschreibung der Maskierung bei `MonitorInput.config`/`Monitor.config`.
    - Frontend: Monitor-Editor behandelt `***` wie der Kanal-Editor („Ersetzen“-Aktion, unverändert = weglassen), auch
      für Map-Werte (Header).
    - Pflichtenheft FA-SEC-SEC, FA-API-06; Hinweis in den Release-Notes.

## Approval

| Datum      | Status   | Durch    | Anmerkung         |
|------------|----------|----------|-------------------|
| 2026-10-21 | Proposed | Claude   |                   |
| 2026-10-22 | Accepted | Projektinhaber | im Chat bestätigt |
