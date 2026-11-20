# ADR 0022: Node-Selbstgesundheit (Konnektivität) und Ausschluss aus der Bewertung

|                 |                                                                                                                                          |
|-----------------|------------------------------------------------------------------------------------------------------------------------------------------|
| **Status**      | Accepted                                                                                                                                 |
| **Datum**       | 2026-11-16                                                                                                                               |
| **Entscheider** | Projektinhaber                                                                                                                                 |
| **Bezug**       | FA-CON-04, AS-05, PF-19, ADR 0017 (Node-Gesundheit), 0018 (Befehlsversionen), 0020 (`GetStatus`-Abfrage), 0021 (Melder-Filter `Exclude`) |

## Kontext

Ein Node mit gestörtem Egress sieht alle Ziele als DOWN (PF-19). FA-CON-04 verlangt: Schlagen konfigurierbare
Referenzziele fehl **oder** liegt der Anteil fehlgeschlagener Checks über einem Schwellwert, während die Peers gesund
sind, wird der Node `unhealthy` und aus der Bewertung genommen. AS-05 erwartet dabei keinen DOWN-Alarm und einen
Systemalert „Node unhealthy“.

Bestand:

- `healthy` im Node-Register (ADR 0017) bedeutet nur „für den Leader per Raft erreichbar“. Ein Node ohne Internet, aber
  mit Clusternetz, bleibt gesund.
- Der Leader fragt alle Peers alle 15 s per `GetStatus` ab (Uhrversatz, ADR 0020).
- Der Leader hat die Heartbeats aller Nodes (ADR 0021), und der Aggregator hat den Filter `Exclude`.

## Optionen

### Option A: Jeder Node bewertet sich selbst und schreibt das Ergebnis per Raft

- Vorteile: Einfach, der Node kennt seine Referenzziele.
- Nachteile: Ein Node ohne Egress sieht nicht, ob die Peers dasselbe Problem haben. Ist das Ziel wirklich weg, schließen
  sich alle Nodes selbst aus. Schreibvorgänge von jedem Node.

### Option B: Der Leader entscheidet nur aus den Heartbeats

- Vorteile: Kein neuer Datenweg, Vergleich mit den Peers direkt möglich.
- Nachteile: Keine Referenzziele (verlangt FA-CON-04), langsamer bei wenigen Monitoren.

### Option C: Der Leader entscheidet aus beiden Signalen und schreibt den Wechsel per Raft

- Jeder Node prüft seine Referenzziele lokal und meldet das Ergebnis im `GetStatus`. Der Leader kombiniert das mit der
  Abweichungsquote aus den Heartbeats und schreibt nur Wechsel ins Log.
- Vorteile: Vergleich mit den Peers für beide Signale, eine Entscheidungsstelle wie bei `SetNodeHealth`, sichtbar in
  Register, Admin-API und Systemalert.
- Nachteile: Neuer Raft-Befehl (Befehlsversion 3) und neue Spalte.

## Entscheidung

**Option C.** Die Konnektivität ist eine eigene Dimension neben `healthy`; sie ändert nichts an der Raft-Gesundheit.

**Referenzziele.** `SELF_CHECK_TARGETS` (kommagetrennt, `host:port` für TCP oder `https://…` für HTTP-HEAD). Standard:
leer, dann ist das Signal aus. Jeder Node prüft sie alle 30 s über den normalen Checker-Dialer (Timeout 5 s). Signal
„Referenz fehlgeschlagen“: **alle** Referenzziele schlagen fehl. `GetStatusResponse` erhält die additiven Felder
`self_check_configured`, `self_check_failed` und `self_check_at_ms`. Ältere Nodes liefern nichts, das zählt als „kein
Signal“.

**Abweichungsquote.** Die Quote berechnet der Leader aus den Heartbeats, die er ohnehin hält. Gezählt werden die Slots
der letzten 5 min, in denen der Node gemeldet hat und mindestens ein anderer Node für denselben Monitor UP oder DEGRADED
meldete. Die Quote ist der Anteil dieser Slots, in denen der Node DOWN meldete. Signal „Abweichung“: Quote ≥
`SELF_HEALTH_FAIL_RATIO` (Standard 0,5) bei mindestens 10 solchen Slots.

**Bedingung „gesunde Peers“.** Ein Node wird nur ausgeschlossen, wenn mindestens die Hälfte der übrigen registrierten
Voter weder ausgeschlossen ist noch selbst ein Signal zeigt. Zeigen mehr Nodes ein Signal, liegt das Problem
wahrscheinlich beim Ziel bzw. im Internet, und niemand wird ausgeschlossen. Ein einzelner Voter wird daher nie
ausgeschlossen.

**Hysterese.** Der Leader bewertet alle 15 s (mit der `GetStatus`-Runde):

- `impaired` nach 2 aufeinanderfolgenden Bewertungen mit Signal (≈ 30 s).
- Wieder `ok` nach 2 min ohne Signal.
- Nach einem Leaderwechsel wird erst nach einer vollständigen Runde neu bewertet. Bis dahin gilt der Zustand aus dem
  Register.

**Datenmodell.** Neuer Befehl `SetNodeConnectivity{node_id, impaired, reason}`, nur vom Leader und nur bei Wechseln
(Befehlsversion 3, Feature-Gate nach ADR 0018; vorher wird nichts geschrieben, der Ausschluss gilt dann nur im Speicher
des Leaders). Die neuen Spalten `connectivity_impaired`, `connectivity_reason` und `connectivity_changed_ms` in `nodes`
kommen per additiver Migration; sie gehen in den Fingerprint ein. Die Admin-API und die Cluster-Ansicht zeigen den
Zustand mit Grund an (`reference` | `deviation`).

**Wirkung.**

- Der Aggregator schließt `impaired`-Nodes über `Exclude` aus (ADR 0021).
- `count`-Zuweisungen überspringen sie wie ungesunde Nodes; `all`/`nodes` prüfen weiter und liefern Daten für die
  Rückkehr.
- Für den Systemalert „Node unhealthy“ (AS-05) wird ein Ereignis erzeugt; der Versand kommt mit AP-3.6.

## Konsequenzen

- Positiv: Ein Node ohne Egress erzeugt keine Fehlalarme; ein echter Ausfall des Ziels schließt niemanden aus. Beide
  Signale aus FA-CON-04 kommen ohne zusätzliche Raft-Last je Check aus.
- Negativ / Risiken: Bis zum Ausschluss vergehen ≈ 30 s, mit Abweichungsquote auch mehr (≥ 10 Slots). In dieser Zeit
  kann bei `any` ein falsches DOWN entstehen; bei `majority` bleibt es bei PARTIAL. Mit 2 Votern ist die Bedingung
  „gesunde Peers“ schwach (1 von 1); dieser Aufbau wird ohnehin gewarnt. Neuer Befehl und neue Migration im
  Rolling-Update-Pfad.
- Folgearbeiten: Systemalert und Metriken (AP-3.6). Abnahme AS-05 im Chaos-Harness (AP-3.7, iptables-Egress-Sperre nur
  für Ziele). Referenzziele in der Betriebsdoku beschreiben (Empfehlung: 2–3 unabhängige Ziele).

## Approval

| Datum      | Status   | Durch    | Anmerkung                                    |
|------------|----------|----------|----------------------------------------------|
| 2026-11-16 | Proposed | Claude   |                                              |
| 2026-11-16 | Accepted | Projektinhaber | im Chat angenommen, eingetragen durch Claude |
