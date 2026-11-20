# ADR 0008: Lokale Authentifizierung und Token-Handling in der SPA

|                 |                                                                                  |
|-----------------|----------------------------------------------------------------------------------|
| **Status**      | Accepted                                                                         |
| **Datum**       | 2026-10-19                                                                       |
| **Entscheider** | Projektinhaber                                                                         |
| **Bezug**       | FA-AUTH-01..04, FA-AUTH-09, FA-AUTH-11, NFA-SEC-03/04, PF-32..34, O-04, E13, E15 |

## Kontext

Im Modus `local` stellt der Cluster selbst Tokens aus. Die SPA spricht beliebige Voter an und wechselt bei Fehlern
(Client-Failover). Cookies sind an Hosts gebunden und überleben einen Wechsel des Voters nur bei gemeinsamer
Parent-Domain; Tokens in `localStorage` sind XSS-anfällig. WebSockets erlauben im Browser keinen `Authorization`-Header.

## Optionen (Refresh-Strategie)

### Option A: Access-Token im Speicher, Refresh-Token als HttpOnly-Cookie mit konfigurierbarer Cookie-Domain

- Access-Token (Bearer) funktioniert gegenüber allen Votern. Refresh-Cookie
  (`HttpOnly; Secure; SameSite=Strict; Path=/api/v1/auth`) gilt für die konfigurierte Domain (z.B. `example.com` bei
  `mon1/mon2/mon3.example.com`) – damit kann jeder Voter refreshen. Ohne gemeinsame Domain: Host-Cookie, nach Failover
  ist ein Refresh nur beim ursprünglichen Voter möglich, sonst neuer Login nach Ablauf des Access-Tokens.
- Vorteile: Kein Token in JavaScript-zugänglichem Speicher außer dem kurzlebigen Access-Token; Reload ohne Neu-Login.
- Nachteile: Externes Hosting auf fremder Domain (Cloudflare Pages) wäre Cross-Site → Third-Party-Cookies, in Browsern
  zunehmend blockiert. Lösung dort: SPA unter eigener Subdomain derselben Site (z.B. `status.example.com`).

### Option B: Refresh-Token in `sessionStorage`

- Vorteile: Failover überall möglich.
- Nachteile: XSS kann Refresh-Token stehlen (OWASP rät ab).

### Option C: Nur Access-Token im Speicher, kein Refresh

- Nachteile: Neu-Login bei jedem Reload – schlechte Bedienbarkeit.

## Entscheidung

**Option A.**

- **Passwörter:** Argon2id (`golang.org/x/crypto/argon2`), m=19 MiB, t=2, p=1, 16-Byte-Salt, Parameter im Hash-String
  gespeichert (PHC-Format) für spätere Anhebung.
- **Access-Token:** JWT, EdDSA (Ed25519), `kid`, Laufzeit 15 min, Claims `sub`, `tid` (Tenant), `roles`, `iss` =
  Cluster-ID, `aud` = `pulsequorum`.
- **Signaturschlüssel:** im Raft-Zustand, privater Teil verschlüsselt (ADR 0011); Rotation alle 30 Tage, alter Schlüssel
  bleibt 1 Tag zur Verifikation gültig.
- **Refresh-Token:** opak, 256 Bit, nur Hash gespeichert (Raft), Laufzeit 14 Tage gleitend, Rotation bei jeder Nutzung,
  Wiederverwendung → Widerruf der Familie (FA-AUTH-03). Refresh-Endpunkt verlangt zusätzlich den Header
  `X-Requested-With: pulsequorum` (CSRF-Schutz zusätzlich zu `SameSite=Strict`).
- **Konfiguration:** `AUTH_COOKIE_DOMAIN` (leer = Host-Cookie).
- **Erst-Einrichtung:** Ohne Nutzer erzeugt der Leader einen einmaligen Setup-Token (Log-Ausgabe, 1 h gültig);
  `POST /api/v1/auth/setup` legt den ersten `platform-admin` an.
- **Drosselung:** pro Konto (5 Fehlversuche → progressive Verzögerung bis 15 min) und pro IP; einheitliche
  Fehlermeldung.
- **WebSocket:** `POST /api/v1/stream/ticket` liefert ein einmaliges Ticket (30 s), Übergabe als Query-Parameter
  `ticket`; Verbindung wird bei Ablauf des zugrunde liegenden Access-Tokens nicht sofort getrennt, aber nach Ablauf der
  Sitzung (Refresh-Familie) geschlossen.
- Zitadel-Modus (P4) erhält einen eigenen ADR; die SPA-Schnittstelle `AuthService` wird so geschnitten, dass beide Modi
  dahinter liegen.

## Konsequenzen

- Positiv: OWASP-konform, Failover für den empfohlenen Aufbau (gemeinsame Domain) ohne Neu-Login.
- Negativ: Externes Hosting nur sinnvoll auf derselben Site; dokumentationspflichtig.
- Folgearbeiten: Doku „Domains und Cookies“ (P5), Berechtigungsmatrix-Tests (P1).

## Approval

| Datum      | Status   | Durch    | Anmerkung                                 |
|------------|----------|----------|-------------------------------------------|
| 2026-10-19 | Proposed | Claude   |                                           |
| 2026-10-19 | Accepted | Projektinhaber | im Chat für alle ADRs 0001–0011 bestätigt |
