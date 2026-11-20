# ADR 0021: Konsens-Bewertung auf dem Leader in Bewertungsrunden

|                 |                                                                                              |
|-----------------|----------------------------------------------------------------------------------------------|
| **Status**      | Accepted                                                                                     |
| **Datum**       | 2026-11-16                                                                                   |
| **Entscheider** | Projektinhaber                                                                                     |
| **Bezug**       | FA-CON-01/02/03/05/06, FA-INC-01, NFA-REL-03, Konzept 7.2 und 9.1 (E2), ADR 0005, 0006, 0017 |

## Kontext

Bis P2 bewertet jeder Voter nur seine eigenen Ergebnisse (`internal/aggregator`) und schlägt Statuswechsel per
`SetMonitorState` vor. Mit mehreren zuständigen Nodes widersprechen sich diese Vorschläge; der Konflikt-Check der FSM
(`expected_status`) verhindert nur das Schlimmste. FA-CON-06 verlangt die Bewertung auf dem Leader, FA-CON-01/02/05 den
Konsens nach `consensusPolicy` (`majority` | `any` | `all`, im Datenmodell und in der OpenAPI schon vorhanden) mit den
Status PARTIAL und UNKNOWN.

Offen ist, **was eine Bewertung ist**. Davon hängt FA-CON-03 ab („erst nach `retries + 1` aufeinanderfolgenden negativen
Bewertungen DOWN“). Randbedingungen:

- Alle Nodes prüfen einen Monitor zur selben Wanduhrzeit: `Phase(monitorID, interval) + k · interval`
  (`internal/scheduler`). Heartbeats eines Durchlaufs tragen also fast denselben Zeitstempel.
- Fremde Heartbeats erreichen den Leader per Push (Best Effort, Sekunden) oder Anti-Entropie (≤ 30 s), eigene sofort.
- Ein neuer Leader muss den Zählerstand (Retries) aus den replizierten Heartbeats rekonstruieren können (NFA-REL-03),
  ohne zusätzliche Raft-Schreibvorgänge je Check.

## Optionen

### Option A: Bewertung bei jedem eintreffenden Heartbeat

- Vorteile: Einfach, minimale Latenz.
- Nachteile: Mit n Nodes zählen Retries n-mal so schnell; das Ergebnis hängt von der Ankunftsreihenfolge ab, ein neuer
  Leader kommt zu einem anderen Zählerstand; ein verspäteter Heartbeat kann einen bereits entschiedenen Durchlauf
  umwerfen.

### Option B: Bewertungsrunde je Check-Durchlauf (Slot)

- Der Leader bewertet jeden Durchlauf k eines Monitors genau einmal, sobald alle zuständigen Nodes für k gemeldet haben,
  spätestens zu einer Frist.
- Vorteile: Retries behalten die Bedeutung aus P1 (Anzahl Intervalle). Die Bewertung ist eine reine Funktion der
  Heartbeats je Slot; ein neuer Leader rechnet dasselbe nach. Keine zusätzlichen Raft-Schreibvorgänge.
- Nachteile: Erkennung wartet auf den langsamsten Melder bis zur Frist (wenige Sekunden). Slot-Zuordnung hängt an der
  Phase und bricht bei Intervalländerung neu an.

### Option C: Jeder Node bewertet lokal, der Leader kombiniert Vorschläge

- Vorteile: Lokale Retries wie bisher.
- Nachteile: Ein Raft-Befehl je Node und Wechsel, doppelte Hysterese (lokal und global), schwer nachvollziehbar;
  widerspricht FA-CON-06.

## Entscheidung

**Option B.**

**Ort.** Der Aggregator läuft auf jedem Voter, bewertet und schlägt aber nur vor, solange der Node Leader ist (wie der
Dispatcher, ADR 0014). Follower prüfen und schreiben weiter Heartbeats, bewerten nicht.

**Eingang.** Die drei Stellen, die Heartbeats speichern (eigener Writer im Scheduler, Replikations-Server,
Anti-Entropie), melden gespeicherte Heartbeats an einen Beobachter. Der Aggregator hält je Monitor die Heartbeats der
offenen Slots im Speicher; `metrics.db` liest er nur beim Neuaufbau. Der Beobachter blockiert nie; ein verworfener
Hinweis wird durch die Frist aufgefangen.

**Slot.** `k = floor((ts − phase) / interval)` mit `phase = Phase(monitorID, interval)`. Frist für Slot k:
`start(k) + timeout + 5 s`, höchstens `start(k) + interval`. Der Slot wird bewertet, sobald jeder zuständige Node einen
Heartbeat für k geliefert hat, sonst zur Frist. Später eintreffende Heartbeats für k ändern das Ergebnis von k nicht
mehr, zählen aber als jüngste Meldung des Nodes für k + 1.

**Melder.** Zuständig sind die Nodes der aktuellen Zuweisung (`scheduler.Assigner`, ADR 0017), ab AP-3.2 abzüglich der
als `unhealthy` ausgeschlossenen (FA-CON-04; Schnittstelle als Filter vorgesehen). Je zuständigem Node zählt sein
jüngster Heartbeat mit `ts ≤ Ende von Slot k` und `ts > start(k) − 2 · interval`; ältere oder fehlende gelten als
fehlend (FA-CON-05). Heartbeats nicht zuständiger Nodes zählen nicht.

**Konsens** über die n meldenden Nodes, d davon DOWN, g davon DEGRADED:

| Ergebnis           | Bedingung                                         |
|--------------------|---------------------------------------------------|
| keine Meldung      | n = 0                                             |
| negativ            | `majority`: d > n/2 · `any`: d ≥ 1 · `all`: d = n |
| PARTIAL (positiv)  | Regel nicht erfüllt, d ≥ 1                        |
| DEGRADED (positiv) | d = 0, dieselbe Regel auf g statt d erfüllt       |
| UP (positiv)       | sonst                                             |

Bei `majority` mit 2 Meldern und 1 DOWN ergibt sich also PARTIAL.

**Statuswechsel** wie in P1 (`tracker.next`), nur mit dem Slot-Ergebnis statt des eigenen Ergebnisses: `retries + 1`
negative Slots → DOWN (davor PENDING), `recoveryThreshold` positive Slots beenden DOWN; PARTIAL, DEGRADED und UP sind
positive Ergebnisse und wechseln aus UP/PENDING sofort.

- **UNKNOWN:** n = 0 setzt UNKNOWN ohne Retries (die 2-Intervall-Frist ist schon die Hysterese). **Ausnahme:** Aus DOWN
  und PENDING heraus bleibt der Status bestehen; fehlende Meldungen zählen weder als DOWN noch als Erholung, der
  Incident bleibt offen. Damit ändert sich die FSM nicht (sie schließt den Incident bei jedem Verlassen von DOWN) und es
  entsteht kein Recovery-Alert ohne Messung.
- **Incident:** `AffectedNodes` = die Nodes, die im auslösenden Slot DOWN meldeten; Ursache und Fehlerklasse vom ersten
  davon nach Node-ID (deterministisch).
- Wartung, Pause und `expected_status` wie bisher.

**Leaderwechsel.** Ein neuer Leader bewertet erst nach einer Karenz von 10 s (wie die Gesundheit in ADR 0017), damit die
bei Raft-Ereignissen ausgelöste Anti-Entropie nachliefern kann. Danach baut er je Monitor die Streaks aus den Heartbeats
aller Nodes seit `since` des aktuellen Status neu auf, Slot für Slot mit denselben Regeln. Fehlen ihm Heartbeats, die
der alte Leader hatte, kann sein Zählerstand kleiner sein – das verzögert höchstens einen Wechsel, erzeugt aber keinen
falschen.

**Ohne Quorum** bewertet niemand; das regelt AP-3.5 (Degraded-Modus, FA-ALR-10).

## Konsequenzen

- Positiv: Ein Wechsel je Monitor und Übergang, unabhängig von der Zahl der Nodes; Retries bleiben verständlich;
  Leaderwechsel ohne Zustandsverlust und ohne neue Raft-Befehle; Befehlsversion und FSM bleiben unverändert.
- Negativ / Risiken: Erkennung wartet bis zur Frist auf langsame Melder (bei Standard-Timeout ≈ timeout + 5 s nach
  Check-Start). Fällt der Push aus, liefert erst die Anti-Entropie (≤ 30 s); bis dahin zählt der Node als fehlend bzw.
  mit seiner vorigen Meldung. Last auf dem Leader: eine Bewertung je Monitor und Intervall im Speicher. Ein Status kann
  bei dauerhaft fehlenden Meldungen lange DOWN bleiben – sichtbar über die Selbstüberwachung (AP-3.6).
- Folgearbeiten: AP-3.2 setzt den Melder-Filter (Selbstgesundheit). AP-3.5 nutzt dieselbe Slot-Bewertung für die
  Neubewertung nach Quorum-Rückkehr (FA-UPT-07). Die meldenden Nodes je Wechsel brauchen die Alerts für FA-ALR-11
  (AP-3.5). Tests: Tabellentest aller Regel-Kombinationen (FA-CON-01/02/05), Slot-Zuordnung inkl. Spät-Ankunft,
  Neuaufbau nach Leaderwechsel, Integrationstest 3 Voter (Ziel für einen Node gesperrt → PARTIAL, für alle → genau ein
  DOWN).

## Approval

| Datum      | Status   | Durch    | Anmerkung                                    |
|------------|----------|----------|----------------------------------------------|
| 2026-11-16 | Proposed | Claude   |                                              |
| 2026-11-16 | Accepted | Projektinhaber | im Chat angenommen, eingetragen durch Claude |
