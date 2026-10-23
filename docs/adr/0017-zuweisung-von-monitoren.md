# ADR 0017: Zuweisung von Monitoren an Nodes, Node-Register und Node-Gesundheit

| | |
|---|---|
| **Status** | Accepted |
| **Datum** | 2026-10-23 |
| **Entscheider** | Projektinhaber |
| **Bezug** | FA-SCH-01/02/06, FA-CHK-31, FA-CLU-07/12, E1, E11, ADR 0005, ADR 0012 A3/A9 |

## Kontext

Seit AP-2.2 gibt es Cluster mit mehreren Votern, aber jeder Voter prüft jeden Monitor (`Reconcile` ignoriert `assignment`). AP-2.5 soll die Modi aus E1 umsetzen:

- `all`: alle fähigen Nodes,
- `count: k`: k Nodes per Consistent Hashing über die **gesunden, fähigen** Nodes; fällt einer aus, übernimmt deterministisch der nächste, innerhalb von 2 Intervallen (FA-SCH-02),
- `nodes: [...]`: nur die genannten Nodes.

Dafür fehlen zwei Dinge im replizierten Zustand:

1. **Fähigkeiten (Capabilities).** Ein Node ohne funktionierendes ICMP darf keine ICMP-Monitore bekommen (FA-CHK-31). Die Fähigkeit ermittelt jeder Node beim Start selbst; die anderen Nodes kennen sie bisher nicht. Der Proto-Vertrag enthält `RegisterNode`/`NodeInfo` mit `capabilities`, die FSM wendet den Befehl aber noch nicht an.
2. **Gesundheit.** Damit `count: k` deterministisch ist, müssen alle Nodes dieselbe Menge gesunder Nodes sehen. Weicht die Sicht ab, prüfen zeitweise zu viele oder zu wenige Nodes einen Monitor.

Randbedingungen: `Apply` ist deterministisch (FA-CLU-07). Ein Binary, das eine unbekannte Payload anwendet, lehnt sie heute mit `unsupported` ab, ein neueres Binary wendet sie an: Die Zustände würden auseinanderlaufen. Feature-Gates für gemischte Versionen kommen erst mit AP-2.6.

## Optionen (Gesundheit für `count: k`)

### Option A: Leader schreibt die Gesundheit in den Raft-Zustand
- Der Leader beobachtet die Follower über Raft (`FailedHeartbeatObservation`/`ResumedHeartbeatObservation`, `LastContact` je Follower) und schreibt Zustandswechsel als Befehl `SetNodeHealth` ins Log. Alle Nodes berechnen die Zuweisung aus demselben Zustand.
- Vorteile: Alle Nodes haben dieselbe Sicht, die Zuweisung ist überall identisch. Wechsel sind im Log nachvollziehbar (Audit, Admin-UI). Kein zusätzlicher Netzverkehr.
- Nachteile: Gesundheit bedeutet „für den Leader erreichbar“, nicht „hat Internet“ (das ist FA-CON-04, P3). Schreibt nur bei Wechseln, braucht aber Hysterese gegen Flattern. Ohne Quorum ändert sich nichts mehr – dann laufen die Checks mit der letzten Zuweisung weiter, was gewollt ist.

### Option B: Jeder Node bewertet die Peers selbst
- Etwa über die Aktualität der replizierten Heartbeat-Ströme oder eigene Pings.
- Vorteile: Keine Raft-Schreibvorgänge, funktioniert ohne Quorum.
- Nachteile: Sichten weichen bei Teil-Partitionen ab, `count: k` wird dann doppelt oder gar nicht geprüft. Nicht deterministisch, schwer zu testen.

### Option C: Nur die Raft-Mitgliedschaft zählt
- Vorteile: Am einfachsten.
- Nachteile: Ein ausgefallener Voter bleibt Mitglied (FA-CLU-08: nie automatisch entfernen). Seine Monitore würden nie übernommen. Verfehlt FA-SCH-02.

## Entscheidung

**Option A**, mit folgenden Festlegungen:

**Node-Register (Raft, Datenklasse A)**
- Neue Tabelle `nodes` in `state.db`: Node-ID, Name, Rolle, Fähigkeiten, Labels, Region, Version, `max_command_version`, `healthy`, `health_changed_ms`. Teil des Snapshots.
- `RegisterNode` wird angewendet (Upsert). Jeder Voter registriert sich nach dem Start und erneut, wenn sich seine Daten ändern (Version, Fähigkeiten). Den Befehl schickt der Node selbst; als Follower leitet er ihn an den Leader weiter (AP-2.3). Eine Prüfung, dass die Node-ID zum Absender passt, gibt es nicht: Weiterleiten dürfen nur Voter, und ein Voter darf ohnehin jeden Befehl vorschlagen.
- Fähigkeiten in P2: `icmp`, wenn der ICMP-Selbsttest beim Start gelingt (FA-CHK-31). Weitere (z.B. `ipv6`) kommen mit ihren Checkern.
- Entfernen eines Nodes (`RevokeNode`) löscht auch seinen Registereintrag.

**Gesundheit**
- Neuer Befehl `SetNodeHealth{node_id, healthy}`, den nur der Leader vorschlägt, und nur bei Wechseln. Der Zeitstempel kommt wie üblich aus `issued_at_ms`.
- Ungesund: der Leader hat seit **10 s** (= 10 × `HeartbeatTimeout` im Standard) keinen erfolgreichen Kontakt zum Follower. Wieder gesund: der Follower antwortet seit **10 s** ohne Unterbrechung. Die Hysterese verhindert Flattern bei kurzen Aussetzern.
- Ein neuer Leader gleicht nach der Wahl einmal ab: Er markiert sich selbst als gesund und setzt Follower, zu denen er Kontakt hat bzw. keinen hat, nach denselben Fristen.
- Ein neu registrierter Node gilt als gesund.

**Zuweisung (Paket `scheduler`, rein funktional und deterministisch)**
- Kandidaten sind die Voter aus dem Register (Probes folgen in P5), gefiltert nach der Fähigkeit, die der Monitortyp verlangt (`checker.CapabilityRequirer`).
- `all`: alle fähigen Kandidaten, unabhängig von der Gesundheit. Ein ungesunder Node prüft weiter; ob seine Ergebnisse zählen, entscheidet die Konsens-Bewertung (P3).
- `count: k`: **Rendezvous-Hashing** (Highest Random Weight) über die gesunden, fähigen Kandidaten. Gewicht = erste 8 Byte von SHA-256(`monitor_id` + `\x00` + `node_id`); die k höchsten Gewichte gewinnen, bei Gleichstand entscheidet die Node-ID. Gibt es weniger als k gesunde Kandidaten, prüfen alle gesunden. Rendezvous statt Ring, weil es ohne virtuelle Knoten gleichmäßig verteilt und beim Ausfall eines Nodes nur dessen Monitore verschiebt.
- `nodes: [...]`: die genannten Nodes, wenn sie fähig sind, unabhängig von der Gesundheit (der Nutzer hat sie bewusst gebunden, E11).
- Jeder Node berechnet die Zuweisung bei jedem `Reconcile` aus dem lokalen `state.db` und plant nur Monitore, die ihm zugewiesen sind. Ändert sich Register oder Gesundheit, signalisiert die FSM wie bei jeder Zustandsänderung, die Zuweisung wirkt also nach dem Anwenden des Eintrags (FA-SCH-06).
- Ein Single-Voter-Cluster ohne Registereintrag (vor dem ersten `RegisterNode`) plant wie bisher alles.

**Zeitbudget für die Übernahme (FA-SCH-02)**: Erkennung 10 s + Commit + Reconcile < 15 s, danach startet der übernehmende Node den Monitor mit seinem normalen Jitter innerhalb eines Intervalls. Das liegt bei jedem Intervall ≥ 20 s innerhalb von 2 Intervallen.

**Befehlsversion**: `RegisterNode` und `SetNodeHealth` tragen `version = 2`, `CommandVersion` steigt auf 2. Ein älteres Binary hält beim ersten solchen Eintrag kontrolliert an (bestehendes Verhalten der FSM), statt still abzuweichen. Bis die Feature-Gates aus AP-2.6 da sind, müssen alle Voter auf dieselbe Version aktualisiert werden; einen Release mit gemischten Versionen gibt es vor 1.0 nicht.

**API**: Die Node-Liste (`GET /api/v1/cluster/nodes`) zeigt zusätzlich Fähigkeiten, Gesundheit und Version (optionale Felder, rückwärtskompatibel). Beim Anlegen oder Ändern eines Monitors mit `nodes: [...]` lehnt die API Node-IDs, die nicht im Register stehen, mit 422 ab.

## Konsequenzen

- Positiv: Deterministische, überall gleiche Zuweisung; Übernahme ohne manuelles Eingreifen; ICMP-Monitore landen nur auf fähigen Nodes; Gesundheitswechsel sind im Log und in der Admin-UI sichtbar.
- Negativ / Risiken: „Gesund“ heißt in P2 nur „für den Leader erreichbar“. Ein Node mit gestörtem Internetzugang bleibt gesund, bis FA-CON-04 (P3) ihn ausschließt. Schreibvorgänge im Raft-Log bei jedem Gesundheitswechsel (selten, wegen Hysterese). Ein Node, der nur vom Leader getrennt ist, gilt als ungesund, obwohl er die Ziele erreicht – seine `count`-Monitore wandern ab, `all`/`nodes` laufen weiter.
- Abgrenzung: Diese ADR legt nur fest, **wer** prüft. Wie die Ergebnisse mehrerer Nodes zu einem Status werden, bleibt Konsens-Bewertung (AP-3.1). Bis dahin bewertet weiter jeder Node seine eigenen Ergebnisse.
- Folgearbeiten: Feature-Gates (AP-2.6) prüfen künftig `max_command_version` aus dem Register; Probes im Register (P5); Zuweisung über Labels (P6); Systemalert bei ungesundem Node (AP-3.6).

## Approval

| Datum | Status | Durch | Anmerkung |
|-------|--------|-------|-----------|
| 2026-10-23 | Proposed | Claude | |
| 2026-10-23 | Accepted | Projektinhaber | im Chat bestätigt |
