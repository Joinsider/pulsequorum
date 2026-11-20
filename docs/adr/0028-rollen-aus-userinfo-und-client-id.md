# ADR 0028: Rollen aus `/userinfo` und `client_id` als Ersatz für `azp`

|                 |                                                       |
|-----------------|-------------------------------------------------------|
| **Status**      | Accepted                                              |
| **Datum**       | 2026-11-17                                            |
| **Entscheider** | Projektinhaber                                              |
| **Bezug**       | ADR 0027, FA-AUTH-05..07, FA-AUTH-09, PF-31, RFC 9068 |

## Kontext

ADR 0027 setzt voraus, dass jeder OIDC-Anbieter Rollen oder Gruppen in das Access-Token schreibt und dass das Token ein
`azp` trägt. Die Tests gegen echte Anbieter (Keycloak 26.8.2, PocketID v2.19.3) zeigen, dass das nicht für alle gilt.

Was PocketID v2.19.3 tatsächlich tut (Quelle: Quelltext, Tag v2.19.3 und Fork `pocket-id/fosite` v1.4.3):

- **Format:** Access-Tokens sind JWTs nach RFC 9068 (`typ: at+jwt`, RS256).
- **Claims:** Das Token enthält `iss`, `sub`, `aud` (Client-ID und APP_URL), `client_id`, `exp`, `iat`, `jti`, `scope`
  und `scp`.
- **Kein `azp`:** Stattdessen setzt PocketID `client_id`, den Claim, den RFC 9068 dafür vorsieht.
- **Keine Gruppen und keine eigenen Claims im Access-Token:** `groups` steht nur im ID-Token und unter `/userinfo`.
- **Gruppen-Beschränkung:** Ein Client lässt sich auf Gruppen beschränken („Allowed user groups“). PocketID prüft das
  bei der Ausgabe des Tokens.

Folgen für ADR 0027:

- Mit dem Standard `OIDC_REQUIRE_AZP=true` wird jedes PocketID-Token abgelehnt. Mit `false` wird gar kein Client
  geprüft.
- Das Profil `generic` findet keinen Rollen-Claim. Jeder Nutzer bekommt deshalb 403.

Zitadel v4.20.3 setzt bei Machine- und SPA-Tokens ebenfalls nur `client_id` und kein `azp` (gemessen im Test unter
`deploy/oidc-dev/zitadel`). Andere verbreitete Anbieter verhalten sich ähnlich. Rollen nur im ID-Token oder unter
`/userinfo` sind häufig.

## Optionen

### Frage 1: `client_id` (RFC 9068)

**Option 1A:** `client_id` wird wie `azp` behandelt. Fehlt `azp`, muss `client_id` in `OIDC_CLIENT_IDS` stehen. Sind
beide vorhanden und verschieden, wird das Token abgelehnt.

- Vorteile: Entspricht dem Standard für JWT-Access-Tokens. Die Prüfung des Clients bleibt für PocketID und andere
  Anbieter nach RFC 9068 erhalten.
- Nachteile: keine.

**Option 1B:** keine Änderung. PocketID läuft dann nur mit `OIDC_REQUIRE_AZP=false`, also ganz ohne Prüfung des Clients.

### Frage 2: Rollen, wenn das Access-Token keine enthält

**Option 2A: Rollen aus `/userinfo` (`OIDC_ROLES_SOURCE=userinfo`, opt-in).**

- **Ablauf:** Das Token wird wie bisher geprüft. Danach ruft der Voter mit diesem Token den `userinfo_endpoint` aus der
  Discovery auf und liest den Rollen-Claim, z.B. `groups`.
- **Cache:** Das Ergebnis wird je Token-Hash bis `exp` zwischengespeichert, im Speicher und mit Größenbegrenzung. Pro
  Token gibt es also höchstens einen Aufruf je Voter.
- **Prüfung:** Das `sub` der Antwort muss dem `sub` des Tokens entsprechen.
- **Ausfall:** Ist der IdP nicht erreichbar und das Token nicht im Cache, folgt `503`.
- Vorteile: Funktioniert mit jedem Anbieter, der Gruppen nur unter `/userinfo` liefert. Rollen bleiben feingranular. Den
  Anzeigenamen gibt es dabei gleich mit.
- Nachteile: Bei jedem neuen Token läuft ein Aufruf beim IdP im Anfragepfad. Damit weicht der Modus von „kein Aufruf pro
  Anfrage“ ab, wenn auch nur pro Token. FA-AUTH-07 gilt nur noch eingeschränkt: Tokens, die schon im Cache sind, laufen
  weiter, neue Tokens während eines Ausfalls nicht. Neue Tokens kann der IdP in dieser Zeit aber ohnehin nicht
  ausstellen. Ausnahme ist ein Token, das kurz vor dem Ausfall ausgestellt wurde. Etwas mehr Code.

**Option 2B: feste Rolle (`OIDC_DEFAULT_ROLE`).**

- **Ablauf:** Fehlt der Rollen-Claim, bekommt jedes gültige Token dieses Clients die konfigurierte Rolle. Wer überhaupt
  hinein darf, regelt der IdP, bei PocketID über „Allowed user groups“.
- Vorteile: Kein Aufruf zur Laufzeit, sehr einfach, und FA-AUTH-07 gilt voll.
- Nachteile: Alle Nutzer haben dieselbe Rolle. Für mehrere Rollen bräuchte es mehrere Clients, die die SPA nicht
  unterstützt. Ein Fehler in der Gruppen-Beschränkung beim IdP öffnet den Zugang mit dieser Rolle.

**Option 2C: Rollen im Cluster vergeben.**

- **Ablauf:** Rollen werden je `sub` im Raft-Zustand gepflegt. Ein Admin vergibt sie in der UI.
- Vorteile: Unabhängig vom IdP.
- Nachteile: Neue Nutzerverwaltung neben dem IdP. Widerspricht Option 4A aus ADR 0027. Deutlich mehr Aufwand, und die
  Frage, wie der erste Admin entsteht, stellt sich erneut.

## Entscheidung

Vorschlag: **1A und 2A, zusätzlich 2B als einfache Alternative.**

- **Rollenquelle:** `OIDC_ROLES_SOURCE` mit `access_token` (Standard, Verhalten wie bisher) oder `userinfo`.
- **Feste Rolle:** `OIDC_DEFAULT_ROLE` ist optional und greift nur, wenn die gewählte Quelle keine gültige Rolle
  liefert. `platform-admin` ist als feste Rolle **nicht** erlaubt.
- **Begründung:** Mit 2A funktioniert PocketID mit echten Rollen. Mit 2B kann ein einzelner Betreiber ohne Gruppenpflege
  starten. 1A ist der Standard nach RFC 9068 und schwächt nichts ab.

**Details zu 2A:**

- `userinfo_endpoint` kommt aus der Discovery, die schon den Issuer prüft. Es gelten dieselben Regeln wie für JWKS: nur
  HTTPS (außer bei `OIDC_INSECURE_HTTP`), keine Weiterleitungen, Antworten höchstens 64 KiB, Timeout 5 s.
- Den Aufruf gibt es erst nach vollständiger Prüfung des Tokens. Ungültige Tokens erzeugen also keinen Aufruf beim IdP.
- Gleichzeitige Anfragen mit demselben Token teilen sich einen Aufruf. Der Cache hält höchstens 10 000 Einträge.
- Weicht `sub` ab, wird das Token abgelehnt.
- `401` und `403` von `/userinfo` führen zu `401`. Netzwerkfehler führen zu `503`.
- Die Zahl der `userinfo`-Aufrufe und der Fehlschläge wird als Metrik gezählt.

**Gilt nur für das Profil `generic`.** Bei Zitadel bleiben die Rollen im Access-Token, das ändert sich nicht.

## Konsequenzen

- **Positiv:**
    - PocketID und ähnliche Anbieter sind nutzbar.
    - Die Prüfung des Clients bleibt auch für Anbieter nach RFC 9068 erhalten.
- **Negativ / Risiken:**
    - Mit `userinfo` hängt die erste Anfrage mit jedem neuen Token am IdP.
    - Die feste Rolle verlagert die Zugangskontrolle ganz zum IdP. Das muss die Doku deutlich sagen.
- **Folgearbeiten:**
    - Doku `docs/betrieb/oidc.md` mit einem PocketID-Abschnitt, der gegen eine echte Instanz geprüft ist.
    - Claim-Fixture aus PocketID.
    - Den Anzeigenamen in der SPA aus dem ID-Token zeigen statt `sub` (Befund aus dem Keycloak-Test).

## Approval

| Datum      | Status   | Durch    | Anmerkung                                                           |
|------------|----------|----------|---------------------------------------------------------------------|
| 2026-11-17 | Proposed | Claude   | Befund aus dem Test gegen PocketID v2.19.3 und Keycloak 26.8.2      |
| 2026-11-17 | Accepted | Projektinhaber | im Chat angenommen („Ja ADR 28 annehmen“), eingetragen durch Claude |
