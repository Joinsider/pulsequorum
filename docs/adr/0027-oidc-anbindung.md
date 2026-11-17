# ADR 0027: OIDC-Anbindung mit Profilen (generisch und Zitadel)

| | |
| --- | --- |
| **Status** | Accepted |
| **Datum** | 2026-11-17 |
| **Entscheider** | Projektinhaber |
| **Bezug** | FA-AUTH-05..09, FA-TEN-02/03, NFA-SEC-04/05, PF-31, PF-34, ADR 0008, ADR 0010, ADR 0018, ADR 0019, ADR 0025 |

## Kontext

Das Pflichtenheft sieht neben dem lokalen Modus nur Zitadel als externen IdP vor (FA-AUTH-05..08). Wer selbst hostet, hat aber meist schon einen IdP, etwa Keycloak, Authentik, Authelia, Entra ID oder Google Workspace. Zitadel als einzige Option schließt diese Nutzer aus. Deshalb soll jeder OIDC-Anbieter nutzbar sein. Zitadel bleibt das vollständig getestete Profil und ist der einzige Weg zu mehreren Mandanten.

Die Anforderungen gelten unverändert für jeden Anbieter:

- **FA-AUTH-05:** Authorization Code Flow mit PKCE. Die Voter prüfen JWT-Access-Tokens lokal über gecachte JWKS: Issuer exakt, Audience, `exp`/`nbf`, Algorithmus fest. Opake Tokens werden mit klarer Meldung abgelehnt.
- **FA-AUTH-06:** Die Autorisierung prüft immer Mandant und Rolle zusammen, nie nur die Audience (PF-31).
- **FA-AUTH-07:** Fällt der IdP aus, laufen bestehende Sitzungen, Checks und Alerts weiter. Bei unbekanntem `kid` wird nachgeladen, aber mit Rate-Limit.
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

Andere Anbieter verhalten sich unterschiedlich. Die Angaben stammen aus allgemeinem Wissen und sind nicht gegen echte Instanzen geprüft:

- **Keycloak:** Access-Tokens sind JWT. Rollen stehen in `realm_access.roles` bzw. `resource_access.<client>.roles`. `aud` enthält die API nur mit einem Audience-Mapper.
- **Entra ID:** `aud` ist die Application-ID-URI, App-Rollen stehen in `roles`.
- **Authentik und Authelia:** JWT-Access-Tokens, Gruppen meist in `groups`.
- **Organisationen:** Mandanten im Sinne von Organisationen liefert keiner dieser Anbieter einheitlich.
- **Opake Tokens:** Reines OAuth2 ohne OIDC (z.B. GitHub) liefert opake Tokens. Die lassen sich nicht lokal prüfen.

Im Code gilt heute:

- `internal/auth/jwt.go` ist ein eigener, kleiner Verifier (nur EdDSA, strikte Claims).
- `Local.Authenticate` lädt bei jeder Anfrage den Nutzer aus dem Store.
- `tenants.zitadel_org_id` (UNIQUE) existiert seit der ersten Migration.
- `GET /config.json` wird vom Backend erzeugt und kennt `auth.mode`, `issuer` und `clientId`.
- `Me.id` ist im OpenAPI-Vertrag als `uuid` typisiert. Das `sub` externer IdPs ist eine beliebige Zeichenkette.

## Optionen

### Frage 0: Welche IdPs werden unterstützt?

**Option 0A: nur Zitadel**, wie im Pflichtenheft.

- Vorteile: Nur ein Claim-Format, am wenigsten Testaufwand.
- Nachteile: Wer einen anderen IdP hat, kann das Produkt nur mit einem zweiten IdP nutzen. Damit fällt ein großer Teil der Zielgruppe weg.

**Option 0B: generisches OIDC mit Profilen.**

- Das Profil `generic` ist auf einen Mandanten beschränkt, die Rollen kommen aus einem konfigurierbaren Claim.
- Das Profil `zitadel` kann zusätzlich mehrere Mandanten über Organisationen abbilden.
- Vorteile: Jeder OIDC-Anbieter mit JWT-Access-Tokens ist nutzbar. Prüfung, Cache und SPA-Login sind für alle Anbieter derselbe Code.
- Nachteile: Etwa 20–30 % mehr Aufwand für AP-4.1, vor allem für das Claim-Mapping, Tests und die Einrichtungsdoku. Zudem weicht das vom Pflichtenheft ab.

**Option 0C: mehrere IdPs gleichzeitig in einem Cluster.**

- Vorteile: Jeder Mandant könnte seinen eigenen IdP mitbringen.
- Nachteile: Die SPA braucht eine Issuer-Auswahl, und jeder Issuer muss einem Mandanten zugeordnet werden. Das ist deutlich mehr Angriffsfläche bei wenig Nutzen für den Anfang.

Reines OAuth2 ohne OIDC wird in keiner Option unterstützt, weil es FA-AUTH-07 nicht erfüllen kann. Der Weg dorthin führt über einen OIDC-Broker wie Keycloak, Authentik oder Dex.

### Frage 1: JWT- und JWKS-Prüfung im Backend

**Option 1A: eigener Verifier auf Basis der Standardbibliothek** (`crypto/rsa`, `crypto/ecdsa`, `crypto/ed25519`), nach dem Muster von `jwt.go`.

- Vorteile: Keine neue Abhängigkeit. Algorithmen werden per Allow-List festgelegt, nie per Header. Gleicher Stil wie der lokale Verifier. Etwa 300 Zeilen, vollständig testbar.
- Nachteile: Sicherheitskritischer Eigenbau, deshalb besonders gründliche Tests nötig (Negativtests, Fuzzing des Parsers).

**Option 1B: `github.com/go-jose/go-jose/v4`** (Apache-2.0) für JWS und JWK, Cache und Claims-Prüfung selbst.

- Vorteile: Weit verbreitet.
- Nachteile: Neue Abhängigkeit mit mehreren Sicherheitsmeldungen in den letzten Jahren, darunter fünf GHSAs, die in v4.1.7 behoben wurden. Die Claims-Prüfung und den Cache mit Rate-Limit müssen wir trotzdem selbst schreiben.

**Option 1C: `github.com/coreos/go-oidc/v3`** (Apache-2.0, nutzt go-jose).

- Vorteile:
  - `IDTokenVerifier` prüft schon die Algorithmus-Allow-List vor dem Parsen, genau eine Signatur, `iss` exakt, `aud` enthält einen Wert, `exp` und `nbf`.
  - Derselbe Pfad schützt den OIDC-Authenticator des Kubernetes-API-Servers und ist entsprechend viel genutzt und geprüft.
  - Über das Interface `KeySet` lässt sich der Schlüsselspeicher austauschen, ohne die Prüfung anzufassen.
- Nachteile:
  - Der mitgelieferte `RemoteKeySet` hat kein Rate-Limit und keine Ablage auf Platte. Wir ersetzen ihn deshalb durch einen eigenen `KeySet`.
  - Die Bibliothek ist für ID-Tokens gebaut. `azp`, `iat` und die Höchstlaufzeit prüfen wir selbst danach.
  - Drei neue Module: go-oidc, go-jose (indirekt) und `golang.org/x/oauth2`.

### Frage 2: OIDC in der SPA

**Option 2A: `oidc-client-ts`** (Apache-2.0, einzige Abhängigkeit `jwt-decode`). Der `userStore` wird auf `InMemoryWebStorage` gesetzt.

- Vorteile: Framework-unabhängig und unabhängig vom IdP, klein, PKCE und Refresh-Token-Grant sind eingebaut. Speicherort und Ablauf sind explizit steuerbar.
- Nachteile: Wir brauchen eine dünne Angular-Hülle. Dass `InMemoryWebStorage` in der aktuellen Version vorhanden ist, prüft der Umsetzungsauftrag.

**Option 2B: `angular-auth-oidc-client`** (MIT, v22).

- Vorteile: Für Angular gebaut (Provider, Guards, Interceptor), zertifiziert gegen beliebige OIDC-Anbieter, wird aktiv gepflegt. PKCE und Refresh-Tokens sind eingebaut, Silent Renew geht auch ohne iframe über Refresh-Tokens.
- Nachteile: Ob Tokens ausschließlich im Speicher gehalten werden können, ist nicht belegt. Die Bibliothek legt Tokens und Redirect-Zustand im selben Speicher ab. Ein reiner Speicher im RAM würde den `code_verifier` beim Redirect verlieren.

**Option 2C: eigene PKCE-Implementierung**, etwa 200 Zeilen mit Web Crypto.

- Vorteile: Keine Abhängigkeit, volle Kontrolle.
- Nachteile: Eigenbau im Sicherheitspfad der SPA.

### Frage 3: JWKS bei Ausfall des IdP und bei Neustart

**Option 3A: Cache je Voter im Speicher und als Datei** `<DATA_DIR>/oidc-jwks.json`, zusammen mit dem Discovery-Dokument.

- Vorteile: Einfach. Ein Voter, der während eines IdP-Ausfalls neu startet, kann weiter prüfen.
- Nachteile: Ein Voter, der den IdP nie erreicht hat, prüft gar nicht. Bei neuen Votern bekommt man das mit.

**Option 3B: JWKS über Raft replizieren.** Der Leader holt die Schlüssel und schreibt sie per FSM-Befehl.

- Vorteile: Alle Voter haben dieselben Schlüssel, auch abgeschnittene Voter und neue Voter.
- Nachteile: Neuer FSM-Befehl und neue Tabelle. Die Schlüsselrotation hängt dann am Quorum. Fürs Erste mehr Aufwand als nötig.

### Frage 4: lokale Anmeldung im Modus `oidc`

**Option 4A: Die Modi schließen sich aus.**

- Bei `oidc` antworten `/auth/setup`, `/auth/login`, `/auth/refresh`, `/auth/password` und die Nutzerverwaltung `/users` mit `409 Conflict` und dem Hinweis „managed by the identity provider“.
- Notzugang nur über die Recovery-CLI aus ADR 0019, mit Wechsel des `AUTH_MODE`.
- Vorteile: Eine einzige Vertrauensquelle, kleinere Angriffsfläche.
- Nachteile: Ist der IdP lange ausgefallen, kommt man ohne bestehende Sitzung nur über die CLI an das System.

**Option 4B: wie 4A, zusätzlich ein Notzugang** `AUTH_LOCAL_BREAKGLASS=true` (Standard aus).

- Lokale `platform-admin`-Konten können sich weiter anmelden. Der Authenticator unterscheidet die Tokens am `iss`.
- Vorteile: Bedienbar bei langem IdP-Ausfall.
- Nachteile: Zwei Vertrauensquellen. Passwortkonten bleiben ein Angriffsziel. Die Logik in der SPA wird komplexer.

## Entscheidung

Vorschlag: **0B, 1C, 2B, 3A, 4A.**

Projektinhaber hat im Chat 0B, 2B, 3A und 4A festgelegt. Für Frage 1 wollte er „1B oder 1C, je nachdem, was sicherer ist“, aber keinen Eigenbau (1A). Dieser Entwurf setzt das um, bis zur Annahme ist er aber nur ein Vorschlag.

**Begründung:**

- **0B:** Das Produkt ist für Nutzer mit jedem verbreiteten IdP nutzbar. Prüfung, Cache und SPA bleiben für alle Anbieter derselbe Code. Mehrere Mandanten gibt es nur dort, wo der IdP die Organisation zuverlässig mitliefert, also bei Zitadel.
- **1C statt 1B:**
  - Beide Optionen hängen an go-jose. Dessen Meldungen der letzten Jahre betrafen vor allem JWE (Entschlüsselung) und Speicherverbrauch beim Parsen. JWE nutzen wir nicht.
  - Der Unterschied liegt im eigenen Code. Mit 1B schreiben wir die Prüfung von Algorithmus, Signaturanzahl, `iss`, `aud`, `exp` und `nbf` selbst. Mit 1C übernimmt das der viel genutzte `IDTokenVerifier` von go-oidc (geprüft am Quelltext von v3.21.0, `oidc/verify.go`).
  - Eigener Code bleibt nur für den Schlüsselspeicher (Cache, Datei, Rate-Limit) und für die zusätzlichen Claims. Weniger sicherheitskritischer Eigenbau ist hier das sicherere Ergebnis.
- **2B:** Die Bibliothek ist für Angular gebaut und unabhängig vom IdP. Für den Speicher gibt es eine Auflage, siehe Abschnitt SPA.
- **3A und 4A:** jeweils die einfachste Variante, die die Anforderungen erfüllt.

### Modi und Profile

| `AUTH_MODE` | `OIDC_PROFILE` | Mandanten | Rollenquelle |
| ------------- | ---------------- | ----------- | -------------- |
| `local` (Standard) | – | einer (`default`) | lokale Nutzer (ADR 0008) |
| `oidc` | `generic` (Standard) | einer (`default`) | konfigurierbarer Claim, z.B. `groups`, `roles`, `realm_access.roles` |
| `oidc` | `zitadel` | `OIDC_TENANCY=single` (Standard) oder `multi` | Zitadel-Projekt-Rollen-Claim |

- `AUTH_MODE=zitadel` bleibt als Alias für `AUTH_MODE=oidc` mit `OIDC_PROFILE=zitadel` erhalten. Er wird beim Start mit einem Hinweis geloggt.
- Mehrere IdPs gleichzeitig (0C) sind nicht vorgesehen. Ein Cluster vertraut genau einem Issuer.

### Backend: gemeinsame Token-Prüfung (`internal/auth/oidc`, Code vom Hauptagenten)

**Umsetzung:**
- Die Prüfung läuft über `oidc.NewVerifier(OIDC_ISSUER, keySet, &oidc.Config{ClientID: OIDC_AUDIENCE, SupportedSigningAlgs: OIDC_ALGORITHMS})` aus `github.com/coreos/go-oidc/v3`.
- `keySet` ist ein eigener `oidc.KeySet` (siehe JWKS-Cache). Er prüft die Signatur mit `github.com/go-jose/go-jose/v4`.
- Nicht verwendet werden: `oidc.NewProvider`, `RemoteKeySet` und die Schalter `Skip*` und `InsecureSkipSignatureCheck`. Ein Test stellt sicher, dass keiner davon gesetzt ist.
- Die Versionen werden gepinnt, go-jose mindestens auf v4.1.9. Neue Meldungen fängt `govulncheck` in CI ab (NFA-SEC-06).
- Was go-oidc nicht prüft, prüfen wir danach selbst: `azp`, `iat`, die Höchstlaufzeit, `sub`, `nbf` mit 30 s statt 5 min Leeway, sowie Mandant und Rollen.
- Vor dem Parsen werden Tokens über 16 KiB abgelehnt.

**Prüfregeln:**

1. **Form:**
   - Ein Bearer-Token, das kein JWT aus drei Teilen ist, wird mit `401` und dem Problem-Detail „opaque access tokens are not supported; configure the identity provider to issue JWT access tokens“ abgelehnt (FA-AUTH-05).
   - Alle anderen Fehler ergeben `401` ohne Begründung. Die Begründung wird geloggt.
2. **Header:**
   - `alg` muss in `OIDC_ALGORITHMS` stehen (Standard `RS256`; erlaubt sind `RS256`, `ES256`, `EdDSA`).
   - Der `alg` des Schlüssels im JWKS muss übereinstimmen, `kid` ist Pflicht.
   - RSA-Schlüssel müssen mindestens 2048 Bit haben.
3. **Claims:**
   - `iss` ist exakt gleich `OIDC_ISSUER`.
   - `aud` enthält `OIDC_AUDIENCE`. Das ist Pflicht, damit Tokens, die der IdP für andere Anwendungen ausstellt, nicht gelten. Bei Zitadel ist es die Projekt-ID.
   - `azp` steht in `OIDC_CLIENT_IDS` (Standard: die SPA-Client-ID), sofern der Claim vorhanden ist.
   - Hat das Token kein `azp`, wird es nur angenommen, wenn `OIDC_REQUIRE_AZP=false` gesetzt ist (Standard `true`; manche IdPs setzen `azp` nicht).
   - `sub` ist nicht leer.
   - Für `exp`, `nbf` und `iat` gelten 30 s Leeway wie lokal.
4. **Laufzeit:**
   - `exp − iat` darf höchstens `OIDC_MAX_TOKEN_TTL` betragen (Standard **1 h**).
   - Lange Laufzeiten wie der Zitadel-Standard von 12 h werden damit bewusst abgelehnt. Rollenentzug und Sperren beim IdP wirken erst, wenn das Token abläuft.
   - Die Doku empfiehlt 15 min. Beim Start loggt der Voter einen Hinweis auf diese Einstellung.
5. **Mandant und Rollen:** liefert das Profil (siehe unten). Bleibt keine gültige Rolle übrig, folgt `403` (FA-AUTH-06). Ein gesperrter Mandant ergibt `403`.
6. **Principal:**
   - `UserID` = `sub`, `TenantID` und `Roles` aus dem Profil, `ExpiresAt` = `exp`.
   - `Username` = `sub`. Den Anzeigenamen nimmt die SPA aus dem ID-Token.
   - Pro Anfrage gibt es keinen Aufruf beim IdP.

### Profil `generic`

- **Mandant:** immer `default`. Mandanten-Claims im Token werden ignoriert.
- **Rollen:**
  - Gelesen wird der Claim `OIDC_ROLES_CLAIM` (Pflicht). Erlaubt sind ein Punktpfad für verschachtelte Objekte, z.B. `realm_access.roles`, und ein Array aus Strings oder ein einzelner String.
  - `OIDC_ROLE_MAP` ordnet Werte aus dem IdP den vier Rollen zu, z.B. `uptime-admins=tenant-admin,ops=tenant-editor,staff=tenant-viewer,platform=platform-admin`.
  - Ohne `OIDC_ROLE_MAP` gelten nur Werte, die exakt einer der vier Rollen entsprechen.
  - Unbekannte Werte werden ignoriert.
- **`platform-admin`:** ist in diesem Profil möglich. Es gibt nur einen Mandanten, und den Betreiber stellt der IdP-Admin.
- **Doku:** Einrichtungsbeispiele für Keycloak, Authentik und Entra ID (Audience-Mapper, Rollen-Claim, JWT-Access-Tokens). Sie werden als Beispiele gekennzeichnet, solange sie nicht gegen eine echte Instanz geprüft sind.

### Profil `zitadel`

- **Audience:** `OIDC_AUDIENCE` ist die Projekt-ID.
- **Rollen:**
  - Gezählt werden nur die Schlüssel aus `urn:zitadel:iam:org:project:{projectId}:roles`, **deren Organisations-Map die Organisation des Nutzers enthält**.
  - Die Organisation ist `urn:zitadel:iam:user:resourceowner:id`.
  - Unbekannte Rollennamen werden ignoriert.
- **`OIDC_TENANCY=single` (Standard):**
  - Mandant ist `default`.
  - Die Organisation des Nutzers muss gleich `ZITADEL_PLATFORM_ORG_ID` sein. Sonst folgt `403`, damit Nutzer fremder Organisationen nicht in den einzigen Mandanten gelangen.
- **`OIDC_TENANCY=multi`:**
  - Gesucht wird der Mandant mit `zitadel_org_id` = Organisation. Gibt es keinen, folgt `403` „organization is not provisioned“. Die automatische Anlage kommt mit AP-4.2 (FA-TEN-03).
  - `platform-admin` wird nur anerkannt, wenn die Organisation gleich `ZITADEL_PLATFORM_ORG_ID` ist. Damit kann eine berechtigte Kundenorganisation keinen Plattform-Admin erzeugen, auch wenn ein Grant falsch konfiguriert ist.
- **Neuer FSM-Befehl (nur für `multi`):** `BindTenantOrg{tenant_id, org_id}` setzt `tenants.zitadel_org_id`. Er bekommt Befehlsversion 7 und wird per Feature-Gate erst angewandt, wenn alle Voter Version 7 können (ADR 0018).
- **Regeln für `BindTenantOrg`:**
  - Abgelehnt werden: `_system`, eine leere Org-ID und eine Org-ID, die schon einem anderen Mandanten gehört.
  - Ein Mandant, der schon gebunden ist, wird nur über die Admin-API aus AP-4.2 neu gebunden.
- **Start:** Ist `default` noch ungebunden, bindet der Leader ihn beim Start an `ZITADEL_PLATFORM_ORG_ID`. Damit kann sich der erste Plattform-Admin anmelden.
- **Wechsel von `single` zu `multi`:** Die Daten in `default` bleiben erhalten und gehören dann der Plattform-Organisation.
- **Sperre von `_system`:** `_system` hat nie eine `zitadel_org_id`. FSM und Store lehnen das ab, und `FromContext` bleibt die zweite Sperre.
- **Bestehende lokale Daten:** Die Übernahme lokaler Konten und Daten bleibt AP-4.5 (FA-AUTH-08) und gilt für beide Profile.

### Backend: JWKS-Cache

- **Form:** eigener Typ, der `oidc.KeySet` erfüllt. Die Schlüssel liegen als `jose.JSONWebKeySet` vor.
- **Schlüsselwahl:** nach `kid`. Hat der JWK ein `alg`, muss es zum `alg` im Header passen. Der Schlüsseltyp muss zum Algorithmus passen. RSA-Schlüssel unter 2048 Bit werden beim Laden verworfen. Schlüssel mit `use` ≠ `sig` werden ignoriert.
- **Discovery:** Beim Start wird das Discovery-Dokument geladen. Sein `issuer` muss exakt gleich `OIDC_ISSUER` sein. `jwks_uri` wird daraus übernommen.
- **Regelmäßiges Nachladen:** alle 5 min, oder früher, wenn `max-age` kürzer ist.
- **Unbekanntes `kid`:** löst sofort ein Nachladen aus, höchstens einmal je 30 s pro Voter. Parallele Anfragen warten auf denselben Abruf (single-flight).
- **Fehlschlag:** Schlägt ein Abruf fehl, bleiben die letzten gültigen Schlüssel unbegrenzt in Gebrauch (FA-AUTH-07). Die Metrik `uptimecluster_auth_jwks_fetch_failures_total` zählt die Fehlschläge mit. Ein Systemalert dazu folgt bei Bedarf in P5.
- **Wechsel:** Ein erfolgreich geladenes Schlüsselset ersetzt das alte vollständig. Damit verschwinden auch Schlüssel, die der IdP entfernt hat.
- **Datei:** Erfolgreich geladene Daten werden atomar nach `<DATA_DIR>/oidc-jwks.json` geschrieben (Rechte 0600). Beim Start wird zuerst diese Datei gelesen.
- **Ohne Schlüssel:** Hat ein Voter weder Datei noch Verbindung, lehnt er jede Anfrage mit `503` ab und meldet `/health/ready` als nicht bereit. Checks und Alerts laufen weiter.
- **Verbindung zum IdP:**
  - Der HTTP-Client hat Timeouts und eine Größenbegrenzung (JWKS ≤ 64 KiB).
  - Der IdP wird nur über HTTPS angesprochen. Ausnahme ist `OIDC_INSECURE_HTTP=true` für die Entwicklung, dann gibt es eine Log-Warnung.
  - Weiterleitungen werden nicht befolgt.

### API und Vertrag

- **`Me.id`:** wird im OpenAPI-Vertrag von `format: uuid` auf `string` gelockert. Für Clients ist das rückwärtskompatibel.
- **`Me.authMode`:** bekommt den Wert `oidc`.
- **WebSocket:** Das Ticket bekommt als Sitzungsende das `exp` des Access-Tokens. `revalidate` prüft im Modus `oidc` den Mandanten, ob er noch existiert und nicht gesperrt ist. Beim Ablauf schließt der Server die Verbindung, und die SPA verbindet sich mit neuem Token und Ticket wieder.
- **`/config.json`:** liefert im Modus `oidc` die Felder `issuer`, `clientId` und `scopes`.
  - Die Scopes kommen aus `OIDC_SCOPES`.
  - Standard im Profil `generic`: `openid profile offline_access`.
  - Standard im Profil `zitadel`: zusätzlich `urn:zitadel:iam:user:resourceowner`, `urn:zitadel:iam:org:project:id:{projectId}:aud` und `urn:zitadel:iam:org:projects:roles`.
- **CSP (NFA-SEC-05):** Im Modus `oidc` wird `connect-src` um den Origin des Issuers erweitert. Für alles andere bleibt die CSP unverändert.

### SPA

- **Bibliothek:** `angular-auth-oidc-client` (MIT), gekapselt in einen `OidcAuthStrategy` hinter dem bestehenden `AuthService` (ADR 0008). Die Strategy ist für alle Profile gleich. Die Konfiguration kommt zur Laufzeit aus `/config.json` (Loader der Bibliothek), nie fest im Build.
- **Speicherorte:**
  - Access-, ID- und Refresh-Token liegen nur im Speicher.
  - Nur der kurzlebige Zustand des Redirects (`state`, `nonce`, `code_verifier`) liegt in `sessionStorage` und wird nach dem Callback gelöscht.
  - Umgesetzt wird das über einen eigenen Speicher-Provider der Bibliothek (`AbstractSecurityStorage`). Er legt Token-Einträge im RAM ab und nur die Einträge des Redirect-Zustands in `sessionStorage`.
- **Auflage:** Der erste Umsetzungsschritt prüft an der aktuellen Version, dass sich Tokens und Redirect-Zustand so trennen lassen. Geht das nicht, ohne NFA-SEC-04 zu verletzen, wird dieser ADR vor der weiteren Umsetzung wieder geöffnet. Ausweichen würde man dann auf 2A.
- **Verlängerung:** über Refresh-Tokens (`useRefreshToken: true`, `silentRenew: true`), ohne iframe.
- **Nach einem Reload:** Die Tokens sind dann weg. Die SPA leitet auf oberster Ebene mit `prompt=none` zum IdP um und kommt mit der IdP-Sitzung ohne Eingabe zurück. Es gibt kein iframe, und es sind keine Third-Party-Cookies nötig. Liefert der IdP `login_required`, startet ein normaler Login.
- **Logout:** über `end_session_endpoint` mit `id_token_hint`, falls der IdP ihn anbietet. Sonst wird nur lokal abgemeldet, und die SPA weist darauf hin.

### Konfiguration

| Variable | Bedeutung |
| ---------- | ----------- |
| `AUTH_MODE` | `local` (Standard) oder `oidc`; `zitadel` ist ein Alias |
| `OIDC_PROFILE` | `generic` (Standard) oder `zitadel` |
| `OIDC_ISSUER` | Pflicht |
| `OIDC_AUDIENCE` | Pflicht; bei Zitadel die Projekt-ID |
| `OIDC_CLIENT_IDS` | Pflicht, kommagetrennt; die erste ID ist die SPA |
| `OIDC_REQUIRE_AZP` | Standard `true` |
| `OIDC_ALGORITHMS` | Standard `RS256` |
| `OIDC_MAX_TOKEN_TTL` | Standard `1h`, höchstens `12h` |
| `OIDC_SCOPES` | Standard abhängig vom Profil |
| `OIDC_INSECURE_HTTP` | Standard `false` |
| `OIDC_ROLES_CLAIM` | Pflicht im Profil `generic` |
| `OIDC_ROLE_MAP` | optional, Profil `generic` |
| `OIDC_TENANCY` | `single` (Standard) oder `multi`, nur Profil `zitadel` |
| `ZITADEL_PLATFORM_ORG_ID` | Pflicht im Profil `zitadel` |

Ist die Konfiguration unvollständig oder widersprüchlich, startet der Voter nicht, z.B. bei `OIDC_TENANCY=multi` mit dem Profil `generic`.

### Testkonzept

- **Fake-IdP:** `internal/auth/oidctest` ist ein Fake-IdP auf Basis von `httptest`, ohne Abhängigkeit. Er bietet Discovery, JWKS und das Signieren beliebiger Claims mit RSA, EC und Ed25519. Er kann Schlüssel rotieren, sich abschalten, mit 500 antworten und Antworten verzögern. Er zählt die Abrufe, damit sich das Rate-Limit prüfen lässt.
- **Tabellentests für die gemeinsamen Prüfregeln:**
  - opakes Token
  - `alg=none`, HS256 mit dem öffentlichen Schlüssel als Secret, `alg`-Mismatch zum JWK
  - falscher Issuer, z.B. mit angehängtem Slash
  - fremde Audience, fremder `azp`, fehlender `azp`
  - abgelaufenes Token, zu lange Laufzeit
- **Tabellentests für das Profil `generic`:**
  - Rollen-Claim als Array, als einzelner String und als Punktpfad
  - Rollen-Mapping, nur unbekannte Werte, Claim fehlt
- **Tabellentests für das Profil `zitadel`:**
  - Rolle in fremder Organisation
  - `single` mit fremder Organisation
  - `multi`: unbekannte Organisation, gesperrter Mandant, `platform-admin` aus einer Kundenorganisation, `_system`
- **Fuzzing:** ein Fuzz-Test für den Parser.
- **Ausfall (FA-AUTH-07):** IdP abschalten. Gecachte Tokens werden weiter akzeptiert, das Nachladen bleibt gedrosselt. Ein Neustart mit Cache-Datei funktioniert.
- **FSM:** Tests für `BindTenantOrg`, einschließlich Feature-Gate und Sperre von `_system`.
- **Konfiguration:** Tabellentests für Pflichtfelder, Widersprüche und den Alias `AUTH_MODE=zitadel`.
- **SPA:**
  - Unit-Tests der Strategy mit gemocktem `OidcSecurityService`.
  - Tests des Speicher-Providers: Token-Einträge landen nie in `localStorage` oder `sessionStorage`, und der Redirect-Zustand wird nach dem Callback gelöscht.
- **Konfiguration der Bibliothek:** Ein Test stellt sicher, dass am `oidc.Config` keiner der Schalter `Skip*` oder `InsecureSkipSignatureCheck` gesetzt ist.
- **Gegen echte IdPs:**
  - `deploy/oidc-dev/` bietet Compose-Beispiele mit Zitadel und Keycloak für manuelle End-to-End-Läufe, nicht in CI.
  - Je IdP dient ein dabei aufgezeichnetes, echtes Token (ohne Signaturschlüssel) als Fixture für die Claim-Formen.
  - Zitadel ist Pflicht für die Abnahme von AS-06. Keycloak wird nach Möglichkeit geprüft.

## Konsequenzen

- **Positiv:**
  - Jeder OIDC-Anbieter mit JWT-Access-Tokens ist nutzbar, nicht nur Zitadel.
  - Die Prüfung von Signatur und Standard-Claims übernimmt eine viel genutzte Bibliothek. Eigener Sicherheitscode bleibt nur für Schlüsselspeicher, Zusatz-Claims und Mapping.
  - Die SPA nutzt eine Angular-Bibliothek, die mit jedem OIDC-Anbieter funktioniert.
  - Die Autorisierung prüft immer Mandant und Rolle zusammen (PF-31).
  - Ein Ausfall des IdP stört bestehende Sitzungen und Neustarts mit Cache-Datei nicht.
  - Es sind keine Third-Party-Cookies nötig.
- **Negativ / Risiken:**
  - Mehr Konfiguration und mehr Testfälle, etwa 20–30 % mehr Aufwand für AP-4.1.
  - Mehrere Mandanten gibt es nur mit Zitadel.
  - Fehler im Rollen-Mapping des Profils `generic` können Rechte verschieben. Das wird abgefedert: Unbekannte Werte zählen nicht, und `OIDC_AUDIENCE` ist Pflicht.
  - Neue Go-Abhängigkeiten: `github.com/coreos/go-oidc/v3`, `github.com/go-jose/go-jose/v4` und `golang.org/x/oauth2` (alle Apache-2.0 bzw. BSD-3-Clause, passend zur Allow-List aus ADR 0010; `make licenses` bestätigt das bei der Umsetzung). Sicherheitsmeldungen zu go-jose sind wahrscheinlich und müssen zügig nachgezogen werden.
  - Der eigene `KeySet` und die Zusatzprüfungen bleiben sicherheitskritisch. Das wird durch strenge Negativtests und Fuzzing abgefedert.
  - Für den Token-Speicher in der SPA gibt es eine Auflage (siehe Abschnitt SPA). Schlägt sie fehl, muss der ADR erneut entschieden werden.
  - Rollenentzug wirkt erst nach Ablauf des Tokens, höchstens nach `OIDC_MAX_TOKEN_TTL`.
  - Ein Reload kostet einen Redirect zum IdP. Ist der IdP dabei nicht erreichbar, ist nach dem Reload keine Anmeldung möglich, eine bestehende Sitzung ohne Reload dagegen schon.
  - Neue npm-Abhängigkeit `angular-auth-oidc-client` (MIT, Abhängigkeiten `tslib` und `rfc4648`, passt zur Allow-List aus ADR 0010).
- **Folgearbeiten:**
  - **Pflichtenheft:** FA-AUTH-05..08 sprechen von „Zitadel“ und werden auf „OIDC (Profil `zitadel` für mehrere Mandanten)“ verallgemeinert. FA-TEN-03 (Auto-Provisionierung) gilt nur für das Profil `zitadel` mit `multi`. Die Änderung wird im selben Commit gemacht, in dem der ADR angenommen wird.
  - AP-4.2: automatische Anlage beim ersten Login und Admin-API zur Org-Bindung.
  - AP-4.5: Migration lokaler Konten.
  - Doku „IdP einrichten“:
    - Zitadel: Token-Typ JWT, Access-Token-Laufzeit 15 min, „Assert Roles on Authentication“, Projekt-Grants, Redirect-URIs.
    - Beispiele für Keycloak, Authentik und Entra ID.
  - STRIDE-Ergänzung unter `docs/security/` (NFA-SEC-08).

## Approval

| Datum | Status | Durch | Anmerkung |
|-------|--------|-------|-----------|
| 2026-11-17 | Proposed | Claude | erster Entwurf nur für Zitadel |
| 2026-11-17 | Proposed | Claude | überarbeitet: generisches OIDC mit Profilen, ein Mandant als Standard, mehrere Mandanten über Zitadel (Richtung im Chat vorgegeben) |
| 2026-11-17 | Proposed | Claude | nach Review von Projektinhaber: 0B, 2B, 3A, 4A festgelegt; Frage 1 auf 1C (go-oidc mit eigenem `KeySet`), weil das weniger sicherheitskritischen Eigenbau braucht als 1B |
| 2026-11-17 | Accepted | Projektinhaber | im Chat angenommen („Annehmen, umsetzen“), eingetragen durch Claude |
