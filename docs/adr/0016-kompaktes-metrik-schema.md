# ADR 0016: Kompaktes Schema für Latenz-Minuten und Rollups

| | |
|---|---|
| **Status** | Proposed |
| **Datum** | 2026-10-22 |
| **Entscheider** | Projektinhaber |
| **Bezug** | AS-10, FA-UPT-03..06, NFA-PERF-03, PF-23, ADR 0004, ADR 0013 |

## Kontext

Beim Lasttest für AP-1.15 wurde die Speichergröße von `metrics.db` mit erzeugten Daten gemessen (`test/load/gendata`, 50 Monitore, 10 Tage, 60-s-Intervall, Verdichtung an; Messung über `dbstat`):

| Tabelle inkl. Indizes | Zeilen | Bytes je Zeile |
|---|---|---|
| `latency_minutes` (Tabelle 62 MB, PK-Index 57 MB, Index `minute_ms` 9 MB) | 573.826 | **224** |
| `heartbeats` (roh, letzte 48 h) | 148.556 | 320 |
| `uptime_rollups` | 25.000 | ≈ 210 |

Hochgerechnet auf AS-10 (1.000 Monitore à 60 s, 30 Tage, ein Node):

- Latenz-Minuten: 1.000 × 28 Tage × 1.440 ≈ 40 Mio. Zeilen × 224 B ≈ **9 GB**
- Rohdaten 48 h: 2,9 Mio. Zeilen × 320 B ≈ 0,9 GB
- Stunden-Rollups 30 Tage: ≈ 0,3 GB (nach der Standard-Aufbewahrung von 365 Tagen ≈ 3,7 GB)

Summe ≈ **10 GB**. AS-10 verlangt ≤ 4 GB je Voter. Die Anforderung ist mit dem heutigen Schema nicht erreichbar.

Ursachen:

1. Bei einem 60-s-Intervall erzeugt die Verdichtung (FA-UPT-04) je Heartbeat genau eine Minuten-Zeile – sie verdichtet nichts, sie verschiebt nur.
2. Jede Zeile trägt `tenant_id`, `monitor_id` und `node_id` als UUID-Text (je 36 Bytes), im Tabellen-B-Baum **und** im Primärschlüssel-Index (Tabelle mit `rowid` plus `sqlite_autoindex`), dazu ein dritter Index auf `minute_ms` für die Aufbewahrung.
3. `avg_ms`/`p95_ms` sind `REAL` (8 Bytes), obwohl Millisekunden-Ganzzahlen reichen.

## Optionen

### Option A: Kompaktes Schema (Serien-Schlüssel, WITHOUT ROWID, Ganzzahlen)
- Neue Tabelle `series(id INTEGER PRIMARY KEY, tenant_id, monitor_id, node_id, UNIQUE(monitor_id, node_id))` in `metrics.db`; `latency_minutes` und `uptime_rollups` referenzieren `series_id` (Ganzzahl, 1–3 Bytes) statt dreier UUID-Texte. Die Mandantentrennung bleibt: jede Abfrage löst zuerst `(tenant_id, monitor_id)` → `series_id` auf, die Repository-Methoden erhalten die Tenant-ID weiterhin explizit.
- `WITHOUT ROWID` mit Primärschlüssel `(series_id, minute_ms)` bzw. `(series_id, bucket, start_ms)`: Die Daten liegen nur einmal im B-Baum.
- Latenzen als `INTEGER` (ms), Zähler als `INTEGER`.
- Aufbewahrung ohne eigenen Zeitindex: Löschen je Serie über den Primärschlüssel (`series_id = ? AND minute_ms < ?`) in Batches (FA-UPT-06 bleibt erfüllt).
- Geschätzt ≈ 40–50 B je Minuten-Zeile → AS-10 ≈ 1,8 GB Minuten + 0,9 GB Rohdaten + 0,1 GB Rollups ≈ **2,8 GB**; Rollups nach einem Jahr ≈ 1 GB.
- Vorteile: Erfüllt AS-10 ohne Abstriche an Auflösung und Aufbewahrung (FA-UPT-04/05 unverändert). Abfragen werden eher schneller (kleinere Seiten, weniger I/O).
- Nachteile: Migration von `metrics.db` (Tabellen neu anlegen und umkopieren; vor 1.0 unkritisch). Serien-IDs sind knotenlokal und dürfen nie repliziert werden (Rollups sind ohnehin je Voter berechnet, FA-UPT-03 vergleicht Inhalte, nicht IDs). Eine weitere Indirektion im Code.

### Option B: Gestufte Auflösung
Minuten-Werte 7 Tage, danach 5-Minuten-Werte bis 30 Tage.
- Vorteile: Weniger Zeilen (≈ 17 Mio. statt 40 Mio.).
- Nachteile: Ändert FA-UPT-04/05 (Pflichtenheft); mit dem heutigen Schema ≈ 3,7 GB + Rohdaten – reicht allein nicht für 4 GB. Zusätzlicher Verdichtungsschritt.

### Option C: Kürzere Aufbewahrung der Minuten-Werte (z. B. 7 Tage, danach nur Stunden-Rollups)
- Vorteile: Einfach.
- Nachteile: Ändert FA-UPT-05; Latenzverläufe älter als 7 Tage nur noch stündlich.

### Option D: AS-10-Grenze anheben
- Nachteile: Widerspricht dem Ziel kleiner VPS-Knoten (Pflichtenheft Kap. 3); verschiebt nur das Problem (Rollups wachsen über ein Jahr weiter).

## Entscheidung

Vorgeschlagen: **Option A.** Sie behebt die Ursache (Speicherformat), ohne die fachlichen Anforderungen zu ändern, und schafft Reserve für mehrere Nodes je Monitor (P2: Heartbeats aller Nodes werden repliziert, je Node eine Serie). Option B bleibt als spätere, konfigurierbare Ergänzung möglich, falls größere Installationen sie brauchen.

Nicht Teil dieses ADR: das Schema von `heartbeats`. Es trägt den Replikationsschlüssel `(node_id, incarnation, seq)` (FA-REP-01, ADR 0006); Rohdaten werden nach 48 h verdichtet, ihr Anteil (≈ 0,9 GB bei AS-10) ist tragbar. Eine Verschlankung dort wird mit der Heartbeat-Replikation (AP-2.4) betrachtet.

## Konsequenzen

- Positiv: AS-10 erreichbar (Schätzung ≈ 2,8 GB); kleinere Datei, schnellere Abfragen, kürzere Retention-Läufe.
- Negativ / Risiken: Migration bestehender `metrics.db` (Umkopieren, bei großen Installationen Minuten; vor Release 1.0 tragbar); die Schätzung muss mit `gendata` bestätigt werden, bevor AS-10 abgenommen wird.
- Folgearbeiten (als AP-1.17, nach Annahme):
  - Migration `metrics/00003`: `series`, neue `latency_minutes`/`uptime_rollups` (WITHOUT ROWID, Ganzzahlen), Umkopieren, alte Tabellen löschen, `incremental_vacuum`.
  - Store: Serien-Auflösung mit Cache, Anpassung von `CompactHeartbeats`, `PutRollups`, `ListRollups`, Retention; gemeinsame Store-Testsuite inkl. Mandanten-Isolation.
  - AS-10 erneut messen (`gendata` 1.000 × 30 Tage, Retention-Lauf, API-p95).

## Approval

| Datum | Status | Durch | Anmerkung |
|-------|--------|-------|-----------|
| 2026-10-22 | Proposed | Claude | |
