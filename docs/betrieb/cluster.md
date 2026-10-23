# Cluster erweitern und verkleinern

Dieses Dokument beschreibt, wie Voter zu einem laufenden Cluster hinzugefügt, entfernt und ersetzt werden (Pflichtenheft FA-CLU-03, FA-CLU-08, FA-CLU-10, FA-PKI-02 bis FA-PKI-04). Die Angaben entsprechen `internal/membership`, `internal/clusterhost`, `internal/clusterpki`, `internal/pki` und `internal/config`. Die Grundkonfiguration eines einzelnen Knotens steht in [compose.md](compose.md).

## Überblick

- Jeder Voter öffnet einen Cluster-Port (`CLUSTER_ADDR`, Standard `:9443`). Darüber laufen Raft und der interne gRPC-Dienst, jeweils mit gegenseitiger TLS-Authentifizierung (mTLS). Der Port ist nur für die Cluster-Kommunikation gedacht; die Oberfläche und die API laufen auf `HTTP_ADDR` (Standard `:8080`).
- Die Zertifizierungsstelle (CA) gehört dem Cluster. Sie wird beim ersten Start des ersten Voters erzeugt und über den replizierten Zustand verteilt. Jeder Node erhält ein eigenes Zertifikat mit Rolle und Node-ID.
- Jeder Voter nimmt schreibende API-Anfragen an. Ein Follower leitet sie über den Cluster-Port an den Leader weiter und antwortet erst, wenn die Änderung auch bei ihm angekommen ist; ein Load-Balancer darf also auf beliebige Voter verteilen. Hat der Follower länger als zwei Heartbeat-Timeouts nichts vom Leader gehört oder gelingt die Weiterleitung nicht binnen 3 s, antwortet er mit HTTP 503 und `Retry-After`. Lautet die Antwort „Result Unknown“, ist die Änderung womöglich übernommen; eine Wiederholung mit demselben `Idempotency-Key` legt nichts doppelt an.
- `CLUSTER_ADVERTISE` ist die Adresse, unter der die anderen Knoten diesen Knoten erreichen. Sie wird als Raft-Serveradresse gespeichert und muss als `host:port` mit **stabilem DNS-Namen** angegeben werden. Standard ist `<NODE_NAME>:<Port aus CLUSTER_ADDR>`. Die Planung sollte nicht auf IP-Adressen beruhen, da sich diese bei Containern und VPS ändern können.

## Voter hinzufügen

1. **Join-Token erzeugen.** Ein Plattform-Administrator legt das Token an: REST `POST /api/v1/admin/join-tokens` mit der Rolle `voter`.
   - Das Token wird nur bei der Erzeugung angezeigt (Antwort mit `Cache-Control: no-store`). Der Cluster speichert nur einen Hash des Geheimnisses, es lässt sich später nicht erneut abrufen.
   - Standardmäßig gilt es 1 Stunde, höchstens 24 Stunden.
   - Es ist einmalig: Nach dem ersten erfolgreichen Join ist es verbraucht.
   - Das Token enthält den Fingerprint der Cluster-CA. Der neue Node prüft diesen, bevor er Daten sendet, und vertraut nur einer CA mit diesem Fingerprint.

2. **Neuen Node starten** mit der Adresse eines bestehenden Voters und dem Token:
   - `CLUSTER_JOIN=<host:port eines bestehenden Voters>` (mehrere Adressen durch Komma getrennt; leere Einträge werden ignoriert)
   - `JOIN_TOKEN_FILE=<Pfad>` (bevorzugt, das Token liegt dann nicht in der Prozessumgebung) oder `JOIN_TOKEN=<Token>`. Beides gleichzeitig ist ein Fehler.
   - `CLUSTER_ADVERTISE=<stabiler DNS-Name>:<Port>` und ein eigenes `DATA_DIR` ohne Raft-Zustand.

   Ein Node mit `CLUSTER_JOIN` und ohne vorhandenen Raft-Zustand tritt bei und bootstrappt keinen eigenen Cluster. Ein Node mit Raft-Zustand oder Zertifikat tritt nicht erneut bei, `CLUSTER_JOIN` wird dann ignoriert.

3. **Aufnahme.** Der Leader nimmt den Node zunächst als Nicht-Voter auf. Sobald der Node den eigenen Konfigurationseintrag angewandt hat und höchstens 100 Einträge hinter dem Log des Leaders liegt, befördert der Leader ihn automatisch zum Voter. Im Log erscheinen dazu die Meldungen `node added as nonvoter` und `nonvoter promoted to voter`.
   - Bleibt der Node 10 Minuten Nicht-Voter, schreibt der Leader `node did not catch up and stays a nonvoter` in das Log. Die Beförderung wird dann nicht weiter versucht, bis ein Führungswechsel stattfindet. Ursache prüfen (Netz, Adresse, Ressourcen) und den Node neu starten oder den Leader wechseln lassen.
   - Vorübergehende Fehler (etwa kein Leader oder Netzwerkfehler) werden mit wachsenden Pausen (bis 30 Sekunden) wiederholt, bis der Join gelingt oder der Prozess beendet wird. Endgültige Ablehnungen (ungültiges Token, belegte Adresse, falsche Rolle) brechen den Join sofort ab.

4. **Master-Key.** Hier gibt es zwei Fälle:
   - **Ohne `MASTER_KEY` und `MASTER_KEY_FILE`:** Der Cluster hat seinen Schlüssel beim ersten Start in `DATA_DIR/master.key` erzeugt. Dieser Schlüssel wird beim Join an den neuen Voter übergeben und dort mit Modus 0600 in dessen `master.key` gespeichert. Der Operator muss nichts tun.
   - **Mit `MASTER_KEY` oder `MASTER_KEY_FILE`:** Jeder Voter muss denselben Schlüssel konfigurieren. Der neue Node vergleicht den Fingerprint des Clusters mit seinem Schlüssel; stimmt er nicht überein, bricht der Start ab. Ein Node ohne Schlüsselkonfiguration, der einem Cluster mit externem Schlüssel beitreten will, bricht mit dem Hinweis ab, `MASTER_KEY` oder `MASTER_KEY_FILE` zu setzen. Unabhängig vom Join prüft jeder Voter bei jedem Start beim Laden der Datenschlüssel, ob sein Master-Key zum Cluster passt, und beendet sich sonst (`master key does not match the cluster`).

5. **Anzahl der Voter.** Empfohlen werden 3 oder 5 Voter. Mit 2 oder 4 Votern steigt die Ausfallsicherheit nicht gegenüber 1 bzw. 3 Votern. `GET /api/v1/admin/nodes` listet die Mitglieder und nennt Warnungen bei einer geraden Anzahl oder mehr als 5 Votern.

## Node entfernen

- Das Entfernen erfolgt **nur manuell** durch einen Plattform-Administrator: REST `DELETE /api/v1/admin/nodes/{id}`. Der Cluster entfernt nie automatisch einen Node, auch nicht, wenn er ausgefallen ist.
- Ein Leader kann sich nicht selbst entfernen (Fehler „the leader cannot remove itself“). Vorher die Führung abgeben: Ein regulärer Stopp des Leaders (SIGTERM) überträgt die Führung an einen anderen Voter, sofern der Cluster mehr als einen Server hat. Danach den Node entfernen.
- Der letzte Voter kann nicht entfernt werden.
- Die Antwort nennt die verbleibende Anzahl der Voter (`votersLeft`). Bleiben weniger als 3 Voter oder eine gerade Anzahl, enthält sie zusätzlich eine Warnung. Ein Cluster mit 2 Votern übersteht den Ausfall eines Votern nicht; mit 4 Votern nicht mehr als mit 3.
- Die Zertifikate des entfernten Nodes werden gesperrt. Die Sperre wirkt spätestens nach 10 Sekunden auf allen Knoten. Der Node kann sich danach nicht mehr mit dem alten Zertifikat verbinden.
- Das Entfernen besteht aus zwei Schritten: erst die Entfernung aus der Raft-Konfiguration, dann die Sperre. Schlägt die Sperre fehl, liefert der Vorgang einen Fehler; der Node ist dann bereits aus dem Cluster entfernt, und die Sperre muss erneut ausgelöst werden.
- Der Aufruf muss den Leader erreichen. Ein Node, der nicht Leader ist, antwortet mit `503` und der Bitte, es erneut zu versuchen.
- Fehlerantworten: `409`, wenn der Leader sich selbst oder den letzten Voter entfernen soll; `404`, wenn die Node-ID kein Mitglied ist.
- Das Datenverzeichnis des entfernten Nodes ist danach unbrauchbar, weil es Raft-Zustand und ein gesperrtes Zertifikat enthält und ein erneuter Join nicht stattfindet. Vor einer Wiederverwendung des Verzeichnisses oder des Hosts löschen.

## Node ersetzen

Ein neuer Node kann die Adresse eines bestehenden Voters nicht übernehmen. Ein Join unter einer bereits belegten Adresse wird abgelehnt, bevor das Token verbraucht wird. Die Fehlermeldung nennt die Node-ID des bisherigen Nodes (`address is already used by node <id>`).

Vorgehen:

1. Den alten Node entfernen (siehe oben).
2. Ein neues Join-Token erzeugen.
3. Den neuen Node mit der frei gewordenen Adresse und dem neuen Token starten.

Während des Ersetzens fehlt eine Ausfallreserve; ein Ausfall eines weiteren Votern in dieser Zeit kann das Quorum kosten.

## Adresse ändern

Die Adresse eines bestehenden Voters lässt sich ändern, ohne neues Token und ohne Verlust der Stimme:

1. `CLUSTER_ADVERTISE` des Nodes auf die neue Adresse setzen. `DATA_DIR` und Node-ID bleiben unverändert. Der Node tritt nicht erneut bei, weil er Raft-Zustand und ein Zertifikat hat.
2. Den Node neu starten.
3. Der Node meldet dem Leader die neue Adresse automatisch: Er fragt die anderen Voter nach dem Leader und ruft ihn mit seinem Zertifikat auf. Der Leader ersetzt die Adresse in der Raft-Konfiguration; die Stimme bleibt erhalten. Im Log erscheint `cluster address updated`. Gelingt die Meldung nicht, wiederholt der Node sie mit wachsenden Pausen (bis 30 Sekunden) und schreibt `announcing the new cluster address failed, retrying`.

Bis die Meldung gelingt, ist der Node für die übrigen Voter unter der alten Adresse nicht erreichbar. Er zählt weiter zum Quorum, bringt aber keine Antworten. In dieser Zeit fehlt die Ausfallreserve dieses Votern.

Ist der Node beim Neustart selbst Leader, ist die Meldung nicht getestet. In diesem Fall vorher die Führung abgeben (siehe „Node entfernen“).

## Zertifikate

- Node-Zertifikate sind 90 Tage gültig.
- Sie werden nach zwei Dritteln der Laufzeit erneuert, also nach etwa 60 Tagen. Der Node prüft das stündlich. Ein Leader erneuert sein Zertifikat direkt; ein Follower lässt es über den Leader ausstellen.
- Die Dateien liegen in `DATA_DIR/tls`: `node-cert.pem` (Zertifikatskette) und `node-key.pem` (privater Schlüssel, Modus 0600). Der private Schlüssel verlässt den Node nicht.
- Verbindungen werden spätestens nach einer Stunde geschlossen und neu aufgebaut. Damit übernehmen auch laufende Verbindungen ein erneuertes Zertifikat spätestens nach 1 Stunde.
- Gesperrte Zertifikate werden alle 10 Sekunden aus dem replizierten Zustand nachgeladen.

## Fehlerbilder

| Meldung | Bedeutung | Vorgehen |
|---------|-----------|----------|
| `invalid join token` | Das Token ist abgelaufen, bereits verbraucht, falsch oder gehört zu einem anderen Cluster. Der Cluster meldet alle Fälle gleich, damit ein Angreifer den Grund nicht erfährt. | Neues Token erzeugen und den Node mit diesem neu starten. |
| `the join token is for another role` | Das Token wurde für eine andere Rolle erzeugt. | Token mit Rolle `voter` erzeugen. |
| `address is already used by node <id>` | Die Adresse gehört bereits einem Node. Das Token bleibt gültig. | Bisherigen Node entfernen oder eine andere Adresse verwenden. |
| `this node's master key differs from the cluster's` | Der konfigurierte Master-Key passt nicht zu dem des Clusters. | `MASTER_KEY` bzw. `MASTER_KEY_FILE` auf den Schlüssel des Clusters setzen. |
| `configure MASTER_KEY or MASTER_KEY_FILE` | Der Cluster nutzt einen externen Schlüssel, der Node hat keinen. | Denselben Schlüssel auf dem neuen Node konfigurieren. |
| `master key does not match the cluster` | Beim Start passt der Master-Key nicht zu den replizierten Datenschlüsseln. | Den Schlüssel des Clusters konfigurieren; nie einen neuen erzeugen lassen. |
| `no leader available, retry` | Es gibt gerade keinen Leader (Wahl läuft, Quorum fehlt). | Keine Aktion nötig; der Join wird wiederholt. Bleibt der Fehler bestehen, Quorum und Netz prüfen. |

## Verwandte Dokumente

- [compose.md](compose.md): Konfiguration eines einzelnen Knotens und Variablen
- [icmp.md](icmp.md): ICMP-Prüfungen im Container
