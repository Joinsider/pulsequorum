# ADR 0033: Lückenlose Übergabe vom GapJob an den Live-Aggregator

| | |
|---|---|
| **Status** | Accepted |
| **Datum** | 2026-11-18 |
| **Entscheider** | Projektinhaber |
| **Bezug** | FA-UPT-07, FA-ALR-10, NFA-REL-03; ADR 0018 (Befehlsstufen), ADR 0021 (Bewertung auf dem Leader, Rebuild), ADR 0024 Abschnitt 7 (Neubewertung von Lücken), ADR 0025 Abschnitt 5 (Quorum-Alert) |

## Kontext

`TestDegradedModeAlertsAndCorrects` (`internal/clusterhost/degraded_cluster_test.go`) schlägt lokal etwa einmal in acht Läufen fehl, zuletzt in der CI von PR #28. Der Judge urteilt dann `false_alarm` statt `confirmed`.

### Geprüfter Verdacht: `last_issued_at` (nicht belegt)

Der Verdacht lautete: Jeder angewandte Befehl setzt `last_issued_at` (`recordIssuedAt`). Ein `ReportDegradedAlerts` kurz vor dem ersten Lauf des GapJobs verdeckt die Lücke, oder `gapStart` liegt nach dem degradierten DOWN.

Geprüft wurde das mit temporären Debug-Logs in GapJob, Judge und FSM-`Apply` und dem Test in einer Schleife. Der Fehlschlag wurde mitgeschnitten, das Log liegt den Session-Notizen bei. Ergebnis:

- Die Lücke wird erkannt. Der letzte Befehl war beim ersten Lauf des GapJobs 14,2 s alt (`gapMinAge` = 5 s), `suspects=true`.
- `gapStart` ist der letzte Befehl vor dem Ausfall. Er liegt vor dem degradierten DOWN.
- In allen Läufen wurde der verbliebene Voter wieder Leader. Als Leader nimmt sein GapJob keine neuen Proben, `ReportDegradedAlerts` kommt danach.

Der Verdacht erklärt den Fehlschlag also nicht. Ein Restrisiko bleibt (siehe Frage 2).

### Tatsächlicher Mechanismus: ein Übergabefenster, das niemand bewertet

Der GapJob bewertet je Monitor die Slots, deren Beginn vor `liveStart = jetzt_G − (L+1)·I` liegt. Dabei ist `L` das Rückschaufenster in Slots (`max(retries+1, recovery)+2`) und `I` das Intervall. Den Rest überlässt er dem Live-Aggregator (`reconcile.go`, `reevaluate`).

Der Live-Aggregator wertet erst nach `gap.Done()` und der `LeaderGrace` aus. Sein Rebuild beginnt bei `max(cur.SinceMs, start(slotOf(jetzt_A) − L))` (`aggregator.go`, `rebuild`). Dabei gilt `jetzt_A ≥ jetzt_G + LeaderGrace + δ`. `δ` ist die restliche Laufzeit des GapJobs nach diesem Monitor plus ein Aggregator-Takt.

Die Slots dazwischen bewertet niemand. Beginnt das DOWN in diesem Fenster, findet der GapJob nichts (`changes=[]`). Der Rebuild startet dann beim aktuellen Status UP und setzt DOWN erst auf den ersten Slot, den er nachspielt. Das DOWN in der Historie liegt damit zu spät.

Der Mitschnitt des Fehlschlags (Zeiten relativ zum Lauf des GapJobs, t = 0):

| Zeitpunkt | Ereignis |
|---|---|
| −14,2 s | letzter Befehl vor dem Ausfall (`gapStart`) |
| −4,0 s | `liveStart` des GapJobs; bis hier: keine Änderung |
| −2,14 s | DOWN laut degradiertem Absender (`since` des unbestätigten Alerts) |
| −0,14 s | DOWN laut Live-Rebuild (`since` in `monitor_status_history`) |

Der Judge sucht im Fenster `[since − 2·I, since + (retries+2)·I)` = `[−4,14 s, −0,14 s)`. Das DOWN bei −0,14 s liegt genau auf der offenen oberen Grenze (`StatusHistory` liest `[from, to)`). Das Urteil lautet `false_alarm`.

Im erfolgreichen Lauf begann das DOWN 0,2 s vor `liveStart`. Der GapJob hat es selbst gefunden und auf den richtigen Slot gesetzt.

### Warum der Test so oft trifft

GapJob und Judge rechnen das Intervall in echten Sekunden (`time.Duration(m.IntervalSeconds) * time.Second` und `IntervalSeconds*1000`). Der Aggregator rechnet in `Unit`, im Test 500 ms.

Im Test endet das Fenster des GapJobs also 4 s zurück, der Rebuild beginnt aber nur 1,5 s zurück. Das Fenster ist damit mindestens 2,5 s + `LeaderGrace` + δ breit. Der Judge toleriert 2 s. Ob der Test fehlschlägt, hängt davon ab, wie lange der Neustart nach dem DOWN dauert.

### Wirkung in Produktion

In Produktion gelten `Unit` = 1 s, `LeaderGrace` = 10 s und `I` ≥ 20 s. Nach einer Lücke verliert jede Übernahme `slotOf(jetzt_A) − slotOf(jetzt_G)` Slots je Monitor, meist null oder einen. Die Wahrscheinlichkeit für einen verlorenen Slot liegt bei etwa `(LeaderGrace + δ)/I`, bei 20 s also rund 50 %, bei 60 s rund 17 %.

δ wächst mit der Zahl der Monitore, denn `liveStart` wird je Monitor vor den übrigen Monitoren berechnet. Die Folgen:

- Ein Wechsel in einem verlorenen Slot steht bis zu einem Slot (bei großem δ mehr) zu spät in der Historie (FA-UPT-07).
- Ein kurzer Ausfall, der ganz in die verlorenen Slots fällt, fehlt in der Historie.
- Der Judge toleriert `(retries+2)·I`, urteilt in Produktion also meist richtig. Bei großem δ oder höherer `LeaderGrace` kann er aber `false_alarm` urteilen.

### Nebenbefund: `GapOptions.Degraded` im Test

Der Test setzt `GapOptions.Degraded` nicht, `cmd/uptimecluster/main.go` schon. Damit bleibt `afterDegraded` im Test `false`, und `announceRestored` (ADR 0025, Abschnitt 5) läuft nie. Auf das Urteil hat das keinen Einfluss. Der Test prüft aber einen Pfad der Produktion nicht mit.

## Optionen

### Frage 1: Übergabe zwischen GapJob und Live-Aggregator

**1A Übergabepunkt im Prozess.** Der GapJob meldet dem Aggregator je Monitor das Ende seines Fensters, z.B. `HandOver(tenantID, monitorID, toMs)` am Aggregator. Der Aggregator ist bereits sein `Reevaluator`. Beim nächsten Rebuild in derselben Leaderschaft beginnt der Aggregator bei `max(cur.SinceMs, min(Boden, Übergabepunkt))` und spielt von dort mit denselben Regeln nach. Der Übergabepunkt gilt einmal und verfällt mit der Leaderschaft (`dropAll`). Fehlt er, bleibt alles wie heute. ← empfohlen

- Vorteile: Schließt das Fenster genau, unabhängig von `LeaderGrace`, δ und `Unit`. Keine Änderung an FSM, Proto, Fingerprint oder Befehlsstufe.
- Nachteile: Eine neue Kopplung zwischen GapJob und Aggregator. Der erste Rebuild spielt `(LeaderGrace + δ)/I` Slots mehr nach, das ist billig.

**1B GapJob schätzt den Start des Aggregators.** `liveStart` wird aus `jetzt + LeaderGrace + Reserve` berechnet.

- Vorteile: Nur eine Stelle ändert sich.
- Nachteile: δ ist unbekannt (Zahl der Monitore, Raft-Latenz). Eine zu kurze Reserve lässt das Fenster offen. Eine zu lange bewertet Slots, deren Heartbeats noch kommen können. Das ist fragil.

**1C Rebuild schaut ab Beginn der eigenen Leaderschaft zurück.** Der Boden des ersten Rebuilds bezieht sich auf `leaderSince` statt auf `jetzt`, und der GapJob nutzt dieselbe Rechnung.

- Vorteile: Entfernt den Anteil der `LeaderGrace`.
- Nachteile: Der Anteil δ bleibt. Die Lücke wird nur kleiner, nicht geschlossen.

**1D Judge toleranter machen.** Das Fenster wird um `LeaderGrace + I` erweitert.

- Vorteile: Der Test wird stabil.
- Nachteile: Verdeckt nur das Symptom. Die Historie bleibt falsch (FA-UPT-07), und echte Fehlalarme werden eher bestätigt. Abgelehnt.

### Frage 2: Referenzzeit der Lücke (der ursprüngliche Verdacht)

Jeder Befehl verschiebt `last_issued_at`. Das ist unschädlich, solange der erste Leader nach der Rückkehr des Quorums seine letzte Probe vor dieser Rückkehr genommen hat.

Ein Risikofenster gibt es nur, wenn ein Node Leader wird, bevor `GapJob.Run` seine erste Probe nimmt, und vorher schon ein Befehl festgeschrieben wurde. Im Test startet der Stack nach `launch`. In Produktion startet `main.go` `gapJob.Run` nach `clusterhost.Start`. Beobachtet wurde das nicht.

**2A Erste Probe vor dem Raft-Start.** Der GapJob nimmt seine erste Probe, bevor der Node Raft startet. In `main.go` liegt dann `NewGapJob` vor `clusterhost.Start` oder eine explizite `Sample`-Methode davor, im Test analog. Keine Änderung der FSM-Semantik. ← empfohlen

- Vorteile: Schließt das Fenster ohne neue Befehlsstufe.
- Nachteile: Die Reihenfolge in `main.go` wird zur Bedingung. Ein Test muss sie festhalten.

**2B Nur Bewertungsbefehle verschieben die Referenzzeit.** `recordIssuedAt` schreibt nur bei `SetMonitorState` und `RecordMonitorHistory`. Degraded-, Report-, Register- und Verwaltungsbefehle zählen nicht mehr.

- Vorteile: Robust gegen jede Reihenfolge der Befehle nach der Rückkehr.
- Nachteile:
  - Die Semantik von `Apply` ändert sich für alle Payloads. Nach ADR 0018 braucht das eine neue Befehlsstufe (nächste freie Stufe nach 0032), und `recordIssuedAt` muss die Stufe lesen: darunter wie bisher, ab der Stufe nur Bewertungsbefehle.
  - `fsm_meta.last_issued_at_ms` steht im Fingerprint. Alle Nodes müssen deshalb am selben Index umschalten, und das leistet das Gate über die Stufe.
  - Verliert ein Leader seine Rolle mitten im GapJob, verschiebt der erste korrigierte Monitor die Referenzzeit. Die übrigen Monitore bleiben dann unkorrigiert.

**2C Replizierter Wasserstand.** Ein neuer Befehl, z.B. `MarkEvaluated{through_ms}`, schlägt der Leader erst nach `gap.Done()` und dann periodisch vor. Die Lücke beginnt am Wasserstand, und Proben entfallen ganz.

- Vorteile: Die genaueste Aussage („bis hier hat ein Leader mit Quorum live bewertet“).
- Nachteile:
  - Neuer Proto-Befehl, neue Befehlsstufe (ADR 0018) und ein neuer Fingerprint-Schlüssel.
  - Laufender Raft-Verkehr.
  - Ausnahme im Audit-Log nötig: ADR 0031 schreibt ab Stufe 9 jeden Befehl mit.

### Frage 3: Maßstab im Test

**3A `Unit` auch für GapJob und Judge.** `GapOptions.Unit` und `JudgeOptions.Unit` (Standard 1 s) werden für alle Intervallrechnungen genutzt. Der Test übergibt `consensusUnit` und setzt `GapOptions.Degraded` wie `main.go`. ← empfohlen

- Vorteile: Test und Produktion rechnen im selben Maßstab. Der Test prüft den Quorum-Restored-Pfad mit.
- Nachteile: keine. In Produktion ändert sich nichts.

**3B Unverändert lassen.** Nur 1A umsetzen.

- Nachteile: Der Judge des Tests misst weiter in Sekunden, der Aggregator in halben Sekunden. Weitere Grenzfälle bleiben möglich.

## Entscheidung

Angenommen: **1A + 2A + 3A.**

- 1A behebt die belegte Ursache an der Stelle, an der sie entsteht, ohne Vertrag und ohne FSM-Änderung.
- 2A schließt das theoretische Fenster des ursprünglichen Verdachts mit derselben Eigenschaft.
- 3A macht den Integrationstest aussagekräftig.

2B oder 2C erst, wenn ein Lauf zeigt, dass die Referenzzeit selbst falsch ist.

## Konsequenzen

- Positiv:
  - Nach einer Lücke bewertet genau eine Stelle jeden Slot: der GapJob bis zum Übergabepunkt, der Live-Aggregator ab dort. Die Historie nach einem Totalausfall stimmt slotgenau (FA-UPT-07), und der Judge urteilt auf ihr.
  - Mit 1A + 2A + 3A ändern sich FSM, Proto und OpenAPI nicht. Der Fingerprint bleibt gleich, und es gibt keine neue Befehlsstufe nach ADR 0018. Ein Rolling Update ist ohne Gate möglich, denn die Übergabe ist lokal im Leader.
- Negativ / Risiken:
  - GapJob und Aggregator werden enger gekoppelt. Ein Test muss den Übergabepunkt über eine Leaderschaft hinweg prüfen (verfällt bei `dropAll`).
  - Bei 2B oder 2C kämen eine neue Befehlsstufe, ein geänderter Fingerprint-Inhalt und bei 2C ein neuer Befehl samt Audit-Ausnahme hinzu.
- Folgearbeiten:
  - Den reproduzierenden Test `TestGapHandOverLeavesNoRoundUnevaluated` (`internal/aggregator/handover_test.go`, heute nur mit `UC_REPRO_ADR0033=1` aktiv) dauerhaft einschalten.
  - Abnahme: `TestDegradedModeAlertsAndCorrects` lokal mit `-count=20` grün.
  - In ADR 0024 Abschnitt 7 auf diesen ADR verweisen (das „Rückschaufenster des Aggregators“ ist jetzt der Übergabepunkt).

## Approval

| Datum | Status | Durch | Anmerkung |
|-------|--------|-------|-----------|
| 2026-11-18 | Proposed | Claude | |
| 2026-11-18 | Accepted | Projektinhaber | 1A, 2A und 3A angenommen (im Chat) |
