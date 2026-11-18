# SSRF-Testumgebung (AS-07, ADR 0030)

Reproduzierbare, lokale Ende-zu-Ende-Umgebung für die Abnahme des SSRF-Schutzes (Pflichtenheft FA-SEC-SSRF, AS-07; Entscheidung in [ADR 0030](../../docs/adr/0030-ssrf-durchsetzung.md); Betrieb in [docs/betrieb/ssrf.md](../../docs/betrieb/ssrf.md)).

Ein echter UptimeCluster-Node läuft in einem Podman-Netz. Ohne Internetdienst zeigt er, dass der Schutz interne Ziele blockiert, auch über DNS-Namen, Weiterleitungen und DNS-Rebinding.

> **Nur Entwicklung und Abnahme.** Die Umgebung nutzt feste Testnamen, legt einen Admin mit zufälligem Passwort an und hat keinen Bezug zu einem produktiven System.

## Idee

Der Filter (`checker.BlockPrivate`, `internal/checker/dialer.go`) sperrt keine Adressen aus dem Dokumentationsbereich `203.0.113.0/24` (TEST-NET-3). Das Netz `ssrf-pub` mit diesem Subnetz wirkt deshalb wie das öffentliche Internet. Interne Ziele liegen im Netz `ssrf-int` (`10.99.0.0/24`), das der Filter sperrt.

## Topologie

| Container | `ssrf-pub` (203.0.113.0/24) | `ssrf-int` (10.99.0.0/24) | Rolle |
| --- | --- | --- | --- |
| `ssrf-dns` | 203.0.113.53 | – | Autoritativer DNS für die Zone `ssrf.test.` auf UDP und TCP Port 53 |
| `ssrf-redirect` | 203.0.113.10 | – | „Öffentlicher“ HTTP-Server mit Weiterleitungen auf interne Ziele, `/hit` antwortet 200 |
| `ssrf-target` | – | 10.99.0.10 | Interner Canary: beantwortet jede Anfrage mit 200 und protokolliert sie |
| `ssrf-node` | 203.0.113.20 | 10.99.0.20 | UptimeCluster als einzelner Voter, `127.0.0.1:18080` auf dem Host |

Der Node nutzt `ssrf-dns` als einzigen Resolver (`--dns 203.0.113.53`). Namen außerhalb von `ssrf.test.` beantwortet der DNS-Server mit REFUSED, unbekannte Namen darin mit NXDOMAIN.

### DNS-Zone `ssrf.test.`

| Name | Antwort |
| --- | --- |
| `public.ssrf.test` | A 203.0.113.10 |
| `internal.ssrf.test` | A 10.99.0.10 |
| `loop.ssrf.test` | A 127.0.0.1 |
| `meta.ssrf.test` | A 169.254.169.254 |
| `v6loop.ssrf.test` | AAAA ::1 |
| `rebind.ssrf.test` | A mit TTL 0, wechselt pro A-Abfrage zwischen 203.0.113.10 und 10.99.0.10 |

Jede Abfrage protokolliert `ssrfenv` auf der Standardausgabe des DNS-Containers (`podman logs ssrf-dns`).

## Voraussetzungen

- Podman (rootless, unter macOS mit laufender `podman machine`), `podman info` muss funktionieren.
- `go` (Version laut `go.mod`), `curl`, `jq`, `openssl`.
- Internetzugang für den einmaligen Pull von `docker.io/library/alpine:3.20`.

Die Skripte nutzen einfache `podman`-Befehle, kein Compose.

## Verwendung

Aus dem Repository-Root oder aus diesem Verzeichnis:

```bash
deploy/ssrf-dev/setup.sh on     # Binaries bauen, Netze und Container anlegen, Admin anlegen (Standard: on)
deploy/ssrf-dev/check.sh on     # alle Fälle ausführen, PASS/FAIL-Tabelle, Exit-Code 1 bei Fehlern

deploy/ssrf-dev/setup.sh off    # dieselbe Umgebung mit SSRF_PROTECTION=off
deploy/ssrf-dev/check.sh off

deploy/ssrf-dev/down.sh         # alles entfernen
```

`setup.sh` baut `ssrfenv` (Testdienste) und `uptimecluster` für `linux/<Host-Architektur>` mit `CGO_ENABLED=0` nach `.generated/bin/`. Der Node startet jedes Mal frisch (`--tmpfs /data`), damit Canary-Protokoll und Monitore bei jedem Lauf bei null beginnen. Der Admin wird aus dem einmaligen Setup-Token gleich nach dem Start angelegt. Der Benutzer ist `admin`, das Passwort steht in `.generated/admin.env` (`chmod 600`) und wird nie ausgegeben.

`check.sh` prüft zuerst, ob der laufende Node mit dem übergebenen Modus gestartet wurde, und bricht sonst ab.

Ein Lauf von `check.sh` dauert durch den Rebinding-Fall mehrere Minuten (10 geplante Checks im Abstand von 20 Sekunden).

## Was jeder Fall belegt

Alle Fälle legen einen Monitor oder Kanal an, warten auf das erste geplante Ergebnis (nicht auf den Test-Button) und löschen ihn danach wieder. Die Spalte „Erwartet“ in der Tabelle gilt für `on`; für `off` steht die Abweichung in der Tabelle.

| Fall | Ziel | Mit Schutz (`on`) | Ohne Schutz (`off`) | Was belegt wird |
| --- | --- | --- | --- | --- |
| a | HTTP `http://169.254.169.254/` | 422 am Feld `/config/url` | 201 | Metadaten-Adresse wird beim Speichern abgelehnt |
| b | HTTP `http://10.99.0.10/hit` | 422 am Feld `/config/url` | 201 | Private Adresse wird beim Speichern abgelehnt |
| c | HTTP `http://0x7f.1/` | 422 am Feld `/config/url` | 422 | Nicht kanonische Schreibweise wird immer abgelehnt |
| d | HTTP `http://internal.ssrf.test/hit` | DOWN `ssrf_blocked`, kein Canary-Treffer | UP, Canary-Treffer | Ein Name, der auf eine interne Adresse auflöst, wird am Verbindungsaufbau gesperrt |
| e | HTTP `http://loop.ssrf.test:8080/` | DOWN `ssrf_blocked` | kein `ssrf_blocked` | Name auf Loopback, auch der eigene Node ist gesperrt |
| f1 | HTTP `public.ssrf.test/to-internal` | DOWN `ssrf_blocked` | UP, Canary-Treffer | Weiterleitung auf eine IP im internen Netz |
| f2 | HTTP `public.ssrf.test/to-internal-name` | DOWN `ssrf_blocked` | UP, Canary-Treffer | Weiterleitung auf einen internen Namen |
| f3 | HTTP `public.ssrf.test/to-metadata` | DOWN `ssrf_blocked` | kein `ssrf_blocked` | Weiterleitung auf die Metadaten-Adresse |
| f4 | HTTP `public.ssrf.test/to-loop` | DOWN `ssrf_blocked` | kein `ssrf_blocked` | Weiterleitung auf `127.0.0.1:8080` |
| g | HTTP `public.ssrf.test/ok` | UP | UP | Positivkontrolle: das „öffentliche“ Netz ist erreichbar |
| h | HTTP `rebind.ssrf.test/hit`, 10 geplante Checks | UP und DOWN `ssrf_blocked` gemischt, kein Canary-Treffer | UP, keine Sperre | DNS-Rebinding: die Prüfung läuft bei jedem Verbindungsaufbau, nicht nur beim ersten Auflösen |
| i | DNS `internal.ssrf.test`, Systemresolver | DOWN `ssrf_blocked`; Meldung und Details ohne `10.99`; `details.resolver` = `system` | UP | DNS-Check verrät keine internen Adressen (ADR 0030, Frage 3) |
| j | DNS `public.ssrf.test` | UP | UP | Positivkontrolle für den DNS-Check |
| k | TCP `internal.ssrf.test:80` | DOWN `ssrf_blocked` | UP | Der Schutz gilt auch für TCP-Checks |
| l1 | Webhook `http://10.99.0.10/hook` | 422 an einem URL-Feld | 201 | Webhook auf eine interne IP wird beim Speichern abgelehnt |
| l2 | Webhook `http://internal.ssrf.test/hook`, Testversand | gespeichert, Zustellung fehlgeschlagen, kein Canary-Treffer | gespeichert, Zustellung erfolgreich, Canary-Treffer | Benachrichtigungen unterliegen demselben Schutz (FA-ALR-12) |
| m | Canary-Protokoll am Ende | 0 Zeilen | mindestens 1 Zeile | Gesamtbilanz: kein Zugriff auf interne Ziele, wenn der Schutz an ist |

Die Fälle `d/canary`, `f1/canary`, `f2/canary` und `l2/canary` zählen die Canary-Treffer während des jeweiligen Falls. Mit Schutz muss jeder Wert 0 sein, ohne Schutz mindestens 1. Der Node selbst meldet nie Canary-Treffer, weil er nur über den Dialer geht.

## Aufräumen

`down.sh` entfernt die Container `ssrf-node`, `ssrf-target`, `ssrf-redirect` und `ssrf-dns`, die Netze `ssrf-pub` und `ssrf-int` und das Verzeichnis `.generated/` (Binaries und Admin-Zugang). Andere Container und Netze bleiben unberührt.

Das Verzeichnis `.generated/` ist per `.gitignore` in diesem Verzeichnis ausgeschlossen.

## Dateien

| Datei | Zweck |
| --- | --- |
| `setup.sh` | Baut, startet, legt den Admin an |
| `check.sh` | Führt die Fälle aus und gibt die PASS/FAIL-Tabelle aus |
| `down.sh` | Entfernt Container, Netze und `.generated/` |
| `.gitignore` | Schließt `.generated/` aus |
| `../../test/ssrfenv/cmd/ssrfenv/` | Testdienste: DNS, Redirect und Canary |
