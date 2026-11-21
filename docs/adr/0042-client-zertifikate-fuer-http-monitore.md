# ADR 0042: Client-Zertifikate (mTLS) für HTTP-Monitore

|                 |                                                                                              |
|-----------------|----------------------------------------------------------------------------------------------|
| **Status**      | Proposed                                                                                     |
| **Datum**       | 2026-11-21                                                                                   |
| **Entscheider** | Projektinhaber                                                                               |
| **Bezug**       | FA-CHK-10, FA-CHK-11, FA-CHK-14, FA-SEC-SEC, FA-API-06, ADR 0011, ADR 0015, ADR 0018, ADR 0034 |

## Kontext

Manche Ziele verlangen eine Authentifizierung per mTLS: Der Server fordert im TLS-Handshake ein Client-Zertifikat an
(`CertificateRequest`) und bricht ohne gültiges Zertifikat ab. Ohne diese Fähigkeit meldet der HTTP-Monitor solche
Ziele dauerhaft DOWN (Fehlerklasse `tls`).

Mandanten sollen bei einem HTTP-Monitor ein Zertifikat mit privatem Schlüssel hochladen können. Fordert das Ziel ein
Client-Zertifikat an, wird es mitgeschickt. Fordert das Ziel keines an, ändert sich nichts.

Randbedingungen:

- Der private Schlüssel ist ein Geheimnis im Sinne von FA-SEC-SEC. ADR 0015 regelt Geheimnisse in
  Monitor-Konfigurationen: Felder mit `writeOnly: true` im Typ-Schema werden in der API herausgelöst, mit dem DEK
  verschlüsselt (AAD `monitors/<id>/config<pointer>`), als `***` maskiert und zur Laufzeit über `ResolveConfig`
  eingesetzt. Raft-Log, `state.db`, Snapshots und Exporte sehen nur das Chiffrat.
- Das Zertifikat selbst (öffentlicher Schlüssel, Subject, Aussteller, Laufzeit) ist nicht geheim.
- Der HTTP-Checker baut pro Check einen eigenen Transport mit eigenem Handshake (`handshake` in
  `internal/checker/httpcheck`, FA-CHK-11). Die Kette des Servers prüft er selbst in `VerifyConnection` (FA-CHK-14).
- Checks laufen auf Votern und künftig auf Probes (ADR 0034). Probes erhalten die aufgelöste Konfiguration über mTLS
  und speichern sie nicht (ADR 0015, Punkt 5).
- Der HTTP-Parser lehnt unbekannte Felder ab (`DisallowUnknownFields`). Ein Voter mit älterer Version kann einen
  Monitor mit dem neuen Feld nicht ausführen.

## Optionen

### Option A: Zertifikat und Schlüssel direkt in der Monitor-Konfiguration

Neues optionales Objekt `clientCertificate` in der HTTP-Konfiguration:

```json
"clientCertificate": {
  "certificate": "-----BEGIN CERTIFICATE-----…",   // PEM, Leaf + optional Zwischenzertifikate, lesbar
  "privateKey":  "-----BEGIN PRIVATE KEY-----…"    // PEM, writeOnly → verschlüsselt + maskiert (ADR 0015)
}
```

- Vorteile: Nutzt den bestehenden Mechanismus aus ADR 0015 vollständig (Verschlüsselung, Maskierung, Behalten bei `***`,
  Neuverschlüsselung beim Klonen und bei DEK-Rotation, Export). Kein neuer FSM-Befehl, keine neue Entität, keine
  Migration. Klein.
- Nachteile: Nutzen mehrere Monitore dasselbe Zertifikat, muss es bei jedem hochgeladen und bei Erneuerung einzeln
  getauscht werden. Keine zentrale Übersicht über ablaufende Client-Zertifikate.

### Option B: Zertifikatsspeicher pro Mandant, Monitore verweisen per ID

Neue Entität `client_certificates` (Mandant, Name, Zertifikat, verschlüsselter Schlüssel), eigene REST-Ressource und
UI-Seite; der HTTP-Monitor erhält `clientCertificateId`.

- Vorteile: Ein Zertifikat für viele Monitore; Erneuerung an einer Stelle; Übersicht mit Ablaufdaten.
- Nachteile: Neue Entität mit FSM-Befehlen, Store-Schema, Migration, OpenAPI-Ressource, Rechteprüfung, Löschschutz
  (Zertifikat noch in Verwendung), Export/Restore, Mandantentrennung beim Auflösen der Referenz. Deutlich größer; die
  Referenz muss in Scheduler, Test-Endpunkt und Probe-Auslieferung zusätzlich aufgelöst werden.

### Option C: Nur PKCS#12-Upload (`.p12`/`.pfx`) mit Passwort

- Vorteile: Format, das viele Anwender von Behörden- oder Unternehmens-PKIs bekommen.
- Nachteile: Braucht eine zusätzliche Abhängigkeit für PKCS#12 (die Standardbibliothek kann es nicht), ältere Dateien
  nutzen schwache Algorithmen (RC2/3DES). Das Passwort wäre ein weiteres Geheimnis, obwohl der Schlüssel ohnehin
  verschlüsselt abgelegt wird.

## Entscheidung

**Option A.** Option B kann später als Erweiterung kommen (`clientCertificateId` neben dem eingebetteten Objekt), ohne
Option A zu brechen. Festlegungen:

1. **Schema:** `clientCertificate` ist optional und hat zwei Pflichtfelder: `certificate` (PEM, `maxLength` 16384) und
   `privateKey` (PEM, `writeOnly: true`, `maxLength` 16384). Das Typ-Schema bekommt Titel und Beschreibung für das
   Formular.
2. **Formate:** Zertifikat als PEM-Block `CERTIFICATE`, erster Block ist das Leaf, weitere Blöcke sind
   Zwischenzertifikate und werden mitgeschickt. Schlüssel als PEM mit PKCS#8, PKCS#1 (RSA) oder SEC 1 (EC); unterstützt
   sind RSA ≥ 2048 Bit, ECDSA P-256/P-384 und Ed25519. Verschlüsselte PEM-Schlüssel und PKCS#12 werden in dieser Stufe
   **nicht** angenommen. Die Fehlermeldung und die UI nennen den `openssl`-Befehl zur Umwandlung. Die UI liest die
   hochgeladenen Dateien im Browser als Text und füllt die beiden Felder. Der Server bekommt kein Dateiformat zu sehen.
   (siehe offene Frage F1)
3. **Validierung beim Speichern** (422 mit Feldpfad): Zertifikat und Schlüssel lassen sich parsen; der Schlüssel passt
   zum Leaf (`tls.X509KeyPair`); Schlüsseltyp und -länge wie oben; das Leaf erlaubt `clientAuth` in `ExtKeyUsage`, falls
   die Erweiterung gesetzt ist. Ein bereits abgelaufenes Leaf wird **angenommen**, damit Monitore bei Erneuerung nicht
   unspeicherbar werden. Der Check meldet es dann (Punkt 6). Die Validierung läuft auf der zusammengesetzten
   Konfiguration, also mit dem gespeicherten Schlüssel, wenn `privateKey` als `***` oder gar nicht gesendet wird.
4. **Senden nur auf Anforderung:** Der Checker setzt `tls.Config.GetClientCertificate` und gibt das Zertifikat nur
   zurück, wenn der Server eines anfordert. Er ignoriert dabei die vom Server genannten akzeptierten CAs, damit ein
   Zertifikat aus einer anderen Kette nicht stillschweigend weggelassen wird. `IgnoreTLSErrors` wirkt weiterhin nur auf
   die Prüfung der Serverkette, nicht auf das Client-Zertifikat.
5. **Redirects:** Das Zertifikat wird nur Hosts gezeigt, die zum Host der Monitor-URL gehören (gleicher Hostname, auch
   bei anderem Port oder Schema `http`→`https`). Folgt der Monitor einem Redirect auf einen anderen Host, sendet er dort
   kein Client-Zertifikat. Das entspricht dem Verhalten von Go bei `Authorization`-Headern über Hostgrenzen. Fordert der
   fremde Host trotzdem eines an, schlägt der Handshake mit der Fehlerklasse `tls` fehl und die Meldung nennt den Grund.
   (siehe offene Frage F2)
6. **Ablauf des Client-Zertifikats:** Der Checker prüft das Leaf bei jedem Lauf. Liegt das Ablaufdatum innerhalb von
   `tlsExpiryWarningDays`, ist das Ergebnis **DEGRADED** mit eigener Meldung („client certificate expires in N days“).
   Ist es abgelaufen oder noch nicht gültig, ist das Ergebnis **DOWN** mit Fehlerklasse `tls`, ohne Verbindungsaufbau.
   Die Monitor-Detailseite zeigt Subject, Aussteller und Ablaufdatum des Client-Zertifikats. Diese Werte liest die UI aus
   dem lesbaren Feld `certificate`. (siehe offene Frage F3)
7. **Fehlermeldungen:** Lehnt der Server das Zertifikat ab (TLS-Alerts `bad_certificate`, `certificate_required`,
   `unknown_ca`, `certificate_revoked`, `certificate_expired`, `access_denied`), lautet die Meldung „client certificate
   rejected by server: <alert>“ mit Fehlerklasse `tls`. Fordert der Server ein Zertifikat an, ohne dass eines
   konfiguriert ist, wird das in der Meldung genannt („server requested a client certificate“). Das hilft auch Monitoren
   ohne mTLS bei der Fehlersuche.
8. **Umgang mit dem Klartext:** Der Schlüssel wird pro Check aus der aufgelösten Konfiguration geparst und liegt nur im
   Speicher dieses Checks. Er wird nicht über Checks hinweg zwischengespeichert (FA-CHK-11, keine geteilten Transports).
   Er erscheint nie in Logs, Check-Meldungen, Fehlertexten, Audit-Einträgen oder API-Antworten. Ein Test prüft das für
   Validierungs- und Handshake-Fehler. Der Plugin-Test aus ADR 0015 wird um den Feldnamen `privateKey` erweitert.
9. **Probes:** Wie bei allen Geheimnissen erhalten Probes, auch öffentliche, die aufgelöste Konfiguration über mTLS
   (ADR 0015, Punkt 5) und halten sie nur im Speicher. Für den lokalen Puffer der Probe (ADR 0034) gilt: Er puffert nur
   Ergebnisse, keine Konfigurationen. Das wird im Probe-Code per Test abgesichert.
10. **Rolling Update:** Ältere Binaries lehnen das unbekannte Feld ab. Die API nimmt `clientCertificate` deshalb erst an,
    wenn die aktive Befehlsstufe (ADR 0018) die nächste freie Stufe erreicht hat. Damit ist garantiert, dass alle Voter
    das Feld verstehen. Vorher antwortet sie mit 422 und dem Hinweis „cluster upgrade in progress“. Es gibt keinen neuen
    FSM-Befehl; die Stufe dient nur als Gate. Probes älterer Version melden für solche Monitore einen Konfigurationsfehler
    (Fehlerklasse `internal`), bis sie aktualisiert sind.
11. **SSRF:** Unverändert. Das Ziel bleibt der Host der URL (ADR 0030); das Zertifikat ändert nichts an den
    Verbindungszielen.
12. **Pflichtenheft:** Neue Anforderung FA-CHK-16 „HTTP-Monitore können ein Client-Zertifikat (mTLS) mitsenden;
    Schlüssel verschlüsselt und maskiert gemäß FA-SEC-SEC“. FA-SEC-SEC nennt private Schlüssel ausdrücklich.

### Offene Fragen an den Projektinhaber

- **F1 – PKCS#12:** Reicht PEM in der ersten Stufe (Vorschlag) oder soll `.p12`/`.pfx` direkt unterstützt werden? Dafür
  bräuchte es eine Abhängigkeit (z. B. `software.sslmate.com/src/go-pkcs12`, BSD-3-Clause) und ein Passwortfeld.
- **F2 – Redirects auf fremde Hosts:** Kein Zertifikat senden (Vorschlag) oder auch dort senden?
- **F3 – Ablaufwarnung:** Dieselbe Schwelle `tlsExpiryWarningDays` wie für das Serverzertifikat (Vorschlag) oder ein
  eigenes Feld?

## Konsequenzen

- Positiv: mTLS-geschützte Ziele lassen sich überwachen. Der Schlüssel ist at rest verschlüsselt und für keine Rolle
  lesbar. Kein neuer Speicher- oder Befehlspfad, weil ADR 0015 wiederverwendet wird. Bessere Fehlermeldung bei
  `certificate_required` auch ohne mTLS-Konfiguration.
- Negativ / Risiken: Der private Schlüssel geht an alle ausführenden Nodes, auch an öffentliche Probes. Das ist dasselbe
  Vertrauensmodell wie bei Passwörtern heute, ein Schlüssel ist aber oft langlebiger. Bei vielen Monitoren mit demselben
  Zertifikat ist die Erneuerung mühsam (Option B als spätere Erweiterung). Ein Parse pro Check kostet etwas CPU
  (Ed25519/ECDSA vernachlässigbar, RSA wenige 10 µs). Altbestand im Raft-Log ist kein Thema, weil das Feld neu ist.
- Folgearbeiten (nach Annahme, Arbeitspaket AP-6.5 oder vorgezogen nach Wunsch):
    - **Hauptagent (Sicherheitscode):** Paket-Hilfsfunktion zum Parsen und Prüfen von Zertifikat und Schlüssel (Punkte 2,
      3, 6), `GetClientCertificate` und Redirect-Bindung im Handshake (4, 5), Abbildung der TLS-Alerts ohne Klartext (7,
      8), Gate über die Befehlsstufe in der API (10), Tests zum Nicht-Durchsickern des Schlüssels.
    - **Haiku-Agenten:** Schema-Erweiterung und `Config`-Typ, Verdrahtung der Hilfsfunktionen in `validate`/`Check`,
      Tabellentests mit Testserver, der Client-Zertifikate verlangt (`tls.RequireAndVerifyClientCert`), Fixtures unter
      `testdata`; Angular: Upload-Felder für Zertifikat und Schlüssel im Monitor-Editor (Datei als Text einlesen, `***`
      wie bei anderen Geheimnissen), Anzeige von Subject/Aussteller/Ablauf; OpenAPI-Beschreibung; Pflichtenheft,
      Roadmap, Benutzer-Doku mit `openssl`-Umwandlung.

## Approval

| Datum      | Status   | Durch  | Anmerkung |
|------------|----------|--------|-----------|
| 2026-11-21 | Proposed | Claude |           |
