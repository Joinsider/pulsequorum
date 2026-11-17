# ADR 0027: Zitadel-Anbindung (JWT-Validierung, Mandant und Rollen, SPA-Login)

| | |
|---|---|
| **Status** | Proposed |
| **Datum** | 2026-11-17 |
| **Entscheider** | Projektinhaber |
| **Bezug** | FA-AUTH-05..09, FA-TEN-02/03, NFA-SEC-04/05, PF-31, PF-34, ADR 0008, ADR 0010, ADR 0018, ADR 0019, ADR 0025 |

## Kontext

Mit `AUTH_MODE=zitadel` meldet sich die SPA bei Zitadel an. Die Voter prüfen die Access-Tokens selbst. Dabei gelten:

- **FA-AUTH-05:** Authorization Code Flow mit PKCE. Die Voter prüfen JWT-Access-Tokens lokal über gecachte JWKS: Issuer exakt, `aud` = Projekt-ID, `exp`/`nbf`, Algorithmus fest. Opake Tokens werden mit klarer Meldung abgelehnt.
- **FA-AUTH-06:** Der Mandant kommt aus der Organisation, die Rollen aus dem Projekt-Rollen-Claim. Die Audience allein ist keine Autorisierung (PF-31).
- **FA-AUTH-07:** Fällt Zitadel aus, laufen bestehende Sitzungen, Checks und Alerts weiter. Bei unbekanntem `kid` wird nachgeladen, aber mit Rate-Limit.
- **NFA-SEC-04:** Tokens liegen in der SPA nur im Speicher.
- **ADR 0025:** Der Mandant `_system` ist für alle Akteure gesperrt. `auth.FromContext` lehnt ihn ab, und das bleibt so.

Die Recherche zu Zitadel hat ergeben (Quellen: zitadel.com/docs, Abschnitte Claims, Scopes, Web Keys, Projects):

- **Token-Typ:** Access-Tokens sind standardmäßig opak. JWT muss pro Anwendung eingestellt werden (Token Settings → Token Type).
- **Laufzeit:** Access-Tokens gelten standardmäßig **12 h**, und zwar instanzweit.
- **Signatur:** RS256 ist Standard. ES256/384/512 und EdDSA sind möglich. JWKS liegt unter `/oauth/v2/keys` (`max-age=300`), Discovery unter `/.well-known/openid-configuration`.
- **`aud`:** ist ein Array mit Client-IDs und, auf Anforderung über den Scope `urn:zitadel:iam:org:project:id:{projectId}:aud`, der Projekt-ID. `azp` nennt den anfragenden Client.
- **Name und E-Mail:** `preferred_username` und `email` stehen **nicht** im Access-Token, nur im ID-Token.
- **Rollen:** `urn:zitadel:iam:org:project:{projectId}:roles` hat die Form `{rolle: {orgId: primaryDomain}}`. Eine Rolle gilt also immer in einer bestimmten Organisation.
- **Organisation des Nutzers:** `urn:zitadel:iam:user:resourceowner:id` steht im Token, wenn der Scope `urn:zitadel:iam:user:resourceowner` angefordert wird.
- **Projekt-Grants:** Admins einer berechtigten Organisation dürfen nur die Rollen vergeben, die der Projekteigentümer freigegeben hat. Das ist laut Doku so, aber nicht selbst geprüft.

Im Code gilt heute:

- `internal/auth/jwt.go` ist ein eigener, kleiner Verifier (nur EdDSA, strikte Claims).
- `Local.Authenticate` lädt bei jeder Anfrage den Nutzer aus dem Store.
- `tenants.zitadel_org_id` (UNIQUE) existiert seit der ersten Migration.
- `GET /config.json` wird vom Backend erzeugt und kennt `auth.mode`, `issuer` und `clientId`.
- `Me.id` ist im OpenAPI-Vertrag als `uuid` typisiert. Zitadel-`sub` ist eine numerische Zeichenkette.

## Optionen

### Frage 1: JWT- und JWKS-Prüfung im Backend

**Option 1A: eigener Verifier auf Basis der Standardbibliothek** (`crypto/rsa`, `crypto/ecdsa`, `crypto/ed25519`), nach dem Muster von `jwt.go`.
- Vorteile: Keine neue Abhängigkeit. Algorithmen werden per Allow-List festgelegt, nie per Header. Gleicher Stil wie der lokale Verifier. Etwa 300 Zeilen, vollständig testbar.
- Nachteile: Sicherheitskritischer Eigenbau, deshalb besonders gründliche Tests nötig (Negativtests, Fuzzing des Parsers).

**Option 1B: `github.com/go-jose/go-jose/v4`** (Apache-2.0) für JWS und JWK, Cache und Claims-Prüfung selbst.
- Vorteile: Weit verbreitet.
- Nachteile: Neue Abhängigkeit mit mehreren Sicherheitsmeldungen in den letzten Jahren, darunter fünf GHSAs, die in v4.1.7 behoben wurden. Die Claims-Prüfung und den Cache mit Rate-Limit müssen wir trotzdem selbst schreiben.

**Option 1C: `github.com/coreos/go-oidc/v3`** (Apache-2.0, nutzt go-jose).
- Vorteile: Discovery und `RemoteKeySet` sind fertig.
- Nachteile: Das Nachladen bei unbekanntem `kid` hat kein dokumentiertes Rate-Limit. Es gibt keine persistente Ablage für Neustarts während eines IdP-Ausfalls. Die Claims-Prüfung bleibt trotzdem unsere Aufgabe.

### Frage 2: OIDC in der SPA

**Option 2A: `oidc-client-ts`** (Apache-2.0, einzige Abhängigkeit `jwt-decode`). Der `userStore` wird auf `InMemoryWebStorage` gesetzt.
- Vorteile: Framework-unabhängig, klein, PKCE und Refresh-Token-Grant sind eingebaut. Speicherort und Ablauf sind explizit steuerbar.
- Nachteile: Wir brauchen eine dünne Angular-Hülle. Dass `InMemoryWebStorage` in der aktuellen Version vorhanden ist, prüft der Umsetzungsauftrag.

**Option 2B: `angular-auth-oidc-client`** (MIT, v22).
- Vorteile: Für Angular gebaut, wird aktiv gepflegt.
- Nachteile: Ob Tokens ausschließlich im Speicher gehalten werden können, ist nicht belegt. Die Bibliothek bringt eigene Konventionen mit, etwa iframe-Silent-Renew.

**Option 2C: eigene PKCE-Implementierung**, etwa 200 Zeilen mit Web Crypto.
- Vorteile: Keine Abhängigkeit, volle Kontrolle.
- Nachteile: Eigenbau im Sicherheitspfad der SPA.

### Frage 3: JWKS bei Ausfall von Zitadel und bei Neustart

**Option 3A: Cache je Voter im Speicher und als Datei** `<DATA_DIR>/zitadel-jwks.json`, zusammen mit dem Discovery-Dokument.
- Vorteile: Einfach. Ein Voter, der während eines IdP-Ausfalls neu startet, kann weiter prüfen.
- Nachteile: Ein Voter, der Zitadel nie erreicht hat, prüft gar nicht. Bei neuen Votern bekommt man das mit.

**Option 3B: JWKS über Raft replizieren.** Der Leader holt die Schlüssel und schreibt sie per FSM-Befehl.
- Vorteile: Alle Voter haben dieselben Schlüssel, auch abgeschnittene Voter und neue Voter.
- Nachteile: Neuer FSM-Befehl und neue Tabelle. Die Schlüsselrotation hängt dann am Quorum. Fürs Erste mehr Aufwand als nötig.

### Frage 4: lokale Anmeldung im Modus `zitadel` (bis AP-4.5)

**Option 4A: Die Modi schließen sich aus.**
- Bei `zitadel` antworten `/auth/setup`, `/auth/login`, `/auth/refresh`, `/auth/password` und die Nutzerverwaltung `/users` mit `409 Conflict` und dem Hinweis „managed in Zitadel“.
- Notzugang nur über die Recovery-CLI aus ADR 0019, mit Wechsel des `AUTH_MODE`.
- Vorteile: Eine einzige Vertrauensquelle, kleinere Angriffsfläche.
- Nachteile: Ist Zitadel lange ausgefallen, kommt man ohne bestehende Sitzung nur über die CLI an das System.

**Option 4B: wie 4A, zusätzlich ein Notzugang** `AUTH_LOCAL_BREAKGLASS=true` (Standard aus).
- Lokale `platform-admin`-Konten können sich weiter anmelden. Der Authenticator unterscheidet die Tokens am `iss`.
- Vorteile: Bedienbar bei langem IdP-Ausfall.
- Nachteile: Zwei Vertrauensquellen. Passwortkonten bleiben ein Angriffsziel. Die Logik in der SPA wird komplexer.

## Entscheidung

Vorschlag: **1A, 2A, 3A, 4A.**

**Begründung:**
- **1A:** Die Claims-Prüfung, der Cache und das Rate-Limit müssen in jeder Variante selbst geschrieben werden. Der Teil, den eine Bibliothek abnimmt, ist bei festen Algorithmen klein: JWS-Parsing und RSA/ECDSA-Verify aus der Standardbibliothek. Das passt zu ADR 0008 und zur Regel „Standardbibliothek zuerst“.
- **2A:** Die Speicherung lässt sich explizit steuern, und die Bibliothek hat die wenigsten Abhängigkeiten.
- **3A und 4A:** jeweils die einfachste Variante, die die Anforderungen erfüllt.

### Backend: Token-Prüfung (`internal/auth/zitadel`, Code vom Hauptagenten)

1. **Form:**
   - Ein Bearer-Token, das kein JWT aus drei Teilen ist, wird mit `401` und dem Problem-Detail „opaque access tokens are not supported; set the application's token type to JWT“ abgelehnt (FA-AUTH-05).
   - Alle anderen Fehler ergeben `401` ohne Begründung. Die Begründung wird geloggt.
2. **Header:**
   - `alg` muss in `ZITADEL_ALGORITHMS` stehen (Standard `RS256`; erlaubt sind `RS256`, `ES256`, `EdDSA`).
   - Der `alg` des Schlüssels im JWKS muss übereinstimmen, `kid` ist Pflicht.
   - RSA-Schlüssel müssen mindestens 2048 Bit haben.
3. **Claims:**
   - `iss` ist exakt gleich `ZITADEL_ISSUER`.
   - `aud` enthält `ZITADEL_PROJECT_ID`.
   - `azp` steht in `ZITADEL_CLIENT_IDS` (Standard: die SPA-Client-ID). Damit werden Tokens anderer Anwendungen desselben Projekts abgelehnt.
   - `sub` ist nicht leer.
   - Für `exp`, `nbf` und `iat` gelten 30 s Leeway wie lokal.
4. **Laufzeit:**
   - `exp − iat` darf höchstens `ZITADEL_MAX_TOKEN_TTL` betragen (Standard **1 h**).
   - Der Standard von Zitadel (12 h) wird damit bewusst abgelehnt. Rollenentzug und Sperren in Zitadel wirken erst, wenn das Token abläuft.
   - Die Doku empfiehlt 15 min. Beim Start loggt der Voter einen Hinweis auf diese Einstellung.
5. **Mandant:**
   - Die Organisation ist `urn:zitadel:iam:user:resourceowner:id`.
   - Gesucht wird der Mandant mit `zitadel_org_id` = Organisation. Gibt es keinen, folgt `403` „organization is not provisioned“. Die automatische Anlage kommt mit AP-4.2 (FA-TEN-03).
   - Ein gesperrter Mandant ergibt `403`.
   - `_system` hat nie eine `zitadel_org_id`. FSM und Store lehnen das ab, und `FromContext` bleibt die zweite Sperre.
6. **Rollen:**
   - Gezählt werden nur die Schlüssel aus `urn:zitadel:iam:org:project:{ZITADEL_PROJECT_ID}:roles`, **deren Organisations-Map die Organisation des Nutzers enthält**.
   - Unbekannte Rollennamen werden ignoriert. Bleibt keine gültige Rolle übrig, folgt `403` (FA-AUTH-06).
   - `platform-admin` wird nur anerkannt, wenn die Organisation gleich `ZITADEL_PLATFORM_ORG_ID` ist. Damit kann eine berechtigte Kundenorganisation keinen Plattform-Admin erzeugen, auch wenn ein Grant falsch konfiguriert ist.
7. **Principal:**
   - `UserID` = `sub`, `TenantID` aus Schritt 5, `Roles` aus Schritt 6, `ExpiresAt` = `exp`.
   - `Username` = `sub`. Den Anzeigenamen nimmt die SPA aus dem ID-Token.
   - Pro Anfrage gibt es keinen Aufruf bei Zitadel.

### Backend: JWKS-Cache

- **Discovery:** Beim Start wird das Discovery-Dokument geladen. Sein `issuer` muss exakt gleich `ZITADEL_ISSUER` sein. `jwks_uri` wird daraus übernommen.
- **Regelmäßiges Nachladen:** alle 5 min, oder früher, wenn `max-age` kürzer ist.
- **Unbekanntes `kid`:** löst sofort ein Nachladen aus, höchstens einmal je 30 s pro Voter. Parallele Anfragen warten auf denselben Abruf (single-flight).
- **Fehlschlag:** Schlägt ein Abruf fehl, bleiben die letzten gültigen Schlüssel unbegrenzt in Gebrauch (FA-AUTH-07). Die Metrik `uptimecluster_auth_jwks_fetch_failures_total` zählt die Fehlschläge mit. Ein Systemalert dazu folgt bei Bedarf in P5.
- **Wechsel:** Ein erfolgreich geladenes Schlüsselset ersetzt das alte vollständig. Damit verschwinden auch Schlüssel, die Zitadel entfernt hat.
- **Datei:** Erfolgreich geladene Daten werden atomar nach `<DATA_DIR>/zitadel-jwks.json` geschrieben (Rechte 0600). Beim Start wird zuerst diese Datei gelesen.
- **Ohne Schlüssel:** Hat ein Voter weder Datei noch Verbindung, lehnt er jede Anfrage mit `503` ab und meldet `/health/ready` als nicht bereit. Checks und Alerts laufen weiter.
- **Verbindung zu Zitadel:**
  - Der HTTP-Client hat Timeouts und eine Größenbegrenzung (JWKS ≤ 64 KiB).
  - Zitadel wird nur über HTTPS angesprochen. Ausnahme ist `ZITADEL_INSECURE_HTTP=true` für die Entwicklung, dann gibt es eine Log-Warnung.
  - Weiterleitungen werden nicht befolgt.

### Mandantenzuordnung

- **Neuer FSM-Befehl:** `BindTenantOrg{tenant_id, org_id}` setzt `tenants.zitadel_org_id`. Er bekommt Befehlsversion 7 und wird per Feature-Gate erst angewandt, wenn alle Voter Version 7 können (ADR 0018).
- **Regeln:**
  - Abgelehnt werden: `_system`, eine leere Org-ID und eine Org-ID, die schon einem anderen Mandanten gehört.
  - Ein Mandant, der schon gebunden ist, wird nur über die Admin-API aus AP-4.2 neu gebunden.
- **Start:** Ist `default` noch ungebunden, bindet der Leader ihn beim Start an `ZITADEL_PLATFORM_ORG_ID`. Damit kann sich der erste Plattform-Admin anmelden.
- **Bestehende lokale Daten:** Die Übernahme lokaler Konten und Daten bleibt AP-4.5 (FA-AUTH-08).

### API und Vertrag

- **`Me.id`:** wird im OpenAPI-Vertrag von `format: uuid` auf `string` gelockert. Für Clients ist das rückwärtskompatibel.
- **`Me.authMode`:** liefert `zitadel`.
- **WebSocket:** Das Ticket bekommt als Sitzungsende das `exp` des Access-Tokens. `revalidate` prüft im Modus `zitadel` den Mandanten, ob er noch existiert und nicht gesperrt ist. Beim Ablauf schließt der Server die Verbindung, und die SPA verbindet sich mit neuem Token und Ticket wieder.
- **`/config.json`:** liefert im Modus `zitadel` die Felder `issuer`, `clientId`, `projectId` und `scopes`, mit den Scopes `openid profile offline_access urn:zitadel:iam:user:resourceowner urn:zitadel:iam:org:project:id:{projectId}:aud urn:zitadel:iam:org:projects:roles`.
- **CSP (NFA-SEC-05):** Im Modus `zitadel` wird `connect-src` um den Origin des Issuers erweitert. Für alles andere bleibt die CSP unverändert.

### SPA

- **Bibliothek:** `oidc-client-ts`, gekapselt in einen `ZitadelAuthStrategy` hinter dem bestehenden `AuthService` (ADR 0008).
- **Speicherorte:**
  - Access-, ID- und Refresh-Token liegen nur im Speicher (`InMemoryWebStorage`).
  - Nur der kurzlebige Zustand des Redirects (`state`, `code_verifier`) liegt in `sessionStorage` und wird nach dem Callback gelöscht.
- **Verlängerung:** über den Refresh-Token-Grant, kurz vor `exp`.
- **Nach einem Reload:** Die Tokens sind dann weg. Die SPA leitet auf oberster Ebene mit `prompt=none` zu Zitadel um und kommt mit der Zitadel-Sitzung ohne Eingabe zurück. Es gibt kein iframe, und es sind keine Third-Party-Cookies nötig. Liefert Zitadel `login_required`, startet ein normaler Login.
- **Logout:** über `end_session_endpoint` mit `id_token_hint`.

### Konfiguration

| Variable | Bedeutung |
|----------|-----------|
| `AUTH_MODE=zitadel` | schaltet den Modus ein |
| `ZITADEL_ISSUER` | Pflicht |
| `ZITADEL_PROJECT_ID` | Pflicht |
| `ZITADEL_CLIENT_IDS` | Pflicht, kommagetrennt; die erste ID ist die SPA |
| `ZITADEL_PLATFORM_ORG_ID` | Pflicht |
| `ZITADEL_ALGORITHMS` | Standard `RS256` |
| `ZITADEL_MAX_TOKEN_TTL` | Standard `1h`, höchstens `12h` |
| `ZITADEL_INSECURE_HTTP` | Standard `false` |

Ist die Konfiguration unvollständig, startet der Voter nicht.

### Testkonzept

- **Fake-IdP:** `internal/auth/zitadeltest` ist ein Fake-IdP auf Basis von `httptest`, ohne Abhängigkeit. Er bietet Discovery, JWKS und das Signieren beliebiger Claims mit RSA, EC und Ed25519. Er kann Schlüssel rotieren, sich abschalten, mit 500 antworten und Antworten verzögern. Er zählt die Abrufe, damit sich das Rate-Limit prüfen lässt.
- **Tabellentests für jede Prüfregel:**
  - opakes Token
  - `alg=none`, HS256 mit dem öffentlichen Schlüssel als Secret, `alg`-Mismatch zum JWK
  - falscher Issuer, z.B. mit angehängtem Slash
  - fremde Audience, fremder `azp`
  - abgelaufenes Token, zu lange Laufzeit
  - Rolle in fremder Organisation
  - `platform-admin` aus einer Kundenorganisation
  - unbekannte Organisation, gesperrter Mandant, `_system`
- **Fuzzing:** ein Fuzz-Test für den Parser.
- **Ausfall (FA-AUTH-07):** IdP abschalten. Gecachte Tokens werden weiter akzeptiert, das Nachladen bleibt gedrosselt. Ein Neustart mit Cache-Datei funktioniert.
- **FSM:** Tests für `BindTenantOrg`, einschließlich Feature-Gate und Sperre von `_system`.
- **SPA:** Unit-Tests der Strategy mit gemocktem `UserManager`. Ein Test stellt sicher, dass weder `localStorage` noch `sessionStorage` Tokens enthalten.
- **Gegen echtes Zitadel:** `deploy/zitadel-dev/` ist ein Compose-Beispiel mit Zitadel für einen manuellen End-to-End-Lauf, nicht in CI. Ein dabei aufgezeichnetes, echtes Token (ohne Signaturschlüssel) dient als Fixture für die Claim-Formen. So wird geprüft, was die Recherche nur aus der Doku belegt.

## Konsequenzen

- **Positiv:**
  - Keine neue Go-Abhängigkeit.
  - Die Autorisierung prüft immer Organisation und Rolle zusammen (PF-31).
  - Ein Ausfall von Zitadel stört bestehende Sitzungen und Neustarts mit Cache-Datei nicht.
  - Es sind keine Third-Party-Cookies nötig.
- **Negativ / Risiken:**
  - Der Verifier ist sicherheitskritischer Eigenbau. Das wird durch strenge Negativtests und Fuzzing abgefedert.
  - Rollenentzug wirkt erst nach Ablauf des Tokens, höchstens nach `ZITADEL_MAX_TOKEN_TTL`.
  - Ein Reload kostet einen Redirect zu Zitadel. Ist Zitadel dabei nicht erreichbar, ist nach dem Reload keine Anmeldung möglich, eine bestehende Sitzung ohne Reload dagegen schon.
  - Neue npm-Abhängigkeit `oidc-client-ts` (Apache-2.0, passt zur Allow-List aus ADR 0010).
- **Folgearbeiten:**
  - AP-4.2: automatische Anlage beim ersten Login und Admin-API zur Org-Bindung.
  - AP-4.5: Migration lokaler Konten.
  - Doku „Zitadel einrichten“: Token-Typ JWT, Access-Token-Laufzeit 15 min, „Assert Roles on Authentication“, Projekt-Grants, Redirect-URIs.
  - STRIDE-Ergänzung unter `docs/security/` (NFA-SEC-08).

## Approval

| Datum | Status | Durch | Anmerkung |
|-------|--------|-------|-----------|
| 2026-11-17 | Proposed | Claude | |
