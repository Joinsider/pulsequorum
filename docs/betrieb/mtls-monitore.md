# Client-Zertifikate (mTLS) für HTTP-Monitore

Fordert das Ziel eines HTTP-Monitors im TLS-Handshake ein Client-Zertifikat an, schickt der Monitor das konfigurierte Zertifikat mit (Pflichtenheft FA-CHK-16, ADR 0042). Fordert das Ziel keines an, ändert sich nichts. Das Zertifikat geht nur an den Host der Monitor-URL.

## Unterstützte Formate

- **Zertifikat:** PEM mit Blöcken `CERTIFICATE`. Der erste Block ist das Blatt, danach folgen optional bis zu sieben Zwischenzertifikate.
- **Privater Schlüssel:** genau ein unverschlüsselter PEM-Schlüssel, als PKCS#8 (`PRIVATE KEY`), PKCS#1 (`RSA PRIVATE KEY`) oder SEC 1 (`EC PRIVATE KEY`). Ein `EC PARAMETERS`-Block davor ist erlaubt.
- **Schlüsseltypen:** RSA mit mindestens 2048 Bit, ECDSA P-256 und P-384, Ed25519.
- **Nicht unterstützt:** PKCS#12 (`.p12`, `.pfx`) und verschlüsselte Schlüssel. Hat das Zertifikat eine Erweiterung „Extended Key Usage“, muss sie Client-Authentifizierung erlauben.

## Umwandeln mit openssl

PKCS#12-Datei in Zertifikat und Schlüssel aufteilen (Passwort wird abgefragt):

```bash
openssl pkcs12 -in client.p12 -clcerts -nokeys -out cert.pem
openssl pkcs12 -in client.p12 -nocerts -nodes -out key.pem
```

Verschlüsselten Schlüssel entschlüsseln (Passphrase wird abgefragt):

```bash
openssl pkey -in key-enc.pem -out key.pem
```

Die Datei `key.pem` liegt danach unverschlüsselt auf dem Datenträger. Nach dem Einspielen löschen oder sicher aufbewahren.

## Verhalten

- **Redirects:** Das Zertifikat geht nur an denselben Hostnamen wie die Monitor-URL (anderer Port oder Schema `http`/`https` sind erlaubt). Leitet das Ziel auf einen anderen Hostnamen um und verlangt dieser ein Zertifikat, schlägt der Check mit der Meldung „it is sent only to …“ fehl (Fehlerklasse `tls`).
- **Ablauf:** Liegt das Ablaufdatum innerhalb von `tlsExpiryWarningDays` (Standard 14), ist das Ergebnis DEGRADED („client certificate expires in N days“). Ist das Zertifikat abgelaufen oder noch nicht gültig, ist das Ergebnis DOWN mit Fehlerklasse `tls`, ohne dass eine Verbindung aufgebaut wird.
- **Ablehnung:** Lehnt der Server das Zertifikat ab, lautet die Meldung „client certificate rejected by server: …“. Verlangt der Server ein Zertifikat, ist aber keines konfiguriert, nennt die Meldung „none is configured“.
- **Details:** Jedes Ergebnis enthält `clientCertSubject`, `clientCertIssuer`, `clientCertNotAfter` und `clientCertSent` (ob das Zertifikat tatsächlich gesendet wurde).
- `ignoreTlsErrors` wirkt nicht auf das Client-Zertifikat.

## Rollierendes Update

Die API nimmt `clientCertificate` nur an, solange jeder Voter die Capability `http-client-cert` meldet. Ist ein Voter älter, antwortet die API mit 422 und nennt die Voter, die noch aktualisiert werden müssen („cluster upgrade in progress“). Reihenfolge: zuerst alle Voter aktualisieren, dann Monitore mit Client-Zertifikat anlegen. Probes ohne die Capability blockieren nicht. Ältere Probes melden solche Monitore als Konfigurationsfehler (Fehlerklasse `internal`), bis sie aktualisiert sind.

## Sicherheit

- Der private Schlüssel wird verschlüsselt gespeichert (ADR 0015, AES-256-GCM).
- In der API erscheint er als `***`. Wird ein Monitor mit `***` und demselben Zertifikat gespeichert, bleibt der gespeicherte Schlüssel erhalten.
- Probes erhalten die aufgelöste Konfiguration über mTLS und halten sie nur im Speicher.
- Der Schlüssel liegt nur während eines Checks im Speicher. Er erscheint nie in Logs, Meldungen oder API-Antworten.
- Das Zertifikat ohne Schlüssel ist nicht geheim und wird im Klartext angezeigt.
- Empfehlung: für jeden Monitor ein eigenes Client-Zertifikat mit kurzer Laufzeit verwenden.
