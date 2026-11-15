# Chaos-Testharness (AP-2.9)

Der Harness startet UptimeCluster-Voter als Container, fügt Voter hinzu und
entfernt sie, injiziert Netzfehler und prüft das Verhalten gegen das
Pflichtenheft. Er ist die Grundlage für die Szenarien AS-02 und folgende.
Die Akzeptanzszenarien laufen auf echten Containern; die Unit-Tests brauchen
keinen Container-Runtime.

## Voraussetzungen

- **podman** (rootless, Version 5.x getestet) oder **docker**. Der Driver
  wählt `podman`, wenn es im `PATH` liegt, sonst `docker`.
- Für podman auf macOS: die Maschine muss laufen (`podman machine start`).
  Der Driver prüft das mit `podman version` und bricht sonst ab.
- Go, wie im Repository (`go.mod`).
- Freie Host-Ports: `port-base+1` bis `port-base+Voter` (Standard
  18101 bis 18103), `fake-listen-base` und `fake-listen-base+1` (Standard
  18950 und 18951).

### Hinweise zu rootless podman

Getestet mit podman 5.8 auf macOS (`podman machine`, applehv, Kernel 6.18):

- Die Voter erreichen den Host unter `host.containers.internal`. Das ist der
  Standard für podman; docker bekommt stattdessen `host.docker.internal` mit
  `--add-host host.docker.internal:host-gateway`. **Docker wurde nicht
  getestet.**
- Die Fault-Sidecars laufen mit `--network container:<voter>` und
  `--cap-add NET_ADMIN`. In rootless podman funktionieren `iptables` und
  `tc netem` in dieser Netz-Namespace-Konstellation (geprüft).
- Das Datenvolumen `/data` wird vor dem ersten Start einmal als root im Container
  auf `65532:65532` gesetzt (`chown`), weil neue Volumes dem Benutzer gehören,
  der das Volume anlegt. Auf podman war das Volume auch ohne `chown` für
  Benutzer 65532 beschreibbar; der Schritt ist für docker gedacht, wo neue
  Volumes root gehören.
- Das Join-Token wird als Datei mit Modus 0644 in einem 0700-Verzeichnis
  erzeugt und nur als einzelne Datei read-only eingebunden. So kann der
  Benutzer 65532 es lesen, ohne dass das Verzeichnis lesbar ist.
- **netem wirkt nicht auf Host-Verkehr.** Ein Delay auf `eth0` eines Voters
  verändert nur Verkehr zwischen Containern (Raft, Anti-Entropy). Die HTTP-API
  über den veröffentlichten Port (`127.0.0.1:<port>`) wird im Test nicht
  verzögert (gemessen: 200 ms konfiguriert, Host-Antwort etwa 1 ms). Der
  Selbsttest misst die Latenz daher zwischen Voter 1 und Voter 2 und meldet
  die Host-Messung nur als Information.
- Das Kernel-Modul `sch_netem` ist in der Podman-Maschine vorhanden (Kernel 6.18, Modul im Modulverzeichnis gefunden); `tc netem` hat damit funktioniert.

## Aufruf

Aus dem Repository-Wurzelverzeichnis:

```bash
# alle Szenarien
go run ./test/chaos/cmd/chaos -report /tmp/chaos.md

# nur AS-02
go run ./test/chaos/cmd/chaos -scenarios as02

# Liste der Szenarien
go run ./test/chaos/cmd/chaos -list
```

Der Driver baut die Images, falls nötig: `uptimecluster:chaos` aus dem
`Dockerfile` (mit `--build-arg VERSION=<-version>`) und `uc-chaos-netem:latest`
aus `test/chaos/netem`. Der Ablauf dauert beim ersten Mal einige Minuten
(Frontend-Build im Dockerfile).

Die Ausgabe ist eine Markdown-Tabelle mit den Spalten Schritt, erwartet,
gemessen und Ergebnis. Der Exit-Status ist 1, wenn ein Schritt fehlschlägt.
Secrets (Admin-Passwort, Setup-Token, Join-Token) werden nie ausgegeben.

### Flags

| Flag | Standard | Bedeutung |
|------|----------|-----------|
| `-runtime` | `podman` wenn vorhanden, sonst `docker` | Container-Runtime (`podman` oder `docker`) |
| `-prefix` | `ucchaos` | Präfix aller Container, Volumes und des Netzes |
| `-image` | `uptimecluster:chaos` | Voter-Image |
| `-build` | `auto` | `auto` (nur wenn fehlend), `always`, `never` |
| `-version` | `chaos` | Wert von `VERSION` beim Image-Build |
| `-voters` | `3` | Voter-Plätze je Szenario (1 bis 5) |
| `-port-base` | `18100` | Voter *i* veröffentlicht HTTP auf `127.0.0.1:port-base+i` |
| `-fake-listen-base` | `18950` | Port des Fake-ntfy; das Fake-Ziel nutzt den nächsten Port |
| `-interval` | `20s` | Monitor-Intervall der Szenarien (20 s bis 24 h) |
| `-callback-host` | je Runtime | Name, unter dem Container den Host erreichen |
| `-scenarios` | `all` | Kommagetrennte IDs oder `all` |
| `-keep` | aus | Container, Volumes, Netz und Temp-Verzeichnis nach dem Lauf behalten |
| `-report` | leer | Markdown-Bericht zusätzlich in diese Datei schreiben |
| `-list` | aus | Szenarien anzeigen und beenden |

### Parallele Läufe

Zwei Läufe auf derselben Maschine brauchen eigene `-prefix`, eigene
`-port-base`- und `-fake-listen-base`-Bereiche. Ein Start räumt alle Objekte
mit dem eigenen Präfix auf, auch von einem früheren Lauf (`Purge`). Andere
Präfixe bleiben unberührt, auch wenn sie mit demselben Text beginnen.

## Szenarien

| ID | Quelle | Inhalt |
|----|--------|--------|
| `as02` | `cmd/chaos/as02.go` | AS-02: Voter 1 mit fünf HTTP-Monitoren, Voter 2 und 3 treten bei. Prüft: drei Voter, ein Leader, keine Warnungen; keine Lücke > 2 × Intervall in den eigenen Heartbeats von Voter 1 während der Joins; Heartbeats aller drei Produzenten auf jedem Voter nach etwa zwei Intervallen; identische Daten (Snapshots); Schreibzugriff über Voter 3 auf Voter 1 und 2 innerhalb von 5 s. |
| `as03` | `cmd/chaos/as03.go` | AS-03: Ziel fällt aus, der Leader wird kurz vor dem fälligen DOWN-Alert per SIGKILL beendet. Prüft: neuer Leader < 10 s, genau ein DOWN und ein UP je Monitor, Wiederbeitritt des beendeten Voters, identische Daten; zweiter Failover des neuen Leaders. Auch mit `-voters 5`. |
| `minority` | `cmd/chaos/minority.go` | NFA-REL-01/04/05: bestätigte Schreibzugriffe über einen Follower, dann fällt eine Minderheit aus. Prüft: Lesen und Schreiben über alle Überlebenden, keine Heartbeat-Lücke > 2 × Intervall, nach dem Neustart alle bestätigten Änderungen und alle Heartbeats des ausgefallenen Nodes, die ein Überlebender schon hatte. |
| `partition` | `cmd/chaos/partition.go` | Partition 2\|1 mit isoliertem Leader. Prüft: neuer Leader der Mehrheit < 10 s, Schreiben über die Mehrheit, Schreiben über die Minderheit 503 und nie bestätigt, Lesen auf der Minderheit (veraltet), nach der Heilung identische Daten und kein wiederholter DOWN-Alert. Bestätigte/unbestätigte Alerts der Minderheit folgen mit AS-04 (P3). |
| `latency` | `cmd/chaos/latency.go` | NFA-REL-02: 100 ms ± 10 ms Latenz zwischen allen Votern, zwei Leader-Ausfälle. Prüft: neuer Leader < 10 s, Schreiben unter Latenz. |
| `as09` | `cmd/chaos/as09.go` | AS-09: Image *n* aus `-from-ref` (Standard `HEAD`) per `git worktree`, Image *n+1* aus dem Arbeitsbaum. Rolling Update Follower zuerst, Leader zuletzt, mit Ausfall des Ziels mittendrin. Prüft: keine Lücke > 2 × Intervall auf den laufenden Votern, genau ein DOWN und ein UP je Monitor, danach alle Voter auf *n+1*, identische Daten. |
| `replace` | `cmd/chaos/replace.go` | FA-CLU-08 „Knoten ersetzen“ und Datenverzeichnis löschen: Join unter derselben Adresse wird abgelehnt (alte Node-ID im Log), Entfernen über einen Follower, Neubeitritt mit neuer Node-ID, Kopie des alten Datenverzeichnisses bleibt ausgesperrt. |
| `recovery` | `cmd/chaos/recovery.go` | Runbook `docs/betrieb/recovery.md`: Quorumverlust (2 von 3), `admin raft-info`, `admin recover` (Dry-Run, `--yes`), Sperre der alten Mitglieder, Neubeitritt zweier frischer Voter, veraltete Kopie stört nicht, `admin reset-password`. |
| `selftest` | `cmd/chaos/selftest.go` | Prüft die Fault-Werkzeuge: `Isolate` (Voter 1 erreicht Voter 3 nicht mehr, Replikation von Voter 3 bricht ab), `Heal` (Verbindung und Replikation kommen zurück), `Latency` (Anfragen von Voter 1 an Voter 2 werden um mindestens 150 ms langsamer), `ClearLatency`. |

Neue Szenarien: eine Datei im Paket `cmd/chaos` anlegen, die in `init` mit
`register(Scenario{ID, Label, Title, Run})` registriert. Die ID ist
eindeutig; doppelte IDs führen zu einem Panic beim Start.

## Aufbau

```
test/chaos/
  netem/Containerfile     Sidecar mit iptables und iproute2 (tc)
  harness/                Container-Lifecycle, Faults, Beobachtung, Berichte
  cmd/chaos/              Driver, Szenario-Registry, Szenarien
test/internal/fake/       ntfy-Recorder, HTTP-Ziel mit Hit-Zählern
test/internal/ucapi/      REST-Client der Admin-API
```

- **Runtime** (`harness/runtime.go`) kapselt die CLI. Jeder Aufruf hat einen
  Kontext und ein Timeout; Fehler enthalten Befehl und gekürzte Fehlerausgabe.
- **Cluster** (`harness/cluster.go`): je Voter *i* ein Container
  `<prefix>-v<i>`, ein Volume `<prefix>-v<i>-data`, ein Sidecar
  `<prefix>-v<i>-netem` und ein gemeinsames Netz `<prefix>-net`.
  - `Bootstrap`: Voter 1 startet allein, das Setup-Token wird aus dem Log
    gelesen, der Admin bekommt ein zufälliges Passwort (nur im Speicher).
  - `Join(i)`: Join-Token am Leader erzeugen, in die Temp-Datei schreiben,
    Voter *i* mit `CLUSTER_JOIN` starten, auf Gesundheit und
    Mitgliedschaft als Voter warten. Die Token-Datei wird danach gelöscht.
  - `Start`, `Stop`, `Kill`, `Restart`, `Remove`, `WipeData`, `RemoveMember`.
    `Start` erstellt den Container immer neu aus demselben Volume, weil ein
    Join-Container noch den Token-Mount hat, dessen Datei weg ist. Der
    Sidecar wird bei jedem Neustart des Voters neu erstellt.
  - `Teardown`: entfernt alles des Präfixes und das Temp-Verzeichnis, auch
    bei Fehlern (außer bei `-keep`).
- **Faults** (`harness/faults.go`) laufen im Sidecar desselben Voters:
  - `Isolate` / `IsolateOneWay` / `Heal`: `iptables -I INPUT|OUTPUT -s|-d <IP> -j DROP`
    für die IPs der anderen laufenden Voter, `Heal` flusht INPUT und OUTPUT.
    Der Host erreicht den veröffentlichten Port weiter, weil dieser Verkehr
    nicht von einem anderen Voter kommt.
  - `Latency` / `ClearLatency`: `tc qdisc replace dev eth0 root netem delay … loss …`.
  - `Reachable`: TCP-Verbindung aus dem Sidecar eines Voters zu einem anderen
    (`nc -z`), `TimeHTTP`: gemessene Dauer einer Anfrage zwischen Containern.
- **Beobachtung** (`harness/observe.go`, `snapshot.go`, `gaps.go`):
  - `Leader` / `LeaderOf` / `WaitLeader`: der Leader gilt nur, wenn alle
    erreichbaren Voter übereinstimmen.
  - `Snapshot`: normalisierte Sicht auf Monitore, Kanäle, Wartungsfenster,
    Benutzer und Mitglieder. Volatile Felder (Status, Zeitstempel der Prüfung,
    letzter Login, Replikationsmesswerte) fehlen, damit gleiche Daten gleiche
    Snapshots ergeben. `CompareSnapshots` listet die Unterschiede.
  - `HeartbeatGap` / `MaxGap`: größte Lücke zwischen Heartbeats eines
    Produzenten innerhalb eines Fensters. Ein Fenster ohne Heartbeats ergibt
    seine Länge.
- **Bericht** (`harness/report.go`): Markdown je Szenario, mit Gesamturteil.

## Grenzen

- **Keine Uhrzeitabweichung.** Container teilen sich die Uhr des Hosts. Zeitversatz
  wird nicht getestet; siehe ADR 0020 (Messung des Uhren-Offsets und Skew-Tests).
- **netem wirkt nur zwischen Containern** (siehe oben). Latenz zu Clients oder
  zum Host ist mit diesem Aufbau nicht prüfbar.
- **ICMP** ist in den Containern nicht verfügbar (Warnung im Log des Voters,
  `docs/betrieb/icmp.md`). Die Szenarien verwenden nur HTTP-Monitore.
- **Ein Lauf je Präfix.** Derselbe Präfix darf nicht parallel laufen.
- **Docker** ist eingebaut, aber nicht getestet.
- **Image-Stand.** Das Voter-Image wird aus dem aktuellen Arbeitsverzeichnis
  gebaut, inklusive unversionierter Änderungen. Für reproduzierbare Läufe das
  Repository vorher committen oder ein fertiges Image mit `-build never`
  verwenden.

## Bekannte Produktprobleme (Befunde aus den Läufen)

- **Signing-Key nach dem Join fehlt.** Ein Voter startet seinen HTTP-Server
  und die Anmeldung, bevor sein Raft-Zustand repliziert ist. Beim Start
  erzeugt `cmd/uptimecluster/main.go` den Signing-Key einmal; ist noch kein
  Datenschlüssel da (`no active data key`), wird er nie erneut erzeugt.
  Jede Anmeldung auf diesem Voter antwortet dann mit HTTP 500
  (`no signing key available`, `internal/auth/local.go`). In den Läufen
  betraf das Voter 2 oder 3 in den meisten, nicht in allen Läufen.
  AS-02 erkennt das und startet den betroffenen Voter neu. Der Neustart
  steht im Bericht als INFO-Schritt. Das ist eine Umgehung; der Fehler liegt
  im Produkt.
- **Setup-Token auf joinenden Nodes.** Ein beitretender Voter loggt ein Setup-Token,
  weil er vor der Replikation noch keinen Benutzer sieht. Das ist irreführend,
  weil der Cluster Benutzer hat.
