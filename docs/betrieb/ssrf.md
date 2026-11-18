# SSRF-Schutz

Dieses Dokument beschreibt den SSRF-Schutz von UptimeCluster: was er sperrt, wann er greift, welche Fehlerbilder auftreten und was der Betrieb zusätzlich tun muss (Pflichtenheft FA-SEC-SSRF, FA-ALR-12, FA-CHK-04; Entscheidung in [ADR 0030](../adr/0030-ssrf-durchsetzung.md)). Die Variable `SSRF_PROTECTION` ist in [compose.md](compose.md) beschrieben.

## 1. Überblick

Monitore und Benachrichtigungsziele sind vom Mandanten frei wählbar. Ohne Schutz könnte ein Mandant über einen HTTP-Check oder einen Webhook interne Dienste des Betreibers ansprechen, etwa den Cloud-Metadaten-Dienst oder Dienste im privaten Netz. Der Schutz verhindert das.

Der Schutz prüft die IP-Adresse, mit der sich eine ausgehende Verbindung tatsächlich verbindet, zum Zeitpunkt des Verbindungsaufbaus. Dadurch greifen auch DNS-Rebinding und Weiterleitungen (Redirects) auf interne Ziele.

Die Einstellung `SSRF_PROTECTION` steuert den Schutz:

| `AUTH_MODE`                   | `OIDC_PROFILE` | `SSRF_PROTECTION=auto` | `SSRF_PROTECTION=on` | `SSRF_PROTECTION=off` |
| ----------------------------- | -------------- | ---------------------- | -------------------- | --------------------- |
| `local`                       | –              | aus                    | **an**               | aus                   |
| `oidc`                        | `generic`      | aus                    | **an**               | aus                   |
| `oidc` oder `zitadel` (Alias) | `zitadel`      | **an**                 | **an**               | aus                   |

`auto` ist also nur im Zitadel-Profil an. `off` schaltet den Schutz auch dort ab; davon ist abzuraten, wenn Mandanten Ziele anlegen können.

Die Einstellung gilt je Node (Umgebungsvariable), nicht für den Cluster. Weichen die Voter ab, kann ein Ziel auf einem Node angenommen und auf einem anderen beim Check gesperrt werden. Die Cluster-Ansicht zeigt den Status je Node.

## 2. Gesperrte Bereiche

Die Liste ist fest im Code (`checker.BlockPrivate`, `internal/checker/dialer.go`) und lässt sich nicht konfigurieren. Es gibt bewusst keine Variablen für zusätzliche Sperren oder Freigaben (ADR 0030, Frage 1).

| CIDR              | Grund                                                 |
| ----------------- | ----------------------------------------------------- |
| `0.0.0.0/8`       | nicht spezifiziert (unspecified)                      |
| `10.0.0.0/8`      | privat                                                |
| `100.64.0.0/10`   | Carrier-Grade-NAT                                     |
| `127.0.0.0/8`     | Loopback                                              |
| `169.254.0.0/16`  | Link-Local, inkl. Cloud-Metadaten (`169.254.169.254`) |
| `172.16.0.0/12`   | privat                                                |
| `192.0.0.0/24`    | IETF-Protokollzuweisungen                             |
| `192.168.0.0/16`  | privat                                                |
| `198.18.0.0/15`   | Benchmarking                                          |
| `224.0.0.0/4`     | Multicast                                             |
| `240.0.0.0/4`     | reserviert / Broadcast                                |
| `::/96`           | IPv4-kompatibel / unspezifiziert / Loopback           |
| `::ffff:0:0/96`   | IPv4-gemappt                                          |
| `::ffff:0:0:0/96` | IPv4-übersetzt                                        |
| `64:ff9b::/96`    | NAT64                                                 |
| `64:ff9b:1::/48`  | lokales NAT64                                         |
| `100::/64`        | Discard                                               |
| `2001::/32`       | Teredo                                                |
| `2002::/16`       | 6to4                                                  |
| `fc00::/7`        | Unique Local (privat)                                 |
| `fe80::/10`       | Link-Local                                            |
| `fec0::/10`       | Site-Local (veraltet)                                 |
| `ff00::/8`        | Multicast                                             |

IPv6-Adressen mit Zone (z. B. `fe80::1%eth0`) lehnt die API beim Speichern immer ab, auch ohne Schutz (Abschnitt 3). Beim Verbindungsaufbau prüft der Schutz die Adresse ohne Zone, `fe80::1%eth0` fällt also unter `fe80::/10`.

## 3. Prüfung beim Speichern und beim Verbindungsaufbau

Der Schutz arbeitet an zwei Stellen.

**Beim Speichern** (Monitor oder Benachrichtigungskanal) antwortet die API mit HTTP `422`, bevor etwas gespeichert wird:

- Immer, auch bei `SSRF_PROTECTION=off`: nicht kanonische IP-Schreibweisen (`0x7f.1`, `2130706433`, `0177.0.0.1`, `127.1`, Vollbreitenziffern, ein abschließender Punkt nach einer IP) und IPv6-Adressen mit Zone. Die Meldung lautet etwa `non-canonical IP address notation`.
- Nur bei aktivem Schutz: IP-Literale in gesperrten Bereichen, z. B. `http://169.254.169.254/`. Die Meldung nennt den Grund, z. B. `blocked by SSRF protection (link-local / cloud metadata)`.

Namen (z. B. `db.intern.example`) werden beim Speichern **nicht** aufgelöst. Sie werden erst beim Verbindungsaufbau geprüft.

**Beim Verbindungsaufbau** (Check läuft) setzt der Schutz den Check auf **DOWN** mit der Fehlerklasse `ssrf_blocked`. Das betrifft Namen, die auf eine gesperrte Adresse auflösen, Weiterleitungen auf gesperrte Ziele und Webhook-Zustellungen, die bei aktivem Schutz nicht zugestellt werden.

Beispiele:

| Ziel                                                                | Schutz an    | Ergebnis                                                                           |
| ------------------------------------------------------------------- | ------------ | ---------------------------------------------------------------------------------- |
| `http://169.254.169.254/latest/meta-data/`                          | ja           | 422 beim Speichern                                                                 |
| `http://127.1/`                                                     | ja oder nein | 422 beim Speichern (nicht kanonisch)                                               |
| `https://127.0.0.1.nip.io/`                                         | ja           | Speichern ok, Check **DOWN** `ssrf_blocked`, wenn der Name auf `127.0.0.1` auflöst |
| `https://intern.example/` (löst auf `10.1.2.3` auf)                 | ja           | Speichern ok, Check **DOWN** `ssrf_blocked`                                        |
| `https://intern.example/`                                           | nein         | Speichern ok, Check prüft normal                                                   |
| Öffentliche Seite, die per Redirect auf `http://192.168.1.1/` zeigt | ja           | Check **DOWN** `ssrf_blocked`                                                      |

Bestehende Monitore und Kanäle werden durch eine Änderung des Schutzes nicht umgeschrieben. Die 422-Prüfung greift erst beim nächsten Speichern; bis dahin fängt der Verbindungsaufbau die Ziele ab.

## 4. DNS-Checks bei aktivem Schutz

Ein DNS-Check löst den Namen über einen Resolver auf. Mit aktivem Schutz gilt:

- Jede A- und AAAA-Antwort (auch über CNAME-Ketten) wird gegen die gesperrten Bereiche geprüft, unabhängig davon, welcher Resolver gefragt wurde.
- Liegt eine Antwort in einem gesperrten Bereich, ist das Ergebnis **DOWN** mit der Fehlerklasse `ssrf_blocked`. Meldung und Details enthalten dann **keine** Adressen, nur den Grund (z. B. `answer in a blocked range: private`).
- Als Resolver wird bei fehlender Angabe der Systemresolver angezeigt, als `system`.
- Andere Abfragetypen (TXT, MX, NS usw.) bleiben unverändert. MX- und NS-Ziele sind Namen, keine Adressen.

Die Abfrage selbst, also der Name, den der Mandant abfragt, ist kein Ziel und wird nicht gesperrt.

Folge: Ein DNS-Check auf einen Namen, der bewusst auf eine private Adresse zeigt, schlägt bei aktivem Schutz fehl. Abhilfe im Einzelbetrieb ist `SSRF_PROTECTION=off`; später lassen sich solche Prüfungen über private Probes (AP-5.2) ausführen.

## 5. Benachrichtigungen

Webhooks und Shoutrrr-Dienste unterliegen demselben Schutz wie Checks (FA-ALR-12). Benachrichtigungen gehen immer von den Votern aus und folgen deren Einstellung.

- **Webhook:** Die URL wird beim Speichern geprüft (Abschnitt 3). Eine Zustellung an ein gesperrtes Ziel scheitert dauerhaft.
- **Shoutrrr:** Dienste, die der Schutz nicht umlenken kann, werden bei aktivem Schutz **beim Speichern des Kanals abgelehnt**. Die Meldung lautet sinngemäß `this service cannot be used while SSRF protection is on`. Betroffen ist derzeit nur `logger`. Alle anderen Dienste senden über den geschützten Dialer; das belegt ein Test je Dienst (`internal/alert/shoutrrr_ssrf_test.go`).
- **Host in der Shoutrrr-URL:** Bei Diensten, die den Host aus der URL kontaktieren (`bark`, `generic`, `gotify`, `googlechat`/`hangouts`, `homeassistant`, `matrix`, `mattermost`, `mqtt(s)`, `ntfy`, `opsgenie`, `pagerduty`, `rocketchat`, `signal`, `smtp`, `xmpp(s)`, `zulip`, bei `telegram` der Parameter `apiurl`), gelten beim Speichern dieselben Regeln wie für Webhook-URLs. Dienste mit festem Ziel im Internet (z. B. Discord, Slack, Pushover, Teams) nutzen den URL-Host nur für Token oder IDs; für sie greift allein die Prüfung beim Verbindungsaufbau.

Mit `SSRF_PROTECTION=off` lassen sich alle Shoutrrr-Dienste wie bisher anlegen.

## 6. Was der Schutz nicht abdeckt und was der Betrieb tun muss

Der Schutz kennt nur die gesperrten Bereiche. Er erkennt nicht, ob eine öffentliche Adresse einem Dienst gehört, der dem Node nach Quell-IP vertraut. Dafür muss der Betrieb sorgen:

- **Öffentliche Adressen mit Vertrauen nach Quell-IP:** Die öffentlichen IPs der eigenen Nodes und Dienste beim Hoster, die nur bestimmte IPs zulassen, sind für Checks erreichbar. Ein Mandant kann sie als Ziel angeben. Solche Dienste nie allein über die Quell-IP absichern.
- **Authentisierung statt IP-Vertrauen:** Dienste, die nur Nodes erreichen sollen, mit Token oder mTLS absichern. Die Kommunikation zwischen den Votern (Raft, gRPC) nutzt bereits mTLS.
- **Metriken schützen:** `/metrics` nur an private Schnittstellen binden (`METRICS_ADDR` auf eine interne Adresse) oder mit `METRICS_TOKEN` bzw. `METRICS_TOKEN_FILE` absichern.
- **Egress filtern:** Ausgehenden Verkehr der Nodes auf Netzwerkebene begrenzen, z. B. mit Host-Firewall oder Security Groups. Der Node braucht nur Ziel-Ports, die die Checks wirklich nutzen. Interne Netze und Metadaten-Adressen sind auch dort zu sperren.
- **Fehlermeldungen (offener Punkt):** Die Meldung eines fehlgeschlagenen Checks kann bei Lesefehlern die lokale Adresse des Nodes enthalten. Das ist noch nicht behoben. Mandanten sehen diese Meldungen; bei sensiblen Netzen die Node-Adressen nicht an öffentlichen Schnittstellen verwenden und den Egress-Filter (oben) einrichten.

## 7. Fehlerbilder

| Symptom                                                                                               | Ursache                                                                                                      | Maßnahme                                                                                                                                                                                                         |
| ----------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 422 `non-canonical IP address notation`                                                               | Das Ziel nutzt eine ungewöhnliche IP-Schreibweise (`0x7f.1`, `2130706433`, `127.1`, abschließender Punkt).   | Die Adresse in der Dezimalform mit vier Teilen angeben (`127.0.0.1`). Eine solche Adresse in einem Ziel ist fast immer ein Umgehungsversuch oder ein Tippfehler.                                                 |
| 422 `… blocked by SSRF protection (link-local / cloud metadata)` (bzw. anderer Grund aus Abschnitt 2) | Das IP-Literal liegt in einem gesperrten Bereich und der Schutz ist an.                                      | Ziel mit einem öffentlichen Namen oder einer öffentlichen Adresse angeben. Interne Ziele sind mit diesem Schutz nicht vorgesehen; im Einzelbetrieb ggf. `SSRF_PROTECTION=off` setzen.                            |
| 422 bei Monitoren mit IPv6-Zone (`%eth0`)                                                             | IPv6-Zonen sind immer gesperrt.                                                                              | Zone entfernen und die Adresse ohne `%…` angeben.                                                                                                                                                                |
| DOWN, Fehlerklasse `ssrf_blocked`, bei einem Namen                                                    | Der Name löst beim Verbindungsaufbau auf eine gesperrte Adresse auf (oder eine Weiterleitung führt dorthin). | Prüfen, wohin der Name auflöst (`dig +short <name>`). Ist das beabsichtigt und das Ziel öffentlich, liegt die Auflösung vermutlich an einem internen Resolver: DNS des Betreibers prüfen. Sonst das Ziel ändern. |
| DNS-Check DOWN, Fehlerklasse `ssrf_blocked`, Meldung ohne Adresse                                     | Eine A- oder AAAA-Antwort liegt in einem gesperrten Bereich.                                                 | Gewollt bei aktivem Schutz. Der Name muss öffentlich auflösen. Interne Namen sind mit diesem Schutz nicht prüfbar.                                                                                               |
| Kanal speichern: `this service cannot be used while SSRF protection is on`                            | Der Shoutrrr-Dienst kann nicht über den geschützten Dialer senden.                                           | Einen Dienst mit Webhook-Schnittstelle oder einen Dienst mit festem öffentlichen Ziel wählen. Alternativ `SSRF_PROTECTION=off`, nur im Einzelbetrieb.                                                            |
| Webhook wird nicht zugestellt, Fehler im Zustellversuch                                               | Die Ziel-URL ist ein gesperrtes Ziel.                                                                        | URL auf ein öffentlich erreichbares Ziel ändern.                                                                                                                                                                 |
| DOWN, Fehlerklasse `ssrf_blocked`, bei einem Ziel im Betreibernetz                                    | Der Schutz ist an und das Ziel liegt in einem gesperrten Bereich.                                            | Im Mehrmandantenbetrieb gewollt. Im Einzelbetrieb `SSRF_PROTECTION=off` setzen. Interne Ziele eines Mandanten folgen mit den privaten Probes (AP-5.2).                                                           |
| Ein Voter zeigt andere Ergebnisse als die anderen                                                     | `SSRF_PROTECTION` ist auf den Voter unterschiedlich gesetzt.                                                 | In der Cluster-Ansicht den Status je Node prüfen und die Umgebungsvariable angleichen.                                                                                                                           |

## 8. Testprotokoll

**2026-11-18, AP-4.3:** Ende zu Ende mit der lokalen Umgebung [`deploy/ssrf-dev/`](../../deploy/ssrf-dev/README.md) (Podman, ein Voter, kein Internetdienst). Mit `SSRF_PROTECTION=on` 24 von 24 Prüfungen bestanden: 422 für Metadaten-IP, private IP und `0x7f.1`; DOWN `ssrf_blocked` für Namen auf interne Adressen und auf `127.0.0.1`, für Weiterleitungen auf IP, Namen, Metadaten und Loopback, für TCP auf einen internen Namen und für einen DNS-Check auf einen internen Namen (ohne Adresse, Resolver `system`); DNS-Rebinding: 5 UP, 5 `ssrf_blocked`, kein Treffer am internen Ziel; Webhook auf interne IP 422, auf internen Namen gespeichert, Testversand scheitert. Das interne Ziel erhielt keinen einzigen Treffer. Gegenprobe mit `SSRF_PROTECTION=off`: 22 von 22 bestanden, interne Ziele erreichbar, `0x7f.1` weiterhin 422.

Befund aus dem Lauf, behoben: Die Meldung beim abgelehnten Webhook lautete `invalid web*** URL`, weil die Maskierung auch das feste Präfix traf (Pfadteil `hook`).
