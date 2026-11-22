# ADR 0044: Härtung privater Probes im Kundennetz (lokale Zielrichtlinie und Betriebsvorgaben)

|                 |                                                             |
|-----------------|-------------------------------------------------------------|
| **Status**      | Proposed                                                    |
| **Datum**       | 2026-11-22                                                  |
| **Entscheider** | Projektinhaber                                              |
| **Bezug**       | FA-PRB-01..07, FA-SEC-SSRF, FA-CHK-04, FA-CHK-13, PF-29a, ADR 0030, ADR 0034, ADR 0035, ADR 0040, ADR 0042, ADR 0043 |

## Kontext

Eine private Probe (ADR 0035) läuft im Netz des Kunden und führt Prüfaufträge aus, die sie vom Cluster bekommt. ADR 0035
behandelt die Probe aus Sicht des Clusters als nicht vertrauenswürdigen Client. Die umgekehrte Richtung ist bisher nicht
geregelt: **Aus Sicht des Kunden ist der Cluster nicht vertrauenswürdig.** Wer einen Voter kontrolliert (Betreiber der
Plattform, Angreifer nach Kompromittierung) oder im Mandanten Monitore anlegen darf (auch mit einem gestohlenen
API-Token), steuert, was die Probe im Kundennetz tut.

Heutiger Stand:

- Die lokale SSRF-Sperre ist für private Probes standardmäßig aus (`SSRF_PROTECTION=auto` → aus, ADR 0035 Abschnitt 4).
  Beim Speichern sind interne Ziele erlaubt, wenn ein Monitor nur eigenen privaten Probes zugewiesen ist.
- Die Probe spricht damit **jedes** erreichbare Ziel per HTTP(S), TCP, DNS und ICMP an. HTTP-Monitore setzen Methode,
  Header und Body frei (`httpcheck.Config`), auch `POST`, `PUT` und `DELETE`. Das ist Request-Forgery in das Intranet.
- Ergebnisse fließen zurück: `message` (bis 1 KB) und `details_json`. Eine JSON-Path-Prüfung meldet den gelesenen Wert
  (bis 100 Zeichen, `httpcheck.go`), eine Keyword-Prüfung liefert ein Ja/Nein-Orakel, DNS-Prüfungen liefern die
  Antworten, TLS-Prüfungen Zertifikatsdaten. Damit lassen sich Inhalte interner Dienste auslesen.
- `deploy/probe/compose.yaml` setzt weder ein schreibgeschütztes Dateisystem noch `cap_drop` oder
  `no-new-privileges`.

Was bereits trägt und weiterverwendet wird:

- `checker.Dialer` prüft jede Verbindung in `net.Dialer.Control` auf der tatsächlich verbundenen IP und dem Port, nach
  der DNS-Auflösung und für jeden Redirect-Hop. DNS-Rebinding umgeht das nicht (FA-CHK-04, PF-29a).
- ICMP und DNS-Antworten laufen über `Dialer.Allow(ip)`. DNS-Antworten in gesperrten Bereichen werden schon heute
  verworfen und nicht angezeigt (`dnscheck.blockedAnswer`).
- HTTP ignoriert Proxy-Variablen (FA-CHK-13).
- `probe.Policy` ist bereits eine eigene `AddressPolicy` für Probes.

Private Probes (AP-5.2) sind noch in keinem Release enthalten (zuletzt `v0.0.4`). Strengere Standardwerte brechen
daher keine bestehende Installation.

**Ziel:** Der Kunde legt **lokal auf der Probe** fest, was sie in seinem Netz darf. Der Cluster kann diese Grenze weder
lockern noch umgehen. Die Durchsetzung liegt vollständig in der Probe.

## Optionen

### Frage 1: Lokale Zielrichtlinie

**1A Keine Zielrichtlinie, nur `SSRF_PROTECTION`**

- Vorteile: Kein Aufwand.
- Nachteile: Der Kunde hat nur „alles intern“ oder „nichts intern“. Der eigentliche Anwendungsfall (einige interne
  Dienste) erzwingt „alles intern“.

**1B Zielrichtlinie `PROBE_ALLOW_TARGETS`, ohne Angabe ist alles erlaubt (mit Warnung)**

- Vorteile: Bequem, kein Pflichtfeld.
- Nachteile: Unsicherer Standard. Erfahrungsgemäß bleibt die Warnung unbeachtet.

**1C Zielrichtlinie `PROBE_ALLOW_TARGETS`, für private Probes Pflicht; bewusstes Abschalten nur mit `any`**

- Vorteile: Der Kunde muss sich einmal bewusst entscheiden. `any` ist sichtbar und dokumentiert. Kein Bruch, da noch
  nicht veröffentlicht.
- Nachteile: Ein zusätzlicher Schritt beim Einrichten.

### Frage 2: Lokale Beschränkung der Prüfarten

**2A Keine**

- Nachteile: Schreibende HTTP-Methoden mit frei wählbarem Body auf interne Dienste bleiben möglich.

**2B `PROBE_ALLOW_HTTP_METHODS` (Standard bei privaten Probes `GET,HEAD`) und `PROBE_ALLOW_MONITOR_TYPES`
(Standard alle)**

- Vorteile: Die Probe wird standardmäßig rein lesend. Wer `POST`-Health-Endpunkte braucht, erweitert die Liste bewusst.
- Nachteile: Etwas mehr Konfiguration. Monitore mit anderen Methoden melden auf dieser Probe einen Richtlinienfehler.

### Frage 3: Rückfluss von Inhalten

**3A Unverändert**

- Nachteile: Innerhalb der erlaubten Ziele bleibt das Auslesen über JSON-Path, Keywords, DNS-Antworten und
  Zertifikatsdaten möglich.

**3B Lokaler Schalter `PROBE_RESULT_DETAIL=full|minimal`, Standard `full`**

- Vorteile: Kunden mit hohem Schutzbedarf schalten auf `minimal`. Dann meldet die Probe nur Status, Latenz,
  Fehlerklasse, HTTP-Statuscode, DNS-Rcode und Zertifikatsablauf, aber keine gelesenen Werte, DNS-Antworten,
  Zertifikatsnamen, Adressen oder freien Fehlertexte.
- Nachteile: Mit `minimal` ist die Fehlersuche im UI schwerer. Ein Ja/Nein-Orakel über Keyword-Prüfungen bleibt, weil
  die Statusmeldung selbst die Information ist.

**3C Standard `minimal` für private Probes**

- Nachteile: Macht private Probes für den Normalfall deutlich weniger nützlich. Die Zielrichtlinie aus Frage 1 begrenzt
  das Risiko bereits auf vom Kunden freigegebene Ziele.

### Frage 4: Container-Härtung

**4A Nur Dokumentation**

- Nachteile: Das Beispiel ist die Vorlage, nach der Kunden tatsächlich betreiben.

**4B Gehärtetes `deploy/probe/compose.yaml` als Standard, ergänzt um Doku zur Egress-Beschränkung**

- Vorteile: Der Standardweg ist der sichere. Für nicht unterstützte Einstellungen (z. B. Egress) gibt es konkrete
  Beispiele.
- Nachteile: Muss mit ICMP (`ping_group_range`), SQLite-Puffer und Healthcheck getestet werden.

### Frage 5: Sichtbarkeit im Cluster

**5A Keine**

- Nachteile: Mandanten-Admins sehen nicht, warum Monitore auf einer Probe am Richtlinienfehler scheitern, und nicht, ob
  eine Probe mit `any` läuft.

**5B Probe meldet nur das Vorhandensein einer Richtlinie als Capability**

- Vorteile: Über die vorhandenen `Capabilities` (`[a-z0-9_-]`, im FSM bereits gespeichert): `target-policy`, wenn
  `PROBE_ALLOW_TARGETS` nicht `any` ist. Keine Proto-, FSM- oder OpenAPI-Änderung. Die Liste der erlaubten Netze verlässt
  die Probe nicht.
- Nachteile: Das UI zeigt nur „Richtlinie aktiv / keine“, nicht deren Inhalt.

**5C Probe meldet die vollständige Richtlinie**

- Nachteile: Gibt die interne Netzstruktur des Kunden an den Cluster weiter. Genau das soll die Richtlinie begrenzen.

## Entscheidung

**1C, 2B, 3B, 4B, 5B.**

### 1. Zielrichtlinie `PROBE_ALLOW_TARGETS`

- **Syntax:** Kommagetrennte Einträge `IP`, `CIDR`, jeweils optional mit `:port` oder `:von-bis`. IPv6 in eckigen
  Klammern, z. B. `10.20.0.0/16:443,10.30.1.5:8000-8099,[2001:db8::/48]:443,10.0.0.53:53`. Hostnamen sind nicht
  erlaubt, weil die Prüfung auf der verbundenen IP erfolgt. Der Sonderwert `any` hebt die Zielbeschränkung bewusst auf.
  Ungültige Einträge brechen den Start mit Exit-Code ≠ 0 und einer Meldung zum Eintrag ab.
- **Pflicht für private Probes:** Ist `PROBE_ALLOW_TARGETS` leer und hat die Probe ein Mandanten-Zertifikat, startet sie
  nicht (Exit-Code ≠ 0, Meldung mit Verweis auf `docs/betrieb/probe.md`). Öffentliche Probes: leer bedeutet keine
  zusätzliche Beschränkung (wie bisher). Die Variable darf dort als zusätzliche Grenze gesetzt werden.
- **Prüfung:** Eine neue `AddressPolicy` prüft in `Dialer.Control` IP **und** Port. Eine Verbindung ist erlaubt, wenn ein
  Eintrag sie abdeckt **und** die vorhandene Richtlinie (`probe.Policy`, `SSRF_PROTECTION`) sie erlaubt. Die Richtlinien
  werden nur verschärfend verknüpft.
- **Portlose Prüfungen** (`Dialer.Allow`, also ICMP und DNS-Antworten): Ein Eintrag deckt die IP ab, wenn er sie
  unabhängig vom Port enthält. Ein Eintrag mit Port erlaubt damit auch ICMP an diese IP.
  DNS-Antworten außerhalb der Richtlinie werden wie heute bei gesperrten Bereichen verworfen und nicht gemeldet.
- **Immer gesperrte Bereiche:** Loopback (`127.0.0.0/8`, `::1`), Link-Local inklusive Cloud-Metadaten
  (`169.254.0.0/16`, `fe80::/10`), unspezifizierte Adressen und Multicast werden nur durch Einträge freigegeben, die
  vollständig in diesem Bereich liegen. `any`, `0.0.0.0/0` oder `::/0` decken sie nicht ab.
- **Normalisierung** wie im vorhandenen `Dialer`: IPv4-mapped IPv6 wird entpackt, IPv6-Zonen werden entfernt.
- **Redirects und DNS-Rebinding** sind über `Control` je Verbindung abgedeckt. DNS-Server einer DNS-Prüfung müssen von
  der Richtlinie erfasst sein.
- **Restrisiko:** Die Namensauflösung für HTTP- und TCP-Ziele über den System-Resolver läuft nicht über den `Dialer`.
  Ein Angreifer erfährt so, ob ein interner Name auflösbar ist, aber nicht die Adresse und ohne Verbindung zum Ziel.
  Das bleibt bestehen und wird dokumentiert.

### 2. Prüfarten

- `PROBE_ALLOW_HTTP_METHODS`: kommagetrennt, Standard für private Probes `GET,HEAD`, für öffentliche Probes alle.
  `any` erlaubt alle Methoden.
- `PROBE_ALLOW_MONITOR_TYPES`: kommagetrennt aus den registrierten Typen, Standard alle.
- Die Probe prüft beides **vor** der Ausführung. Bei Verstoß wird keine Verbindung aufgebaut.

### 3. Ergebnisdetails

- `PROBE_RESULT_DETAIL=full|minimal`, Standard `full`.
- `minimal` reduziert `message` auf einen festen Text je Fehlerklasse (z. B. `assertion failed`, `HTTP 503`) und
  `details_json` auf eine feste Positivliste: HTTP-Statuscode, DNS-Rcode, Tage bis Zertifikatsablauf. Die Reduktion
  passiert in der Probe vor dem Puffer (`metrics.db`). Was die Probe nie speichert, kann sie nicht übertragen.

### 4. Richtlinienverstöße

- Ergebnis `DOWN` mit der vorhandenen Fehlerklasse `ssrf_blocked` und der Meldung `blocked by local probe policy:
  <target|method|type>`. Ziel-IP und Port erscheinen bei privaten Probes nicht in der Meldung. Keine neue Fehlerklasse,
  also keine Proto-Änderung.
- Metrik `pulsequorum_probe_policy_blocked_total{reason="target|method|type"}`.
- Beim Start protokolliert die Probe die wirksame Richtlinie lokal (`info`). Die Einträge sind Konfiguration des
  Kunden und werden nur lokal geloggt, nie an den Cluster gesendet.

### 5. Sichtbarkeit

- Capability `target-policy`, wenn `PROBE_ALLOW_TARGETS` gesetzt und nicht `any` ist. Die Probe-Ansicht im UI zeigt
  „Zielrichtlinie aktiv“ bzw. „keine Zielrichtlinie (`any`)“.
- Der Cluster nutzt die Capability **nur zur Anzeige**. Er lockert und verschärft nichts danach. Die Durchsetzung bleibt
  allein lokal.

### 6. Container-Härtung (`deploy/probe/compose.yaml`, `docs/betrieb/probe.md`)

- `read_only: true`, `tmpfs: [/tmp]`, Daten nur im Volume `/data`.
- `cap_drop: [ALL]`, `security_opt: [no-new-privileges:true]`, `pids_limit`, vorhandenes `mem_limit`.
- ICMP weiter über `net.ipv4.ping_group_range` ohne Capability (wie heute).
- `PROBE_ALLOW_TARGETS` als Pflichtfeld im Beispiel mit Platzhalter, `PROBE_ALLOW_HTTP_METHODS` und
  `PROBE_RESULT_DETAIL` kommentiert.
- Doku: Egress der Probe auf die Voter-Ports und die erlaubten Ziele beschränken (Beispiele für `nftables` und
  Kubernetes-`NetworkPolicy`), Einordnung von `any` und `minimal`, das Restrisiko der Namensauflösung.

### 7. Was sich nicht ändert

- Keine Änderung an FSM-Befehlen (Befehlsstufe bleibt `10`), Protobuf-Verträgen oder OpenAPI.
- Prüfung beim Speichern und `SSRF_PROTECTION` bleiben wie in ADR 0035. Die neue Richtlinie liegt zusätzlich darüber.
- Gilt für das Haupt-Image mit `NODE_ROLE=probe` und, falls ADR 0043 angenommen wird, gleich für `pulsequorum-probe`.

## Konsequenzen

- **Positiv:** Der Kunde begrenzt die Probe selbst auf freigegebene Ziele und lesende Zugriffe. Ein kompromittierter
  Cluster oder ein gestohlenes Mandanten-Token kann die Probe nicht mehr frei im Kundennetz einsetzen. Der Standardweg
  im Compose-Beispiel ist gehärtet. Das ist eine wirksamere Sicherheitsmaßnahme für private Probes als ein schlankeres
  Image (ADR 0043).
- **Negativ / Risiken:**
  - Ein Pflichtfeld mehr beim Einrichten privater Probes.
  - Monitore können nach einer Richtlinienänderung auf der Probe plötzlich fehlschlagen. Die Meldung muss deshalb klar
    auf die lokale Richtlinie zeigen.
  - Innerhalb der freigegebenen Ziele bleibt bei `full` das Auslesen über Prüfergebnisse möglich. Das ist dokumentiert,
    `minimal` ist die Gegenmaßnahme.
  - Die Namensauflösung über den System-Resolver bleibt als kleines Restrisiko.
- **Folgearbeiten:**
  - Nach `Accepted`: Umsetzung als **AP-5.2c**, vorrangig vor AP-5.2b.
  - **Sicherheitscode schreibt der Hauptagent** (CLAUDE.md Regel 1): Parser und `AddressPolicy` der Zielrichtlinie,
    Verknüpfung mit `probe.Policy`, Prüfung von Methode und Typ vor der Ausführung, Reduktion bei `minimal`,
    Startabbruch.
  - An Haiku-Agenten: Tabellentests (Parser, IPv4-mapped, Zonen, Grenzen von Portbereichen, immer gesperrte Bereiche,
    Redirect auf nicht erlaubtes Ziel, DNS-Rebinding, ICMP, DNS-Antworten, `minimal`), Metrik, Capability und
    UI-Anzeige, gehärtetes Compose-Beispiel mit Starttest (auch ICMP und Puffer bei `read_only`), Doku.
  - Ende-zu-Ende-Test mit Podman ohne Internet (wie `deploy/ssrf-dev/`): erlaubtes Ziel UP, nicht erlaubtes Ziel,
    Redirect und `POST` mit Richtlinienfehler.
  - Pflichtenheft: neue Anforderungen **FA-PRB-08** (lokale Zielrichtlinie, Pflicht für private Probes),
    **FA-PRB-09** (lokale Beschränkung von Methoden und Typen), **FA-PRB-10** (reduzierte Ergebnisdetails). Roadmap.

## Approval

| Datum      | Status   | Durch  | Anmerkung |
|------------|----------|--------|-----------|
| 2026-11-22 | Proposed | Claude | Wartet auf Review durch Projektinhaber |
