# Abnahme P2 – Cluster (AP-2.9)

**Stand:** 2026-11-15, Commits `1137552`, `3229537`, `ae926d8` (Harness und Szenarien), `baa3825` (Fix Signierschlüssel) und `b5ecb9e` (Uhrversatz, ADR 0020).

**Messumgebung:**
- Apple M4 Pro (Notebook), Podman 5.8 rootless in einer VM mit 4 CPUs und 7,5 GB.
- Voter als Container im selben Podman-Netz; Netzfehler über eine Sidecar im Netz-Namespace jedes Voters (iptables, `tc netem`).
- Monitore: HTTP gegen ein Fake-Ziel auf dem Host, Intervall 20 s, retries 1, Zuweisung `all`; Benachrichtigung über einen Fake-ntfy-Server.

**Werkzeug:** `test/chaos` (README dort). Uhrversatz läuft nicht im Container, sondern als Integrationstest `internal/clusterhost/skew_test.go` (ADR 0020).

## Ergebnis

| ID | Kriterium | Gemessen | Ergebnis |
|----|-----------|----------|----------|
| AS-02 | Zwei Voter treten bei: 3 Voter, keine Unterbrechung, identische Daten | 3 Voter, ein Leader, keine Warnungen; größte Lücke der Heartbeats von Voter 1 während der Joins 20 s (Grenze 40 s); Heartbeats aller 3 Erzeuger auf jedem Voter; Snapshots gleich; Schreiben über Voter 3 auf Voter 1 und 2 sichtbar | erfüllt |
| AS-03 | Leader hart beenden: neuer Leader < 10 s, kein doppelter oder verlorener Alert | SIGKILL kurz vor dem fälligen DOWN: Wahl 1,5 s (3 Voter) bzw. 1,8 s (5 Voter); je Monitor genau ein DOWN und ein UP; zweiter Failover 2,1 s bzw. 1,6 s ohne weitere Alerts; Snapshots nach Wiederbeitritt gleich | erfüllt |
| AS-09 | Rolling Update aller Voter: keine Unterbrechung von Checks und Alerts | *n* aus `HEAD`, *n+1* aus dem Arbeitsbaum, Reihenfolge 2, 3, 1; Update 2 min 20 s; größte Lücke auf laufenden Votern 20 s (Grenze 40 s); Zielausfall während des Updates: je Monitor genau ein DOWN und ein UP; danach alle Voter auf *n+1*, Befehlsversion 2 vorher und nachher, Snapshots gleich | erfüllt (Einschränkung 1) |
| NFA-REL-01 | ⌊(n−1)/2⌋ Ausfälle ohne Verlust von Checks, Alerts, Lese- und Schreibzugriff | Ein Follower von 3 beendet: Anlegen, Ändern, Löschen über die Überlebenden 2–132 ms; größte Heartbeat-Lücke 20 s; Alerts siehe AS-03 | erfüllt |
| NFA-REL-02 | Neuer Leader < 10 s bei ≤ 100 ms Latenz | 100 ms ± 10 ms zwischen allen Votern: 3,6 s und 5,2 s; Partition 2\|1: 4,1 s | erfüllt |
| NFA-REL-04 | Kein Verlust bestätigter Änderungen beim Ausfall einer Minderheit | Alle vor dem Ausfall bestätigten Änderungen (Monitor, Wartungsfenster) nach dem Neustart auf allen Votern; Schreiben über die isolierte Minderheit: 503, nach der Heilung nirgends vorhanden | erfüllt |
| NFA-REL-05 | Kein Verlust von Heartbeats, die ein Überlebender empfangen hat | Alle 12 Heartbeats des beendeten Nodes, die die Überlebenden vor dem Ausfall hatten, danach auf allen Votern | erfüllt |
| FA-CLU-08 | Knoten ersetzen; Runbook im Chaos-Test | Join unter belegter Adresse abgelehnt (Log: `address is already used by node <alte ID>`); Entfernen über einen Follower; Neubeitritt mit neuer Node-ID in 3 s; alte Kopie des Datenverzeichnisses bleibt ausgesperrt (`bad certificate`). Runbook Quorumverlust vollständig: `raft-info` wählt den Node mit Index 32 statt 31, `recover`, Sperre der alten Mitglieder nach 2 s, zwei Neubeitritte, veraltete Kopie stört nicht, `reset-password` | erfüllt |
| ADR 0020 | Uhrversatz | Integrationstest mit 0 s, +90 s, −90 s: Cluster bildet sich, Schreiben über jeden Node; gemessene Abweichung 90.000 bzw. −89.999 ms (gefordert ±1 s). Mit +6 min laufen bestehende Verbindungen weiter; ein Zertifikat wird abgelehnt, solange die Uhr des Prüfers mehr als 5 min hinter der des Ausstellers liegt; JWT-Toleranz ±30 s | erfüllt (Grenzen in `docs/betrieb/cluster.md`, Abschnitt Uhrzeit) |

## Befunde und Korrekturen während der Abnahme

1. **Neu beigetretene Voter ohne Signierschlüssel:**
   - **Symptom:** Ein Voter, der beim Start noch nicht aufgeholt hatte, fand keinen Schlüssel für Access-Tokens. Jede Anmeldung auf ihm endete bis zur stündlichen Wartung mit 500. Zusätzlich gab er einen Setup-Token aus. Den hätte die FSM abgelehnt, er war aber irreführend.
   - **Häufigkeit:** In 3 von 4 AS-02-Läufen.
   - **Behoben** (`baa3825`):
     - Nur der Leader erzeugt Schlüssel und den Setup-Token.
     - Eine Anmeldung ohne Schlüssel lädt neu.
     - Die Wartung versucht es alle 5 s, solange kein Schlüssel geladen ist.
   - **Nachtest:** AS-02 ohne Neustart-Workaround; Anmeldung auf Voter 2 und 3 sofort erfolgreich.
2. **Entfernen über Follower** (in AP-2.8 behoben): Ohne die Weiterleitung an den Leader wäre der Ersatz eines Nodes über die UI nur auf dem Leader möglich gewesen. Der Replace-Lauf entfernt über einen Follower.
3. **Doku:**
   - **`upgradePending`:** Die Betriebsdoku behauptete `upgradePending: true` für jedes Update. Laut ADR 0018 gilt das nur bei unterschiedlichen Befehlsversionen. Korrigiert in `c3dc807`.
   - **Anmeldung ohne Quorum:** Dass eine Anmeldung ohne Quorum mit 503 endet, steht jetzt im Recovery-Runbook.

## Einschränkungen

1. **AS-09 mit gleicher Befehlsversion:** *n* und *n+1* unterscheiden sich nur in der Softwareversion, weil es noch keinen Release mit neuer Befehlsversion gibt. Das Anheben der Befehlsversion während eines Updates ist durch den Integrationstest `TestRollingUpdate` (AP-2.6) abgedeckt. Der Upgrade-Test *n → n+1* über echte Releases folgt mit der Release-Pipeline.
2. **Latenz nur zwischen Containern:** Latenz und Partitionen wirken nur auf den Verkehr zwischen den Containern. Rootless Podman leitet Host-Zugriffe auf veröffentlichte Ports an `netem` vorbei.
3. **Alerts der Minderheit:** In der Partition lässt sich die Zahl der DOWN-Alerts nicht der Mehrheit oder Minderheit zuordnen. Geprüft ist, dass nach der Heilung kein DOWN wiederholt wird. Bestätigte und unbestätigte Alerts der Minderheit gehören zu AS-04 (P3).
4. **Heartbeat-Lücken an der Grenze:** Ein verpasster Heartbeat-Durchlauf erreicht die Grenze von 2 × Intervall. In einem frühen AS-09-Lauf mit kürzerem Messfenster lag eine Lücke beim Neustart des Leaders genau bei 40 s.
5. **Nur Podman geprüft:** Docker ist im Harness verdrahtet, aber nicht geprüft.
6. **Einzelmessungen:** Jede Wahlzeit ist eine Messung je Lauf. Der höchste Wert war 5,2 s unter Latenz.
