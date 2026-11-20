# SQLite-Spike: modernc.org/sqlite vs. github.com/ncruces/go-sqlite3

Roadmap AP-0.15, offener Punkt O-07. Bezug: ADR 0004 (Persistenzschicht), PF-22..24.
Wegwerf-Spike unter `spikes/`, nicht für Produktcode übernehmen.

## Frage

Welcher CGO-freie SQLite-Treiber passt für die Heartbeat-Last (Batch-Insert, Range-Query, Retention-Delete)?
ADR 0004 sagt: `modernc` bleibt, außer `ncruces` ist im Benchmark mehr als Faktor 2 besser.

## Setup

- Treiber: `modernc.org/sqlite v1.60.1`, `github.com/ncruces/go-sqlite3 v0.35.6` (über `driver`, via `database/sql`).
  Das Paket `github.com/ncruces/go-sqlite3/embed` wird nicht mehr benötigt (ist laut Quelle deprecated) und ist daher nicht importiert.
- Build und Lauf mit `CGO_ENABLED=0`.
- Datei-DB im Temp-Verzeichnis, pro Lauf neu angelegt, danach gelöscht.
- Pragmas über den DSN (identisch für beide Treiber):
  - Schreib-Handle: `auto_vacuum(INCREMENTAL)`, `journal_mode(WAL)`, `journal_size_limit(67108864)`, `busy_timeout(5000)`, `synchronous(NORMAL)`, `foreign_keys(1)`, `_txlock=immediate`, `SetMaxOpenConns(1)`.
  - Lese-Handle: `mode=ro`, `busy_timeout(5000)`, `synchronous(NORMAL)`, `foreign_keys(1)`, max. 8 Verbindungen.
- Read-back: Der Schreib-Handle prüft alle Pragmas per `PRAGMA`. Der Lese-Pool hält alle 8 Verbindungen gleichzeitig und prüft jede einzeln. Bei Abweichung bricht der Lauf ab. Alle Läufe haben das bestanden (Schreib-Handle: `journal_mode=wal`, `busy_timeout=5000`, `synchronous=1`, `foreign_keys=1`, `auto_vacuum=2`, `journal_size_limit=67108864`; Leser: 8/8 Verbindungen OK).
- Schema wie in der Aufgabe. Zusätzlich `NOT NULL` auf allen Spalten außer `latency_ms`, `error_class`, `message`. Bei `WITHOUT ROWID` ist das für die PK-Spalten ohnehin Pflicht. Die Indizes sind `(monitor_id, ts_ms)` und `(ts_ms)`.
- Datenmodell: 1.000 Monitore, 3 Knoten, 20-s-Intervall. Jeder Knoten prüft jeden Monitor in jedem Intervall. Das ergibt 3.000 Zeilen je Intervall, also ca. 13 Mio. Zeilen/Tag. 20 Tenants, eine Incarnation pro Knoten, `seq = Intervall * 1000 + Monitor-Index`.
- Maschine: Apple M4 Pro (14 CPU-Kerne: 10 Performance, 4 Effizienz), 24 GB RAM, `Darwin 27.0.0 arm64` (`uname -a`), Go 1.27.2 `darwin/arm64`, `machdep.cpu.brand_string = Apple M4 Pro`. Es handelt sich um ein Notebook, nicht um einen Server. Hintergrundlast ist nicht ausgeschlossen.

### Ausgeführte Messungen

Gemessen wurden **2 Stunden (1.080.000 Zeilen)** und **12 Stunden (6.480.000 Zeilen)** für jede Kombination aus Treiber und Tabellenvariante. Zusätzlich gab es einen Sensitivitätslauf mit 100 ms Pause zwischen den Leser-Queries (2 Stunden, nur `rowid`).

Die **2-Tage-Zielgröße (ca. 26 Mio. Zeilen) wurde nicht gefahren**. Bei rund 30.000 Zeilen/s würde allein der Load ca. 15 Minuten dauern, pro Kombination. Das hätte die 20-Minuten-Grenze gesprengt. Die 2-Tage-Werte unten sind lineare Extrapolationen aus 2 h und 12 h.

Pro Lauf:
1. **Batch-Insert:** 500 Zeilen pro Transaktion, `INSERT ... ON CONFLICT DO NOTHING`, Prepared Statement in der Transaktion. Gemessen wird der Durchsatz und die Dauer je Transaktion.
2. **Gleichzeitige Last:** 30 Sekunden (`-conc-seconds`, im Lauf 20 s) ein Schreiber, der weiter Intervalle einfügt, plus 8 Leser. Leser fragen `SELECT ts_ms,status,latency_ms FROM heartbeats WHERE monitor_id=? AND ts_ms BETWEEN ? AND ?` mit 24-h-Fenster auf zufälligen Monitoren ab. Ohne Pause laufen die Leser am Anschlag. Gemessen werden Leser-p50/p95/max, Schreibdurchsatz, Anzahl `SQLITE_BUSY`, und die maximale WAL-Größe (Stichprobe alle 100 ms).
3. **Retention:** Alle Zeilen vor der Mitte der geladenen Daten löschen, in Batches à 5.000 Zeilen, je Batch eine eigene Immediate-Transaktion. Gemessen werden Gesamtzeit, Batch-p95 und -max (Transaktionsdauer als Näherung für die Schreibsperre). Danach `PRAGMA incremental_vacuum`, DB-Größe, WAL-Größe und Freelist vor und nach.
4. **DB-Größe je Million Zeilen:** Dateigröße nach Load und `wal_checkpoint(TRUNCATE)`, geteilt durch die geladenen Millionen Zeilen. Enthält Indizes.
5. **Cross-Kompilierung:** `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...` läuft für das Modul mit beiden Treibern durch.

## Ergebnisse

Vollständige Tabelle: `RESULTS.md` (generiert aus `results/*.json`). Einzelläufe mit Logs: `results/*.log`.

### Batch-Insert und Last

Insert-Durchsatz (Zeilen/s, Lauf ohne Leser, bzw. Load-Phase):

| Treiber | Tabelle | 2 h | 12 h |
|---|---|---|---|
| modernc | rowid | 37.699 | 25.397 |
| modernc | WITHOUT ROWID | 38.748 | 30.601 |
| ncruces | rowid | 34.741 | 32.348 |
| ncruces | WITHOUT ROWID | 29.407 | 29.985 |

Gleichzeitige Last (1 Schreiber + 8 Leser, Leser ohne Pause):

| Treiber | Tabelle | Schreiben (Zeilen/s) | Leser p50 / p95 (ms) | Max. WAL (MB) | SQLITE_BUSY |
|---|---|---|---|---|---|
| modernc | rowid, 2 h | 12.119 | 18,0 / 21,8 | 1.264 | 0 |
| modernc | WITHOUT ROWID, 2 h | 11.349 | 26,4 / 33,5 | 1.270 | 0 |
| ncruces | rowid, 2 h | 23.345 | 10,0 / 14,7 | 2.415 | 0 |
| ncruces | WITHOUT ROWID, 2 h | 24.114 | 11,3 / 14,5 | 2.716 | 0 |
| modernc | rowid, 12 h | 12.708 | 101,5 / 168,0 | 1.397 | 0 |
| modernc | WITHOUT ROWID, 12 h | 10.320 | 155,4 / 171,6 | 1.259 | 0 |
| ncruces | rowid, 12 h | 37.685 | 50,6 / 66,2 | 4.119 | 0 |
| ncruces | WITHOUT ROWID, 12 h | 31.391 | 60,7 / 75,1 | 3.834 | 0 |

Sensitivitätslauf, 2 h, `rowid`, Leser mit 100 ms Pause:

| Treiber | Schreiben (Zeilen/s) | Leser p50 / p95 (ms) | Max. WAL (MB) |
|---|---|---|---|
| modernc | 33.376 | 18,2 / 31,1 | 12,9 |
| ncruces | 31.147 | 3,8 / 13,2 | 10,5 |

Über alle 10 Läufe gab es **0 SQLITE_BUSY-Fehler** und **0 sonstige Fehler**.

### Retention, Größe, Vakuum

| Treiber | Tabelle | Retention gesamt (s), 2 h / 12 h | Batch p95 / max (ms), 12 h | DB nach Load (MB), 12 h | DB nach Delete (MB) | DB nach incremental_vacuum (MB) | Vakuum (s) |
|---|---|---|---|---|---|---|---|
| modernc | rowid | 4,6 / 30,5 | 51,9 / 65,0 | 795 | 827 | 433 | 1,6 |
| modernc | WITHOUT ROWID | 5,9 / 36,9 | 71,5 / 132,6 | 823 | 849 | 441 | 2,4 |
| ncruces | rowid | 14,8 / 34,5 | 60,8 / 90,2 | 795 | 888 | 494 | 1,9 |
| ncruces | WITHOUT ROWID | 6,0 / 41,7 | 72,9 / 110,3 | 823 | 903 | 495 | 2,5 |

Die Retention löschte im 12-h-Lauf 3.240.000 Zeilen in 648 Batches. Die Freelist lag nach dem Delete bei rund 100.000 bis 104.000 Seiten und war nach `incremental_vacuum` vollständig frei. Die WAL-Datei war nach dem Checkpoint jeweils 0 MB.

**DB-Größe je Million Zeilen:** 122 MB (rowid, beide Treiber, identische Dateiformate) und 128 MB (WITHOUT ROWID). Das sind rund 4,7 % mehr für WITHOUT ROWID.

### 2-Tage-Extrapolation (nicht gemessen)

Lineare Hochrechnung aus 12 h für 26 Mio. Zeilen:
- DB-Größe: ca. 3,2 GB (rowid) bzw. ca. 3,3 GB (WITHOUT ROWID), vor dem Vakuum.
- Load bei ca. 30.000 Zeilen/s: ca. 15 Minuten.
- Retention der Hälfte (ca. 13 Mio. Zeilen): ca. 2 bis 2,5 Minuten.
- Leser-Query: Das 24-h-Fenster liefert dann etwa 12.960 statt 6.480 Zeilen. Das Ergebnis wäre bei linearem Verhalten etwa doppelt so teuer wie im 12-h-Lauf. Das entspricht ca. 200 ms p50 für modernc und ca. 100 ms für ncruces unter Vollast.

Die Extrapolation ist grob. Sie ignoriert den Cache-Effekt (siehe unten).

### Cross-Kompilierung

`GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...` für das Modul: **erfolgreich** mit beiden Treibern.

## Beobachtungen und Überraschungen

1. **Reihenfolge der DSN-Pragmas ist kritisch.** Setzt man `journal_mode(WAL)` vor `auto_vacuum(INCREMENTAL)`, bleibt bei `ncruces` `auto_vacuum=0` stehen, auch mit explizitem `PRAGMA auto_vacuum=…` danach. SQLite schreibt beim Wechsel auf WAL den Datei-Header, danach lässt sich der Modus nicht mehr ändern. `modernc` hat die andere Reihenfolge toleriert. Im Spike steht `auto_vacuum` deshalb vorne. Der Read-back-Selbsttest aus ADR 0004 hat genau diesen Fehler gefunden. Der Test ist also nötig.

2. **`journal_size_limit` ist eine Verbindungseinstellung.** Auf den Lese-Verbindungen zeigt `PRAGMA journal_size_limit` den Wert `-1`. Der Selbsttest prüft diesen Wert deshalb nur auf dem Schreib-Handle.

3. **WAL wächst stark unter überlappender Leserlast.** Mit acht Lesern ohne Pause wuchs die WAL-Datei auf 1,3 GB (modernc) bzw. 2,4 bis 4,1 GB (ncruces). Der Grund sind überlappende Lese-Snapshots. Der Checkpoint kann die WAL dann nicht auf Null zurücksetzen. Mit 100 ms Pause zwischen den Queries blieb die WAL bei 10 bis 13 MB. Das bestätigt die Warnung aus ADR 0004 vor WAL-Wachstum durch Lesetransaktionen. Die Gegenmaßnahme sind kurze Lesetransaktionen und Lücken zwischen Lesern, damit ein Checkpoint-TRUNCATE durchlaufen kann. Auf Produktionsseite gehört dazu eine Überwachung der WAL-Größe als Metrik.

4. **Der Insert-Durchsatz sinkt mit der DB-Größe.** Im Debug-Lauf sank er von rund 110.000 auf rund 36.000 Zeilen/s, während die Datenbank auf etwa 130 MB wuchs. Die Ursache ist vermutlich der Standard-Seiten-Cache (2 MB), den der Spike nicht erhöht hat. Die Vermutung ist nicht gemessen. Eine `cache_size`-Einstellung wäre der nächste Schritt, falls der Insert-Durchsatz kritisch wird.

5. **Leser verhalten sich nicht gleich.** Ohne Pause ist `ncruces` bei der Lesezeit etwa 1,8- bis 2-fach schneller (2 h: 10 ms gegen 18 ms p50; 12 h: 51 ms gegen 102 ms p50). Mit 100 ms Pause ist der Unterschied größer (3,8 ms gegen 18,2 ms p50).

6. **Einzelne Ausreißer.** Die maximale Transaktionsdauer in der Load-Phase lag bei mehreren Läufen zwischen rund 0,7 und 4,7 Sekunden (z. B. `modernc` rowid 12 h: 4.731 ms). Die p95-Werte sind stabil. Die Ausreißer sind nicht erklärt. Vermutlich stören Systemvorgänge den Notebook-Lauf. Ein Retention-Batch von `ncruces` rowid 2 h hatte max. 538 ms, also über dem 200-ms-Ziel. Im 12-h-Lauf und im Pausen-Lauf lag der Wert bei 90 bzw. 54 ms. Ein Einzelwert, nicht reproduziert.

7. **Der Retention-Ansatz hält das 200-ms-Ziel.** Im 12-h-Lauf lag der Batch-p95 zwischen 52 und 73 ms, das Maximum zwischen 65 und 133 ms. Die Löschung ist also für die Schreibsperre tragbar. Ohne `incremental_vacuum` bleibt die Datei dagegen bei voller Größe. Das Vakuum dauert nur 1,6 bis 2,5 s, gibt aber rund die Hälfte des Platzes frei.

## Empfehlung

**Treiber:** `modernc` beibehalten (ADR 0004). Die Schwelle von Faktor 2 wird nur teilweise erreicht. Die Bewertung:

- Insert-Durchsatz ohne Leser: zwischen 0,9- und 1,3-fach, also kein klarer Unterschied. Unter der Schwelle.
- Retention: etwa gleich. Unter der Schwelle.
- Größe: identisch.
- Lesezeiten: 2-fach im 12-h-Lauf (p50 51 gegen 102 ms, p95 66 gegen 168 ms), 2,4-fach p95 mit 100-ms-Pause (13,2 gegen 31,1 ms) und 4,8-fach p50 mit Pause (3,8 gegen 18,2 ms). Das ist nahe an oder über der Schwelle.
- Schreiben unter Vollast: rund 3-fach zugunsten von `ncruces` (37.685 gegen 12.708 Zeilen/s). Mit Pause gleich (31.147 gegen 33.376).
- WAL-Wachstum unter Vollast: 2 bis 3-fach größer bei `ncruces`. Das ist ein Nachteil für `ncruces`.

Die Lesevorteile von `ncruces` sind real. Sie sind aber nicht eindeutig und stammen aus Einzelläufen auf einem Notebook. Der WAL-Nachteil wiegt im Betrieb schwer. Ein Wechsel auf `ncruces` wäre nach ADR 0004 ein ergänzender ADR. Die Entscheidung liegt bei Projektinhaber. Empfehlung: `modernc` bleibt. Eine Wiederholung der Lesemessung auf einem stabileren Gerät wäre die Grundlage für einen späteren Wechsel.

**Tabellenvariante:** `rowid` (Standard). Die Dateigröße ist rund 5 % kleiner. Die Leser sind unter Vollast schneller (modernc 102 gegen 155 ms p50, ncruces 51 gegen 61 ms p50). Beim Insert und bei der Retention zeigt `WITHOUT ROWID` keinen konsistenten Vorteil. Der mögliche Vorteil von `WITHOUT ROWID` (clustered PK) greift vermutlich bei dieser Abfrage nicht, weil sie über `(monitor_id, ts_ms)` läuft und danach die Zeilen nachlädt.

## Grenzen

- Ein Lauf pro Konfiguration, keine Wiederholungen, keine Konfidenzintervalle. Die Schwankungen zwischen Läufen sind teils erheblich (z. B. Insert-Durchsatz 25 bis 39 k Zeilen/s).
- Notebook, keine Server-Hardware. Hintergrundaktivität ist möglich.
- 2 Tage nicht gefahren, Ergebnisse dafür sind lineare Extrapolationen.
- Die Retention-Dauer misst die Transaktion inklusive Commit. Das entspricht der Schreibsperre, ist aber nicht die exakte interne Sperrdauer.

## Dateien

- `main.go`: Treiber-Setup, Pragma-Selbsttest, Messphasen, Summary-Tabelle (`-summarize`).
- `bench.go`: Datengenerator, Schema, Insert, Lese- und Schreib-Last, Retention, Hilfsfunktionen.
- `run_all.sh`: Die gesamte Messmatrix (ca. 25 Minuten).
- `go.mod`, `go.sum`: Module `pulsequorum/spikes/sqlite`, Abhängigkeiten `modernc.org/sqlite v1.60.1`, `github.com/ncruces/go-sqlite3 v0.35.6`.
- `results/*.json`, `results/*.log`: Rohdaten und Logs je Lauf.
- `RESULTS.md`: Ergebnistabellen, generiert.

Reproduzieren: `./run_all.sh` oder einzeln `CGO_ENABLED=0 go run . -driver=modernc -variant=rowid -rows=1080000 -think-ms=0 -conc-seconds=20`. Danach `go run . -summarize`.
