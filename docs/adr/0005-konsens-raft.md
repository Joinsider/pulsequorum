# ADR 0005: Konsens mit hashicorp/raft und FSM-Design

| | |
|---|---|
| **Status** | Proposed |
| **Datum** | 2026-10-19 |
| **Entscheider** | Projektinhaber |
| **Bezug** | FA-CLU-01..12, FA-NODE-03, PF-01..09, O-01, Konzept 6.1, E10, E12 |

## Kontext

Konfiguration und Zustand (Datenklasse A) müssen auf allen Votern stark konsistent sein; ein Single-Node muss ohne Migration zum Cluster wachsen. Recherchierte Fallstricke: einmaliges Bootstrap, unsichere parallele Mitgliedschaftsänderungen, Adresswechsel in Containern, unklare Ergebnisse bei Leadership-Verlust, Snapshot-Schleifen bei zu kleinem `TrailingLogs`, Determinismus der FSM.

## Optionen

### Option A: `hashicorp/raft` (MPL-2.0, v1.8.x) + `raft-boltdb/v2` als Log-/Stable-Store
- Vorteile: Produktionsbewährt (Consul, Nomad, Vault), Leadership Transfer, Nicht-Voter, `ReloadConfig`; boltdb-Store ausgereift.
- Nachteile: Netzwerk-Transport eigener Bauart (siehe ADR 0006); boltdb-Datei wächst ohne Compaction.

### Option B: `hashicorp/raft` + `raft-wal`
- Vorteile: Bessere Schreibleistung, kein Wachstum wie bei bolt.
- Nachteile: Laut README weiterhin experimentell; Migrationspfade in Vault/Consul aufwendig.

### Option C: `etcd-io/raft`
- Vorteile: Sehr robust, vom etcd-Team gepflegt.
- Nachteile: Nur der Algorithmus – Transport, Storage, Snapshots, Membership-Workflow selbst bauen; deutlich mehr Aufwand.

### Option D: rqlite/dqlite einbetten
- Nachteile: rqlite ist als eigener Prozess gedacht; dqlite benötigt CGO. Beides widerspricht E10/ein statisches Binary.

## Entscheidung

**Option A**, Wechsel auf `raft-wal` später über ADR möglich (Store ist austauschbar, aber nie gemischt im Cluster).

FSM-Design:
1. **Befehle** sind versionierte Protobuf-Nachrichten (`Command{ version, request_id, issued_at_ms, actor, tenant_id, oneof payload }`). Zeitstempel und neue IDs (UUIDv7) setzt der Leader **vor** `Apply`; die FSM liest nie die Uhr oder Zufall (FA-CLU-07).
2. **Materialisierung** in die lokale SQLite-Datenbank (Klasse-A-Tabellen) innerhalb einer Transaktion je Log-Eintrag; der angewandte Raft-Index wird in derselben Transaktion gespeichert. Beim Start werden nur Einträge nach diesem Index erneut angewandt (schneller Neustart statt Replay).
3. **Request-Dedup**: Tabelle `request_dedup(request_id, applied_index, result)` mit Aufbewahrung 24 h (anhand `issued_at_ms` der Befehle, deterministisch) (FA-CLU-06).
4. **Snapshots**: `VACUUM INTO` einer Kopie der Klasse-A-Tabellen in eine temporäre Datei, Restore ersetzt diese Tabellen atomar. Klasse-B-Daten (Heartbeats) sind nie Teil des Snapshots.
5. **Parameter**: `SnapshotThreshold` 8192, `TrailingLogs` 20 000, `SnapshotInterval` 2 min; per Konfiguration änderbar.
6. **Bootstrap-Erkennung**: `raft.HasExistingState` entscheidet; nur ein Node ohne Zustand **und** ohne `CLUSTER_JOIN` bootstrappt (FA-CLU-02).
7. **Mitgliedschaft**: Beitritt als Nicht-Voter (`AddNonvoter`), Promotion zu Voter nach Aufholen (Index-Abstand < 100). Ein Mutex auf dem Leader serialisiert Änderungen und wartet auf den Commit (FA-CLU-04). Keine automatische Entfernung (FA-CLU-08).
8. **Feature-Gates**: Jeder Node meldet seine unterstützte Befehlsversion; neue Befehlsversionen werden erst verwendet, wenn alle Voter sie unterstützen (FA-CLU-12).
9. **Node-Identität**: Raft-`ServerID` = persistente zufällige Node-ID; `ServerAddress` = stabile Advertise-Adresse (DNS).

## Konsequenzen

- Positiv: Bewährte Bibliothek, kleine Snapshots, schneller Neustart, Determinismus testbar.
- Negativ: MPL-2.0-Abhängigkeit (Datei-Copyleft, kompatibel mit AGPL und kommerzieller Nutzung, da unverändert genutzt); eigene Arbeit für Transport (ADR 0006).
- Risiken: Prüfen, dass v1.8.x die Korrektur für Mitgliedschaftsänderungen über Term-Grenzen enthält (Raft-Spike AP-0.14).
- Folgearbeiten: Raft-Spike validiert Bootstrap-Erkennung, Nicht-Voter-Promotion und Snapshot/Restore.

## Approval

| Datum | Status | Durch | Anmerkung |
|-------|--------|-------|-----------|
| 2026-10-19 | Proposed | Claude | |
