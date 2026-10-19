# Spike AP-0.14: Raft, FSM, Heartbeat-Replikation

> **Wegwerf-Prototyp.** Code aus diesem Verzeichnis wird **nie** in den Produktcode übernommen (CLAUDE.md, Regel 2).
> Eigenes `go.mod`, damit das Hauptmodul keine Abhängigkeiten erbt.

Ziel: die riskantesten Annahmen aus [ADR 0005](../../docs/adr/0005-konsens-raft.md) und [ADR 0006](../../docs/adr/0006-inter-node-kommunikation.md) mit drei Votern in Containern prüfen, bevor die ADRs entschieden werden. Die ADRs selbst wurden **nicht** geändert; die Empfehlungen stehen unten in [Empfehlungen](#empfehlungen-für-die-adrs).

## Kurzfazit

- Alle sieben Prüfpunkte sind bestätigt. Beide Messläufe (Raft-Timeouts 1000 ms und 500 ms) liefen ohne Fehler durch.
- Neuer Leader nach `kill -9` des Leaders:
  - **1,8–2,5 s** mit den Standard-Timeouts (1000 ms),
  - **1,1–1,7 s** mit 500 ms,
  - **0,2–0,4 s** beim geordneten Stopp mit Leadership Transfer.
  - NFA-REL-02 (< 10 s) und FA-NODE-04 (< 5 s) werden deutlich eingehalten.
- `hashicorp/raft` **v1.8.0** (aktuellste Version) enthält die Korrektur für den Membership-Bug aus dem raft-dev-Thread von 2015 (Details in [Prüfpunkt 7](#prüfpunkt-7-membership-bug-über-term-grenzen)).
- ALPN-Multiplexing (`uc-raft/1` und `h2` auf einem TLS-Port mit mTLS) funktioniert ohne Auffälligkeiten.
- Wichtigste Korrekturen an den ADRs (alle Punkte unter [Empfehlungen](#empfehlungen-für-die-adrs)):
  1. **Klasse-A-Daten in eine eigene SQLite-Datei.** `VACUUM INTO` kopiert immer die ganze Datenbank, nicht einzelne Tabellen.
  2. **Promotions-Kriterium verschärfen.** „Index-Abstand < 100“ befördert einen leeren Knoten sofort zum Voter.
  3. **Gelöschter Knoten unter derselben Adresse:** Raft lehnt doppelte Adressen ab. Die alte ID muss zuerst entfernt werden; das braucht ein dokumentiertes Verfahren.
  4. **Heartbeat-Hochwassermarke muss „lückenlos bis“ bedeuten**, nicht `max(seq)`.
  5. **Raft-Replikations-Backoff:** Nach einer Netztrennung holt ein Follower das Log erst bis zu ca. 10 s später auf, obwohl er den Leader schon wieder sieht. Das muss im Health-Status sichtbar sein.

## Aufbau

| Datei | Inhalt |
|-------|--------|
| `main.go` | Konfiguration per Env, Node-ID, Bootstrap/Join-Entscheidung, Raft-Setup, Observer, Shutdown mit Leadership Transfer |
| `tlsmux.go` | Spike-CA (`spike pki`), TLS-1.3-Listener mit mTLS, ALPN-Dispatch, `raft.StreamLayer`, HTTP/2-Client |
| `fsm.go` | Deterministische FSM auf SQLite (`state.db`): Request-Dedup, angewandter Index in derselben Transaktion, Snapshot per `VACUUM INTO`, Restore per Datei-Tausch |
| `heartbeat.go` | Heartbeats in `hb.db`; Push je Peer mit begrenztem Puffer; Anti-Entropie (Watermarks + FetchRange) |
| `api.go` | Write-Forwarding, Join-Workflow (Nonvoter → Aufholen → Voter, per Mutex serialisiert), Admin-API für das Skript |
| `bench.go` | `spike vacuumbench`: Kosten von `VACUUM INTO`, wenn Heartbeats in derselben Datei liegen |
| `compose.yml`, `Dockerfile` | 3 Voter; Image `FROM scratch` mit statischem Binary |
| `scenarios.sh` | Baut alles, startet den Cluster und führt die Szenarien S0–S6 aus |
| `results/run-*.txt` | Ausgaben der beiden Messläufe. Container-Logs liegen in `results/logs/` und sind nicht eingecheckt. |

### Topologie

- Drei Container `spike-n1..3` in zwei Netzen:
  - `raftspike-cluster`: nur Inter-Node-Verkehr, Alias `raft-nX`, Port 9443. Die Advertise-Adresse ist stabil per DNS.
  - `raftspike-mgmt`: Admin-HTTP, veröffentlicht als `localhost:1800X`.
- Netztrennung per `podman network disconnect raftspike-cluster spike-nX`. Der Knoten bleibt dabei über das Admin-Netz beobachtbar.
- Inter-Node-Verkehr läuft über **einen** TLS-Port mit ALPN:
  - `uc-raft/1` → `raft.NetworkTransport` mit eigenem `StreamLayer`,
  - `h2` → `net/http` mit HTTP/2, JSON statt gRPC (für den Spike ausreichend). Darüber laufen Forwarding, Join, Push, Watermarks und Fetch.
- Datenverzeichnis je Knoten:
  - `node_id`,
  - `raft.db` (raft-boltdb/v2: Log- und Stable-Store),
  - `snapshots/`,
  - `state.db` (Klasse A),
  - `hb.db` (Klasse B, mit Inkarnation in `hb_meta`).

### Ausführen

```bash
cd spikes/raft
./scenarios.sh                    # Standard: RAFT_TIMEOUT_MS=1000
RAFT_TIMEOUT_MS=500 ./scenarios.sh
KEEP=1 ./scenarios.sh             # Cluster danach laufen lassen
```

Voraussetzungen: Go, `podman` + `podman-compose` (oder `CTR=docker COMPOSE="docker compose"`), `jq`, `curl`, `perl`.

Bewusst gesetzte Spike-Parameter im Skript:
- `RAFT_TRAILING_LOGS=5`: Nach einem Snapshot werden Logs sofort gekürzt. Ein wieder beitretender Knoten muss daher per **InstallSnapshot** aufholen.
- `JOIN_HOLD_MS=1500`: Der Leader hält den Membership-Mutex beim Aufholen mindestens 1,5 s. Nur so überlappen zwei gleichzeitige Joins nachweisbar.
- Heartbeat alle 100 ms, Anti-Entropie alle 2 s.

### Umgebung der Messung

| | |
|---|---|
| Host | macOS, arm64 |
| Container | Podman 6.1.4 (applehv-VM), podman-compose 1.6.0 |
| Go | 1.27.2 |
| Bibliotheken | `hashicorp/raft` v1.8.0, `raft-boltdb/v2` v2.4.2, `modernc.org/sqlite` v1.60.1, `google/uuid` v1.6.0 |

Alle drei Container laufen auf demselben Host. Es gibt keine künstliche Latenz. Die Zahlen sind daher Untergrenzen für reale Netze.

## Ergebnisse je Szenario

Werte aus `results/run-20261019-075848-t1000.txt` (T=1000 ms) und `results/run-20261019-080129-t500.txt` (T=500 ms). „T“ ist `HeartbeatTimeout = ElectionTimeout`, `LeaderLeaseTimeout = T/2`.

### S0: Bootstrap, Bulk-Daten, gleichzeitiger Join (Prüfpunkte 1–3)

- n1 startet ohne `CLUSTER_JOIN` und ohne Zustand und bootstrappt allein → Leader.
- Danach wird ein Bulk-Befehl mit 10.000 Konfigurationszeilen (ca. 6 MB) angewandt: 60–75 ms als **ein** Log-Eintrag.
- Snapshot von n1 (`VACUUM INTO`, 6,07 MB): 21–39 ms.
- n2 und n3 starten gleichzeitig mit `CLUSTER_JOIN=raft-n1:9443`:
  - n2: Nonvoter nach 1–3 ms, Voter nach Aufholen.
  - n3: hat **1.342–1.357 ms auf den Membership-Mutex gewartet** und wurde erst danach als Nonvoter aufgenommen.
  - Die Joins liefen also strikt nacheinander (FA-CLU-04).
- Danach stimmen die FSM-Fingerprints (SHA-256 über alle Zeilen) auf allen drei Knoten überein.

### S1: Bootstrap-Erkennung aus persistiertem Zustand (Prüfpunkt 1)

| Aktion | Entscheidung | Ergebnis |
|--------|--------------|----------|
| n1 (Bootstrap-Knoten) neu erzeugt mit `CLUSTER_JOIN=raft-n2:9443` | `resume (existing raft state; CLUSTER_JOIN and bootstrap ignored)` | ein Cluster, 3 Voter |
| n2 neu erzeugt **ohne** `CLUSTER_JOIN` | `resume …` | kein zweiter Cluster, 3/3/3 Voter, ein Leader |

`raft.HasExistingState` (prüft Term, Log und Snapshots) reicht als Kriterium aus. PF-01 ist damit abgedeckt.

### S2: Write-Forwarding und Request-Dedup (Prüfpunkte 3 und 4)

- Ein `PUT` auf einen Follower wird an den Leader weitergeleitet (`forwarded:true, applied_by:<leader>`).
- Dieselbe `X-Request-ID` über einen anderen Follower ergibt `deduped:true` mit **derselben** `version_id` (UUIDv7 vom ursprünglichen Leader).
- Alle Knoten lesen danach denselben Wert und dieselbe Version.
- Latenz (curl, Mittel über 20 Schreibvorgänge): Leader 22–26 ms, über Follower 22–28 ms. Das Forwarding kostet lokal also nur ca. 1–2 ms.

### S3: `kill -9` des Leaders, 5 Runden (Prüfpunkt 6)

| Runde | T=1000: Wahl / erster Write | T=500: Wahl / erster Write |
|-------|------------------------------|-----------------------------|
| 1 | 2164 / 2198 ms | 1425 / 1461 ms |
| 2 | 2518 / 2551 ms | 1198 / 1233 ms |
| 3 | 2399 / 2433 ms | 1650 / 1685 ms |
| 4 | 2356 / 2391 ms | 1201 / 1238 ms |
| 5 | 1825 / 1917 ms | 1118 / 1178 ms |
| **Spanne** | **1,8–2,5 s** | **1,1–1,7 s** |

- „Wahl“: Zeit, bis beide Überlebende denselben neuen Leader melden.
- „Erster Write“: Zeit bis zum ersten erfolgreichen `PUT`.
- **Dedup nach Leaderwechsel:** In allen 10 Runden hat ein Retry mit derselben Request-ID beim neuen Leader `deduped:true` und dieselbe `version_id` geliefert. Es entstand also kein Duplikat (FA-CLU-06, PF-04).
- Der getötete Knoten war nach `podman start` in 0,25–0,6 s wieder Follower mit identischem FSM.
- **Geordneter Stopp (SIGTERM):** `LeadershipTransfer` brauchte 3–134 ms. Neuer Leader nach **195–385 ms**.

### S4: Netztrennung 2|1 mit dem Leader in der Minderheit (Prüfpunkt 6)

| Messgröße | T=1000 | T=500 |
|-----------|--------|-------|
| Mehrheit wählt neuen Leader | 2543 ms | 1047 ms |
| Alter Leader tritt zurück (laut Log nach `LeaderLeaseTimeout`) | ca. 0,5–0,6 s nach dem Schnitt | ca. 0,3 s |
| Write auf Minderheit | sofort 503 „no leader“ (19–21 ms) | dto. |
| Read auf Minderheit | 200, veraltete Daten (gewollt) | dto. |
| Term der Minderheit nach 15 s | **unverändert 12** (Pre-Vote) | dto. |
| Nach Heilung folgt der Knoten dem neuen Leader | 843 ms | 310 ms |
| FSM der Minderheit konvergiert | **5,8 s** nach Heilung | **9,1 s** nach Heilung |
| Heartbeat-Lücke geschlossen (laut Log, Anti-Entropie) | < 1 s nach Heilung | < 0,5 s (334 bzw. 157 Zeilen in 2–3 ms) |

- Die Mehrheit schreibt weiter. Die Minderheit liest nur noch.
- Dank **Pre-Vote** (seit v1.7.0 standardmäßig aktiv) erhöht der isolierte Knoten seinen Term nicht. Nach der Heilung lehnt der Leader dessen Pre-Vote ab („rejecting pre-vote request since we have a leader“). Es gibt keine störende Neuwahl.
- **Überraschung:** Die Heartbeats von Raft laufen nach der Heilung sofort wieder; der Knoten meldet also sofort den richtigen Leader. Die **Log-Replikation** zu ihm setzt aber erst 5–9 s später wieder ein.
  - Ursache: `replicateTo` wartet nach Fehlern `backoff(10ms, failures, 12)`, also bis zu **10,24 s** (`replication.go`, `util.go`).
  - Nur das Heartbeat-Backoff ist auf `HeartbeatTimeout/2` gedeckelt.
- Die Angabe „hb gap closed 6,5/9,4 s“ in den Lauf-Ausgaben ist ein Mess-Artefakt: Das Skript prüft erst nach der FSM-Konvergenz. Laut Container-Log schließt die Anti-Entropie die Lücke wenige hundert Millisekunden nach der Heilung.
- **Push-Pfad:** Ein Push zum abgetrennten Peer hängt, bis der HTTP/2-Ping-Timeout greift. Das waren ca. 8 s (`SendPingTimeout` 5 s + `PingTimeout` 3 s). Danach werden Batches verworfen; die Anti-Entropie füllt sie nach.

### S4b: Netztrennung 2|1 mit einem Follower in der Minderheit

- Leader und Mehrheit schreiben ungestört weiter (20/20 Writes).
- Ein Write auf den isolierten Follower dauerte **ca. 7–8 s**, bis `http2: client connection lost` kam (→ 503 `result_unknown`).
  - Der Follower kannte den Leader noch und hat weitergeleitet.
  - Der Request hing dann bis zum HTTP/2-Ping-Timeout.
- Nach der Heilung hatte der Follower nach 2,8–3,0 s wieder ein identisches FSM.

### S5: Datenverzeichnis eines Knotens löschen (Prüfpunkte 5 und 6)

1. Volume von n2 gelöscht, danach 10 Writes und ein Snapshot auf dem Leader.
2. n2 startet neu mit **neuer Node-ID und neuer Inkarnation**.
3. Join-Versuch → **409**: „address already used by another server id“. Raft lehnt doppelte Adressen in `checkConfiguration` ab. Der Spike entfernt bewusst nicht automatisch (FA-CLU-08).
4. Der Cluster bleibt mit 2/3 Votern beschreibbar.
5. Der Operator entfernt die alte ID (`RemoveServer`). Danach Join als Nonvoter, Aufholen per **InstallSnapshot** (6,09 MB, Restore in **12–27 ms**), Beförderung zum Voter. Gesamtdauer 3,4–3,5 s, davon 1,5 s künstlicher `JOIN_HOLD_MS`.
6. Ergebnis:
   - FSM identisch.
   - Der Heartbeat-Strom der **alten** Inkarnation ist auf den anderen Knoten vollständig erhalten (716/716 bzw. 693/693).
   - Der neue n2 hat ihn per Anti-Entropie vollständig zurückerhalten.
   - Es gibt keinen Datenverlust (FA-REP-06, PF-12).

### S6: Alle Knoten neu starten (Prüfpunkt 6)

| Variante | T=1000: Leader / erster Write | T=500: Leader / erster Write |
|----------|--------------------------------|-------------------------------|
| `kill` aller drei, dann Start | 1835 / 1872 ms | 2252 / 2288 ms |
| `stop` (SIGTERM) aller drei, dann Start | 1485 / 1521 ms | 901 / 940 ms |

Die Zeiten enthalten den Container-Start.

- Alle Knoten: `has_existing_state=true` → `resume`.
- `NoSnapshotRestoreOnStart=true`, weil `state.db` mindestens so aktuell ist wie der neueste Snapshot.
- Raft spielt beim Start das Log ab dem eigenen letzten Snapshot erneut ab. Die FSM überspringt diese Einträge anhand des gespeicherten Index (`skipped_on_replay` = 89–90 auf n1, 1–2 auf den anderen).
- Danach stimmen die FSM-Fingerprints auf allen Knoten überein. Die Zeilenzahl ist genau um den einen Test-Write gewachsen.
- Die Heartbeat-Ströme sind vollständig, und `seq` läuft in derselben Inkarnation weiter.

### Zusatzmessung: `VACUUM INTO` mit Heartbeats in derselben Datei

`spike vacuumbench`, Host-SSD:

| Inhalt | Dauer `VACUUM INTO` | Snapshot-Größe |
|--------|--------------------|----------------|
| 10k Konfigurationszeilen | 11–14 ms | 5,3 MB |
| + 1 Mio. Heartbeats | 130 ms | 24,3 MB |
| + 5 Mio. Heartbeats | 663 ms | 106,4 MB |

Hochrechnung für 100 Monitore × 1/min × 3 Voter × 90 Tage ≈ 39 Mio. Zeilen: ca. 5 s und ca. 0,8 GB **pro Snapshot**. Jeder InstallSnapshot würde diese Menge übertragen. Mit einer getrennten `state.db` bleibt der Snapshot bei ca. 6 MB.

## Prüfpunkt 7: Membership-Bug über Term-Grenzen

Es geht um den raft-dev-Thread von 2015 (Ongaro): Single-Server-Änderungen sind unsicher, wenn ein neuer Leader eine Konfigurationsänderung einbringt, bevor er einen Eintrag aus seinem eigenen Term committed hat.

Befund für **v1.8.0**. Das ist die aktuelle Version; `go list -m -versions` zeigt keine neuere 1.8.x.

- `raft.go`, `configurationChangeChIfStable()`: Konfigurationsänderungen werden erst angenommen, wenn
  1. die letzte Konfiguration committed ist **und**
  2. `getCommitIndex() >= leaderState.commitment.startIndex`, d. h. der Leader hat sein No-Op aus dem aktuellen Term committed.
- Der Code verweist ausdrücklich auf den raft-dev-Thread (`groups.google.com/forum/#!msg/raft-dev/t4xj6dJTP6E/...`).
- Ergänzend in `runLeader`: Jeder neue Leader schreibt zuerst ein `LogNoop`. Es existiert höchstens **eine** uncommittete Konfiguration im Log.
- `nextConfiguration()` unterstützt optimistische Nebenläufigkeit über `prevIndex`.
- Im `CHANGELOG.md` gibt es keinen eigenen Eintrag dazu. Die Korrektur gehört sehr wahrscheinlich zum Umbau `library-v2-stage-one`, der als v1.0.0 veröffentlicht wurde (Server-IDs, versioniertes Protokoll). Das ist aus dem Changelog allein aber nicht eindeutig ablesbar; maßgeblich ist der Code-Befund oben.

**Ergebnis:** Die in ADR 0005 genannte Prüfung ist erledigt; die Korrektur ist enthalten. Die Bibliothek serialisiert einzelne Konfigurationsänderungen bereits selbst. Der Mutex auf dem Leader wird nur für **mehrschrittige** Abläufe gebraucht (Nonvoter → Aufholen → Voter; Remove → Add).

## Überraschungen und Erkenntnisse

1. **`VACUUM INTO` kennt keine Tabellenauswahl.** ADR 0005 sagt „`VACUUM INTO` einer Kopie der Klasse-A-Tabellen“. Das geht nur, wenn Klasse A in einer **eigenen Datei** liegt (Messung oben).
2. **„Fuzzy“-Snapshot ist sicher und blockiert `Apply` nicht.**
   - `Snapshot()` liefert sofort zurück. Das eigentliche `VACUUM INTO` läuft in `Persist` parallel zu weiteren `Apply`-Aufrufen.
   - Die Kopie kann dadurch neuer sein als der Snapshot-Index. Das schadet nicht: `raft_meta.applied_index` steckt in der Kopie, und `Apply` überspringt alles bis zu diesem Index.
   - Voraussetzung: **sämtlicher** FSM-Zustand inklusive Dedup-Tabelle und angewandtem Index liegt in derselben Datei und wird in **einer** Transaktion je Eintrag geschrieben.
3. **Schneller Neustart funktioniert:**
   - `NoSnapshotRestoreOnStart` darf nur gesetzt werden, wenn `state.db.applied_index >= latestSnapshot.Index`. Sonst (z. B. `state.db` verloren, `raft.db` noch da) muss Raft den Snapshot einspielen.
   - Raft spielt beim Start trotzdem das Log ab dem eigenen letzten Snapshot ab. Die FSM muss diese Einträge billig überspringen.
4. **Promotions-Kriterium „Abstand < 100 Einträge“ ist trügerisch.**
   - Bei kurzem Log (frischer Cluster) besteht ein leerer Joiner die Prüfung sofort.
   - Dasselbe gilt, wenn ein einzelner Eintrag große Daten trägt oder ein InstallSnapshot noch läuft.
   - Der Spike verlangt zusätzlich: Der Joiner hat **mindestens den Index seines eigenen `AddNonvoter`-Eintrags angewandt**.
   - `hashicorp/raft` stellt den Match-Index je Follower nicht öffentlich bereit. Der Leader muss den Joiner per RPC nach seinem `AppliedIndex` fragen.
5. **Gelöschter Knoten unter gleicher Adresse.** `checkConfiguration` verbietet doppelte Adressen. Ein neu aufgesetzter Knoten mit gleicher DNS-Adresse kann erst beitreten, wenn die alte ID entfernt ist. Bei 3 Votern fehlt in diesem Fenster jede Ausfallreserve (2 von 2).
6. **Replikations-Backoff nach Netztrennung bis 10,24 s** (siehe S4). Der Knoten wirkt „gesund“ (hat einen Leader), liefert aber bis dahin veraltete Lesedaten.
7. **Write-Forwarding kann bis zum Transport-Timeout hängen**, wenn der Follower vom Leader getrennt ist, ihn aber noch kennt (S4b: 7–8 s).
8. **Hochwassermarke = lückenlos, nicht `max(seq)`.** Der Push-Pfad kann `seq` 200 zustellen, während 150–199 verworfen wurden. Eine `max`-Marke würde die Lücke nie anfordern. Der Spike berechnet `contig` (alle `seq` in 1..contig vorhanden) per SQL.
9. **Inkarnation gehört in die Heartbeat-Datenbank selbst.** Liegt sie in einer separaten Datei, kann `hb.db` verloren gehen, während die Inkarnation bleibt. Dann beginnt `seq` wieder bei 1 unter einer bekannten Inkarnation (PF-12). Der Spike speichert sie in `hb_meta`.
10. **`net/http` wählt HTTP/2 nur für `*tls.Conn`.** Ein vom Mux an `http.Server.Serve` übergebenes Conn darf nicht gewrappt sein. Sonst fällt der Server auf HTTP/1.1 zurück bzw. das ALPN-`h2` passt nicht. Für gRPC gilt Entsprechendes; siehe Empfehlung 0006-1.
11. **Eigene Fehler beim Bau der FSM:** Ein durchgereichtes `sql.ErrNoRows` aus der Dedup-Prüfung ließ den ersten Bulk-Befehl auf jedem Knoten gleich scheitern. Das war deterministisch, aber falsch. Gefunden wurde es nur durch Fingerprint-Vergleich und Zeilenzählung. Das bestätigt FA-CLU-07 (Determinismus-Test). Außerdem darf ein `Apply`-Fehler nicht still als Ergebnis zurückgegeben werden; siehe Empfehlung 0005-7.

## Empfehlungen für die ADRs

Die ADRs wurden nicht geändert. Die folgenden Punkte sind Vorschläge für das Review durch den Projektinhaber.

### ADR 0005 (Konsens/FSM)

1. **Punkt 4 (Snapshots) präzisieren:** Klasse-A-Daten liegen in einer **eigenen SQLite-Datei** (`state.db`), Klasse B (Heartbeats, Aggregate) in einer zweiten Datei. Joins über beide Dateien laufen bei Bedarf per `ATTACH`.
   - Snapshot = `VACUUM INTO` von `state.db` in `Persist`. Ein „Fuzzy“-Snapshot ist erlaubt, siehe Erkenntnis 2.
   - Restore = Datei-Tausch unter Schreibsperre und anschließendes Neuöffnen.
   - Das betrifft auch ADR 0004 (Persistenz).
2. **Punkt 2 ergänzen:** `NoSnapshotRestoreOnStart = (state.applied_index >= latestSnapshot.Index)` beim Start berechnen. Die FSM überspringt Einträge `<= applied_index`. Das ist zu dokumentieren und zu testen.
3. **Punkt 7 (Promotion) ändern:** Befördert wird, wenn der Joiner **mindestens den Index seines `AddNonvoter`-Eintrags angewandt hat** und sein Abstand zu `LastIndex` < N ist.
   - Dafür braucht es ein RPC `ClusterService.GetStatus` (`applied_index`). Der Match-Index ist in `hashicorp/raft` nicht öffentlich.
   - Gesamt-Timeout (z. B. 10 min für große Snapshots) mit Abbruch: Der Knoten bleibt Nonvoter, und es gibt einen Systemalert.
4. **Punkt 7 (Serialisierung) präzisieren:**
   - Die Bibliothek serialisiert einzelne Änderungen bereits (`configurationChangeChIfStable`, höchstens eine uncommittete Konfiguration).
   - Der Leader-Mutex sichert die mehrschrittigen Abläufe.
   - Der Mutex ist leader-lokal. Nach einem Leaderwechsel muss der Join-Handler idempotent fortsetzen: Ist der Knoten schon Nonvoter, geht es direkt zur Promotion; der Spike macht das so.
   - Zusätzlich `prevIndex` aus `GetConfiguration()` nutzen.
5. **Neuer Punkt „Knoten ersetzen“** (betrifft FA-CLU-08/-10):
   - Ein Join mit neuer ID unter bereits vergebener Adresse wird mit **409 + alter ID** abgelehnt.
   - Die UI/CLI bietet „Knoten ersetzen“ an: `platform-admin` bestätigt, dann folgen Remove(alt) und Join(neu), serialisiert.
   - Im Runbook steht: Während des Ersetzens hat ein 3er-Cluster keine Ausfallreserve. Wer das vermeiden will, nutzt für den neuen Knoten vorübergehend eine **andere** Advertise-Adresse (4 Voter, Quorum 3) und entfernt den alten erst danach.
6. **Timeouts:**
   - `HeartbeatTimeout = ElectionTimeout = 1000 ms` (Bibliotheksstandard) als Default beibehalten. Gemessen: 1,8–2,5 s Failover ohne Netzlatenz, also viel Reserve zu NFA-REL-02 (< 10 s) bei bis zu 100 ms Latenz.
   - 500 ms bringt ca. 0,8 s, ist aber anfälliger für GC-Pausen und Jitter. Daher konfigurierbar, aber nicht Default.
   - **Pre-Vote** (Default seit v1.7.0) ausdrücklich aktiviert lassen und im ADR erwähnen.
7. **`Apply`-Fehler:** Ein SQL-Fehler in `Apply` muss den Knoten anhalten (Readiness false, Prozessende). Eine stille Fehlerrückgabe würde zu divergierenden FSMs führen. Determinismus-Test (FA-CLU-07) plus Fingerprint-Vergleich der Klasse-A-Tabellen zwischen Votern als Selbstüberwachung (P3).
8. **Fehlerabbildung (FA-CLU-06) festschreiben:**
   - `ErrNotLeader`/`ErrEnqueueTimeout` → nichts im Log, gefahrlos wiederholbar.
   - `ErrLeadershipLost`/`ErrRaftShutdown`/Forwarding-Transportfehler → 503 „Ergebnis unbekannt“.
   - Im Spike in 10/10 Runden bestätigt.
9. **Health/Readiness:** „hat Leader“ reicht nicht. Nach einer Netztrennung hinkt ein Follower wegen des Replikations-Backoffs (bis 10,24 s, nicht konfigurierbar) hinterher.
   - Health sollte `applied_index` gegen den Commit-Index des Leaders zeigen (per Status-RPC) bzw. `LastContact`.
   - Das passt zu FA-NODE-05 und NFA-PERF-05.

### ADR 0006 (Inter-Node, Replikation)

1. **Option A (ein Port, ALPN) ist bestätigt.** Raft über `NetworkTransport` mit eigenem `StreamLayer` und TLS 1.3/mTLS lief in allen Szenarien ohne Auffälligkeit. Für gRPC als `h2`-Zweig:
   - Das bereits gehandshakte `*tls.Conn` an `grpc.Server.Serve` übergeben, mit TransportCredentials, die den vorhandenen TLS-Zustand als `AuthInfo` liefern (kein zweiter Handshake),
   - oder alternativ den Mux in den `tls.Config.GetConfigForClient`/ALPN-Pfad legen.
   - Das ist Arbeit für AP-0.13 bzw. P2. Im Spike lief stattdessen `net/http` mit HTTP/2.
2. **Hochwassermarke definieren** als „höchstes `seq`, bis zu dem lückenlos alles vorhanden ist“, nicht `max(seq)`.
   - Mit Aufbewahrung (Retention) braucht die Definition eine Untergrenze: lückenlos ab dem Retention-Horizont.
   - Alternativ zeitbasierte Bucket-Prüfsummen. Das sollte vor P2 im ADR oder im Proto-Vertrag festgelegt werden.
3. **Push ist „best effort“, Anti-Entropie die Wahrheit.** Fehlgeschlagene Push-Batches werden verworfen statt erneut gesendet. Das ist einfacher und im Spike korrekt (Lücken jeweils in < 1 s nach Wiederverbindung geschlossen). Anti-Entropie zieht **alle** Ströme, die der Peer kennt, auch die von Dritten.
4. **Inkarnation in der Heartbeat-Datenbank speichern** (Erkenntnis 9), nicht nur „im Datenverzeichnis“.
5. **Keepalive/Timeouts konkretisieren.** Ein toter HTTP/2-Stream wurde erst nach ca. 8 s erkannt (Ping 5 s + 3 s). Mit den im ADR genannten gRPC-Keepalives (15 s bei Votern) wären es 15–20 s.
   - Für Heartbeats ist das unkritisch: Puffer 256 bei 100 ms entspricht ca. 25 s, danach greift die Anti-Entropie.
   - **Write-Forwarding** braucht dagegen ein eigenes, kurzes Deadline-Budget (≤ 3 s).
   - Außerdem sofort 503 senden, wenn `LastContact` zum Leader älter als ca. 2 × `HeartbeatTimeout` ist (Erkenntnis 7).
6. **Anti-Entropie zusätzlich bei Raft-Ereignissen auslösen** (Observer `PeerObservation`/`ResumedHeartbeatObservation`), nicht nur bei erfolgreichem Push nach Fehlern. So schließt sich die Lücke auch dann schnell, wenn der Push-Pfad noch an einem toten Stream hängt.

## Grenzen des Spikes

- Alle Container laufen auf einem Host ohne künstliche Latenz oder Paketverlust. NFA-REL-02 (≤ 100 ms Latenz) ist nicht direkt gemessen.
- Partitionen per `network disconnect`: Neue Verbindungen scheitern sofort an DNS („no such host“). Bestehende Verbindungen werden zu toten TCP-Strömen. Asymmetrische Partitionen und reiner Paketverlust (`tc netem`) sind nicht getestet.
- Nicht abgedeckt:
  - gRPC (nur HTTP/2 + JSON), Probes, CA-Rotation, Join-Token,
  - Clock-Skew, volle Platte,
  - 5-Voter-Cluster, Last mit vielen Monitoren,
  - Snapshot-Schleifen (PF-08) mit großen Snapshots unter Schreiblast.
- Join-Identität im Spike: Cert-CN = Knotenname (n1..n3). Im Produkt ist das Zertifikat an die Node-ID gebunden und wird per Join-Token ausgestellt.
- `JOIN_HOLD_MS` und `RAFT_TRAILING_LOGS=5` sind künstliche Spike-Einstellungen (siehe [Ausführen](#ausführen)).
