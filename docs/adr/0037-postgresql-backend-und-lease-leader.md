# ADR 0037: PostgreSQL-Backend und Lease-basierte Leaderwahl

|                 |                                                                                                                                                                                                                                                              |
|-----------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **Status** | Accepted |
| **Datum**       | 2026-11-19                                                                                                                                                                                                                                                   |
| **Entscheider** | Projektinhaber                                                                                                                                                                                                                                                     |
| **Bezug**       | Z-S-01, Z-S-04, NFA-PORT-03, NFA-MNT-04, NFA-REL-02, FA-CLU-05/06/07, FA-NODE-04, FA-REP-01..05, FA-ALR-03/10, FA-UPT-03..07, FA-PRB-03, PF-04, PF-35, R-03 · Konzept 3, 6.3, E8, E10 · ADR 0004, 0005, 0012, 0013, 0016, 0017, 0018, 0021, 0024, 0025, 0033 |

## Kontext

E10 sieht neben dem Standard (SQLite + Raft je Voter) ein optionales PostgreSQL-Backend vor. Konzept 6.3 beschreibt es
so: gemeinsame Tabellen in PostgreSQL, Heartbeats direkt in PostgreSQL, Leaderwahl über eine Lease in PostgreSQL,
Hochverfügbarkeit der Daten durch die Datenbank (Patroni, Managed DB). Zielgruppe sind kommerzielles Hosting, sehr viele
Monitore und vorhandene DB-Infrastruktur. ADR 0004 hat dafür einen eigenen ADR „inkl. Partitionierung und
Lease-basierter Leaderwahl“ angekündigt. Das Pflichtenheft verlangt eine Repository-Schnittstelle mit den Backends
`embedded` und `postgres` und eine gemeinsame Integrations-Testsuite (NFA-PORT-03, PF-35). Konfiguriert wird über
`STORAGE_BACKEND=embedded|postgres` und `DATABASE_URL`; unterstützt wird PostgreSQL ≥ 16.

**Was der Code heute voraussetzt** (Stand nach P4):

- `store.State` (rund 100 Methoden) und `store.Metrics` (rund 30 Methoden) sind sauber getrennt. Die gemeinsame Suite
  `internal/store/storetest` (rund 3.500 Zeilen, inkl. Mandanten-Isolation) läuft aber nur gegen
  `internal/store/sqlite`. sqlc erzeugt `statedb` und `metricsdb` aus rund 930 Zeilen SQLite-SQL. goose migriert beide
  Dateien (18 + 6 Migrationen).
- Klasse-A-Schreibzugriffe laufen **nur** über `cluster.Node.Apply` → Raft-Log → `FSM.Apply` → `State.Update(tx)`. Die
  Logik steckt in `applyPayload(ctx, tx, cmd)` und hängt nur an `store.StateTx`, nicht an Raft. Dedup, `applied_index`,
  `command_level`, `last_issued_at` und Audit schreibt die FSM in derselben Transaktion.
- Leader-Aufgaben sind über eine Funktion `IsLeader func() bool` verdrahtet: Aggregator, Dispatcher, Flapping, Wartung,
  Systemalerts, Gap-Job, Abgleich aus dem Degraded-Modus, Selbstgesundheit, Schlüssel, Audit-Prune,
  Konfigurations-Migration. Raft-spezifisch sind dagegen `clusterhost` und `membership` (Join, Promotion, `Members`),
  die Node-Gesundheit aus Raft-Heartbeats (`OnHeartbeat`), `OnPeerResumed` → Anti-Entropie, Snapshots, der
  Prüfpunkt-Fingerprint (ADR 0025) und die Recovery-CLI (ADR 0019).
- Klasse B liegt je Voter in `metrics.db` und wird repliziert (ADR 0006, AP-2.4). Rollups, Verdichtung, Retention und
  Purge laufen **auf jedem** Voter über dessen lokale Daten.
- `StateEvent`s (Statuswechsel, Incidents) und das Signal `FSM.Changes()` entstehen auf jedem Node beim Anwenden des
  Logs. Der WebSocket-Hub und `scheduler.Reconcile` hängen daran.

**Recherchierte Fallstricke:**

- **Session-Advisory-Locks** (`pg_advisory_lock`) gehören zu einer Verbindung. Bei PgBouncer im Transaction-Pooling
  funktionieren sie laut PgBouncer-Doku nicht: Sperren bleiben hängen oder landen bei fremden Clients. Eine halb offene
  TCP-Verbindung hält die Sperre, bis der Server sie erkennt; das dauert ohne `tcp_keepalives_*` bzw.
  `idle_session_timeout` bis zu Stunden. Der Halter merkt einen Verlust erst bei der nächsten Abfrage. Ein Fencing-Token
  gibt es nicht. `pg_advisory_xact_lock` ist dagegen transaktionsgebunden und funktioniert mit jedem Pooling.
- **Lease ohne Fencing** ist unsicher: Ein Prozess, der pausiert (GC, VM-Stall), schreibt nach Ablauf seiner Lease
  weiter. Abhilfe ist ein monoton steigender Token (Epoche), den jede geschützte Schreiboperation in derselben
  Transaktion prüft (Kleppmann, „How to do distributed locking“).
- **Uhren:** Läuft eine Lease nach den Uhren der Nodes ab, ist sie bei Uhrversatz unsicher (ADR 0020). Die Ablaufzeit
  muss deshalb von **einer** Uhr kommen, der Datenbankuhr (`clock_timestamp()`). Der Halter misst seine Frist mit der
  eigenen monotonen Uhr ab dem *Beginn* der letzten erfolgreichen Verlängerung.
- **goose:** Mehrere Voter starten gleichzeitig gegen dieselbe Datenbank. Der Session-Locker von goose braucht
  Session-Affinität. Seit v3.26 gibt es `lock.NewPostgresTableLocker` (Lease-Tabelle mit Heartbeat, ohne
  Session-Affinität); im Projekt ist v3.30.1 im Einsatz.
- **sqlc:** Engines lassen sich nicht in einer Query-Datei mischen (`?` gegen `$1`, `sqlc.slice` gegen
  `= ANY($1::text[])`, `MAX(a, b)` gegen `GREATEST`). Je Engine entstehen eigene Pakete und Typen (PF-35). Mit
  `sql_package: pgx/v5` werden Nullable-Spalten zu `pgtype.*` statt `sql.Null*`.
- **Portabilität:** `LIKE` ist in SQLite für ASCII case-insensitiv, in PostgreSQL nicht (→ `ILIKE` mit Escaping).
  `ORDER BY` auf Text sortiert in PostgreSQL nach der Locale der Datenbank. Keyset-Paging über Text-IDs braucht deshalb
  `COLLATE "C"`, sonst weicht die Reihenfolge von SQLite ab. `BLOB` wird `BYTEA`. Zeitstempel als `BIGINT` ms und
  Booleans als `INTEGER 0/1` bleiben portabel (Pflichtenheft 7.2); `timestamptz` und `boolean` würden den gemeinsamen
  Mapping-Code teilen. Partitionierte Tabellen brauchen den Partitionsschlüssel im Primärschlüssel.
- **pgx v5** (MIT) ist der Standardtreiber; `pgxpool` bringt `CopyFrom` für Batch-Inserts, und `pgx/v5/stdlib` liefert
  ein `*sql.DB` für goose.
- **Testumgebung:** `testcontainers-go` (MIT) braucht Docker bzw. einen Podman-Socket. `fergusstrange/embedded-postgres`
  (MIT) lädt PostgreSQL-Binaries (zonky, Apache-2.0) zur Testzeit aus Maven herunter. Beides ist eine zusätzliche
  Abhängigkeit. GitHub Actions bietet Service-Container ohne neue Go-Abhängigkeit.

## Optionen

### Frage 1 – Betriebsmodell

#### Option 1A: Raft bleibt für Konsens, PostgreSQL ist nur Speicher

Alle Voter betreiben weiter Raft. Die FSM schreibt nicht in SQLite, sondern in die gemeinsame Datenbank, und nur der
jeweils erste Node, der einen Index anwendet, schreibt wirklich
(`UPDATE fsm_meta SET applied_index … WHERE applied_index < $1`).

- Vorteile: Leaderwahl, Membership, Feature-Gates und Degraded-Modus bleiben unverändert.
- Nachteile: Zwei Verfügbarkeitsbedingungen (Raft-Quorum **und** Datenbank) statt einer. Die HA der Datenbank bringt
  nichts, weil das Quorum weiterhin 3 Voter verlangt. Snapshots, Restore und Fingerprint ergeben bei einer gemeinsamen
  Kopie keinen Sinn. Das widerspricht Konzept 6.3.

#### Option 1B: Hybrid – Raft + SQLite für Klasse A, PostgreSQL nur für Klasse B

Konfiguration und Zustand bleiben wie heute. Nur Heartbeats, Minuten und Rollups liegen gemeinsam in PostgreSQL; die
Heartbeat-Replikation entfällt.

- Vorteile: Kleinster Umfang. FSM, Determinismus, Membership, PKI und Recovery bleiben unberührt, und das eigentliche
  Mengenproblem (Konzept 17: „ab mehreren Tausend Monitoren“) ist gelöst.
- Nachteile: Voter bleiben zustandsbehaftet (Volumes für `raft/` und `state.db`), und das Raft-Quorum bleibt nötig. Das
  ist nicht das Modell aus Konzept 6.3, und Betreiber mit Managed DB gewinnen keine einfachere HA. Ein dritter
  Betriebsmodus entsteht, der getestet werden muss.

#### Option 1C: PostgreSQL ersetzt Raft (Lease-Leader, gemeinsamer Zustand)

Im Modus `postgres` läuft kein Raft. Klasse A und B liegen in PostgreSQL, die Leaderwahl läuft über eine Lease in der
Datenbank. Die Voter sind bis auf einen kleinen lokalen Puffer zustandslos.

- Vorteile: Entspricht Konzept 6.3 und E10. Die Datenbank ist die einzige Verfügbarkeitsbedingung: Ein einzelner Voter
  bleibt voll funktionsfähig, solange er die Datenbank erreicht (kein Mehrheitsquorum der Voter nötig). Keine
  Replikation, keine Snapshots, kein Fingerprint-Abgleich. Voter können als Kubernetes-Deployment ohne PVC laufen.
- Nachteile: Größter Umfang (siehe Konsequenzen). Die Schicht „Koordination“ muss hinter einer Schnittstelle
  austauschbar werden. Ein zweiter Schreibpfad für Klasse A entsteht, der Degraded-Modus muss neu gedacht werden, und
  die Datenbank wird zum Single Point of Failure, wenn sie selbst nicht hochverfügbar betrieben wird.

### Frage 2 – Leaderwahl (nur bei 1C)

#### Option 2A: Session-Advisory-Lock (`pg_try_advisory_lock`) auf einer eigenen Verbindung

- Vorteile: Wenig Code, Freigabe beim Verbindungsabbruch automatisch.
- Nachteile: Funktioniert nicht hinter PgBouncer im Transaction-Pooling. Ein Verlust wird erst spät erkannt (halb offene
  Verbindungen, TCP-Keepalive). Kein Fencing-Token, also kein Schutz gegen einen pausierten Alt-Leader. Die
  Failover-Zeit hängt von Servereinstellungen ab, die wir nicht kontrollieren (NFA-REL-02).

#### Option 2B: Lease-Tabelle mit Epoche als Fencing-Token und Ablauf nach Datenbankuhr

Eine Zeile `leader_lease(id=1, holder, epoch, expires_at)`. Erwerb und Verlängerung erfolgen mit einem einzigen
bedingten `UPDATE … WHERE expires_at < clock_timestamp() OR holder = $me`; beim Wechsel des Halters steigt `epoch`.
Leader-Schreibzugriffe prüfen die Epoche in ihrer Transaktion.

- Vorteile: Unabhängig vom Pooling-Modus. Fencing in der Datenbank selbst, Ablauf nur nach einer Uhr, Failover-Zeit über
  TTL steuerbar und testbar. Die Lease ist in der Admin-API sichtbar.
- Nachteile: Etwas mehr eigener Code. Eine Schreiblast von einer Zeile je Sekunde. Seiteneffekte außerhalb der Datenbank
  (Alert-Versand) sind nicht gefenct, nur deren Buchung.

#### Option 2C: Externer Koordinator (etcd, Kubernetes-Lease)

- Nachteile: Eine weitere Abhängigkeit, die Z8 widerspricht. Kubernetes nur für einen Teil der Betreiber.

### Frage 3 – Schreibpfad für Klasse A (nur bei 1C)

#### Option 3A: Nur der Leader schreibt, Follower leiten weiter (wie heute)

- Vorteile: Gleiche Semantik wie im Raft-Modus, `issued_at_ms` von einer Uhr.
- Nachteile: Inter-Node-Forwarding bleibt Pflicht. Schreiben fällt aus, solange kein Leader feststeht (bis TTL), obwohl
  die Datenbank erreichbar ist.

#### Option 3B: Jeder Voter wendet Befehle selbst an, serialisiert über eine Transaktionssperre

`BEGIN; SELECT pg_advisory_xact_lock(<Konstante>); … applyPayload(tx, cmd) …; COMMIT`. Derselbe `pb.Command`, dieselbe
Funktion `applyPayload`, dieselbe Dedup-Tabelle. `applied_index` wird ein lückenloser Zähler, der unter der Sperre
hochgezählt wird. `issued_at_ms` setzt die Transaktion aus der Datenbankuhr, bevor `applyPayload` läuft. Leader-Befehle
tragen zusätzlich die Epoche und werden nur angewandt, wenn sie noch gültig ist.

- Vorteile: Die gesamte FSM-Logik wird wiederverwendet. Die Serialisierung entspricht dem heutigen Single-Writer, sodass
  Revisionsprüfungen, Planlimits und Zählungen unverändert korrekt bleiben. Funktioniert mit PgBouncer. Kein Forwarding
  nötig, und API-Schreibzugriffe gehen auch ohne Leader.
- Nachteile: Alle Klasse-A-Schreibzugriffe laufen seriell (unkritisch: Klasse A ist selten, Heartbeats nehmen die Sperre
  nicht). Ergebnis „unbekannt“ bei Verbindungsabbruch während `COMMIT` (wie PF-04, gelöst durch Request-ID).

#### Option 3C: `SERIALIZABLE` ohne globale Sperre

- Vorteile: Parallelität.
- Nachteile: Jeder Befehl braucht Retry-Logik bei Serialisierungsfehlern. Die FSM-Funktionen wurden für einen einzelnen
  Schreiber geschrieben; subtile Unterschiede ließen sich nur schwer testen.

### Frage 4 – Änderungsfeed für andere Nodes (nur bei 1C)

#### Option 4A: `LISTEN/NOTIFY`

- Nachteile: Braucht eine Session (nicht hinter Transaction-Pooling), Benachrichtigungen gehen bei Verbindungsabbruch
  verloren.

#### Option 4B: Tabelle `state_events` im selben Commit, Polling des Index

Die beim Anwenden gesammelten `StateEvent`s werden als JSON mit dem `applied_index` gespeichert. Jeder Node fragt jede
Sekunde `fsm_meta.applied_index` ab und liest neue Einträge in Indexreihenfolge. Daraus entstehen `Changes()` und
`OnStateChange` wie bisher.

- Vorteile: Verlustfrei, geordnet, poolingfest, testbar. `NOTIFY` kann später als reiner Weckruf dazukommen.
- Nachteile: Bis zu 1 s Verzögerung für Änderungen anderer Nodes; Retention-Job nötig (24 h).

### Frage 5 – Heartbeats und Ausfall der Datenbank (nur bei 1C)

#### Option 5A: Direkt in PostgreSQL schreiben, bei Fehler verwerfen

- Nachteile: Jede Störung der Datenbank reißt Lücken in die Messdaten.

#### Option 5B: Lokaler Spool (SQLite-Datei `spool.db`) mit Nachlieferung

Der Scheduler schreibt wie heute lokal. Ein Shipper überträgt die Daten in Batches (`CopyFrom` in eine Staging-Tabelle
bzw. `INSERT … ON CONFLICT DO NOTHING`) und löscht sie danach. Begrenzt auf 24 h bzw. 100 MB wie der Probe-Puffer
(FA-PRB-03). Schlüssel und Inkarnation bleiben `(node_id, incarnation, seq)`.

- Vorteile: Keine Lücken bei kurzen DB-Ausfällen oder Failover (Patroni). Wiederverwendung von `store/sqlite` und des
  Probe-Puffers aus AP-5.1.
- Nachteile: Doppelter Schreibweg; Rollups müssen auf die Nachlieferung warten (wie heute auf die Replikation).

### Frage 6 – Degraded-Modus (E8, FA-ALR-10) im Modus `postgres`

#### Option 6A: Kein Versand ohne Datenbank, nur Erkennung

Ohne erreichbare Datenbank gibt es keinen bestätigten Zustand und keine Kanäle mit entschlüsselbaren Zielen. Der Node
misst weiter, puffert lokal, meldet `degraded` in `/health` und in Prometheus. Nach der Rückkehr bewertet der Leader die
Lücke nach (ADR 0024 Punkt 7).

- Vorteile: Einfach; keine Klartext-Geheimnisse im Speicher, keine zweite Konfigurationskopie.
- Nachteile: Bei einem DB-Totalausfall gehen keine Monitor-Alerts raus. Das weicht von E8 ab; im Modus `postgres`
  übernimmt die HA der Datenbank die Rolle des Quorums.

#### Option 6B: Lokale Schattenkopie von Klasse A und Degraded-Versand wie ADR 0024

Jeder Node hält eine periodische Kopie der Konfiguration in einer lokalen SQLite-Datei. Absender ist der kleinste
erreichbare Node ohne Datenbankzugang; bewertet werden nur eigene Heartbeats.

- Vorteile: E8 gilt auch hier.
- Nachteile: Zweite Kopie aller Klasse-A-Daten inkl. verschlüsselter Geheimnisse auf jedem Node; Export-Logik,
  Konsistenzfragen; nennenswerter zusätzlicher Umfang.

### Frage 7 – Testumgebung für die gemeinsame Suite

#### Option 7A: testcontainers-go

- Vorteile: Selbstständige Tests, bekannte Bibliothek.
- Nachteile: Neue Abhängigkeit mit großem Abhängigkeitsbaum; braucht Docker- bzw. Podman-Socket, auch lokal.

#### Option 7B: embedded-postgres

- Nachteile: Lädt Binaries zur Testzeit aus dem Internet; arm64/macOS-Varianten wechselnd gepflegt.

#### Option 7C: Externe Datenbank über Umgebungsvariable

`UC_TEST_POSTGRES_URL`; in CI ein Service-Container `postgres:16` und `postgres:18`; lokal `make test-postgres` startet
einen Podman- bzw. Docker-Container. Jeder Test legt ein eigenes Schema an und löscht es danach.

- Vorteile: Keine neue Go-Abhängigkeit, schnell, parallel, mehrere PG-Versionen in der Matrix.
- Nachteile: Ohne Variable werden die Postgres-Tests übersprungen; CI muss sie deshalb erzwingen (Fehlschlag statt Skip,
  wenn `CI=true`).

## Entscheidung

Entscheidung: **1C + 2B + 3B + 4B + 5B + 6A + 7A.** Umgesetzt wird in zwei Stufen; für Stufe 1 ist die Aufnahme in
Release 1.0 offen (siehe offene Frage 1).

**1. Betriebsmodell (1C).**

- `STORAGE_BACKEND=postgres` ersetzt Raft vollständig. Klasse A und Klasse B liegen in **einer** PostgreSQL-Datenbank je
  Cluster, in einem eigenen Schema (`DATABASE_SCHEMA`, Standard `pulsequorum`). Die Datenbank *ist* der Cluster.
- **Mischbetrieb ist verboten.**
    - Ein Node mit `postgres` startet nicht, wenn sein Datenverzeichnis Raft-Zustand enthält (`raft/`, `state.db`), und
      umgekehrt.
    - Die Datenbank trägt in `cluster_meta` `backend = postgres` und eine `cluster_id`. Ein Node mit fremder
      `cluster_id` im Datenverzeichnis bzw. Zertifikat startet nicht.
    - `RegisterNode` und `GetStatus` melden das Backend; ein Join zwischen Backends wird abgelehnt
      (`FAILED_PRECONDITION`).
- Der Modus `embedded` bleibt Standard und unverändert (Z-M-08).

**2. Koordinationsschicht.**

- Neue Schnittstelle `cluster.Coordinator` mit `IsLeader`, `Leader`, `Apply`, `ApplyAsLeader`, `Changes`,
  `OnStateChange`, `Status`, `Members`, `AppliedIndex`, `CommandLevel`, `Shutdown`.
- `cluster.Node` (Raft) implementiert sie unverändert; `ApplyAsLeader` entspricht dort `ApplyLocal`.
- Neues Paket `internal/pgcoord` implementiert sie für PostgreSQL.
- `main` und `clusterhost` verdrahten nur noch die Schnittstelle. Raft-spezifische Teile (Membership-Join/Promotion,
  Snapshots, Fingerprint-Prüfpunkt, Recovery-CLI, Heartbeat-Replikation, Anti-Entropie, `raft_stale`-Alert) laufen nur
  im Modus `embedded`. Die Recovery-CLI lehnt den Modus `postgres` mit einer klaren Meldung ab.

**3. Lease (2B).**

- Tabelle
  `leader_lease(id SMALLINT PRIMARY KEY CHECK (id = 1), holder TEXT, epoch BIGINT, acquired_at, expires_at TIMESTAMPTZ)`.
  `TIMESTAMPTZ` ist hier zulässig, weil die Tabelle engine-spezifisch ist und nie über `store` gelesen wird.
- Erwerb und Verlängerung in einer Anweisung:
  `UPDATE … SET holder = $me, epoch = CASE WHEN holder = $me THEN epoch ELSE epoch + 1 END, expires_at = clock_timestamp() + $ttl WHERE id = 1 AND (holder = $me OR expires_at < clock_timestamp()) RETURNING epoch`.
- Standardwerte: `LEASE_TTL` 6 s, Verlängerung jede Sekunde, Bewerbung der anderen Nodes jede Sekunde.
- **Selbstabsetzung:** Der Halter gilt nur so lange als Leader, bis TTL − 1 s seit dem *Start* der letzten erfolgreichen
  Verlängerung vergangen sind (monotone Uhr). Damit endet seine Führung lokal vor dem Ablauf in der Datenbank; schlägt
  eine Verlängerung fehl, setzt er sich spätestens dann ab.
- Failover nach hartem Ausfall ≤ TTL + 1 s ≈ 7 s (NFA-REL-02 < 10 s).
- Beim Shutdown setzt der Leader `expires_at = clock_timestamp()`, sodass ein anderer Node innerhalb von ≤ 1 s übernimmt
  (FA-NODE-04).
- **Fencing:** `ApplyAsLeader` übergibt die Epoche. Die Apply-Transaktion prüft
  `SELECT 1 FROM leader_lease WHERE id = 1 AND holder = $me AND epoch = $e AND expires_at > clock_timestamp() FOR SHARE`;
  ohne Treffer wird der Befehl mit `ErrNotApplied` (nicht Leader) abgelehnt. Alle Leader-Jobs (Aggregator, Dispatcher,
  Systemalerts, Node-Gesundheit, Gap-Job, Abgleich, Rollups) schreiben ausschließlich über `ApplyAsLeader` bzw.
  epochengeprüfte Metrics-Transaktionen.
- Der Alert-Versand selbst ist ein Seiteneffekt außerhalb der Datenbank. Ein pausierter Alt-Leader kann einen Versand
  doppelt auslösen; das bleibt innerhalb von *at least once* (FA-ALR-03) und entspricht dem heutigen Verhalten bei
  `ErrLeadershipLost`.
- Nach einem Leaderwechsel gelten die Karenzen aus ADR 0017 und 0021 (10 s) unverändert.
- **Node-Gesundheit** (ADR 0017): Jeder Node schreibt jede Sekunde
  `node_presence(node_id, seen_at = clock_timestamp())`. Der Leader setzt `SetNodeHealth` mit denselben Schwellen (10 s)
  nach der Datenbankuhr. `OnHeartbeat` aus Raft entfällt in diesem Modus.

**4. Schreibpfad Klasse A (3B).**

- `pgcoord.Apply` führt in einer Transaktion aus:
    1. `pg_advisory_xact_lock(<Konstante>)`
    2. Dedup über `request_id`
    3. `issued_at_ms` aus `clock_timestamp()` setzen
    4. Feature-Gate prüfen (`command_level`, ADR 0018)
    5. `applyPayload(tx, cmd)`, Audit, `last_issued_at`
    6. `applied_index + 1`
    7. `state_events` schreiben
    8. `COMMIT`
- `applyPayload` und die Hilfsfunktionen werden dafür aus `FSM.Apply` herausgelöst und von beiden Backends genutzt. Es
  gibt **keine** zweite Fachlogik.
- Fehlerabbildung wie ADR 0012 A8: Abbruch vor `COMMIT` → `ErrNotApplied`; Verbindungsverlust während `COMMIT` →
  `ErrUnknownOutcome`; Wiederholung mit gleicher Request-ID ist idempotent (FA-CLU-06, PF-04).
- **Determinismus** (FA-CLU-07) ist im Modus `postgres` nicht mehr Voraussetzung, weil nur eine Kopie existiert;
  `applyPayload` bleibt trotzdem deterministisch, weil es geteilt wird.
- Snapshot, Restore und Fingerprint-Abgleich (ADR 0025 `fingerprint_mismatch`) entfallen. `Fingerprint` bleibt als
  Methode für Export- und Migrationsprüfungen.
- **Feature-Gates** (ADR 0018) gelten weiter: `command_level` liegt in der gemeinsamen `fsm_meta`. Der Leader hebt ihn
  an, sobald alle registrierten, nicht gesperrten Voter mit Präsenz in den letzten 10 min ihn unterstützen.

**5. Änderungsfeed (4B).** Tabelle `state_events(applied_index BIGINT PRIMARY KEY, events JSONB)`. Jeder Node pollt jede
Sekunde (konfigurierbar) und liefert Ereignisse geordnet an `OnStateChange`, das Signal an `Changes()`. Der Hub filtert
wie bisher zuerst nach Mandant. Die Retention von 24 h übernimmt ein Leader-Job.

**6. Klasse B.**

- Heartbeats, Serien, Latenz-Minuten, Rollups, Watermarks und Purge-Marken liegen gemeinsam in PostgreSQL. Replikation
  und Anti-Entropie entfallen.
- `heartbeats` ist **von Anfang an** nach `ts_ms` tageweise partitioniert (deklarativ, ohne `pg_partman`). Eine spätere
  Umstellung erfordert sonst ein Umkopieren der größten Tabelle.
    - Primärschlüssel `(node_id, incarnation, seq, ts_ms)`. Ein Heartbeat trägt immer denselben `ts_ms`, daher bleibt
      `ON CONFLICT DO NOTHING` idempotent.
    - Ein Leader-Job legt Partitionen 3 Tage im Voraus an. Die Retention entfernt ganze Partitionen jenseits der
      Rohdaten-Aufbewahrung und löscht nur in der Randpartition zeilenweise.
    - `latency_minutes` und `uptime_rollups` bleiben unpartitioniert (kompaktes Schema aus ADR 0016, `series_id BIGINT`,
      jetzt clusterweit statt knotenlokal).
- Rollups, Verdichtung, Retention und Purge laufen **nur auf dem Leader** und schreiben epochengeprüft. FA-UPT-03
  (gleiche Uptime auf allen Votern) ist damit trivial erfüllt.
- Die Wartebedingung aus AP-2.4 („erst nach vollständiger Replikation, höchstens 1 h“) wird zu: erst, wenn alle Nodes
  mit Präsenz ihren Spool bis zum Bucket-Ende geleert haben (`node_presence.flushed_until_ms`), höchstens 1 h.
- Lokal bleibt nur `spool.db` (5B), eine SQLite-Datei mit dem vorhandenen `store/sqlite`-Code. `degraded_alerts` wird
  bei 6A nicht gebraucht; bei 6B käme es in dieselbe Datei.
- Die Speicherschnittstelle `store.Metrics` wird dafür aufgeteilt: Replikationsmethoden (`StreamPositions`,
  `AdvanceStreamPosition`, `HeartbeatsAfter`, `LastSeq`) in eine eigene Schnittstelle `store.Replicated`, die nur
  `embedded` erfüllt.

**7. Degraded-Modus (6A) – offene Frage 2.**

- Im Modus `postgres` gilt ein Node als degraded, wenn er seit `DEGRADED_AFTER` weder eine gültige Lease lesen noch die
  Datenbank erreichen kann.
- Er misst weiter, puffert im Spool, meldet `degraded` in `/health`, Prometheus und Cluster-Ansicht und versendet
  **keine** Monitor-Alerts.
- Nach der Rückkehr greift die Nachbewertung der Lücke (ADR 0024 Punkt 7, ADR 0033) unverändert, weil sie nur
  `last_issued_at` und die Heartbeats braucht.
- Die Dokumentation stellt klar: Im Modus `postgres` ersetzt die HA der Datenbank das Voter-Quorum. Ein einzelner
  Datenbankserver ist ein Single Point of Failure.

**8. Abfrageschicht und Migrationen.**

- `internal/store/postgres` mit eigenem `queries/state.sql` und `queries/metrics.sql` und sqlc `engine: postgresql`,
  `sql_package: pgx/v5` in derselben `sqlc.yaml`.
- Eine eigene Migrationsreihe je Engine, für Postgres in **einer** Reihe (`migrations/postgres/`), weil es nur eine
  Datenbank gibt. Die Basis-Migration bildet den Stand der 18 + 6 SQLite-Migrationen in einem Schritt ab; ab dann gilt
  Expand/Contract (NFA-MNT-04).
- **Gegen Auseinanderlaufen (PF-35):** Ein Prüfskript in `make generate-check` vergleicht die `-- name:`-Liste beider
  Engines; jede Query braucht ihr Gegenstück oder eine Begründung in einer Ausnahmeliste.
- **Portabilitätsregeln** (ergänzen ADR 0004):
    - Text-Schlüssel mit `COLLATE "C"`
    - Suche mit `ILIKE` und Escaping von `%`/`_`
    - `GREATEST` statt `MAX(a, b)`
    - `= ANY($n::text[])` statt `sqlc.slice`
    - `BYTEA` statt `BLOB`
    - Zeitstempel `BIGINT` ms, Booleans `INTEGER` mit `CHECK (x IN (0, 1))`
    - JSON in gemeinsamen Pfaden als `TEXT`
- goose mit `goose.DialectPostgres` über `pgx/v5/stdlib` und `lock.NewPostgresTableLocker`, damit parallel startende
  Voter nur einmal migrieren, auch hinter PgBouncer.
- Ein älteres Binary muss gegen ein neueres, nur erweitertes Schema starten (Rolling Update, NFA-PORT-04); das wird
  getestet. Ein Binary verweigert den Start, wenn das Schema eine Contract-Migration enthält, die es nicht kennt
  (Markierung in `cluster_meta.min_binary_version`).

**9. Treiber und Pool.**

- `github.com/jackc/pgx/v5` (MIT), aktuelle v5-Version beim Einbau; `pgxpool` für die Anwendung, `stdlib` nur für goose.
- Poolgröße konfigurierbar (`DATABASE_MAX_CONNS`, Standard 10).
- Alle Anweisungen sind mit PgBouncer im Transaction-Pooling verträglich: keine Session-Sperren, keine `SET` ohne
  `LOCAL`, keine serverseitigen Prepared Statements über Transaktionen hinweg (`default_query_exec_mode` konfigurierbar,
  Standard `cache_describe`).

**10. Sicherheit** (vom Hauptagenten umzusetzen).

- `DATABASE_URL` ist ein Geheimnis:
    - `DATABASE_URL_FILE` wird unterstützt.
    - Das Passwort wird in Logs, Fehlermeldungen, `/health` und Admin-API maskiert.
    - Bei `sslmode` < `verify-full` zu einem Nicht-Loopback-Host gibt es beim Start eine Warnung;
      `DATABASE_TLS_REQUIRED=true` macht daraus einen Abbruch.
- Geheimnisse in der Datenbank bleiben wie in SQLite verschlüsselt bzw. gehasht (ADR 0011, 0015). Wer Datenbankzugriff
  hat, sieht keine Klartext-Geheimnisse, solange er den Master-Key nicht hat. Der Master-Key bleibt außerhalb der
  Datenbank.
- **Mandantentrennung:** Jede Query trägt die Tenant-ID wie in SQLite. `storetest` mit allen Isolationstests läuft gegen
  beide Engines und ist Merge-Bedingung. Row-Level-Security ist **nicht** Teil dieses ADR (offene Frage 4).
- **Node-Identität:** Join-Token, CSR und mTLS bleiben (Probes, `GetStatus`, Uhrvergleich). Ein gesperrter Node verliert
  damit sein Zertifikat, **nicht** aber den Datenbankzugang. Die Betriebsdoku verlangt deshalb Datenbank-Zugangsdaten je
  Node bzw. deren Rotation beim Sperren.

**11. Wechsel des Backends.**

- Nur offline, über Export/Import aus AP-5.5 (Z-S-04).
- `embedded → postgres`: Cluster stoppen, Export vom Leader (Klasse A vollständig, Klasse B optional),
  `admin import --target postgres` in eine **leere** Datenbank, Fingerprint-Vergleich von Klasse A, neue Voter mit
  `postgres` starten. `postgres → embedded` geht analog.
- Ein Live-Wechsel und ein Mischbetrieb während des Wechsels sind ausgeschlossen. Ohne AP-5.5 ist der Modus `postgres`
  nur für Neuinstallationen nutzbar.

**12. Tests (7A, testcontainers-go).**

- `github.com/testcontainers/testcontainers-go` (MIT) mit dem Modul `modules/postgres`, nur in Testcode, nie im
  Produkt-Binary; die Lizenzprüfung (ADR 0010) deckt den Abhängigkeitsbaum ab.
- Ein Container je Testpaket (`TestMain`), ein eigenes Schema je Test; Images `postgres:16` und `postgres:18`, per
  Digest gepinnt, als Matrix.
- Ohne Docker- bzw. Podman-Socket werden die Tests lokal übersprungen; bei `CI=true` schlagen sie fehl, statt zu
  überspringen.
- `storetest.RunState`/`RunMetrics` laufen gegen beide Engines.
- Zusätzlich:
    - Lease-Tests: Erwerb, Verlängerung, Ablauf, Selbstabsetzung, Fencing eines pausierten Halters, Shutdown-Übergabe
    - Clustertest mit 3 Votern über eine Datenbank: Leader hart beenden → neuer Leader < 10 s, genau ein Alert je
      Übergang (AS-03 sinngemäß)
    - DB-Neustart während Last: kein Heartbeat verloren (Spool)
    - Test mit PgBouncer im Transaction-Pooling (Compose unter `deploy/postgres-dev/`)

**Stufen.**

- **Stufe 1** (AP-5.4): Punkte 1–6 und 8–10, Spool, 6A, Tests, Compose-Beispiel, Betriebsdoku.
- **Stufe 2** (nach AP-5.5): Import/Export-Wechsel (11).
- Spätere Erweiterungen, jeweils mit eigenem ADR, falls gewünscht: Option 6B, `NOTIFY` als Weckruf, RLS.

## Konsequenzen

- **Positiv:**
    - Konzept 6.3 wird umgesetzt, ohne die Fachlogik zu duplizieren: `applyPayload` und die Leader-Jobs bleiben eine
      Implementierung.
    - Voter im Modus `postgres` sind nahezu zustandslos; ein Voter allein reicht für volle Funktion, solange die
      Datenbank erreichbar ist.
    - Lease und Fencing hängen nicht von Node-Uhren und Pooling-Modus ab; das Failover ist über die TTL steuerbar.
    - Die gemeinsame Suite deckt beide Engines ab; das Prüfskript macht fehlende Gegenstücke sichtbar.
    - Rollups und Uptime sind im Modus `postgres` clusterweit identisch, und große Installationen bekommen
      Partitionierung und eine Datenbank, die der Betreiber skaliert.
- **Negativ / Risiken:**
    - **Umfang:** Etwa 930 Zeilen SQL portieren, eine zweite Implementierung mit rund 130 Methoden, eine neue
      Koordinationsschicht mit Umbau von `main` und `clusterhost`, Lease, Feed, Spool, Partitionen, CI-Job. Das
      entspricht grob dem Umfang von P2 (Schätzung im Umsetzungsplan: 20–28 Arbeitstage). R-03 (doppelte Backends binden
      Kapazität) tritt ein.
    - Zwei Klasse-A-Schreibpfade (Raft-FSM und `pgcoord`) müssen dieselben Seiteneffekte erzeugen (Audit, Dedup,
      Ereignisse). Abhilfe ist ein gemeinsamer Tabellentest „Befehl → Zustand“ gegen beide Pfade.
    - Im Modus `postgres` gibt es bei einem DB-Ausfall keine Monitor-Alerts (6A); das ist eine bewusste Abweichung von
      E8 und muss im Pflichtenheft vermerkt werden.
    - Die globale Transaktionssperre begrenzt den Klasse-A-Durchsatz; bei sehr vielen Statuswechseln je Sekunde wäre das
      neu zu bewerten (Messung im Lasttest).
    - Ein gesperrter Node behält seinen Datenbankzugang, bis der Betreiber die Zugangsdaten wechselt.
    - Partitionsverwaltung ist eigener Code; ein Fehler dort kann Inserts blockieren. Abhilfe: Default-Partition plus
      Systemalert `partition_missing`.
    - Der Modus `postgres` verdoppelt die Testmatrix (Unit, Integration, Cluster) dauerhaft.
- **Folgearbeiten:**
    - Pflichtenheft:
        - FA-ALR-10 und FA-UPT-03 mit Vermerk zum Modus `postgres`
        - neue Konfiguration `LEASE_TTL`, `DATABASE_URL_FILE`, `DATABASE_SCHEMA`, `DATABASE_MAX_CONNS`,
          `DATABASE_TLS_REQUIRED`
        - ggf. Verschiebung von Z-S-01/NFA-PORT-03 (offene Frage 1)
    - ADR 0004 und 0025 erhalten einen Hinweis „ergänzt durch 0037“.
    - Betriebsdoku `docs/betrieb/postgres.md`: HA-Anforderung, PgBouncer, Zugangsdaten je Node, Backup über
      PostgreSQL-Mittel.
    - Compose-Beispiel `deploy/compose/postgres/`.
    - Systemalerts `database_unreachable` und `partition_missing`; Metriken für Lease-Epoche, Spool-Größe und
      Feed-Rückstand.

**Offene Fragen an Projektinhaber:**

1. **Umfang/Release:** Z-S-01 ist ein *Soll*-Kriterium, NFA-PORT-03 verlangt Postgres aber als *Muss* in P5. Soll Stufe
   1 in Release 1.0 (P5 wird dann deutlich länger), oder wird das PostgreSQL-Backend auf 1.1 verschoben und NFA-PORT-03
   für 1.0 auf „Schnittstelle + Suite vorbereitet“ geändert? Empfehlung im Umsetzungsplan: verschieben, in P5 nur die
   Vorarbeiten (Koordinations-Schnittstelle, Aufteilung `store.Replicated`, Prüfskript).
2. **Degraded im Modus `postgres`:** Ist 6A (kein Versand bei DB-Ausfall, nur Erkennung und Nachbewertung) akzeptabel,
   oder muss E8 auch hier gelten (6B, deutlich mehr Umfang)?
3. **Option 1B (Hybrid)** als kleinerer Zwischenschritt für Installationen, die nur das Mengenproblem haben – gewünscht
   oder bewusst nicht?
4. **Row-Level-Security** als zweite Verteidigungslinie der Mandantentrennung: später als eigener ADR, oder schon in
   Stufe 1?
5. **Unterstützte Versionen:** PostgreSQL 16 bis 18 in der CI-Matrix, PgBouncer im Transaction-Pooling offiziell
   unterstützt – einverstanden?
6. **Backend-Wechsel:** Reicht der Offline-Wechsel über Export/Import (mit Ausfallzeit), und soll der Rückweg
   `postgres → embedded` ebenfalls unterstützt und getestet werden?

## Approval

| Datum      | Status   | Durch  | Anmerkung |
|------------|----------|--------|-----------|
| 2026-11-19 | Proposed | Claude |           |
| 2026-11-20 | Accepted | Projektinhaber | im Chat; Tests auf 7A (testcontainers-go) geändert; offene Fragen wie vorgeschlagen (Stufe 1 nach Release 1.0) |
