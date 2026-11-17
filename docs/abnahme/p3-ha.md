# Abnahme P3 – HA-Logik (AP-3.7)

**Stand:** 2026-11-17. Erste Messung auf `385dba8`, Nachmessung nach ADR 0026 auf `7e2ba9e` mit dem Prüfer aus `066dc84`; Szenarien in `9437e30`.

**Messumgebung:**
- Apple M4 Pro (Notebook), Podman 5.8 rootless in einer VM mit 4 CPUs und 7,5 GB.
- Harness `test/chaos`, siehe [p2-cluster.md](p2-cluster.md): 3 Voter als Container, Netzfehler über eine Sidecar mit iptables im Netz-Namespace jedes Voters, Fake-Ziel und Fake-ntfy auf dem Host.
- Monitore: HTTP gegen das Fake-Ziel, Intervall 20 s, retries 1, Zuweisung `all`. Damit liegt die Grenze aus NFA-REL-03 bei (1 + 1) × 20 s + 30 s = **70 s**.

**Aufruf:**

```bash
go run ./test/chaos/cmd/chaos -scenarios as04,as04nb,as05
```

`as04nb` ist AS-04 mit `ALERT_BUNDLE_WINDOW=0s`.

## Ergebnis

| ID | Kriterium | Gemessen | Ergebnis |
|----|-----------|----------|----------|
| AS-04 | Partition 2\|1, Ziel fällt aus: Mehrheit alarmiert bestätigt, Minderheit „unbestätigt“; nach Heilung Korrektur oder Bestätigung und identische Daten | Minderheit nach 17 s degraded (Partition = sie selbst), Mehrheit behält ihren Leader. Je Monitor genau ein bestätigter DOWN von der Mehrheit und ein DOWN der Minderheit mit „⚠ unconfirmed – cluster degraded (1/3 nodes)“ und deren Sicht-Zeile; kein bestätigter DOWN von der Minderheit. Nach der Heilung: Leader auf allen Votern nach 2,2 s, Minderheit nach 3 s nicht mehr degraded, beide unbestätigten Alerts nach 19 s beurteilt, kein „False alarm“, keine zusätzliche Bestätigung (der Kanal hatte den bestätigten Alert schon), kein wiederholter DOWN, Snapshots gleich; nach der Wiederherstellung des Ziels je Monitor genau ein UP | erfüllt |
| NFA-REL-03 | DOWN-Alert spätestens (retries + 1) × Intervall + 30 s nach dem Ausfall | Vor ADR 0026 mit Bündelung: zweiter Monitor 75–86 s (3 von 3 Läufen über der Grenze). Nach ADR 0026 mit Bündelung (Standard 30 s), 4 gültige Läufe: jeder bestätigte DOWN nach 41–61 s, je Monitor genau einer. Ohne Bündelung (2 Läufe): 51–61 s | erfüllt (Befund 1 behoben) |
| NFA-REL-06 | Quorumverlust: weiter messen, lesen, unbestätigt alarmieren; automatische Konvergenz | siehe AS-04; die Konvergenz kam ohne Eingriff | erfüllt |
| AS-05 | Node mit gesperrtem Egress: kein DOWN-Alarm für Ziele, Systemalert „Node unhealthy“ | Sperre nur zu Ziel und Referenzziel, Clusterverkehr frei. Node nach 46 s `impaired` (Grund `reference`), Systemalert `node_impaired` nach 63 s auf dem Systemkanal; Raft-Gesundheit unverändert, kein anderer Node ausgeschlossen, kein Leaderwechsel; 0 DOWN für die `majority`-Monitore. Nach der Freigabe nach 2 min 16 s wieder `ok`, Entwarnung nach 2 min 32 s; Snapshots gleich | erfüllt (Einschränkung 1) |

## Befunde und Korrekturen

1. **Bündelung verzögert den zweiten DOWN über die Grenze von NFA-REL-03.**
   - **Ablauf:** Der Dispatcher schickt den ersten fälligen Alert eines Kanals sofort. Alle weiteren, die innerhalb von `ALERT_BUNDLE_WINDOW` fällig werden, hält er bis zum Ende des Fensters zurück (ADR 0023). Ein „Bündel“ aus einem einzigen Alert geht als normale Nachricht raus.
   - **Folge:** Fällt ein zweiter Monitor wenige Sekunden nach dem ersten aus, kommt sein Alert bis zu 30 s später.
   - **Messung:**
     - Ohne Fenster liegen zwischen dem Statuswechsel (zurückdatiert auf den Beginn der ersten fehlgeschlagenen Runde) und der Zustellung etwa 41 s: eine weitere Runde (20 s), dann Check-Timeout und Meldefrist von 5 s. Das liegt innerhalb der Grenze.
     - Mit Fenster sind es 62 s.
   - **Entscheidung:** Projektinhaber hat entschieden, dass DOWN-Alerts nie zurückgehalten werden (ADR 0026). Ein fälliger DOWN schickt sofort alles, was für den Kanal fällig ist; das Fenster bleibt bestehen, andere Ereignisse werden weiter gebündelt.
   - **Behoben** in `7e2ba9e`, auch für den lokalen Dispatcher des Degraded-Modus.
   - **Nachmessung:** Jeder bestätigte DOWN kam innerhalb der Grenze an. Fallen zwei Monitore im selben Durchlauf aus, gehen ihre DOWN als ein Bündel „2 status changes“ raus. Das verzögert nichts.
2. **Systemkanal direkt nach dem Bootstrap:** `POST /api/v1/admin/system-channels` antwortete mit 404, bis der Leader den Mandanten `_system` angelegt hatte. Jetzt kommt 503 mit `Retry-After` (`385dba8`).
3. **Grenze im Szenario zu streng:** Die erste Fassung von AS-05 erwartete den Systemalert innerhalb von 60 s. Laut ADR 0022 und 0025 sind etwa 30 s Hysterese plus eine Bewertungsrunde zu erwarten. Die Grenze liegt jetzt bei 2 min, gemessen wurden 63 s und 83 s.
4. **Prüfer erkannte Bündel nicht:** Nach ADR 0026 schlugen zwei Läufe fehl, weil der Prüfer Alerts nur am Titel einer Einzelnachricht erkannte. Zwei gleichzeitige DOWN oder UP in einer Bündelnachricht zählte er nicht mit. In einem dieser Läufe belegen die Logs des Leaders, dass alle Alerts zugestellt wurden. Der Prüfer liest jetzt auch die Zeilen von Bündeln (`066dc84`). Im ersten dieser Läufe fehlten beide UP; die Logs waren dort schon gelöscht, die Ursache ist dieselbe Erklärung, aber nicht belegt.
5. **Während P3 gefunden und behoben:**
   - Der Aggregator des Degraded-Absenders bekam keine Monitore (`9e8e996`).
   - Ein neuer Leader wartete vor der Nachbewertung nicht auf erreichbare Peers (`ae0c905`).
   - Ein Voter, der unter neuer Adresse Leader wurde, meldete seine Adresse nicht (`7ee828f`).
   - Der Standard-Webhook enthielt das `system`-Objekt nicht (`1fce59f`).

## Einschränkungen

1. **`any`-Monitor in AS-05:** Bei der Konsens-Regel `any` kann bis zum Ausschluss des Nodes ein falsches DOWN entstehen, laut ADR 0022 akzeptiert. In einem frühen Lauf trat es auf (DOWN nach 57 s, Ausschluss nach 68 s, UP 1 s danach), im abgenommenen Lauf nicht. Monitore mit `majority` blieben in allen Läufen ohne DOWN.
2. **Urteil nicht direkt beobachtet:** Die Urteile über unbestätigte Alerts sind nur indirekt geprüft: kein „False alarm“ in ntfy und die Zahl der Urteile im Leader-Log. Die API-Snapshots des Harness enthalten `unconfirmed_alerts` nicht. Dass die Tabelle auf allen Votern gleich ist, prüft der Integrationstest `TestDegradedModeAlertsAndCorrects` über die FSM-Fingerprints.
3. **Nur 2\|1 getestet:** Ein vollständiger Quorumverlust (1\|1\|1) mit Nachbewertung der Lücke ist nur im Integrationstest abgedeckt, nicht im Chaos-Harness.
4. **Einzelmessungen:** Jede Zeit stammt aus einem einzelnen Lauf. AS-04 lief vor ADR 0026 dreimal mit und zweimal ohne Bündelung, danach siebenmal (davon vier mit dem korrigierten Prüfer oder mit Einzelnachrichten auswertbar, alle erfüllt). AS-05 lief zweimal.
6. **AS-05 zählt Systemalerts nur über den Titel:** Ein Systemalert in einer Bündelnachricht würde dort nicht erkannt. In den Läufen kam jeder Systemalert einzeln.
5. **Nur Podman geprüft:** Docker ist im Harness verdrahtet, aber nicht getestet.
