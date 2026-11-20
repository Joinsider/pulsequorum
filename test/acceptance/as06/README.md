# Acceptance-Szenario AS-06 (automatisierter Treiber)

Automatisiert das Szenario AS-06 aus `docs/pflichtenheft.md`, Abschnitt 13.2:

> Zwei Mandanten über Zitadel. Mandant A versucht, einen Monitor von Mandant B zu
> lesen oder zu ändern (ID-Manipulation) → 404, kein Datenleck in API und WebSocket.

Der Treiber prüft, dass Mandant B die Objekte von Mandant A über die REST-API weder
lesen noch ändern kann und dass Listen und Live-Stream von B nichts davon zeigen.

Der Knoten wird separat gestartet (Zitadel-Entwicklungsumgebung plus Knoten mit OIDC).
Der Treiber `as06` läuft danach gegen die Basis-URL des Knotens.

## Voraussetzungen

1. **Zitadel lokal** (nur Entwicklung): `deploy/oidc-dev/zitadel`. Das Skript `setup.sh`
   braucht `podman-compose`, `curl`, `jq` und `openssl`. Es legt die Organisationen
   `acme` und `globex` an, dazu die Maschinenbenutzer `acme-admin-bot` und
   `globex-admin-bot` mit der Rolle `tenant-admin`. Die Umgebung für den Knoten landet
   in `deploy/oidc-dev/zitadel/.generated/pulsequorum.env` (Modus `oidc`, Profil
   `zitadel`, Tenancy `multi`).

2. **Knoten mit dieser Umgebung starten.** Die Variablen müssen in den Prozess:

   ```bash
   set -a; source deploy/oidc-dev/zitadel/.generated/pulsequorum.env; set +a
   make run   # oder das gebaute Binary mit denselben Variablen
   ```

3. **Die Mandanten `acme` und `globex` müssen existieren.** Die Umgebungsdatei setzt
   `OIDC_AUTO_PROVISION` nicht. Zwei Wege (siehe `docs/betrieb/oidc.md`, Abschnitte 5
   und 9):
   - **Automatische Anlage:** `OIDC_AUTO_PROVISION=true` und `OIDC_AUTO_PROVISION_PLAN`
     vor dem Start setzen. Der erste Login jeder Organisation legt dann den Mandanten an.
   - **Durch einen Plattform-Administrator:** Mit `platform-admin-bot` die Mandanten
     `acme` und `globex` über `POST /api/v1/admin/tenants` anlegen.

   Ohne einen der beiden Wege lehnt der Knoten den Login der Bots mit 403 ab. Der
   Treiber bricht dann beim Schritt „Mandanten“ ab.

4. **Zugriffstoken holen.** Die Token gelten nur kurz. Sie direkt vor dem Lauf in ein
   Verzeichnis mit Modus `700` schreiben, und zwar aus dem Repository-Wurzelverzeichnis:

   ```bash
   dir="$(mktemp -d)"
   chmod 700 "$dir"
   deploy/oidc-dev/zitadel/token.sh acme-admin-bot > "$dir/a.tok"
   deploy/oidc-dev/zitadel/token.sh globex-admin-bot > "$dir/b.tok"
   chmod 600 "$dir/a.tok" "$dir/b.tok"
   ```

   `token.sh` gibt nur das Token auf stdout aus. Fehler gehen nach stderr, ohne
   Geheimnisse. Die Dateien enthalten je ein Bearer-Token eines `tenant-admin` in
   einem der beiden Mandanten.

## Aufruf

```bash
go run ./test/acceptance/as06 \
  -base http://127.0.0.1:8080 \
  -token-a-file "$dir/a.tok" \
  -token-b-file "$dir/b.tok"
```

| Flag | Standard | Bedeutung |
|------|----------|-----------|
| `-base` | `http://127.0.0.1:8080` | Basis-URL des Knotens (der Präfix `/api/v1` wird ergänzt) |
| `-token-a-file` | leer, Pflicht | Datei mit dem Token des `tenant-admin` von Mandant A |
| `-token-b-file` | leer, Pflicht | Datei mit dem Token des `tenant-admin` von Mandant B |
| `-event-wait` | `90` | Sekunden, die auf ein Ereignis des Monitors von A im Stream von A gewartet wird (10 bis 600) |

Die Token werden mit `os.ReadFile` gelesen und getrimmt. Sie werden nie ausgegeben,
auch nicht in Fehlermeldungen. Alles, was der Treiber ausgibt, wird vorher von den
Token-Werten bereinigt.

Ergebnis ist eine Markdown-Tabelle auf stdout (Schritt, erwartet, gemessen, Ergebnis)
mit dem Gesamturteil `AS-06 overall: PASS` oder `FAIL`. Der Exit-Status ist 1, wenn
ein Schritt fehlschlägt. Ein Abbruch durch Fehler im Ablauf setzt den Status ebenfalls
auf 1, die bis dahin gesammelten Zeilen bleiben erhalten. Exit-Status 2 bedeutet
falsche Aufrufparameter.

## Ablauf

1. **Mandanten:** `GET /tenant` mit A und B. Beide Antworten müssen 200 sein und
   verschiedene Mandanten-IDs liefern. Sonst bricht der Lauf ab, weil das Setup falsch ist.
2. **Streams:** B öffnet zuerst einen WebSocket-Stream, dann A. Jeder holt sich dafür
   ein Ticket (`POST /stream/ticket`) und verbindet sich mit `/api/v1/stream?ticket=…`.
   Die Nachrichten werden gesammelt.
3. **Objekte von A anlegen:** Webhook-Kanal (`https://target.as06.invalid/hook`), HTTP-Monitor
   (`https://target.as06.invalid/`, Intervall 60 s, 0 Wiederholungen), Wartungsfenster für den
   Monitor (einmalig, in 24 h) und ein API-Token (Rolle `tenant-viewer`, 2 h gültig).
   Vom Token wird nur die ID behalten.
4. **Zugriff von B auf die Objekte von A:** Jede Anfrage erwartet eine Verweigerung:
   - 404 bei Lesen, Ändern (PUT), Löschen, Pausieren, Fortsetzen, Klonen, Test,
     Heartbeats, Uptime, Kanal-Lesen/-Ändern/-Löschen/-Test, Wartungsfenster-Lesen/-Ändern/-Löschen,
     Token-Widerruf.
   - Incidents mit `monitorId` von A: 404, oder 200 mit leerer Liste.
   - Bulk-Pause `POST /monitors/bulk` mit der ID von A: 404 oder 422 (unbekannte IDs).

   Zu jeder Antwort wird der **vollständige Rumpf** geprüft. Darin dürfen weder der
   Monitorname, der Kanalname, der Name des Wartungsfensters und des Tokens noch die
   Mandanten-ID von A vorkommen.
5. **Listen von B:** Monitore, Kanäle, Wartungsfenster, Incidents, API-Token und
   Audit-Log. Keine Liste darf eine ID oder einen Namen von A enthalten. Monitore und
   Incidents werden mit Seitenlimit 500 gelesen, die übrigen Listen mit der
   Standardgröße der ersten Seite.
6. **Unverändert für A:** Der Monitor existiert noch mit gleichem Namen, ist nicht
   pausiert und hat dieselbe `updatedAt` wie nach dem Anlegen. Kanal und Wartungsfenster
   existieren mit gleichem Namen, das Fenster ist aktiv. Das Token ist gelistet und nicht
   abgelaufen, also nicht widerrufen.
7. **Ereignisse:**
   - A pausiert und setzt den Monitor fort. Der Treiber wartet, bis der Stream von A
     eine Nachricht mit der ID des Monitors enthält (höchstens `-event-wait`). Zeile:
     „A's stream shows event of its monitor“.
   - **Positivkontrolle für B:** B legt einen eigenen HTTP-Monitor an (Name mit
     Zufallssuffix, Ziel `https://target.as06.invalid/`, Intervall 60 s, 0 Wiederholungen),
     pausiert und setzt ihn fort. Der Stream von B muss eine Nachricht mit der ID dieses
     Monitors enthalten (höchstens `-event-wait`). Zeilen: „B's own monitor created“,
     „B's own monitor paused“ und „resumed“, „B's stream shows event of its own monitor“.
     Ohne diese Kontrolle würde ein leerer Stream von B die Leck-Prüfung trivial bestehen.
   - Danach 5 s Nachfrist, damit eine Panne im Stream von B Zeit hat, sichtbar zu werden.
   - Zuletzt darf keine Nachricht im Stream von B die ID, den Namen oder die
     Mandanten-ID von A oder die IDs der anderen Objekte von A enthalten. Die Zeile
     „B's stream carries no data of A“ nennt die Gesamtzahl der Nachrichten, die B
     empfangen hat (einschließlich der Ereignisse des eigenen Monitors von B).
8. **Aufräumen:** Zuerst der Monitor von B, dann Wartungsfenster, Monitor, Kanal und
   API-Token von A (in dieser Reihenfolge). Jedes Löschen ist eine eigene Zeile. Ein
   fehlgeschlagenes Löschen steht als `FAIL` in der Tabelle, bricht aber den
   Aufräumvorgang nicht ab und verdeckt keine früheren Ergebnisse.

## Abweichungen vom Auftrag und Grenzen

- **Wartezeit auf Ereignisse:** Die Heartbeats kommen je Monitor im Takt des Intervalls
  (60 s). Ein Fenster von „etwa 5 s“ reicht deshalb nicht. Der Treiber wartet bis
  `-event-wait` (Standard 90 s) und hält danach für B eine feste Nachfrist von 5 s.
- **Revision:** Der Monitor trägt in der API keine `revision`. Die Unveränderlichkeit
  wird über `updatedAt`, Name und den Pausenzustand geprüft.
- **PATCH:** Die API-Spezifikation kennt für Monitore kein PATCH. Geprüft werden PUT und
  DELETE.
- **Zielhost:** Monitor und Webhook zeigen auf `target.as06.invalid` (RFC 2606, nie
  auflösbar). Der Monitor geht DOWN, der Webhook schlägt fehl; es verlässt kein Verkehr
  zu einem fremden Host den Rechner. Das Ergebnis hängt davon nicht ab.
- **Leck-Prüfung als Textsuche:** Die Prüfung sucht die Zeichenketten der Objekte von A
  im Antworttext. Die Namen tragen einen zufälligen Suffix, die IDs sind UUIDs. Eine sehr
  kurze Mandanten-ID könnte daher einen falschen Treffer melden, was als FAIL sichtbar wird.
- Der Treiber räumt auch nach einem Fehler auf, solange Objekte von A angelegt wurden.

## Tests

```bash
go test -race ./test/acceptance/as06/... ./test/internal/ucapi/...
```

Die Unit-Tests brauchen keinen laufenden Knoten. Sie prüfen die Optionen (fehlende und
leere Token-Dateien, Bereichsgrenzen), das Urteil über jede Antwort (Verweigerung, Leck,
unerwarteter Erfolg), das Bereinigen der Ausgabe von Token-Werten, die Tabelle und den
Stream-Client gegen einen `httptest`-Knoten.

Der Ablauf selbst läuft nur gegen einen echten Knoten.
