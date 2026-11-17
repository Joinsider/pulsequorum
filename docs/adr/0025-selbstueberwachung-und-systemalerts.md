# ADR 0025: Selbstüberwachung, Systemalerts und Prometheus-Metriken

| | |
|---|---|
| **Status** | Proposed |
| **Datum** | 2026-11-17 |
| **Entscheider** | Projektinhaber |
| **Bezug** | FA-OBS-01/02, FA-REP-05/06, FA-PKI-06, Z-S-03, AS-05, PF-12, PF-21; ADR 0012 (A7, Snapshot-Timeout), 0013 (WAL-Größe), 0014 (Outbox), 0017 (Node-Gesundheit), 0018 (Feature-Gates, gemischte Versionen), 0020 (Uhrversatz), 0022 (Selbstgesundheit), 0023 (Drosselung), 0024 (Degraded-Modus) |

## Kontext

Der Cluster erkennt schon viele Störungen, meldet sie aber nur im Log, in der Health-API oder in der Cluster-Ansicht. Ein Plattform-Admin erfährt davon nicht aktiv. Gefordert ist:

- **FA-OBS-01:** Systemalerts an einen plattformweiten Admin-Kanal für folgende Störungen:
  - Voter offline
  - Quorum verloren bzw. wiederhergestellt
  - Replikationsrückstand
  - Uhrzeitabweichung > 2 s
  - Speicherplatz knapp (< 15 %)
  - WAL-Datei > 512 MB
  - Raft-Stand `stale`
  - Zertifikatsablauf
  - Inkarnationswechsel
  - abweichender FSM-Fingerprint
- **FA-REP-05:** Ab einem Replikationsrückstand von 5 min (Standard) gibt es einen Systemalert.
- **FA-REP-06:** Ein unerwarteter Inkarnationswechsel eines Nodes (Datenverlust) wird gemeldet.
- **FA-PKI-06:** Vor dem Ablauf der CA und der Node-Zertifikate wird gewarnt, 30, 7 und 1 Tag vorher.
- **FA-OBS-02:** Prometheus-Metriken unter `/metrics`, optional, abschaltbar und separat absicherbar.
- **Offene Systemalerts aus früheren ADRs:**
  - Node in der Konnektivität eingeschränkt (ADR 0022, AS-05)
  - gemischte Versionen über längere Zeit (ADR 0018)
  - Snapshot-Timeout beim Join (ADR 0012)

Was schon da ist:

| Signal | Wo |
|---|---|
| Voter erreichbar (`healthy`) | Register, der Leader entscheidet (ADR 0017) |
| Konnektivität eingeschränkt | Register (ADR 0022) |
| Uhrversatz je Peer | `clockwatch` auf jedem Voter (ADR 0020) |
| Replikationsrückstand | `replication.PeerStatus.Lag` (nur die eigene Sicht des Nodes) |
| Raft-Stand `stale` | `/api/v1/health` des jeweiligen Nodes |
| WAL-Größe | `sqlite.DB.WALSize` |
| Inkarnation | lokal in `metrics.db`; nicht im Register |
| CA-Zertifikat | replizierter Zustand; Node-Zertifikate nur auf dem jeweiligen Node |
| FSM-Fingerprint | wird berechnet, aber im Betrieb nie zwischen Nodes verglichen |
| Degraded-Zustand | `internal/degraded` (ADR 0024) |
| Speicherplatz | wird nicht gemessen |

Der Leader fragt alle Voter alle 15 s per `GetStatus` ab. Prometheus-Metriken gibt es noch nicht.

## Optionen

### Wo liegt der Admin-Kanal?

**A1: Kanal aus der Umgebung (`SYSTEM_ALERT_URL`) auf jedem Node, lokal versendet**

- Vorteile: Funktioniert auch ohne Quorum.
- Nachteile:
  - Das Geheimnis liegt im Klartext in der Konfiguration jedes Nodes.
  - Keine UI, kein Testversand, keine Ereignisfilter, keine Drosselung über den Cluster.
  - Jeder Node meldet selbst, also kommen Duplikate.

**A2: Normale Benachrichtigungskanäle in einem reservierten Systemmandanten, versendet über die Outbox**

- Vorteile: Verschlüsselung, Maskierung, UI, Testversand, Rate-Limit, Bündelung und *at least once* sind schon gebaut. Pro Störung meldet nur der Leader.
- Nachteile: Ohne Quorum gibt es keine Outbox. Ausgerechnet „Quorum verloren“ ginge so nicht raus.

**A3: A2, dazu „Quorum verloren“ über den Degraded-Absender aus ADR 0024**

- Vorteile: Wie A2. Den einen Fall ohne Leader deckt der Pfad ab, der dafür schon existiert. Er liest die Systemkanäle aus dem lokalen Zustand und kennzeichnet den Alert als unbestätigt.
- Nachteile: Zwei Versandwege für Systemalerts.

### Wer erkennt die Störungen?

**B1: Jeder Node meldet seine eigenen Störungen**

- Nachteile: Ein ausgefallener Node meldet nichts. Gleichartige Störungen kommen mehrfach. Die Hysterese wäre lokal und ginge beim Neustart verloren.

**B2: Der Leader sammelt alle Signale über die bestehende `GetStatus`-Runde, bewertet sie und schreibt nur Zustandswechsel per Raft**

- Vorteile: Eine Entscheidungsstelle, wie bei `SetNodeHealth` und `SetNodeConnectivity`. Der Zustand überlebt einen Leaderwechsel, und die Zustellung folgt ADR 0014.
- Nachteile: Neue additive Felder in `GetStatus`, ein neuer Befehl und eine neue Tabelle.

### Wie entstehen die Metriken?

**C1: `prometheus/client_golang` (Apache-2.0) mit eigener Registry, ohne globalen Zustand**

- Vorteile: Verbreiteter Standard, korrektes Textformat, Histogramme.
- Nachteile: Neue Abhängigkeit, mit `prometheus/common`, `procfs` und `client_model`; `protobuf` und `x/sys` sind schon da.

**C2: Eigenes Textformat (~200 Zeilen)**

- Vorteile: Keine neue Abhängigkeit.
- Nachteile: Histogramme, Escaping und das OpenMetrics-Format müsste man selbst pflegen. Fehler fallen erst beim Scrapen auf.

## Entscheidung

**A3, B2 und C1.**

### 1. Systemmandant und Systemkanäle

- Reservierter Mandant mit der ID `_system`, der beim Bootstrap bzw. beim Anheben auf Level 6 einmal per Raft angelegt wird. Er hat keine Monitore, keine Nutzer und kein Kontingent.
- Seine Kanäle sind gewöhnliche Benachrichtigungskanäle. Sie haben den neuen Ereignistyp `system` und dieselbe Verschlüsselung, Maskierung und Prüfung der Ziele wie Mandantenkanäle.
- Verwaltet werden sie über `/api/v1/admin/system-channels` (Liste, Anlegen, Ändern, Löschen, Testversand), **nur mit der Rolle `platform-admin`**. Die Mandanten-API lehnt `_system` überall ab; die Mandantengrenze bleibt so geschlossen.
- Ist kein Systemkanal eingerichtet, gibt es nur Log, Metrik und den Hinweis in der Admin-UI.

### 2. Signale: additive Felder in `GetStatusResponse`

Jeder Voter meldet:

| Feld | Inhalt |
|---|---|
| `incarnation` | Inkarnation aus `metrics.db` |
| `disk_free_ratio` | Anteil freier Bytes im Datenverzeichnis, per `statfs` |
| `state_wal_bytes`, `metrics_wal_bytes` | WAL-Größen beider Datenbanken |
| `cert_not_after_ms` | Ablauf des eigenen Node-Zertifikats |
| `replication_lag_ms` | größter eigener Rückstand zu einem verbundenen Peer; `-1` = nie aufgeholt |
| `fingerprint_index`, `fingerprint` | letzter Prüfpunkt, siehe Punkt 4 |

Ältere Nodes senden diese Felder nicht. Für sie gibt es kein Signal und keinen Alert. Applied- und Commit-Index sowie `last_contact_ms` liefert `GetStatus` schon heute; daraus leitet der Leader `stale` ab wie die Health-API.

### 3. Bewertung auf dem Leader (`internal/sysalert`)

Ein Collector auf dem Leader bewertet alle 30 s:

| Art (`kind`) | Betreff | Bedingung (Standard) | Hysterese |
|---|---|---|---|
| `voter_offline` | Node | `healthy = false` im Register | sofort (das Register hat schon eine eigene) |
| `node_impaired` | Node | `connectivity_impaired` im Register | sofort |
| `replication_lag` | Node | `replication_lag_ms` ≥ `REPLICATION_LAG_ALERT` (5 min) oder nie aufgeholt seit > 5 min | 2 Bewertungen |
| `clock_skew` | Node | \|Versatz\| > `CLOCK_SKEW_ALERT` (2 s) | 2 Bewertungen |
| `disk_low` | Node | `disk_free_ratio` < `DISK_FREE_ALERT` (0,15) | 2 Bewertungen |
| `wal_large` | Node | eine WAL > `WAL_SIZE_ALERT` (512 MB) | 2 Bewertungen |
| `raft_stale` | Node | Applied-Rückstand bzw. `last_contact` über der Schwelle aus ADR 0012 A9 | 2 Bewertungen |
| `cert_expiry` | CA bzw. Node | Ablauf in ≤ 30 / 7 / 1 Tagen | je Stufe einmal |
| `incarnation_changed` | Node | gemeldete Inkarnation ≠ zuletzt bekannte | einmalig, kein Ende |
| `fingerprint_mismatch` | Prüfpunkt | Fingerprints bei gleichem Index verschieden | einmalig je Prüfpunkt |
| `mixed_versions` | Cluster | `max_command_version` der Voter seit > 24 h verschieden | einmal am Tag |
| `quorum` | Cluster | siehe Punkt 5 | – |

- Ein Alert endet, wenn die Bedingung 2 Bewertungen lang nicht mehr gilt. Dann geht eine Entwarnung raus. Ausnahmen sind die einmaligen Arten.
- Erst nach einer vollständigen `GetStatus`-Runde in der aktuellen Leaderschaft wird bewertet, wie in ADR 0022.
- Meldet ein Node gar nicht, gilt nur `voter_offline`. Für einen Node, den niemand erreicht, entstehen also keine Folgealarme.
- Die letzte bekannte Inkarnation je Node steht im replizierten Zustand (Punkt 6). Ein Leaderwechsel gilt daher nicht als Wechsel. Neu registrierte Nodes werden ohne Alert übernommen.

### 4. FSM-Fingerprint-Prüfpunkte

- Der Leader schlägt jede Stunde `Checkpoint{}` vor; der Befehl ändert nichts.
- Jeder Node berechnet beim Anwenden in derselben Transaktion den Fingerprint und hält ihn nur im Speicher, also nicht repliziert. Der Wert hängt nur vom Zustand ab, `Apply` bleibt deterministisch.
- Die Kosten fallen einmal pro Stunde an. Grob gemessen dauert ein Fingerprint mit 1.000 Monitoren und 30 Tagen Historie unter einer Sekunde; die Abnahme misst das nach.
- Weicht ein Fingerprint bei gleichem `fingerprint_index` ab, meldet der Leader `fingerprint_mismatch` mit den betroffenen Nodes. Er repariert nichts automatisch; das Runbook beschreibt die Neusynchronisation per Snapshot.

### 5. Quorum

- **Verlust:** Der Degraded-Absender jeder Partition (ADR 0024) schickt beim Eintritt in den Degraded-Modus einmal „Quorum lost (x/n nodes reachable)“ an die Systemkanäle aus dem lokalen Zustand. Der Alert ist unbestätigt und hat ein eigenes Präfix für die `alertId`. Er wird im lokalen Protokoll vermerkt, aber nicht gemeldet und nicht beurteilt.
- **Rückkehr:** Der erste Leader mit einer Lücke aus ADR 0024 (`last_issued_at_ms`) legt `quorum`-Entwarnung „Quorum restored after <Dauer>“ an. Kurze Leaderwechsel ohne Lücke (< 5 s) bleiben still.

### 6. Datenmodell (Befehlsversion 6)

- Tabelle `system_alerts(kind, subject, active, level, since_ms, updated_ms, details_json)` mit dem Primärschlüssel `(kind, subject)`. `level` ist die Ablaufstufe bzw. ein Zähler. Die Tabelle geht in den Fingerprint ein.
- Tabelle `node_incarnations(node_id, incarnation, seen_ms)`.
- Befehl `SetSystemAlert{kind, subject, active, level, details}`, nur vom Leader und nur bei Wechseln. Die FSM schreibt die Zeile und legt für jeden aktiven Kanal des Mandanten `_system`, der `system` abonniert, einen Outbox-Eintrag an:
  - `alert_key = sys:<kind>:<subject>:<since>:<active|clear>`
  - Ereignis `system`
  - Nutzlast mit `kind`, `subject`, `active`, `details` und der Absender-Sicht aus ADR 0024
- Befehl `RecordIncarnation{node_id, incarnation}`.
- Befehl `Checkpoint{}`.
- `details` enthält nur Messwerte und Node-IDs, keine Geheimnisse und keine Fehlertexte von Zielen.
- Unter Level 6 wird nichts geschrieben; der Collector protokolliert dann nur.
- Drosselung und Bündelung laufen wie bei Mandantenkanälen (ADR 0023).

### 7. Prometheus

- Abhängigkeit `github.com/prometheus/client_golang`, eine eigene `prometheus.Registry` je Prozess, keine globalen Collectoren.
- **Endpunkt:** `METRICS_ADDR` (Standard leer, also aus) startet einen eigenen Listener mit nur `/metrics`. Er läuft getrennt vom API-Port und lässt sich so per Firewall oder Netzwerk absichern.
- **Optionaler Bearer-Token:** `METRICS_TOKEN` oder `METRICS_TOKEN_FILE`, Vergleich in konstanter Zeit.
- Es gibt keine Labels mit Mandanten- oder Monitornamen, nur Typen, Node-IDs und Zustände. So bleiben die Metriken mandantenneutral und die Kardinalität klein.

| Metrik | Typ |
|---|---|
| `uptimecluster_checks_total{type,result}` | Counter |
| `uptimecluster_check_duration_seconds{type}` | Histogramm |
| `uptimecluster_checks_running` | Gauge |
| `uptimecluster_raft_state{state}` | Gauge |
| `uptimecluster_raft_applied_index`, `uptimecluster_raft_commit_index`, `uptimecluster_raft_last_contact_seconds` | Gauges |
| `uptimecluster_replication_lag_seconds{peer}` | Gauge |
| `uptimecluster_replication_dropped_total{peer}` | Counter |
| `uptimecluster_db_size_bytes{db}`, `uptimecluster_db_wal_bytes{db}` | Gauges |
| `uptimecluster_disk_free_ratio` | Gauge |
| `uptimecluster_outbox_pending` | Gauge |
| `uptimecluster_alert_deliveries_total{kind,outcome}` | Counter |
| `uptimecluster_degraded` | Gauge |
| `uptimecluster_system_alerts_active{kind}` | Gauge |
| `uptimecluster_clock_offset_seconds{peer}` | Gauge |
| `uptimecluster_build_info{version}` | Gauge |

### 8. UI

- Cluster-Ansicht: Liste der aktiven Systemalerts.
- Admin-Bereich: Verwaltung der Systemkanäle (Formular wie bei Mandantenkanälen).

## Konsequenzen

- **Positiv:**
  - Plattform-Admins erfahren aktiv von jeder Störung aus FA-OBS-01, je Störung einmal und mit Entwarnung.
  - Verschlüsselung, Maskierung, Drosselung und *at least once* gelten ohne neuen Versandweg.
  - Abweichende Replikas fallen im Betrieb auf und nicht erst im Test.
  - Prometheus deckt Langzeitbeobachtung und eigene Schwellen ab.
- **Negativ / Risiken:**
  - Neue Abhängigkeit (C1) mit drei transitiven Modulen. Die Lizenzprüfung (`make licenses`) wird um sie ergänzt.
  - `Checkpoint` kostet stündlich eine Fingerprint-Berechnung auf jedem Node und blockiert die Anwendung des Logs währenddessen kurz.
  - Ein ausgefallener Leader verzögert alle Systemalerts bis zur nächsten Wahl. Das ist gewollt, weil Raft-Wahlen in Sekunden laufen.
  - „Quorum lost“ kommt bei mehreren Partitionen mehrfach, je Partition einmal, aber gekennzeichnet (PF-28).
  - Der reservierte Mandant `_system` muss an jeder Mandanten-Schnittstelle ausgeschlossen werden. Das sichern Tests ab.
- **Folgearbeiten:**
  - Probe-Offline-Alerts mit P5.
  - Schwellen je Plattform in der UI (P4).
  - Runbook „Fingerprint weicht ab“.
  - Abnahme AS-05 (Systemalert „Node unhealthy“) im Chaos-Harness (AP-3.7).
  - Tests: Tabellentest des Collectors je Art inkl. Hysterese und Leaderwechsel; FSM-Tabellentest der drei Befehle bei Level 5 und 6; Ausschluss von `_system` aus der Mandanten-API; Metrik-Endpunkt mit und ohne Token; Clustertest für Voter offline → Alert → Entwarnung und für einen erzwungenen Fingerprint-Unterschied.

## Approval

| Datum | Status | Durch | Anmerkung |
|-------|--------|-------|-----------|
| 2026-11-17 | Proposed | Claude | |
