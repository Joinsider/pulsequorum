# ADR 0019: Recovery-CLI – Quorumverlust und Admin-Passwort

| | |
|---|---|
| **Status** | Proposed |
| **Datum** | 2026-10-25 |
| **Entscheider** | Projektinhaber |
| **Bezug** | FA-OBS-04, FA-CLU-08, FA-PKI-04, PF-06, ADR 0005, ADR 0006, ADR 0008, ADR 0012 A5 |

## Kontext

FA-OBS-04 verlangt CLI-Unterbefehle im Binary für Recovery-Aufgaben. Genannt sind ausdrücklich der Peers-Override und das Zurücksetzen des Admin-Passworts. FA-CLU-08 und PF-06 verlangen ein dokumentiertes Verfahren für den Quorumverlust, das ohne automatisches Entfernen von Votern auskommt.

Ausgangslage:

- Das Binary kennt heute nur `version` und `healthcheck`.
- **Quorumverlust:** Fallen in einem Cluster mit 3 Votern 2 dauerhaft aus, gibt es keinen Leader mehr. Schreibzugriffe und Mitgliedschaftsänderungen sind unmöglich, auch das Entfernen der toten Voter. hashicorp/raft bietet dafür `raft.RecoverCluster`: Es schreibt offline eine neue Konfiguration in Log und Snapshot eines gestoppten Nodes. Das ist von Natur aus unsicher:
  - Einträge, die nur auf den verlorenen Nodes committet waren, gehen verloren.
  - Einträge, die auf dem Überlebenden nie committet waren, werden committet.
  - Laufen danach Nodes mit alten Zuständen weiter, droht Split-Brain.
- `RecoverCluster` wendet beim Kompaktieren alle Logeinträge auf die übergebene FSM an. Unsere FSM schreibt direkt in `state.db`. Beim Recovery wird `state.db` also wie bei einem normalen Neustart nachgezogen.
- **Admin-Passwort:** Lokale Nutzer liegen im Raft-Zustand (ADR 0008). Ein Passwort lässt sich nur über einen Raft-Befehl ändern; `state.db` direkt zu ändern würde die Replikate auseinanderlaufen lassen. Die Erst-Einrichtung über den Setup-Token greift nur, solange es gar keinen Nutzer gibt.

## Optionen

### Quorumverlust

**Option A: Offline-Befehl `admin recover` auf genau einem Überlebenden, die übrigen treten neu bei**
- Der Betreiber wählt den Überlebenden mit dem neuesten Log (`admin raft-info` zeigt letzten Index und Term). Nur dort läuft `RecoverCluster`, mit diesem Node als einzigem Voter. Alle anderen Nodes werden geleert und treten als neue Nodes mit neuen Tokens bei.
- Vorteile:
  - Es gibt genau einen gültigen Zustand, kein Abgleich unterschiedlicher Logs.
  - Der Ablauf passt zum bestehenden Join, die Methode empfiehlt auch die hashicorp-Dokumentation.
  - Gut testbar.
- Nachteile:
  - Die übrigen Überlebenden verlieren ihre lokalen Daten und holen den Zustand per Snapshot neu.
  - Ihre lokal gespeicherten Heartbeats (Klasse B) gehen verloren. Die Kopien der replizierten Heartbeats auf dem wiederhergestellten Node bleiben.

**Option B: `RecoverCluster` auf allen Überlebenden mit identischer Konfiguration**
- Vorteile: Kein Neu-Beitritt, lokale Daten bleiben.
- Nachteile:
  - Haben die Überlebenden unterschiedlich lange Logs, kompaktiert jeder seinen eigenen Stand zu einem Snapshot.
  - Ob Raft das danach sauber zusammenführt, hängt an Details. Ein Fehler zeigt sich erst später als abweichender Zustand.

**Option C: `peers.json` im Datenverzeichnis, beim Start ausgewertet (Consul/Nomad-Stil)**
- Vorteile: Bekanntes Muster.
- Nachteile:
  - Eine vergessene Datei wirkt bei jedem Start.
  - Kein Dry-Run, keine Rückfrage.
  - Dieselbe Mehrdeutigkeit wie B, sobald mehrere Nodes eine Datei bekommen.

### Admin-Passwort

**Option A: Online über den Cluster-Port mit dem Node-Zertifikat**
- `admin reset-password` läuft neben dem laufenden Node (`docker exec`). Es nutzt das Zertifikat aus `DATA_DIR/tls`, um über `ClusterService.Apply` die bestehenden Befehle zu schicken: neuer Hash per `PutEntity`/User, danach alle Sitzungen widerrufen.
- Vorteile:
  - Keine neue Netzschnittstelle und kein neuer FSM-Befehl.
  - Wirkt im ganzen Cluster.
  - Wer `DATA_DIR` lesen kann, hat ohnehin den Master-Key und den Node-Schlüssel; das ist keine neue Berechtigung.
- Nachteile: Braucht ein Quorum. Ohne Quorum zuerst `admin recover`.

**Option B: Einmal-Reset-Token, das der Nutzer in der UI einlöst**
- Vorteile: Das Passwort tippt der Nutzer selbst in der UI.
- Nachteile: Ein neuer API-Endpunkt und ein neues Token-Konzept nur für den Notfall.

## Entscheidung

Für den Quorumverlust **Option A**, für das Passwort **Option A**.

**1. Rahmen**
- Neue Unterbefehle unter `uptimecluster admin …`, eigenes Paket `internal/admincli`. Sie lesen dieselbe Umgebung wie der Server (`DATA_DIR`, `ADVERTISE_ADDR`, …).
- Ausgaben sind für Menschen gedacht. Wo sinnvoll gibt es `--json`.
- Exit-Codes: 0 = Erfolg, 1 = Fehler, 2 = falsche Bedienung.
- Kein Befehl gibt Geheimnisse aus oder nimmt sie als Argument entgegen.

**2. `admin raft-info` (offline, nur lesend)**
- Gibt aus: Node-ID, letzten Log-Index und -Term, angewendeten Index aus `state.db`, letzte Raft-Konfiguration (ID, Adresse, Rolle), Befehlsversion des Clusters (ADR 0018).
- Läuft nur bei gestopptem Node. Der Raft-Log ist dann nicht gesperrt; läuft der Node noch, meldet der Befehl das klar, statt zu warten.
- Mit diesen Angaben wählt der Betreiber den Überlebenden: höchster Term, dann höchster Index.

**3. `admin recover --yes` (offline)**
- Läuft nur bei gestopptem Node. Ohne `--yes` zeigt der Befehl nur, was er tun würde: alte Konfiguration, neue Konfiguration, betroffene Nodes, Warnung zum Datenverlust.
- Erst wird gesichert: `raft/` und `state.db` als Kopie nach `DATA_DIR/recovery-backup-<zeit>/`. Scheitert die Sicherung, bricht der Befehl ab.
- Dann `raft.RecoverCluster` mit der FSM auf `state.db` und der Konfiguration **nur dieser Node als Voter**. Die Adresse kommt aus der bisherigen Konfiguration.
- Die IDs aller anderen bisherigen Mitglieder schreibt der Befehl nach `DATA_DIR/recovery-revoke.json`.
- Beim nächsten Start schlägt der Node, sobald er Leader ist und die CA bereitsteht, für jede dieser IDs `RevokeNode` vor (FA-PKI-04). Das löscht auch den Registereintrag (ADR 0017). Danach löscht er die Datei. Ein zurückkehrender alter Node wird so an mTLS abgewiesen, statt mit veraltetem Zustand Wahlen zu stören (PF-06).
- Die übrigen Überlebenden werden gestoppt. Ihr Datenverzeichnis wird beiseitegelegt, nicht gelöscht. Sie treten als **neue Nodes** mit neuer Node-ID und neuem Join-Token bei. Ihre alten IDs sind gesperrt.

**4. `admin reset-password --username <name> [--tenant <id>]` (online)**
- Liest das neue Passwort von stdin, ohne Echo bei einem Terminal, mit Wiederholung. Alternativ aus `--password-file`.
- Prüft es mit `auth.CheckPassword` und hasht es mit `auth.HashPassword` (Argon2id).
- Liest Nutzer und Revision aus der lokalen `state.db`, nur lesend; SQLite im WAL-Modus erlaubt das neben dem laufenden Node.
- Schickt über den Cluster-Port des lokalen Nodes (`ADVERTISE_ADDR`, mTLS mit dem Node-Zertifikat, Rolle Voter) `PutEntity`/User mit dem neuen Hash und der gelesenen Revision, dann den Widerruf aller Sitzungen des Nutzers. Ein Follower leitet wie gewohnt an den Leader weiter (AP-2.3).
- Akteur im Befehl ist `KIND_SYSTEM` mit der ID `cli:<node-id>` (Audit, FA-DAT-01).
- Ein gesperrter Nutzer wird nur auf ausdrücklichen Wunsch (`--enable`) wieder aktiviert.
- Ohne Leader oder Quorum gibt es eine klare Fehlermeldung mit Hinweis auf `admin recover`.

**5. Abgrenzung**
- „Knoten ersetzen“ (FA-CLU-08) bleibt in der Admin-API und kommt in die Admin-UI (AP-2.8). Ohne Quorumverlust braucht es keine CLI.
- Probes (P5) brauchen keine Recovery-Befehle.

**6. Runbook und Tests**
- Runbook `docs/betrieb/recovery.md`: Quorumverlust Schritt für Schritt, Entscheidungshilfe für die Wahl des Überlebenden, Passwort-Reset, Fehlerbilder.
- Integrationstest in `internal/clusterhost`:
  - 3 Voter, 2 werden gestoppt.
  - `recover` auf dem dritten.
  - Neustart als Single-Voter mit allen bisher committeten Daten.
  - Gesperrte alte IDs.
  - Neuer Beitritt eines frischen Nodes.
  - Passwort-Reset über einen Follower.
- Die Abnahme im Chaos-Test folgt mit AP-2.9.

## Konsequenzen

- **Positiv:**
  - Ein klarer, getesteter Weg aus dem Quorumverlust mit genau einem gültigen Zustand.
  - Alte Nodes können nach der Recovery nicht mehr stören.
  - Passwort-Reset ohne neue Angriffsfläche, über die bestehenden, deterministischen Befehle.
- **Negativ / Risiken:**
  - `recover` kann Daten verlieren: Einträge, die nur auf den ausgefallenen Nodes committet waren, gehen verloren. Bestätigung, Dry-Run und Sicherung mildern das, verhindern es aber nicht.
  - Die neu beitretenden Nodes verlieren ihre lokalen Heartbeats.
  - Der Reset setzt Zugriff auf `DATA_DIR` voraus. Wer den hat, kontrolliert den Node ohnehin.
- **Folgearbeiten:**
  - Chaos-Test des Runbooks (AP-2.9).
  - Eintrag im Audit-Log (P3).

## Approval

| Datum | Status | Durch | Anmerkung |
|-------|--------|-------|-----------|
| 2026-10-25 | Proposed | Claude | |
