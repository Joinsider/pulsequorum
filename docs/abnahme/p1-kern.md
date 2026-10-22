# Abnahme P1 – Kern (AP-1.15)

Stand: 2026-10-22, Commits `e491cce` (Lasttest) und `2c19703` (AS-10). Messumgebung: Apple M4 Pro (Notebook), Podman 6.1 in einer VM mit 4 CPUs / 7,5 GB; der Voter läuft als Container mit **`--cpus 2 --memory 1g`** (Pflichtenheft NFA-PERF-01: 2 vCPU / 1 GB). Werkzeuge: `test/load/loadtest`, `test/load/gendata`, `test/acceptance/as01`, `web/e2e` (Playwright).

## Ergebnis

| ID | Kriterium | Gemessen | Ergebnis |
|----|-----------|----------|----------|
| NFA-PERF-01 | 1.000 Monitore à 60 s, < 50 % CPU (2 vCPU) im Mittel | Ø 3,6 % von 2 vCPU (7,1 % eines Kerns), Spitze 53,6 % eines Kerns; 15 min | erfüllt |
| NFA-PERF-02 | RAM < 512 MB | max. 68 MiB | erfüllt |
| NFA-PERF-03 | API p95 < 200 ms: Monitorliste (1.000 inkl. Status) | p95 66,9 ms (vollständige Liste, 2 Seiten à 500) | erfüllt |
| NFA-PERF-03 | API p95 < 200 ms: Uptime-Abfragen bis 365 d | p95 1,6 ms | erfüllt (Datenbasis nur 15 min, siehe AS-10) |
| NFA-PERF-04 | Statuswechsel in der UI ≤ 2 s nach Commit | p95 17 ms, max. 71 ms (30 Wechsel über WebSocket) | erfüllt |
| NFA-PERF-07 | Image < 40 MB, statisch ohne CGO | 31,4 MB | erfüllt |
| NFA-USE-01 | Single-Node-Inbetriebnahme mit Compose < 5 min | bereit 7 s nach `compose up`, Healthcheck `healthy`; Anleitung `docs/betrieb/compose.md` | erfüllt |
| AS-01 | DOWN-Alert über ntfy ≤ (retries+1) × Intervall + 30 s, Incident, Uptime < 100 % | Compose-Setup, Intervall 20 s, retries 1: Alert nach 40,9 s (Grenze 70 s), Incident offen und nach Recovery beendet, Uptime 47,6 % | erfüllt |
| AS-10 | 30 d Daten bei 1.000 Monitoren, ≤ 4 GB je Voter, API p95 < 200 ms | 2,2 GB; Monitorliste p95 96 ms, Uptime p95 2 ms; Retention-Lauf ohne Fehler (nach ADR 0016, AP-1.17) | erfüllt |
| E2E | Kernabläufe der SPA (Pflichtenheft 13.1) | 6/6 Playwright-Tests gegen das eingebettete Binary, zweimal grün | erfüllt |

## Befunde und Korrekturen während der Abnahme

1. **Monitorliste zu langsam** (p95 200 ms im ersten Lauf): je Monitor zwei Abfragen (letzter Heartbeat, Uptime 24 h). Behoben durch Batch-Abfragen je Seite (`e491cce`, Benchmark 500 Monitore: 854 ms → 26 ms).
2. **Abfrageplan der Heartbeat-Seiten**: Die Keyset-Bedingung als `OR`-Kette verhinderte die Nutzung des Zeitindex als Untergrenze; jede Seite las alle Rohdaten des Monitors. Behoben (`d208eb5`), mit Plan-Test.
3. **Falsche DOWN-Meldungen im ersten Lauf** (≈ 420 in 20 min) und Ausreißer bei der Statuslatenz (bis 48 s): verursacht durch die Messumgebung – das Zielsystem lief auf dem macOS-Host, Podmans Netzwerk-Proxy (gvproxy) verwarf SYNs bei ≈ 17 neuen Verbindungen/s (66 Sockets in `SYN-SENT`, keine Lecks im Voter: 90 Dateideskriptoren). Mit der Zielfarm im selben Container-Netz: 0 falsche DOWN, Latenz ≤ 71 ms. Die Anleitung steht in `test/load/loadtest/README.md`.
4. **Compose-Datei**: nicht existierende Variable `STORAGE_BACKEND` entfernt; Healthcheck in die Compose-Datei, weil Podman ihn aus OCI-Images verwirft.

## AS-10: Speicherbedarf

**Vor ADR 0016** (gemessen mit `gendata`, 50 Monitore, 10 Tage, `dbstat`): Latenz-Minuten 224 B/Zeile, Rohdaten 320 B/Zeile, Rollups ≈ 210 B/Zeile → Hochrechnung 1.000 Monitore × 30 Tage ≈ 10 GB.

**Nach ADR 0016** (AP-1.17, kompaktes Schema), voller Lauf mit 1.000 Monitoren à 60 s und 30 Tagen (43,2 Mio. Heartbeats, Uptime-Job tageweise mit Verdichtung, Laufzeit 1 h 25 min):

| Tabelle inkl. Indizes | Zeilen | Größe | Bytes je Zeile |
|---|---|---|---|
| `minutes` (Latenz-Minuten) | 40,3 Mio. | 1.173 MiB | 30,5 |
| `heartbeats` (Rohdaten ≥ 48 h) inkl. 3 Indizes | 3,05 Mio. | 981 MiB | 337 |
| `rollups` | 1,5 Mio. | 59 MiB | 40 |
| `state.db` | – | 1 MiB | – |
| **Summe** | | **2,2 GB** | |

Danach Voter auf diesen Daten gestartet (nativ, 10 min Messung, 1.000 aktive Monitore): bereit nach 3 s, erster Retention-Lauf löschte 86.345 abgelaufene Zeilen in Batches, Monitorliste p95 96 ms, Uptime-Abfragen (24 h–365 d, zufällig) p95 2 ms, RAM max. 246 MiB.

Vorgehen und Grenzen der Messung: Monitore wurden über die API angelegt; als Testdaten-Vorbereitung wurden ihre wenigen echten Heartbeats gelöscht und das Erstellungsdatum per SQL um 31 Tage zurückdatiert, damit die erzeugte Historie zählt. Die Konsens-Statushistorie liegt in `state.db` und wird von `gendata` nicht erzeugt; Konsens-Uptime über den erzeugten Zeitraum ist daher „unbekannt“, die Abfragekosten sind dieselben. Rollups wachsen über die Standard-Aufbewahrung von 365 Tagen auf ≈ 0,7 GB.

## Nicht gemessen

- NFA-PERF-01 Variante „300 Monitore à 20 s“ (geringere Last als 1.000 à 60 s: 15 statt 16,7 Checks/s).
- Probe-Speicher (NFA-PERF-02, zweiter Teil) – Probes kommen in P5.
