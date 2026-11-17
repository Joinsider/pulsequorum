# Anmeldung über einen OIDC-Anbieter

Dieses Dokument beschreibt, wie der Cluster die Anmeldung an einen externen OpenID-Connect-Anbieter (IdP) delegiert: Keycloak, Authentik, Entra ID, Zitadel oder ein anderer Anbieter mit JWT-Access-Tokens. Grundlage ist ADR 0027 (FA-AUTH-05 bis FA-AUTH-07, FA-TEN-02/03, NFA-SEC-04/05). Die Variablen entsprechen `internal/config/config.go`.

Die Grundkonfiguration eines Einzelknotens steht in [compose.md](compose.md). Der Notzugang bei Ausfall des IdP ist in [recovery.md](recovery.md) beschrieben.

## 1. Überblick

Die Anmeldung wird über `AUTH_MODE` gewählt. Im Modus `oidc` wählt `OIDC_PROFILE` das Claim-Mapping, und bei Zitadel `OIDC_TENANCY` die Zahl der Mandanten:

| `AUTH_MODE` | `OIDC_PROFILE` | `OIDC_TENANCY` | Mandanten | Rollenquelle |
| --- | --- | --- | --- | --- |
| `local` (Standard) | – | – | einer (`default`) | lokale Nutzer |
| `oidc` | `generic` (Standard) | – | einer (`default`) | konfigurierbarer Claim, z. B. `groups`, `roles`, `realm_access.roles` |
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
- **`azp`:** Der Claim `azp` ist die Client-ID der SPA und muss in `OIDC_CLIENT_IDS` stehen. Fehlt `azp`, wird das Token nur mit `OIDC_REQUIRE_AZP=false` angenommen.
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
| `OIDC_CLIENT_IDS` | – (Pflicht) | Kommagetrennte Liste der erlaubten `azp`-Werte. Der erste Eintrag ist die Client-ID der SPA. Leere und doppelte Einträge sind Fehler. |
| `OIDC_REQUIRE_AZP` | `true` | `false`: Tokens ohne `azp` werden angenommen. Ist `azp` vorhanden, muss er trotzdem in `OIDC_CLIENT_IDS` stehen. |
| `OIDC_ALGORITHMS` | `RS256` | Erlaubte Signaturalgorithmen, kommagetrennt aus `RS256`, `ES256` und `EdDSA`. |
| `OIDC_MAX_TOKEN_TTL` | `1h` | Höchstlaufzeit eines Access-Tokens (`exp` minus `iat`). Größer als `0`, höchstens `12h`. |
| `OIDC_SCOPES` | je Profil, siehe unten | Angeforderte Scopes, durch Komma oder Leerzeichen getrennt. Muss `openid` enthalten. |
| `OIDC_INSECURE_HTTP` | `false` | Erlaubt einen `http`-Issuer. Nur für die Entwicklung; der Start protokolliert eine Warnung. |
| `OIDC_ROLES_CLAIM` | – (Pflicht bei `generic`) | Claim mit den Rollen als Punktpfad, z. B. `realm_access.roles`. Der Wert ist ein Array aus Strings oder ein einzelner String. Bei `zitadel` nicht erlaubt. |
| `OIDC_ROLE_MAP` | leer | Ordnet Werte des IdP den Rollen zu, Format `idpWert=rolle,idpWert2=rolle2`. Nur bei `generic`. Doppelte IdP-Werte sind ein Fehler. |
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

Ein Token ohne gültige Rolle wird mit `403` abgelehnt (`the token grants no role for this application`).

### Profil `generic`

Der Claim `OIDC_ROLES_CLAIM` wird gelesen, und `OIDC_ROLE_MAP` ordnet seine Werte den Rollen zu. Ohne `OIDC_ROLE_MAP` zählen nur Werte, die genau einem der vier Rollennamen entsprechen. Unbekannte Werte werden ignoriert. Der Mandant ist immer `default`. Mandanten-Claims im Token werden ignoriert.

Beispiel:

```bash
OIDC_ROLE_MAP=uptime-admins=tenant-admin,ops=tenant-editor,staff=tenant-viewer,platform=platform-admin
```

`platform-admin` ist in diesem Profil möglich, weil es nur einen Mandanten gibt und der IdP-Administrator die Rolle vergibt. Fehler in der Zuordnung können Rechte verschieben. Deshalb die Zuordnung klein halten und mit einem Testnutzer prüfen.

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

## 6. Beispiele für andere Anbieter

> **Beispiel, nicht gegen eine echte Instanz geprüft.** Die Angaben stammen aus der allgemeinen Dokumentation der Anbieter. Namen von Menüs und Feldern können je nach Version abweichen.

### Keycloak

1. Realm anlegen. Der Issuer lautet `https://kc.example.com/realms/<realm>`.
2. Client `uptimecluster-spa` anlegen: Client-Typ **public**, Standard-Flow aktiviert, PKCE mit Code Challenge Method `S256`. Valid Redirect URIs `https://<host>/auth/callback`, Valid Post Logout Redirect URIs `https://<host>/login`, Web Origins `https://<host>`.
3. Audience sicherstellen: Ein Audience-Mapper im Client-Scope des SPA-Clients nimmt den API-Client `uptimecluster-api` in `aud` auf.
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

1. App-Registrierung für die SPA anlegen: Plattform „Single-page application“, Redirect-URI `https://<host>/auth/callback`, Post-Logout-URI `https://<host>/login`.
2. App-Registrierung für die API anlegen. Unter „Eine API verfügbar machen“ die **Application-ID-URI** setzen und einen Scope definieren, z. B. `access_as_user`.
3. Unter „App-Rollen“ die Rollen `platform-admin`, `tenant-admin`, `tenant-editor` und `tenant-viewer` anlegen. Sie landen im Claim `roles`. Den Nutzern oder Gruppen die Rollen zuweisen.
4. Im Manifest der API-Anwendung `requestedAccessTokenVersion` auf `2` setzen. Dann hat das Token `aud` gleich der Application-ID der API und `azp` gleich der Client-ID der SPA.
5. Die Access-Token-Laufzeit prüfen. Entra liefert laut Microsoft-Dokumentation meist 60 bis 90 min. Solche Tokens überschreiten den Standard von `OIDC_MAX_TOKEN_TTL` (1 h) und werden abgelehnt. Entweder die Laufzeit über eine Token-Lebensdauer-Richtlinie verkürzen oder `OIDC_MAX_TOKEN_TTL` bewusst anheben.
6. Der Scope der API muss in `OIDC_SCOPES` stehen, sonst liefert der Endpunkt kein Access-Token für die API.
7. Manche Entra-Konfigurationen liefern kein `azp`. Dann `OIDC_REQUIRE_AZP=false` setzen, und zwar nur, wenn das Token wirklich keinen `azp` hat.

```bash
AUTH_MODE=oidc
OIDC_PROFILE=generic
OIDC_ISSUER=https://login.microsoftonline.com/<tenant-id>/v2.0
OIDC_AUDIENCE=<Application-ID der API>
OIDC_CLIENT_IDS=<Client-ID der SPA>
OIDC_SCOPES=openid,profile,offline_access,api://<Application-ID der API>/access_as_user
OIDC_ROLES_CLAIM=roles
```

## 7. Betrieb und Ausfall

**Schlüssel-Cache.** Jeder Voter speichert die Discovery-Daten und die Signaturschlüssel in `<DATA_DIR>/oidc-jwks.json` mit den Rechten `0600`. Die Datei wird atomar geschrieben. Beim Start liest der Voter zuerst diese Datei.

**Aktualisierung:**

- Die Schlüssel werden alle 5 min neu geladen, oder früher, wenn der IdP ein kürzeres `max-age` angibt.
- Ist ein `kid` im Token unbekannt, lädt der Voter die Schlüssel nach. Das geschieht höchstens einmal je 30 s pro Voter. Parallele Anfragen warten auf denselben Abruf.
- Ein erfolgreich geladener Schlüsselsatz ersetzt den alten vollständig. Entfernte Schlüssel werden damit ungültig.

**Ausfall des IdP:**

- Bestehende Sitzungen laufen weiter. Die Prüfung von Access-Tokens erfolgt lokal, ohne Aufruf beim IdP.
- Die letzten gültigen Schlüssel bleiben in Gebrauch, auch über längere Zeit.
- Ein Neustart funktioniert mit der Cache-Datei, auch wenn der IdP nicht erreichbar ist.
- Ein Fehlschlag beim Laden zählt die Metrik `uptimecluster_auth_jwks_fetch_failures_total`.
- Checks und Alarme laufen unabhängig vom IdP weiter.
- Ein Voter ohne Cache-Datei und ohne Verbindung zum IdP beantwortet Anfragen mit `503` und meldet `/readyz` als nicht bereit. Der Grund steht in der Antwort.
- **Reload der SPA:** Die Tokens liegen nur im Speicher und sind nach einem Neuladen weg. Die SPA leitet dann mit `prompt=none` zum IdP um. Dafür muss der IdP erreichbar sein. Liefert er `login_required`, folgt ein normaler Login.

**Sicherheit der Verbindung zum IdP:**

- Der IdP wird nur über HTTPS angesprochen, außer bei `OIDC_INSECURE_HTTP=true`.
- Antworten sind auf 64 KiB begrenzt. Es gibt Zeitlimits, und Weiterleitungen werden nicht befolgt.
- Im Modus `oidc` erweitert der Voter die Content-Security-Policy `connect-src` automatisch um den Ursprung (Origin) des Issuers.

## 8. Fehlersuche

Die Begründung für `401` steht nicht in der Antwort, sondern im Log. Der Voter schreibt bei jeder abgelehnten Anfrage eine Zeile `oidc: token rejected` mit dem Feld `reason`.

| Symptom | Ursache | Vorgehen |
| --- | --- | --- |
| `401`, Meldung `opaque access tokens are not supported …` | Der IdP stellt opake Tokens aus. | Den Token-Typ auf JWT umstellen (Zitadel: Token Type der Anwendung). |
| `401` ohne Detail, `reason` im Log | Ein Prüfschritt ist fehlgeschlagen. | Das `reason` auswerten, siehe unten. |
| `401`, `reason` z. B. `no azp` | Das Token enthält kein `azp`. | Client-ID im IdP prüfen oder `OIDC_REQUIRE_AZP=false` setzen (nur wenn nötig). |
| `401`, `reason` z. B. `azp is not an allowed client` | Das Token stammt von einem anderen Client. | Client-ID der SPA in `OIDC_CLIENT_IDS` prüfen. |
| `401`, `reason` mit `lifetime … exceeds the maximum` | Das Token lebt länger als `OIDC_MAX_TOKEN_TTL`. | Access-Token-Laufzeit auf 15 min setzen oder `OIDC_MAX_TOKEN_TTL` anheben (höchstens `12h`). |
| `401`, `reason` mit `ID token presented as access token` | Die SPA sendet das ID-Token statt des Access-Tokens. | Im Client das Access-Token senden. |
| `401`, `reason` mit Issuer-, Audience- oder Signaturfehler | `OIDC_ISSUER`, `OIDC_AUDIENCE` oder die Algorithmen passen nicht. | Den `iss`- und `aud`-Claim eines Tokens mit der Konfiguration vergleichen. Auf einen abschließenden `/` achten. |
| `403`, `the token grants no role for this application` | Keine gültige Rolle im Token. | Bei `generic`: `OIDC_ROLES_CLAIM` und `OIDC_ROLE_MAP` prüfen. Bei `zitadel`: „Assert Roles on Authentication“, Rollenschlüssel und Organisation des Nutzers prüfen. |
| `403`, `organization is not admitted` | `single`: Die Organisation des Nutzers ist nicht `ZITADEL_PLATFORM_ORG_ID`. | Nutzer in die Plattform-Organisation holen oder auf `multi` umstellen. |
| `403`, `organization is not provisioned` | `multi`: Der Organisation ist kein Mandant zugeordnet. | Mandant anlegen und binden (Automatik und Admin-API mit AP-4.2). |
| `403`, `the token names no organization …` | Der Scope `urn:zitadel:iam:user:resourceowner` fehlt. | Den Scope in `OIDC_SCOPES` ergänzen. |
| `403`, `tenant does not exist`, `tenant is suspended` | Der Mandant fehlt oder ist gesperrt. | Mandanten in der Verwaltung prüfen. |
| `503` | Keine Signaturschlüssel vorhanden oder ein Dienst ist vorübergehend nicht erreichbar. | `/readyz` abfragen, Log prüfen, Metrik `uptimecluster_auth_jwks_fetch_failures_total` ansehen, Verbindung vom Voter zum IdP prüfen. |
| Start schlägt mit `invalid configuration` fehl | Eine OIDC-Variable ist ungültig oder passt nicht zum Profil. | Die Fehlerliste im Log lesen und die Tabelle in Abschnitt 3 vergleichen. |
| Browser meldet CORS-Fehler am Token-Endpunkt | Der IdP erlaubt den Ursprung der SPA nicht. | Web Origins bzw. CORS-Einstellungen im IdP ergänzen. |
| `409 Conflict`, „users are managed by the identity provider“ | Im Modus `oidc` ist lokale Anmeldung abgeschaltet. | Erwartet. Anmeldung über den IdP verwenden. |

## Verwandte Dokumente

- [compose.md](compose.md): Grundkonfiguration und Umgebungsvariablen
- [recovery.md](recovery.md): Notzugang und Recovery-CLI
- [cluster.md](cluster.md): Voter hinzufügen und entfernen
- ADR 0027: Entscheidung und Begründung
