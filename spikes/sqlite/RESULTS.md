# Ergebnisse: SQLite-Spike (AP-0.15, O-07)

Generiert von `go run . -summarize` aus `results/*.json`. Setup, Maschine und Interpretation: siehe `README.md`.

## Insert, Lese-/Schreiblast

| Treiber | Tabelle | Zeilen (Std. simuliert) | Insert Zeilen/s | Tx p50 / p95 / max (ms) | Last: Schreiben Zeilen/s | Last: Schreib-Tx p95 (ms) | Last: Leser-Queries | Leser p50 / p95 / max (ms) | SQLITE_BUSY | Sonstige Fehler | Max. WAL unter Last (MB) |
|---|---|---|---|---|---|---|---|---|---|---|---|
| modernc | rowid | 1080000 (2.00 h) | 37699 | 6.74 / 27.02 / 52.5 | 12119 | 45.11 | 8776 | 18.03 / 21.84 / 48.1 | 0 | 0 | 1263.9 |
| modernc | rowid (Leser-Pause 100 ms) | 1080000 (2.00 h) | 37156 | 6.14 / 25.77 / 861.0 | 33376 | 27.51 | 1367 | 18.22 / 31.12 / 39.7 | 0 | 0 | 12.9 |
| modernc | without-rowid | 1080000 (2.00 h) | 38748 | 7.29 / 24.30 / 31.3 | 11349 | 51.04 | 5961 | 26.41 / 33.54 / 55.0 | 0 | 0 | 1269.6 |
| ncruces | rowid | 1080000 (2.00 h) | 34741 | 6.17 / 25.50 / 1018.3 | 23345 | 32.23 | 14875 | 10.03 / 14.65 / 162.1 | 0 | 0 | 2414.6 |
| ncruces | rowid (Leser-Pause 100 ms) | 1080000 (2.00 h) | 39337 | 5.14 / 24.75 / 715.8 | 31147 | 23.08 | 1499 | 3.76 / 13.16 / 59.5 | 0 | 0 | 10.5 |
| ncruces | without-rowid | 1080000 (2.00 h) | 29407 | 6.81 / 26.33 / 1051.4 | 24114 | 25.53 | 13785 | 11.25 / 14.52 / 47.0 | 0 | 0 | 2715.8 |
| modernc | rowid | 6480000 (12.00 h) | 25397 | 15.63 / 31.66 / 4731.4 | 12708 | 54.67 | 1252 | 101.53 / 168.00 / 2120.8 | 0 | 0 | 1396.6 |
| modernc | without-rowid | 6480000 (12.00 h) | 30601 | 9.41 / 26.19 / 1530.0 | 10320 | 54.92 | 1028 | 155.38 / 171.63 / 219.3 | 0 | 0 | 1258.5 |
| ncruces | rowid | 6480000 (12.00 h) | 32348 | 8.16 / 26.02 / 1185.4 | 37685 | 21.13 | 2919 | 50.56 / 66.17 / 513.9 | 0 | 0 | 4118.6 |
| ncruces | without-rowid | 6480000 (12.00 h) | 29985 | 9.22 / 27.39 / 1128.5 | 31391 | 24.33 | 2453 | 60.71 / 75.10 / 657.1 | 0 | 0 | 3833.8 |

## Retention, Speichergroesse

| Treiber | Tabelle | Zeilen | Geloescht | Batches (5k) | Retention gesamt (s) | Batch p95 / max (ms) | DB nach Load (MB) | MB je 1 Mio. Zeilen | WAL vor Checkpoint (MB) | DB nach Delete (MB) | incremental_vacuum (s) | DB nach VACUUM (MB) | Freelist vor / nach (Seiten) | WAL nach VACUUM (MB) |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| modernc | rowid | 1080000 | 540000 | 108 | 4.6 | 48.9 / 53.3 | 131.9 | 122.1 | 5.3 | 162.5 | 0.25 | 98.5 | 16359 / 0 | 0.0 |
| modernc | rowid (Leser-Pause 100 ms) | 1080000 | 540000 | 108 | 4.7 | 48.4 / 129.4 | 131.9 | 122.1 | 5.4 | 214.5 | 0.23 | 150.6 | 16317 / 0 | 0.0 |
| modernc | without-rowid | 1080000 | 540000 | 108 | 5.9 | 62.2 / 63.9 | 138.0 | 127.8 | 5.7 | 166.6 | 0.25 | 100.0 | 17022 / 0 | 0.0 |
| ncruces | rowid | 1080000 | 540000 | 108 | 14.8 | 334.8 / 537.8 | 131.9 | 122.1 | 5.4 | 188.9 | 0.33 | 125.2 | 16269 / 0 | 0.0 |
| ncruces | rowid (Leser-Pause 100 ms) | 1080000 | 540000 | 108 | 4.9 | 51.3 / 53.9 | 131.9 | 122.1 | 5.4 | 208.3 | 0.21 | 144.7 | 16265 / 0 | 0.0 |
| ncruces | without-rowid | 1080000 | 540000 | 108 | 6.0 | 59.3 / 61.8 | 138.0 | 127.8 | 5.7 | 199.2 | 0.24 | 132.5 | 17070 / 0 | 0.0 |
| modernc | rowid | 6480000 | 3240000 | 648 | 30.5 | 51.9 / 65.0 | 795.2 | 122.7 | 5.5 | 827.1 | 1.61 | 433.1 | 100742 / 0 | 0.0 |
| modernc | without-rowid | 6480000 | 3240000 | 648 | 36.9 | 71.5 / 132.6 | 823.1 | 127.0 | 5.9 | 849.1 | 2.40 | 441.2 | 104307 / 0 | 0.0 |
| ncruces | rowid | 6480000 | 3240000 | 648 | 34.5 | 60.8 / 90.2 | 795.2 | 122.7 | 5.7 | 887.9 | 1.91 | 494.1 | 100703 / 0 | 0.0 |
| ncruces | without-rowid | 6480000 | 3240000 | 648 | 41.7 | 72.9 / 110.3 | 823.1 | 127.0 | 5.9 | 903.0 | 2.48 | 495.0 | 104324 / 0 | 0.0 |
