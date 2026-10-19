# ADR 0006: Inter-Node-Kommunikation, mTLS und Heartbeat-Replikation

| | |
|---|---|
| **Status** | Accepted |
| **Datum** | 2026-10-19 |
| **Entscheider** | Projektinhaber |
| **Bezug** | FA-PKI-01..06, FA-REP-01..06, FA-CLU-05, FA-PRB-01..04, PF-10..12, PF-30, O-02, E9 |

## Kontext

Über den Cluster-Port laufen Raft, Write-Forwarding, Heartbeat-Replikation und das Probe-Gateway, alles mit mTLS über eine clustereigene CA. Probes stehen hinter NAT und halten lange Streams. Heartbeat-Schlüssel müssen Datenverlust/Neuinstallation eines Nodes überstehen.

## Optionen (Transport)

### Option A: Ein Port (9443), TLS-Listener mit ALPN-Multiplexing
- ALPN `uc-raft/1` → hashicorp/raft `NetworkTransport` über eigenen `StreamLayer`; ALPN `h2` → gRPC (Replikation, Forwarding, Join, Probes).
- Vorteile: Ein Port in Firewall/Compose, gemeinsame TLS-Konfiguration, Raft nutzt seinen erprobten Transport.
- Nachteile: Etwas Code für den Listener-Mux.

### Option B: Zwei Ports (Raft-TCP + gRPC)
- Vorteile: Am einfachsten.
- Nachteile: Mehr Ports zu öffnen, doppelte TLS-Konfiguration; Konzept nennt nur einen Cluster-Port.

### Option C: Raft über gRPC (`Jille/raft-grpc-transport` o.ä.)
- Vorteile: Ein Protokoll.
- Nachteile: Drittprojekt mit kleiner Nutzerbasis im kritischsten Pfad.

## Entscheidung

**Option A.**

**PKI und Join**
- CA (ECDSA P-256) entsteht beim Bootstrap; CA-Schlüssel liegt verschlüsselt (ADR 0011) im Raft-Zustand, damit jeder Leader Zertifikate ausstellen kann.
- Join-Token-Format: `uc1.<token-id>.<secret>.<sha256(CA-SPKI)>`; serverseitig nur `hash(secret)` gespeichert, Einmalnutzung, TTL (Standard 1 h), Rolle und optional Mandant. Der Beitretende prüft den CA-Fingerprint vor dem Senden seiner CSR.
- Leaf-Zertifikate: 90 Tage, `NotBefore` −5 min, Erneuerung bei 2/3 der Laufzeit, Ausgabe über `GetCertificate`/`GetClientCertificate`; Verbindungen haben `MaxConnectionAge` 1 h.
- Sperrliste (Serien entfernter Nodes) im Raft-Zustand, geprüft in `VerifyPeerCertificate`.

**Replikation**
- Heartbeat-Schlüssel `(node_id, incarnation, seq)`. Inkarnation = zufällige ID, neu bei leerem Datenverzeichnis.
- `ReplicationService.StreamHeartbeats`: Push-Stream je Peer mit begrenztem Puffer; ist der Puffer voll, wird der Stream geschlossen und der Peer holt per Anti-Entropie nach (kein Blockieren der Checks).
- Anti-Entropie alle 30 s und nach Reconnect: `GetWatermarks` → `FetchRange(node, incarnation, after_seq, limit=5000)`.
- Einfügen idempotent (`ON CONFLICT DO NOTHING`), in Batches.
- Probe-Heartbeats nimmt der verbundene Voter an, speichert sie und verteilt sie wie eigene weiter.

**gRPC-Parameter**
- Client-Keepalive 30 s (Probes) bzw. 15 s (Voter), Server `MinTime` 10 s, `PermitWithoutStream=true`.
- Reconnect mit exponentiellem Backoff (Basis 1 s, max. 30 s) und Jitter.

**Write-Forwarding**
- Follower leiten Schreibbefehle per gRPC `ClusterService.Apply` an den Leader weiter; die Request-ID des Clients wird beibehalten (ADR 0005).

## Konsequenzen

- Positiv: Ein Port, durchgehend mTLS, robuste Replikation ohne Konflikte, Probes NAT-fähig.
- Negativ: Eigener Code für ALPN-Mux, PKI-Workflow und Anti-Entropie – dafür Raft-Spike (AP-0.14) und Chaos-Tests.
- Folgearbeiten: Proto-Definitionen (AP-0.13), Runbook CA-Rotation (P5).

## Approval

| Datum | Status | Durch | Anmerkung |
|-------|--------|-------|-----------|
| 2026-10-19 | Proposed | Claude | |
| 2026-10-19 | Accepted | Projektinhaber | im Chat für alle ADRs 0001–0011 bestätigt |
