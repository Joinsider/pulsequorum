# ADR 0020: Uhrversatz zwischen Nodes messen und testen

| | |
|---|---|
| **Status** | Proposed |
| **Datum** | 2026-11-15 |
| **Entscheider** | Projektinhaber |
| **Bezug** | Pflichtenheft 13.1 (Cluster/Chaos), FA-PKI-03, FA-OBS-01, FA-UPT-02, Kap. 3 (NTP Pflicht), Kap. 9 Cluster-Ansicht, PF-11, ADR 0012, AP-2.9 |

## Kontext

Die Teststrategie (Pflichtenheft 13.1) verlangt Chaos-Tests mit 3/5 Votern in Docker, unter anderem mit **Clock-Skew**. Beim Entwurf des Harness für AP-2.9 hat sich gezeigt:

- **Container teilen die Wanduhr des Host-Kernels.** Linux-Zeit-Namespaces gibt es nur für `CLOCK_MONOTONIC` und `CLOCK_BOOTTIME`, nicht für `CLOCK_REALTIME`. Ein einzelner Container lässt sich also nicht verstellen.
- **libfaketime wirkt nicht.** Das Binary ist statisch gebaut (`CGO_ENABLED=0`, distroless). Go liest die Uhr per vDSO bzw. Systemaufruf, nicht über die libc.
- **Wanduhrabhängig sind im Code** unter anderem:
  - die Gültigkeit der Node-Zertifikate (`NotBefore` 5 min zurückdatiert, FA-PKI-03);
  - JWTs (30 s Toleranz), die jeder Node prüft, auch wenn ein anderer sie ausgestellt hat;
  - `IssuedAtMs` der Raft-Befehle (vom Leader gesetzt, die FSM übernimmt sie);
  - Zeitstempel der Heartbeats (vom Erzeuger gesetzt, FA-UPT-02);
  - Wartungsfenster (Leader);
  - die Replikationsposition.
  
  Raft selbst nutzt monotone Zeit.
- **Die Uhrzeitabweichung wird bisher nicht gemessen.** Das Konzept („Nodes tauschen ihre Uhrzeit aus; bei > 2 s Abweichung Warnung im UI“), die Cluster-Ansicht (Pflichtenheft Kap. 9) und der Systemalert in FA-OBS-01 setzen sie aber voraus. Die Admin-UI aus AP-2.8 zeigt sie deshalb noch nicht.

Die übrigen Chaos-Szenarien sind davon nicht betroffen: Node-Kill, Partition, Latenz, Datenverzeichnis löschen und Rolling Update gehen mit Compose. Sie sind reiner Testcode und werden in AP-2.9 ohne ADR umgesetzt.

## Optionen

### Option A: Uhrversatz im Prozess testen, Abweichung im Betrieb messen
- Komponenten mit Wanduhr bekommen ihre Uhr als `func() time.Time` über Konstruktoren und `clusterhost.Options.Clock`. Das sind die übrigen direkten `time.Now()`-Aufrufe außerhalb der Checker-Latenzmessung.
- Ein Integrationstest in `internal/clusterhost` startet 3 Voter im selben Prozess, jeder mit eigener versetzter Uhr.
- Im Betrieb liefert `GetStatus` zusätzlich die Wanduhr des Angefragten. Der Leader berechnet daraus die Abweichung jedes Nodes (Mitte der Laufzeit, NTP-artig) und gibt sie in der Node-Liste der Admin-API aus. Die Admin-UI zeigt sie an und warnt ab 2 s.
- Vorteile:
  - Läuft überall, auch in CI, deterministisch und schnell.
  - Keine Testschnittstelle im Produkt-Binary: keine Umgebungsvariable, kein Build-Tag.
  - Die Messung erfüllt die offene Anforderung aus Konzept und Pflichtenheft.
- Nachteile:
  - Kein Test über echte Netzwerk- und TLS-Stacks mit echter Kernel-Uhr.
  - Bibliotheken wie `crypto/tls` und gRPC lesen die Uhr selbst. Dafür setzt der Test die Uhr im Verifier über die vorhandenen Callbacks (`tls.Config.Time` bzw. `pki.Verifier`).
  - Der Umbau berührt viele Konstruktoren, ohne das Verhalten zu ändern.

### Option B: Umgebungsvariable für einen Uhrversatz, nur in einem Chaos-Build (`-tags chaos`)
- Vorteile: Echter Test im Container zusammen mit den übrigen Chaos-Szenarien.
- Nachteile:
  - Testcode im Produkt-Quelltext und ein zweites Image.
  - Bibliotheken, die die Uhr selbst lesen, sehen den Versatz trotzdem nicht. Der Test täuscht also eine Abdeckung vor, die er nicht hat.
  - Es bleibt eine globale Uhr nötig, im Widerspruch zu „kein globaler Zustand“.

### Option C: Je Voter eine eigene VM mit verstellter Uhr (z.B. je eine Podman-Machine, NTP aus)
- Vorteile: Vollständig realistisch.
- Nachteile:
  - Schwer, langsam und plattformabhängig.
  - In CI kaum umsetzbar.
  - Schlecht reproduzierbar.

## Entscheidung

Vorgeschlagen wird **Option A**.

**1. Uhr als Abhängigkeit**
- `clusterhost.Options.Clock func() time.Time` (Standard `time.Now`) wird an alle Komponenten durchgereicht, die die Wanduhr für Entscheidungen nutzen:
  - PKI-Ausstellung und -Prüfung;
  - JWT, Tickets und Rate-Limit;
  - `IssuedAtMs`;
  - Scheduler-Zeitstempel und Heartbeat-Writer;
  - Wartung;
  - Node-Gesundheit;
  - Replikation;
  - Uptime-Jobs.
- Latenzmessungen der Checker bleiben bei `time.Now()`/`time.Since`. Sie messen Dauern mit monotoner Zeit.

**2. Uhrzeitabweichung messen**
- `GetStatusResponse` bekommt `wall_clock_unix_ms` (neues Feld, rückwärtskompatibel).
- Der Leader fragt den Status ohnehin für Promotion und Replikationsanzeige ab. Er schätzt die Abweichung als `remote − (t0 + t1)/2` und verwirft Messungen mit einer Laufzeit über 1 s.
- Die Node-Liste der Admin-API bekommt `clockOffsetMs` (optional; der Leader selbst hat 0).
- Die Admin-UI zeigt den Wert und warnt ab 2 s (Konzept, FA-OBS-01).
- Der Systemalert dazu folgt mit AP-3.6.

**3. Test**
- Integrationstest in `internal/clusterhost` mit 3 Votern und folgenden Uhren:
  - Node B um +90 s versetzt, Node C um −90 s versetzt;
  - danach C um +6 min versetzt.
- Erwartet wird:
  - Der Cluster bildet sich bzw. bleibt bestehen, Schreiben über jeden Node funktioniert.
  - Ein auf A ausgestelltes Access-Token wird auf B nur innerhalb der dokumentierten Toleranz akzeptiert.
  - Die Admin-API zeigt die Abweichungen mit ±1 s Genauigkeit.
  - Bei 6 min Versatz scheitern neue Zertifikate von C erwartbar an `NotBefore`. Der Test hält die Fehlermeldung fest.
- Die Ergebnisse gehen als Toleranzangaben in die Betriebsdoku: NTP ist Pflicht, und es wird beschrieben, was ab welchem Versatz passiert.

## Konsequenzen

- **Positiv:**
  - Clock-Skew ist reproduzierbar getestet, ohne Testschnittstellen im Binary.
  - Die geforderte Anzeige der Uhrzeitabweichung kommt hinzu und schafft die Grundlage für den Systemalert (AP-3.6).
- **Negativ / Risiken:**
  - Abweichung von Pflichtenheft 13.1: Clock-Skew läuft nicht im Container-Chaos-Test, sondern als Integrationstest.
  - Uhrfehler, die nur in Bibliotheken außerhalb der injizierten Pfade wirken, bleiben ungetestet.
  - Viele Konstruktoren ändern sich, das kann Konflikte mit paralleler Arbeit geben.
- **Folgearbeiten:**
  - Pflichtenheft 13.1 präzisieren (Clock-Skew → Integrationstest, ADR 0020).
  - Systemalert „Uhrzeitabweichung > 2 s“ in AP-3.6.

## Approval

| Datum | Status | Durch | Anmerkung |
|-------|--------|-------|-----------|
| 2026-11-15 | Proposed | Claude | |
