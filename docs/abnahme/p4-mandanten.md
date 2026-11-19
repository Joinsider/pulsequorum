# Abnahme P4 – Mandanten (AP-4.6)

**Stand:** 2026-11-19, gemessen auf `7d88107` (Branch `feat/AP4.6`).

**Messumgebung:**
- Apple M4 Pro (Notebook), Podman 6.2 rootless in einer VM mit 4 CPUs und 7,5 GB, Go 1.27.4.
- **AS-06:** ein Voter (`bin/uptimecluster`, Modus `oidc`, Profil `zitadel`, `OIDC_TENANCY=multi`, `OIDC_AUTO_PROVISION=true`) direkt auf dem Host gegen Zitadel v4.20.3 aus `deploy/oidc-dev/zitadel`. Mandant A ist die Organisation `acme`, Mandant B `globex`; beide melden sich über ihre Maschinenbenutzer `acme-admin-bot` und `globex-admin-bot` (Rolle `tenant-admin`, Client Credentials) an.
- **AS-07:** die Umgebung `deploy/ssrf-dev` (ein Voter im Podman-Netz, eigener DNS-Server, interner Canary, Netz `203.0.113.0/24` als „Internet“), siehe [docs/betrieb/ssrf.md](../betrieb/ssrf.md).

**Aufruf:**

```bash
# AS-06 (Anleitung: test/acceptance/as06/README.md)
deploy/oidc-dev/zitadel/setup.sh
set -a; source deploy/oidc-dev/zitadel/.generated/uptimecluster.env; set +a
OIDC_AUTO_PROVISION=true DATA_DIR=… bin/uptimecluster &
deploy/oidc-dev/zitadel/token.sh acme-admin-bot   > "$dir/a.tok"
deploy/oidc-dev/zitadel/token.sh globex-admin-bot > "$dir/b.tok"
go run ./test/acceptance/as06 -base http://127.0.0.1:8080 -token-a-file "$dir/a.tok" -token-b-file "$dir/b.tok"

# AS-07
deploy/ssrf-dev/setup.sh on  && deploy/ssrf-dev/check.sh on
deploy/ssrf-dev/setup.sh off && deploy/ssrf-dev/check.sh off
deploy/ssrf-dev/down.sh
```

## Ergebnis

| ID | Kriterium | Gemessen | Ergebnis |
|----|-----------|----------|----------|
| AS-06 | Zwei Mandanten über Zitadel; A versucht, Monitor von B zu lesen oder zu ändern (ID-Manipulation): 404, kein Datenleck in API und WebSocket | Treiber `test/acceptance/as06`, 2 Läufe, alle Zeilen bestanden (erster Lauf 44, zweiter mit Gegenprobe 49 Zeilen). Mit den IDs des anderen Mandanten antworten Lesen, Ändern, Löschen, Pause, Fortsetzen, Klonen und Test des Monitors, Heartbeats und Uptime, Kanal (Lesen, Ändern, Test, Löschen), Wartungsfenster (Lesen, Ändern, Löschen) und Widerruf des API-Tokens mit `404`. Incidents des fremden Monitors: `200` ohne Einträge. Massenaktion mit fremder ID: `422` (Vertrag: unbekannte ID mit Position). Keine Antwort enthält Namen, IDs oder Mandanten-ID des anderen Mandanten. Listen (Monitore, Kanäle, Wartung, Incidents, API-Tokens, Audit-Log) zeigen nur eigene Daten. Die Objekte des angegriffenen Mandanten sind danach unverändert. WebSocket: Der angreifende Mandant erhielt 3 Nachrichten zu seinem eigenen Monitor (Gegenprobe) und keine mit Daten des anderen | erfüllt |
| AS-07 | HTTP-Monitor auf `http://169.254.169.254/` bzw. per DNS-Rebinding auf 127.0.0.1: abgelehnt bzw. DOWN mit Fehlerklasse `ssrf_blocked` | `check.sh on`: 24/24 bestanden. IP-Literal `169.254.169.254` beim Speichern `422` mit Feld `/config/url`; Name auf `127.0.0.1` und Weiterleitungen auf Metadaten-IP und `127.0.0.1:8080` DOWN `ssrf_blocked`; DNS-Rebinding (wechselnd öffentlich und intern): 10 Ergebnisse, 5 UP, 5 `ssrf_blocked`, kein Treffer am internen Canary. Gegenprobe `check.sh off`: 22/22, dieselben Ziele sind erreichbar | erfüllt (Einschränkung 2) |
| FA-UI-05 | Admin-Bereich mit Route-Guard: Cluster, Join-Tokens, Mandanten, Systemalerts | Seiten Cluster, Mandanten, Pläne, Systemkanäle seit AP-2.x/4.2; neu das Plattform-Audit-Log `/admin/audit`. Zusätzlich für Mandanten-Admins: API-Tokens `/api-tokens`, Audit-Log `/audit`, Massenaktionen im Dashboard. Sichtprüfung im Browser (lokaler Voter, Modus `local`), siehe unten | erfüllt (Einschränkung 3) |

### Sichtprüfung der neuen Oberflächen

Lokaler Voter im Modus `local`, vier Monitore, Browser mit 1400 × 900 Pixeln:

- **Massenaktionen:** Zwei Monitore gewählt, „Alle sichtbaren“ zeigt den Zwischenzustand. „Pause“ setzt beide auf `Paused`, Meldung „2 monitor(s) paused“, Auswahl danach leer.
- **API-Tokens:** Dialog mit Name, Rolle und Gültigkeit. Das Geheimnis erscheint einmal mit Kopierknopf und Hinweis. Danach enthalten `localStorage`, `sessionStorage` und die URL kein `uct_`.
- **Audit-Log:** Einträge für `monitor.create`, `monitor.pause` (beide mit derselben Request-ID) und `api_token.create`. Die Plattform-Ansicht zeigt zusätzlich die Spalte Mandant.

## Befunde und Korrekturen

1. **Gekürzte Request-IDs waren nutzlos:** Massenaktionen tragen IDs wie `api:default:<uuid>`. Die Tabelle zeigte die ersten acht Zeichen, also für alle `api:defa`. Jetzt wird der Teil nach dem letzten Doppelpunkt gekürzt (`49c59c9`).
2. **Gegenprobe für den Stream fehlte:** Der erste Lauf von AS-06 bestand mit 0 Nachrichten im Stream des Angreifers. Das wäre auch bei einem defekten Stream bestanden worden. Der Treiber prüft jetzt zusätzlich, dass der Angreifer Ereignisse seines eigenen Monitors erhält.

## Einschränkungen

1. **Massenaktion mit fremder ID antwortet 422, nicht 404.** Das entspricht dem Vertrag (`/monitors/bulk`: unbekannte ID mit Position) und verrät nichts, weil eine fremde ID wie eine nicht existierende behandelt wird. Die Antwort enthält keine Daten des anderen Mandanten.
2. **Rebinding auf ein internes Netz, nicht auf 127.0.0.1:** Die Zone in `deploy/ssrf-dev` wechselt zwischen einer öffentlichen und einer internen Adresse (`10.99.0.10`). 127.0.0.1 ist über einen festen Namen und eine Weiterleitung geprüft (Fälle e und f4). Der Filter behandelt beide Bereiche gleich (ADR 0030).
3. **Sichtprüfung nur im Modus `local`:** Die neuen Seiten wurden nicht mit einer OIDC-Anmeldung im Browser geprüft. Die Endpunkte dahinter sind über AS-06 mit Zitadel-Tokens abgedeckt.
4. **Akteur der Ersteinrichtung:** Der Eintrag `user.create` der Ersteinrichtung zeigt den Akteur „Unknown“. Das ist bekannt (AP-4.4: Befehle vor Stufe 9 und beim Bootstrap) und betrifft die Abnahme nicht.
5. **Einzelmessungen:** AS-06 lief zweimal, AS-07 je einmal mit und ohne Schutz.
