# Probe-Betrieb

Dieses Dokument beschreibt den Betrieb von Pulsequorum-Nodes mit der Rolle `NODE_ROLE=probe` (Pflichtenheft FA-PRB-01 bis FA-PRB-07, ADR 0034 und ADR 0035). Die Angaben entsprechen `internal/probe`, `internal/probegw`, `internal/membership`, `internal/sysalert`, `internal/config` und `cmd/pulsequorum/probe.go`. Ein einsatzfertiges Compose-Beispiel liegt in [deploy/probe/compose.yaml](../../deploy/probe/compose.yaml).

## Architektur & Quorum-Neutralität

- **Reine Mess-Nodes:** Eine Probe führt Monitore aus, hält aber keinen Raft-Zustand, kein `state.db`, keine Benutzer- oder Mandantendaten und keine Cluster-UI (`HTTP_ADDR` liefert ausschließlich `/healthz` und `/readyz`).
- **Ausgehendes mTLS:** Die Probe öffnet keinen Cluster-Port, sondern verbindet sich ausgehend per gRPC/mTLS zum Cluster-Port (`CLUSTER_ADDR`, Standard `:9443`) eines Voters. Sie funktioniert damit hinter NAT und Firewalls ohne eingehende Portweiterleitung.
- **Quorum-neutral (FA-PRB-07):** Probes erscheinen in der Admin-Cluster-Übersicht und im Node-Registry als Rolle `probe`, zählen jedoch nie als Raft-Voter oder -Nicht-Voter. Der Ausfall aller Probes beeinträchtigt weder Raft-Leader noch Quorum; umgekehrt retten Probes kein verlorenes Voter-Quorum.
- **Geheimnisse nur im Arbeitsspeicher:** Verschlüsselte Monitor-Zugangsdaten werden erst auf dem Voter beim Streaming entschlüsselt und auf der Probe ausschließlich im RAM gehalten — niemals in `voters.json`, Log-Ausgaben oder `metrics.db`.

## Erster Beitritt (Initial Join) & Persistierte Dateien

1. **Join-Token erzeugen:**
   - **Öffentliche Probe:** Ein Plattform-Administrator erstellt per REST `POST /api/v1/admin/join-tokens` ein einmaliges Token mit `{"role": "probe"}`.
   - **Private Probe (FA-PRB-05, ADR 0035):** Ein `tenant-admin` erstellt per REST `POST /api/v1/probes/join-token` (oder in der Web-UI unter *Private Probes*) ein einmaliges Token, das im Voter fest an den eigenen Mandanten (`owner_tenant_id`) gebunden ist (vorbehaltlich Planlimit `maxPrivateProbes`).
2. **Umgebung konfigurieren:**
   - `NODE_ROLE=probe`
   - `CLUSTER_SEEDS`: kommaseparierte Adressen eines oder mehrerer Voter (`host:port` oder `https://host:port`)
   - `JOIN_TOKEN_FILE` (bevorzugt) oder `CLUSTER_JOIN_TOKEN` / `JOIN_TOKEN` (nur `JOIN_TOKEN_FILE` **oder** Umgebungsvariable, nie beides gleichzeitig)
   - optional `NODE_NAME`, `NODE_REGION` (max. 64 Bytes), `NODE_LABELS` (`key=value,key2=value2`, max. 32 Paare)
3. **Dateien im `DATA_DIR` (`/data`):**
   - `node.id` – persistierte UUID des Nodes
   - `node.key` / `node.crt` – per mTLS ausgestelltes Probe-Zertifikat (`Role: probe`)
   - `ca-bundle.pem` – Vertrauensanker für die Cluster-CA (wird bei Zertifikatserneuerung/Snapshot vom Voter aktuell gehalten)
   - `voters.json` – lokal gelernte Liste aller bekannten Voter-Adressen; nach dem ersten Join kann die Probe auch ohne unveränderte `CLUSTER_SEEDS` jeden Voter ansteuern
   - `metrics.db` – lokaler SQLite-Puffer mit eigener `incarnation` und kleinem Cache (`ProbeMetricsCacheKiB = 2 MiB`, `ReadConns = 1`)

Sobald `node.crt` existiert, wird ein gesetztes Join-Token beim Start ignoriert. Vor dem erneuten Beitritt mit demselben Verzeichnis nach einer Entfernung muss `DATA_DIR` geleert werden.

## Lokaler Puffer & Ausfallüberbrückung

Bei Verbindungsabbruch zum Cluster puffert die Probe alle Prüfergebnisse fortlaufend in `metrics.db` unter der eigenen Stream-ID `(node_id, incarnation)` mit monoton steigender `seq`:

| Variable | Standard | Bedeutung |
|----------|----------|-----------|
| `PROBE_BUFFER_MAX_AGE` | `24h` | Maximale Aufbewahrungsdauer unbestätigter Messwerte vor Bereinigung |
| `PROBE_BUFFER_MAX_BYTES` | `100MiB` | Maximale Dateigröße von `metrics.db` (`B`, `KiB`, `MiB`, `GiB`, `KB`, `MB`, `GB`) |
| `PROBE_ACK_RETAIN` | `10m` | Aufbewahrungsdauer bereits bestätigter Messwerte nach `Ack` durch den Voter |

- **Verworfenes Melden (`lost_below`):** Müssen wegen Alter oder Größenlimit unbestätigte Einträge gelöscht werden, protokolliert `metrics.db` die verworfenen Zeilen (`dropped_total`) und rückt `lost_below` vor. Bei Wiederverbindung überspringt der Voter die verlorene Lücke sauber und die Probe zählt die Gesamtzahl in der Cluster-Übersicht (`probeBuffer.droppedTotal`).
- **Zulieferung ohne Duplikate:** Nach Reconnect startet der Upload beim Maximum aus `voterPos` (`StreamPosition`) und `minSeq - 1`, sodass bei normaler Pufferung jeder Messwert exakt einmal auf alle Voter repliziert wird.

## Keepalive, Tick-Überwachung & Voter-Failover

- **gRPC-Keepalive:** Client-Ping alle `30 s` (`PermitWithoutStream: true`, Timeout `10 s`); der Server akzeptiert Pings ab `15 s` auch ohne aktive RPCs. Das verhindert stilles Verwerfen langlebiger TCP-Verbindungen durch NAT-Gateways.
- **Anwendungs-Tick & Timeout:** Der Voter sendet mindestens alle `5 s` ein `SubscribeResponse`-Keepalive/Snapshot/Delta. Bleibt jede Server-Nachricht länger als `PROBE_SESSION_TIMEOUT` (Standard `15 s`) aus, bricht die Probe die Sitzung ab und schaltet auf den nächsten bekannten Voter um.
- **Backoff mit Jitter:** Nach einer etablierten Verbindung beginnt der Wechsel auf einen anderen Voter sofort ohne Pause. Erst wenn eine komplette Runde über alle Voter-Kandidaten fehlschlägt, greift ein exponentieller Full-Jitter-Backoff (`1 s` bis `30 s`).

## Private Probes & Mandantenbindung (FA-PRB-05, ADR 0035)

- **Mandantenbindung in der PKI:** Beim Beitritt über ein privates Join-Token prägt die Cluster-CA `owner_tenant_id` verbindlich in das Client-Zertifikat und in das `nodes`-Registry ein. Private Probes zählen zum Planlimit `maxPrivateProbes` (Prüfung sowohl beim Ausstellen des Tokens als auch atomar beim Join).
- **Strikte Isolation:** Das Voter-Gateway (`internal/probegw`), der Scheduler (`Assigner`) und die Probe-Runtime erzwingen durchgehend dieselbe Mandantengrenze (`owner_tenant_id == monitor.tenant_id`). Private Probes erhalten und verarbeiten niemals Monitore anderer Mandanten; öffentliche Probes erhalten keine fremden privaten Monitore.
- **Monitor-Zuweisung & interne Ziele (SSRF):**
  - Öffentliche und eigene private Probes lassen sich unter `GET /api/v1/monitors/assignable-nodes` abrufen und im Monitor unter `assignment.mode = "nodes"` gezielt zuweisen.
  - Ist ein HTTP-, TCP-, ICMP- oder DNS-Monitor ausschließlich auf eigene private Probes (`assignment.mode = "nodes"`) zugewiesen, lockert der Server beim Speichern (`POST`/`PUT`) die SSRF-Zielprüfung (RFC-1918-/Loopback-Ziele erlaubt). Auf der Probe selbst steuert `SSRF_PROTECTION` (für private Probes per Default `false`, für öffentliche `true`) den tatsächlichen Dialer-Schutz.
- **Mandanten-Verwaltung:** `tenant-admin`-Benutzer verwalten ihre privaten Probes über `GET /api/v1/probes`, `POST /api/v1/probes/join-token` und `DELETE /api/v1/probes/{id}` sowie in der UI unter `/probes` (inkl. Pufferstatus).

## Probe entfernen & Zertifikat widerrufen

- **Öffentliche Probe:** Ein Plattform-Administrator entfernt sie über `DELETE /api/v1/admin/nodes/{id}` oder die Admin-Cluster-UI.
- **Private Probe:** Ein `tenant-admin` entfernt sie über `DELETE /api/v1/probes/{id}` oder `/probes` (oder ein Plattform-Administrator über `/admin/nodes/{id}`). Beim Löschen eines Mandanten (`DeleteTenant`) werden sämtliche zugehörigen privaten Probes automatisch entfernt und deren Zertifikate widerrufen.
- Probes lösen beim Entfernen **keine** Quorum-Warnungen aus.
- Der Cluster trägt die Node-ID in die Sperrliste (`revoked_nodes`) ein und löscht den Eintrag aus dem Registry.
- Der verbundene Voter trennt den aktiven `Subscribe`-Stream unmittelbar; nachfolgende Anfragen werden mit `PERMISSION_DENIED` bzw. per mTLS-Sperre abgewiesen.
- Sobald alle bekannten Voter die Ablehnung bestätigen, beendet sich der Probe-Prozess mit einem eindeutigen Fehler (`ErrCertRejected` → Exit-Code `1`), um keinen dauerhaften Reconnect-Sturm zu erzeugen. Für einen erneuten Beitritt wird das alte `DATA_DIR` gelöscht und ein neues Join-Token erzeugt.

## Probe-Offline-Alerts (`FA-PRB-06`)

- **Verzögerung (`PROBE_OFFLINE_ALERT`, Standard `5m`):** Meldet eine bereits registrierte Probe länger als `PROBE_OFFLINE_ALERT` keinen Heartbeat und hält keine aktive Stream-Verbindung (`last_seen_ms` / `probe_offline`), löst der Cluster einen Alert aus (`0` deaktiviert die Erkennung; Probe-Health-Prüfung über `ProbeHealthWatcher` im Cluster).
- **Routing:**
  - **Öffentliche Probes (`owner_tenant_id` leer):** Ereignis geht als Systemalert in den Systemkanal `_system` (und in die Admin-System-Alert-Übersicht).
  - **Private Probes (`owner_tenant_id` gesetzt):** Ereignis `probe_offline` geht ausschließlich an die Benachrichtigungskanäle des jeweiligen Mandanten (ab Befehlsstufe `10`, bei neuen Kanälen standardmäßig aktiviert).
- Bei erneuter Verbindung/Lebenszeichen wird das aufgelöste Recovery-Ereignis (`probe_offline` mit Status `recovered`) gesendet.

## Gesundheit, Metriken & Ressourcenbedarf

- **`/healthz` & `/readyz`:** Lauschen auf `HTTP_ADDR` (Standard `:8080`). `/healthz` meldet den Prozesszustand (für Docker/Compose-`healthcheck`), `/readyz` liefert erst `200 OK`, wenn die Probe aktiv mit einem Voter verbunden ist und ihren ersten Konfigurations-Snapshot empfangen hat (sonst `503 Service Unavailable`).
- **Prometheus `/metrics`:** Optional über `METRICS_ADDR` aktivierbar und optional mit `METRICS_TOKEN` / `METRICS_TOKEN_FILE` geschützt. Voter exportieren zudem `pulsequorum_probe_rejected_total{reason}` (u. a. `non_probe_role`, `revoked`, `unregistered`, `owner_mismatch`, `foreign_monitor`, `upload_identity`) für abgelehnte Probe-Zugriffe.
- **Speicherbedarf:** Durch den Verzicht auf Raft/UI/State-Store sowie den begrenzten SQLite-Reader-Pool (`1` Reader, `2 MiB` Cache) bleibt der Speicherbedarf im Regelbetrieb deutlich unter `64 MiB` RSS (NFA-PERF-02); im Compose-Beispiel kann ein enges Limit (z. B. `mem_limit: 128m`) gesetzt werden.
