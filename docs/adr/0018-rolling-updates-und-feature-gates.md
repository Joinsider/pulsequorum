# ADR 0018: Rolling Updates – Befehlsversionen, Feature-Gate und Expand/Contract

| | |
|---|---|
| **Status** | Proposed |
| **Datum** | 2026-10-23 |
| **Entscheider** | Projektinhaber |
| **Bezug** | FA-CLU-07/12, NFA-PORT-04, NFA-MNT-04, PF-09, ADR 0004, ADR 0005 Nr. 8, ADR 0017 |

## Kontext

FA-CLU-12 verlangt, dass Voter unterschiedlicher Minor-Versionen gemeinsam laufen. Neue Raft-Befehle dürfen erst verwendet werden, wenn alle Voter sie kennen. NFA-PORT-04 verlangt, dass jede Minor-Version von der vorherigen per Rolling Update erreichbar ist. ADR 0005 (Nr. 8) legt nur das Prinzip fest: Jeder Node meldet seine Befehlsversion, und neue Versionen werden erst verwendet, wenn alle Voter sie unterstützen. Wie das konkret aussieht, ist offen.

Stand nach AP-2.5:

- `CommandVersion = 2` ist die höchste Version, die das Binary anwenden kann. Die FSM hält kontrolliert an, wenn ein Eintrag eine höhere `version` trägt.
- **Fast alle Befehle tragen `version = CommandVersion`**, nicht die Version, die sie wirklich brauchen (`commands.go`, `sessions.go`, `pki.go`, `Node.ApplyLocal` als Vorgabe). Hebt ein Release `CommandVersion` an, hält jedes ältere Binary schon beim ersten beliebigen Befehl an, nicht erst beim neuen. Ein Rolling Update ist so unmöglich.
- Jeder Voter meldet `max_command_version` im Node-Register (ADR 0017). Noch liest niemand diesen Wert.
- Eine Proto-Payload, die ein altes Binary nicht kennt, landet dort als `unsupported`-Ablehnung, beim neuen Binary dagegen wird sie angewendet. Ein neues Feld in einer bekannten Payload ignoriert das alte Binary stillschweigend. In beiden Fällen laufen die Zustände auseinander, ohne dass es jemand merkt. Allein die Versionsprüfung verhindert das.
- `state.db` wird beim Öffnen per goose migriert, auch nach einem Snapshot-Restore. Ein Snapshot eines neueren Nodes kann Migrationen enthalten, die ein älterer Node nicht kennt.
- Ein Release mit gemischten Versionen gibt es vor 1.0 nicht (ADR 0017). Es gibt also keinen Bestand, der ohne Gate migriert werden müsste.

## Optionen (wo die freigegebene Version herkommt)

### Option A: Gespeicherte Cluster-Befehlsversion, die nur steigt
- Der Raft-Zustand enthält eine **aktive Befehlsversion** (`command_level`). Der Leader hebt sie an, sobald alle Voter laut Register eine höhere Version unterstützen. Sie sinkt nie.
- Vorteile:
  - Eine Zahl, die alle Nodes gleich sehen. Vorschlagen wird damit zu einer einfachen Prüfung `version ≤ command_level`.
  - Downgrades lassen sich erkennen: Ein Binary, dessen `CommandVersion` unter dem gespeicherten Level liegt, startet nicht.
  - Joins lassen sich prüfen.
  - Der Wechsel steht im Log und ist in der Admin-UI sichtbar.
- Nachteile:
  - Ein zusätzlicher Befehl und eine Hintergrundschleife auf dem Leader.
  - Ist das Level einmal angehoben, ist kein Rollback auf die alte Version mehr möglich (nur Restore aus Backup).

### Option B: Bei jedem Vorschlag aus dem Register berechnen
- Minimum der `max_command_version` über alle Voter der aktuellen Raft-Konfiguration.
- Vorteile: Kein neuer Befehl. Ein Downgrade zurück ist jederzeit möglich, solange kein neuer Befehl geschrieben wurde.
- Nachteile:
  - Ein Node, der nach dem Anheben auf ein altes Binary zurückgeht, hält beim nächsten neuen Eintrag an. Das Register zeigt dann noch die alte, hohe Zahl.
  - Followers sehen eine andere Raft-Konfiguration als der Leader, solange sie hinterherhinken.
  - Keine eindeutige Grenze für Expand/Contract.

### Option C: Wie A, aber das Anheben bestätigt ein Admin manuell
- Vorteile: Der Betreiber kann vor dem „Point of no Return“ noch zurück.
- Nachteile:
  - Ein zusätzlicher Bedienschritt bei jedem Update.
  - Vergisst man ihn, bleiben neue Funktionen stumm aus.
  - Für Selbsthoster mit kleinen Clustern unnötig.

## Entscheidung

**Option A**, mit folgenden Festlegungen. Eine manuelle Bestätigung (Option C) kann später als Schalter dazukommen, ohne das Modell zu ändern.

**1. Befehlsversion je Payload**
- Jede Payload hat eine feste **Mindestversion**: die Version, ab der ein Binary sie genau so anwendet. Eine Tabelle im Paket `cluster` ordnet jedem `oneof`-Fall diese Version zu. Alle heutigen Payloads haben Version 1, außer `RegisterNode` und `SetNodeHealth` (2).
- Der Konstruktor bzw. `Node.Apply` setzt `Command.version` aus dieser Tabelle, nicht aus `CommandVersion`. Ein Test stellt sicher, dass jeder `oneof`-Fall einen Eintrag hat.
- **Regel für Änderungen:**
  - Ändert sich, was `Apply` mit einer Payload macht (neues Feld mit Wirkung, andere Validierung, anderes Ergebnis), bekommt sie eine neue Mindestversion. `CommandVersion` steigt mit.
  - Wird das neue Feld nur optional genutzt, setzt der Konstruktor die höhere Version nur, wenn es gesetzt ist.
  - Proto-Feldnummern werden nie wiederverwendet.
- `Apply` behält Code für alle Versionen ab `MinCommandLevel` (siehe 4). Ältere Einträge können im Log oder Snapshot stehen.

**2. Aktive Befehlsversion im Raft-Zustand**
- `fsm_meta` bekommt den Schlüssel `command_level`. Er wird in den FSM-Fingerprint aufgenommen.
- Fehlt der Schlüssel, gilt **2**: Das ist die Basis, das erste Release mit Gate. Alle Befehle mit Version 1 oder 2 sind damit immer erlaubt.
- Neuer Befehl `SetCommandLevel{level}` (Mindestversion 2). `Apply` übernimmt den Wert nur, wenn er größer ist. Sonst ist es ein Erfolg ohne Schreibzugriff.
- **Befehle, die das Gate selbst braucht** (`RegisterNode`, `SetCommandLevel`), bleiben dauerhaft auf der Basisversion. Sonst könnte sich ein neuer Node nicht registrieren, und das Level könnte nie steigen.

**3. Anheben durch den Leader**
- Eine Schleife auf dem Leader läuft bei Registeränderungen, Konfigurationsänderungen und nach der Wahl, zusätzlich alle 30 s.
- Sie berechnet `ziel = min(max_command_version)` über alle Voter **der aktuellen Raft-Konfiguration**. Ein Voter ohne Registereintrag zählt mit der Basisversion.
- Ist `ziel > command_level`, schlägt sie `SetCommandLevel(ziel)` vor. Nicht-Voter in der Aufholphase zählen mit, sobald sie in der Konfiguration stehen.
- Ein Node registriert sich bei jedem Start neu (ADR 0017). Nach dem Update des letzten Voters steigt das Level deshalb innerhalb weniger Sekunden.

**4. Prüfungen**
- **Vorschlagen:**
  - `Node.Apply` lehnt einen Befehl mit `version > command_level` (lokal angewendeter Zustand) ab, bevor er vorgeschlagen oder weitergeleitet wird.
  - Der Fehler ist `*CommandError{Code: "unsupported"}` mit Hinweis auf das laufende Update. Die API antwortet mit 503 und einem Hinweis, dass die Funktion erst nach dem Update aller Voter verfügbar ist.
  - `ApplyLocal` auf dem Leader prüft zusätzlich `version ≤ CommandVersion` des Leaders. Das schützt, falls ein neuerer Follower an einen älteren Leader weiterleitet.
- **Join:**
  - Der Leader lehnt einen Join ab, wenn `node.max_command_version < command_level` ist (`FAILED_PRECONDITION`, „Binary zu alt, mindestens Version …“).
  - Ein Joiner ohne Angabe zählt als Basisversion.
- **Start und Downgrade:**
  - Ein Node, dessen lokaler Zustand `command_level > CommandVersion` hat, startet nicht. Die Fehlermeldung nennt die nötige Version.
  - Jedes Binary hat zusätzlich `MinCommandLevel`, die älteste Version, deren Befehle es noch anwenden kann. Liegt das Level des Clusters darunter, startet es ebenfalls nicht („Update über mehr als eine Minor-Version, bitte zuerst auf … aktualisieren“).
  - Das Anhalten der FSM bei unbekannter Version bleibt als letzte Sicherung.

**5. Schema (Expand/Contract, NFA-MNT-04)**
- **Expand in Release n:**
  - Neue Tabellen und Spalten, Spalten nur nullable oder mit Default. Keine Änderung der Bedeutung bestehender Spalten.
  - Release n−1 muss auf dem erweiterten Schema unverändert laufen.
  - Neue Spalten werden nur von Befehlen beschrieben, die erst nach dem Anheben des Levels erlaubt sind. So schreiben alle Nodes dieselben Werte, und der Fingerprint bleibt gleich.
  - Backfills laufen als deterministisches SQL in der Migration (nur aus bestehenden Zeilen) oder als Raft-Befehl, nie mit Uhrzeit oder Zufall.
- **Contract frühestens in Release n+1:**
  - Erst dann darf entfernt oder umbenannt werden, was n noch liest.
  - Weil n+1 nur auf ein Cluster mit dem Level von n startet (siehe 4), läuft kein Binary mehr mit, das die alte Form braucht.
- **Snapshots:**
  - Ein älteres Binary muss einen Snapshot mit unbekannten, neueren Migrationen öffnen können. goose darf beim Öffnen nicht an Versionen scheitern, die es nicht kennt. Ein Test sichert das ab, die Migrationseinstellungen werden bei Bedarf angepasst.
  - Ein neueres Binary wendet beim Öffnen fehlende Migrationen an (heutiges Verhalten).
- Für `metrics.db` (lokal, nicht repliziert) gilt dieselbe Expand/Contract-Regel, damit ein Node auf die vorherige Minor-Version zurück kann, solange das Level nicht angehoben ist.

**6. Andere Verträge**
- **gRPC zwischen Nodes:**
  - Neue RPCs liefern auf älteren Nodes `Unimplemented`. Der Aufrufer behandelt das als „noch nicht verfügbar“, nicht als Fehler des Peers.
  - Neue Felder sind optional.
- **REST-API und SPA:** Neue Felder sind optional (wie bisher). Während eines Updates kann ein Load-Balancer Anfragen auf verschiedene Versionen verteilen. Die SPA muss mit fehlenden neuen Feldern umgehen.

**7. Sichtbarkeit**
- Die Admin-API zeigt die aktive Befehlsversion des Clusters und je Node `maxCommandVersion`.
- Liegt mindestens ein Voter unter der höchsten gemeldeten Version, zeigt sie zusätzlich den Hinweis „Update läuft“.
- Ein Systemalert bei dauerhaft gemischten Versionen kommt mit AP-3.6.

**8. Tests (PF-09)**
- Die maximale Befehlsversion eines Nodes lässt sich nur in Tests überschreiben (`cluster.Config`).
- Ein Integrationstest mit 3 Votern prüft:
  - Ein Testbefehl mit Version 3 wird abgelehnt, solange ein Node nur 2 kann.
  - Nach dem „Update“ dieses Nodes (Neustart mit 3) steigt das Level, und der Befehl wird angewendet.
  - Ein Neustart mit 2 danach scheitert mit klarer Meldung.
  - Ein Join mit 2 wird abgelehnt.
- Ein echter Binary-Test n → n+1 (vorheriges Release-Tag bauen, Voter für Voter tauschen) kommt in die Release-Pipeline, sobald es ein erstes Release gibt.

## Konsequenzen

- **Positiv:**
  - Rolling Updates ohne Anhalten alter Nodes.
  - Neue Funktionen schalten sich automatisch frei, sobald alle Voter aktualisiert sind.
  - Downgrades und übersprungene Minor-Versionen werden beim Start mit klarer Meldung abgefangen, statt erst in der FSM.
  - Eine eindeutige Grenze, ab wann Contract-Migrationen erlaubt sind.
- **Negativ / Risiken:**
  - Nach dem Anheben des Levels ist ein Rollback nur über ein Backup möglich. Das muss in der Betriebsdoku deutlich stehen.
  - Disziplin bei jeder Änderung an `Apply`: Eine vergessene neue Mindestversion führt wieder zu stillen Abweichungen. Gegenmittel sind der Tabellentest je `oneof`-Fall, ein Review-Punkt in der PR-Vorlage und der Fingerprint-Vergleich (AP-3.6).
  - Während eines Updates antworten neue Funktionen mit 503.
- **Abgrenzung:** Probes melden keine Befehlsversion, weil sie keine Raft-Befehle anwenden. Ihre Kompatibilität regelt der Probe-Vertrag (P5).
- **Folgearbeiten:**
  - Betriebsdoku „Update eines Clusters“ (AP-2.6).
  - PR-Vorlage mit Prüfpunkt „Apply geändert → Mindestversion“.
  - Binary-Upgrade-Test in der Release-Pipeline.
  - Systemalert bei gemischten Versionen (AP-3.6).
  - Optional ein Admin-Schalter, der das Anheben zurückhält (Option C).

## Approval

| Datum | Status | Durch | Anmerkung |
|-------|--------|-------|-----------|
| 2026-10-23 | Proposed | Claude | |
