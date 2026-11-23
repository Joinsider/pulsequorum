# Probe-Betrieb

Dieses Dokument beschreibt den Betrieb von Pulsequorum-Nodes mit der Rolle `NODE_ROLE=probe` (Pflichtenheft FA-PRB-01 bis FA-PRB-07, ADR 0034 und ADR 0035). Die Angaben entsprechen `internal/probe`, `internal/probegw`, `internal/membership`, `internal/sysalert`, `internal/config` und `cmd/pulsequorum/probe.go`. Ein einsatzfertiges Compose-Beispiel liegt in [deploy/probe/compose.yaml](../../deploy/probe/compose.yaml).

## Probe-Image

Probes laufen im eigenen Image `ghcr.io/joinsider/pulsequorum-probe` (ADR 0043). Das Binary `/pulsequorum-probe` ist der Entrypoint; es kennt die Unterbefehle `healthcheck` und `version` und setzt `NODE_ROLE=probe` standardmäßig. Mit `NODE_ROLE=voter` verweigert es den Start.

Warum ein eigenes Image:

- **Kleiner:** Ohne SPA, REST-API, OIDC- und Shoutrrr-Code ist das Image deutlich schlanker.
- **Weniger Befunde und seltenere Zwangs-Updates:** Der Scanner findet in einem schlankeren Image weniger Schwachstellen. Dadurch müssen Probe-Betreiber seltener wegen Befunden außerhalb der Probe-Funktion aktualisieren.
- **Ehrlich begrenzt:** Die Code-Pfade von Raft und bbolt sind im Probe-Binary weiterhin eingebunden, werden im Probe-Betrieb aber nie ausgeführt. Das ist **keine Sicherheitsgrenze**. Der wirksame Schutz ist die [lokale Richtlinie der Probe](#lokale-richtlinie-der-probe-adr-0044) in diesem Dokument.

Das Hauptimage `ghcr.io/joinsider/pulsequorum` mit `NODE_ROLE=probe` funktioniert weiterhin. Bestehende Installationen müssen nicht umgestellt werden. Das Compose-Beispiel [deploy/probe/compose.yaml](../../deploy/probe/compose.yaml) nutzt das Probe-Image.

**Tags:** Das Probe-Image folgt den Tags des Hauptimages: `X.Y.Z` (z. B. `0.0.5`) und `X.Y` (z. B. `0.0`) für Versionen ohne Suffix. Der Tag `latest` gibt es erst ab Version `1.0.0` (ADR 0040). Bis dahin ist `latest` nicht zu verwenden, weil er auf Breaking Changes in `0.x` zeigen würde. Für produktive Umgebungen empfiehlt sich ein fester Tag `X.Y.Z`.

Der Container-Healthcheck im Compose-Beispiel ruft `/pulsequorum-probe healthcheck` auf.

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

- **`/healthz` & `/readyz`:** Lauschen auf `HTTP_ADDR` (Standard `:8080`). `/healthz` meldet den Prozesszustand (Docker/Compose-`healthcheck` über `/pulsequorum-probe healthcheck`, das denselben Endpunkt abfragt), `/readyz` liefert erst `200 OK`, wenn die Probe aktiv mit einem Voter verbunden ist und ihren ersten Konfigurations-Snapshot empfangen hat (sonst `503 Service Unavailable`).
- **Prometheus `/metrics`:** Optional über `METRICS_ADDR` aktivierbar und optional mit `METRICS_TOKEN` / `METRICS_TOKEN_FILE` geschützt. Voter exportieren zudem `pulsequorum_probe_rejected_total{reason}` (u. a. `non_probe_role`, `revoked`, `unregistered`, `owner_mismatch`, `foreign_monitor`, `upload_identity`) für abgelehnte Probe-Zugriffe.
- **Probe-Richtlinie `/metrics`:** Probes exportieren `pulsequorum_probe_policy_blocked_total{reason}` (`target`, `method`, `type`) für Checks, die durch die lokale Probe-Richtlinie (`PROBE_ALLOW_TARGETS`, `PROBE_ALLOW_HTTP_METHODS`, `PROBE_ALLOW_MONITOR_TYPES`) blockiert wurden.
- **Speicherbedarf:** Durch den Verzicht auf Raft/UI/State-Store sowie den begrenzten SQLite-Reader-Pool (`1` Reader, `2 MiB` Cache) bleibt der Speicherbedarf im Regelbetrieb deutlich unter `64 MiB` RSS (NFA-PERF-02); im Compose-Beispiel kann ein enges Limit (z. B. `mem_limit: 128m`) gesetzt werden.

## Lokale Richtlinie der Probe (ADR 0044)

**Warum:** Die Probe führt aus, was der Cluster ihr schickt. Im Kundennetz ist der Cluster jedoch nicht vertrauenswürdig: Ein kompromittierter Voter oder ein gestohlenes Mandanten-Token kann Monitore anlegen, die die Probe im internen Netz ausführt. Deshalb legt der Kunde **lokal auf der Probe** fest, welche Ziele sie erreichen darf, welche HTTP-Methoden und Prüfarten sie ausführt und wie viele Ergebnisdetails sie meldet. Der Cluster kann diese Grenze weder lockern noch umgehen. Die Durchsetzung liegt vollständig in der Probe. Die Einträge verlassen die Probe nicht; der Cluster sieht nur, ob eine Richtlinie aktiv ist (Capability `target-policy`, Anzeige „Zielrichtlinie aktiv“ bzw. „Keine Zielrichtlinie“).

### Variablen

| Variable | Standard | Bedeutung |
|----------|----------|-----------|
| `PROBE_ALLOW_TARGETS` | ungesetzt; für private Probes **Pflicht** | Erlaubte Ziele als kommagetrennte IP- oder CIDR-Einträge, optional mit `:port` oder `:von-bis`. `any` hebt die Zielbeschränkung bewusst auf. |
| `PROBE_ALLOW_HTTP_METHODS` | private Probes: `GET,HEAD`; öffentliche Probes: alle | Erlaubte HTTP-Methoden (`GET`, `HEAD`, `POST`, `PUT`, `PATCH`, `DELETE`, `OPTIONS`) oder `any`. |
| `PROBE_ALLOW_MONITOR_TYPES` | alle (`http`, `tcp`, `icmp`, `dns`) | Erlaubte Monitor-Typen, kommagetrennt. |
| `PROBE_RESULT_DETAIL` | `full` | `full` meldet alle Ergebnisdetails, `minimal` reduziert sie (siehe unten). |

Ungültige Werte brechen den Start mit Exit-Code ≠ 0 und einer Meldung zum fehlerhaften Eintrag ab.

### Syntax von `PROBE_ALLOW_TARGETS`

- Einträge sind IPv4- oder IPv6-Adressen bzw. CIDR-Netze. Hostnamen sind nicht erlaubt, weil die Prüfung auf der tatsächlich verbundenen IP erfolgt.
- Optional `:port` (einzelner Port) oder `:von-bis` (Portbereich). IPv6 mit Port steht in eckigen Klammern.
- Ohne Portangabe erlaubt ein Eintrag alle Ports. ICMP prüft keinen Port: Jeder Eintrag, auch einer mit Port, erlaubt ICMP an die Adressen seines Netzes.
- Der Sonderwert `any` ist nur allein zulässig. `any` schaltet die Zielbeschränkung ab, außer für die unten genannten immer gesperrten Bereiche.

Beispiele:

```bash
# Interne Webdienste im Netz 10.20.0.0/16 nur auf HTTPS, ein Dienst mit Portbereich, IPv6 mit Port, DNS-Server
PROBE_ALLOW_TARGETS=10.20.0.0/16:443,10.30.1.5:8000-8099,[2001:db8::/48]:443,10.0.0.53:53

# Alle Ports im Netz 10.0.0.0/8 (einschließlich ICMP)
PROBE_ALLOW_TARGETS=10.0.0.0/8

# Bewusst ohne Zielbeschränkung (nur für Probes, die frei prüfen sollen)
PROBE_ALLOW_TARGETS=any

# Health-Endpunkte per POST prüfen und nur HTTP- und DNS-Monitore zulassen
PROBE_ALLOW_HTTP_METHODS=GET,HEAD,POST
PROBE_ALLOW_MONITOR_TYPES=http,dns

# Nur reduzierte Ergebnisdetails melden
PROBE_RESULT_DETAIL=minimal
```

**DNS-Server:** Die DNS-Server einer DNS-Prüfung müssen von `PROBE_ALLOW_TARGETS` erfasst sein. Enthält eine Antwort Adressen außerhalb der Richtlinie, schlägt die Prüfung mit `ssrf_blocked` fehl, ohne die Adressen zu melden.

### Immer gesperrte Bereiche

Die folgenden Bereiche öffnet nur ein Eintrag, der **vollständig** darin liegt (z. B. `169.254.169.254/32:80`). `any`, `0.0.0.0/0` und `::/0` öffnen sie nicht:

| Bereich | Bedeutung |
|---------|-----------|
| `0.0.0.0/8`, `::/128` | nicht spezifizierte Adressen |
| `127.0.0.0/8`, `::1/128` | Loopback |
| `169.254.0.0/16`, `fe80::/10` | Link-Local, inklusive Cloud-Metadaten (`169.254.169.254`) |
| `224.0.0.0/4`, `ff00::/8` | Multicast |
| `255.255.255.255/32` | Broadcast |

### Verhalten bei Verstoß

- Das Ergebnis ist `DOWN` mit der Fehlerklasse `ssrf_blocked` und der Meldung `blocked by local probe policy: target`, `method` bzw. `type`. Auf privaten Probes erscheinen Ziel-IP und Port nicht in der Meldung.
- Bei verbotener Methode oder verbotenem Typ startet die Prüfung gar nicht. Ein verbotenes Ziel wird beim Verbindungsaufbau abgewiesen, bevor eine Verbindung entsteht.
- Der Zähler `pulsequorum_probe_policy_blocked_total{reason="target|method|type"}` zählt die Verstöße.
- Beim Start protokolliert die Probe die wirksame Richtlinie lokal (`probe local policy`, Level `info`).
- Monitore, die nach einer Richtlinienänderung nicht mehr erlaubt sind, schlagen auf dieser Probe fehl. Die Meldung nennt dann die lokale Richtlinie als Ursache. Richtlinie anpassen und Probe neu starten.

### Start ohne Zielrichtlinie (private Probes)

Hat eine private Probe ein Mandanten-Zertifikat, aber `PROBE_ALLOW_TARGETS` ist nicht gesetzt, beendet sie sich nach dem Beitritt mit einem Fehler, der auf diese Dokumentation verweist. Das Zertifikat bleibt erhalten. Nach dem Setzen der Variable (oder `any`, bewusst) genügt ein Neustart; ein erneuter Beitritt ist nicht nötig.

### Minimaler Ergebnismodus (`PROBE_RESULT_DETAIL=minimal`)

Die Probe meldet nur:

- Status, Latenz, Fehlerklasse, Zeitangaben (Timings)
- HTTP-Statuscode bzw. DNS-Rcode
- Zeitpunkt des Zertifikatsablaufs
- ICMP-Paket- und RTT-Statistik

Nicht gemeldet werden gelesene Werte (JSON-Path), DNS-Antworten, Zertifikatsnamen, Adressen und freie Fehlertexte. Die Probe reduziert die Daten schon vor dem Puffer (`metrics.db`); was sie nicht speichert, kann sie nicht übertragen.

**Trade-off:** Die Fehlersuche im UI wird schwieriger, weil die Ursache eines Fehlers nicht mehr im Text steht. Eine Keyword-Prüfung bleibt ein Ja/Nein-Orakel, weil der Status selbst die Information trägt. `minimal` ist vor allem für Kunden mit hohem Schutzbedarf gedacht.

### Restrisiko

- **Namensauflösung:** HTTP- und TCP-Ziele mit Hostnamen werden über den System-Resolver der Probe aufgelöst. Diese Auflösung läuft nicht über die Zielrichtlinie. Ein Angreifer kann daher erfahren, ob ein interner Name auflösbar ist, aber nicht dessen Adresse, und er erreicht damit keine Verbindung zum Ziel. Wer das ausschließen will, sperrt DNS für die Probe auf den internen Resolver oder nutzt IP-Adressen.
- **Inhalte erlaubter Ziele:** Innerhalb der erlaubten Ziele kann der Modus `full` Inhalte über JSON-Path-Prüfungen, DNS-Antworten und Zertifikatsdaten sichtbar machen. Dagegen hilft `minimal`.
- **`any`:** Mit `any` ist die Zielbeschränkung aufgehoben. Die Probe kann dann alle Ziele außerhalb der immer gesperrten Bereiche erreichen. Diese Einstellung gehört nur auf Probes, die bewusst frei prüfen sollen.

### Container-Härtung im Compose-Beispiel

[deploy/probe/compose.yaml](../../deploy/probe/compose.yaml) ist gehärtet:

- `read_only: true`: Das Root-Dateisystem ist schreibgeschützt. Schreibbar bleiben `/data` (Volume `probe-data`, mit `node.id`, Zertifikaten, `voters.json` und `metrics.db`) und `/tmp` (`tmpfs`) für SQLite-Hilfsdateien.
- `cap_drop: [ALL]`: Keine Linux-Capabilities. ICMP läuft über unprivilegierte Ping-Sockets (`net.ipv4.ping_group_range`, siehe [ICMP-Monitore im Container](icmp.md)).
- `security_opt: [no-new-privileges:true]`: Prozesse können keine zusätzlichen Rechte erlangen.
- `pids_limit: 128` und `mem_limit: 128m` begrenzen Prozesse und Speicher.
- `PROBE_ALLOW_TARGETS` ist als Pflichtfeld mit Platzhalter gesetzt. `PROBE_ALLOW_HTTP_METHODS`, `PROBE_ALLOW_MONITOR_TYPES` und `PROBE_RESULT_DETAIL` stehen auskommentiert mit ihren Standardwerten.

Mit `read_only` sollten Änderungen am Compose-Beispiel geprüft werden: Alle Schreibzugriffe der Probe müssen unter `/data` oder `/tmp` liegen.

### Egress begrenzen

Die Zielrichtlinie wirkt in der Probe. Zusätzlich sollte der Netzwerkausgang des Probe-Containers auf die tatsächlich nötigen Ziele beschränkt werden. Wird die Probe selbst kompromittiert, greift ihre lokale Richtlinie nicht mehr. Die Firewall bleibt dann als zweite Schicht.

#### nftables (Docker-Host)

Das Beispiel erlaubt ausgehenden Verkehr des Probe-Containers nur zu den Voter-Ports `9443` der angegebenen Voter und zu den erlaubten Zielnetzen. Alles andere wird verworfen. Adressen und Netze sind anzupassen.

```nft
table inet pulsequorum_probe {
  set probe_addr { type ipv4_addr; elements = { 172.20.0.10 } }
  set voters { type ipv4_addr; elements = { 203.0.113.10, 203.0.113.11 } }
  set targets { type ipv4_addr; flags interval; elements = { 10.20.0.0/16, 10.0.0.53 } }

  # Priority -10: these rules run before Docker's own forward rules.
  chain egress {
    type filter hook forward priority -10; policy accept;

    ip saddr @probe_addr ip daddr @voters tcp dport 9443 accept
    ip saddr @probe_addr ip daddr @targets accept
    ip saddr @probe_addr drop
  }
}
```

Hinweise:

- Die Netz-Regeln sind gröber als `PROBE_ALLOW_TARGETS` (alle Ports im Zielnetz). Die Probe prüft Ports und Methoden selbst; die Firewall ist die zweite Schicht.
- Die Namensauflösung über den System-Resolver braucht Egress zum DNS-Server. Der Resolver muss deshalb in `targets` oder über eine eigene Regel erreichbar sein.
- Die Regel gilt für die IP des Containers im Docker-Netz (`172.20.0.10` im Beispiel). Mit einer festen Adresse im eigenen Netz lässt sich das Set stabil halten.

#### Kubernetes NetworkPolicy

Die Policy erlaubt Egress zu den Voters auf Port `9443`, zu einem erlaubten Zielnetz auf Port `443` und DNS. Ingress wird nicht eingeschränkt, weil die Probe keine eingehenden Verbindungen annimmt. Namespace, Labels und CIDRs sind anzupassen.

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: pulsequorum-probe-egress
  namespace: pulsequorum
spec:
  podSelector:
    matchLabels:
      app: pulsequorum-probe
  policyTypes:
    - Egress
  egress:
    # Voter-Gateway (gRPC/mTLS)
    - to:
        - ipBlock:
            cidr: 203.0.113.10/32
        - ipBlock:
            cidr: 203.0.113.11/32
      ports:
        - protocol: TCP
          port: 9443
    # Erlaubtes Zielnetz (Beispiel: HTTPS im Netz 10.20.0.0/16)
    - to:
        - ipBlock:
            cidr: 10.20.0.0/16
      ports:
        - protocol: TCP
          port: 443
    # DNS im Cluster (kube-dns / CoreDNS)
    - to:
        - namespaceSelector: {}
          podSelector:
            matchLabels:
              k8s-app: kube-dns
      ports:
        - protocol: UDP
          port: 53
        - protocol: TCP
          port: 53
```

Hinweise:

- NetworkPolicies unterscheiden nur TCP, UDP und SCTP. ICMP lässt sich damit nicht gezielt erlauben. Ein `to`-Eintrag ohne `ports` erlaubt für das CIDR alle Protokolle inklusive ICMP; die Einschränkung auf die Ziele übernimmt dann die Zielrichtlinie der Probe.
- Die Policy wirkt nur, wenn das CNI NetworkPolicies unterstützt.
