# ADR 0039: CA-Rotation mit überlappendem Trust-Bundle

| | |
|---|---|
| **Status** | Proposed |
| **Datum** | 2026-11-19 |
| **Entscheider** | Projektinhaber |
| **Bezug** | FA-PKI-01..06, FA-OBS-01/03/04, PF-10, PF-11, NFA-SEC-01, E9, ADR 0006, ADR 0011, ADR 0012, ADR 0018, ADR 0019, ADR 0025, AP-2.1, AP-5.1, AP-5.5 |

## Kontext

FA-PKI-05 verlangt eine CA-Rotation mit überlappendem Trust-Bundle. Das Verfahren muss dokumentiert sein, und das Runbook muss getestet sein (Priorität S, P5). Die Rotation ist aus zwei Gründen nötig:

- **Regulär:** Die Cluster-CA läuft 10 Jahre (`pki.CAValidity`). Ein Schlüssel soll nicht unbegrenzt leben. Vor dem Ablauf muss eine neue CA bereitstehen, sonst fällt jede mTLS-Verbindung aus.
- **Kompromittierung:** Der CA-Schlüssel ist abgeflossen, z.B. aus einem Backup oder Export zusammen mit dem KEK. Dann muss die alte CA so schnell wie möglich das Vertrauen verlieren.

**Stand im Code (AP-2.1/2.2):**

- `internal/pki`:
  - ECDSA-P-256-CA mit `MaxPathLenZero`. Sie signiert Blattzertifikate direkt, ohne Zwischen-CA.
  - Blätter laufen 90 Tage, `NotBefore` −5 min, Erneuerung bei 2/3 der Laufzeit. `NotAfter` wird auf das `NotAfter` der CA gekappt.
  - Die Identität steht als URI-SAN im Blatt: `uc://<cluster>/<rolle>/<node>[?tenant=]`.
- **Fingerprint:** `pki.Fingerprint` ist der SHA-256 über die SPKI. Er überlebt ein Neusignieren mit demselben Schlüssel.
- **`pki.Verifier`:**
  - Liest `Roots()` bei jedem Handshake neu und baut daraus einen Pool. Zwischenzertifikate gibt es nicht.
  - Prüft Kette, Verwendung, Sperrliste (Serien und Nodes), Cluster-ID und Rolle in `VerifyConnection`.
  - TLS 1.3, Session-Tickets aus.
  - `ServerConfig`/`ClientConfig` liefern das eigene Zertifikat über `GetCertificate`/`GetClientCertificate` aus einem `atomic.Pointer` (`pki.Holder`).
  - **Hot-Reload ist für Blatt und Trust-Bundle also schon da.** Er wirkt auf neue Handshakes. Bestehende Verbindungen leben bis `MaxConnectionAge` (1 h: Raft über `agedConn`, gRPC über `keepalive.ServerParameters` mit 30 s Grace).
- **Raft-Zustand:**
  - `cluster_ca(fingerprint, cluster_id, cert_der, key_enc, created_ms)`. Der Schlüssel ist nach ADR 0011 verschlüsselt, AAD `cluster_ca/<fp>/key`.
  - `applyPutClusterCA` lehnt eine zweite CA mit `conflict` ab („Replacing the CA is rotation, which comes with P5“).
- **`clusterpki.Authority`:**
  - `current()` und damit der Signierer ist die **älteste** Zeile (`ORDER BY created_ms`).
  - `Bundle()` liefert alle Zeilen. Die Cluster-ID kommt aus der ersten.
  - `CreateJoinToken` und `membership.Service.Join` nehmen den Fingerprint von `bundle[0]`.
  - `Redeem` akzeptiert jeden Fingerprint aus dem Bundle.
- **Join:**
  - `pki.JoinClientConfig` verlangt, dass der angesprochene Voter neben seinem Blatt die CA mit dem Fingerprint des Tokens schickt.
  - Das Blatt muss **direkt von dieser CA** signiert sein.
- **`NodeCert`:**
  - Speichert `leaf + ausstellende CA` in `DATA_DIR/tls/node-cert.pem`. Das gelieferte `BundleDER` speichert es **nicht**.
  - Ein Voter nimmt das Bundle aus dem Raft-Zustand. Vor dem ersten Zustand nimmt er nur die eigene ausstellende CA.
  - Eine Probe hätte heute nur ihre ausstellende CA als Vertrauensanker.
- **Erneuerung:**
  - Prüfung stündlich (`renewalCheck`), ausgelöst nur durch die Laufzeit.
  - Follower erneuern per `RenewCertificate` beim Leader. Der Leader nimmt die Identität aus dem aktuellen, per mTLS geprüften Blatt.
- **Ablaufwarnung:** `sysalert.caVerdict` bewertet nur `cas[0]` mit 30/7/1 Tagen (FA-PKI-06).
- **Seriennummern:** Ausgestellte Serien werden nirgends erfasst. Die Roadmap führt das bei AP-2.1 als offen.

**Randbedingungen:**

- **Rolling Updates (ADR 0018):** Neue FSM-Befehle brauchen eine neue Befehlsstufe. Ein älteres Binary würde eine zweite CA anders behandeln, nämlich mit `conflict`, und der Zustand liefe auseinander. Das Schema wird nach Expand/Contract erweitert.
- **`Apply` ist deterministisch:** Schlüssel und Zertifikate entstehen außerhalb der FSM auf dem Leader. Uhrzeiten kommen nur aus `issued_at_ms`.
- **Probes (P5, noch nicht gebaut):** Sie haben keinen Raft-Zustand, stehen hinter NAT und können lange offline sein. Ihr Trust-Bundle müssen sie über den Probe-Vertrag bekommen.
- **Join-Tokens:** Sie pinnen den CA-Fingerprint (FA-PKI-02, PF-10). Ein Token darf durch die Rotation nicht zur Hintertür werden.

**Vergleich mit anderen Systemen (Recherche):**

| System | Verfahren | Bemerkung |
|---|---|---|
| Kubernetes (manuell) | `ca.crt` als Bundle alt+neu an alle Komponenten, dann Blätter neu ausstellen, dann alte CA entfernen. Mehrere Neustarts. | Klassisches Drei-Phasen-Bundle. Talos nennt die Phasen „accepted → issuing → remove accepted“, StarlingX `trust-both-cas`. |
| Linkerd | Trust-Anchor-Bundle alt+neu ausrollen, Issuer von der neuen Wurzel ausstellen, Data Plane neu starten, alte Wurzel entfernen, erneut neu starten. | Bundle, kein Cross-Signing. Buoyant warnt vor zu frühem Entfernen der alten Wurzel. |
| CockroachDB | Kombinierte `ca.crt` (neu, dann alt) an alle Nodes und Clients, danach Node- und Client-Zertifikate rotieren, dann alte CA entfernen. | Bundle. Reload per SIGHUP. |
| Consul Connect | Neue Wurzel wird **vom alten Schlüssel cross-signiert**. Blätter der neuen Wurzel werden mit dem Cross-Zertifikat ausgeliefert, damit Proxys mit altem Bundle sie noch prüfen können. Ohne Cross-Signing nur mit `ForceWithoutCrossSigning` und kurzer Störung. | Cross-Signing lohnt, weil Proxys ihr Bundle nicht synchron bekommen. |
| Nomad | Alle Zertifikate müssen von derselben CA stammen. TLS-Reload per SIGHUP, neue Verbindungen nehmen die neue Konfiguration, alte behalten die bisherige. | Kein eingebautes Rotationsverfahren. |
| etcd | Trust-Datei mit alt+neu, Reload nur für neue Verbindungen sicher, sonst rollierender Neustart je Member. | Bundle, Phasen je Member. |

Gemeinsames Muster: **erst vertrauen, dann signieren, dann entfernen**. Jede Phase beginnt erst, wenn alle Teilnehmer die vorige übernommen haben. Cross-Signing setzen nur Systeme ein, deren Clients das Bundle nicht zuverlässig bekommen.

**Go-Details:**

- Mit `GetCertificate`, `GetClientCertificate` und `VerifyConnection` mit dynamischem Pool ist ein Neustart unnötig. Ein `GetConfigForClient` ist nicht nötig.
- `x509.Verify` erzwingt `MaxPathLen` auch für den Anker. Ein vom alten CA-Zertifikat (`MaxPathLenZero`) cross-signiertes neues CA-Zertifikat wäre als Zwischenzertifikat ungültig (`TooManyIntermediates`). Cross-Signing setzt also voraus, die alte CA mit Pfadlänge 1 neu zu signieren und dieses Zertifikat zuerst überall zu verteilen. Das ist selbst eine Bundle-Verteilung.

## Optionen

### Option A: Drei-Phasen-Bundle über Raft (trust → issue → retire), ohne Cross-Signing
- Eine neue CA wird als `next` in den Raft-Zustand geschrieben. Alle vertrauen ihr, sie signiert aber noch nicht. Danach wird sie `active` und signiert, die alte wird `previous`. Alle Blätter werden neu ausgestellt. Zuletzt wird `previous` entfernt.
- Vorteile:
  - Entspricht Kubernetes, Linkerd, CockroachDB und Talos.
  - Voter bekommen das Bundle ohnehin synchron und deterministisch über Raft. Die Lücke, die Cross-Signing schließt, haben wir für Voter nicht.
  - Der Verifier bleibt ohne Zwischenzertifikate. Die Join-Pinning-Logik bleibt unverändert.
  - Ein Rollback ist bis zum Entfernen von `previous` ein einfacher Rollentausch.
- Nachteile:
  - Probes brauchen einen eigenen Weg für das Bundle (Probe-Vertrag).
  - Zwischen „aktiv“ und „alle erneuert“ gibt es ein kurzes Fenster, in dem ein Voter noch ein altes Blatt zeigt. Join-Tokens zur neuen CA scheitern an diesem Voter, also muss die Token-Ausgabe kurz warten.

### Option B: Cross-Signing (Consul-Stil)
- Die neue CA wird zusätzlich vom alten Schlüssel signiert. Blätter der neuen CA werden mit diesem Zwischenzertifikat ausgeliefert, damit Teilnehmer mit altem Bundle sie prüfen können.
- Vorteile:
  - Teilnehmer mit veraltetem Bundle, etwa lange offline Probes, prüfen neue Blätter trotzdem, solange die alte CA gilt.
  - Join-Tokens zur alten CA bleiben während der Rotation brauchbar.
- Nachteile:
  - Die alte CA hat `MaxPathLenZero`. Sie müsste zuerst mit Pfadlänge 1 neu signiert und verteilt werden, was eine Bundle-Rotation vor der Rotation bedeutet.
  - Der Verifier und `JoinClientConfig` müssen Zwischenzertifikate zulassen. Das bringt mehr Angriffsfläche in den sicherheitskritischen Pfad, etwa Pfadaufbau und mehrere Ketten.
  - Im Kompromittierungsfall ist Cross-Signing schädlich: Der kompromittierte alte Schlüssel bürgt dann für die neue Kette.
  - Nach dem Entfernen der alten CA hilft es nicht mehr. Lange offline Probes müssen trotzdem neu beitreten.

### Option C: Nur Verlängerung (gleicher Schlüssel, neues CA-Zertifikat)
- Das CA-Zertifikat wird mit demselben Schlüssel und späterem `NotAfter` neu signiert. Fingerprint und Tokens bleiben gleich.
- Vorteile: Trivial, keine Phasen, kein Neuausstellen nötig.
- Nachteile:
  - Der Schlüssel wird nicht gewechselt. Gegen Kompromittierung hilft das nicht, und es erfüllt FA-PKI-05 nicht.
  - Der Schlüssel lebt unbegrenzt.

### Option D: Re-Bootstrap (neuer Cluster, alle Nodes treten neu bei)
- Vorteile: Kein neuer Code, sauberer Schnitt.
- Nachteile: Ausfall, Verlust lokaler Daten auf den neu beitretenden Nodes (wie bei ADR 0019) und viel Handarbeit. Kein „überlappendes Trust-Bundle“.

### Option E: Zweistufige Hierarchie (Offline-Root, Online-Zwischen-CA)
- Vorteile: Die Rotation der Zwischen-CA ändert keine Vertrauensanker. Der Root-Schlüssel liegt nicht im Raft-Zustand.
- Nachteile:
  - Der Betreiber muss einen Offline-Root verwalten, was dem Ziel des Selbsthostens ohne PKI-Wissen widerspricht.
  - Das Modell aus ADR 0006 ändert sich: Pfadlänge, Zwischenzertifikate im Verifier, Pinning im Join-Token.
  - Die Root-Rotation bleibt trotzdem nötig.

## Entscheidung

**Option A.** Option D bleibt der dokumentierte letzte Ausweg, wenn ein Voter selbst kompromittiert ist (siehe 7). Option B wird nicht umgesetzt. Option E ist eine offene Frage für später.

**1. Datenmodell (Expand nach ADR 0018)**
- `cluster_ca` bekommt die Spalte `state TEXT NOT NULL DEFAULT 'active'` mit den Werten `next`, `active` und `previous`. Dazu kommt `activated_ms INTEGER`, nullable.
  - Bestehende Zeilen bleiben `active`.
  - Höchstens zwei Zeilen: genau eine `active` und höchstens eine `next` oder `previous`.
- **Invarianten in `Apply`:**
  - Alle Zeilen haben dieselbe `cluster_id`.
  - Jede CA ist `IsCA`, hat `MaxPathLenZero`, P-256 und einen verschlüsselten Schlüssel.
  - Der Fingerprint passt zum Zertifikat.
- **Neue Tabelle `issued_certs(serial, node_id, ca_fingerprint, not_after_ms, issued_ms)`.**
  - Der Leader erfasst damit jedes ausgestellte Blatt per Raft-Befehl, bevor er es herausgibt.
  - Einträge werden nach `not_after_ms` bereinigt, wie die Sperrliste.
  - Das schließt die offene AP-2.1-Folgearbeit „Serien beim Ausstellen erfassen“ und ist die Grundlage für den Notfallmodus (siehe 7).
  - Die Tabelle kommt in den FSM-Fingerprint, die Spalten von `cluster_ca` ebenfalls.
- **Lesepfade:**
  - `Authority.signer`, `CreateJoinToken`, `Join` und `sysalert.caVerdict` nehmen die Zeile mit `state='active'`, nicht mehr `cas[0]`.
  - `Bundle()` liefert `active` zuerst, dann die übrigen.

**2. Raft-Befehle** (neue `oneof`-Payloads mit Befehlsstufe **13**; Vergabe der P5-Stufen siehe Roadmap P5)

| Befehl | Phase | Wirkung in `Apply` | Ablehnung |
|---|---|---|---|
| `AddClusterCA{fingerprint, doc, emergency}` | 1 „trust“ | Legt die CA als `next` an. Ab dem Anwenden vertraut ihr jeder Voter beim nächsten Handshake. | Es gibt schon `next` oder `previous`; andere Cluster-ID; ungültiges Zertifikat; unverschlüsselter Schlüssel |
| `ActivateClusterCA{fingerprint}` | 2 „issue“ | Ziel `next` → `active`, bisher `active` → `previous`, `activated_ms = issued_at_ms`. **Löscht alle unbenutzten Join-Tokens.** Auch für den Rollback: Ziel `previous` tauscht zurück. | Ziel ist weder `next` noch `previous` |
| `RemoveClusterCA{fingerprint}` | 3 „retire“ oder Abbruch | Löscht die Zeile `next` (Abbruch vor der Aktivierung) oder `previous` (Abschluss) mitsamt Schlüssel-Chiffrat. Löscht die zugehörigen `issued_certs`. | Ziel ist `active` |
| `RecordIssuedCertificate{serial, node_id, ca_fingerprint, not_after_ms}` | jede Ausstellung | Erfasst die Serie | unbekannte CA |

- Die Bootstrap-CA entsteht weiter mit `PutClusterCA` (Version 1). Es entsteht eine Zeile `active`.
- **Rolling Updates:** Die Befehle sind erst erlaubt, wenn das Level angehoben ist. Während eines Rolling Updates antwortet `admin ca …` mit 503 „Update läuft“ (ADR 0018, Abschnitt 4). Nach dem Anheben startet kein Binary ohne diese Befehle mehr. Ein Downgrade mitten in der Rotation ist damit ausgeschlossen.
- Ein Rolling Update auf n+1 *während* einer Rotation ist erlaubt. Ein neu gestarteter Node lädt sein Blatt von der Platte und das Bundle aus dem Zustand und erneuert bei Bedarf sofort (siehe 3).
- **Schlüsselerzeugung:** Nur der Leader erzeugt Schlüssel und Zertifikat (`pki.NewCA` mit derselben Cluster-ID, `crypto/rand`). Er verschlüsselt sie mit dem Keyring (ADR 0011, AAD `cluster_ca/<fp>/key`) und schlägt erst dann vor. `Apply` sieht nur das Chiffrat.

**3. Verhalten außerhalb der FSM**
- **Erneuerungsauslöser:**
  - `NodeCert.NeedsRenewal` gilt zusätzlich, wenn das eigene Blatt nicht von der `active`-CA stammt (Vergleich `AuthorityKeyId`/Signatur mit der aktiven CA).
  - Voter prüfen das im 10-s-Takt zusammen mit dem Neuladen der Sperrliste, mit Jitter 0–30 s. Der Leader erneuert sich sofort nach dem Anwenden von `ActivateClusterCA`.
- **Erneuerung über die CA-Grenze:**
  - `RenewCertificate` mit einem Blatt der `previous`-CA ist erlaubt, wenn die Serie in `issued_certs` steht.
  - Ein mit dem alten Schlüssel selbst geprägtes Blatt bekommt so kein Zertifikat der neuen CA.
  - Für Blätter von vor dieser Änderung (keine Serie erfasst) gilt eine Übergangsregel: Im regulären Modus reichen Node-ID im Raft-Register und gültige Kette. Im Notfallmodus wird abgelehnt.
- **Join-Tokens:**
  - Sie pinnen immer die `active`-CA.
  - Zwischen `ActivateClusterCA` und dem Zeitpunkt, an dem alle Voter der Raft-Konfiguration ein Blatt der aktiven CA melden, lehnt `CreateJoinToken` mit `409 rotation in progress` ab. Sonst würde ein Join an einem noch nicht erneuerten Voter am Pinning scheitern.
  - Alte Tokens löscht `ActivateClusterCA` (siehe 2). Ein Token zur alten CA ist so nie mehr einlösbar, auch nicht im Kompromittierungsfall, in dem ein Angreifer mit dem alten Schlüssel einen Voter vortäuschen könnte.
- **Trust-Bundle auf der Platte:** `NodeCert.Install` speichert `BundleDER` zusätzlich als `DATA_DIR/tls/ca-bundle.pem`. Bei Probes ist diese Datei die Quelle von `roots()`. Bei Votern ist sie die Rückfallquelle vor dem ersten Raft-Zustand.
- **Bestehende Verbindungen:**
  - Beim Anwenden von `RemoveClusterCA` schließt jeder Node aktiv alle Verbindungen, deren Peer-Blatt von der entfernten CA stammt. Dafür führt `clusternet` ein Verbindungsregister mit Peer-Blatt.
  - Ohne das würde eine Verbindung bis zu 1 h (`MaxConnectionAge`) mit altem Vertrauen weiterlaufen.
  - Raft und gRPC wählen danach neu und handshaken mit aktuellem Bundle.
- **Ablauf von Blättern:** Das `NotAfter` eines Blatts bleibt auf das `NotAfter` seiner ausstellenden CA gekappt. Eine neue CA wird mit voller Laufzeit erzeugt.

**4. Phasenmodell und Vorbedingungen**

Die Rotation steuert der Leader. Zwischen den Phasen prüft er Vorbedingungen. Diese prüft der Leader selbst, nicht die FSM, denn sie hängen von Live-Zustand ab. Die Prüfung läuft nach jedem Leaderwechsel weiter, weil die Phase im Raft-Zustand steht (`next` vorhanden bzw. `previous` vorhanden).

```
            AddClusterCA            ActivateClusterCA             RemoveClusterCA(previous)
 [CA1 active] ─────────▶ [CA1 active, CA2 next] ─────────▶ [CA2 active, CA1 previous] ─────────▶ [CA2 active]
                 │  ▲                               │  ▲
   RemoveClusterCA(next)                ActivateClusterCA(CA1) = Rollback
                 ▼                                  ▼
            [CA1 active]                 [CA1 active, CA2 next]
```

| Übergang | Vorbedingung (Leader prüft, `--force` übergeht) |
|---|---|
| Phase 1 → 2 | Jeder Voter der Raft-Konfiguration meldet in `GetStatus` ein `applied_index ≥` Index von `AddClusterCA` und die neue CA in `trust_bundle_fingerprints`. Jede bekannte, verbundene Probe meldet die neue CA im Bundle. Mindestverweildauer 2 min. |
| Phase 2 → 3 | Jeder Voter meldet `cert_issuer_fingerprint = active`. Jede bekannte Probe ebenfalls, oder sie ist als offline markiert und wird im Status ausdrücklich genannt. Danach mindestens `MaxConnectionAge + Grace` gewartet **oder** das aktive Schließen der Verbindungen (3) ist vorhanden. Im Standard wird gewartet, als doppelte Sicherung. |

- **Warum 1 → 2 auf den angewendeten Index wartet:** Ein Follower, der `AddClusterCA` noch nicht angewendet hat, würde das neue Blatt des Leaders ablehnen. Er bekäme dann keine `AppendEntries` mehr und könnte nie aufholen.
- **Fortschaltung:**
  - Im Standard schaltet die Leader-Schleife (wie `SetCommandLevel`, alle 30 s und bei Änderungen) die Phasen automatisch weiter, sobald die Vorbedingungen gelten.
  - Mit `--manual` wartet sie in jeder Phase auf `admin ca activate` bzw. `admin ca finish`.
  - Der Modus steht im Dokument von `AddClusterCA`, damit ein neuer Leader ihn kennt.
- **Dauer regulär:** etwa 1–2 h. Den größten Teil macht das Warten auf `MaxConnectionAge` aus.

**5. Bedienung (Erweiterung der Admin-CLI nach ADR 0019)**
- **Online-Befehle über den Cluster-Port** mit dem Node-Zertifikat (Rolle Voter), wie `reset-password`, über ein neues RPC `ClusterService.RotateCA{action, fingerprint, force, emergency, manual}`. Ein Follower leitet an den Leader weiter. Nur Voter-Zertifikate dürfen es aufrufen.
  - `uptimecluster admin ca status [--json]`: Phase, CAs mit Fingerprint, Zustand und `NotAfter`, je Node Aussteller des Blatts und Bundle, offene Vorbedingungen.
  - `uptimecluster admin ca rotate [--manual] [--emergency]`: Phase 1. Ohne `--yes` nur Vorschau.
  - `uptimecluster admin ca activate [--force]`, `uptimecluster admin ca finish [--force]`: manuelle Schritte.
  - `uptimecluster admin ca abort`: Abbruch vor `finish`. Vor der Aktivierung entfernt er `next`. Nach der Aktivierung tauscht er zurück (`ActivateClusterCA(previous)`), danach `RemoveClusterCA(next)`.
- **Admin-API:** nur lesend, `GET /api/v1/admin/cluster/ca` für `platform-admin`, mit Anzeige in der Cluster-Ansicht. Schreibende Aktionen gibt es nur über die CLI. Begründung: selten, folgenreich, setzt Zugriff auf einen Voter voraus.
- **Audit:** Akteur `KIND_SYSTEM`, ID `cli:<node-id>`, im Audit-Log (ADR 0031).

**6. Probes (Vorgabe für AP-5.1)**
- Die Probe speichert das Bundle aus `JoinResponse`/`RenewCertificateResponse` (`ca_bundle_der`) in `ca-bundle.pem` und vertraut nur diesem.
- **Abgleich des Bundles:**
  - Der Voter schickt im Subscribe-Stream (Zuweisungen bzw. Keepalive) `active_ca_fingerprint` und `trust_bundle_fingerprints` mit.
  - Weicht das von der Probe ab, ruft sie `RenewCertificate` auf. Die Antwort enthält das neue Bundle und, ab Phase 2, ein Blatt der neuen CA.
  - Die Probe meldet ihren Aussteller und ihr Bundle im Hello bzw. Heartbeat. Der Leader nutzt das für die Vorbedingungen.
- **Eine Probe, die über die ganze Rotation offline war,** vertraut dem neuen Voter-Blatt nicht, und ihr altes Blatt wird abgelehnt. Sie muss mit neuem Token neu beitreten. Der Status nennt sie, und der Probe-Offline-Alert (P5) meldet sie.
- **Kein Cross-Signing für Probes:** Bewusst in Kauf genommen. Probes sind zustandsarm, ein Neubeitritt kostet nur einen Token.

**7. Sicherheit: reguläre Rotation vs. Kompromittierung**
- **Reguläre Rotation:** Überlappung so lange wie nötig, automatische Phasen, Rollback bis `finish`. Der alte Schlüssel wird mit `RemoveClusterCA` aus dem Zustand gelöscht.
  - Das Chiffrat bleibt in älteren Raft-Snapshots, im Log bis zur Kompaktierung und in Backups. Das ist dokumentiert. Bei regulärer Rotation ist es unkritisch, weil der alte Schlüssel kein Vertrauen mehr hat.
- **Notfallmodus (`--emergency`)** bei abgeflossenem CA-Schlüssel ohne Hinweis auf einen kompromittierten Voter:
  - Phase 1 und 2 folgen unmittelbar aufeinander: `ActivateClusterCA` direkt nach Anwenden von `AddClusterCA` auf allen Votern, ohne Wartezeit für Probes.
  - Ab `AddClusterCA{emergency}` vertraut der Verifier der `previous`-CA **nur noch für Serien aus `issued_certs`** (Allowlist). Ein Angreifer mit dem alten Schlüssel kann sich damit schon in der Überlappung nicht mehr verbinden.
  - Erneuerung über die CA-Grenze gibt es nur für erfasste Serien. Ohne Serie wird nicht erneuert, auch nicht mit Übergangsregel.
  - `finish` ist erlaubt, sobald alle Voter erneuert sind, ohne Wartezeit auf `MaxConnectionAge`. Das aktive Schließen der Verbindungen ist dafür Pflicht. Nicht erneuerte Probes müssen neu beitreten.
  - Kein Rollback auf die kompromittierte CA (`abort` ist im Notfallmodus gesperrt).
  - Alle unbenutzten Join-Tokens sind gelöscht. Neue Tokens pinnen die neue CA.
- **Was CA-Rotation nicht löst:**
  - Ist ein Voter kompromittiert (Zugriff auf `DATA_DIR`), hat der Angreifer KEK, Node-Schlüssel und eine echte, erfasste Serie. Die Rotation schließt ihn dann nicht aus.
  - Runbook dafür: betroffenen Node mit `RemoveNode` entfernen und sperren (FA-PKI-04), dann Notfallrotation, KEK-Rotation (ADR 0011), Rotation aller Geheimnisse (Kanal-URLs, OIDC-Client-Secret, JWT-Schlüssel). Bei Unklarheit, welcher Node betroffen ist: Re-Bootstrap (Option D).
  - Das sagt das Runbook ausdrücklich, damit die Rotation keine falsche Sicherheit erzeugt.
- **Sperrliste:** Die Sperrliste (FA-PKI-04) hilft gegen einen kompromittierten CA-Schlüssel nicht, denn der Angreifer wählt Serien und Node-IDs frei. Dafür ist die Allowlist da.
- **Backups (AP-5.5):** Ein vor der Rotation erstelltes Backup enthält die alte CA mit Schlüssel-Chiffrat.
  - Ein Restore setzt das Vertrauen auf die alte CA zurück. Alle Nodes mit neuen Blättern sind dann fremd und müssen neu beitreten.
  - Im Kompromittierungsfall sind alte Backups als kompromittiert zu behandeln.
  - Der Export (FA-OBS-03) nimmt den Rotationszustand und `issued_certs` auf.

**8. Ablaufüberwachung (Ergänzung zu ADR 0025)**
- `cert_expiry` für die CA bewertet die `active`-CA, nicht `cas[0]`. Die Stufen 30/7/1 Tage bleiben (FA-PKI-06). Der Alert verweist auf das Runbook.
- **Neuer Systemalert `ca_rotation_stalled`:** Eine Rotation steht länger als 24 h in Phase 1 oder 2. Details sind die offenen Vorbedingungen und die Nodes, die sie blockieren. Lange Überlappung verlängert das Zeitfenster für die alte CA.
- **Prometheus:** `uptimecluster_ca_not_after_seconds{state}` und `uptimecluster_ca_rotation_phase` (0 = keine, 1 = trust, 2 = issue), ohne Mandanten-Labels.

**9. Tests und Runbook (Akzeptanz FA-PKI-05)**
- **Tabellentests je Befehl in `internal/cluster`:** Übergänge, Ablehnungen, Löschen der Tokens, Fingerprint, Determinismus.
- **Integrationstest in `internal/clusterhost`** mit 3 Votern unter Schreiblast:
  - Vollständige Rotation ohne Leaderverlust und ohne fehlgeschlagene Schreibbefehle (PF-11).
  - Nach `finish` wird ein Client mit altem Blatt abgewiesen. Ein alter Token ist ungültig, ein neuer Token funktioniert.
  - Rollback in Phase 2, Abbruch in Phase 1.
  - Neustart eines Voters in jeder Phase. Leaderwechsel in jeder Phase.
  - Ein Voter, der über die ganze Rotation gestoppt war, wird abgewiesen und tritt mit Recovery-Verfahren neu bei.
  - Notfallmodus: ein mit dem alten Schlüssel selbst geprägtes Voter-Blatt wird schon in der Überlappung abgewiesen und bekommt keine Erneuerung.
- Runbook `docs/betrieb/ca-rotation.md` mit Testprotokoll gegen einen Compose-Cluster (wie `docs/betrieb/recovery.md`).

## Konsequenzen

- **Positiv:**
  - FA-PKI-05 ist ohne Ausfall und ohne Neustart erfüllt. Hot-Reload nutzt die vorhandenen Callbacks.
  - Das Verfahren entspricht dem bewährten Muster von Kubernetes, Linkerd und CockroachDB. Es läuft über Raft deterministisch, Leaderwechsel und Rolling Updates übersteht es.
  - Der Verifier bleibt einfach: keine Zwischenzertifikate, Join-Pinning unverändert.
  - `issued_certs` schließt die offene AP-2.1-Folgearbeit und macht den Notfallmodus erst wirksam.
  - Ein klarer Unterschied zwischen regulärer Rotation und Kompromittierung, mit ehrlicher Grenze (kompromittierter Voter).
- **Negativ / Risiken:**
  - Neue Befehlsstufe. Rotation erst, wenn alle Voter aktualisiert sind.
  - Kurzes Fenster nach der Aktivierung ohne Ausgabe von Join-Tokens (409).
  - Lange offline Probes müssen nach einer Rotation neu beitreten.
  - Das alte Schlüssel-Chiffrat bleibt in alten Snapshots und Backups.
  - Zusätzlicher Raft-Eintrag je Zertifikatsausstellung. Bei 90-Tage-Blättern unerheblich.
  - Vorbedingungen hängen am Live-Status der Nodes. Ein nicht erreichbarer Voter blockiert die Rotation, bis `--force` sie übergeht. Mit `--force` riskiert der Betreiber, dass dieser Voter neu beitreten muss.
  - Mehr sicherheitskritischer Code: Allowlist, Erneuerung über die CA-Grenze, Verbindungsregister. Alles davon schreibt der Hauptagent.
- **Folgearbeiten:**
  - Proto: Befehle, RPC `RotateCA`, `GetStatus` um `cert_issuer_fingerprint` und `trust_bundle_fingerprints`.
  - Migration und FSM, `clusterpki`, Verbindungsregister in `clusternet`.
  - Admin-CLI und API-Status, Systemalert und Metriken.
  - Vorgaben für den Probe-Vertrag (AP-5.1).
  - Runbook und Integrationstests.
  - Hinweis in der Backup-Doku (AP-5.5).

## Offene Fragen an Projektinhaber

1. **Automatische Fortschaltung als Standard** (Vorschlag) oder jede Phase manuell bestätigen (`--manual` als Standard)?
2. **Laufzeit der CA:** 10 Jahre belassen oder auf z.B. 5 Jahre senken? Und eine empfohlene oder automatische reguläre Rotation, etwa jährlich, analog zur DEK-Rotation aus ADR 0011?
3. **Ablaufwarnung für die CA:** Reichen 30/7/1 Tage (FA-PKI-06), oder soll die CA zusätzlich 180/90 Tage vorher warnen? Eine Rotation dauert zwar nur Stunden, muss aber geplant werden.
4. **Übergangsregel für Blätter ohne erfasste Serie** (vor dieser Änderung ausgestellt): im regulären Modus erlauben, wie vorgeschlagen? Alternative: Die Einführung von `issued_certs` erneuert sofort alle Blätter einmal, dann entfällt die Regel.
5. **Schreibende Aktionen nur über die CLI** (Vorschlag) oder auch über Admin-API und UI für `platform-admin`?
6. **Option E (Offline-Root mit Zwischen-CA)** als spätere Härtung für den kommerziellen Betrieb vormerken, oder dauerhaft verwerfen?
7. **Algorithmuswechsel:** Soll die Rotation einen Wechsel auf Ed25519 erlauben? FA-PKI-01 lässt beides zu. Vorschlag: nein, P-256 beibehalten.

## Approval

| Datum | Status | Durch | Anmerkung |
|-------|--------|-------|-----------|
| 2026-11-19 | Proposed | Claude | |
