# Pulsequorum – Konzept (v0.3)

> Hochverfügbarer, mandantenfähiger, selbst hostbarer Uptime-Monitor im Stil von Uptime Kuma. Läuft als Cluster aus 1…N Docker-Containern auf verschiedenen Hosts.
> Stand v0.3: Alle Grundsatzentscheidungen getroffen. Nächste Schritte stehen in Kapitel 21.

---

## 0. Entscheidungslog

| # | Thema | Entscheidung |
|---|-------|--------------|
| E1 | Verteilung der Checks | Pro Monitor wählbar: alle Nodes, k Nodes (automatisch verteilt) oder bestimmte Nodes |
| E2 | DOWN-Regel | Pro Monitor wählbar: Mehrheit (Vorauswahl), ein Node, alle Nodes |
| E3 | Monitor-Typen MVP | HTTP(S), TCP, ICMP, DNS. Push folgt später |
| E4 | Größenordnung | bis ~1.000 Monitore |
| E5 | Kürzestes Intervall | 20 Sekunden |
| E6 | Aufbewahrung | Rohdaten 30 Tage, Stunden-Rollups 1 Jahr, Tages-Rollups unbegrenzt |
| E7 | Benachrichtigungen | Shoutrrr-kompatible URLs (E-Mail, Telegram, Discord, ntfy, Gotify u.v.m.) + generischer Webhook mit JSON-Template |
| E8 | Ohne Quorum | Alerts werden weiter versendet, aber als „unbestätigt – Cluster degraded“ gekennzeichnet |
| E9 | Inter-Node-Kommunikation | Eingebautes mTLS mit clustereigener CA |
| E10 | Datenhaltung | Standard self-contained (SQLite + Raft); optional externe PostgreSQL |
| E11 | Standorte | Nodes bei verschiedenen Anbietern/Regionen; interne Dienste müssen über gebundene Nodes prüfbar sein |
| E12 | Clustergröße | 3–5 Voter + beliebig viele Probe-Nodes |
| E13 | Nutzer & Mandanten | Mandantenfähig über **Zitadel** als IdP; Zitadel ist optional (ohne: lokale Nutzer, ein Mandant) |
| E14 | Statusseiten | Ja, in einer späteren Phase, pro Mandant |
| E15 | Frontend-Zugriff | SPA spricht alle Nodes direkt an, Failover im Client |
| E16 | Backend-Sprache | **Go** (Kap. 18) |
| E17 | Projektrahmen | Eigene Nutzung + Open Source + kommerzielles Produkt |
| E18 | Lizenz | **AGPLv3 + kommerzielle Lizenz** (Dual Licensing, Kap. 19) |
| E19 | Rohdaten-Verdichtung | Standardmäßig **an** (nach 48 h), abschaltbar |
| E20 | Projektname | Arbeitstitel **Pulsequorum** bleibt vorerst |
| E21 | Plattform-Admin-UI | Im selben Frontend, nur für `platform-admin` sichtbar |

---

## 1. Ziele

| # | Ziel |
|---|------|
| Z1 | Überwachung von Diensten (HTTP(S), TCP, ICMP, DNS; später weitere) mit konfigurierbaren Erwartungen. |
| Z2 | Uptime (24 h / 7 d / 30 d / 365 d), Antwortzeiten und Incidents je Monitor und je Messstandort. |
| Z3 | **Hochverfügbarkeit:** Fällt ein Host aus, überwachen und alarmieren die übrigen weiter; die Daten bleiben konsistent. |
| Z4 | **Ein Image für alle Modi:** Single-Node = Cluster mit einem Mitglied, jederzeit erweiterbar ohne Migration. |
| Z5 | **Erweiterbar:** neue Monitor-Typen über eine Plugin-Schnittstelle, Benachrichtigungen über URL-Schema. |
| Z6 | **Mandantenfähig:** strikte Trennung der Daten pro Mandant, Identitäten über Zitadel. |
| Z7 | **Frontend unabhängig von einzelnen Nodes:** eingebettet in jedem Node und optional statisch extern gehostet. |
| Z8 | **Schlank im Betrieb:** ohne externe Abhängigkeiten lauffähig; PostgreSQL und Zitadel sind optionale Ausbaustufen. |

### Nicht-Ziele (vorerst)
- Kein APM, keine Metrik-/Log-Plattform.
- Kein Import aus Uptime Kuma (kann später als Community-Feature folgen).

---

## 2. Begriffe

| Begriff | Bedeutung |
|---------|-----------|
| **Node** | Laufende Instanz des Containers. Rolle: *Voter* oder *Probe*. |
| **Voter** | Vollwertiger Node mit Stimmrecht im Raft-Konsens. Liefert API/UI, prüft, speichert alle Daten. Empfohlen: 1, 3 oder 5. |
| **Probe** | Leichter Mess-Node ohne Stimmrecht und ohne API/UI. Verbindet sich **ausgehend** mit dem Cluster; funktioniert daher auch hinter NAT, z.B. im Heimnetz oder in einem Kundennetz. |
| **Leader** | Aktuell gewählter Raft-Leader unter den Votern. Nimmt Konfigurationsänderungen an und versendet Alerts. |
| **Mandant (Tenant)** | Abgeschlossener Bereich mit eigenen Monitoren, Kanälen, Statusseiten und Nutzern. Entspricht einer *Organization* in Zitadel. |
| **Monitor** | Konfigurierter Check (Typ, Ziel, Erwartungen, Intervall, Zuweisung, DOWN-Regel). |
| **Heartbeat** | Einzelnes Check-Ergebnis eines Nodes für einen Monitor. |
| **Konsens-Status** | Aus den Heartbeats der zuständigen Nodes abgeleiteter Gesamtstatus. |

---

## 3. Betriebsmodi

| Modus | Aufbau | Ausfallsicherheit |
|-------|--------|-------------------|
| **Single** | 1 Voter, lokale Nutzer, ein Mandant | keine |
| **Cluster** | 3 Voter (empfohlen) | 1 Voter darf ausfallen |
| **Cluster groß** | 5 Voter | 2 Voter dürfen ausfallen |
| **+ Probes** | zusätzlich beliebig viele Probes | Probes erhöhen Messabdeckung, nicht das Quorum |
| **+ PostgreSQL** | Voter teilen sich eine externe DB statt eigener Replikation | abhängig von der HA der Datenbank |
| **+ Zitadel** | Mandantenfähigkeit, SSO | Zitadel-Ausfall blockiert nur neue Logins (siehe 12.4) |

Gerade Voter-Anzahlen (2, 4) bringen keinen Gewinn an Ausfallsicherheit; das UI warnt davor.

---

## 4. Architektur-Überblick

```mermaid
flowchart LR
    subgraph Clients
        SPA[Angular SPA<br/>eingebettet oder Cloudflare Pages]
        Status[Statusseiten<br/>spätere Phase]
    end

    Zitadel[(Zitadel<br/>optional)]

    subgraph Core[Voter – verschiedene Anbieter/Regionen]
        N1[Voter 1<br/>Hetzner]
        N2[Voter 2<br/>Netcup]
        N3[Voter 3<br/>OVH]
    end

    subgraph Probes[Probes – optional]
        P1[Probe Heimnetz<br/>hinter NAT]
        P2[Probe Kundennetz]
    end

    N1 <-->|Raft + Replikation, mTLS| N2
    N2 <-->|mTLS| N3
    N1 <-->|mTLS| N3

    P1 -->|ausgehend, mTLS| N1 & N2 & N3
    P2 -->|ausgehend, mTLS| N1 & N2 & N3

    SPA -->|HTTPS/WSS, Client-Failover| N1 & N2 & N3
    SPA -->|OIDC Login| Zitadel
    N1 & N2 & N3 -.->|JWKS, gecacht| Zitadel

    N1 & N2 & N3 & P1 & P2 -->|Checks| Targets[(Überwachte Dienste)]
    N1 -.->|Alerts nur durch Leader| Notify[Shoutrrr / Webhook]
```

Alle Voter sind identisch und gleichwertig. Es gibt keinen festen „Master-Host“, nur einen dynamisch gewählten Leader.

---

## 5. Komponenten

### 5.1 Voter

```
┌──────────────────────────────────────────────────────────┐
│ Voter                                                    │
│  HTTP API (REST) │ WebSocket Live-Feed │ eingebettete SPA │
│  ─────────────────────────────────────────────────────── │
│  Auth (OIDC/Zitadel oder lokal) + Tenant-Kontext         │
│  ─────────────────────────────────────────────────────── │
│  Service-Layer: Monitore, Uptime, Incidents, Kanäle      │
│  ─────────────────────────────────────────────────────── │
│  Scheduler + Checker-Plugins │ Aggregator │ Alert-Mgr    │
│  ─────────────────────────────────────────────────────── │
│  Cluster-Layer: Raft │ Heartbeat-Replikation │ Probe-GW  │
│  ─────────────────────────────────────────────────────── │
│  Storage-Abstraktion: SQLite (Standard) │ PostgreSQL     │
└──────────────────────────────────────────────────────────┘
```

| Komponente | Aufgabe |
|------------|---------|
| **HTTP API** | REST für Frontend und Automatisierung. Schreibende Requests werden intern an den Leader weitergeleitet. |
| **WebSocket** | Live-Events (Heartbeats, Statuswechsel, Node-Status), gefiltert nach Mandant. |
| **Auth** | Validiert OIDC-Tokens (Zitadel) oder lokale Sessions; leitet daraus Mandant und Rolle ab. |
| **Scheduler** | Plant die Checks, für die dieser Node zuständig ist (siehe Kap. 7), mit Jitter. |
| **Checker-Plugins** | Führen die Checks aus. |
| **Aggregator** | Berechnet Konsens-Status und Uptime-Rollups. |
| **Alert-Manager** | Erkennt Statuswechsel, dedupliziert, versendet. Aktiv nur auf dem Leader (bzw. im Degraded-Modus, siehe 9.3). |
| **Probe-Gateway** | gRPC-Endpunkt, über den Probes Konfiguration abonnieren und Heartbeats hochladen. |
| **Storage-Abstraktion** | Repository-Schicht mit zwei Implementierungen (Kap. 6). |

### 5.2 Probe
Schlanker Prozess (gleiches Image, `NODE_ROLE=probe`), bestehend nur aus Scheduler, Checker-Plugins und einem gRPC-Client:
- baut eine ausgehende mTLS-Verbindung zu einem beliebigen Voter auf (Failover über die Voter-Liste),
- abonniert die Monitore, die ihm zugewiesen sind,
- lädt Heartbeats hoch; bei Verbindungsverlust puffert er lokal und lädt später nach.

Probes sind bewusst **keine Raft-Mitglieder**. Dadurch funktionieren sie hinter NAT, beeinflussen das Quorum nicht und lassen sich beliebig skalieren.

---

## 6. Datenhaltung & Synchronisation

### 6.1 Zwei Datenklassen

| Klasse | Beispiele | Mechanismus (embedded) | Konsistenz | Ohne Quorum |
|--------|-----------|------------------------|------------|-------------|
| **A – Konfiguration & Zustand** | Mandanten, Monitore, Kanäle, lokale Nutzer, Wartungsfenster, Nodes, Alert-Zustand, Schlüssel | Raft-Log → lokale SQLite | stark | read-only |
| **B – Messdaten** | Heartbeats | append-only pro Node, asynchron an alle Voter repliziert | eventual, konfliktfrei | weiter beschreibbar |

Begründung: Konfiguration muss überall identisch sein und ändert sich selten → Raft. Heartbeats entstehen massenhaft und dürfen bei Quorumverlust nicht blockieren → konfliktfreie Replikation.

### 6.2 Heartbeat-Replikation
- Schlüssel `(node_id, seq)`. Zwei Nodes schreiben nie denselben Datensatz → kein Merge, keine Konflikte.
- Jeder Voter streamt neue Heartbeats an alle anderen Voter; Probe-Heartbeats nimmt ein Voter an und verteilt sie weiter.
- **Anti-Entropie:** Nach Ausfall oder Netztrennung fragt ein Node: „alles von Node X ab Sequenz N“. Lücken schließen sich automatisch.
- Jeder Voter hat dadurch alle Heartbeats und kann jede Leseanfrage allein beantworten.

### 6.3 Storage-Backends

| | **embedded** (Standard) | **postgres** (optional) |
|---|---|---|
| Konfiguration | Raft + SQLite pro Voter | gemeinsame Tabellen in PostgreSQL |
| Heartbeats | lokale SQLite, repliziert | direkt in PostgreSQL |
| Leader-Wahl | Raft | Lease über PostgreSQL (Advisory Lock bzw. Lease-Tabelle) |
| HA der Daten | durch die App | durch PostgreSQL (z.B. Patroni, Managed DB) |
| Wann sinnvoll | 1–5 Voter, kein DB-Betrieb gewünscht | kommerzielles Hosting, sehr viele Monitore, vorhandene DB-Infrastruktur |

Die Geschäftslogik arbeitet nur gegen die Repository-Schnittstelle; das Backend wird per `STORAGE_BACKEND=embedded|postgres` gewählt. Ein Export/Import-Werkzeug erlaubt den späteren Wechsel.

### 6.4 Zeit
NTP ist Pflicht. Nodes tauschen ihre Uhrzeit aus; bei > 2 s Abweichung Warnung im UI.

### 6.5 Datenmodell (vereinfacht)

```
Tenant(id, name, zitadel_org_id?, plan, limits_json)
Monitor(id, tenant_id, type, name, config_json, interval_s, timeout_s, retries,
        assignment_json, consensus_policy, tags, paused)
Heartbeat(node_id, seq, tenant_id, monitor_id, ts, status, latency_ms, message, details_json)
MonitorState(monitor_id, consensus_status, confirmed, since)
Incident(id, tenant_id, monitor_id, started_at, resolved_at, cause)
NotificationChannel(id, tenant_id, kind[shoutrrr|webhook], url_enc, template?)
MaintenanceWindow(id, tenant_id, monitor_ids, start, end, recurrence)
LocalUser(id, name, password_hash, role)          -- nur ohne Zitadel
Node(id, name, role[voter|probe], region, owner_tenant_id?, last_seen)
UptimeRollup(monitor_id, node_id?, bucket[1h|1d], start, up_s, down_s, maint_s, unknown_s, lat_avg, lat_p95)
```

`tenant_id` steht an allen mandantenbezogenen Daten; jede Abfrage wird in der Repository-Schicht zwingend danach gefiltert.

### 6.6 Speicherbedarf (Abschätzung)

Annahmen: 1.000 Monitore, 3 Voter, ~100 Byte pro Heartbeat inkl. Index, Rohdaten 30 Tage.

| Ø-Intervall | Heartbeats/Tag | Rohdaten 30 Tage pro Voter |
|-------------|----------------|----------------------------|
| 60 s | ~4,3 Mio. | ~13 GB |
| 20 s (alle) | ~13 Mio. | ~39 GB |

Replikationsverkehr bei 60 s: grob 150–300 MB pro Tag und Verbindung, also unkritisch.

**Verdichtung (E19, Standard an):** Heartbeats älter als 48 h werden verdichtet:
- **Statuswechsel** bleiben exakt erhalten (Zeitpunkt, Node, Meldung) → Uptime und Incidents bleiben sekundengenau.
- **Latenzen** werden zu Minutenwerten je Node und Monitor zusammengefasst (min/avg/max/p95, Anzahl OK/Fehler).
- Ergebnis: grob ~1–4 GB statt 13–39 GB pro Voter für 30 Tage.
- Abschaltbar (`RAW_COMPACTION=false`), dann bleiben die vollen Rohdaten 30 Tage erhalten. Die Verdichtung läuft deterministisch auf jedem Voter, es wird nichts zusätzlich repliziert.

---

## 7. Zuweisung & Konsens-Status

### 7.1 Zuweisung (pro Monitor, E1/E11)

| Modus | Verhalten | Typischer Einsatz |
|-------|-----------|-------------------|
| `all` (Vorauswahl im Formular) | alle Voter und alle öffentlichen Probes prüfen | öffentliche Websites, Multi-Standort-Messung |
| `count: k` | k Nodes, verteilt per Consistent Hashing; fällt einer aus, übernimmt automatisch der nächste | Lastverteilung |
| `nodes: [...]` | nur ausgewählte Nodes | **interne Dienste**, die nur bestimmte Nodes/Probes erreichen |

Später: Zuweisung über Node-Labels (z.B. `region=eu`).

**Private Probes (kommerziell interessant):** Ein Mandant kann eigene Probes registrieren (`owner_tenant_id`). Diese prüfen nur Monitore dieses Mandanten, z.B. Dienste in seinem internen Netz.

### 7.2 DOWN-Regel (pro Monitor, E2)

| Policy | DOWN, wenn … |
|--------|--------------|
| `majority` (Vorauswahl) | mehr als die Hälfte der meldenden Nodes DOWN meldet |
| `any` | mindestens ein Node DOWN meldet |
| `all` | alle meldenden Nodes DOWN melden |

Bei `nodes: [x]` mit nur einem Node fällt die Regel natürlich mit dessen Ergebnis zusammen.

Zusatzstatus **PARTIAL**: Ein Teil der Nodes meldet DOWN, die Regel ist aber nicht erfüllt → Hinweis auf ein regionales Problem.

Ein Node, der selbst nichts mehr erreicht (eigener Netzausfall: alle seine Checks schlagen plötzlich gleichzeitig fehl, und er fehlt im Cluster), wird automatisch aus der Bewertung genommen.

---

## 8. Ausfall- und Partitionsszenarien (3 Voter)

| Szenario | Checks | Konfig ändern | Alerts | UI |
|----------|--------|---------------|--------|----|
| Alle online | alle | ja | Leader, bestätigt | jeder Voter |
| 1 Voter offline | 2 Voter + Probes | ja | Leader (ggf. neu gewählt), bestätigt | die übrigen Voter |
| 2 Voter offline | 1 Voter + Probes | nein | **unbestätigt** (E8) | verbleibender Voter, read-only |
| Split 2 \| 1 | alle messen weiter | nur Mehrheitsseite | Mehrheit: bestätigt; Minderheit: unbestätigt | beide Seiten lesbar |
| Voter kehrt zurück | — | — | — | holt Raft-Log und Heartbeats automatisch nach |
| Probe offline | Rest | ja | Selbstüberwachungs-Alert | — |

---

## 9. Alerting

### 9.1 Ablauf (mit Quorum)
1. Aggregator erkennt einen Wechsel des Konsens-Status (unter Beachtung von `retries`).
2. Leader schreibt `MonitorState` und ggf. `Incident` per Raft.
3. Nach dem Commit versendet der Leader die Benachrichtigung über die Kanäle des Mandanten.

### 9.2 Deduplizierung
Der Alert-Zustand liegt im Raft-Log; ein neuer Leader weiß, was bereits gemeldet wurde. Zustellung: *at least once*, kein Alert geht verloren.

### 9.3 Degraded-Modus (E8)
Ohne Quorum gibt es keinen Leader. Damit trotzdem genau ein Node alarmiert, versendet in jeder Minderheitspartition der Node mit der kleinsten ID die Alerts:
- Kennzeichnung im Text/Titel: **„⚠ unbestätigt – Cluster degraded (1/3 Nodes)“**,
- lokales Protokoll der versendeten Alerts,
- nach Wiederherstellung des Quorums gleicht der Leader den Zustand ab und sendet bei Bedarf eine Korrektur („Fehlalarm“ bzw. „bestätigt“).

### 9.4 Selbstüberwachung
Systemalerts an einen plattformweiten Admin-Kanal: Voter/Probe offline, Quorum verloren/wiederhergestellt, Replikationsrückstand, Uhrzeitabweichung, Speicherplatz knapp.

### 9.5 Kanäle (E7)
- **Shoutrrr-URLs** decken u.a. E-Mail (SMTP), Telegram, Discord, Slack, Teams, Matrix, ntfy, Gotify und Pushover ab. Beispiele:
  ```
  telegram://<token>@telegram?chats=@meinkanal
  discord://<token>@<webhook-id>
  ntfy://user:pass@ntfy.sh/mein-topic
  gotify://gotify.example.com/<token>
  smtp://user:pass@mail.example.com:587/?from=alert@example.com&to=ops@example.com
  ```
  Im Frontend gibt es für die gängigen Dienste kleine Formulare, die die URL erzeugen; Experten können die URL direkt eingeben.
- **Generischer Webhook** mit frei definierbarem JSON-Template (Platzhalter wie `{{monitor.name}}`, `{{status}}`, `{{confirmed}}`).
- Umsetzung mit der Bibliothek `nicholas-fedor/shoutrrr` (gepflegter Fork des ursprünglichen Shoutrrr), Version gepinnt.
- URLs enthalten Geheimnisse → verschlüsselt gespeichert, im UI maskiert.

---

## 10. Uptime-Berechnung

- Grundlage ist die Zeitreihe des **Konsens-Status**; zusätzlich wird die Uptime **je Node** ausgewiesen (Standortvergleich).
- `Uptime % = Zeit_UP / (Gesamtzeit − Wartung − unbekannt) × 100`
- PARTIAL zählt standardmäßig als UP (pro Mandant einstellbar).
- Zeiträume ohne Daten (gesamter Cluster aus) gelten als „unbekannt“, nicht als DOWN.
- Unbestätigte Statuswechsel aus dem Degraded-Modus werden nach Wiederherstellung neu bewertet.

**Aufbewahrung (E6, E19):** Rohdaten 48 h voll, danach verdichtet bis 30 Tage · Stunden-Rollups 1 Jahr · Tages-Rollups unbegrenzt. Rollups werden auf jedem Voter deterministisch aus denselben Daten berechnet.

---

## 11. Monitor-Typen & Plugin-Schnittstelle

### 11.1 MVP (E3)

| Typ | Prüfungen |
|-----|-----------|
| **HTTP(S)** | Methode, Header, Body, Auth (Basic/Bearer), erwartete Statuscodes (z.B. `200-299,301`), Keyword vorhanden/nicht vorhanden, JSON-Pfad = Wert, max. Antwortzeit, Redirects, TLS gültig + Ablauf-Warnung (Tage konfigurierbar) |
| **TCP** | Port erreichbar, Verbindungszeit |
| **ICMP** | Erreichbarkeit, Paketverlust, RTT (Container benötigt `NET_RAW`) |
| **DNS** | Record-Typ (A, AAAA, CNAME, MX, TXT, NS …), erwarteter Wert, eigener Resolver |

**Später:** Push/Heartbeat, Zertifikats- und Domain-Ablauf als eigene Typen, gRPC-Health, SMTP/IMAP, Datenbanken, MQTT, Gruppen-/Kombi-Monitore.

### 11.2 Schnittstelle (Go)

```go
type Checker interface {
    Type() string                                   // "http", "tcp", …
    ConfigSchema() json.RawMessage                  // JSON Schema: Validierung + dynamisches Formular
    Validate(cfg json.RawMessage) error
    Check(ctx context.Context, cfg json.RawMessage) Result
}

type Result struct {
    Status    Status         // UP | DOWN | DEGRADED
    LatencyMs int
    Message   string
    Details   map[string]any // z.B. Statuscode, TLS-Ablaufdatum
}

// Registrierung beim Start:
func init() { checker.Register(&httpChecker{}) }
```

Das `configSchema` wird über die API ausgeliefert; das Frontend rendert Formulare für neue Typen ohne eigene Änderung. Probes und Voter nutzen dieselben Plugins.

---

## 12. Mandanten & Authentifizierung (E13)

### 12.1 Zwei Modi

| | **lokal** (ohne Zitadel) | **Zitadel** |
|---|---|---|
| Mandanten | genau einer (`default`) | beliebig viele, 1 Zitadel-Organisation = 1 Mandant |
| Nutzer | lokale Konten (Argon2id) | in Zitadel, inkl. SSO, MFA, Einladungen |
| Token | vom Cluster signiertes JWT (Schlüssel im Raft-Store) | Zitadel-Access-Token (OIDC) |
| Wechsel | lokal → Zitadel per Migration: Mandant `default` wird einer Organisation zugeordnet | — |

### 12.2 Rollen
| Rolle | Rechte |
|-------|--------|
| `platform-admin` | Cluster, Nodes, Mandanten, Limits, Systemalerts |
| `tenant-admin` | alles innerhalb des eigenen Mandanten inkl. Kanäle und private Probes |
| `tenant-editor` | Monitore anlegen/bearbeiten |
| `tenant-viewer` | nur lesen |

In Zitadel als Projekt-Rollen modelliert; Organisation und Rollen kommen als Claims im Token.

### 12.3 Ablauf (Zitadel)
- Die SPA führt den OIDC Authorization Code Flow mit PKCE direkt gegen Zitadel aus.
- Jeder Voter validiert Tokens selbstständig über die gecachten JWKS-Schlüssel – keine Abhängigkeit vom Leader.
- Neue Mandanten: Organisation in Zitadel anlegen → erster Login legt den Mandanten automatisch an (oder Provisionierung durch `platform-admin`).

### 12.4 Verfügbarkeit
Ist Zitadel nicht erreichbar, laufen Checks, Alerts und bestehende Sessions weiter (gecachte Schlüssel). Nur neue Logins schlagen fehl. Empfehlung für Produktivbetrieb: Zitadel selbst hochverfügbar betreiben oder Zitadel Cloud nutzen.

### 12.5 Limits pro Mandant (kommerziell)
Max. Anzahl Monitore, kürzestes erlaubtes Intervall, Anzahl Statusseiten, private Probes – hinterlegt im Mandanten-Plan.

---

## 13. Frontend (E15)

- **Angular SPA**, ein Build-Artefakt.
- **Hosting:** in jeden Voter eingebettet; optional zusätzlich auf Cloudflare Pages / GitHub Pages.
- **Laufzeit-Konfiguration** `config.json`:
  ```json
  {
    "nodes": ["https://mon1.example.com", "https://mon2.example.com", "https://mon3.example.com"],
    "auth": { "mode": "zitadel", "issuer": "https://auth.example.com", "clientId": "…" }
  }
  ```
- **Client-Failover:** Beim Start Health-Check aller Nodes, Auswahl des schnellsten gesunden, automatischer Wechsel bei Fehlern; die aktuelle Node-Liste wird zusätzlich von `/api/v1/cluster/nodes` nachgeladen.
- **Schreiben:** an einen beliebigen Voter, der intern an den Leader weiterleitet.
- **CORS:** Allow-List der Frontend-Origins; bei Zitadel müssen alle Origins als Redirect-URIs eingetragen sein.
- **Ansichten (Mandant):** Dashboard · Monitor-Detail (Konsens + Status/Latenz je Node) · Monitor-Editor (dynamisch aus Schema) · Benachrichtigungen · Wartungsfenster · Incidents · Mandanten-Einstellungen · private Probes.
- **Plattform-Admin (E21):** eigener Bereich `/admin` im selben Frontend, per Lazy-Loaded Angular-Modul und Route-Guard nur für `platform-admin` sichtbar: Cluster (Nodes, Leader, Quorum, Replikationsrückstand, Uhrzeit), Join-Tokens, Mandanten & Pläne/Limits, Systemalerts. Die eigentliche Absicherung erfolgt serverseitig über die Rolle im Token; das Ausblenden im UI ist nur Komfort.
- **Später:** öffentliche Statusseiten pro Mandant mit eigener Domain (E14).

---

## 14. API (Überblick)

```
GET    /api/v1/health                          # Rolle, Leader, Quorum, Version
POST   /api/v1/auth/login | /refresh           # nur lokaler Modus
GET    /api/v1/me                              # Nutzer, Mandant, Rollen

GET    /api/v1/monitors                        # inkl. Konsens-Status
POST   /api/v1/monitors
GET    /api/v1/monitors/{id}
PUT    /api/v1/monitors/{id}
DELETE /api/v1/monitors/{id}
POST   /api/v1/monitors/{id}/pause | /resume
GET    /api/v1/monitors/{id}/heartbeats?from&to&node
GET    /api/v1/monitors/{id}/uptime?range=24h|7d|30d|365d&node
GET    /api/v1/monitor-types                   # inkl. configSchema

CRUD   /api/v1/notifications
POST   /api/v1/notifications/{id}/test
CRUD   /api/v1/maintenance
GET    /api/v1/incidents

GET    /api/v1/cluster/nodes
POST   /api/v1/cluster/join-tokens             # voter | probe [, tenant]
DELETE /api/v1/cluster/nodes/{id}

CRUD   /api/v1/admin/tenants                   # platform-admin

WS     /api/v1/stream                          # Live-Events, mandantengefiltert
gRPC   ProbeService (Subscribe, UploadHeartbeats)   # Cluster-Port
```

Mandanten-Kontext ergibt sich ausschließlich aus dem Token, nie aus URL-Parametern.

---

## 15. Sicherheit

- **mTLS** zwischen allen Nodes mit clustereigener CA (E9). Die CA entsteht beim Bootstrap; neue Nodes erhalten ihr Zertifikat per einmaligem, zeitlich begrenztem Join-Token.
- Cluster-Port per Firewall nur für Voter-IPs öffnen; Probes verbinden sich ausgehend.
- Geheimnisse (Kanal-URLs, Webhook-Tokens) verschlüsselt im Store; Schlüsselrotation über Raft.
- Strikte Mandantentrennung in der Repository-Schicht, abgesichert durch automatisierte Tests.
- Rate-Limiting auf Login und öffentlichen Endpunkten; SSRF-Schutz konfigurierbar (z.B. private IP-Ranges für Mandanten-Monitore auf öffentlichen Nodes sperren – wichtig im kommerziellen Betrieb).

---

## 16. Deployment

### 16.1 Single-Node (lokal, ohne Zitadel)
```yaml
services:
  uptime:
    image: ghcr.io/<org>/pulsequorum:latest
    restart: unless-stopped
    environment:
      NODE_NAME: vps1
      NODE_ROLE: voter
      PUBLIC_URL: https://mon1.example.com
      AUTH_MODE: local
      STORAGE_BACKEND: embedded
    volumes:
      - ./data:/data
    ports:
      - "8080:8080"   # API + UI
      - "9443:9443"   # Cluster (für späteren Join)
    cap_add:
      - NET_RAW       # ICMP
```

### 16.2 Weiteren Voter hinzufügen
```yaml
    environment:
      NODE_NAME: vps2
      NODE_ROLE: voter
      PUBLIC_URL: https://mon2.example.com
      CLUSTER_JOIN: https://mon1.example.com:9443
      CLUSTER_JOIN_TOKEN: <token>
```

### 16.3 Probe (z.B. im Heimnetz)
```yaml
    environment:
      NODE_NAME: home-probe
      NODE_ROLE: probe
      CLUSTER_SEEDS: https://mon1.example.com:9443,https://mon2.example.com:9443
      CLUSTER_JOIN_TOKEN: <token>
    # keine eingehenden Ports nötig
```

### 16.4 Mit Zitadel / PostgreSQL
```yaml
      AUTH_MODE: zitadel
      ZITADEL_ISSUER: https://auth.example.com
      ZITADEL_AUDIENCE: <project-id>
      STORAGE_BACKEND: postgres
      DATABASE_URL: postgres://…
```

---

## 17. Skalierung

| Ziel | Maßnahme |
|------|----------|
| Mehr Ausfallsicherheit | 3 → 5 Voter |
| Mehr Messstandorte / interne Netze | Probes (öffentlich oder privat pro Mandant) |
| Mehr Monitore | `count: k` statt `all`; Probes übernehmen Last; ab mehreren Tausend Monitoren PostgreSQL-Backend |
| Mehr Leselast | jeder Voter beantwortet Lesezugriffe lokal |

---

## 18. Tech-Stack (E16: Go)

### 18.1 Komponenten

| Bereich | Wahl |
|---------|------|
| Sprache | Go (aktuelle stabile Version) |
| HTTP/API | `net/http` + `chi` (Router), OpenAPI-Spezifikation als Vertrag zum Frontend |
| WebSocket | `coder/websocket` |
| Raft | `hashicorp/raft` + `raft-boltdb` (Log-Store) |
| SQLite | `modernc.org/sqlite` (pure Go, kein CGO → einfache Multi-Arch-Builds) |
| PostgreSQL | `pgx` |
| SQL | `sqlc` (typsichere Queries aus SQL), Migrationen mit `goose` |
| Inter-Node | `grpc-go` über mTLS, Protobuf-Definitionen in `proto/` |
| OIDC/Zitadel | `coreos/go-oidc` (Token-Validierung über JWKS) |
| Notifications | `nicholas-fedor/shoutrrr` |
| ICMP / DNS | `prometheus-community/pro-bing` / `miekg/dns` |
| Logging / Metriken | `log/slog` (Standardbibliothek), optional `/metrics` im Prometheus-Format |
| Frontend | Angular, Charts mit ECharts, per `go:embed` ins Binary eingebettet |
| Build | GitHub Actions → Multi-Arch-Image (amd64/arm64) in GHCR, Basis `distroless/static` |

Ergebnis: **ein einziges statisches Binary** (Voter und Probe, gesteuert über `NODE_ROLE`), Image ~20–30 MB.

### 18.2 Repository-Struktur (Vorschlag)

```
pulsequorum/
├── cmd/pulsequorum/        # main – startet Voter oder Probe
├── internal/
│   ├── api/                  # REST-Handler, WebSocket, Write-Forwarding
│   ├── auth/                 # lokal + OIDC/Zitadel, Rollen, Tenant-Kontext
│   ├── checker/              # Plugin-Registry + http, tcp, icmp, dns
│   ├── scheduler/            # Zuweisung, Jitter, Consistent Hashing
│   ├── aggregator/           # Konsens-Status, Rollups, Verdichtung
│   ├── alert/                # Alert-Manager, Shoutrrr, Webhook-Templates
│   ├── cluster/              # Raft-FSM, Replikation, Anti-Entropie, Probe-Gateway, PKI
│   ├── store/                # Repository-Interfaces + sqlite/ + postgres/
│   └── tenant/               # Mandanten, Pläne, Limits
├── proto/                    # gRPC-Definitionen (Replikation, Probe-Service)
├── api/openapi.yaml          # REST-Vertrag
├── web/                      # Angular-App
├── deploy/                   # Compose-Beispiele (single, cluster, probe)
└── docs/
```

### 18.3 Einstieg in Go (für Java-Erfahrung)
- „A Tour of Go“ und „Effective Go“ (offizielle Doku) als Grundlage.
- Wichtigste Umstellungen gegenüber Spring: explizite Abhängigkeiten statt DI-Container, Fehler als Rückgabewerte, implizite Interfaces, Goroutines/Channels mit `context.Context` für Abbruch und Timeouts.
- Empfehlung: Erst einen einzelnen Checker (HTTP) samt Test schreiben, um die Sprache an einem überschaubaren Stück kennenzulernen, bevor es an den Cluster-Layer geht.

---

## 19. Lizenz- und Geschäftsmodell (E18: AGPLv3 + kommerzielle Lizenz)

### 19.1 Modell
- **Open Source unter AGPLv3:** Jeder darf Pulsequorum frei nutzen, verändern und selbst betreiben. Wer eine veränderte Version Dritten über das Netzwerk anbietet (z.B. als gehosteten Dienst), muss den Quellcode dieser Version offenlegen.
- **Kommerzielle Lizenz:** Für Unternehmen, die die AGPL-Pflichten nicht erfüllen wollen oder können (z.B. eigene proprietäre Anpassungen, Einbettung in ein geschlossenes Produkt, White-Label-Hosting). Optional kombiniert mit Support und SLA.
- Alle Features stehen unter beiden Lizenzen zur Verfügung; der Unterschied liegt in den Pflichten, nicht im Funktionsumfang.

### 19.2 Voraussetzungen
- **CLA (Contributor License Agreement):** Dual Licensing funktioniert nur, wenn du die Rechte am gesamten Code hältst. Externe Beiträge brauchen daher eine CLA, z.B. automatisiert über CLA Assistant in GitHub.
- **Lizenzprüfung der Abhängigkeiten** in der CI (z.B. `go-licenses`), damit nur Lizenzen eingebunden werden, die sowohl mit AGPL als auch mit der kommerziellen Lizenz vereinbar sind (MIT, BSD, Apache-2.0, MPL-2.0 unproblematisch).
- **Markenschutz:** Spätestens mit dem finalen Namen prüfen; Forks dürfen den Code nutzen, aber nicht den Namen.
- Rechtliche Ausgestaltung der kommerziellen Lizenz und der CLA mit einem Anwalt klären.

---

## 20. Umsetzungsphasen

| Phase | Inhalt |
|-------|--------|
| **0 – Fundament** | Repo, CI, Lizenz + CLA, OpenAPI- und Proto-Entwurf, **technischer Spike: Raft + Heartbeat-Replikation mit 3 Nodes** (größtes technisches Risiko früh prüfen) |
| **1 – Kern** | Single-Voter mit Cluster-fähiger Architektur (Raft mit 1 Mitglied), Storage-Abstraktion (embedded), HTTP/TCP/ICMP/DNS, Uptime + Rollups + Verdichtung, Shoutrrr + Webhook, lokale Nutzer, Angular-UI eingebettet |
| **2 – Cluster** | Join/Leave, mTLS + CA, Heartbeat-Replikation + Anti-Entropie, Write-Forwarding, Zuweisung (`all`, `count`, `nodes`), Admin-Bereich: Cluster-Ansicht |
| **3 – HA-Logik** | Konsens-Status + PARTIAL, Leader-Alerting mit Deduplizierung, Degraded-Modus, Selbstüberwachung |
| **4 – Mandanten** | Zitadel-Anbindung, Rollen, Mandanten-Limits, SSRF-Schutz, Admin-Bereich: Mandanten |
| **5 – Reichweite** | Probes (inkl. private Probes), externes Frontend mit Client-Failover, PostgreSQL-Backend |
| **6 – Ausbau** | Statusseiten, Push-Monitore, weitere Typen, Node-Labels |

---

## 21. Nächste Schritte

1. **OpenAPI-Spezifikation** (`api/openapi.yaml`) für die REST-API ausarbeiten.
2. **Protobuf-Definitionen** für Replikation und Probe-Service entwerfen.
3. **Raft-Spike:** minimaler Prototyp mit 3 Containern (Konfig per Raft, Heartbeats per Replikation, Ausfall eines Nodes simulieren).
4. **Phase 1 in Arbeitspakete/Issues** zerlegen.
5. Finalen **Projektnamen** festlegen (vor dem ersten öffentlichen Release, wegen Domain, GitHub-Org und Marke).
