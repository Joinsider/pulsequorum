# Pflichtenheft – UptimeCluster

| | |
|---|---|
| **Dokument** | Pflichtenheft (Software Requirements Specification) |
| **Produkt** | UptimeCluster (Arbeitstitel, E20) |
| **Version** | 1.1 |
| **Status** | Freigegeben (ADRs 0001–0013 Accepted) |
| **Datum** | 2026-10-19 |
| **Grundlage** | [Konzept v0.3](../uptime-cluster-konzept.md), Recherche zu Pitfalls (Kap. 12) |
| **Auftraggeber / Product Owner** | Projektinhaber |
| **Verfasser** | Claude (Opus 5.5) |

### Änderungshistorie

| Version | Datum | Änderung |
|---------|-------|----------|
| 1.0-draft | 2026-10-19 | Erstfassung aus Konzept v0.3 und Pitfall-Recherche |
| 1.1 | 2026-10-19 | Präzisierungen aus den Spikes gemäß ADR 0012 und 0013 (FA-NODE-03/05/06, FA-CLU-03/06/08/09/10/13, FA-REP-01/03/07, PF-24) |

---

## Inhaltsverzeichnis

1. [Einleitung](#1-einleitung)
2. [Zielbestimmung](#2-zielbestimmung)
3. [Produkteinsatz](#3-produkteinsatz)
4. [Produktumgebung](#4-produktumgebung)
5. [Systemüberblick](#5-systemüberblick)
6. [Funktionale Anforderungen](#6-funktionale-anforderungen)
7. [Produktdaten](#7-produktdaten)
8. [Nichtfunktionale Anforderungen](#8-nichtfunktionale-anforderungen)
9. [Schnittstellen](#9-schnittstellen)
10. [Benutzungsoberfläche](#10-benutzungsoberfläche)
11. [Rahmenbedingungen](#11-rahmenbedingungen)
12. [Risiken und Pitfalls](#12-risiken-und-pitfalls)
13. [Qualitätssicherung und Abnahme](#13-qualitätssicherung-und-abnahme)
14. [Offene Punkte](#14-offene-punkte)
15. [Glossar](#15-glossar)
16. [Quellen](#16-quellen)

---

## 1. Einleitung

### 1.1 Zweck

Dieses Pflichtenheft beschreibt verbindlich, **was** UptimeCluster leisten muss, und dient als Grundlage für Architekturentscheidungen (ADRs), Umsetzungsplanung ([Roadmap](roadmap.md)), Tests und Abnahme. Das **Wie** wird in ADRs festgelegt; dieses Dokument legt es nur fest, wo eine Lösung aus fachlichen oder Sicherheitsgründen zwingend ist.

### 1.2 Geltungsbereich

Gilt für alle Bestandteile des Produkts: Go-Backend (Voter- und Probe-Rolle), Angular-SPA, Inter-Node-Protokolle, Container-Image, Deployment-Beispiele und Dokumentation. Nicht enthalten sind der Betrieb von Zitadel und PostgreSQL selbst.

### 1.3 Normative Grundlagen

- Gliederung angelehnt an **ISO/IEC/IEEE 29148:2018** (Requirements Engineering) und die klassische Pflichtenheft-Struktur nach Balzert.
- Qualitätsmerkmale nach **ISO/IEC 25010:2023**.
- Sicherheitsanforderungen orientiert an **OWASP ASVS 4.x** und den OWASP Cheat Sheets (Password Storage, Authentication, Session Management, SSRF Prevention).

### 1.4 Konventionen

**Anforderungs-IDs:** `<Typ>-<Bereich>-<Nr>`, z.B. `FA-CHK-03` (funktional, Checks), `NFA-SEC-05` (nichtfunktional, Sicherheit), `PF-12` (Pitfall). IDs sind stabil; entfallene Anforderungen werden als *gestrichen* markiert, nicht neu vergeben.

**Priorität (MoSCoW):**

| Kürzel | Bedeutung |
|--------|-----------|
| **M** | Muss – ohne diese Anforderung ist die jeweilige Phase nicht abnahmefähig |
| **S** | Soll – wichtig, kann begründet in eine spätere Phase verschoben werden |
| **K** | Kann – wünschenswert, nur bei verfügbarem Aufwand |
| **W** | Wird nicht umgesetzt (in diesem Release-Zyklus) |

**Phase:** Phase der [Roadmap](roadmap.md), in der die Anforderung erstmals erfüllt sein muss (P0–P6).

**Verbindlichkeit:** „muss“ = zwingend, „soll“ = Regelfall mit begründeten Ausnahmen, „kann“ = optional.

### 1.5 Bezug zum Konzept

Die Entscheidungen E1–E21 des Konzepts gelten als fachliche Vorgaben. Wo dieses Pflichtenheft das Konzept präzisiert oder aufgrund der Recherche ergänzt, ist dies mit **[neu]** bzw. **[präzisiert]** gekennzeichnet. Bei Widerspruch gilt das Pflichtenheft.

---

## 2. Zielbestimmung

### 2.1 Musskriterien (Release 1.0 = Phasen 0–5)

| ID | Kriterium | Quelle |
|----|-----------|--------|
| Z-M-01 | Überwachung von HTTP(S)-, TCP-, ICMP- und DNS-Zielen mit konfigurierbaren Erwartungen, Intervall ab 20 s | Z1, E3, E5 |
| Z-M-02 | Uptime (24 h / 7 d / 30 d / 365 d), Antwortzeiten und Incidents je Monitor und je Messstandort | Z2 |
| Z-M-03 | Betrieb als Cluster aus 1, 3 oder 5 Votern; Ausfall von ⌊(n−1)/2⌋ Votern ohne Funktionsverlust bei Checks, Alerts und Lesezugriff | Z3, E12 |
| Z-M-04 | Ein Image/Binary für alle Rollen; Wachstum von 1 auf n Voter ohne Datenmigration | Z4 |
| Z-M-05 | Benachrichtigung über Shoutrrr-URLs und generische Webhooks, genau ein Alert pro Statuswechsel (at-least-once, dedupliziert) | E7, Kap. 9 |
| Z-M-06 | Mandantenfähigkeit mit strikter Datentrennung; Identitäten lokal oder über Zitadel | Z6, E13 |
| Z-M-07 | Angular-SPA eingebettet in jeden Voter, mit Client-Failover zwischen Votern | Z7, E15 |
| Z-M-08 | Ohne externe Abhängigkeiten lauffähig (SQLite + Raft) | Z8, E10 |
| Z-M-09 | Probes hinter NAT, inkl. privater Probes pro Mandant | E11, Kap. 7.1 |
| Z-M-10 | Sicher im kommerziellen Mehrmandantenbetrieb (mTLS, SSRF-Schutz, verschlüsselte Geheimnisse) | E9, Kap. 15 |

### 2.2 Sollkriterien

| ID | Kriterium |
|----|-----------|
| Z-S-01 | Optionales PostgreSQL-Backend (E10) |
| Z-S-02 | Externe Auslieferung der SPA (Cloudflare Pages o.ä.) |
| Z-S-03 | Prometheus-Metriken (`/metrics`) |
| Z-S-04 | Export/Import aller Daten (Backend-Wechsel, Backup) |

### 2.3 Kannkriterien (Phase 6)

| ID | Kriterium |
|----|-----------|
| Z-K-01 | Öffentliche Statusseiten pro Mandant mit eigener Domain (E14) |
| Z-K-02 | Push-/Heartbeat-Monitore, weitere Typen (gRPC-Health, SMTP/IMAP, DB, MQTT, Zertifikats-/Domain-Ablauf) |
| Z-K-03 | Zuweisung über Node-Labels |
| Z-K-04 | Gruppen-/Kombi-Monitore |

### 2.4 Abgrenzungskriterien (Nicht-Ziele)

- Kein APM, keine Metrik-/Log-Plattform, keine Ressourcen-Überwachung (CPU/RAM) der Ziele.
- Kein Import aus Uptime Kuma (ggf. später Community-Feature).
- Kein eigener Identity Provider über lokale Nutzer hinaus (kein SSO ohne Zitadel, kein MFA im lokalen Modus in 1.0).
- Keine automatische Entfernung ausgefallener Voter (bewusste Entscheidung, siehe FA-CLU-08).
- Kein Betrieb über mehr als 5 Voter.

---

## 3. Produkteinsatz

### 3.1 Anwendungsbereiche

- **Selbstbetrieb:** Überwachung eigener Dienste von mehreren Standorten (VPS bei verschiedenen Anbietern, Heimnetz-Probe).
- **Open-Source-Nutzung:** Single-Node-Betrieb als Uptime-Kuma-Alternative mit späterer HA-Option.
- **Kommerziell:** gehosteter Mehrmandanten-Dienst, private Probes in Kundennetzen, kommerzielle Lizenz.

### 3.2 Zielgruppen und Rollen

| Rolle | Beschreibung | Technische Rolle |
|-------|--------------|------------------|
| Plattform-Betreiber | betreibt den Cluster, verwaltet Nodes und Mandanten | `platform-admin` |
| Mandanten-Administrator | verwaltet Monitore, Kanäle, Nutzer, private Probes eines Mandanten | `tenant-admin` |
| Mandanten-Bearbeiter | legt Monitore an und ändert sie | `tenant-editor` |
| Mandanten-Leser | sieht Status und Historie | `tenant-viewer` |
| Automatisierung | Zugriff über die REST-API (API-Token, Phase 4) | je nach Token-Rolle |
| Statusseiten-Besucher (P6) | anonym, nur lesend | – |

Annahmen zur Nutzerkompetenz: Betreiber beherrschen Docker/Compose, DNS und TLS-Grundlagen. Mandanten-Nutzer benötigen keine technischen Kenntnisse über den Cluster.

### 3.3 Betriebsbedingungen

- Dauerbetrieb 24/7, unbeaufsichtigt. Updates als Rolling Update Voter für Voter.
- Nodes in unterschiedlichen Rechenzentren/Anbietern mit Latenzen bis ~100 ms untereinander.
- Probes in Netzen mit NAT, ohne eingehende Verbindungen, ggf. instabiler Anbindung.

---

## 4. Produktumgebung

### 4.1 Software

| Komponente | Anforderung |
|------------|-------------|
| Laufzeit | Linux-Container (Docker ≥ 24, Podman ≥ 4, Kubernetes); Binary zusätzlich für linux/amd64, linux/arm64 |
| Basis-Image | distroless/static, Nutzer `nonroot` |
| Zeit | NTP/chrony auf jedem Host (Pflicht) |
| Optional | Zitadel (aktuelle Version, JWT-Access-Tokens), PostgreSQL ≥ 16 |
| Browser | jeweils die zwei aktuellen Hauptversionen von Chrome, Firefox, Safari, Edge |

### 4.2 Hardware (Mindestausstattung je Voter, 1.000 Monitore à 60 s)

| Ressource | Minimum | Empfehlung |
|-----------|---------|------------|
| vCPU | 1 | 2 |
| RAM | 512 MB | 1 GB |
| Speicher | 10 GB SSD | 20 GB SSD (bei abgeschalteter Verdichtung: 50 GB) |

Probe: 0,25 vCPU, 128 MB RAM, 1 GB Speicher (Puffer).

### 4.3 Netzwerk

| Port | Zweck | Erreichbarkeit |
|------|-------|----------------|
| 8080/tcp | REST-API, WebSocket, SPA | öffentlich bzw. hinter Reverse Proxy (TLS) |
| 9443/tcp | Cluster: Raft, Replikation, Probe-Gateway (mTLS) | Voter ↔ Voter; Probes ausgehend zu Votern |

### 4.4 Mengengerüst

| Größe | Zielwert 1.0 |
|-------|--------------|
| Monitore gesamt | ≤ 1.000 (E4) |
| Kürzestes Intervall | 20 s (E5) |
| Voter | 1, 3 oder 5 |
| Probes | ≤ 50 |
| Mandanten | ≤ 500 |
| Nutzer je Mandant | ≤ 100 |
| Gleichzeitige UI-Sitzungen (WebSocket) | ≤ 500 je Voter |
| Heartbeats/Tag (1.000 Monitore à 60 s, `all` auf 3 Votern) | ~4,3 Mio. |

---

## 5. Systemüberblick

Architektur gemäß Konzept Kap. 4–6. Zusammenfassung:

- **Voter** bilden einen Raft-Cluster. Konfiguration und Zustand (Datenklasse A) werden über Raft repliziert und in einer lokalen SQLite-Datenbank je Voter materialisiert.
- **Heartbeats** (Datenklasse B) werden append-only pro Node erzeugt und asynchron an alle Voter repliziert; Anti-Entropie schließt Lücken.
- **Probes** verbinden sich ausgehend per gRPC/mTLS mit einem Voter, abonnieren ihre Monitore und laden Heartbeats hoch.
- **Leader** schreibt Statuswechsel per Raft und versendet Alerts; ohne Quorum greift der Degraded-Modus.
- **SPA** spricht beliebige Voter direkt an; Schreibzugriffe werden intern zum Leader weitergeleitet.

```
SPA ──HTTPS/WSS──► Voter 1..n ◄──mTLS (Raft, Replikation)──► Voter
                       ▲
Probe ──mTLS, ausgehend┘          Voter/Probe ──Checks──► Ziele
Leader ──► Shoutrrr / Webhook
```

---

## 6. Funktionale Anforderungen

Spalten: **ID** · **Anforderung** · **P**riorität · **Ph**ase · **Abnahmekriterium**.

### 6.1 Node-Betrieb und Konfiguration (FA-NODE)

| ID | Anforderung | P | Ph | Abnahmekriterium |
|----|-------------|---|----|------------------|
| FA-NODE-01 | Ein einziges Binary startet abhängig von `NODE_ROLE` (`voter`\|`probe`) die jeweilige Rolle. | M | P0 | Start mit beiden Werten erfolgreich; ungültiger Wert → Exit-Code ≠ 0 mit verständlicher Meldung. |
| FA-NODE-02 | Konfiguration ausschließlich über Umgebungsvariablen (optional zusätzlich Datei); alle Optionen dokumentiert mit Default. Ungültige Konfiguration bricht den Start ab. | M | P0 | Tabelle aller Variablen in der Doku; Test für Validierung. |
| FA-NODE-03 **[neu]** | Beim ersten Start erzeugt jeder Node eine **zufällige, persistente Node-ID** im Datenverzeichnis und eine **Inkarnations-ID**, die in der Heartbeat-Datenbank (`metrics.db`) selbst gespeichert wird. Fehlt `metrics.db`, entsteht immer eine neue Inkarnation, auch wenn die Node-ID erhalten bleibt (ADR 0012 B4). Die Node-ID wird nie aus IP oder Hostname abgeleitet. | M | P1 | Neustart behält ID und Inkarnation; Löschen von `metrics.db` oder des Datenverzeichnisses erzeugt eine neue Inkarnation. (PF-03, PF-07, PF-12) |
| FA-NODE-04 | Graceful Shutdown: laufende Checks und Schreibvorgänge werden innerhalb von 10 s beendet bzw. abgeschlossen; ein Leader gibt die Führung vorher ab (Leadership Transfer). | M | P2 | SIGTERM-Test: keine abgebrochenen Transaktionen, neuer Leader < 5 s. |
| FA-NODE-05 | `GET /api/v1/health` liefert Rolle, Node-ID, Version, Leader, Quorum-Status, Raft-Replikationsstand (`applied_index` gegenüber dem Commit-Index des Leaders, `LastContact`; Zustand `stale` ab Schwellwert, ADR 0012 A9), Heartbeat-Replikationsrückstand, Uhrzeitabweichung, ICMP-Modus. Liveness und Readiness getrennt (`/healthz`, `/readyz`). | M | P0 (Basis), P2 (Cluster-Felder) | Contract-Test gegen OpenAPI. |
| FA-NODE-06 **[neu]** | Start-Selbsttest: prüft Schreibbarkeit des Datenverzeichnisses, effektive SQLite-Pragmas (`journal_mode`, `busy_timeout`, `synchronous`, `foreign_keys`, `auto_vacuum` auf jeder Verbindung, `journal_size_limit` am Schreib-Handle; ADR 0013 C3), verfügbaren ICMP-Modus, Uhrzeitabweichung zu den Peers. Ergebnisse erscheinen in Health und Log. | M | P1 | Test mit absichtlich falscher Konfiguration. (PF-11, PF-17) |
| FA-NODE-07 | Strukturierte Logs (JSON oder Text, wählbar) über `log/slog`; Log-Level zur Laufzeit nicht nötig, per Variable einstellbar. Keine Geheimnisse in Logs. | M | P0 | Log-Review-Test: maskierte Kanal-URLs. |

### 6.2 Cluster und Konsens (FA-CLU)

| ID | Anforderung | P | Ph | Abnahmekriterium |
|----|-------------|---|----|------------------|
| FA-CLU-01 | Ein Voter ohne Join-Konfiguration bildet einen Raft-Cluster mit einem Mitglied (Bootstrap). | M | P1 | Single-Node startet, ist Leader, nimmt Schreibzugriffe an. |
| FA-CLU-02 **[präzisiert]** | Bootstrap vs. Join wird **aus dem persistierten Zustand** entschieden (vorhandenes Raft-Log/Snapshot), nicht allein aus Konfigurationsflags. Ein bereits initialisierter Node bootstrappt nie erneut. | M | P1 | Neustart eines Clusters mit geänderten Env-Variablen bildet keinen zweiten Cluster. (PF-01) |
| FA-CLU-03 | Beitritt weiterer Voter über `CLUSTER_JOIN` + einmaliges Join-Token; der neue Voter wird zunächst als Nicht-Voter aufgenommen, holt den Zustand nach und wird zum Voter befördert, sobald er **mindestens den Index seines eigenen `AddNonvoter`-Eintrags angewandt hat** und weniger als 100 Einträge zurückliegt (Abfrage per `ClusterService.GetStatus`). Nach 10 min ohne Erfolg bleibt er Nicht-Voter, und es gibt einen Systemalert. Nach einem Leaderwechsel setzt der Join idempotent fort (ADR 0012 A3/A4). | M | P2 | 1→3 Voter ohne Unterbrechung der Checks; ein leerer Joiner wird nie vor dem Aufholen befördert. (PF-02) |
| FA-CLU-04 **[neu]** | Mitgliedschaftsänderungen werden **serialisiert**: höchstens eine gleichzeitig, die nächste erst nach Commit der vorherigen. | M | P2 | Paralleler Join zweier Nodes wird nacheinander abgearbeitet. (PF-02) |
| FA-CLU-05 | Schreibende API-Requests an einen Follower werden intern an den Leader weitergeleitet; der Client merkt davon nichts. | M | P2 | Schreiben über jeden Voter erfolgreich. |
| FA-CLU-06 **[neu]** | Jeder schreibende Raft-Befehl trägt eine **Request-ID**; die FSM dedupliziert anhand einer persistierten Tabelle (Teil des Snapshots). Fehlerabbildung (ADR 0012 A8): `ErrNotLeader`/`ErrEnqueueTimeout` → intern erneut versuchen bzw. 503 mit `Retry-After`; `ErrLeadershipLost`, `ErrRaftShutdown` und Transportfehler beim Forwarding → HTTP 503 „Ergebnis unbekannt“, nicht „fehlgeschlagen“. Wiederholung mit gleicher ID ist idempotent. Forwarding hat eine Deadline von ≤ 3 s; ist `LastContact` zum Leader älter als 2 × `HeartbeatTimeout`, antwortet der Follower sofort mit 503 (ADR 0012 B5). | M | P2 | Test: Retry nach Leaderwechsel erzeugt keinen doppelten Monitor. (PF-04) |
| FA-CLU-07 **[präzisiert]** | Die FSM-`Apply`-Funktion ist deterministisch: Zeitstempel und IDs werden vom Leader in den Befehl geschrieben; kein `time.Now()`, kein Zufall, keine Map-Iterationsreihenfolge in `Apply`. | M | P1 | Determinismus-Test: dasselbe Log auf zwei FSMs ergibt identische Datenbankinhalte. (PF-05) |
| FA-CLU-08 **[neu]** | Ausgefallene Voter werden **nie automatisch** entfernt. Entfernen nur durch `platform-admin` (API/UI), mit Warnung bei Quorum-Gefährdung. Für Quorum-Verlust existiert ein dokumentiertes Recovery-Verfahren (Runbook). **Knoten ersetzen:** Ein Join mit neuer Node-ID unter bereits vergebener Adresse wird mit HTTP 409 und der alten Node-ID abgelehnt; UI/CLI bieten „Knoten ersetzen“ (Bestätigung → Entfernen alt → Join neu, serialisiert) mit Hinweis auf die fehlende Ausfallreserve währenddessen (ADR 0012 A5). | M | P2 | Runbook getestet im Chaos-Test. (PF-06) |
| FA-CLU-09 | Raft-Snapshots enthalten nur Datenklasse A: Snapshot = `VACUUM INTO` der Datei `state.db` (die ausschließlich Klasse A inkl. Dedup-Tabelle und angewandtem Index enthält), nie per Dateikopie einer laufenden DB; Restore per Datei-Tausch. Beim Start wird ein Snapshot nur eingespielt, wenn `state.db` älter ist als der neueste Snapshot (ADR 0012 A1/A2). `TrailingLogs` ist so bemessen, dass ein Follower nach Snapshot-Restore per Log aufholen kann. | M | P1 | Restore-Test mit 10.000 Monitoren. (PF-08) |
| FA-CLU-10 | Adressänderung eines Voters (neue IP/DNS) und Ersetzen eines Knotens (FA-CLU-08) sind über ein dokumentiertes Verfahren möglich, ohne Quorum zu verlieren. Nodes werden über stabile DNS-Namen adressiert. | S | P2 | Test: Voter mit neuer IP tritt wieder bei. (PF-07) |
| FA-CLU-13 **[neu]** | Ein Fehler beim Anwenden eines Log-Eintrags in der FSM hält den Node an (Readiness false, Prozessende mit Fehler); er wird nie still übergangen. Ab P3 vergleichen die Voter regelmäßig einen Fingerprint der Klasse-A-Daten (Systemalert bei Abweichung) (ADR 0012 A7). | M | P1 (Anhalten), P3 (Fingerprint) | Test: absichtlich fehlschlagender Befehl beendet den Node; Fingerprint-Abweichung erzeugt Alert. (PF-05) |
| FA-CLU-11 | UI/Health warnen bei gerader Voter-Anzahl (2, 4) und bei > 5 Votern. | S | P2 | Anzeige im Admin-Bereich. |
| FA-CLU-12 **[neu]** | Rolling Updates: Nodes unterschiedlicher Minor-Versionen müssen gemeinsam laufen können. Neue Raft-Befehle/Schema-Features werden erst aktiviert, wenn alle Voter die nötige Version melden (Feature-Gate über Raft). | M | P2 | Upgrade-Test n → n+1 Voter für Voter. (PF-09) |

### 6.3 Inter-Node-Sicherheit und Join (FA-PKI)

| ID | Anforderung | P | Ph | Abnahmekriterium |
|----|-------------|---|----|------------------|
| FA-PKI-01 | Beim Bootstrap erzeugt der Cluster eine eigene CA (Ed25519 oder ECDSA P-256). Sämtliche Inter-Node-Verbindungen (Raft, Replikation, Probe-Gateway) laufen über mTLS mit von dieser CA signierten Zertifikaten. | M | P2 | Verbindung ohne gültiges Client-Zertifikat wird abgewiesen. |
| FA-PKI-02 **[präzisiert]** | Join-Tokens sind einmalig verwendbar, zeitlich begrenzt (Standard 1 h), rollengebunden (`voter`\|`probe` [+ Mandant]) und serverseitig nur als Hash gespeichert. Das Token enthält den **Fingerprint der Cluster-CA**; der beitretende Node prüft ihn, bevor er Daten sendet (kein Trust-on-first-use). | M | P2 | Test: Token mit falschem CA-Fingerprint, abgelaufenes und wiederverwendetes Token werden abgelehnt. (PF-10) |
| FA-PKI-03 | Node-Zertifikate laufen begrenzt (Standard 90 Tage) und werden automatisch vor Ablauf erneuert; `NotBefore` wird um 5 min zurückdatiert (Clock-Skew). Neue Zertifikate greifen über `GetCertificate`-Callbacks bei neuen Handshakes; Verbindungen haben eine maximale Lebensdauer, damit rotierte Zertifikate wirksam werden. | M | P2 | Rotation im Test ohne Unterbrechung. (PF-11) |
| FA-PKI-04 | Zertifikate entfernter Nodes werden widerrufen (Sperrliste im Raft-Zustand, geprüft bei jedem Handshake). | M | P2 | Entfernter Node kann sich nicht mehr verbinden. |
| FA-PKI-05 | CA-Rotation mit überlappendem Trust-Bundle ist möglich (dokumentiertes Verfahren). | S | P5 | Runbook getestet. |
| FA-PKI-06 | Ablauf-Warnungen für CA und Node-Zertifikate als Systemalert (30/7/1 Tage). | M | P3 | Systemalert im Test. |

### 6.4 Heartbeat-Replikation (FA-REP)

| ID | Anforderung | P | Ph | Abnahmekriterium |
|----|-------------|---|----|------------------|
| FA-REP-01 **[präzisiert]** | Heartbeats werden mit dem Schlüssel **(node_id, incarnation, seq)** gespeichert; `seq` ist pro Inkarnation streng monoton. Einfügungen sind idempotent (Konflikt = ignorieren). Heartbeat-Tabelle als normale rowid-Tabelle (ADR 0013 C2). | M | P1 (Schema), P2 (Replikation) | Doppelte Zustellung erzeugt keine Duplikate. (PF-12) |
| FA-REP-02 | Jeder Voter streamt eigene und von Probes empfangene Heartbeats an alle anderen Voter. | M | P2 | Nach 1 min haben alle Voter identische Heartbeat-Mengen (Prüfsumme je Node/Inkarnation). |
| FA-REP-03 | Anti-Entropie: Voter tauschen periodisch, nach Wiederverbindung und bei Raft-Ereignissen (Peer wieder erreichbar) Hochwassermarken je (node_id, incarnation) aus – definiert als höchstes `seq`, bis zu dem lückenlos alles vorhanden ist (ab Retention-Horizont), nicht `max(seq)` (ADR 0012 B2/B6) – und fordern fehlende Bereiche seitenweise an (begrenzte Seitengröße, begrenzte Parallelität je Peer). | M | P2 | Nach 1 h Netztrennung schließt sich die Lücke vollständig, ohne Speicher-Spitzen > 100 MB. |
| FA-REP-07 **[neu]** | Der Push-Pfad ist Best Effort: fehlgeschlagene Batches werden verworfen, nicht erneut gesendet; die Anti-Entropie ist maßgeblich und zieht alle Ströme, die der Peer kennt, auch die dritter Knoten (ADR 0012 B3). | M | P2 | Test: verworfene Batches werden nach Wiederverbindung vollständig nachgeholt. |
| FA-REP-04 | Backpressure: Ein langsamer Peer bremst weder Checks noch die Replikation zu anderen Peers. | M | P2 | Test mit gedrosseltem Peer. |
| FA-REP-05 | Replikationsrückstand je Peer wird gemessen, in Health/Admin-UI angezeigt und ab Schwellwert (Standard 5 min) als Systemalert gemeldet. | M | P3 | Systemalert im Test. |
| FA-REP-06 **[neu]** | Ein unerwarteter Inkarnationswechsel eines Nodes (Datenverlust) wird erkannt und als Systemalert gemeldet; alte Inkarnationen bleiben bis zum Ablauf der Aufbewahrung gültig. | M | P3 | Test: Datenverzeichnis löschen → Alert, keine Datenverluste anderer Nodes. (PF-12) |

### 6.5 Monitor-Verwaltung (FA-MON)

| ID | Anforderung | P | Ph | Abnahmekriterium |
|----|-------------|---|----|------------------|
| FA-MON-01 | CRUD für Monitore mit Name, Typ, typspezifischer Konfiguration, Intervall, Timeout, Retries, Zuweisung, DOWN-Regel, Tags, Beschreibung. | M | P1 | API- und UI-Test. |
| FA-MON-02 | Typspezifische Konfiguration wird gegen das JSON-Schema des Checker-Plugins validiert (Server **und** Client). | M | P1 | Ungültige Konfiguration → HTTP 422 mit Feldfehlern. |
| FA-MON-03 **[präzisiert]** | Intervall ≥ 20 s (bzw. Mandanten-Limit); **Timeout ≤ 80 % des Intervalls**; Retries 0–10. | M | P1 | Validierung im Test. (PF-15) |
| FA-MON-04 | Pausieren/Fortsetzen; pausierte Monitore erzeugen keine Heartbeats und zählen als „pausiert“ (nicht DOWN, nicht in Uptime). | M | P1 | Test. |
| FA-MON-05 | Klonen eines Monitors. | S | P1 | UI-Test. |
| FA-MON-06 | Massenaktionen (pausieren, fortsetzen, löschen, Tags) über die API. | S | P4 | API-Test. |
| FA-MON-07 | Löschen entfernt Konfiguration sofort; zugehörige Heartbeats/Rollups werden asynchron gelöscht. | M | P1 | Test. |

### 6.6 Checks (FA-CHK)

**Allgemein**

| ID | Anforderung | P | Ph | Abnahmekriterium |
|----|-------------|---|----|------------------|
| FA-CHK-01 | Plugin-Schnittstelle gemäß Konzept 11.2 (`Type`, `ConfigSchema`, `Validate`, `Check`); Registrierung zur Startzeit; Voter und Probes nutzen dieselben Plugins. | M | P1 | Neuer Typ ohne Änderung an Scheduler/UI integrierbar (Test-Plugin). |
| FA-CHK-02 | Jeder Check erhält einen `context.Context` mit Deadline; nach Ablauf wird er abgebrochen, alle Goroutinen enden. | M | P1 | Goroutine-Leak-Test (`goleak`). (PF-16) |
| FA-CHK-03 **[neu]** | Ergebnisse enthalten neben Status/Latenz eine **Fehlerklasse** (`dns`, `connect`, `tls`, `timeout`, `http_status`, `assertion`, `internal`) und – wo anwendbar – Phasenzeiten und aufgelöste IP. | M | P1 | Für jede Fehlerklasse ein Test. (PF-14) |
| FA-CHK-04 **[neu]** | Alle ausgehenden Verbindungen der Checker laufen über einen zentralen Dialer mit Hook für den SSRF-Schutz (FA-SEC-SSRF) – bereits ab P1, Durchsetzung konfigurierbar. | M | P1 | Architekturtest: kein Checker nutzt eigenen Dialer. |

**HTTP(S)**

| ID | Anforderung | P | Ph | Abnahmekriterium |
|----|-------------|---|----|------------------|
| FA-CHK-10 | Methode, Header, Body, Basic-/Bearer-Auth, erwartete Statuscodes als Liste/Bereiche (`200-299,301`), Keyword vorhanden/nicht vorhanden (optional Groß-/Kleinschreibung), JSON-Pfad = Wert, max. Antwortzeit, Redirects folgen ja/nein mit Limit (Standard 10). | M | P1 | Tabellentests gegen `httptest.Server`. |
| FA-CHK-11 **[neu]** | Jeder Check nutzt eine **frische Verbindung** (kein Connection-Reuse zwischen Checks, keine geteilten Transports zwischen Monitoren), damit Ausfälle beim Verbindungsaufbau nicht verdeckt werden. | M | P1 | Test: Server, der neue Verbindungen ablehnt, wird als DOWN erkannt. (PF-13) |
| FA-CHK-12 **[neu]** | Getrennte Timeouts für DNS/Dial, TLS-Handshake, Response-Header und Gesamtdauer; Body-Lesen begrenzt auf konfigurierbare Größe (Standard 1 MiB) für Keyword/JSON-Prüfung. | M | P1 | Tests für jeden Timeout. (PF-14) |
| FA-CHK-13 **[neu]** | Proxy-Umgebungsvariablen (`HTTP_PROXY` etc.) werden ignoriert; Proxy nur explizit pro Monitor (S, P6). Fester, identifizierbarer User-Agent (`UptimeCluster/<version>`), überschreibbar. | M | P1 | Test mit gesetzter `HTTP_PROXY`. (PF-13) |
| FA-CHK-14 **[präzisiert]** | TLS: Gültigkeit der Kette inkl. Hostname (SNI) prüfen; Ablauf-Warnung N Tage vorher (Standard 14) als eigener Status **DEGRADED**. Bei „Zertifikatsfehler ignorieren“ wird die Kette dennoch separat geprüft und das Ablaufdatum ermittelt. | M | P1 | Tests mit abgelaufenem, selbstsigniertem und fast ablaufendem Zertifikat. |
| FA-CHK-15 | IPv4/IPv6 wählbar (`auto`, `ipv4`, `ipv6`). | S | P1 | Test. |

**TCP**

| ID | Anforderung | P | Ph | Abnahmekriterium |
|----|-------------|---|----|------------------|
| FA-CHK-20 | Host + Port erreichbar, Verbindungszeit gemessen; optional TLS-Handshake. | M | P1 | Tests. |

**ICMP**

| ID | Anforderung | P | Ph | Abnahmekriterium |
|----|-------------|---|----|------------------|
| FA-CHK-30 | Erreichbarkeit, Paketverlust, RTT (min/avg/max) mit konfigurierbarer Paketanzahl (Standard 3). | M | P1 | Test gegen localhost. |
| FA-CHK-31 **[neu]** | ICMP-Modus ist konfigurierbar (`unprivileged` per UDP-ICMP-Socket, `privileged` per Raw-Socket, `auto`) und wird beim Start geprüft. Ein Node ohne funktionierendes ICMP meldet die **Capability** `icmp=false`; ICMP-Monitore werden ihm nicht zugewiesen. | M | P1 (Modus), P2 (Capability-Zuweisung) | Test in Container mit/ohne `NET_RAW` bzw. `ping_group_range`. (PF-17) |
| FA-CHK-32 | Dokumentierte Container-Konfigurationen für Docker, Podman rootless, Kubernetes. | M | P1 | Doku vorhanden, manuell verifiziert. |

**DNS**

| ID | Anforderung | P | Ph | Abnahmekriterium |
|----|-------------|---|----|------------------|
| FA-CHK-40 | Record-Typen A, AAAA, CNAME, MX, TXT, NS, SOA, SRV, CAA; erwarteter Wert; eigener Resolver (IP:Port) oder System-Resolver. | M | P1 | Tests gegen eingebetteten Test-DNS-Server. |
| FA-CHK-41 **[neu]** | Bei abgeschnittener Antwort (TC-Bit) automatischer Wiederholungsversuch per TCP. Vergleich von Mehrfachantworten **reihenfolgeunabhängig** (Mengenvergleich), Modi „enthält“ und „gleich“. | M | P1 | Tests. (PF-18) |
| FA-CHK-42 | AD-Bit wird als Detail ausgewiesen; DNSSEC-Validierung ist kein Fehlerkriterium in 1.0. | K | P6 | – |

### 6.7 Scheduler und Zuweisung (FA-SCH)

| ID | Anforderung | P | Ph | Abnahmekriterium |
|----|-------------|---|----|------------------|
| FA-SCH-01 | Jeder Node plant genau die Checks, für die er zuständig ist; Zuweisungsmodi `all`, `count: k`, `nodes: [...]` (E1). | M | P1 (`all` lokal), P2 (alle Modi) | Tests mit 3 Nodes. |
| FA-SCH-02 | `count: k` verteilt per Consistent Hashing (Rendezvous oder Ring) über die **gesunden, fähigen** Nodes; bei Ausfall übernimmt deterministisch der nächste. | M | P2 | Test: Node-Ausfall → Übernahme innerhalb 2 Intervallen. |
| FA-SCH-03 **[präzisiert]** | **Jitter auch beim ersten Lauf**: Startzeitpunkte werden gleichmäßig über das Intervall verteilt (deterministisch aus Monitor-ID abgeleitet), damit kein Thundering Herd nach Start/Neustart entsteht. | M | P1 | Messung: 1.000 Monitore à 60 s → max. 3× Durchschnittslast pro Sekunde. (PF-15) |
| FA-SCH-04 | Ein Check eines Monitors läuft nie parallel zu sich selbst; Überläufe werden übersprungen und gezählt. | M | P1 | Test. |
| FA-SCH-05 | Planungsgenauigkeit: Abweichung vom Sollzeitpunkt ≤ 1 s bei Nennlast. | M | P1 | Lasttest. |
| FA-SCH-06 | Konfigurationsänderungen wirken innerhalb von 5 s auf allen zuständigen Nodes. | M | P2 | Test. |
| FA-SCH-07 | Begrenzung gleichzeitiger Checks je Node (konfigurierbar, Standard 256). | M | P1 | Test. |

### 6.8 Konsens-Status (FA-CON)

| ID | Anforderung | P | Ph | Abnahmekriterium |
|----|-------------|---|----|------------------|
| FA-CON-01 | Konsens-Status aus den jüngsten Heartbeats der zuständigen Nodes gemäß DOWN-Regel `majority` (Standard), `any`, `all` (E2). | M | P3 | Tabellentests aller Kombinationen. |
| FA-CON-02 | Status **PARTIAL**, wenn ein Teil DOWN meldet, die Regel aber nicht erfüllt ist. | M | P3 | Test. |
| FA-CON-03 **[präzisiert]** | Retries: Ein Monitor wechselt erst nach `retries + 1` aufeinanderfolgenden negativen Konsens-Bewertungen auf DOWN (dazwischen PENDING); Rückkehr zu UP nach konfigurierbarer Anzahl positiver Bewertungen (Standard 1). | M | P1 (Single), P3 (Konsens) | Tests. (PF-20) |
| FA-CON-04 **[präzisiert]** | **Node-Selbstgesundheit:** Ein Node, dessen eigene Konnektivität gestört ist (konfigurierbare Referenzziele schlagen fehl **oder** Anteil fehlgeschlagener Checks über Schwellwert bei gleichzeitig gesunden Peers), wird als `unhealthy` markiert und aus der Bewertung genommen. | M | P3 | Test: Node mit blockiertem Egress erzeugt keinen DOWN-Alarm. (PF-19) |
| FA-CON-05 | Heartbeats älter als 2 × Intervall gelten als fehlend; fehlende Meldungen zählen nicht als DOWN. Meldet kein zuständiger Node, ist der Status UNKNOWN. | M | P3 | Test. |
| FA-CON-06 | Konsens-Bewertung erfolgt auf dem Leader; der resultierende `MonitorState` wird per Raft geschrieben. | M | P3 | Test. |

### 6.9 Uptime, Rollups und Aufbewahrung (FA-UPT)

| ID | Anforderung | P | Ph | Abnahmekriterium |
|----|-------------|---|----|------------------|
| FA-UPT-01 | Uptime % = Zeit_UP / (Gesamtzeit − Wartung − unbekannt) × 100 für 24 h / 7 d / 30 d / 365 d, je Konsens und je Node. PARTIAL zählt standardmäßig als UP (pro Mandant einstellbar). | M | P1 | Referenzberechnungen im Test. |
| FA-UPT-02 **[präzisiert]** | Zeiten werden als **UTC-Millisekunden (Integer)** gespeichert. Uptime je Node wird ausschließlich aus dem geordneten Heartbeat-Strom dieses Nodes berechnet; Ordnung über Nodes hinweg nie über Zeitdifferenzen verschiedener Uhren. | M | P1 | Test mit simuliertem Clock-Skew von 2 s. (PF-21) |
| FA-UPT-03 | Stunden- und Tages-Rollups (up/down/maint/unknown-Sekunden, Latenz avg/p95) werden auf jedem Voter deterministisch aus denselben Daten berechnet. | M | P1 | Zwei Voter liefern identische Rollups (Test). |
| FA-UPT-04 | Verdichtung (E19, Standard an, nach 48 h): Statuswechsel bleiben exakt, Latenzen werden je Minute/Node/Monitor aggregiert (min/avg/max/p95, OK/Fehler-Anzahl). Abschaltbar (`RAW_COMPACTION=false`). | M | P1 | Uptime vor/nach Verdichtung identisch (Test). |
| FA-UPT-05 | Aufbewahrung: Rohdaten 30 d (bzw. verdichtet), Stunden-Rollups 1 Jahr, Tages-Rollups unbegrenzt (E6); Werte konfigurierbar. | M | P1 | Test mit simulierter Zeit. |
| FA-UPT-06 **[neu]** | Löschungen für Aufbewahrung laufen **in kleinen Batches** (Standard ≤ 5.000 Zeilen pro Transaktion) mit Pausen; freier Speicher wird über inkrementelles Vacuum zurückgewonnen; Dateigröße wird überwacht. | M | P1 | Lasttest: Retention-Lauf blockiert Schreibzugriffe nie > 200 ms. (PF-23, PF-24) |
| FA-UPT-07 | Unbestätigte Statuswechsel aus dem Degraded-Modus werden nach Wiederherstellung des Quorums neu bewertet. | M | P3 | Test. |

### 6.10 Incidents (FA-INC)

| ID | Anforderung | P | Ph | Abnahmekriterium |
|----|-------------|---|----|------------------|
| FA-INC-01 | Ein Incident beginnt mit dem Wechsel auf DOWN und endet mit der Rückkehr zu UP; Ursache (Fehlermeldung, betroffene Nodes) wird gespeichert. | M | P1 | Test. |
| FA-INC-02 | Incident-Liste je Mandant, filterbar nach Monitor und Zeitraum. | M | P1 | API-/UI-Test. |
| FA-INC-03 | Incidents können kommentiert/bestätigt (acknowledged) werden. | K | P6 | – |

### 6.11 Alerting und Benachrichtigungen (FA-ALR)

| ID | Anforderung | P | Ph | Abnahmekriterium |
|----|-------------|---|----|------------------|
| FA-ALR-01 | Benachrichtigungskanäle je Mandant: Shoutrrr-URL oder generischer Webhook (E7); Zuordnung Kanal ↔ Monitor (n:m) und Standardkanäle. | M | P1 | Test. |
| FA-ALR-02 | Alerts **nur bei Statuswechsel** (UP↔DOWN, optional DEGRADED/PARTIAL), optional Wiederholung alle N Minuten solange DOWN. | M | P1 | Test. (PF-25) |
| FA-ALR-03 **[präzisiert]** | Versand über eine **persistente Outbox** (at-least-once): Retry mit exponentiellem Backoff + Jitter, max. Versuche konfigurierbar; der Alert-Zustand liegt im Raft-Log, ein neuer Leader setzt offene Zustellungen fort (Deduplizierung über stabile Alert-ID = Monitor + Incident + Übergang). | M | P1 (lokal), P3 (Cluster) | Test: Leaderwechsel während Versand → genau eine Benachrichtigung je Übergang pro Kanal (Duplikate nur bei unbestätigter Zustellung). |
| FA-ALR-04 **[neu]** | Rate-Limit je Ziel (Token Bucket); HTTP 429 mit `retry_after` wird beachtet; mehrere gleichzeitige Statuswechsel eines Mandanten werden zu einer Nachricht gebündelt (Standard-Fenster 30 s, abschaltbar). | M | P3 | Test mit simuliertem 429. (PF-26) |
| FA-ALR-05 **[neu]** | Dauerhafte Fehler (HTTP 401/403/404, ungültige URL) werden **nicht** wiederholt; der Kanal wird als fehlerhaft markiert und der `tenant-admin` informiert (UI + ggf. anderer Kanal). | M | P3 | Test. (PF-26) |
| FA-ALR-06 **[neu]** | Flapping-Erkennung: Mehr als N Wechsel in Fenster T (Standard 6 in 30 min) → Status „flapping“, Einzelalerts unterdrückt, eine Zusammenfassung. | S | P3 | Test. |
| FA-ALR-07 **[präzisiert]** | Webhook-Templates erzeugen **gültiges JSON**: Werte werden JSON-kodiert eingesetzt (kein String-Zusammenbau); Template-Funktionen sind eingeschränkt; ungültiges Template wird beim Speichern abgelehnt. | M | P1 | Test mit Monitornamen, die `"`, `\` und Zeilenumbrüche enthalten. (PF-27) |
| FA-ALR-08 | Testversand je Kanal (`POST /notifications/{id}/test`). | M | P1 | Test. |
| FA-ALR-09 | Kanal-URLs enthalten Geheimnisse: verschlüsselt gespeichert (FA-SEC-SEC), im UI und in Logs maskiert. | M | P1 | Test. |
| FA-ALR-10 | Degraded-Modus (E8): Ohne Quorum versendet je Minderheitspartition der erreichbare Voter mit der kleinsten ID die Alerts mit Kennzeichnung „⚠ unbestätigt – Cluster degraded (x/n Nodes)“; lokales Versandprotokoll; nach Quorum-Rückkehr Abgleich und ggf. Korrektur („Fehlalarm“/„bestätigt“). | M | P3 | Partitionstest. (PF-28) |
| FA-ALR-11 **[neu]** | Jeder Alert enthält die Sicht des Absenders (Anzahl erreichbarer Voter, meldende Nodes), damit Empfänger Duplikate aus mehreren Partitionen einordnen können. | M | P3 | Test. (PF-28) |
| FA-ALR-12 | Benachrichtigungsziele unterliegen demselben SSRF-Schutz wie Checks. | M | P4 | Test. (PF-29) |
| FA-ALR-13 | Wartungsfenster unterdrücken Alerts der betroffenen Monitore. | M | P1 | Test. |

### 6.12 Wartungsfenster (FA-MNT)

| ID | Anforderung | P | Ph | Abnahmekriterium |
|----|-------------|---|----|------------------|
| FA-MNT-01 | Einmalige und wiederkehrende Wartungsfenster (täglich, wöchentlich, monatlich, Cron-Ausdruck) je Mandant mit Zeitzone. | M | P1 | Tests inkl. Sommerzeitwechsel. |
| FA-MNT-02 | Während Wartung: Status MAINTENANCE, zählt nicht in die Uptime, keine Alerts. | M | P1 | Test. |

### 6.13 Authentifizierung und Autorisierung (FA-AUTH)

| ID | Anforderung | P | Ph | Abnahmekriterium |
|----|-------------|---|----|------------------|
| FA-AUTH-01 | Modus `local` (Standard): lokale Nutzer mit Passwort (Argon2id, mindestens m=19 MiB, t=2, p=1 gemäß OWASP), ein Mandant `default`. Erster Start: Einrichtung des ersten `platform-admin` über einmaligen Setup-Link/Token im Log. | M | P1 | Test; kein Default-Passwort. |
| FA-AUTH-02 | Lokale Tokens: vom Cluster signierte JWTs (EdDSA, `kid`), Signaturschlüssel im Raft-Zustand (verschlüsselt), Schlüsselrotation mit Überlappung. Access-Token-Laufzeit ≤ 15 min. | M | P1 | Test: Token eines Voters wird von jedem anderen Voter akzeptiert (P2). |
| FA-AUTH-03 | Refresh-Tokens werden bei jeder Nutzung rotiert; Wiederverwendung eines alten Tokens widerruft die ganze Token-Familie. | M | P1 | Test. |
| FA-AUTH-04 **[neu]** | Login-Drosselung **pro Konto** (und zusätzlich pro IP), progressive Verzögerung, Sperre mit Ablauf; Fehlermeldungen verraten nicht, ob ein Konto existiert. | M | P1 | Test. (PF-33) |
| FA-AUTH-05 | Modus `zitadel`: SPA nutzt Authorization Code Flow mit PKCE; Voter validieren **JWT-Access-Tokens** lokal über gecachte JWKS (Issuer exakt, `aud` = Projekt-ID, `exp`/`nbf`, Algorithmus fest). Opake Tokens werden mit klarer Fehlermeldung abgelehnt. | M | P4 | Test gegen Zitadel-Testinstanz bzw. Mock-IdP. (PF-31) |
| FA-AUTH-06 | Mandant aus der Zitadel-Organisation, Rollen aus dem Projekt-Rollen-Claim; **die Autorisierung prüft immer Rollen-Claim + Organisation**, nicht nur die Audience. | M | P4 | Test: Token mit fremder Audience/fehlender Rolle → 403. (PF-31) |
| FA-AUTH-07 | Zitadel nicht erreichbar → bestehende Sitzungen, Checks und Alerts laufen weiter (JWKS-Cache, Refetch bei unbekanntem `kid` mit Rate-Limit). | M | P4 | Test mit abgeschaltetem IdP. |
| FA-AUTH-08 | Migration `local` → `zitadel`: Mandant `default` wird einer Organisation zugeordnet. | S | P4 | Test. |
| FA-AUTH-09 | Rollen `platform-admin`, `tenant-admin`, `tenant-editor`, `tenant-viewer` (Konzept 12.2); serverseitig an jedem Endpunkt durchgesetzt. | M | P1 (lokal), P4 (Zitadel) | Berechtigungsmatrix-Test für alle Endpunkte. |
| FA-AUTH-10 | API-Tokens (langlebig, mandanten- und rollengebunden, widerrufbar, nur gehasht gespeichert). | S | P4 | Test. |
| FA-AUTH-11 **[neu]** | WebSocket-Authentifizierung über ein **kurzlebiges Ticket** (≤ 30 s, einmalig), das per HTTPS geholt wird; kein Langzeit-Token in der URL. Verbindungen werden bei Ablauf der Sitzung geschlossen. | M | P1 | Test. (PF-32) |

### 6.14 Mandanten (FA-TEN)

| ID | Anforderung | P | Ph | Abnahmekriterium |
|----|-------------|---|----|------------------|
| FA-TEN-01 | `tenant_id` an allen mandantenbezogenen Daten **ab P1** (auch im Single-Tenant-Modus). Jede Repository-Methode erhält die Tenant-ID explizit und filtert danach. | M | P1 | Automatisierter Isolationstest über alle Repository-Methoden. |
| FA-TEN-02 | Mandanten-Kontext ausschließlich aus dem Token, nie aus URL-/Body-Parametern. | M | P1 | Test: Manipulierte IDs fremder Mandanten → 404. |
| FA-TEN-03 | Mandanten-Verwaltung durch `platform-admin`: anlegen, sperren, löschen (mit Datenlöschung), Plan/Limits. Automatische Anlage beim ersten Login einer neuen Zitadel-Organisation (abschaltbar). | M | P4 | Test. |
| FA-TEN-04 | Limits je Plan: max. Monitore, kürzestes Intervall, Statusseiten, private Probes, Kanäle; Überschreitung → HTTP 403 mit verständlicher Meldung. | M | P4 | Test. |
| FA-TEN-05 | WebSocket-Events werden strikt nach Mandant gefiltert. | M | P1 | Test mit zwei Mandanten. |

### 6.15 Probes (FA-PRB)

| ID | Anforderung | P | Ph | Abnahmekriterium |
|----|-------------|---|----|------------------|
| FA-PRB-01 | Probe verbindet sich ausgehend per gRPC/mTLS mit einem Voter aus `CLUSTER_SEEDS`; Failover über die Voter-Liste mit Backoff + Jitter. | M | P5 | Test: Voter-Ausfall → Probe wechselt < 30 s. |
| FA-PRB-02 | Probe abonniert die ihr zugewiesenen Monitore (Stream) und erhält Änderungen innerhalb von 5 s. | M | P5 | Test. |
| FA-PRB-03 | Heartbeats werden hochgeladen; bei Verbindungsverlust lokal gepuffert (begrenzt, Standard 24 h bzw. 100 MB) und nachgeliefert. | M | P5 | Test: 1 h offline → vollständige Nachlieferung. |
| FA-PRB-04 **[neu]** | gRPC-Keepalive für NAT-Pfade: Client-Ping 30–60 s; Server erlaubt Pings ohne aktive Calls und mit passender Mindestrate; maximale Verbindungsdauer begrenzt. | M | P5 | Langzeittest hinter NAT (24 h). (PF-30) |
| FA-PRB-05 | Private Probes (`owner_tenant_id`) prüfen nur Monitore ihres Mandanten; Registrierung durch `tenant-admin` über Join-Token mit Mandantenbindung. | M | P5 | Test. |
| FA-PRB-06 | Probe-Offline wird als Systemalert (Plattform) bzw. Mandanten-Alert (private Probe) gemeldet. | M | P5 | Test. |
| FA-PRB-07 | Probes sind keine Raft-Mitglieder und beeinflussen das Quorum nicht. | M | P5 | Test. |

### 6.16 REST-API und Live-Feed (FA-API)

| ID | Anforderung | P | Ph | Abnahmekriterium |
|----|-------------|---|----|------------------|
| FA-API-01 | REST-API gemäß Konzept Kap. 14, versioniert unter `/api/v1`, beschrieben in `api/openapi.yaml` (OpenAPI 3.x, Version gemäß ADR 0003) als Vertrag. | M | P0 (Entwurf), P1 | Contract-Tests: Implementierung entspricht Spezifikation. |
| FA-API-02 | Einheitliches Fehlerformat (RFC 9457 Problem Details). | M | P1 | Test. |
| FA-API-03 | Paginierung (Cursor) für Listen; Zeitbereichsabfragen für Heartbeats mit Obergrenze der Ergebnismenge. | M | P1 | Test. |
| FA-API-04 | WebSocket `/api/v1/stream`: Heartbeats, Statuswechsel, Node-Status; Client kann auf Monitore filtern. | M | P1 | Test. |
| FA-API-05 | CORS mit Allow-List der Frontend-Origins (exakte Origin-Reflexion, `Vary: Origin`, nie `*` bei Credentials). | M | P1 | Test. (PF-34) |
| FA-API-06 | Monitor-Typen inkl. JSON-Schema über `GET /api/v1/monitor-types`. | M | P1 | Test. |
| FA-API-07 | Rate-Limiting auf Login- und öffentlichen Endpunkten. | M | P1 | Test. |

### 6.17 Frontend (FA-UI)

| ID | Anforderung | P | Ph | Abnahmekriterium |
|----|-------------|---|----|------------------|
| FA-UI-01 | Angular-SPA, ein Build-Artefakt, eingebettet per `go:embed` in jeden Voter. | M | P0 | `make build` erzeugt Binary mit UI. |
| FA-UI-02 | Laufzeit-Konfiguration über `config.json` (Node-Liste, Auth-Modus, Issuer, Client-ID); eingebettete Variante erzeugt sie dynamisch aus der Node-Konfiguration. | M | P0 (Basis), P5 (extern) | Test. |
| FA-UI-03 | Client-Failover: Health-Check aller Nodes beim Start, Auswahl des schnellsten gesunden, Wechsel bei Fehlern, Nachladen der Node-Liste von `/api/v1/cluster/nodes`. | M | P5 | E2E-Test mit gestopptem Voter. |
| FA-UI-04 | Ansichten Mandant: Dashboard, Monitor-Detail (Konsens + Status/Latenz je Node, Uptime-Balken, Diagramme), Monitor-Editor (dynamisch aus JSON-Schema), Benachrichtigungen, Wartungsfenster, Incidents, Mandanten-Einstellungen, private Probes. | M | P1 (Kern), P4/P5 (Rest) | E2E-Tests. |
| FA-UI-05 | Plattform-Admin `/admin` als lazy-geladener Bereich mit Route-Guard (nur Komfort; Absicherung serverseitig): Cluster, Join-Tokens, Mandanten, Systemalerts (E21). | M | P2 (Cluster), P4 (Mandanten) | E2E-Test. |
| FA-UI-06 | Live-Aktualisierung über WebSocket mit automatischem Reconnect (Backoff + Jitter) und Neusynchronisierung. | M | P1 | Test. |
| FA-UI-07 | Formulare für gängige Shoutrrr-Dienste (E-Mail, Telegram, Discord, Slack, ntfy, Gotify, Teams, Matrix, Pushover), Experten-Eingabe der URL. | S | P1 | UI-Test. |
| FA-UI-08 | Hell-/Dunkelmodus, responsiv bis 360 px Breite. | S | P1 | Visueller Test. |
| FA-UI-09 | Mehrsprachigkeit vorbereitet (i18n), ausgeliefert: Deutsch, Englisch. | S | P1 | – |

### 6.18 Selbstüberwachung und Betrieb (FA-OBS)

| ID | Anforderung | P | Ph | Abnahmekriterium |
|----|-------------|---|----|------------------|
| FA-OBS-01 | Systemalerts an plattformweiten Admin-Kanal: Voter/Probe offline, Quorum verloren/wiederhergestellt, Replikationsrückstand, Uhrzeitabweichung > 2 s, Speicherplatz knapp (< 15 %), WAL-Datei > 512 MB, Raft-Replikationsstand `stale`, Zertifikatsablauf, Inkarnationswechsel, abweichender FSM-Fingerprint. | M | P3 | Test je Alarm. |
| FA-OBS-02 | Prometheus-Metriken unter `/metrics` (optional, abschaltbar, separat absicherbar): Checks/s, Check-Dauer, laufende Checks, Raft-Status, Replikationsrückstand, DB-/WAL-Größe, Outbox-Länge. | S | P3 | Test. |
| FA-OBS-03 | Backup: konsistenter Export (Konfiguration + optional Messdaten) über API/CLI; Restore nur in frischen Cluster (dokumentiert). | S | P5 | Restore-Test. |
| FA-OBS-04 | CLI-Unterbefehle im Binary (z.B. `uptimecluster admin …`) für Recovery-Aufgaben (Peers-Override, Reset Admin-Passwort). | S | P2 | Runbook-Test. |

### 6.19 Statusseiten (FA-STP, Phase 6)

| ID | Anforderung | P | Ph | Abnahmekriterium |
|----|-------------|---|----|------------------|
| FA-STP-01 | Öffentliche Statusseiten pro Mandant mit Auswahl von Monitoren, Gruppen, Incidents, eigener Domain. | K | P6 | – |

---

## 7. Produktdaten

### 7.1 Datenklassen

| Klasse | Inhalt | Mechanismus | Konsistenz | Ohne Quorum |
|--------|--------|-------------|------------|-------------|
| A | Mandanten, Monitore, Kanäle, Nutzer, Wartungsfenster, Nodes, MonitorState, Incidents, Alert-Outbox-Zustand, Schlüssel, Join-Tokens, Request-Dedup | Raft → SQLite | stark | read-only |
| B | Heartbeats, verdichtete Latenzen, Rollups | append-only je Node, asynchron repliziert; Rollups lokal deterministisch | eventual, konfliktfrei | beschreibbar |

### 7.2 Datenmodell (logisch)

Basierend auf Konzept 6.5, mit folgenden **Präzisierungen**:

- `Heartbeat(node_id, incarnation, seq, tenant_id, monitor_id, ts_ms, status, latency_ms, error_class, message, details_json)` – Primärschlüssel `(node_id, incarnation, seq)`; Indizes `(monitor_id, ts_ms)` und `(ts_ms)`.
- `Node(id, name, role, incarnation, region, labels, capabilities_json, owner_tenant_id?, advertise_addr, version, last_seen_ms)`.
- `RequestDedup(request_id, applied_index, result_json)` – Teil der FSM.
- `AlertOutbox(id, tenant_id, alert_key, channel_id, payload, attempts, next_attempt_ms, state)`.
- `SigningKey(kid, alg, public_key, private_key_enc, created_ms, retired_ms?)`.
- `SecretKey(key_id, wrapped_dek, created_ms, retired_ms?)` – Datenschlüssel, gewrappt mit dem Master-Key (FA-SEC-SEC).
- `LatencyMinute(monitor_id, node_id, minute_ms, min, avg, max, p95, ok_count, err_count)` – Ergebnis der Verdichtung.
- Zeitstempel überall `INTEGER` UTC-Millisekunden; Booleans `INTEGER 0/1`; keine engine-spezifischen Typen in gemeinsamen Pfaden (Portabilität SQLite/PostgreSQL).

### 7.3 Aufbewahrung

| Daten | Aufbewahrung | Konfigurierbar |
|-------|--------------|----------------|
| Rohe Heartbeats | 48 h voll, danach verdichtet bis 30 d | ja |
| Statuswechsel | 30 d exakt (danach in Incidents/Rollups enthalten) | ja |
| Stunden-Rollups | 1 Jahr | ja |
| Tages-Rollups | unbegrenzt | ja |
| Incidents | unbegrenzt | ja |
| Audit-Log | 1 Jahr | ja |

### 7.4 Audit-Log

| ID | Anforderung | P | Ph |
|----|-------------|---|----|
| FA-DAT-01 | Alle schreibenden Aktionen (wer, wann, Mandant, Objekt, Aktion, ohne Geheimnisse) werden im Audit-Log (Datenklasse A) erfasst und für `tenant-admin`/`platform-admin` einsehbar. | S | P4 |

---

## 8. Nichtfunktionale Anforderungen

### 8.1 Leistung (Performance Efficiency)

| ID | Anforderung | P | Ph |
|----|-------------|---|----|
| NFA-PERF-01 | 1.000 Monitore à 60 s (bzw. 300 à 20 s) auf einem Voter mit 2 vCPU / 1 GB RAM bei < 50 % CPU im Mittel. | M | P1 |
| NFA-PERF-02 | RAM-Verbrauch eines Voters bei Nennlast < 512 MB; einer Probe mit 100 Monitoren < 64 MB. | M | P1/P5 |
| NFA-PERF-03 | API p95 < 200 ms für Monitorliste (1.000 Einträge inkl. Status) und Uptime-Abfragen bis 365 d (aus Rollups). | M | P1 |
| NFA-PERF-04 | Statuswechsel erscheint in der UI ≤ 2 s nach Commit. | M | P1 |
| NFA-PERF-05 | Replikationsrückstand im Normalbetrieb < 5 s. | M | P2 |
| NFA-PERF-06 | Start eines Voters (bis Ready) < 10 s bei 1.000 Monitoren und 30 d Daten. | S | P2 |
| NFA-PERF-07 | Container-Image < 40 MB, statisches Binary ohne CGO. | M | P0 |

### 8.2 Zuverlässigkeit und Verfügbarkeit

| ID | Anforderung | P | Ph |
|----|-------------|---|----|
| NFA-REL-01 | Bei n Votern dürfen ⌊(n−1)/2⌋ ausfallen ohne Verlust von Checks, Alerts, Lese- oder Schreibzugriff. | M | P2 |
| NFA-REL-02 | Neuer Leader nach Leader-Ausfall < 10 s (bei Latenz ≤ 100 ms zwischen Votern). | M | P2 |
| NFA-REL-03 | Alarmierungslatenz: DOWN-Alert spätestens `(retries + 1) × Intervall + 30 s` nach tatsächlichem Ausfall. | M | P3 |
| NFA-REL-04 | Kein Verlust bestätigter Konfigurationsänderungen (Raft-Commit) bei Ausfall einer Minderheit. | M | P2 |
| NFA-REL-05 | Kein Verlust von Heartbeats, die mindestens ein überlebender Voter empfangen hat. | M | P2 |
| NFA-REL-06 | Quorumverlust: weiter messen, lesen, unbestätigt alarmieren; automatische Konvergenz nach Rückkehr ohne manuellen Eingriff. | M | P3 |
| NFA-REL-07 | Absturz (kill -9) eines Nodes führt nie zu einer beschädigten Datenbank. | M | P1 |

### 8.3 Sicherheit

| ID | Anforderung | P | Ph |
|----|-------------|---|----|
| NFA-SEC-01 | mTLS für sämtliche Inter-Node-Kommunikation (FA-PKI); TLS ≥ 1.2, bevorzugt 1.3. | M | P2 |
| NFA-SEC-02 | Strikte Mandantentrennung (FA-TEN-01/02), abgesichert durch automatisierte Isolationstests in CI. | M | P1 |
| **FA-SEC-SSRF** **[präzisiert]** | SSRF-Schutz für Checks, DNS-Resolver und Benachrichtigungsziele: Prüfung der **tatsächlich verbundenen IP zum Dial-Zeitpunkt** (`net.Dialer.Control`), damit DNS-Rebinding und Redirects abgedeckt sind. Gesperrte Bereiche (konfigurierbar, im Mehrmandantenbetrieb auf öffentlichen Nodes Standard an): Loopback, RFC 1918, Link-Local inkl. Cloud-Metadaten (169.254.169.254), CGNAT 100.64/10, ULA fc00::/7, Unspecified, Multicast, Broadcast, sowie IPv6-Einbettungen von IPv4 (`::ffff:0:0/96`, `64:ff9b::/96`, `2002::/16`, `::/96`). Nicht-kanonische IP-Schreibweisen (oktal, hex, dezimal) werden abgelehnt. Private Probes eines Mandanten sind ausgenommen. | M | P4 (Durchsetzung), P1 (Hook) |
| **FA-SEC-SEC** **[präzisiert]** | Geheimnisse (Kanal-URLs, Webhook-Header, Signaturschlüssel) werden mit **Envelope Encryption** gespeichert: AES-256-GCM mit zufälligem Nonce, Datenschlüssel (DEK) je Rotationsepoche, gewrappt mit einem Master-Key (KEK), der **nicht** in der Datenbank liegt (Env/Datei/KMS). Jede Chiffre trägt `key_id`; Kontext (Tabelle + Objekt-ID) als Additional Data. Rotation über Raft mit Hintergrund-Neuverschlüsselung. | M | P1 |
| NFA-SEC-03 | Passwörter: Argon2id gemäß OWASP; kein Passwort im Klartext in Logs, API-Antworten oder Raft-Log. | M | P1 |
| NFA-SEC-04 | Tokens in der SPA **nur im Speicher**, nicht in `localStorage`; Refresh-Mechanismus gemäß ADR. | M | P1 |
| NFA-SEC-05 | Security-Header: CSP ohne `unsafe-eval`, `X-Content-Type-Options`, `Referrer-Policy`, `frame-ancestors 'none'`. | M | P1 |
| NFA-SEC-06 | Abhängigkeiten: `govulncheck` und `npm audit` in CI; kritische Funde blockieren Releases. | M | P0 |
| NFA-SEC-07 | Container läuft als `nonroot`, Read-only-Root-FS möglich, nur `NET_RAW` als zusätzliche Capability (optional). | M | P1 |
| NFA-SEC-08 | Threat Model (STRIDE) je Phase mit sicherheitsrelevanten Änderungen, abgelegt unter `docs/security/`. | S | P2 |

### 8.4 Wartbarkeit

| ID | Anforderung | P | Ph |
|----|-------------|---|----|
| NFA-MNT-01 | Testabdeckung Go ≥ 70 % gesamt, ≥ 85 % für `aggregator`, `cluster`, `auth`, `store`; Frontend ≥ 60 %. | M | P1 |
| NFA-MNT-02 | `go vet`, `gofmt`, `golangci-lint`, `eslint`, `prettier` in CI ohne Befunde. | M | P0 |
| NFA-MNT-03 | API-Vertrag (OpenAPI) und Protobuf sind die einzige Quelle für Schnittstellentypen; generierter Code wird in CI auf Aktualität geprüft. | M | P1 |
| NFA-MNT-04 | Schemaänderungen nur über versionierte Migrationen, Expand/Contract für Rolling Updates (keine Umbenennung/Löschung im selben Release). | M | P1 |
| NFA-MNT-05 | Architekturentscheidungen als ADR dokumentiert (siehe CLAUDE.md). | M | P0 |

### 8.5 Portabilität und Kompatibilität

| ID | Anforderung | P | Ph |
|----|-------------|---|----|
| NFA-PORT-01 | Multi-Arch-Image (amd64, arm64) in GHCR. | M | P0 |
| NFA-PORT-02 | Lauffähig unter Docker, Podman (rootless), Kubernetes; Compose-Beispiele für Single, Cluster, Probe. | M | P1 (Single), P2/P5 |
| NFA-PORT-03 | Storage-Backend über Repository-Schnittstelle austauschbar (`embedded`, `postgres`); gemeinsame Integrations-Testsuite für beide. | M | P1 (Schnittstelle), P5 (Postgres) |
| NFA-PORT-04 | Upgrade-Pfad: jede Minor-Version ist von der vorherigen Minor-Version per Rolling Update erreichbar. | M | P2 |

### 8.6 Benutzbarkeit und Barrierefreiheit

| ID | Anforderung | P | Ph |
|----|-------------|---|----|
| NFA-USE-01 | Single-Node-Inbetriebnahme mit einer Compose-Datei in < 5 min. | M | P1 |
| NFA-USE-02 | Barrierefreiheit nach WCAG 2.2 AA für Mandanten-Ansichten (Status nie nur über Farbe). | S | P1 |
| NFA-USE-03 | Verständliche Fehlermeldungen mit Handlungsempfehlung (z.B. ICMP-Modus, Zitadel-Token-Typ). | M | P1 |

---

## 9. Schnittstellen

### 9.1 REST-API

Vertrag: `api/openapi.yaml` (OpenAPI 3.x, siehe ADR 0003). Endpunkte gemäß Konzept Kap. 14, ergänzt um:

```
GET    /healthz | /readyz                     # Liveness / Readiness
POST   /api/v1/stream/ticket                  # kurzlebiges WebSocket-Ticket (FA-AUTH-11)
GET    /api/v1/incidents/{id}
GET    /api/v1/audit                          # P4
CRUD   /api/v1/api-tokens                     # P4
POST   /api/v1/admin/tenants/{id}/suspend     # P4
```

### 9.2 WebSocket

`WS /api/v1/stream?ticket=…` – Nachrichten als JSON mit `type` (`heartbeat`, `state`, `node`, `incident`) und `tenant_id`-gefiltertem Inhalt. Ticket einmalig, ≤ 30 s gültig.

### 9.3 Inter-Node (gRPC, Port 9443)

Protobuf-Definitionen in `proto/`:
- `ClusterService` – Join, Leave, Raft-Transport (oder Raft über eigenen Stream, gemäß ADR), Write-Forwarding.
- `ReplicationService` – `StreamHeartbeats`, `GetWatermarks`, `FetchRange`.
- `ProbeService` – `Subscribe` (Monitor-Zuweisungen), `UploadHeartbeats`.

### 9.4 Benachrichtigungen

- Shoutrrr-URLs (Bibliothek `nicholas-fedor/shoutrrr`, Version gepinnt).
- Generischer Webhook: HTTP POST, JSON aus Template, frei definierbare Header (verschlüsselt), Timeout 10 s.

### 9.5 Konfiguration (Umgebungsvariablen, Auszug)

| Variable | Default | Bedeutung |
|----------|---------|-----------|
| `NODE_NAME` | Hostname | Anzeigename |
| `NODE_ROLE` | `voter` | `voter` \| `probe` |
| `DATA_DIR` | `/data` | Datenverzeichnis |
| `HTTP_ADDR` | `:8080` | API/UI |
| `CLUSTER_ADDR` | `:9443` | Cluster-Port |
| `PUBLIC_URL` | – | Öffentliche URL des Voters |
| `CLUSTER_ADVERTISE` | `<NODE_NAME>:<Cluster-Port>` | Stabile Cluster-Adresse (DNS:Port) |
| `RAFT_TIMEOUT` | `1s` | Raft-Heartbeat- und Election-Timeout (ADR 0012 A6) |
| `CLUSTER_JOIN` / `CLUSTER_SEEDS` | – | Beitritt (Voter) / Seeds (Probe) |
| `CLUSTER_JOIN_TOKEN` | – | Join-Token |
| `AUTH_MODE` | `local` | `local` \| `zitadel` |
| `ZITADEL_ISSUER` / `ZITADEL_AUDIENCE` | – | OIDC |
| `STORAGE_BACKEND` | `embedded` | `embedded` \| `postgres` |
| `DATABASE_URL` | – | PostgreSQL |
| `MASTER_KEY_FILE` / `MASTER_KEY` | – | KEK für Geheimnisse (Pflicht ab P1; beim ersten Start generierbar) |
| `RAW_COMPACTION` | `true` | Verdichtung |
| `ICMP_MODE` | `auto` | `auto` \| `privileged` \| `unprivileged` \| `off` |
| `SSRF_PROTECTION` | `auto` | `off` \| `on` \| `auto` (an im Zitadel-Modus) |
| `CORS_ORIGINS` | – | Zusätzliche erlaubte Frontend-Origins |
| `LOG_LEVEL` / `LOG_FORMAT` | `info` / `json` | Logging |

---

## 10. Benutzungsoberfläche

- **Navigation:** Dashboard · Monitore · Incidents · Wartung · Benachrichtigungen · Einstellungen; `/admin` nur für `platform-admin`.
- **Dashboard:** Liste/Kacheln mit Konsens-Status, Uptime 24 h, Antwortzeit, Status je Node als kompakte Punkte; Filter nach Tags/Status; Suche.
- **Monitor-Detail:** Status-Historie (Balken), Latenz-Diagramm je Node (ECharts), Uptime-Kennzahlen, Incidents, letzte Fehler mit Fehlerklasse.
- **Monitor-Editor:** generiert aus JSON-Schema, mit Vorschau „jetzt testen“ (einmaliger Check auf ausgewähltem Node).
- **Status-Darstellung:** UP, DOWN, DEGRADED, PARTIAL, PENDING, MAINTENANCE, PAUSED, UNKNOWN – jeweils Farbe **und** Symbol/Text.
- **Cluster-Ansicht (Admin):** Nodes mit Rolle, Leader, Quorum, Version, Rückstand, Uhrzeitabweichung, Capabilities; Join-Token erzeugen; Node entfernen mit Quorum-Warnung.

---

## 11. Rahmenbedingungen

### 11.1 Lizenz und Recht

| ID | Anforderung | P | Ph |
|----|-------------|---|----|
| RB-LIC-01 | Lizenz AGPLv3 + kommerzielle Lizenz (E18). SPDX-Header in jeder Quelldatei (Variante `AGPL-3.0-only` oder `-or-later` gemäß ADR). | M | P0 |
| RB-LIC-02 | Externe Beiträge nur mit **CLA** (nicht DCO – DCO erlaubt keine Relizenzierung); CLA Assistant o.ä. in GitHub; Text anwaltlich prüfen lassen. | M | vor erstem externen Beitrag |
| RB-LIC-03 | Lizenzprüfung aller Go- und npm-Abhängigkeiten in CI gegen Allow-List (MIT, BSD-2/3, Apache-2.0, ISC, MPL-2.0); andere Lizenzen nur nach ADR. | M | P0 |
| RB-LIC-04 | AGPL §13: Die UI enthält einen Link auf den Quellcode der laufenden Version. | M | P1 |
| RB-LIC-05 | `THIRD_PARTY_NOTICES` wird beim Build erzeugt und ausgeliefert. | M | P1 |

### 11.2 Datenschutz (DSGVO)

- Personenbezogene Daten: E-Mail/Name von Nutzern, IP-Adressen in Logs/Audit. Minimierung, dokumentierte Aufbewahrung, Löschung bei Mandantenlöschung.
- Überwachte Ziele und Antworten können personenbezogene Daten enthalten → Antwortkörper werden nicht gespeichert, nur Ergebnis und gekürzte Fehlermeldung (≤ 1 KB).
- Für den kommerziellen Betrieb: AVV-Vorlage und TOMs (außerhalb des Software-Umfangs).

### 11.3 Technische Vorgaben

Go (aktuelle stabile Version, derzeit 1.27), Angular 22 (aktuelle Major), hashicorp/raft, SQLite (pure Go), gRPC, Shoutrrr, Zitadel; Details und Alternativen in ADRs.

---

## 12. Risiken und Pitfalls

Ergebnis der Recherche (Okt. 2026). Jede Zeile nennt Problem, Bedeutung für UptimeCluster und die daraus abgeleitete Anforderung. Quellen in Kap. 16.

### 12.1 Cluster und Raft

| ID | Pitfall | Auswirkung | Maßnahme |
|----|---------|------------|----------|
| PF-01 | `BootstrapCluster` darf nur „am Anfang der Zeit“ aufgerufen werden; erneutes Bootstrappen nach Restore/Neustart führt zu Fehlern oder getrennten Clustern. | Zwei Cluster mit gleichem Namen, Datenverlust. | FA-CLU-02 |
| PF-02 | Mitgliedschaftsänderungen: bekannter Sicherheitsfehler bei parallelen Single-Server-Änderungen über Term-Grenzen; `AddVoter` nur auf dem Leader. | Nicht überlappende Quoren → Split-Brain. | FA-CLU-03, FA-CLU-04 (serialisiert, erst Nicht-Voter, dann Promotion) |
| PF-03 | Raft speichert Adressen; wechselnde Container-IPs führen zu verwaisten Peers und Quorum-Verlust (bekannte Consul-Probleme). | Cluster zerfällt nach Neustarts. | FA-NODE-03, FA-CLU-10 (stabile IDs, DNS-Adressen) |
| PF-04 | `Apply`-Timeout begrenzt nur die Wartezeit; bei `ErrLeadershipLost` ist das Ergebnis unklar. | Doppelte Objekte bei Client-Retry. | FA-CLU-06 (Request-ID + Dedup in FSM) |
| PF-05 | FSM muss deterministisch sein. | Divergierende Datenbanken je Voter. | FA-CLU-07 + Determinismus-Test |
| PF-06 | Automatisches Entfernen „toter“ Voter verschiebt Quoren; Recovery nach Quorumverlust erfordert manuellen Peers-Override und kann Daten verlieren. | Datenverlust, Split-Brain. | FA-CLU-08, FA-OBS-04 (Runbook + CLI) |
| PF-07 | Node-ID aus IP/Hostname abgeleitet. | Identitätskonflikte nach Umzug. | FA-NODE-03 |
| PF-08 | Restore großer Snapshots dauert; überholt der Leader in der Zeit `TrailingLogs`, gerät der Follower in eine Snapshot-Schleife. Snapshot durch Dateikopie einer laufenden SQLite-DB ist inkonsistent. | Follower holt nie auf. | FA-CLU-09 (nur Klasse A im Snapshot, Backup-API, `TrailingLogs` bemessen) |
| PF-09 | Gemischte Versionen während Rolling Updates (neue Raft-Befehle, die alte Nodes nicht kennen). | FSM-Fehler auf alten Nodes. | FA-CLU-12 (Feature-Gates), NFA-MNT-04 |
| PF-10 | Join ohne CA-Pinning = Trust-on-first-use; abgefangene Tokens. | Fremder Node im Cluster. | FA-PKI-02 (CA-Fingerprint im Token, einmalig, TTL, gehasht) |
| PF-11 | Zertifikatsrotation wirkt nur bei neuen Handshakes; Clock-Skew macht gültige Zertifikate „noch nicht gültig“. | Verbindungsabbrüche, Ausfall nach Ablauf. | FA-PKI-03 (Callbacks, Max-Connection-Age, Backdating), FA-NODE-06 |

### 12.2 Replikation und Zeit

| ID | Pitfall | Auswirkung | Maßnahme |
|----|---------|------------|----------|
| PF-12 | Sequenznummern beginnen nach Datenverlust/Restore wieder bei 1; Peers verwerfen neue Daten als Duplikate. | Stiller Datenverlust. | FA-REP-01 (Inkarnation im Schlüssel), FA-REP-06 |
| PF-21 | Wanduhren verschiedener Nodes weichen ab und springen bei NTP-Korrekturen. | Falsche Uptime, falsche Reihenfolge. | FA-UPT-02 (Ordnung je Node über seq; UTC-ms), FA-OBS-01 (Skew-Alert) |

### 12.3 Checks

| ID | Pitfall | Auswirkung | Maßnahme |
|----|---------|------------|----------|
| PF-13 | Keep-Alive/Connection-Reuse verdeckt Ausfälle (bestehende Verbindung antwortet, neue schlagen fehl); HTTP/2 hält tote Verbindungen; `HTTP_PROXY` leitet Checks unbemerkt um. | Falsch-positive UP-Meldungen. | FA-CHK-11, FA-CHK-13 |
| PF-14 | Ein globales `Client.Timeout` deckt Phasen nicht differenziert ab; ohne Fehlerklasse ist unklar, ob Ziel oder Prüfer gestört ist (häufige Uptime-Kuma-Beschwerde „timeout exceeded“ bei erreichbarer Seite). | Nicht diagnostizierbare Fehlalarme. | FA-CHK-03, FA-CHK-12 |
| PF-15 | Thundering Herd: alle Monitore starten gleichzeitig; Timeout ≥ Intervall erzeugt überlappende Checks. | Lastspitzen, Selbst-DoS, Fehlalarme. | FA-SCH-03, FA-SCH-04, FA-MON-03 |
| PF-16 | Goroutine-Leaks bei Checks ohne Kontext-Abbruch. | Speicherwachstum über Tage. | FA-CHK-02 |
| PF-17 | ICMP in Containern: unprivilegierter Modus braucht `net.ipv4.ping_group_range`, privilegierter Raw-Sockets/`NET_RAW`; `nonroot` + `NET_RAW` reicht ohne effektive Capability nicht; Gatus hatte genau diesen Fehler. | ICMP-Monitore immer DOWN. | FA-CHK-31, FA-CHK-32, FA-NODE-06 |
| PF-18 | DNS: abgeschnittene UDP-Antworten (TC-Bit), TCP/53 blockiert, Reihenfolge von Mehrfachantworten variiert. | Falsche DNS-Fehler. | FA-CHK-41 |
| PF-19 | Eigener Netzausfall eines Nodes sieht aus wie Ausfall aller Ziele. | Massen-Fehlalarm (Uptime-Kuma-Issue „false DOWN on local connection loss“). | FA-CON-04 |
| PF-20 | Ohne Bestätigungslogik erzeugen einzelne Paketverluste Alarme. | Alarmmüdigkeit. | FA-CON-03, Mehrheitsregel |

### 12.4 Storage (SQLite)

| ID | Pitfall | Auswirkung | Maßnahme |
|----|---------|------------|----------|
| PF-22 | `SQLITE_BUSY` bei mehreren schreibenden Verbindungen aus dem `database/sql`-Pool; Pragmas im DSN werden bei Tippfehlern stillschweigend ignoriert (dann kein WAL, `busy_timeout=0`). | Sporadische Schreibfehler unter Last (auch Uptime Kuma 2.x betroffen). | ADR Storage: **eine** Schreibverbindung (`_txlock=immediate`) + separater Lese-Pool; Batch-Inserts; FA-NODE-06 (Pragmas beim Start verifizieren) |
| PF-23 | Gelöschte Daten verkleinern die Datei nicht (`auto_vacuum=NONE`); `auto_vacuum` lässt sich nur vor Anlage der Tabellen setzen. Uptime Kuma: Datenbanken mit vielen GB, langsame Dashboards ab ~500 Monitoren durch Full-Scans. | Volllaufende Platten, langsame UI. | Erste Migration setzt `auto_vacuum=INCREMENTAL`; FA-UPT-06; Rollups statt Rohdaten-Scans (NFA-PERF-03) |
| PF-24 | WAL wächst unbegrenzt (Checkpoint-Starvation) bei lange offenen Lesetransaktionen; große `DELETE`s halten den Schreiber lange. | Platten- und Latenzprobleme. | Kurze Lesetransaktionen (keine offenen Cursor über Streams), Lese-Pool von `metrics.db` auf 4 Verbindungen begrenzt, `journal_size_limit`, `wal_checkpoint(TRUNCATE)` alle 60 s, WAL-Größe als Metrik mit Systemalert ab 512 MB; FA-UPT-06. Im Spike bestätigt: WAL wuchs unter Dauerlast auf 1,3–4,1 GB (ADR 0013 C4) |
| PF-35 | `sqlc` erzeugt je Engine eigene Pakete und Typen; Queries müssen für SQLite und PostgreSQL doppelt gepflegt werden. | Doppelter Aufwand, semantische Abweichungen. | ADR Storage: portables SQL (Integer-Zeitstempel, 0/1-Booleans), Repository-Interface, **gemeinsame Integrations-Testsuite** für beide Backends (NFA-PORT-03); PostgreSQL erst P5 |
| PF-36 | `synchronous=NORMAL` im WAL-Modus kann bei Stromausfall die letzten Transaktionen verlieren. | Verlust jüngster Heartbeats. | Akzeptabel für Klasse B (Replikate auf anderen Votern); Raft-Log selbst fsynct. |

### 12.5 Benachrichtigungen

| ID | Pitfall | Auswirkung | Maßnahme |
|----|---------|------------|----------|
| PF-25 | Alert pro fehlgeschlagenem Check → Alarmflut. | Kanäle unbrauchbar. | FA-ALR-02, FA-ALR-06 |
| PF-26 | Rate-Limits (Telegram ~1 Nachricht/s pro Chat, 20/min in Gruppen; Discord: 10.000 ungültige Requests in 10 min → Cloudflare-Sperre der IP). | Gesperrte Plattform-IP trifft alle Mandanten. | FA-ALR-04, FA-ALR-05 |
| PF-27 | `text/template` escaped nichts; `html/template` escaped nicht für JSON. | Kaputtes oder manipuliertes Webhook-JSON. | FA-ALR-07 |
| PF-28 | „Kleinste ID alarmiert“ gilt je Minderheitspartition → Duplikate bei mehreren Partitionen; die Minderheit hat oft selbst das Netzproblem. | Doppelte/irreführende Alerts. | FA-ALR-10, FA-ALR-11 |
| PF-29 | Benachrichtigungs-URLs sind nutzerdefiniert → SSRF über Webhooks. | Zugriff auf interne Dienste. | FA-ALR-12, FA-SEC-SSRF |
| PF-37 | Shoutrrr-Fork `nicholas-fedor/shoutrrr` (MIT) ist gepflegt, aber Ein-Personen-Projekt. | Ausfall der Wartung. | Version pinnen, Abhängigkeit hinter eigener Schnittstelle kapseln, Webhook als Rückfallebene. |

### 12.6 Sicherheit, Auth, Frontend

| ID | Pitfall | Auswirkung | Maßnahme |
|----|---------|------------|----------|
| PF-29a | SSRF-Prüfung nur beim Parsen (TOCTOU/DNS-Rebinding), IPv4-in-IPv6-Einbettungen und nicht-kanonische IP-Schreibweisen umgehen Filter (mehrere CVEs 2025/26 in bekannten Projekten). | Zugriff auf Metadaten-Endpunkte/interne Netze. | FA-SEC-SSRF (Dial-Zeit-Prüfung, vollständige Bereichsliste, Fuzz-Tests) |
| PF-31 | Zitadel liefert standardmäßig **opake** Access-Tokens; nur JWT lassen sich lokal validieren. Clients können beliebige Audience-Scopes anfordern → Audience allein ist keine Autorisierung. | Login funktioniert nicht bzw. Rechteausweitung. | FA-AUTH-05, FA-AUTH-06, Doku zur Zitadel-Einrichtung |
| PF-32 | Browser können bei WebSockets keinen `Authorization`-Header setzen; Tokens in der URL landen in Proxy-Logs. | Token-Leck. | FA-AUTH-11 |
| PF-33 | Login-Drosselung nur pro IP ist mit verteilten Angriffen umgehbar. | Credential Stuffing. | FA-AUTH-04 |
| PF-34 | Cookies sind an einen Host gebunden → Sitzungen überleben Client-Failover zwischen Votern nicht; `localStorage`-Tokens sind XSS-anfällig. | Failover erzwingt Neu-Login oder unsichere Speicherung. | NFA-SEC-04 (Bearer im Speicher), ADR Auth (Refresh-Strategie), FA-API-05 |
| PF-38 | `go:embed` schlägt fehl, wenn das Verzeichnis leer ist bzw. nur Punkt-Dateien enthält; Build-Reihenfolge Frontend → Backend. | Kaputte CI-Builds. | Platzhalter-`index.html` im Repo, `make build` erzwingt Reihenfolge. |
| PF-39 | Falsche Cache-Header: gecachte `index.html` lädt nach Update veraltete Bundles. | UI-Fehler nach Updates. | `index.html` `no-cache`, gehashte Assets `immutable`; SPA-Fallback nicht für `/api/*`. |
| PF-40 | Zoneless Angular: Callbacks außerhalb von Angular (WebSocket) aktualisieren die Ansicht nur über Signals. | „Hängende“ Live-Ansicht. | Live-Daten ausschließlich über Signals. |

### 12.7 Lizenz und Projekt

| ID | Pitfall | Auswirkung | Maßnahme |
|----|---------|------------|----------|
| PF-41 | DCO reicht für Dual Licensing nicht; ohne CLA sind fremde Beiträge nicht relizenzierbar. | Kommerzielle Lizenz rechtlich angreifbar. | RB-LIC-02 |
| PF-42 | Inkompatible Lizenzen transitiver Abhängigkeiten. | Rechtsrisiko für beide Lizenzen. | RB-LIC-03 |
| PF-43 | Uptime Kuma fehlen Multi-Location-Checks, HA, API, Rollenmodell, PostgreSQL – die meistgewünschten Features. | Chance zur Differenzierung, aber auch Erwartungshaltung. | Bereits durch Z-M-02/03/06/09, FA-API, Z-S-01 abgedeckt. |

### 12.8 Projektrisiken

| ID | Risiko | W'keit | Auswirkung | Maßnahme |
|----|--------|--------|------------|----------|
| R-01 | Komplexität des Cluster-Layers übersteigt Aufwandsschätzung | hoch | hoch | Raft-Spike in P0, Single-Node zuerst (P1), Chaos-Tests früh |
| R-02 | Einarbeitung in Go | mittel | mittel | Einstieg über HTTP-Checker (Konzept 18.3), Reviews |
| R-03 | Doppelte Storage-Backends binden Kapazität | mittel | mittel | PostgreSQL erst P5, gemeinsame Testsuite |
| R-04 | Zitadel-Änderungen (Claims, Token-Typ) | niedrig | mittel | Kapselung in `auth`, Integrationstest gegen Zitadel-Container |
| R-05 | Projektname/Marke kollidiert | mittel | mittel | Namensprüfung vor erstem öffentlichen Release |

---

## 13. Qualitätssicherung und Abnahme

### 13.1 Teststrategie

| Ebene | Inhalt | Werkzeug |
|-------|--------|----------|
| Unit | Checker, Konsens-Regeln, Uptime-Berechnung, Scheduler-Jitter, Templates, SSRF-Filter (inkl. Fuzzing) | `go test`, `testing/fuzz`, Vitest |
| Integration | Repository gegen SQLite (und PostgreSQL ab P5) mit gemeinsamer Suite; Mandanten-Isolation; API-Contract gegen OpenAPI | `go test`, Testcontainers |
| Determinismus | Raft-Log-Replay auf zwei FSMs → identischer Dump; Rollups auf zwei Votern identisch | `go test` |
| Cluster/Chaos | 3/5 Voter + Probes in Docker: Node-Kill, Partition (2\|1), Latenz, Clock-Skew, Datenverzeichnis löschen, Rolling Update | Docker Compose + Skripte (ggf. toxiproxy) |
| E2E | Kernabläufe in der SPA, Client-Failover | Playwright |
| Last | 1.000 Monitore, 20-s-Intervalle, Retention-Lauf | eigener Last-Generator + Ziel-Mock |
| Sicherheit | `govulncheck`, `npm audit`, Lizenzprüfung, Berechtigungsmatrix-Test | CI |

### 13.2 Abnahmeszenarien (Auszug)

| ID | Szenario | Erwartung |
|----|----------|-----------|
| AS-01 | Single-Node per Compose starten, Admin einrichten, HTTP-Monitor anlegen, Ziel abschalten | DOWN-Alert über ntfy innerhalb `(retries+1) × Intervall + 30 s`, Incident angelegt, Uptime < 100 % |
| AS-02 | Zwei weitere Voter beitreten lassen | Cluster mit 3 Votern, keine Unterbrechung, alle Voter zeigen identische Daten |
| AS-03 | Leader hart beenden | neuer Leader < 10 s, kein doppelter oder verlorener Alert |
| AS-04 | Partition 2\|1, Ziel fällt aus | Mehrheit alarmiert bestätigt, Minderheit „unbestätigt“; nach Heilung Korrektur/Bestätigung und identische Daten |
| AS-05 | Node mit blockiertem Egress | kein DOWN-Alarm für Ziele, Systemalert „Node unhealthy“ |
| AS-06 | Zwei Mandanten über Zitadel, Mandant A versucht Monitor von B zu lesen/ändern (ID-Manipulation) | 404, kein Datenleck in API und WebSocket |
| AS-07 | Mandant legt HTTP-Monitor auf `http://169.254.169.254/` bzw. per DNS-Rebinding auf 127.0.0.1 an | abgelehnt bzw. DOWN mit Fehlerklasse `ssrf_blocked` |
| AS-08 | Probe hinter NAT 24 h, 1 h Verbindungsverlust | alle Heartbeats nachgeliefert, keine Duplikate |
| AS-09 | Rolling Update aller Voter auf neue Minor-Version | keine Unterbrechung von Checks und Alerts |
| AS-10 | 30 Tage Daten bei 1.000 Monitoren, Retention-Lauf | Speicher ≤ 4 GB je Voter (Verdichtung an), API p95 < 200 ms |

### 13.3 Definition of Done (je Arbeitspaket)

- Anforderungen des Pakets erfüllt, Abnahmekriterien als automatisierte Tests vorhanden (wo möglich).
- Lint und Tests grün, Abdeckungsziele nicht unterschritten.
- OpenAPI/Proto aktualisiert, Doku (README/Betrieb) aktualisiert.
- Roadmap-Status aktualisiert, Commit(s) nach CLAUDE.md.
- Bei großen Änderungen: ADR im Status Accepted.

---

## 14. Offene Punkte

Die folgenden Punkte werden über ADRs entschieden (siehe [docs/adr/](adr/)):

| # | Frage | Ziel-ADR |
|---|-------|----------|
| O-01 | Raft-Log-Store: `raft-boltdb` (ausgereift) vs. `raft-wal` (experimentell) | ADR Konsens |
| O-02 | Raft-Transport: eigener TCP-Transport mit mTLS vs. Raft über gRPC-Stream (ein Port) | ADR Inter-Node |
| O-03 | Codegenerierung API: `oapi-codegen` (chi, strict-server) vs. `ogen`; Angular-Client-Generator | ADR API-Vertrag |
| O-04 | Refresh-Strategie lokaler Tokens bei Client-Failover (Cookie pro Node vs. Refresh-Token im Speicher + erneuter Login bei Reload vs. gemeinsame Parent-Domain) | ADR Auth lokal |
| O-05 | Verteilung des Master-Keys (KEK) an alle Voter | ADR Geheimnisse (P1) |
| O-06 | SPDX-Variante `AGPL-3.0-only` vs. `-or-later` | ADR Lizenz |
| O-07 | SQLite-Treiber: `modernc.org/sqlite` vs. `ncruces/go-sqlite3` (beide CGO-frei); eigener Benchmark | ADR Storage |
| O-08 | Finaler Projektname | vor erstem öffentlichen Release |

---

## 15. Glossar

Begriffe Node, Voter, Probe, Leader, Mandant, Monitor, Heartbeat, Konsens-Status: siehe Konzept Kap. 2. Ergänzend:

| Begriff | Bedeutung |
|---------|-----------|
| **Inkarnation** | Zufällige ID eines Node-Lebenszyklus; ändert sich, wenn das Datenverzeichnis neu angelegt wird. |
| **Hochwassermarke** | Höchste lückenlos empfangene `seq` je (Node, Inkarnation). |
| **Anti-Entropie** | Periodischer Abgleich zum Schließen von Replikationslücken. |
| **Quorum** | Mehrheit der konfigurierten Voter (⌊n/2⌋ + 1). |
| **Degraded-Modus** | Betrieb ohne Quorum: messen, lesen, unbestätigt alarmieren. |
| **Capability** | Fähigkeit eines Nodes (z.B. `icmp`, `ipv6`), Grundlage der Zuweisung. |
| **Outbox** | Persistente Warteschlange ausstehender Benachrichtigungen. |
| **KEK / DEK** | Key-Encryption-Key (Master-Key) / Data-Encryption-Key. |
| **SSRF** | Server-Side Request Forgery: Missbrauch des Servers für Anfragen an interne Ziele. |

---

## 16. Quellen

Recherche Oktober 2026 (Auswahl; Details in den jeweiligen ADRs).

**Raft / Cluster**
- hashicorp/raft (MPL-2.0, v1.8.0): https://github.com/hashicorp/raft, https://pkg.go.dev/github.com/hashicorp/raft
- raft-wal (experimentell): https://github.com/hashicorp/raft-wal
- Membership-Change-Bug (raft-dev, 2015): https://groups.google.com/g/raft-dev/c/t4xj6dJTP6E
- Nomad/Vault Outage-Recovery: https://developer.hashicorp.com/nomad/tutorials/archive/outage-recovery, https://developer.hashicorp.com/vault/tutorials/raft/raft-lost-quorum
- Consul IP-Wechsel: https://discuss.hashicorp.com/t/understanding-node-ip-changes-for-a-raft-cluster/56854
- rqlite 8.0 / 9.2 (Snapshots, Restarts): https://philipotoole.com/rqlite-8-0-large-dataset-support-and-core-feature-upgrades/, https://rqlite.io/docs/guides/backup/
- gRPC Keepalive: https://grpc.io/docs/guides/keepalive/
- K3s Join-Token mit CA-Hash: https://docs.k3s.io/cli/token
- Go Monotonic Time: https://go.googlesource.com/proposal/+/refs/heads/master/design/12914-monotonic.md

**Storage**
- modernc.org/sqlite: https://pkg.go.dev/modernc.org/sqlite
- go-sqlite-bench: https://github.com/cvilsmeier/go-sqlite-bench
- SQLite WAL / Pragmas: https://sqlite.org/wal.html, https://sqlite.org/pragma.html
- Uptime Kuma Performance (#4747): https://github.com/louislam/uptime-kuma/issues/4747
- sqlc Konfiguration (mehrere Engines): https://docs.sqlc.dev/en/latest/reference/config.html
- goose: https://github.com/pressly/goose

**Checks / Alerting / SSRF**
- Go net/http Timeouts: https://blog.cloudflare.com/the-complete-guide-to-golang-net-http-timeouts
- golang/go#39752, #36026 (Verbindungs-Reuse): https://golang.org/issue/39752, https://github.com/golang/go/issues/36026
- pro-bing: https://github.com/prometheus-community/pro-bing; Gatus ICMP-Issue: https://github.com/TwiN/gatus/issues/697
- Go DNS TC/TCP: https://golang.org/issue/64896
- SSRF / safeurl: https://blog.doyensec.com/2022/12/13/safeurl.html
- Telegram Bot-Limits: https://core.telegram.org/bots/faq; Discord Rate Limits: https://docs.discord.com/developers/topics/rate-limits
- nicholas-fedor/shoutrrr: https://github.com/nicholas-fedor/shoutrrr

**Frontend / Auth / Lizenz**
- Angular Releases: https://angular.dev/reference/releases
- Go embed: https://pkg.go.dev/embed
- OWASP Password Storage / Authentication / Session Management Cheat Sheets: https://cheatsheetseries.owasp.org/
- Zitadel Scopes / Token-Typ: https://zitadel.com/docs/apis/openidoauth/scopes, https://zitadel.com/docs/guides/manage/console/applications
- angular-auth-oidc-client: https://github.com/damienbod/angular-auth-oidc-client
- oapi-codegen: https://github.com/oapi-codegen/oapi-codegen; ogen: https://github.com/ogen-go/ogen
- CLA Assistant: https://github.com/cla-assistant/cla-assistant
