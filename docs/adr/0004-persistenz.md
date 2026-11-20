# ADR 0004: Persistenzschicht (SQLite, Schema, Migrationen, Repository)

|                 |                                                                                                              |
|-----------------|--------------------------------------------------------------------------------------------------------------|
| **Status**      | Accepted                                                                                                     |
| **Datum**       | 2026-10-19                                                                                                   |
| **Entscheider** | Projektinhaber                                                                                                     |
| **Bezug**       | FA-TEN-01, FA-UPT-02..06, FA-NODE-06, NFA-PORT-03, NFA-MNT-04, PF-22..24, PF-35, PF-36, O-07, E10, Konzept 6 |

## Kontext

Jeder Voter hält Klasse-A-Daten (über Raft materialisiert) und Klasse-B-Daten (Heartbeats, bis ~13 Mio. Zeilen/Tag bei
20-s-Intervallen) lokal in SQLite, CGO-frei. Später kommt PostgreSQL hinzu. Recherchierte Fallstricke: `SQLITE_BUSY` bei
mehreren schreibenden Pool-Verbindungen, stillschweigend ignorierte DSN-Pragmas, wachsende Dateien trotz Löschungen,
WAL-Wachstum durch lange Lesetransaktionen, doppelter Pflegeaufwand mit `sqlc` für zwei Dialekte. Uptime Kuma wurde ab ~
500 Monitoren durch Full-Scans langsam.

## Optionen

### Treiber

- **A1: `modernc.org/sqlite`** (BSD-3, reines Go, transpiliertes C) – im Konzept gewählt, weit verbreitet;
  Schreibleistung in Benchmarks unter `mattn` (CGO).
- **A2: `ncruces/go-sqlite3`** (MIT, SQLite als WASM via wazero) – CGO-frei, aktiv; Leistung je nach Benchmark
  unterschiedlich.
- **A3: `mattn/go-sqlite3`** – schnell, aber CGO → widerspricht statischem Multi-Arch-Build.

### Aufteilung der Daten

- **B1: Eine Datenbankdatei** für alles.
- **B2: Zwei Dateien:** `state.db` (Klasse A, nur von der Raft-FSM beschrieben) und `metrics.db` (Klasse B).

### Abfrageschicht

- **C1: `sqlc` + `goose`** (Konzept) – typsicher, SQL bleibt sichtbar; für PostgreSQL eigene Query-Dateien.
- **C2: Handgeschriebenes `database/sql`** – flexibel, mehr Boilerplate, keine Compile-Zeit-Prüfung.
- **C3: ORM (GORM, ent)** – verdeckt SQL, schwer zu optimieren für Zeitreihen.

## Entscheidung

**A1 + B2 + C1**, unter Vorbehalt des SQLite-Spikes (AP-0.15): Ist `ncruces` im Heartbeat-Benchmark (Batch-Insert,
Range-Query, Retention) um mehr als Faktor 2 besser, folgt ein ergänzender ADR.

**Verbindungen und Pragmas**

- Je Datei ein **Schreib-Handle** (`SetMaxOpenConns(1)`, `_txlock=immediate`) und ein **Lese-Handle** (Pool, `mode=ro`,
  max. 8 Verbindungen).
- Pragmas über den DSN (`_pragma=journal_mode(WAL)`, `busy_timeout(5000)`, `synchronous(NORMAL)`, `foreign_keys(1)`,
  `journal_size_limit(67108864)`); **Start-Selbsttest** liest die effektiven Werte auf jeder neuen Verbindung zurück und
  bricht bei Abweichung ab (FA-NODE-06).
- `auto_vacuum=INCREMENTAL` wird in der ersten Migration gesetzt (vor Anlage der Tabellen).
- Lesetransaktionen sind kurz; Streams (WebSocket, Export) lesen seitenweise, nie mit offenem Cursor über längere Zeit.
  Periodisch `wal_checkpoint(TRUNCATE)`; WAL-Größe als Metrik.

**Zwei Dateien (B2)**

- `state.db`: nur die FSM schreibt; Raft-Snapshot = `VACUUM INTO` dieser Datei (ADR 0005). Seltene Schreibzugriffe,
  `synchronous(FULL)`.
- `metrics.db`: Heartbeats, Latenz-Minuten, Rollups. Batch-Inserts (bis 500 Zeilen oder 1 s), Retention in Batches ≤
  5.000 Zeilen mit `incremental_vacuum` danach.
- Abfragen, die beide Dateien brauchen, werden im Service-Layer zusammengeführt (kein `ATTACH` im Hot Path).

**Schema-Regeln (portabel für PostgreSQL)**

- Zeitstempel `INTEGER` UTC-Millisekunden (`BIGINT` in PostgreSQL), Booleans `INTEGER 0/1`, IDs als Text (UUIDv7), JSON
  als `TEXT` (in PostgreSQL `JSONB` nur in engine-spezifischen Queries).
- Heartbeats: `PRIMARY KEY (node_id, incarnation, seq)`, Index `(monitor_id, ts_ms)`, Index `(ts_ms)`; `WITHOUT ROWID`
  wird im Spike gemessen.
- Jede mandantenbezogene Tabelle hat `tenant_id NOT NULL`.

**Repository**

- Interfaces in `internal/store` (z.B. `MonitorRepo.Get(ctx, tenantID, id)`), Implementierungen in
  `internal/store/sqlite` (später `internal/store/postgres`).
- Die Tenant-ID ist Pflichtparameter jeder mandantenbezogenen Methode; ein generischer Isolationstest ruft jede Methode
  mit fremder Tenant-ID auf.
- `sqlc` je Engine eigenes Query-Verzeichnis (`internal/store/sqlite/queries`, `…/postgres/queries`); generierter Code
  wird eingecheckt und in CI auf Aktualität geprüft.
- **Gemeinsame Integrations-Testsuite** (`internal/store/storetest`), die gegen jede Implementierung läuft.

**Migrationen**

- `goose` mit eingebetteten SQL-Migrationen je Datei und Engine; jeder Node migriert seine lokalen Dateien beim Start
  selbst (deterministisch, unabhängig von Raft).
- Nur Expand/Contract: Spalten werden nie im selben Release umbenannt oder gelöscht, in dem ihr Ersatz eingeführt wird
  (NFA-MNT-04).

## Konsequenzen

- Positiv: Keine Busy-Fehler durch Design, kleine Raft-Snapshots, getrennte Wartung von Konfig- und Messdaten,
  PostgreSQL-Weg vorbereitet.
- Negativ: Queries für PostgreSQL müssen doppelt gepflegt werden (bewusst erst P5); zwei Dateien bedeuten zwei
  Migrationspfade.
- Folgearbeiten: SQLite-Spike (AP-0.15), eigener ADR für das PostgreSQL-Backend inkl. Partitionierung und
  Lease-basierter Leaderwahl (P5).

## Approval

| Datum      | Status   | Durch    | Anmerkung                                 |
|------------|----------|----------|-------------------------------------------|
| 2026-10-19 | Proposed | Claude   |                                           |
| 2026-10-19 | Accepted | Projektinhaber | im Chat für alle ADRs 0001–0011 bestätigt |
