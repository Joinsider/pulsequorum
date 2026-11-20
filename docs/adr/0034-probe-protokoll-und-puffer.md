# ADR 0034: Probe-Protokoll, Keepalive, Failover und lokaler Puffer

|                 |                                                                                                                                                                                                                                                                                                                                                                                                                                    |
|-----------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **Status** | Accepted |
| **Datum**       | 2026-11-19                                                                                                                                                                                                                                                                                                                                                                                                                         |
| **Entscheider** | Projektinhaber                                                                                                                                                                                                                                                                                                                                                                                                                           |
| **Bezug**       | FA-PRB-01..04, FA-PRB-07, FA-REP-01..03/07, FA-PKI-01..03, FA-SCH-01/02, NFA-PERF-02, AS-08, PF-11, PF-12, PF-30; ADR 0006 (gRPC-Parameter, Replikation), ADR 0012 B2/B3/B5, ADR 0015 (Geheimnisse an Probes), ADR 0017 (Register, Zuweisung, Gesundheit), ADR 0018 (Befehlsversionen), ADR 0021 (Konsens-Slots), ADR 0025 (Signale über `GetStatus`), ADR 0039 (Bundle-Abgleich für Probes, Proposed); Mandantenbindung: ADR 0035 |

## Kontext

AP-5.1 baut die Probe-Rolle: ein Prozess mit `NODE_ROLE=probe`, ohne Raft, ohne API/UI, der sich ausgehend per gRPC/mTLS
mit einem Voter verbindet, seine Monitore abonniert, prüft und die Heartbeats hochlädt. Er steht oft hinter NAT
(Heimnetz, Kundennetz, CGNAT) und hat eine instabile Anbindung.

**Anforderungen**

- FA-PRB-01: Verbindung zu einem Voter aus `CLUSTER_SEEDS`, Failover mit Backoff und Jitter, Wechsel nach Voter-Ausfall
  in < 30 s.
- FA-PRB-02: Abo der zugewiesenen Monitore, Änderungen kommen in ≤ 5 s an.
- FA-PRB-03: Upload, bei Verbindungsverlust lokal puffern (Standard 24 h bzw. 100 MB) und nachliefern. Abnahme: 1 h
  offline, danach vollständig nachgeliefert.
- FA-PRB-04: Client-Ping 30–60 s, der Server erlaubt Pings ohne aktive Calls mit passender Mindestrate, die
  Verbindungsdauer ist begrenzt. Abnahme: 24 h hinter NAT.
- FA-PRB-07: Probes sind keine Raft-Mitglieder und beeinflussen das Quorum nicht.
- AS-08: Probe 24 h hinter NAT, 1 h Verbindungsverlust. Ergebnis: alle Heartbeats nachgeliefert, keine Duplikate.
- NFA-PERF-02: Eine Probe mit 100 Monitoren braucht < 64 MB RAM; Ressourcenrahmen 0,25 vCPU und 1 GB Platte.

**Was es schon gibt (Stand `master`, AP-4.6)**

| Baustein                  | Stand                                                                                                                                                                                                                                              |
|---------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `proto/.../probe.proto`   | `ProbeService` mit `Subscribe` (Server-Stream) und `UploadHeartbeats` (Bidi). Der Vertrag ist entworfen, aber nicht implementiert und nicht registriert.                                                                                           |
| `pki`                     | Rolle `probe`, URI-SAN `uc://<cluster>/probe/<id>[?tenant=<t>]`, Join-Token `uc1.…` mit CA-Fingerprint.                                                                                                                                            |
| `clusterpki.Authority`    | Probe-Token (auch mit Mandant) erzeugen und einlösen. Das Zertifikat trägt Rolle und Mandant aus dem gespeicherten Token.                                                                                                                          |
| `membership.Service.Join` | Für Probes: Token einlösen, Zertifikat ausstellen. Kein `AddVoter`, kein Registereintrag, keine Voter-Liste in der Antwort.                                                                                                                        |
| `clusternet`              | ALPN-Mux. Raft (`uc-raft/1`) nur für Voter-Zertifikate (Test vorhanden). gRPC-Zugriff je Methode über `Access` (`Anonymous`, `AnyNode`, `VoterOnly`). Server-Keepalive: `MinTime` 10 s, `PermitWithoutStream`, `MaxConnectionAge` 1 h, Grace 30 s. |
| `scheduler`               | `Scheduler.Sync([]store.Monitor)`, `Sink`, `HeartbeatWriter` (seq je Inkarnation, Batches), `Assigner` (Probes noch ausgeschlossen), `Options.Unit` für verkürzte Tests.                                                                           |
| `store` / `metrics.db`    | Heartbeat-Schlüssel `(node_id, incarnation, seq)`, `InsertHeartbeats` idempotent, lückenlose `stream_positions`, `HeartbeatsAfter`, Inkarnation in `metrics.db` (ADR 0012 B4).                                                                     |
| `replication`             | Push Best Effort, Anti-Entropie zieht alle Ströme, auch die dritter Nodes. Backoff mit Jitter (privat im Paket).                                                                                                                                   |
| `cmd/pulsequorum`       | `NODE_ROLE=probe` wartet nur auf das Ende („not implemented yet“).                                                                                                                                                                                 |
| Register                  | `nodes`-Tabelle ohne `owner_tenant_id`. `nodeFromInfo` verwirft den Owner. `SetNodeHealth` beruht auf Raft-Beobachtungen und erfasst Probes daher nicht.                                                                                           |

**Randbedingungen aus der Recherche**

- gRPC-Keepalive: Ein Client-Ping unter `MinTime` des Servers führt zu `GOAWAY too_many_pings`. Ohne
  `PermitWithoutStream` auf dem Server gilt das auch für Pings ohne aktive Streams. grpc-go hebt Client-`Time` < 10 s
  auf 10 s an. `MaxConnectionAge` bekommt in grpc-go ±10 % Jitter. Die gRPC-Doku rät von Client-Intervallen deutlich
  unter einer Minute ab. Unsere Server-Policy erlaubt das ausdrücklich (`MinTime` 10 s).
- NAT-Leerlauf-Timeouts: AWS NAT Gateway 350 s, Azure NAT Gateway 4 min (Standard, verwirft still ohne RST), GCP Cloud
  NAT 20 min (established). Heimrouter und CGNAT sind uneinheitlich. RFC 5382 fordert ≥ 2 h 4 min, in der Praxis kommen
  5 min und weniger vor. Ein Ping alle 30 s liegt unter allen bekannten Werten.
- Backoff: Die gRPC-Verbindungs-Backoff-Spezifikation nimmt 1 s Start, Faktor 1,6, ±20 % Jitter, max. 120 s und
  mindestens 20 s Connect-Timeout. ADR 0006 legt für uns Basis 1 s, max. 30 s und Jitter fest. „Full Jitter“ (AWS
  Architecture Blog) verteilt Reconnect-Stürme am besten.
- Heartbeats sind nach FA-REP-01 bereits idempotent. Dedup braucht keinen neuen Mechanismus, nur eine korrekte Quittung.

## Optionen

### Frage 1: Form des Protokolls

**1A Zwei Streams wie im Entwurf: `Subscribe` (Server-Stream) und `UploadHeartbeats` (Bidi)**

- Vorteile: Vertrag gibt es schon. Getrennte Flusssteuerung (große Zuweisungs-Snapshots blockieren keine
  Upload-Quittungen und umgekehrt). Beide laufen als HTTP/2-Streams über **eine** TCP-Verbindung, Keepalive gilt für
  beide. Jeder Stream ist für sich einfach zu testen.
- Nachteile: Zwei Streams, die beim Failover gemeinsam neu aufgebaut werden müssen. Der Voter muss sie über die
  Zertifikats-Identität einer Probe-Sitzung zuordnen.

**1B Ein Bidi-Stream `Session` mit `oneof` für Zuweisung, Upload, Quittung, Tick**

- Vorteile: Eine Sitzung, ein Lebenszyklus, eindeutige Sitzungszuordnung.
- Nachteile: Eigenes Multiplexing und Flusssteuerung im Anwendungsprotokoll, Head-of-Line-Blocking zwischen Snapshot und
  Upload. Der Vertrag müsste neu entworfen werden. Schwerer testbar.

**1C Unäre Aufrufe (`GetAssignment`-Polling, `Upload` je Batch)**

- Vorteile: Am einfachsten, zustandslos, robust bei instabilen Verbindungen.
- Nachteile: FA-PRB-02 (≤ 5 s) verlangt Polling alle ≤ 5 s für jede Probe. Mehr Last, keine schnelle Ausfallerkennung
  über den Stream.

### Frage 2: Erkennung eines toten Voters (FA-PRB-01 < 30 s) bei gleichzeitig FA-PRB-04 (Ping 30–60 s)

**2A Nur gRPC-Keepalive (`Time` 30 s, `Timeout` 10 s)**

- Vorteile: Keine Anwendungslogik.
- Nachteile: Bei stillem Paketverlust (Host weg, Partition, Azure-NAT verwirft ohne RST) erkennt der Client den Ausfall
  erst nach bis zu 40 s. Danach kommt der Verbindungsaufbau zum nächsten Seed. Damit wird < 30 s verfehlt. Ein kürzerer
  Ping widerspricht FA-PRB-04.

**2B Keepalive 30 s für NAT plus Anwendungs-Tick im `Subscribe`-Stream**

- Der Voter sendet alle 5 s eine leere `SubscribeResponse` als Tick. Bleibt er 15 s lang aus, gilt die Sitzung als tot,
  und die Probe wechselt.
- Vorteile: Erkennung ≤ 15 s, unabhängig vom Ping-Intervall. Daten-Frames fallen nicht unter die Ping-Policy des
  Servers. Der Tick transportiert auch den Bundle-Abgleich aus ADR 0039 und die Serverzeit.
- Nachteile: ~1 kleine Nachricht je 5 s und Probe (bei ≤ 50 Probes vernachlässigbar).

**2C Kürzerer Keepalive (`Time` 10 s, `Timeout` 5 s)**

- Vorteile: Keine Anwendungslogik.
- Nachteile: Verstößt gegen FA-PRB-04 (30–60 s). Mehr Ping-Last auf Mobil- und Funkanbindungen.

### Frage 3: Lokaler Puffer

**3A Eigene `metrics.db` der Probe (SQLite, vorhandener Store)**

- Die Probe öffnet `metrics.db` wie ein Voter und schreibt mit dem vorhandenen `HeartbeatWriter`. Der Puffer ist der
  eigene Strom. Hochladen heißt `HeartbeatsAfter(position)`, Aufräumen heißt Batch-Löschen.
- Vorteile: Keine neue Abhängigkeit, kein neues Format. Inkarnation, seq-Fortsetzung nach Neustart, Crash-Sicherheit
  (WAL), idempotentes Einfügen und Range-Lesen sind getestet. Dieselbe Semantik wie die Replikation zwischen Votern.
- Nachteile: Das Schema enthält Tabellen, die die Probe nicht nutzt (Rollups, Latenz-Minuten). SQLite-Speicher muss für
  NFA-PERF-02 klein gehalten werden (`cache_size`). Die Größe der Datei folgt Löschungen nur mit inkrementellem Vacuum
  (PF-23, `auto_vacuum=INCREMENTAL` ist schon gesetzt).

**3B bbolt (bereits direkte Abhängigkeit über `raft-boltdb`)**

- Key = seq (Big-Endian), Value = Protobuf-Heartbeat, ein Bucket je Inkarnation.
- Vorteile: Sehr schlank, sortierte Keys passen zum Range-Lesen.
- Nachteile: Neuer Code für Inkarnation, seq-Fortsetzung, Quittungsstand und Größenlimit. bbolt gibt freigegebene Seiten
  nicht an das Dateisystem zurück (Datei wächst bis zum Höchststand; Kompaktieren nur per Kopie). Zweites Speicherformat
  neben SQLite.

**3C Eigenes Append-Log in Segmenten (z.B. 4 MB, CRC je Datensatz)**

- Vorteile: Minimaler Speicher, Löschen ganzer Segmente ist billig, Größenlimit exakt.
- Nachteile: Am meisten eigener Code an einer heiklen Stelle (Torn Writes, fsync, Index für seq → Offset, Recovery).
  Kein Wiederverwenden bestehender Tests.

### Frage 4: Quittung und Nachlieferung beim Failover

**4A Quittung = lückenlose Position des Probe-Stroms auf dem empfangenden Voter; die Probe löscht sofort**

- Vorteile: Einfach.
- Nachteile: Stirbt der Voter nach der Quittung und vor der Replikation endgültig, sind die Heartbeats verloren.

**4B Quittung wie 4A, aber die Probe behält quittierte Heartbeats noch eine Schutzfrist und sendet nach jedem (Re-)
Connect ab der Position des neuen Voters**

- Vorteile: Deckt den Fall „quittiert, aber noch nicht repliziert“ ab, solange die Schutzfrist länger ist als Push plus
  Anti-Entropie (30 s). Duplikate sind dank Schlüssel folgenlos.
- Nachteile: Etwas mehr Plattenbedarf. Doppelte Übertragung nach Failover (nur ab der Position des neuen Voters).

**4C Quittung erst, wenn die Mehrheit der Voter den Heartbeat hat**

- Vorteile: Stärkste Zusicherung.
- Nachteile: Der Voter müsste Positionen anderer Voter abfragen. Die Quittung hängt dann an der Replikation, und ohne
  Quorum gäbe es keine Quittung. Das widerspricht Klasse B („ohne Quorum beschreibbar“).

### Frage 5: Gesundheit und Registereintrag von Probes (Voraussetzung für `count` und Probe-Alerts)

**5A Die Probe schreibt periodisch einen Raft-Befehl (Lebenszeichen)**

- Nachteile: Log-Schreiblast proportional zur Probe-Zahl. Ohne Quorum gäbe es keine Lebenszeichen. Probes dürfen `Apply`
  nicht aufrufen (FA-CLU-05).

**5B Jeder Voter meldet seine Probe-Sitzungen in `GetStatus`; der Leader entscheidet und schreibt nur Wechsel (
`SetNodeHealth`)**

- Vorteile: Gleiches Muster wie ADR 0017 und ADR 0025. Der Leader fragt `GetStatus` ohnehin alle 30 s ab. Nur
  Zustandswechsel landen im Log. `SetNodeHealth` gibt es schon.
- Nachteile: Erkennung in der Größenordnung des Abfrageintervalls. Bei 30 s Abfrage plus Hysterese sind das ≈ 60–90 s,
  für `count`-Übernahme und Alerts ausreichend.

**5C Gesundheit aus der Frische der Heartbeats der Probe**

- Nachteile: Eine Probe ohne Monitore wirkt offline. Ein gepufferter Rückstand täuscht Gesundheit vor. Nicht
  deterministisch über Voter.

## Entscheidung

**1A, 2B, 3A, 4B, 5B.** Im Einzelnen:

### 1. Rollen und Pakete

- **Probe-Seite** `internal/probe` (neues Paket): Seeds und bekannte Voter, Verbindungsaufbau und Failover, `Subscribe`
  -Client, Upload-Client, Puffer-Pflege. Wiederverwendet werden `scheduler.Scheduler` (`Sync` mit den abonnierten
  Monitoren, kein `Resolve`, weil die Konfiguration aufgelöst ankommt), `scheduler.HeartbeatWriter` als `Sink` in die
  lokale `metrics.db`, die Checker-Registry und `clusterpki.NodeCert` (Zertifikat, Erneuerung).
- **Voter-Seite** `internal/probegw` (neues Paket): `ProbeService`-Server, Sitzungsverwaltung, Zuweisung je Probe,
  Annahme der Heartbeats. Registriert auf dem Cluster-Port im `h2`-Zweig.
- **Zugriff:** neue Stufe `clusternet.ProbeOnly` für beide RPCs (Zertifikat mit Rolle `probe`, sonst
  `PermissionDenied`). `RenewCertificate` bleibt `AnyNode`. Alle übrigen RPCs bleiben `VoterOnly`.
- **`main`:** `runProbe` startet `metrics.db`, Selbsttest (ICMP usw. wie beim Voter, FA-NODE-06 ohne Raft-Teile),
  `NodeCert`, Join beim ersten Start, Checker-Registry, Scheduler, `internal/probe`. HTTP nur für `/healthz` und
  `/readyz` auf `HTTP_ADDR`, `/metrics` nur mit `METRICS_ADDR`. Keine API, keine SPA.

### 2. Beitritt und Identität

- Erster Start: Kein Zertifikat in `DATA_DIR/tls` → `Join` mit `CLUSTER_JOIN_TOKEN` an einen Seed (der Join wird zum
  Leader weitergeleitet, wie bei Votern). Die Probe prüft den CA-Fingerprint aus dem Token vor dem Senden (vorhandener
  `membership.Join`-Pfad, PF-10). Gespeichert werden Schlüssel, Kette und Bundle (ADR 0039: `ca-bundle.pem` ist der
  einzige Vertrauensanker der Probe).
- Folgestarts: Das Token wird ignoriert (Info-Log). Ein abgelehntes Zertifikat (widerrufen, fremde CA) beendet die Probe
  mit verständlicher Meldung und Exit-Code ≠ 0 („neu beitreten“). Es gibt keine Endlosschleife.
- `JoinResponse.voters` wird jetzt auch für Probes gefüllt.
- Der Leader legt beim Join den Registereintrag an (siehe 7), damit die Probe sofort in Admin-UI und Zuweisung
  erscheint.
- Erneuerung bei 2/3 der Laufzeit über `RenewCertificate` am verbundenen Voter (`AnyNode`). Der Voter leitet sie an den
  Leader weiter. Bundle-Abgleich nach ADR 0039 Abschnitt 6 über die Felder im Tick (siehe 4).

### 3. Verbindung, Keepalive, Failover

**Adressen.** `CLUSTER_SEEDS` nimmt kommagetrennt `host:port` oder `https://host:port` (wie im Konzept 16.3). Dazu kommt
die zuletzt gelernte Voter-Liste in `DATA_DIR/probe/voters.json` (aus `JoinResponse.voters` und
`SubscribeResponse.voters`). Kandidaten sind die Vereinigung, Seeds zuerst als Rückfall, falls die gelernte Liste
veraltet ist. Der Server wird ausschließlich über das Zertifikat geprüft (`pki.OnlyVoters`, gleiche Cluster-ID), nicht
über den Hostnamen.

**Auswahl.** Die Kandidaten werden beim Start einmal zufällig gemischt, damit sich Probes verteilen. Die Probe bleibt
beim verbundenen Voter, bis die Sitzung endet. Beim Wechsel versucht sie zuerst den nächsten Kandidaten **ohne
Wartezeit**. Erst wenn eine ganze Runde scheitert, folgt Backoff: Basis 1 s, Faktor 2, max. 30 s, Full Jitter
(`rand(0, min(30 s, 1 s · 2^n))`), wie ADR 0006. Connect-Timeout je Kandidat 5 s. Der vorhandene Backoff aus
`internal/replication` wird dafür in ein kleines gemeinsames Paket `internal/backoff` gezogen (ohne Verhaltensänderung
für die Replikation).

**gRPC-Parameter**

| Seite                     | Parameter                           | Wert                                  | Grund                                                                  |
|---------------------------|-------------------------------------|---------------------------------------|------------------------------------------------------------------------|
| Probe (Client)            | `keepalive.ClientParameters.Time`   | **30 s**                              | FA-PRB-04, unter allen bekannten NAT-Timeouts (Azure 4 min, AWS 350 s) |
|                           | `Timeout`                           | 10 s                                  |                                                                        |
|                           | `PermitWithoutStream`               | true                                  | Verbindung bleibt auch zwischen Streams offen                          |
| Voter (Server, bestehend) | `EnforcementPolicy.MinTime`         | 10 s                                  | ≤ Client-`Time`, sonst `too_many_pings`                                |
|                           | `PermitWithoutStream`               | true                                  |                                                                        |
|                           | `ServerParameters.Time` / `Timeout` | 15 s / 10 s                           | Server pingt auch selbst, räumt tote Probe-Verbindungen auf            |
|                           | `MaxConnectionAge` / `Grace`        | 1 h (±10 % Jitter von grpc-go) / 30 s | Zertifikatsrotation wirkt (PF-11), Probes verteilen sich neu           |

- `MaxConnectionAge` beendet beide Streams nach der Grace-Zeit. Die Probe behandelt `GOAWAY`/`Unavailable` nach
  Verbindungsalter als normalen Wechsel: Sie verbindet sofort neu, gern zum selben Voter, ohne Backoff.
- **Tick (2B):** Der Voter sendet alle **5 s** eine `SubscribeResponse` ohne Änderungen. Nach **15 s** ohne Nachricht
  verwirft die Probe die Verbindung (`ClientConn.Close`) und wechselt. Budget für FA-PRB-01: 15 s Erkennung + ≤ 5 s
  Connect + Snapshot < 30 s.
- Ein Voter ohne `ProbeService` (älteres Binary) antwortet `Unimplemented`. Die Probe nimmt den nächsten Kandidaten (ADR
  0018 Abschnitt 6).

### 4. Vertrag (`probe.proto`, Änderungen vor der ersten Implementierung)

Der Vertrag ist noch nirgends implementiert. Die Änderungen sind deshalb frei, werden aber nur additiv mit neuen
Feldnummern gemacht.

```proto
message SubscribeRequest {
  repeated string capabilities = 1;
  string version = 2;
  uint64 known_revision = 3;          // nur informativ, siehe unten
  uint32 protocol_version = 4;        // 1; der Voter lehnt Unbekanntes mit FAILED_PRECONDITION ab
  string incarnation = 5;             // Inkarnation der metrics.db der Probe
  string name = 6;                    // NODE_NAME
  string region = 7;
  map<string, string> labels = 8;
  int64 wall_clock_unix_ms = 9;       // Uhrversatz (ADR 0020)
  int64 cert_not_after_ms = 10;       // FA-PKI-06
  string cert_issuer_fingerprint = 11;        // ADR 0039
  repeated string trust_bundle_fingerprints = 12; // ADR 0039
  ProbeBufferStatus buffer = 13;      // Füllstand für Admin-UI/Alerts
}

message ProbeBufferStatus {
  int64 bytes = 1;
  uint64 unacked = 2;
  int64 oldest_unacked_ms = 3;
  uint64 dropped_total = 4;           // durch Limit verworfen, nie quittiert
}

message SubscribeResponse {
  uint64 revision = 1;                // applied_index des Voters bei Berechnung
  bool full_snapshot = 2;
  repeated ProbeMonitor upserts = 3;
  repeated string removed_monitor_ids = 4;
  StreamPosition stream_position = 5; // Position des Probe-Stroms auf diesem Voter
  repeated NodeInfo voters = 6;       // für voters.json (nur im ersten und bei Änderung)
  int64 server_time_ms = 7;
  string active_ca_fingerprint = 8;   // ADR 0039
  repeated string trust_bundle_fingerprints = 9; // ADR 0039
}

message UploadHeartbeatsRequest {
  repeated Heartbeat heartbeats = 1;
  // Alles mit seq < lost_below ist auf der Probe ohne Quittung verloren
  // (Pufferlimit). 0 = nichts verloren.
  uint64 lost_below = 2;
}
```

- **Snapshot und Deltas:** Jeder neue `Subscribe`-Stream beginnt mit einem vollständigen Snapshot
  (`full_snapshot=true`). `known_revision` wird nicht für Deltas über Voter-Grenzen genutzt, weil `applied_index` je
  Voter verschieden ist. Danach berechnet der Voter bei jedem FSM-Änderungssignal (wie `scheduler.Reconcile`, entprellt
  250 ms) die Zuweisung der Probe neu. Er vergleicht je Monitor einen Hash über die aufgelöste Konfiguration und sendet
  nur Upserts und Entfernungen. Das hält FA-PRB-02 (≤ 5 s) mit großem Abstand ein.
- **`ProbeMonitor.config_json`** enthält Geheimnisse im Klartext (ADR 0015 Punkt 5). Die Probe hält sie nur im Speicher,
  schreibt sie nie in `metrics.db` oder Logs. Ein Test sichert das ab.
- **Größen:** Snapshot in Teilen zu ≤ 500 Monitoren (mehrere Nachrichten, `full_snapshot` nur an der ersten).
  Upload-Batch ≤ 500 Heartbeats. Es gilt die bestehende `MaxRecvMsgSize` von 4 MB und die Feldgrenzen der Replikation
  (`message` ≤ 1 KiB, `details` ≤ 16 KiB).

### 5. Puffer (3A)

- Die Probe nutzt eine eigene `metrics.db` mit dem vorhandenen Schema und Store. Die Inkarnation entsteht wie beim Voter
  (FA-NODE-03). Fehlt die Datei, gibt es eine neue Inkarnation. Der Strom beginnt dann bei seq 1, die Voter sehen einen
  Inkarnationswechsel (FA-REP-06, Alert nach ADR 0035).
- SQLite für die Probe sparsam konfigurieren: kleiner Lese-Pool (1–2), `cache_size` begrenzt, damit NFA-PERF-02 (< 64 MB
  bei 100 Monitoren) hält. Wird in der Abnahme gemessen.
- **Quittungsstand** als Watermark `probe_acked_seq` (vorhandenes `SetWatermark`). Er wird je Quittung erhöht, nie
  gesenkt.
- **Aufräumen** (alle 60 s, Batches ≤ 5.000, FA-UPT-06):
    1. Quittierte Heartbeats, die seit **≥ 10 min** quittiert sind (`PROBE_ACK_RETAIN`, Schutzfrist für 4B, deutlich
       über Push + Anti-Entropie von 30 s).
    2. Unquittierte Heartbeats älter als `PROBE_BUFFER_MAX_AGE` (Standard **24 h**).
    3. Solange die Datei größer ist als `PROBE_BUFFER_MAX_BYTES` (Standard **100 MB**, gemessen als
       `page_count · page_size` nach `incremental_vacuum`): die ältesten Heartbeats nach seq.

    - Bei 2 und 3 setzt die Probe `lost_below` auf die höchste gelöschte unquittierte seq + 1, zählt `dropped_total`
      hoch und loggt eine Warnung. Rollups und Latenz-Minuten erzeugt die Probe nicht.
- Kein `fsync` je Heartbeat: Es gilt `synchronous=NORMAL` wie beim Voter (PF-36). Ein Stromausfall der Probe kann die
  letzten Sekunden kosten, was für Klasse B akzeptiert ist.

### 6. Upload, Quittung, Dedup (4B)

- Nach jedem (Re-)Connect liest die Probe `stream_position` aus der ersten `SubscribeResponse` und sendet ab
  `max(position, kleinstes vorhandenes seq − 1)`. Das Fenster ist ≤ 4 unquittierte Batches. Danach gehen neue Heartbeats
  sofort hinaus (Flush 1 s wie beim `HeartbeatWriter`).
- Der Voter prüft jeden Batch (Regeln zur Mandanten- und Identitätsprüfung: ADR 0035). Dann:
    1. `InsertHeartbeats` (idempotent, `ON CONFLICT DO NOTHING`)
    2. `OnStored` an den Aggregator (ADR 0021)
    3. `Replicator.Publish` an die anderen Voter (Best Effort, ADR 0012 B3)
    4. Antwort mit der lückenlosen `StreamPosition` des Probe-Stroms
- `lost_below`: Der Voter hebt die Position des **eigenen Stroms der aufrufenden Probe** (Node-ID aus dem Zertifikat)
  mit `AdvanceStreamPosition` auf `lost_below − 1`. Ohne das bliebe die Position an der Lücke hängen, und jede
  Wiederverbindung würde endlos nachsenden. Andere Ströme kann eine Probe so nicht beeinflussen. Gesetzt wird
  `lost_below` nur für **nie quittierte, wirklich verlorene** Heartbeats, nie für quittiert-gelöschte. Sonst könnte ein
  Voter eine Lücke überdecken, die ein anderer Voter noch füllen würde.
- **Dedup:** Der Schlüssel `(node_id, incarnation, seq)` macht jede Wiederholung folgenlos. AS-08 „keine Duplikate“ ist
  damit eine Eigenschaft des Schemas, keine des Protokolls. Der Test prüft sie trotzdem.
- **Verspätete Heartbeats:** Nachgelieferte Heartbeats landen in Rohdaten, Verlauf und Latenzdiagrammen. Konsens-Slots,
  die schon bewertet sind, ändern sie nicht (ADR 0021). Eine offline Probe zählt dort als „fehlende Meldung“. Rollups
  warten auf späte Heartbeats höchstens `maxReplicationHoldBack = 1 h` (`internal/uptime`). Danach gepufferte
  Probe-Heartbeats fehlen in den Stunden-Rollups der Probe-Serie (Konsens-Serie unberührt). Siehe offene Frage F3.

### 7. Register, Gesundheit, Zuweisung (5B)

- **Register:** Neuer Befehl `RegisterProbe{NodeInfo node, string owner_tenant_id}` mit Mindestversion **10** (ADR
  0018). `RegisterNode` bleibt auf der Basisversion und für Voter. Den Befehl schlägt nur ein Voter vor: der Leader beim
  Join und der Gateway-Voter beim `Subscribe`, wenn sich Name, Region, Labels, Version oder Fähigkeiten geändert haben.
  Rolle, ID und Owner stammen immer aus dem Zertifikat, nie aus der Anfrage. Migration (Expand):
  `nodes.owner_tenant_id TEXT NOT NULL DEFAULT ''`, `nodes.incarnation TEXT NOT NULL DEFAULT ''`. Unter Level 10 nimmt
  der Gateway Probes an, registriert sie aber nicht. Sie bekommen dann eine leere Zuweisung und eine Log-Warnung.
- **Sitzungen in `GetStatus`:** neues additives Feld `repeated ProbeSession probe_sessions` (`node_id`,
  `connected_since_ms`, `last_message_ms`). Der Leader führt die Meldungen aller Voter zusammen.
- **Gesundheit:** Ungesund, wenn **60 s** lang kein Voter eine lebende Sitzung meldet. Wieder gesund nach **30 s**
  ununterbrochener Sitzung. Der Leader schreibt Wechsel mit dem vorhandenen `SetNodeHealth`. 60 s liegen über dem
  Failover-Budget (30 s) und dem Abfrageintervall, damit ein Voter-Wechsel nicht flattert. Neuer Leader: Karenz wie ADR
  0017 bzw. eine volle `GetStatus`-Runde.
- **Zuweisung:** `Assigner.candidates` nimmt Probes auf, mit der Mandantenregel aus ADR 0035:
    - `all`: alle fähigen Voter + alle fähigen **öffentlichen** Probes (Konzept 7.1)
    - `count`: Rendezvous über gesunde, fähige Voter + öffentliche Probes
    - `nodes`: die genannten Nodes, wenn fähig und für den Mandanten zulässig
    - Der Golden-Test der Gewichte bleibt unverändert.
- **Entfernen:** vorhandenes `RevokeNode` (Register-Eintrag weg, Zertifikate gesperrt). Eine verbundene Probe wird beim
  nächsten Handshake abgewiesen, der Gateway schließt laufende Sitzungen bei Widerruf sofort.

### 8. FA-PRB-07: Quorum-Neutralität

Verbindlich und getestet:

- Der Join einer Probe ruft nie `AddVoter`/`AddNonvoter` auf. Probes stehen nie in der Raft-Konfiguration.
- ALPN `uc-raft/1` lehnt Probe-Zertifikate ab (bestehender Test).
- `Apply`, `GetStatus`, `RemoveNode` und die Replikation bleiben `VoterOnly`. Probes schlagen nie Befehle vor.
- Command-Level (ADR 0018), Degraded-Zählung „x/n Nodes“ (ADR 0024), Promotion und Leaderwahl zählen nur Voter der
  Raft-Konfiguration.
- Probes haben keinen Degraded-Modus (ADR 0024). Ohne Quorum prüfen und puffern sie weiter. Ein Voter nimmt Uploads auch
  ohne Quorum an (Klasse B), Zuweisungsänderungen gibt es dann nicht.

### 9. Teststrategie

| Ebene                     | Inhalt                                                                                                                                                                                                                                                                                                                                                                                                                    | Werkzeug                             |
|---------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------------|
| Unit (`internal/probe`)   | Kandidatenliste, Mischen, Backoff mit Full Jitter (deterministischer Zufall), Tick-Timeout, Puffer-Aufräumen (Reihenfolge 1–3, `lost_below`), Quittungsstand, Snapshot/Delta-Anwendung, kein Klartext-Geheimnis in `metrics.db`/Log                                                                                                                                                                                       | Tabellentests, Fake-Uhr              |
| Unit (`internal/probegw`) | Delta-Berechnung, Hash-Vergleich, Tick, Annahme/Ablehnung von Batches, `lost_below` nur für den eigenen Strom, Sitzungsersatz (neue Sitzung derselben Probe schließt die alte)                                                                                                                                                                                                                                            | bufconn                              |
| Integration in-process    | 3 Voter (`clusterhost`-Harness) + 2 Probes über echtes mTLS auf `127.0.0.1`: Snapshot ≤ 5 s nach Monitor-Änderung, Leader-Kill und Kill des verbundenen Voters → Wechsel < 30 s, keine Lücke, identische Prüfsummen je `(probe, incarnation)` auf allen Votern (FA-REP-02)                                                                                                                                                | `go test ./internal/clusterhost/...` |
| FA-PRB-07                 | 3 Voter + 5 Probes: 2 Voter stoppen → Quorum weg, trotz Probes. Alle Probes stoppen → Leader, Quorum und Command-Level unverändert. Raft-Konfiguration enthält nie eine Probe                                                                                                                                                                                                                                             | Integration                          |
| **AS-08 verkürzt** (CI)   | `scheduler.Options.Unit = 10 ms`, `PROBE_ACK_RETAIN`/Limits skaliert. „1 h offline“ = 360 Intervalle bei 1 s-Intervall-Monitoren. Trennung über einen schließbaren Listener bzw. Dialer-Fehlerinjektion. Prüft: alle seq 1..N auf allen Votern, `COUNT = MAX(seq)` je Strom, keine Lücke, keine Duplikate, `dropped_total = 0`. Zusatzfall Überlauf: Limit klein → `lost_below` hebt die Position, kein Endlos-Nachsenden | `go test -run AS08`                  |
| AS-08 voll (Chaos)        | Probe-Container in eigenem Netz hinter einem NAT-Container (iptables `MASQUERADE`, `nf_conntrack_tcp_timeout_established=120`). Keepalive 30 s muss die Verbindung 24 h halten (keine Reconnects außer `MaxConnectionAge`). 1 h `iptables DROP` am NAT → Nachlieferung, Vergleich der Heartbeat-Mengen über die Voter-APIs. CI-Kurzlauf 15 min mit 2 min Trennung, 24 h als manueller/nächtlicher Lauf                    | `test/chaos` (neues Szenario `as08`) |

## Konsequenzen

- **Positiv:**
    - Kaum neue Mechanik: Puffer, Dedup, Lücken-Semantik und Replikation folgen dem Voter-Modell. Keine neue
      Abhängigkeit.
    - NAT-tauglich nach bekannten Timeouts. Ausfallerkennung entkoppelt vom Ping-Intervall.
    - Failover ohne zentrale Koordination. Verteilung der Probes über Zufall und `MaxConnectionAge`.
    - Probes beeinflussen weder Quorum noch Command-Level.
- **Negativ / Risiken:**
    - SQLite-Speicher der Probe muss für NFA-PERF-02 nachgewiesen werden. Rückfall wäre 3B, mit eigenem Code.
    - Quittiert-aber-nicht-repliziert und danach endgültiger Voter-Verlust nach Ablauf der Schutzfrist (10 min) verliert
      Daten. Das ist sehr unwahrscheinlich, weil Push sofort läuft und die Anti-Entropie alle 30 s.
    - Bleibt der Strom auf einem Voter an einer Lücke hängen, die nur ein dauerhaft toter Voter füllen könnte, endet das
      erst mit der Retention. Das ist sichtbar als Replikationsrückstand.
    - Nach Pufferüberlauf sind Heartbeats verloren (gewollt begrenzt). Sichtbar über `dropped_total`.
    - Nachgelieferte Heartbeats ändern bewertete Konsens-Slots nicht. Bei > 1 h Verzögerung fehlen sie in
      Stunden-Rollups der Probe-Serie (F3).
    - Neuer Befehl und Migration: Level 10, Feature-Gate nach ADR 0018. Probes funktionieren erst nach dem Update aller
      Voter vollständig.
    - Klartext-Geheimnisse gehen an öffentliche Probes. Das ist der Vertrauensgrad „Plattform-Node“. Private Probes: ADR
      0035.
- **Folgearbeiten:**
    - Pflichtenheft: PF-30 fehlt in der Risikotabelle 12.x (nur aus FA-PRB-04 referenziert). Nachtragen:
      „NAT-/Firewall-Leerlauf-Timeouts trennen lange Streams still; gRPC-Keepalive vs. Server-Enforcement
      (`too_many_pings`)“.
    - Neue Variablen in 9.5: `PROBE_BUFFER_MAX_AGE`, `PROBE_BUFFER_MAX_BYTES`, `PROBE_ACK_RETAIN`.
    - FA-PRB-04 um den 5-s-Tick ergänzen.
    - Compose-Beispiel `deploy/probe` (NFA-PORT-02). Betriebsdoku „Probe hinter NAT“.
    - Admin-UI: Probes mit Version, Sitzung (Voter), Pufferstand, Gesundheit.

### Offene Fragen an Projektinhaber

- **F1 – Öffentliche Probes in `all`:** Nach Konzept 7.1 nehmen öffentliche Probes automatisch an allen `all`-Monitoren
  aller Mandanten teil. Das ändert Melder und Konsens bestehender Monitore, sobald eine Probe beitritt. Vorschlag: so
  lassen (Konzept). Alternative: Probes nur über `count`/`nodes` bzw. ab P6 über Labels.
- **F2 – Ausfallerkennung:** Reicht 5-s-Tick / 15-s-Timeout, oder soll der Tick konfigurierbar sein
  (`PROBE_SESSION_TIMEOUT`)?
- **F3 – Verspätete Probe-Heartbeats und Rollups:** Hinnehmen (Rohdaten vollständig, Stunden-Rollup der Probe-Serie
  unvollständig bei > 1 h Verzögerung) oder „schmutzige Stunden“ nachrechnen (eigene Änderung am Uptime-Job, eigener
  ADR)? Vorschlag: hinnehmen und dokumentieren. Die Abnahme AS-08 betrifft Heartbeats, nicht Rollups.
- **F4 – Schutzfrist `PROBE_ACK_RETAIN` = 10 min:** einverstanden?
- **F5 – Probe-HTTP:** Nur `/healthz`/`/readyz` auf `HTTP_ADDR` (Standard `:8080`) oder standardmäßig aus, damit eine
  Probe gar keinen Port öffnet?

## Approval

| Datum      | Status   | Durch  | Anmerkung                                |
|------------|----------|--------|------------------------------------------|
| 2026-11-19 | Proposed | Claude | Vorbereitung AP-5.1; offene Fragen F1–F5 |
| 2026-11-20 | Accepted | Projektinhaber | im Chat; 1A bestätigt, offene Fragen wie vorgeschlagen |
