# ADR 0013: Persistenz – Ergebnisse des SQLite-Spikes

|                 |                                                                                                                                                                                                       |
|-----------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **Status**      | Accepted                                                                                                                                                                                              |
| **Datum**       | 2026-10-19                                                                                                                                                                                            |
| **Entscheider** | Projektinhaber                                                                                                                                                                                              |
| **Bezug**       | Ergänzt ADR 0004 (schließt O-07) · [Spike AP-0.15](../../spikes/sqlite/README.md), [Messwerte](../../spikes/sqlite/RESULTS.md) · ADR 0012 (Teil C) · FA-NODE-06, FA-UPT-06, NFA-PERF-01/03, PF-22..24 |

## Kontext

ADR 0004 hat `modernc.org/sqlite` gewählt, unter dem Vorbehalt eines Benchmarks: Ist `ncruces/go-sqlite3` mit der
Heartbeat-Last **um mehr als Faktor 2 besser**, folgt ein ergänzender ADR. Der Spike AP-0.15 hat beide Treiber mit dem
Setup aus ADR 0004 gemessen (WAL, ein Schreib-Handle mit `_txlock=immediate`, Lese-Pool mit 8 Verbindungen,
`auto_vacuum=INCREMENTAL`, Batch-Inserts à 500 Zeilen, Retention in Batches à 5.000 Zeilen). Datenmengen: 2 h (1,08 Mio.
Zeilen) und 12 h (6,48 Mio. Zeilen) bei 1.000 Monitoren, 3 Nodes, 20-s-Intervall. Einzelläufe auf einem Notebook (Apple
M4 Pro); die Zahlen sind Richtwerte, keine Abnahmewerte.

### Messergebnisse (12 h, Tabelle mit rowid)

| Messgröße                                     | modernc         | ncruces         | Faktor            |
|-----------------------------------------------|-----------------|-----------------|-------------------|
| Batch-Insert (ohne Leser)                     | 25.400 Zeilen/s | 32.300 Zeilen/s | 1,3×              |
| Schreiben unter Volllast (8 Leser ohne Pause) | 12.700 Zeilen/s | 37.700 Zeilen/s | 3,0×              |
| Leser p50 / p95 (24-h-Range-Query, Volllast)  | 102 / 168 ms    | 51 / 66 ms      | 2,0× / 2,5×       |
| Leser p50 / p95 (100 ms Pause, 2 h)           | 18 / 31 ms      | 3,8 / 13 ms     | 4,8× / 2,4×       |
| Max. WAL unter Volllast                       | 1,4 GB          | 4,1 GB          | 0,3× (schlechter) |
| Retention 3,24 Mio. Zeilen gesamt             | 30,5 s          | 34,5 s          | 0,9×              |
| Retention-Batch p95 / max                     | 52 / 65 ms      | 61 / 90 ms      | 0,9×              |
| DB-Größe je 1 Mio. Zeilen                     | 122 MB          | 122 MB          | 1,0×              |
| `SQLITE_BUSY`                                 | 0               | 0               | –                 |

Weitere Befunde:

1. **Pragma-Reihenfolge:** Steht `journal_mode(WAL)` im DSN vor `auto_vacuum(INCREMENTAL)`, bleibt bei `ncruces`
   `auto_vacuum=0` stehen. `modernc` toleriert die Reihenfolge. Der Pragma-Selbsttest aus ADR 0004 hat das erkannt.
2. **`journal_size_limit`** ist eine Verbindungseinstellung und auf Lese-Verbindungen `-1`.
3. **WAL-Wachstum (PF-24 bestätigt):** Mit acht Lesern ohne Pause verhindern sich überlappende Lesetransaktionen
   gegenseitig das Zurücksetzen des WAL (1,3–4,1 GB). Mit 100 ms Pause zwischen Abfragen bleibt das WAL bei 10–13 MB.
4. **Insert-Durchsatz sinkt mit der Datenbankgröße**, vermutlich wegen des Standard-Seiten-Caches (2 MB); nicht
   gemessen.
5. **Ausreißer:** Einzelne Load-Transaktionen dauerten bis 4,7 s (p95 stabil bei ~30 ms), ein Retention-Batch einmal 538
   ms. Ursache ungeklärt, nicht reproduziert.
6. Retention mit `incremental_vacuum` gibt den Platz vollständig frei (Freelist danach 0); der Cross-Compile für
   `linux/arm64` ohne CGO klappt bei beiden Treibern.

### Einordnung

Der Bedarf liegt weit unter beiden Treibern: 1.000 Monitore à 20 s auf 3 Nodes ergeben rund **150 Heartbeat-Zeilen/s**,
also weniger als 1 % des gemessenen Schreibdurchsatzes unter Volllast. Die Dashboards lesen laut ADR 0004 und
NFA-PERF-03 aus Rollups, nicht aus Roh-Heartbeats; die gemessene Volllast (acht Leser, die ununterbrochen
24-h-Rohbereiche abfragen) ist ein Extremfall. `ncruces` ist beim Lesen etwa doppelt so schnell, erzeugt unter Volllast
aber ein dreimal größeres WAL. Die Schwelle aus ADR 0004 ist nur bei einzelnen Kennzahlen erreicht.

## Optionen

### Option A: Bei `modernc.org/sqlite` bleiben

- Vorteile: Verbreitetste CGO-freie Lösung, keine Änderung an ADR 0004, kleineres WAL unter Last; Leistung mit großem
  Abstand ausreichend.
- Nachteile: Lesezugriffe etwa halb so schnell wie mit `ncruces`.

### Option B: Zu `ncruces/go-sqlite3` wechseln

- Vorteile: ~2× schnellere Lesezugriffe, ~3× mehr Schreibdurchsatz unter Volllast.
- Nachteile: Deutlich größeres WAL unter Last; Pragma-Reihenfolge empfindlicher; kleinere Nutzerbasis; der Vorteil wird
  bei unserer Last nicht gebraucht.

## Entscheidung

**C1 – Treiber: Option A, `modernc.org/sqlite` bleibt (O-07 geschlossen).** Der Treiber bleibt hinter der
Repository-Schicht gekapselt, sodass ein späterer Wechsel nur `internal/store/sqlite` betrifft. **Neubewertung**, falls
der Lasttest in AP-1.15 NFA-PERF-01 oder NFA-PERF-03 verfehlt; dann ergänzender ADR mit Messung auf Server-Hardware.

**C2 – Tabellenvariante: rowid.** Heartbeats werden als normale Tabelle mit `PRIMARY KEY (node_id, incarnation, seq)`
angelegt, **nicht** `WITHOUT ROWID`: rund 4,5 % kleiner, schnellere Range-Queries unter Last, bei Insert und Retention
kein konsistenter Vorteil der Alternative. Retention löscht über `rowid` in Batches.

**C3 – Pragma-Reihenfolge und Selbsttest.** Im DSN steht `auto_vacuum` **vor** `journal_mode`. Der Start-Selbsttest
(FA-NODE-06) prüft `journal_mode`, `busy_timeout`, `synchronous`, `foreign_keys` und `auto_vacuum` auf jeder Verbindung,
`journal_size_limit` nur auf dem Schreib-Handle.

**C4 – Schutz vor WAL-Wachstum** (konkretisiert ADR 0004, PF-24):

- Lesetransaktionen auf `metrics.db` sind kurz; Roh-Heartbeat-Abfragen der API sind paginiert und auf einen Zeitbereich
  begrenzt (siehe `limit` ≤ 5.000 im OpenAPI-Vertrag). Uptime- und Latenz-Ansichten lesen aus Rollups bzw.
  Minuten-Aggregaten.
- Der Lese-Pool von `metrics.db` wird auf **4** Verbindungen begrenzt (statt 8), damit Lücken für Checkpoints entstehen.
- Ein Hintergrundjob führt periodisch (Standard: alle 60 s) `wal_checkpoint(TRUNCATE)` aus und protokolliert, wenn er
  nicht vollständig gelingt.
- Die WAL-Größe ist eine Metrik; ab 512 MB gibt es einen Systemalert (FA-OBS-01).

**C5 – Seiten-Cache.** `cache_size` wird für das Schreib-Handle von `metrics.db` konfigurierbar gemacht (Standard 64
MB); der Effekt wird im Lasttest AP-1.15 gemessen und der Standard ggf. angepasst.

**C6 – Ausreißer untersuchen.** Der Lasttest AP-1.15 misst die maximale Schreib-Transaktionsdauer gesondert. Erste
Hypothese: Der automatische WAL-Checkpoint läuft im Commit-Pfad des Schreibers. Bestätigt sich das, wird
`wal_autocheckpoint` auf dem Schreib-Handle abgeschaltet und nur noch der Hintergrundjob aus C4 checkpointet.

## Konsequenzen

- Positiv: ADR 0004 bleibt in der Treiberfrage unverändert; die Messwerte belegen ausreichend Reserve (Faktor > 80 beim
  Schreiben); konkrete Maßnahmen gegen WAL-Wachstum und für den Selbsttest.
- Negativ: Lesezugriffe sind langsamer als mit `ncruces` möglich wäre; das ist bei Rollup-basierten Ansichten
  unkritisch, wird aber im Lasttest überprüft.
- Folgearbeiten nach Accept: C2–C5 in AP-1.1, AP-1.9 und AP-3.6; C1 und C6 im Lasttest AP-1.15.
- Grenzen des Spikes: Notebook statt Server, Einzelläufe, keine 2-Tage-Datenmenge (26 Mio. Zeilen nur extrapoliert),
  kein Langzeitlauf mit wiederholter Retention.

## Approval

| Datum      | Status   | Durch    | Anmerkung         |
|------------|----------|----------|-------------------|
| 2026-10-19 | Proposed | Claude   |                   |
| 2026-10-19 | Accepted | Projektinhaber | im Chat bestätigt |
