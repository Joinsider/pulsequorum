# ADR 0026: DOWN-Alerts werden durch die Bündelung nicht zurückgehalten

|                 |                                                                                                            |
|-----------------|------------------------------------------------------------------------------------------------------------|
| **Status**      | Accepted                                                                                                   |
| **Datum**       | 2026-11-17                                                                                                 |
| **Entscheider** | Projektinhaber                                                                                                   |
| **Bezug**       | NFA-REL-03, FA-ALR-04, ADR 0023 (Punkt 3, Bündelung), ADR 0024 (lokaler Dispatcher), Abnahme P3 (Befund 1) |

## Kontext

Laut ADR 0023 geht der erste fällige Alert eines Shoutrrr-Kanals sofort raus. Alle weiteren, die innerhalb von
`ALERT_BUNDLE_WINDOW` (Standard 30 s) fällig werden, hält der Dispatcher bis zum Ende des Fensters zurück und schickt
sie dann als eine Nachricht.

In der Abnahme von P3 (AS-04) kam deshalb der DOWN eines zweiten Monitors, der kurz nach dem ersten ausfiel, nach 75–86
s an. NFA-REL-03 verlangt höchstens (retries + 1) × Intervall + 30 s, im Test also 70 s. Ohne Bündelung waren es
höchstens 61 s.

Projektinhaber hat im Chat entschieden: DOWN-Alerts werden nie zurückgehalten.

## Optionen

### Option A: DOWN-Alerts sofort, andere Ereignisse weiter gebündelt

- Vorteile: NFA-REL-03 gilt unabhängig von der Bündelung. Entwarnungen und Hinweise werden weiter gesammelt.
- Nachteile: Bei einem Massenausfall kommen mehr Einzelnachrichten.

### Option B: NFA-REL-03 um das Bündelfenster erweitern

- Vorteile: Keine Codeänderung.
- Nachteile: Die Erkennung eines Ausfalls wird für Kanäle mit Bündelung um bis zu 30 s langsamer. Das ist gerade für
  Chat-Kanäle wie Telegram ungünstig, die man wegen der schnellen Meldung nutzt.

## Entscheidung

**Option A.** Diese Entscheidung ändert Punkt 3 von ADR 0023.

- **Kein Zurückhalten:** Ist in einem laufenden Bündelfenster ein Eintrag mit Ereignis `down` fällig, schickt der
  Dispatcher sofort alle fälligen Einträge des Kanals. Bei einem Eintrag geht er einzeln raus, bei mehreren als eine
  Bündelnachricht, wie bisher am Fensterende. Zurückgehaltene Einträge anderer Ereignisse gehen dabei mit.
- **Fenster bleibt bestehen:** Das laufende Fenster wird weder verlängert noch neu gestartet. Später fällige Einträge
  anderer Ereignisse warten weiter bis zu seinem Ende.
- **Rate-Limit gilt weiter:** Ohne Token wartet auch ein DOWN auf den nächsten Token. Bei 1/s und Burst 5 dauert das
  höchstens wenige Sekunden. Ein `Retry-After`-Sperre (429) gilt weiter für alle Einträge des Kanals.
- **Gleiche Regel im lokalen Dispatcher:** Sie gilt auch für den Dispatcher des Degraded-Modus (ADR 0024) mit
  unbestätigten DOWN-Alerts.
- **Webhooks:** Sie werden weiterhin nie gebündelt; für sie ändert sich nichts.
- **Andere Ereignisse:** `up`, `degraded`, `partial`, `flapping`, `channel_failed` und `system` werden weiter gebündelt.
  Ebenso Korrekturen nach ADR 0024, die das Ereignis des ursprünglichen Alerts tragen.

## Konsequenzen

- **Positiv:** NFA-REL-03 gilt auch mit Bündelung. Die Bündelung reduziert weiterhin Entwarnungen und Hinweise.
- **Negativ / Risiken:** Bei einem Massenausfall bekommt ein Chat-Kanal bis zu 5 Nachrichten sofort und dann eine pro
  Sekunde. Liegen beim Senden mehrere DOWN gleichzeitig an, gehen sie als ein Bündel raus. Telegram-Gruppen mit 20
  Nachrichten pro Minute können bei mehr als etwa 20 gleichzeitig ausfallenden Monitoren eine 429 liefern; die wird nach
  ADR 0023 behandelt.
- **Folgearbeiten:**
    - Tests: DOWN im laufenden Fenster geht sofort, mit den zurückgehaltenen Einträgen; ein UP im Fenster wartet weiter;
      das Fenster wird nicht verlängert; dieselben Fälle im lokalen Dispatcher.
    - AS-04 mit Bündelung nachmessen und den Bericht der Abnahme von P3 aktualisieren.

## Approval

| Datum      | Status   | Durch    | Anmerkung                                                           |
|------------|----------|----------|---------------------------------------------------------------------|
| 2026-11-17 | Proposed | Claude   | Richtung im Chat von Projektinhaber gewählt („DOWN nie zurückhalten“)     |
| 2026-11-17 | Accepted | Projektinhaber | im Chat angenommen („Annehmen, umsetzen“), eingetragen durch Claude |
