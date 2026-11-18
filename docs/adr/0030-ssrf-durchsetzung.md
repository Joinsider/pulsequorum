# ADR 0030: SSRF-Durchsetzung: Bereichslisten, Prüfung beim Speichern, DNS-Antworten

|                 |                                                                                                                |
| --------------- | -------------------------------------------------------------------------------------------------------------- |
| **Status**      | Accepted                                                                                                       |
| **Datum**       | 2026-11-18                                                                                                     |
| **Entscheider** | Projektinhaber                                                                                                       |
| **Bezug**       | FA-SEC-SSRF, FA-ALR-12, FA-CHK-04, PF-29, PF-29a, AS-07; ADR 0014 (Benachrichtigungen), ADR 0022 (Selbstcheck) |

## Kontext

Seit P1 laufen alle ausgehenden Verbindungen der Checker, der Benachrichtigungen und des Selbstchecks über `checker.Dialer` (FA-CHK-04). Die Richtlinie `BlockPrivate` prüft in `net.Dialer.Control` die IP, mit der sich der Socket tatsächlich verbindet. Damit sind DNS-Rebinding und Redirects abgedeckt (PF-29a). `SSRF_PROTECTION=auto|on|off` schaltet sie, `auto` ist im Zitadel-Profil an. ICMP prüft die aufgelöste Adresse vor dem Senden, DNS-Checks schicken einen vom Mandanten gesetzten Resolver durch die Richtlinie, Webhooks und Shoutrrr-Dienste mit `HTTPClientSetter` oder `DialContextSetter` nutzen den Dialer, andere Dienste werden bei aktivem Schutz abgelehnt (ADR 0014).

Bei der Sichtung für AP-4.3 fielen diese Lücken gegenüber FA-SEC-SSRF auf:

1. **Gezonte IPv6-Adressen** (`[::1%lo0]`, `[fe80::1%eth0]`) umgingen jeden Bereich, weil `netip.Prefix.Contains` sie nie trifft. Als Bugfix ohne ADR behoben (`fix(checker): block zoned IPv6 addresses…`), zusammen mit dem Bereich `::ffff:0:0:0/96` (IPv4-translated).
2. **Bereiche sind fest.** Betreiber können weder zusätzlich sperren (z.B. die öffentlichen Adressen der eigenen Nodes) noch einzelne interne Ziele freigeben, ohne den Schutz ganz abzuschalten. Das Pflichtenheft nennt die Bereiche „konfigurierbar“; zu klären ist, ob das Listen oder nur das Ein- und Ausschalten meint.
3. **Nicht-kanonische IP-Schreibweisen** (`0x7f.1`, `2130706433`, `0177.0.0.1`, `127.1`) werden beim Speichern nicht abgelehnt. Der Go-Resolver behandelt sie als Namen, der cgo-Resolver (`getaddrinfo`, `inet_aton`) als IP. Die Dial-Zeit-Prüfung fängt beides ab, FA-SEC-SSRF verlangt aber die Ablehnung, und der Mandant sieht den Fehler erst am DOWN-Check.
4. **IP-Literale in gesperrten Bereichen** (`http://169.254.169.254/`) werden gespeichert und erst beim Check DOWN. AS-07 lässt „abgelehnt bzw. DOWN“ zu; eine Ablehnung beim Speichern ist klarer.
5. **DNS-Checks als Leck.** Ein DNS-Check über den System-Resolver kann interne Namen auflösen (Docker-Dienste, Split-Horizon, `metadata.google.internal`) und zeigt die Antworten in Meldung und Details. Ein Mandant kann so das interne Netz des Betreibers kartieren, ohne je eine Verbindung aufzubauen.
6. **Shoutrrr:** ADR 0014 verlangt je Dienst einen Test, dass er den Dialer wirklich nutzt. Es gibt nur die Prüfung der Schnittstellen, keinen Test, der eine echte Verbindung auf eine gesperrte Adresse versucht.
7. **Private Probes** sind laut FA-SEC-SSRF ausgenommen, existieren aber erst mit AP-5.2.

Die Richtungsfragen wurden am 2026-11-18 im Chat entschieden: 1A, 2B, 3B (siehe Optionen).

## Optionen

### Frage 1: Konfigurierbare Bereiche

**1A Feste Liste.** Nur die Liste aus FA-SEC-SSRF. ← gewählt

- Vorteile: nichts falsch zu konfigurieren.
- Nachteile: eigene öffentliche Node-Adressen bleiben erreichbar; interne Ziele auf einem öffentlichen Node gehen nur mit `SSRF_PROTECTION=off` für alle Mandanten.

**1B Nur zusätzliche Sperren (`SSRF_DENY_CIDRS`).**

- Vorteile: kann den Schutz nur verschärfen.
- Nachteile: wie 1A für interne Ziele.

**1C Sperren und Freigaben (`SSRF_DENY_CIDRS`, `SSRF_ALLOW_CIDRS`).**

- Vorteile: Betreiber sperren die eigenen Adressen und geben gezielt einzelne interne Netze frei, statt den Schutz abzuschalten.
- Nachteile: Eine Freigabe gilt für alle Mandanten des Nodes. Fehlkonfiguration öffnet interne Netze; deshalb harte Grenzen (siehe Entscheidung).

### Frage 2: Prüfung beim Speichern

**2A Nur beim Dial.** Unverändert, alles wird erst beim Check `ssrf_blocked`.

- Vorteile: kein neuer Code.
- Nachteile: Die Pflichtenheft-Forderung „nicht-kanonische Schreibweisen ablehnen“ ist nur indirekt erfüllt; schlechte Rückmeldung.

**2B Zusätzlich beim Speichern.** ← gewählt

- Vorteile: klare 422-Meldung am Feld; keine Abhängigkeit davon, welcher Resolver eine Schreibweise wie deutet.
- Nachteile: Speichern und Dial müssen dieselbe Richtlinie nutzen; Namen bleiben eine Dial-Zeit-Frage (sie können sich jederzeit ändern).

### Frage 3: DNS-Antworten bei aktivem Schutz

**3A Unverändert.** Antworten werden angezeigt, nie kontaktiert.

- Vorteile: DNS-Checks auf interne Namen funktionieren weiter.
- Nachteile: Leck interner Namen und Adressen an Mandanten.

**3B Gesperrte Antworten machen den Check `ssrf_blocked`.** ← gewählt

- Vorteile: Kein Mandant erfährt interne Adressen; dasselbe Ergebnis wie bei einem HTTP-Check auf den Namen.
- Nachteile: DNS-Checks auf Namen, die bewusst privat auflösen, gehen bei aktivem Schutz nicht (dafür gibt es `SSRF_PROTECTION=off` im Einzelbetrieb oder später private Probes).

## Entscheidung

### 1. Feste Bereichsliste (1A)

- Die gesperrten Bereiche sind die feste Liste aus FA-SEC-SSRF in `checker.BlockPrivate`, ergänzt um `::ffff:0:0:0/96`. Es gibt keine Variablen für zusätzliche Sperren oder Freigaben.
- **Auslegung des Pflichtenhefts:** „Gesperrte Bereiche (konfigurierbar …)“ in FA-SEC-SSRF meint das Ein- und Ausschalten des Schutzes über `SSRF_PROTECTION` (`auto|on|off`), nicht einzelne Bereiche. Das Pflichtenheft wird in diesem Sinn präzisiert.
- **Begründung:** Wer selbst hostet, betreibt einen Mandanten im lokalen oder generischen Modus. Dort ist der Schutz mit `auto` aus, interne Ziele gehen also ohnehin. Im Mehrmandantenbetrieb würde eine Freigabe für alle Mandanten eines Nodes gelten und genau das öffnen, wovor der Schutz bewahrt. Interne Ziele eines Mandanten gehören auf seine privaten Probes (AP-5.2). Ohne Listen gibt es nichts falsch zu konfigurieren.
- **Was 1A nicht abdeckt:** Öffentliche Adressen, denen die eigene Infrastruktur nach Quell-IP vertraut (die öffentlichen IPs der eigenen Nodes, Dienste beim Hoster mit IP-Allowlist), bleiben für Checks erreichbar. Gegenmaßnahmen liegen beim Betrieb und kommen in die Betriebsdoku: Dienste nie allein über die Quell-IP absichern (Token oder mTLS, wie bei Raft/gRPC), Metriken nur an private Schnittstellen binden oder mit `METRICS_TOKEN` schützen, und ausgehenden Verkehr der Nodes auf Netzwerkebene filtern (Host-Firewall, Security Groups).
- **Später erweiterbar:** Eine zusätzliche Sperrliste (`SSRF_DENY_CIDRS`) lässt sich als optionale Variable ohne Bruch nachrüsten, falls der Betrieb sie braucht. Eine Freigabeliste nur mit neuem ADR.
- `GetStatus` meldet, ob der Schutz auf dem Node an ist; weichen die Voter ab, zeigt die Cluster-Ansicht das an. Ein eigener Systemalert dafür ist nicht Teil dieses ADR.
- Die Einstellung bleibt lokal je Node (Umgebungsvariable), nicht im Raft-State: Sie ist Betreiberkonfiguration, und ein Mandant darf sie nie ändern können.

### 2. Prüfung beim Speichern (2B)

Eine Funktion `checker.ValidateHost(host string, policy AddressPolicy) error` (Hauptagent) prüft Ziele beim Speichern:

- **Immer** (auch bei `SSRF_PROTECTION=off`): Ein Host, dessen letztes Label nur aus Ziffern besteht oder mit `0x` beginnt, muss eine kanonische IPv4-Adresse nach `netip.ParseAddr` sein (vier Dezimalteile, keine führenden Nullen). Sonst 422 „non-canonical IP address notation“. Das folgt der WHATWG-Regel „ends in a number“, die alle Browser nutzen. IPv6-Literale müssen sich mit `netip.ParseAddr` parsen lassen; **Zonen** (`%eth0`) werden immer abgelehnt.
- **Bei aktivem Schutz:** Ein IP-Literal, das die Richtlinie sperrt, wird mit 422 und dem Grund (z.B. „link-local / cloud metadata“) abgelehnt.
- Namen werden beim Speichern **nicht** aufgelöst: Das Ergebnis wäre beim nächsten Check schon veraltet, und eine Auflösung beim Speichern würde selbst interne Namen bestätigen (Frage 3). Sie bleiben der Dial-Zeit-Prüfung überlassen.
- Betroffen sind alle Felder, die ein Ziel enthalten: HTTP-URL, TCP- und ICMP-Host, DNS-Resolver, Webhook-URL und der Host der Shoutrrr-URL (soweit der Dienst einen Host aus der URL kontaktiert). Der DNS-Abfragename ist kein Ziel und bleibt frei.
- Bestehende Monitore und Kanäle werden nicht nachträglich umgeschrieben. Beim nächsten Speichern greift die Prüfung; bis dahin fängt sie die Dial-Zeit-Prüfung ab.
- Die API nutzt die Richtlinie des Nodes, der den Request annimmt. Ist der Schutz nicht auf allen Votern gleich eingestellt, kann ein Ziel auf einem Node angenommen und auf einem anderen beim Check gesperrt werden; das zeigt die Anzeige aus Abschnitt 1.

### 3. DNS-Antworten (3B)

Ist der Schutz an, prüft der DNS-Checker jede A- und AAAA-Antwort (auch in CNAME-Ketten aufgelöste) mit der Richtlinie, unabhängig davon, welcher Resolver gefragt wurde. Ist eine Antwort gesperrt, ist das Ergebnis DOWN mit Fehlerklasse `ssrf_blocked`; Meldung und Details enthalten dann **keine** Antworten, nur den Grund ohne Adresse („answer in a blocked range: private“). Andere Typen (TXT, MX, NS, …) bleiben unverändert; MX- und NS-Ziele sind Namen, keine Adressen.

### 4. Shoutrrr-Dienste

Ein Test je Shoutrrr-Dienst, den `Validate` bei aktivem Schutz zulässt: URL auf einen lokalen Listener (127.0.0.1 bzw. einen Namen, der dorthin auflöst), Senden mit `BlockPrivate`, erwartet wird `ErrSSRFBlocked` und **keine** angenommene Verbindung am Listener. Dienste mit festem Ziel (z.B. Telegram) testen über einen umgebogenen Resolver bzw. werden mit Begründung ausgenommen. Ein Dienst, der den Test nicht besteht, kommt auf eine Sperrliste in `internal/alert` und wird bei aktivem Schutz wie ein Dienst ohne Setter abgelehnt.

### 5. Private Probes

Die Ausnahme für private Probes (FA-SEC-SSRF) wird erst mit AP-5.2 umgesetzt. Vorgabe für das dortige ADR: Ein privater Probe gehört genau einem Mandanten, prüft nur dessen Monitore und nutzt seine eigene, lokale Richtlinie (Standard aus). Voter und öffentliche Probes nutzen nie eine gelockerte Richtlinie für Mandantenziele. Benachrichtigungen gehen immer von Votern aus und unterliegen immer deren Richtlinie (FA-ALR-12).

### 6. Tests

- Tabellentests für Richtlinie, Listen und Grenzen, `ValidateHost` mit allen Schreibweisen aus PF-29a.
- Fuzz-Tests: `ValidateHost` gegen einen Referenzparser (jeder als IPv4 deutbare Host ist kanonisch oder abgelehnt), Richtlinie über zufällige IPv4/IPv6 mit Zone und Einbettung (Urteil gleich der normalisierten Form), Parser der CIDR-Listen.
- Ende zu Ende (AS-07): HTTP-Monitor auf `http://169.254.169.254/` (422), auf einen Namen, der nach 127.0.0.1 auflöst (DOWN, `ssrf_blocked`), Redirect von einem öffentlichen Testserver auf ein internes Ziel (DOWN), Webhook auf 127.0.0.1 (422) bzw. per Name (Zustellung scheitert dauerhaft), DNS-Check auf einen internen Namen (DOWN ohne Adresse).

## Konsequenzen

- Positiv: FA-SEC-SSRF und FA-ALR-12 vollständig umgesetzt; nichts an der Bereichsliste falsch zu konfigurieren; keine internen Adressen in DNS-Ergebnissen; Fehler am Feld statt erst am Check.
- Negativ / Risiken: Öffentliche Ziele, denen die eigene Infrastruktur nach Quell-IP vertraut, sperrt die Anwendung nicht; das muss der Betrieb absichern (siehe Abschnitt 1). Im Mehrmandantenbetrieb gibt es bis AP-5.2 keinen Weg, einzelne interne Ziele zu prüfen. Ein unterschiedlich eingestellter Schutz auf den Votern führt zu uneinheitlichen Ergebnissen (die Anzeige macht es sichtbar, verhindert es nicht). Die Ablehnung nicht-kanonischer Schreibweisen gilt auch ohne Schutz und kann einen exotischen, bisher gespeicherten Monitor beim nächsten Speichern ablehnen.
- Folgearbeiten: Pflichtenheft FA-SEC-SSRF präzisieren („konfigurierbar“ = `SSRF_PROTECTION`); Doku `docs/betrieb/ssrf.md` (gesperrte Bereiche, Fehlerbilder, Absicherung von Diensten mit IP-Allowlist, Egress-Filter); Eintrag in `compose.md`; UI-Hinweis bei `ssrf_blocked`; Ausnahme für private Probes in AP-5.2; ggf. Systemalert bei abweichender Einstellung; ggf. `SSRF_DENY_CIDRS` als Erweiterung.

## Approval

| Datum      | Status   | Durch    | Anmerkung                                                                              |
| ---------- | -------- | -------- | -------------------------------------------------------------------------------------- |
| 2026-11-18 | Proposed | Claude   | Richtungsfragen im Chat entschieden: zuerst 1C, 2B, 3B; Frage 1 danach auf 1A geändert |
| 2026-11-18 | Accepted | Projektinhaber | im Chat angenommen („Accept ADR 30“), eingetragen durch Claude                         |
