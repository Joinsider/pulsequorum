# ADR 0024: Degraded-Modus, Absender-Sicht und Korrektur nach Quorum-Rückkehr

|                 |                                                                                                                                                                                              |
|-----------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **Status**      | Accepted                                                                                                                                                                                     |
| **Datum**       | 2026-11-16                                                                                                                                                                                   |
| **Entscheider** | Projektinhaber                                                                                                                                                                                     |
| **Bezug**       | FA-ALR-10/11, FA-UPT-07, NFA-REL-06, AS-04, PF-28, Konzept 9.3 (E8), ADR 0014 (Outbox), 0018 (Befehlsversionen), 0021 (Bewertung auf dem Leader), 0022 (Selbstgesundheit), 0023 (Drosselung) |

## Kontext

Ohne Quorum gibt es keinen Leader. Seit ADR 0021 bewertet und alarmiert nur der Leader, eine Partition ohne Quorum ist
also stumm. Gefordert ist:

- **FA-ALR-10 (E8):** Je Minderheitspartition versendet der erreichbare Voter mit der kleinsten ID die Alerts,
  gekennzeichnet mit „⚠ unbestätigt – Cluster degraded (x/n Nodes)“. Er führt ein lokales Versandprotokoll. Nach der
  Rückkehr des Quorums folgen ein Abgleich und gegebenenfalls eine Korrektur („Fehlalarm“ bzw. „bestätigt“).
- **FA-ALR-11:** Jeder Alert enthält die Sicht des Absenders, also die Zahl der erreichbaren Voter und die meldenden
  Nodes.
- **FA-UPT-07:** Unbestätigte Statuswechsel aus dem Degraded-Modus werden nach der Rückkehr des Quorums neu bewertet.
- **NFA-REL-06:** Bei Quorumverlust wird weiter gemessen, gelesen und unbestätigt alarmiert. Nach der Rückkehr
  konvergiert der Cluster ohne manuellen Eingriff.
- **AS-04:** Bei einer Partition 2|1 mit ausgefallenem Ziel alarmiert die Mehrheit bestätigt und die Minderheit
  unbestätigt. Nach der Heilung folgt eine Korrektur oder Bestätigung, und die Daten sind identisch.

Was schon da ist:

- Ohne Quorum kann jeder Voter die Klasse A lesen: Monitore, Kanäle, Wartungsfenster, Status. Mit dem lokalen Keyring
  kann er auch Kanalgeheimnisse entschlüsseln.
- Heartbeats (Klasse B) werden weiter zwischen den erreichbaren Nodes repliziert.
- Der Aggregator (ADR 0021) bewertet Slots über eine austauschbare `Cluster`-Schnittstelle.
- `SetMonitorState.confirmed` existiert schon und ist bisher immer `true`.
- `monitor_status_history` ist die Quelle der Konsens-Uptime.
- `GetStatus` zwischen Votern liefert `leader_id` und das Selbsttest-Ergebnis.

Zwei Fälle sind zu unterscheiden:

- **Teilpartition (z.B. 2|1):** Die Mehrheit hat weiter einen Leader. Ihre Historie hat keine Lücke. Nur die Minderheit
  ist im Degraded-Modus.
- **Vollständiger Verlust (z.B. 1|1|1 oder 2 von 3 Nodes aus):** Niemand hat einen Leader, und für die Dauer des
  Ausfalls entsteht keine bestätigte Historie.

## Optionen

### Option A: Minderheit bleibt stumm

- Vorteile: Keine Duplikate, nichts zu korrigieren.
- Nachteile: Widerspricht E8 und FA-ALR-10. Fallen zwei von drei Nodes aus, gehen echte Ausfälle unbemerkt durch.

### Option B: Lokaler Absender, lokales Protokoll, Abgleich über Raft nach der Rückkehr

- Der Absender bewertet mit dem bestehenden Aggregator. Statt per Raft schreibt er in ein lokales Protokoll und
  versendet direkt.
- Nach der Rückkehr meldet er sein Protokoll per Raft. Der Leader fällt je unbestätigtem Alert ein Urteil und schließt
  Lücken in der Historie aus den Heartbeats.
- Vorteile: Nutzt Aggregator, Sender und Drosselung wieder. Urteil und Korrektur sind replizierter Zustand. Die FSM
  bleibt deterministisch, weil das Urteil im Befehl steht.
- Nachteile: Zwei neue Befehle, eine erweiterte Meldung, Befehlsversion 5. Bei asymmetrischen Partitionen sind mehrere
  Absender möglich.

### Option C: Schatten-FSM je Partition, die nach der Heilung zusammengeführt wird

- Vorteile: Volle Historie auch während der Partition.
- Nachteile: Das ist Konfliktauflösung zwischen divergenten Zuständen, also genau das, was Raft vermeidet. Hoher Aufwand
  und schwer zu testen.

## Entscheidung

**Option B.**

**1. Erkennung.**

- Ein Voter ist **degraded**, wenn er seit `DEGRADED_AFTER` (Standard 15 s) keinen Leader kennt.
- Der Zustand endet, sobald ein Leader bekannt ist und der Node dessen Commit-Index angewandt hat.
- Probes (P5) haben keinen Degraded-Modus.

**2. Erreichbarkeit und Absenderwahl.**

- Ein degradierter Voter fragt alle 5 s jeden registrierten Voter aus dem lokalen Register per `GetStatus` ab (Timeout 2
  s, mTLS wie bisher).
- **Erreichbar** ist ein Peer, der in den letzten 15 s geantwortet hat.
- **Partition** ist der Node selbst plus die erreichbaren Peers, die ebenfalls keinen Leader melden (`leader_id` leer).
- **Absender** ist der Node mit der kleinsten ID in der Partition. Nodes mit fehlgeschlagenem Selbsttest
  (`self_check_failed`, ADR 0022) werden übersprungen, außer alle haben ihn nicht bestanden.
- Jeder Node rechnet die Wahl selbst aus; es gibt keine Abstimmung. Sehen sich zwei Nodes asymmetrisch, sind zwei
  Absender möglich. Die Absender-Sicht (Punkt 5) macht das für Empfänger einordenbar.

**3. Bewertung und Versand im Degraded-Modus.**

- Nur der Absender bewertet. Er nutzt den Aggregator aus ADR 0021 mit denselben Slot-Regeln, Retries und
  Recovery-Schwellen und mit `Exclude` aus ADR 0022.
- Ausgangspunkt ist der zuletzt bestätigte Status aus dem lokalen Zustand.
- Melder sind nur die Nodes der Partition, deren Heartbeats er ohnehin per Replikation erhält.
- Statt `SetMonitorState` per Raft legt er einen Eintrag im **lokalen Protokoll** an und versendet ihn direkt. Dafür
  nutzt er die Kanäle, Ereignisfilter und Wartungsfenster aus dem lokalen Zustand sowie Sender, Rate-Limit und Bündelung
  aus ADR 0023.
- Monitore, die im bestätigten Zustand `flapping` sind, bleiben unterdrückt. Neues Flapping erkennt der Degraded-Modus
  nicht.
- Kennzeichnung: Titel mit dem Präfix „⚠ unbestätigt – Cluster degraded (x/n Nodes)“, wobei x die Größe der Partition
  und n die Zahl der registrierten Voter ist. In der Nutzlast steht `confirmed: false`. Die `alertId` hat einen eigenen
  Namensraum (`degraded:<node>:<monitor>:<since>:<status>`).
- **Schutz vor Alarmflut (PF-28):**
    - Schlägt der eigene Selbsttest fehl, versendet der Absender keine Monitor-Alerts.
    - Wechseln innerhalb eines Bündelfensters mehr als 50 % der Monitore eines Mandanten (mindestens 5) auf DOWN, geht
      statt der Einzelalerts eine Sammelnachricht raus („node-c sieht n Monitore DOWN, vermutlich eigenes Netzproblem“).
      Die Einträge stehen trotzdem im Protokoll.
- Zustellung: Wiederholung mit dem Backoff aus ADR 0014, nur im Speicher und nur solange der Node Absender bleibt. *At
  least once* gilt im Degraded-Modus nicht.

**4. Lokales Protokoll.**

- Neue node-lokale Tabelle `degraded_alerts` in `metrics.db`. Sie wird nicht repliziert und geht nicht in den
  Fingerprint ein.
- Spalten: `id`, `tenant_id`, `monitor_id`, `channel_id`, `status`, `since_ms`, `sent_ms`, `outcome`, `reachable`,
  `total`, `reporters` (JSON) und `reported`.
- Aufbewahrung 30 Tage.
- Ein Absender, der seine Rolle verliert, behält seine Einträge. Ein neuer Absender startet wieder vom bestätigten
  Status. Ein schon gemeldeter Wechsel kann dabei ein zweites Mal gemeldet werden. Dieses Risiko nehmen wir in Kauf.

**5. Absender-Sicht in jedem Alert (FA-ALR-11, ab Befehlsversion 5).**

- `AlertPayload` erhält `sender: {node_id, reachable_voters, total_voters, reporters[], confirmed}`. Shoutrrr-Texte
  enden mit einer Zeile wie „Sicht: node-a · 3/3 Voter · gemeldet von node-a, node-b, node-c“.
- Bei bestätigten Alerts zählt `reachable_voters` die gesunden Voter im Register (ADR 0017). `reporters` sind die Nodes,
  die im entscheidenden Slot gemeldet haben.
- Dafür erhält `SetMonitorState` das additive Feld `reporters` (vom Leader gesetzt). Die FSM übernimmt es unverändert in
  den Outbox-Eintrag.
- Unter Level 5 fehlt `reporters`; die Sicht enthält dann nur Absender und Voter-Zahl.

**6. Abgleich nach der Rückkehr (FA-ALR-10, ab Befehlsversion 5).**

- Sobald ein Node nicht mehr degraded ist, sendet er seine nicht gemeldeten Einträge per Write-Forwarding als
  `ReportDegradedAlerts{node_id, entries[]}`. Er sendet sie in Blöcken zu je 100 und setzt danach `reported`.
- Die FSM speichert sie in der neuen replizierten Tabelle `unconfirmed_alerts`, idempotent über `(node_id, local_id)`.
- Der Leader fällt das Urteil, sobald der betroffene Zeitraum bestätigte Historie hat, also nach Punkt 7.
    - **Bestätigt**, wenn `monitor_status_history` für den Monitor im Fenster
      `[since − 2·Intervall, since + (retries + 2)·Intervall]` denselben Status zeigt.
    - Sonst **Fehlalarm**.
    - Einträge desselben Monitors und Kanals werden als Episode beurteilt: Ein Fehlalarm für DOWN macht das folgende UP
      mit falsch.
- Er schlägt `ResolveUnconfirmedAlerts{entries[]: {id, verdict}}` vor. Die FSM speichert das Urteil und legt
  Outbox-Einträge an. Diese Einträge gehen an denselben Kanal und laufen über Drosselung und Bündelung.
    - **Fehlalarm** wird immer gemeldet: „Fehlalarm: X war nicht DOWN (Sicht von node-c, 1/3 Voter)“.
    - **Bestätigt** wird nur gemeldet, wenn derselbe Kanal für diesen Wechsel noch keinen bestätigten Alert bekommen
      hat. Bei 2|1 hat die Mehrheit den Wechsel meist schon gemeldet.
- Einträge, deren Zeitraum älter als die Rohdaten-Retention ist, werden ohne Nachricht als „nicht prüfbar“ geschlossen.

**7. Neubewertung von Lücken (FA-UPT-07, ab Befehlsversion 5).**

- **Teilpartition:** Die Historie der Mehrheit bleibt, wie sie ist, denn sie wurde mit Quorum entschieden. Heartbeats
  der Minderheit, die später eintreffen, ändern sie nicht.
- **Vollständiger Verlust:** Ein neuer Leader prüft nach der Grace-Zeit, ob seit dem `issued_at_ms` des letzten
  angewandten Befehls mehr Zeit vergangen ist als das Rückschaufenster des Aggregators. Ist das so, ist das eine Lücke.
    - Zuerst wartet er auf den Heartbeat-Nachlauf: Anti-Entropy mit allen erreichbaren Peers abgeschlossen, höchstens 2
      min.
    - Dann bewertet er die Slots der Lücke (höchstens 24 h) mit denselben Regeln nach.
    - Das Ergebnis schreibt er je Monitor als `RecordMonitorHistory{monitor_id, from_ms, to_ms, entries[]}`. Die FSM
      ersetzt die Historie im Intervall und legt geschlossene Incidents an, markiert mit `reevaluated`. Sie legt
      **keine** Outbox-Einträge an.
    - Den aktuellen Status setzt der Leader danach normal per `SetMonitorState`. Ein noch andauernder Ausfall wird also
      bestätigt alarmiert.
- Das `issued_at_ms` des letzten Befehls fehlt bisher als Wert. Es kommt in `fsm_meta` und geht in den Fingerprint ein.

**Datenmodell und Verträge.**

- `metrics.db`: Tabelle `degraded_alerts`, lokal.
- `state.db`: Tabelle `unconfirmed_alerts` und `fsm_meta.last_issued_at_ms`; Spalte `incidents.reevaluated`.
- Proto:
    - `SetMonitorState.reporters`
    - neue Befehle `ReportDegradedAlerts`, `ResolveUnconfirmedAlerts` und `RecordMonitorHistory`
    - `GetStatusResponse` bleibt unverändert (`leader_id` und Selbsttest reichen)
- API (additiv):
    - `GET /api/v1/health` erhält `degraded`, `degraded_since` und `partition` (Node-IDs).
    - Incidents erhalten `reevaluated`.
    - Die Cluster-Ansicht zeigt den Degraded-Zustand und offene unbestätigte Alerts.
- Konfiguration: `DEGRADED_AFTER` (Standard 15 s, `0` schaltet den Versand im Degraded-Modus ab).
- Feature-Gate: Befehlsversion 5. Darunter versendet der Degraded-Absender trotzdem, weil er die FSM nicht braucht.
  Abgleich, Korrektur, Neubewertung und `reporters` fehlen dann.

## Konsequenzen

- **Positiv:**
    - Auch ohne Quorum gehen Alarme raus (E8).
    - Empfänger sehen bei jedem Alert, aus welcher Sicht er kommt.
    - Fehlalarme der Minderheit werden nachträglich als solche gemeldet.
    - Die Uptime hat nach einem vollständigen Ausfall keine Lücke, sondern eine nachbewertete Historie.
    - Aggregator, Sender und Drosselung bleiben eine Implementierung.
- **Negativ / Risiken:**
    - Bei 2|1 bekommen Empfänger einen Wechsel zweimal, einmal bestätigt und einmal unbestätigt. Das ist gewollt und
      gekennzeichnet.
    - Asymmetrische Partitionen und Wechsel des Absenders können zusätzliche Duplikate erzeugen.
    - Ist die Minderheit vom Cluster getrennt, hat aber Internet, misst sie echt. Ihr „Fehlalarm“-Urteil kann dann
      falsch sein, wenn die Mehrheit ihrerseits das Ziel nicht sah. Das Urteil folgt immer der bestätigten Historie.
    - Die Nachbewertung erzeugt Incidents ohne Benachrichtigung. Ein Ausfall, der vollständig in die Lücke fällt und vom
      Degraded-Absender nicht gemeldet wurde, erscheint nur in der UI.
    - Drei neue Befehle und zwei Migrationen im Rolling-Update-Pfad.
- **Folgearbeiten:**
    - Systemalert „Quorum verloren/wiederhergestellt“ und Metriken (AP-3.6).
    - Abnahme AS-04 im Chaos-Harness (AP-3.7).
    - Tests:
        - Tabellentest Absenderwahl (inkl. Selbsttest und asymmetrischer Sicht)
        - Aggregator mit lokalem Protokoll
        - Flutschutz
        - FSM-Tabellentests für die drei Befehle, jeweils Level 4 gegen 5
        - Urteilsregel (Fenstergrenzen, Episoden)
        - Clustertest 2|1 und 1|1|1 mit Heilung und identischen Fingerprints

## Approval

| Datum      | Status   | Durch    | Anmerkung                                                           |
|------------|----------|----------|---------------------------------------------------------------------|
| 2026-11-16 | Proposed | Claude   |                                                                     |
| 2026-11-16 | Accepted | Projektinhaber | im Chat angenommen („Annehmen, umsetzen“), eingetragen durch Claude |
