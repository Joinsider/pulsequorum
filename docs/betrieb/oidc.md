# Anmeldung über einen OIDC-Anbieter

Dieses Dokument beschreibt, wie der Cluster die Anmeldung an einen externen OpenID-Connect-Anbieter (IdP) delegiert: PocketID, Keycloak, Authentik, Entra ID, Zitadel oder ein anderer Anbieter mit JWT-Access-Tokens. Grundlage sind ADR 0027 (FA-AUTH-05 bis FA-AUTH-07, FA-TEN-02/03, NFA-SEC-04/05) und ADR 0028 (Rollen aus `/userinfo`, `client_id`). Die Variablen entsprechen `internal/config/config.go`.

Die Grundkonfiguration eines Einzelknotens steht in [compose.md](compose.md). Der Notzugang bei Ausfall des IdP ist in [recovery.md](recovery.md) beschrieben.

## 1. Überblick

Die Anmeldung wird über `AUTH_MODE` gewählt. Im Modus `oidc` wählt `OIDC_PROFILE` das Claim-Mapping, und bei Zitadel `OIDC_TENANCY` die Zahl der Mandanten:

| `AUTH_MODE` | `OIDC_PROFILE` | `OIDC_TENANCY` | Mandanten | Rollenquelle |
| --- | --- | --- | --- | --- |
| `local` (Standard) | – | – | einer (`default`) | lokale Nutzer |
| `oidc` | `generic` (Standard) | – | einer (`default`) | konfigurierbarer Claim, z. B. `groups`, `roles`, `realm_access.roles`, aus dem Access-Token oder aus `/userinfo` (`OIDC_ROLES_SOURCE`) |
| `oidc` | `zitadel` | `single` (Standard) | einer (`default`), gebunden an die Plattform-Organisation | Zitadel-Projekt-Rollen |
| `oidc` | `zitadel` | `multi` | einer je Organisation | Zitadel-Projekt-Rollen |

- `AUTH_MODE=zitadel` ist ein Alias für `AUTH_MODE=oidc` mit `OIDC_PROFILE=zitadel`. Der Start protokolliert dazu einen Hinweis.
- Ein Cluster vertraut genau einem Issuer.
- Mehrere Mandanten gibt es nur mit dem Profil `zitadel` und `OIDC_TENANCY=multi`.

**Was lokal bleibt:** Im Modus `oidc` gibt es keine lokale Anmeldung mehr. Anmeldung mit Passwort, Passwortänderung, Token-Erneuerung über den lokalen Pfad und die Nutzerverwaltung antworten mit `409 Conflict` und dem Hinweis „users are managed by the identity provider“.

**Notzugang:** Ist der IdP dauerhaft nicht erreichbar, bleibt nur die Recovery-CLI (ADR 0019). Dafür wird `AUTH_MODE` vorübergehend auf `local` gesetzt. `reset-password` setzt `AUTH_MODE=local` voraus. Das Vorgehen steht in [recovery.md](recovery.md).

## 2. Voraussetzungen beim IdP

Diese Punkte gelten für jeden Anbieter:

- **JWT-Access-Tokens.** Der Cluster prüft Access-Tokens lokal mit den Signaturschlüsseln des IdP. Opake Tokens werden mit `401` und der Meldung `opaque access tokens are not supported; configure the identity provider to issue JWT access tokens` abgelehnt.
- **Authorization Code Flow mit PKCE** für die SPA als öffentlichen Client (ohne Client-Secret). Die Methode `S256` ist zu verwenden.
- **Redirect-URI:** `https://<host>/auth/callback`.
- **Post-Logout-URI:** `https://<host>/login`.
- **Refresh-Tokens** über den Scope `offline_access`. Die SPA erneuert Tokens damit ohne iframe.
- **Audience:** Das Access-Token enthält im Claim `aud` den Wert aus `OIDC_AUDIENCE`. Tokens für andere Anwendungen werden abgelehnt.
- **Client-Kennung (`azp` oder `client_id`):** Der Cluster prüft die Client-Kennung des Tokens. Er liest den Claim `azp`, und wenn der fehlt, den Claim `client_id` (RFC 9068). Beide zählen gleich. Der Wert muss in `OIDC_CLIENT_IDS` stehen. Sind beide Claims vorhanden, müssen sie gleich sein; sonst wird das Token abgelehnt. Fehlen beide, wird das Token nur mit `OIDC_REQUIRE_AZP=false` angenommen. PocketID und Zitadel setzen nur `client_id` und brauchen deshalb keine Ausnahme.
- **ID-Tokens** werden nicht als Access-Token akzeptiert.
- **Laufzeit des Access-Tokens:** empfohlen werden **15 min**. Ein Token, dessen Laufzeit (`exp` minus `iat`) über `OIDC_MAX_TOKEN_TTL` liegt (Standard 1 h), wird abgelehnt. Wird ein Rollenentzug beim IdP wirksam, erst wenn das Token abläuft.
- **CORS:** Der Token-Endpunkt des IdP muss den Ursprung der SPA (`https://<host>`) erlauben.
- **Discovery:** Der Wert `issuer` im Dokument `/.well-known/openid-configuration` muss exakt `OIDC_ISSUER` entsprechen. Die Signaturschlüssel werden über die dort genannte `jwks_uri` geladen.
- **Signaturalgorithmus:** Standard ist `RS256`. Über `OIDC_ALGORITHMS` sind auch `ES256` und `EdDSA` möglich. Der Algorithmus muss zu dem Schlüssel im JWKS passen.

## 3. Variablen

Alle Variablen werden beim Start gelesen. Ungültige oder widersprüchliche Werte beenden den Start, und die Meldung nennt alle Fehler auf einmal. Boolesche Werte sind nur `true` oder `false`.

| Variable | Standard | Bedeutung |
| --- | --- | --- |
| `AUTH_MODE` | `local` | `local` oder `oidc`. `zitadel` ist ein Alias für `oidc` mit `OIDC_PROFILE=zitadel`. |
| `OIDC_PROFILE` | `generic` | `generic` oder `zitadel`. Mit `AUTH_MODE=zitadel` ist nur `zitadel` erlaubt. |
| `OIDC_ISSUER` | – (Pflicht) | Issuer-URL. Muss **exakt** dem Claim `iss` entsprechen, einschließlich eines abschließenden `/`. Nur `https`; `http` nur mit `OIDC_INSECURE_HTTP=true`. Ohne Query und Fragment. |
| `OIDC_AUDIENCE` | – (Pflicht) | Wert, der im Claim `aud` enthalten sein muss. Bei Zitadel die Projekt-ID. |
| `OIDC_CLIENT_IDS` | – (Pflicht) | Kommagetrennte Liste der erlaubten Client-Kennungen (`azp` bzw. `client_id`). Der erste Eintrag ist die Client-ID der SPA. Leere und doppelte Einträge sind Fehler. |
| `OIDC_REQUIRE_AZP` | `true` | `true`: Das Token braucht den Claim `azp` oder `client_id`. `false`: Tokens ohne beide Claims werden angenommen. Ist einer der Claims vorhanden, muss sein Wert trotzdem in `OIDC_CLIENT_IDS` stehen. |
| `OIDC_ALGORITHMS` | `RS256` | Erlaubte Signaturalgorithmen, kommagetrennt aus `RS256`, `ES256` und `EdDSA`. |
| `OIDC_MAX_TOKEN_TTL` | `1h` | Höchstlaufzeit eines Access-Tokens (`exp` minus `iat`). Größer als `0`, höchstens `12h`. |
| `OIDC_SCOPES` | je Profil, siehe unten | Angeforderte Scopes, durch Komma oder Leerzeichen getrennt. Muss `openid` enthalten. |
| `OIDC_INSECURE_HTTP` | `false` | Erlaubt einen `http`-Issuer. Nur für die Entwicklung; der Start protokolliert eine Warnung. |
| `OIDC_ROLES_CLAIM` | – (Pflicht bei `generic`) | Claim mit den Rollen als Punktpfad, z. B. `realm_access.roles`. Der Wert ist ein Array aus Strings oder ein einzelner String. Bei `OIDC_ROLES_SOURCE=access_token` wird er im Access-Token gelesen, bei `userinfo` in der Antwort von `/userinfo`. Bei `zitadel` nicht erlaubt. |
| `OIDC_ROLE_MAP` | leer | Ordnet Werte des IdP den Rollen zu, Format `idpWert=rolle,idpWert2=rolle2`. Nur bei `generic`. Doppelte IdP-Werte sind ein Fehler. |
| `OIDC_ROLES_SOURCE` | `access_token` | `access_token`: Rollen-Claim aus dem Access-Token (bisheriges Verhalten). `userinfo`: Rollen-Claim aus `/userinfo`, siehe Abschnitt 4. Nur bei `generic`. |
| `OIDC_DEFAULT_ROLE` | leer | Rolle für ein gültiges Token, wenn die gewählte Rollenquelle keine gültige Rolle liefert. Leer, `tenant-admin`, `tenant-editor` oder `tenant-viewer`. `platform-admin` ist nicht erlaubt. Nur bei `generic`, siehe Abschnitt 4. |
| `OIDC_TENANCY` | `single` | `single` oder `multi`. Nur bei `zitadel`; bei `generic` nicht erlaubt. |
| `ZITADEL_PLATFORM_ORG_ID` | – (Pflicht bei `zitadel`) | ID der Zitadel-Organisation der Plattform. Nur bei `zitadel`. |

**Standard von `OIDC_SCOPES`:**

- Profil `generic`: `openid profile offline_access`
- Profil `zitadel`: `openid profile offline_access urn:zitadel:iam:user:resourceowner urn:zitadel:iam:org:project:id:<OIDC_AUDIENCE>:aud urn:zitadel:iam:org:projects:roles`

Variablen des anderen Profils sind ein Startfehler, zum Beispiel `OIDC_ROLES_CLAIM` mit `zitadel` oder `OIDC_TENANCY` mit `generic`.

**Beispiel für das Profil `generic`:**

```bash
AUTH_MODE=oidc
OIDC_PROFILE=generic
OIDC_ISSUER=https://idp.example.com/realms/uptime
OIDC_AUDIENCE=uptimecluster-api
OIDC_CLIENT_IDS=uptimecluster-spa
OIDC_ROLES_CLAIM=roles
```

## 4. Rollen

Es gibt vier Rollen. Sie gelten im Cluster unabhängig vom Profil:

| Rolle | Bedeutung |
| --- | --- |
| `platform-admin` | Plattform-Administrator, z. B. für Join-Tokens und das Entfernen von Nodes |
| `tenant-admin` | Administrator im eigenen Mandanten |
| `tenant-editor` | Bearbeitung im eigenen Mandanten |
| `tenant-viewer` | Nur Lesezugriff im eigenen Mandanten |

Ein Token ohne gültige Rolle wird mit `403` abgelehnt (`the token grants no role for this application`). Ausnahme ist `OIDC_DEFAULT_ROLE` im Profil `generic`, siehe unten.

### Profil `generic`

Der Claim `OIDC_ROLES_CLAIM` wird gelesen, und `OIDC_ROLE_MAP` ordnet seine Werte den Rollen zu. Ohne `OIDC_ROLE_MAP` zählen nur Werte, die genau einem der vier Rollennamen entsprechen. Unbekannte Werte werden ignoriert. Der Mandant ist immer `default`. Mandanten-Claims im Token werden ignoriert.

Beispiel:

```bash
OIDC_ROLE_MAP=uptime-admins=tenant-admin,ops=tenant-editor,staff=tenant-viewer,platform=platform-admin
```

`platform-admin` ist in diesem Profil möglich, weil es nur einen Mandanten gibt und der IdP-Administrator die Rolle vergibt. Fehler in der Zuordnung können Rechte verschieben. Deshalb die Zuordnung klein halten und mit einem Testnutzer prüfen.

#### Rollen aus `/userinfo` (`OIDC_ROLES_SOURCE=userinfo`)

Manche Anbieter schreiben Gruppen oder Rollen nicht ins Access-Token, sondern nur in die Antwort des `userinfo_endpoint` (PocketID, siehe Abschnitt 6). Mit `OIDC_ROLES_SOURCE=userinfo` liest der Voter den Rollen-Claim aus dieser Antwort:

- **Ablauf:** Das Access-Token wird zuerst vollständig geprüft. Erst danach ruft der Voter mit diesem Token den `userinfo_endpoint` aus der Discovery auf. Ungültige Tokens erzeugen also keinen Aufruf beim IdP.
- **Aufrufe:** Pro neuem Access-Token und Voter gibt es höchstens einen Aufruf. Das Ergebnis wird bis zum Ablauf des Tokens (`exp`) zwischengespeichert. Gleichzeitige Anfragen mit demselben Token teilen sich einen Aufruf.
- **Cache:** Der Cache liegt im Speicher und hält höchstens 10 000 Einträge. Nach einem Neustart des Voters ist er leer.
- **Prüfung der Antwort:** Die Antwort muss JSON sein und ein `sub` haben, das dem `sub` des Access-Tokens entspricht. Sonst wird das Token abgelehnt.
- **Schutz des Aufrufs:** Wie bei den JWKS gilt nur HTTPS (außer bei `OIDC_INSECURE_HTTP`), keine Weiterleitungen, Antworten höchstens 64 KiB, Zeitlimit 5 s.
- **Fehlerabbildung:** Lehnt der IdP das Token ab (`401` oder `403` von `/userinfo`), antwortet der Cluster mit `401`. Ist der IdP nicht erreichbar, antwortet er mit `503`.
- **Ausfall des IdP:** Tokens, die schon im Cache sind, funktionieren weiter. Neue Tokens schlagen während des Ausfalls fehl (`503`). Ein Token, das kurz vor dem Ausfall ausgestellt wurde, ist die Ausnahme, weil es noch nicht im Cache liegt.
- **Metriken:** `uptimecluster_auth_userinfo_requests_total` zählt die Aufrufe bei `/userinfo`, `uptimecluster_auth_userinfo_failures_total` die fehlgeschlagenen. Ein Anstieg der Fehler ist ein Hinweis auf einen IdP-Ausfall oder eine falsche `userinfo_endpoint`-Adresse.

Die Rollen werden mit `OIDC_ROLE_MAP` zugeordnet wie bisher.

#### Feste Rolle (`OIDC_DEFAULT_ROLE`)

Ist die gewählte Rollenquelle leer oder liefert sie keine gültige Rolle, bekommt jedes gültige Token dieses Clients die Rolle aus `OIDC_DEFAULT_ROLE`. Das gilt auch, wenn der Rollen-Claim vorhanden ist, aber keiner seiner Werte zugeordnet werden kann. Ohne diese Variable bleibt es bei `403`.

> **Achtung: Die Zugangskontrolle liegt dann vollständig beim IdP.** Jeder Nutzer, den der IdP für diesen Client anmeldet, bekommt die Standardrolle, und der Cluster prüft keine Gruppe mehr. Der Client muss beim IdP auf die erlaubten Gruppen beschränkt sein (bei PocketID „Allowed user groups“). Ein Fehler in dieser Beschränkung öffnet den Cluster mit der Standardrolle. Die Standardrolle deshalb nur setzen, wenn der IdP den Zugang tatsächlich steuert.

Die Standardrolle ist nie `platform-admin`. Mehrere Rollen über einen Client lassen sich damit nicht abbilden.

### Profil `zitadel`

- Gezählt werden nur die Projekt-Rollen, die in der **Organisation des Nutzers** vergeben sind. Eine Rolle aus einer fremden Organisation hat keine Wirkung.
- Die Rollen müssen die Schlüssel `platform-admin`, `tenant-admin`, `tenant-editor` und `tenant-viewer` haben. Andere Schlüssel werden ignoriert.
- `platform-admin` wird nur anerkannt, wenn die Organisation des Nutzers gleich `ZITADEL_PLATFORM_ORG_ID` ist. Das gilt in beiden Tenancy-Modi.

## 5. Zitadel einrichten

Schritte in der Zitadel-Konsole:

1. **Projekt anlegen.** Die Projekt-ID notieren. Sie ist `OIDC_AUDIENCE`.
2. **Rollen im Projekt anlegen** mit den Schlüsseln `platform-admin`, `tenant-admin`, `tenant-editor` und `tenant-viewer`. Die Schlüssel müssen genau so geschrieben werden.
3. **Im Projekt „Assert Roles on Authentication“ aktivieren.** Ohne diese Einstellung enthält das Token keine Rollen.
4. **Anwendung vom Typ „User Agent“ anlegen** (SPA mit PKCE). Redirect-URI `https://<host>/auth/callback`, Post-Logout-URI `https://<host>/login`. Die Client-ID notieren. Sie ist der erste Eintrag in `OIDC_CLIENT_IDS`.
5. **Token-Typ der Anwendung auf JWT setzen** (Token Settings → Token Type). Der Standard ist opak, und opake Tokens werden abgelehnt.
6. **Token-Laufzeit prüfen.** Sie wird instanzweit in den Standardeinstellungen der Instanz festgelegt. Zitadel liefert standardmäßig 12 h. Für den Cluster die **Access-Token-Laufzeit auf 15 min** setzen. Längere Laufzeiten werden nach dem Standard von `OIDC_MAX_TOKEN_TTL` (1 h) abgelehnt.
7. **Plattform-Organisation bestimmen.** Ihre ID steht in `ZITADEL_PLATFORM_ORG_ID`. Plattform-Administratoren gehören zu dieser Organisation und erhalten die Rolle `platform-admin`.
8. **Rollen an Nutzer vergeben.** Die Rollen gelten nur in der Organisation, in der sie vergeben wurden.

Beispiel für die Umgebung eines Einzelmandanten-Clusters:

```bash
AUTH_MODE=oidc
OIDC_PROFILE=zitadel
OIDC_ISSUER=<issuer aus der Discovery-Datei der Instanz>
OIDC_AUDIENCE=<Projekt-ID>
OIDC_CLIENT_IDS=<Client-ID der SPA>
OIDC_TENANCY=single
ZITADEL_PLATFORM_ORG_ID=<ID der Plattform-Organisation>
```

### Mehrere Mandanten (`OIDC_TENANCY=multi`)

- **Projekt-Grants an Kundenorganisationen.** Der Eigentümer des Projekts gibt das Projekt an jede Kundenorganisation frei und wählt die Rollen aus, die sie vergeben darf. Laut Zitadel-Dokumentation können Administratoren einer Kundenorganisation nur diese freigegebenen Rollen vergeben. Dieses Verhalten ist nicht selbst geprüft.
- **Standard-Mandant.** Ist der Mandant `default` noch nicht an eine Organisation gebunden, bindet ihn der Leader beim Start an `ZITADEL_PLATFORM_ORG_ID`. So kann sich der erste Plattform-Administrator anmelden. Ein Wechsel von `single` zu `multi` lässt die Daten in `default` unverändert. Sie gehören dann der Plattform-Organisation.
- **Neue Organisationen.** Eine Organisation ohne Mandanten wird mit `403` (`organization is not provisioned`) abgelehnt. Die automatische Anlage beim ersten Login und die Admin-API zur Bindung einer Organisation an einen Mandanten kommen mit AP-4.2.

### Befunde aus dem Test mit Zitadel v4.20.3

Gemessen mit Zitadel v4.20.3 und Login V2 (Umgebung unter `deploy/oidc-dev/zitadel`):

- **Client-Kennung:** Die Tokens tragen `client_id`, kein `azp`. Das ist seit ADR 0028 gültig, weil `client_id` wie `azp` geprüft wird.
- **Login V2 ist Pflicht** in Zitadel v4 und läuft als eigener Container. Die Anmeldeseite wird nicht von Zitadel selbst ausgeliefert.
- **Reload ohne Zitadel-Sitzung:** Mit Login V2 zeigt `prompt=none` ohne bestehende Sitzung das Anmeldeformular von Zitadel an, statt `login_required` zu liefern. Nach einem Neuladen der SPA landen Nutzer ohne Zitadel-Sitzung deshalb direkt auf der Anmeldeseite von Zitadel. Das hat keine Auswirkung auf die Sicherheit.
- **Maschinenbenutzer:** Zitadel verwendet als Client-ID den Benutzernamen, nicht die Benutzer-ID. Der Token-Typ der Anwendung muss JWT sein.

## 6. PocketID

> **Wird gerade gegen eine echte Instanz geprüft.**

PocketID ist ein schlanker, selbst hostbarer Anbieter mit Passkeys. Die Angaben beruhen auf dem Quelltext von PocketID v2.19.3 (Fork `pocket-id/fosite` v1.4.3):

- **Access-Token:** Ein JWT nach RFC 9068 (`typ: at+jwt`), standardmäßig mit RS256. Andere Algorithmen sind nur über die CLI `pocket-id key-rotate --alg` möglich.
- **Claims im Access-Token:** `iss`, `sub`, `aud` (Client-ID und `APP_URL`), `client_id`, `exp`, `iat`, `jti`, `scope`. Kein `azp`.
- **Keine Gruppen im Access-Token.** Der Claim `groups` steht nur im ID-Token und unter `/userinfo`. Er ist ein Array der Gruppen-**Namen** und erscheint nur, wenn der Scope `groups` angefordert wird.
- **Folgerung:** Mit dem Profil `generic` ist `OIDC_ROLES_SOURCE=userinfo` nötig, damit die Rollen aus `groups` kommen.

### Umgebung

```bash
AUTH_MODE=oidc
OIDC_PROFILE=generic
OIDC_ISSUER=https://id.example.com
OIDC_AUDIENCE=<Client-ID>
OIDC_CLIENT_IDS=<Client-ID>
OIDC_ROLES_SOURCE=userinfo
OIDC_ROLES_CLAIM=groups
OIDC_SCOPES="openid profile groups offline_access"
OIDC_ROLE_MAP=uptimecluster-admins=tenant-admin,uptimecluster-viewers=tenant-viewer
```

- `OIDC_ISSUER` ist die `APP_URL` von PocketID ohne abschließendes `/`. Der Wert muss exakt dem `issuer` der Discovery entsprechen.
- `OIDC_AUDIENCE` und `OIDC_CLIENT_IDS` sind die Client-ID. Der Claim `aud` enthält sie, und `client_id` ist der Claim, den der Cluster prüft.
- `OIDC_SCOPES`: Der Scope `groups` ist für den Rollen-Claim nötig. `offline_access` ist laut Quelltext für Refresh-Tokens nicht zwingend, wenn der Client den Refresh-Grant erlaubt. Er schadet nicht und entspricht dem Standard, daher bleibt er in der Liste.
- `OIDC_ROLE_MAP` ist ein Beispiel. Gruppennamen und Rollen wählt man selbst. Die Gruppen-Namen müssen genau so in PocketID stehen.

### Einrichtung in PocketID

1. **Gruppen anlegen**, zum Beispiel `uptimecluster-admins` und `uptimecluster-viewers`, und die Nutzer zuweisen.
2. **OIDC-Client anlegen:**
   - **Public Client** aktivieren. PKCE wird automatisch verlangt. PocketID akzeptiert zusätzlich `plain`, die SPA sendet aber immer `S256`.
   - **Callback-URL:** `https://<host>/auth/callback`
   - **Logout-Callback-URL:** `https://<host>/login`
   - **Access-Token-Laufzeit:** 15 min. Der Standard von PocketID ist 60 min, und der Cluster lehnt Tokens über `OIDC_MAX_TOKEN_TTL` (1 h) ab, deshalb ist der Wert anzupassen.
   - **Allowed user groups:** auf die Gruppen beschränken, die Zugang haben sollen. Nur Mitglieder dieser Gruppen bekommen ein Token. Das ist die Zugangskontrolle, wenn `OIDC_DEFAULT_ROLE` gesetzt ist (siehe Abschnitt 4).
   - Die Client-ID notieren. Sie geht in `OIDC_AUDIENCE` und `OIDC_CLIENT_IDS`.
3. **`INTERNAL_APP_URL` nicht setzen** oder auf eine Adresse, die sowohl die Voter als auch der Browser erreichen. PocketID schreibt diesen Wert in die Discovery-Daten, in `jwks_uri` und `token_endpoint`. Zeigt er auf eine Adresse, die nur intern gilt, kann der Browser den Token-Endpunkt nicht erreichen, und die Voter scheitern beim Abruf der Schlüssel.
4. **Erreichbarkeit prüfen:** Die Voter müssen `jwks_uri` und `userinfo_endpoint` aus der Discovery erreichen. Der `userinfo_endpoint` wird für `OIDC_ROLES_SOURCE=userinfo` gebraucht. Der Browser braucht den `token_endpoint`.

### Weitere Hinweise zu PocketID

- **CORS:** PocketID antwortet auf Token-, Userinfo- und JWKS-Endpunkt mit `Access-Control-Allow-Origin: *`. Ein CORS-Eintrag im IdP ist deshalb nicht nötig.
- **Anmeldung:** Nutzer melden sich bei PocketID mit Passkeys an. Für Einrichtung und Wiederherstellung gibt es Einmal-Links.

## 7. Beispiele für andere Anbieter

### Keycloak

Gegen Keycloak 26.8.2 Ende zu Ende geprüft: API mit echten Tokens und Anmeldung in der SPA.

1. Realm anlegen. Der Issuer lautet `https://kc.example.com/realms/<realm>`.
2. Client `uptimecluster-spa` anlegen: Client-Typ **public**, Standard-Flow aktiviert, PKCE mit Code Challenge Method `S256`. Valid Redirect URIs `https://<host>/auth/callback`, Valid Post Logout Redirect URIs `https://<host>/login`, Web Origins `https://<host>`.
3. Audience sicherstellen: Ein Audience-Mapper im Client-Scope des SPA-Clients nimmt den API-Client `uptimecluster-api` in `aud` auf. Im Token steht `aud` dann zusätzlich `account`. Das ist unschädlich, weil der Cluster nur verlangt, dass `OIDC_AUDIENCE` enthalten ist.
4. Den Scope `offline_access` dem SPA-Client zuweisen.
5. Realm-Rollen `platform-admin`, `tenant-admin`, `tenant-editor` und `tenant-viewer` anlegen oder über eigene Namen zuordnen. Keycloak legt die Realm-Rollen im Token unter `realm_access.roles` ab.
6. Die Access-Token-Laufzeit im Realm auf 15 min setzen.

```bash
AUTH_MODE=oidc
OIDC_PROFILE=generic
OIDC_ISSUER=https://kc.example.com/realms/<realm>
OIDC_AUDIENCE=uptimecluster-api
OIDC_CLIENT_IDS=uptimecluster-spa
OIDC_ROLES_CLAIM=realm_access.roles
```

### Authentik

> **Nicht gegen eine echte Instanz geprüft.** Die Angaben stammen aus der allgemeinen Dokumentation der Anbieter. Namen von Menüs und Feldern können je nach Version abweichen.

1. Einen OAuth2/OpenID-Provider mit Client-Typ **Public** anlegen. Redirect-URIs wie oben setzen.
2. Einen **Signing Key** (RSA) auswählen, damit Access-Tokens als JWT signiert werden.
3. Die Gültigkeit des Access-Tokens auf 15 min setzen.
4. Ein Scope-Mapping, das die Gruppen als Claim `groups` ausgibt, dem Provider zuweisen.
5. Bei Authentik ist `aud` standardmäßig die Client-ID des Providers. In diesem Fall ist `OIDC_AUDIENCE` gleich der Client-ID der SPA. Das Token-`aud` vorab prüfen.
6. Die Issuer-URL von Authentik endet mit `/`. Den Wert exakt aus der Discovery-Datei übernehmen.

```bash
AUTH_MODE=oidc
OIDC_PROFILE=generic
OIDC_ISSUER=https://auth.example.com/application/o/<slug>/
OIDC_AUDIENCE=uptimecluster-spa
OIDC_CLIENT_IDS=uptimecluster-spa
OIDC_ROLES_CLAIM=groups
OIDC_ROLE_MAP=uptime-admins=tenant-admin,ops=tenant-editor,staff=tenant-viewer,platform=platform-admin
```

### Entra ID (Microsoft)

> **Nicht gegen eine echte Instanz geprüft.** Die Angaben stammen aus der allgemeinen Dokumentation der Anbieter. Namen von Menüs und Feldern können je nach Version abweichen.

1. App-Registrierung für die SPA anlegen: Plattform „Single-page application“, Redirect-URI `https://<host>/auth/callback`, Post-Logout-URI `https://<host>/login`.
2. App-Registrierung für die API anlegen. Unter „Eine API verfügbar machen“ die **Application-ID-URI** setzen und einen Scope definieren, z. B. `access_as_user`.
3. Unter „App-Rollen“ die Rollen `platform-admin`, `tenant-admin`, `tenant-editor` und `tenant-viewer` anlegen. Sie landen im Claim `roles`. Den Nutzern oder Gruppen die Rollen zuweisen.
4. Im Manifest der API-Anwendung `requestedAccessTokenVersion` auf `2` setzen. Dann hat das Token `aud` gleich der Application-ID der API und `azp` gleich der Client-ID der SPA.
5. Die Access-Token-Laufzeit prüfen. Entra liefert laut Microsoft-Dokumentation meist 60 bis 90 min. Solche Tokens überschreiten den Standard von `OIDC_MAX_TOKEN_TTL` (1 h) und werden abgelehnt. Entweder die Laufzeit über eine Token-Lebensdauer-Richtlinie verkürzen oder `OIDC_MAX_TOKEN_TTL` bewusst anheben.
6. Der Scope der API muss in `OIDC_SCOPES` stehen, sonst liefert der Endpunkt kein Access-Token für die API.
7. Manche Entra-Konfigurationen liefern weder `azp` noch `client_id`. Dann `OIDC_REQUIRE_AZP=false` setzen, und zwar nur, wenn das Token wirklich keine Client-Kennung hat.

```bash
AUTH_MODE=oidc
OIDC_PROFILE=generic
OIDC_ISSUER=https://login.microsoftonline.com/<tenant-id>/v2.0
OIDC_AUDIENCE=<Application-ID der API>
OIDC_CLIENT_IDS=<Client-ID der SPA>
OIDC_SCOPES=openid,profile,offline_access,api://<Application-ID der API>/access_as_user
OIDC_ROLES_CLAIM=roles
```

## 8. Betrieb und Ausfall

**Schlüssel-Cache.** Jeder Voter speichert die Discovery-Daten und die Signaturschlüssel in `<DATA_DIR>/oidc-jwks.json` mit den Rechten `0600`. Die Datei wird atomar geschrieben. Beim Start liest der Voter zuerst diese Datei.

**Aktualisierung:**

- Die Schlüssel werden alle 5 min neu geladen, oder früher, wenn der IdP ein kürzeres `max-age` angibt.
- Ist ein `kid` im Token unbekannt, lädt der Voter die Schlüssel nach. Das geschieht höchstens einmal je 30 s pro Voter. Parallele Anfragen warten auf denselben Abruf.
- Ein erfolgreich geladener Schlüsselsatz ersetzt den alten vollständig. Entfernte Schlüssel werden damit ungültig.

**Ausfall des IdP:**

- Die Prüfung von Access-Tokens erfolgt lokal, ohne Aufruf beim IdP. Bestehende Sitzungen laufen deshalb weiter, solange die Rollenquelle keinen Aufruf braucht.
- Mit `OIDC_ROLES_SOURCE=userinfo` braucht jedes neue Token einen Aufruf bei `/userinfo`. Während eines Ausfalls schlagen neue Tokens mit `503` fehl. Tokens, die schon im Cache sind, laufen weiter (siehe Abschnitt 4).
- Die letzten gültigen Schlüssel bleiben in Gebrauch, auch über längere Zeit.
- Ein Neustart funktioniert mit der Cache-Datei, auch wenn der IdP nicht erreichbar ist. Der Cache für `/userinfo` ist dagegen nach einem Neustart leer.
- Ein Fehlschlag beim Laden zählt die Metrik `uptimecluster_auth_jwks_fetch_failures_total`.
- Checks und Alarme laufen unabhängig vom IdP weiter.
- Ein Voter ohne Cache-Datei und ohne Verbindung zum IdP beantwortet Anfragen mit `503` und meldet `/readyz` als nicht bereit. Der Grund steht in der Antwort.
- **Reload der SPA:** Die Tokens liegen nur im Speicher und sind nach einem Neuladen weg. Die SPA leitet dann mit `prompt=none` zum IdP um. Dafür muss der IdP erreichbar sein. Liefert er `login_required`, folgt ein normaler Login. Zu Zitadel mit Login V2 siehe Abschnitt 5.

**Sicherheit der Verbindung zum IdP:**

- Der IdP wird nur über HTTPS angesprochen, außer bei `OIDC_INSECURE_HTTP=true`.
- Antworten sind auf 64 KiB begrenzt. Es gibt Zeitlimits, und Weiterleitungen werden nicht befolgt.
- Im Modus `oidc` erweitert der Voter die Content-Security-Policy `connect-src` automatisch um den Ursprung (Origin) des Issuers.

## 9. Fehlersuche

Die Begründung für `401` steht nicht in der Antwort, sondern im Log. Der Voter schreibt bei jeder abgelehnten Anfrage eine Zeile `oidc: token rejected` mit dem Feld `reason`.

| Symptom | Ursache | Vorgehen |
| --- | --- | --- |
| `401`, Meldung `opaque access tokens are not supported …` | Der IdP stellt opake Tokens aus. | Den Token-Typ auf JWT umstellen (Zitadel: Token Type der Anwendung). |
| `401` ohne Detail, `reason` im Log | Ein Prüfschritt ist fehlgeschlagen. | Das `reason` auswerten, siehe unten. |
| `401`, `reason` mit fehlender Client-Kennung (kein `azp` und kein `client_id`) | Das Token enthält keinen der beiden Claims. | Client-ID im IdP prüfen oder `OIDC_REQUIRE_AZP=false` setzen (nur wenn nötig). |
| `401`, `reason` z. B. `azp is not an allowed client` | Das Token stammt von einem anderen Client. | Client-ID der SPA in `OIDC_CLIENT_IDS` prüfen. |
| `401`, `reason` mit `lifetime … exceeds the maximum` | Das Token lebt länger als `OIDC_MAX_TOKEN_TTL`. | Access-Token-Laufzeit auf 15 min setzen oder `OIDC_MAX_TOKEN_TTL` anheben (höchstens `12h`). |
| `401`, `reason` mit `ID token presented as access token` | Die SPA sendet das ID-Token statt des Access-Tokens. | Im Client das Access-Token senden. |
| `401`, `reason` mit Issuer-, Audience- oder Signaturfehler | `OIDC_ISSUER`, `OIDC_AUDIENCE` oder die Algorithmen passen nicht. | Den `iss`- und `aud`-Claim eines Tokens mit der Konfiguration vergleichen. Auf einen abschließenden `/` achten. |
| `401` bei `OIDC_ROLES_SOURCE=userinfo` | `/userinfo` lehnt das Token ab. | Gültigkeit des Tokens im IdP prüfen, Laufzeit und Scopes (z. B. `groups`) kontrollieren. |
| `401` bei `OIDC_ROLES_SOURCE=userinfo`, die Antwort von `/userinfo` hat kein passendes `sub` oder ist kein JSON | Die Antwort gehört zu einem anderen Nutzer oder ist unbrauchbar. | IdP-Konfiguration und `userinfo_endpoint` prüfen. |
| `403`, `the token grants no role for this application` | Keine gültige Rolle im Token oder in `/userinfo`, und keine `OIDC_DEFAULT_ROLE`. | Bei `generic`: `OIDC_ROLES_SOURCE`, `OIDC_ROLES_CLAIM` und `OIDC_ROLE_MAP` prüfen, bei `userinfo` die Gruppen des Nutzers. Bei `zitadel`: „Assert Roles on Authentication“, Rollenschlüssel und Organisation des Nutzers prüfen. |
| `403`, `organization is not admitted` | `single`: Die Organisation des Nutzers ist nicht `ZITADEL_PLATFORM_ORG_ID`. | Nutzer in die Plattform-Organisation holen oder auf `multi` umstellen. |
| `403`, `organization is not provisioned` | `multi`: Der Organisation ist kein Mandant zugeordnet. | Mandant anlegen und binden (Automatik und Admin-API mit AP-4.2). |
| `403`, `the token names no organization …` | Der Scope `urn:zitadel:iam:user:resourceowner` fehlt. | Den Scope in `OIDC_SCOPES` ergänzen. |
| `403`, `tenant does not exist`, `tenant is suspended` | Der Mandant fehlt oder ist gesperrt. | Mandanten in der Verwaltung prüfen. |
| `503` | Keine Signaturschlüssel vorhanden oder ein Dienst ist vorübergehend nicht erreichbar. Bei `userinfo` auch: `/userinfo` ist nicht erreichbar. | `/readyz` abfragen, Log prüfen, Metriken `uptimecluster_auth_jwks_fetch_failures_total` und `uptimecluster_auth_userinfo_failures_total` ansehen, Verbindung vom Voter zum IdP prüfen. |
| Start schlägt mit `invalid configuration` fehl | Eine OIDC-Variable ist ungültig oder passt nicht zum Profil. | Die Fehlerliste im Log lesen und die Tabelle in Abschnitt 3 vergleichen. |
| Browser meldet CORS-Fehler am Token-Endpunkt | Der IdP erlaubt den Ursprung der SPA nicht. | Web Origins bzw. CORS-Einstellungen im IdP ergänzen. |
| `409 Conflict`, „users are managed by the identity provider“ | Im Modus `oidc` ist lokale Anmeldung abgeschaltet. | Erwartet. Anmeldung über den IdP verwenden. |

## 10. Tests gegen echte Anbieter

Für die manuelle Ende-zu-Ende-Prüfung gegen lokale Instanzen gibt es Umgebungen unter `deploy/oidc-dev/`: Keycloak 26.8.2 und Zitadel v4.20.3. Anleitung, Testnutzer, Clients und Start der Container stehen in [deploy/oidc-dev/README.md](../../deploy/oidc-dev/README.md).

## Verwandte Dokumente

- [compose.md](compose.md): Grundkonfiguration und Umgebungsvariablen
- [recovery.md](recovery.md): Notzugang und Recovery-CLI
- [cluster.md](cluster.md): Voter hinzufügen und entfernen
- ADR 0027: Entscheidung und Begründung
- ADR 0028: Rollen aus `/userinfo` und `client_id` als Ersatz für `azp`
