# ADR 0012: Präzisierungen aus den Spikes (Raft, SQLite)

| | |
|---|---|
| **Status** | Accepted |
| **Datum** | 2026-10-19 |
| **Entscheider** | Projektinhaber |
| **Bezug** | Ergänzt (ersetzt nicht) ADR 0004, 0005, 0006 · Spikes [AP-0.14](../../spikes/raft/README.md), [AP-0.15](../../spikes/sqlite/README.md) · FA-CLU-03/04/06/08/10, FA-NODE-05, FA-REP-01/03/04, PF-01, PF-04, PF-12, PF-24 |

## Kontext

Die Spikes AP-0.14 (Raft mit 3 Votern) und AP-0.15 (SQLite-Benchmark) haben die akzeptierten ADRs 0004–0006 im Kern bestätigt:
- Bootstrap-Erkennung, Nonvoter-Join, Write-Forwarding, Request-Dedup, Snapshot/Restore und Heartbeat-Replikation mit Inkarnation funktionieren wie geplant.
- Failover nach `kill -9` des Leaders dauert 1,8–2,5 s, mit Leadership Transfer 0,2–0,4 s (NFA-REL-02 verlangt < 10 s).
- `hashicorp/raft` v1.8.0 enthält die Korrektur für den Membership-Bug über Term-Grenzen (offener Prüfpunkt aus ADR 0005).
- Ein TLS-Port mit ALPN (`uc-raft/1` und `h2`) läuft ohne Auffälligkeiten.

Dabei sind aber Lücken und Ungenauigkeiten aufgefallen. Weil sie das Verhalten akzeptierter ADRs ändern, werden sie hier gebündelt entschieden (CLAUDE.md, Regel 2). Die ursprünglichen ADRs bleiben gültig; bei Widerspruch gilt dieser ADR.

## Entscheidung

### A. Konsens und FSM (ergänzt ADR 0005)

**A1 – Snapshot nur aus `state.db`.** `VACUUM INTO` kopiert immer die ganze Datenbank, nicht einzelne Tabellen. Die Formulierung „Kopie der Klasse-A-Tabellen“ in ADR 0005 wird ersetzt durch:
- Klasse A liegt ausschließlich in `state.db` (wie in ADR 0004 bereits vorgesehen), Klasse B nie.
- Snapshot = `VACUUM INTO` von `state.db` in `FSMSnapshot.Persist`. Ein „Fuzzy“-Snapshot (Kopie neuer als der Snapshot-Index) ist erlaubt, weil der angewandte Index in der Kopie steckt und `Apply` ältere Einträge überspringt.
- Voraussetzung: **sämtlicher** FSM-Zustand inklusive Dedup-Tabelle und angewandtem Index liegt in `state.db` und wird in **einer** Transaktion je Log-Eintrag geschrieben.
- Restore = Datei-Tausch unter Schreibsperre, danach Neuöffnen.
- Messung: Mit 5 Mio. Heartbeats in derselben Datei wäre der Snapshot 106 MB statt 6 MB groß.

**A2 – Schneller Neustart.** Beim Start gilt `NoSnapshotRestoreOnStart = (state.applied_index >= latestSnapshot.Index)`. Fehlt `state.db` oder ist sie älter, spielt Raft den Snapshot ein. Die FSM überspringt alle Einträge `<= applied_index` ohne Seiteneffekte. Beides wird getestet.

**A3 – Promotion zum Voter.** „Index-Abstand < 100“ allein reicht nicht: Ein leerer Joiner besteht die Prüfung bei kurzem Log sofort. Neues Kriterium:
- Der Joiner hat **mindestens den Index seines eigenen `AddNonvoter`-Eintrags angewandt** **und** sein Abstand zu `LastIndex` ist < 100.
- Weil `hashicorp/raft` den Match-Index je Follower nicht öffentlich macht, fragt der Leader den Joiner per neuem RPC **`ClusterService.GetStatus`** (u. a. `applied_index`, `last_contact`).
- Gesamt-Timeout 10 min (große Snapshots). Wird er überschritten, bleibt der Knoten Nonvoter, und es gibt einen Systemalert.

**A4 – Serialisierung von Mitgliedschaftsänderungen.** Die Bibliothek erlaubt bereits höchstens eine uncommittete Konfiguration (`configurationChangeChIfStable`). Der Leader-Mutex aus ADR 0005 sichert nur **mehrschrittige** Abläufe (Nonvoter → Aufholen → Voter; Entfernen → Hinzufügen). Weil der Mutex leader-lokal ist, setzt der Join-Handler nach einem Leaderwechsel idempotent fort: Ist der Knoten schon Nonvoter, folgt direkt die Promotion. Änderungen nutzen `prevIndex` aus `GetConfiguration()`.

**A5 – Knoten ersetzen.** Raft verbietet doppelte Adressen. Ein Join mit neuer Node-ID unter einer bereits vergebenen Adresse wird mit **HTTP 409 und der alten Node-ID** abgelehnt. Es wird nie automatisch entfernt (FA-CLU-08).
- UI und CLI bieten „Knoten ersetzen“ an: Der `platform-admin` bestätigt, dann folgen serialisiert Entfernen (alt) und Join (neu).
- Das Runbook weist darauf hin, dass ein 3er-Cluster während des Ersetzens keine Ausfallreserve hat. Alternative ohne dieses Fenster: Der neue Knoten tritt vorübergehend unter einer anderen Advertise-Adresse bei (4 Voter), danach wird der alte entfernt.

**A6 – Timeouts.** Default `HeartbeatTimeout = ElectionTimeout = 1000 ms` (Bibliotheksstandard), konfigurierbar. 500 ms bringt ca. 0,8 s schnelleren Failover, ist aber anfälliger für GC-Pausen und Jitter. **Pre-Vote** (Default seit v1.7.0) bleibt ausdrücklich aktiv.

**A7 – Fehler in `Apply`.** Ein Fehler beim Anwenden eines Log-Eintrags (z. B. SQL-Fehler) **hält den Knoten an**: Readiness wird false, der Prozess beendet sich mit Fehler. Eine stille Fehlerrückgabe würde zu divergierenden FSMs führen. Ergänzend vergleichen die Voter ab P3 regelmäßig einen Fingerprint der Klasse-A-Tabellen (Systemalert bei Abweichung).

**A8 – Fehlerabbildung für Schreibzugriffe** (präzisiert FA-CLU-06):

| Raft-Fehler | Bedeutung | API-Antwort |
|-------------|-----------|-------------|
| `ErrNotLeader`, `ErrEnqueueTimeout` | Eintrag nicht im Log | intern erneut versuchen bzw. weiterleiten; sonst 503 mit `Retry-After` |
| `ErrLeadershipLost`, `ErrRaftShutdown`, Transportfehler beim Forwarding | Ausgang unbekannt | 503 „Ergebnis unbekannt“; Wiederholung mit gleicher Request-ID ist sicher |

Im Spike in 10 von 10 Leaderwechseln bestätigt: Der Retry lieferte das ursprüngliche Ergebnis, kein Duplikat.

**A9 – Health zeigt den Replikationsstand.** Nach einer Netztrennung kennt ein Follower sofort wieder den Leader, die Log-Replikation setzt wegen eines internen Backoffs (bis 10,24 s, nicht konfigurierbar) aber erst später ein. „Hat einen Leader“ reicht daher als Gesundheitskriterium nicht. Health und Admin-UI zeigen `applied_index` gegenüber dem Commit-Index des Leaders sowie `LastContact`. Überschreitet der Rückstand einen Schwellwert, meldet der Knoten den Zustand `stale` (FA-NODE-05, NFA-PERF-05).

### B. Inter-Node-Kommunikation und Replikation (ergänzt ADR 0006)

**B1 – gRPC im `h2`-Zweig des ALPN-Mux.** Option A aus ADR 0006 ist bestätigt. Der Mux übergibt das bereits gehandshakte `*tls.Conn` unverändert an den gRPC-Server. Die Transport-Credentials liefern den vorhandenen TLS-Zustand als `AuthInfo`, ohne zweiten Handshake. Verbindungen dürfen nicht gewrappt werden, sonst greift HTTP/2 nicht.

**B2 – Hochwassermarke.** `StreamPosition.seq` bedeutet „höchstes `seq`, bis zu dem **lückenlos** alles vorhanden ist“, gerechnet ab dem Retention-Horizont, nicht `max(seq)`. Sonst würde eine Lücke vor dem höchsten empfangenen `seq` nie nachgefordert. Der Kommentar im Proto-Vertrag wird entsprechend angepasst.

**B3 – Push ist Best Effort, Anti-Entropie ist maßgeblich.** Fehlgeschlagene Push-Batches werden verworfen, nicht erneut gesendet. Die Anti-Entropie schließt die Lücke (im Spike jeweils in < 1 s nach Wiederverbindung). Sie zieht **alle** Ströme, die der Peer kennt, auch die dritter Knoten.

**B4 – Inkarnation in der Heartbeat-Datenbank.** Die Inkarnation wird in einer Metatabelle von `metrics.db` gespeichert, nicht nur als Datei im Datenverzeichnis. Fehlt `metrics.db`, entsteht immer eine neue Inkarnation, auch wenn die Node-ID erhalten bleibt (PF-12).

**B5 – Kurzes Zeitbudget für Write-Forwarding.** Ein vom Leader getrennter Follower hing im Spike 7–8 s am Transport-Timeout. Forwarding erhält daher eine eigene Deadline von **≤ 3 s**. Ist `LastContact` zum Leader älter als 2 × `HeartbeatTimeout`, antwortet der Follower sofort mit 503. Für Heartbeat-Streams bleiben die Keepalives aus ADR 0006 unverändert (Puffer plus Anti-Entropie decken die Erkennungszeit ab).

**B6 – Anti-Entropie auch bei Raft-Ereignissen.** Zusätzlich zum 30-s-Intervall und zum Reconnect des Push-Streams wird ein Abgleich bei Raft-Observer-Ereignissen ausgelöst (Peer wieder erreichbar, Heartbeats wieder aufgenommen). So schließt sich eine Lücke auch dann schnell, wenn der Push-Pfad noch an einem toten Stream hängt.

### C. Persistenz (ergänzt ADR 0004)

Die Ergebnisse des SQLite-Spikes (AP-0.15) werden separat in [ADR 0013](0013-persistenz-ergebnisse-sqlite-spike.md) entschieden, weil dieser ADR vor ihrer Fertigstellung angenommen wurde.

## Konsequenzen

- Positiv: Die bekannten Fallstricke aus den Spikes sind vor der Umsetzung (P1/P2) entschieden; die Grundarchitektur bleibt unverändert.
- Folgearbeiten nach Accept:
  - Proto: `ClusterService.GetStatus` ergänzen (rückwärtskompatibel), Kommentar zu `StreamPosition` präzisieren (B2).
  - Pflichtenheft: FA-CLU-03 (Promotions-Kriterium), FA-CLU-08/10 („Knoten ersetzen“), FA-NODE-05 (Replikationsstand), FA-CLU-06 (Fehlerabbildung) anpassen.
  - Roadmap: A1/A2/A7/A8 in AP-1.2, A3–A5/A9 und B1–B6 in AP-2.2–AP-2.4, Runbook in AP-2.7.
- Nicht durch die Spikes abgedeckt (bleibt Aufgabe der Chaos-Tests in AP-2.9): reale Netzlatenz, Paketverlust und asymmetrische Partitionen, Clock-Skew, 5 Voter, Snapshot-Schleifen unter Last, gRPC, Probes, Join-Token und CA-Rotation.

## Approval

| Datum | Status | Durch | Anmerkung |
|-------|--------|-------|-----------|
| 2026-10-19 | Proposed | Claude | Teil C folgt nach AP-0.15 |
| 2026-10-19 | Accepted | Projektinhaber | Teile A und B; Teil C ausgelagert in ADR 0013 |
