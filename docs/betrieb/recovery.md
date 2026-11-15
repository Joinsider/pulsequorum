# Recovery: Quorumverlust und Admin-Passwort

Dieses Runbook beschreibt die Befehle unter `uptimecluster admin` (ADR 0019, FA-OBS-04, FA-CLU-08). Sie laufen im selben Image wie der Server und lesen dieselbe Konfiguration (`DATA_DIR`, `CLUSTER_ADVERTISE`, …), z.B. mit `docker compose run --rm <dienst> admin …` bei gestopptem Node oder `docker compose exec <dienst> /uptimecluster admin …` bei laufendem. In den Beispielen unten steht verkürzt `uptimecluster admin …`.

| Befehl | Node | Zweck |
|--------|------|-------|
| `admin raft-info [--json]` | gestoppt | Log-Position und letzte Raft-Konfiguration dieses Nodes anzeigen |
| `admin recover [--yes]` | gestoppt | Diesen Node nach Quorumverlust zum einzigen Voter machen |
| `admin reset-password --username <name>` | läuft | Neues Passwort für einen lokalen Nutzer setzen |

## Quorumverlust

**Woran man ihn erkennt:** Kein Node ist Leader, Schreibzugriffe enden mit 503 (`no leader available`), die Logs zeigen wiederholte Wahlen. Auch eine neue Anmeldung endet mit 503, weil sie eine Sitzung im Raft-Zustand anlegt; bestehende Access-Tokens funktionieren für Lesezugriffe bis zu ihrem Ablauf (15 min). Checks laufen weiter, Ergebnisse werden lokal gespeichert (NFA-REL-06).

**Erst prüfen, ob die ausgefallenen Nodes zurückkommen können.** Kommt die Mehrheit der Voter wieder, stellt sich das Quorum von selbst wieder her, ohne Datenverlust. `recover` ist nur für den Fall gedacht, dass die Mehrheit **dauerhaft** verloren ist.

> **Achtung:** `recover` kann Daten verlieren. Änderungen, die nur auf den verlorenen Nodes bestätigt waren, sind danach weg. Änderungen, die auf dem gewählten Node noch nicht bestätigt waren, gelten danach als bestätigt.

### 1. Alle überlebenden Nodes stoppen

Jeder überlebende Voter wird gestoppt. Solange ein Node läuft, verweigern `raft-info` und `recover` die Arbeit (`the node is still running`).

### 2. Den Node mit dem neuesten Stand wählen

Auf jedem Überlebenden:

```bash
uptimecluster admin raft-info
```

Gewählt wird der Node mit dem **höchsten letzten Term**, bei Gleichstand dem **höchsten letzten Index**. Er verliert am wenigsten.

### 3. Auf genau diesem Node `recover` ausführen

Erst ohne `--yes`, um den Plan zu sehen (alte und neue Konfiguration, Nodes, die gesperrt werden):

```bash
uptimecluster admin recover
```

Dann ausführen:

```bash
uptimecluster admin recover --yes
```

Der Befehl:

1. sichert `raft/` und `state.db` nach `DATA_DIR/recovery-backup-<zeit>/`,
2. schreibt eine neue Raft-Konfiguration mit diesem Node als einzigem Voter,
3. merkt sich die übrigen bisherigen Mitglieder in `DATA_DIR/recovery-revoke.json`.

**`recover` nie auf mehr als einem Node ausführen.** Zwei Nodes mit eigener Konfiguration wären zwei getrennte Cluster.

### 4. Den wiederhergestellten Node starten

Er wird sofort Leader. Sobald die Cluster-CA bereitsteht, sperrt er alle Zertifikate der übrigen bisherigen Mitglieder (`old members revoked after recovery` im Log) und löscht `recovery-revoke.json`. Ein alter Node, der später doch wieder anläuft, wird damit am Cluster-Port abgewiesen.

Prüfen: Die Node-Liste der Admin-API zeigt nur diesen Node, Schreibzugriffe funktionieren wieder.

### 5. Die anderen Nodes neu beitreten lassen

Die übrigen Überlebenden haben einen veralteten Zustand und gesperrte Zertifikate. Sie treten als **neue Nodes** bei:

1. Datenverzeichnis des Nodes beiseitelegen (umbenennen, nicht löschen, bis der Cluster wieder stabil läuft).
2. Mit leerem Datenverzeichnis, neuem Join-Token (Admin-API) und `CLUSTER_JOIN` starten, wie unter [cluster.md](cluster.md#voter-hinzufügen) beschrieben. Der Node bekommt eine neue Node-ID.
3. Einen Node nach dem anderen, bis wieder 3 (oder 5) Voter laufen.

Lokal gespeicherte Heartbeats dieser Nodes gehen dabei verloren. Die Kopien, die der wiederhergestellte Node von ihnen hatte, bleiben erhalten.

### 6. Aufräumen

Läuft der Cluster wieder mit voller Ausfallreserve, können die Sicherung `recovery-backup-<zeit>/` und die beiseitegelegten Datenverzeichnisse gelöscht werden.

## Admin-Passwort zurücksetzen

Wenn sich kein `platform-admin` mehr anmelden kann. Voraussetzungen: Der Node läuft, der Cluster hat einen Leader, `AUTH_MODE=local`.

```bash
docker compose exec -it <dienst> /uptimecluster admin reset-password --username admin
```

Das Passwort wird zweimal ohne Anzeige abgefragt. Für Skripte: `--password-file <datei>` oder über stdin (`printf '%s\n' "$PW" | … reset-password --username admin`). Das Passwort steht nie in den Argumenten oder im Log.

Der Befehl setzt das neue Passwort im ganzen Cluster und beendet alle Sitzungen des Nutzers. Ein gesperrter Nutzer bleibt gesperrt, außer mit `--enable`. Der Befehl trägt als Akteur `cli:<node-id>`; das Audit-Log (P3) zeigt ihn dann so an.

Wer `admin`-Befehle ausführen kann, hat Zugriff auf `DATA_DIR` und damit auf den Master-Key und den Schlüssel des Nodes. Der Zugriff auf Container und Volumes muss entsprechend geschützt sein.

## Fehlerbilder

| Meldung | Bedeutung | Vorgehen |
|---------|-----------|----------|
| `the node is still running` | Der Raft-Log ist von einem laufenden Prozess gesperrt. | Node stoppen, Befehl wiederholen. |
| `no Raft state in …` | Das Datenverzeichnis gehörte nie zu einem Cluster, oder `DATA_DIR` zeigt woandershin. | `DATA_DIR` prüfen. |
| `this node is not a member of its last configuration` | Der Node war zuletzt nicht mehr Mitglied. | `recover` auf einem anderen Überlebenden ausführen. |
| `the cluster has no leader` | `reset-password` braucht ein Quorum. | Erst das Quorum wiederherstellen (siehe oben). |
| `this node at … does not answer` | Der Node läuft nicht oder `CLUSTER_ADVERTISE` stimmt nicht. | Node starten bzw. Konfiguration prüfen. |
| `the user was changed in the meantime, or this node has not caught up …` | Der Nutzer wurde gerade geändert, oder dieser Node hinkt dem Leader hinterher. | Nach ein paar Sekunden wiederholen. |
| `no local user …` | Den Nutzernamen gibt es nicht. | Nutzernamen prüfen (Groß-/Kleinschreibung egal). |
| `password must have 12 to … characters` | Das Passwort ist zu kurz oder zu lang. | Längeres Passwort wählen. |

## Verwandte Dokumente

- [cluster.md](cluster.md): Cluster erweitern, Nodes entfernen, Rolling Updates
- ADR 0019: Entscheidung und Begründung
