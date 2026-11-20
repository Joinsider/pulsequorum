# ADR 0003: API-Vertrag (OpenAPI) und Codegenerierung

|                 |                                                   |
|-----------------|---------------------------------------------------|
| **Status**      | Accepted                                          |
| **Datum**       | 2026-10-19                                        |
| **Entscheider** | Projektinhaber                                          |
| **Bezug**       | FA-API-01..06, NFA-MNT-03, O-03, Konzept 14, 18.1 |

## Kontext

`api/openapi.yaml` ist laut Konzept der Vertrag zwischen Backend und Frontend und Grundlage für Automatisierung durch
Kunden. Handgeschriebene Typen auf beiden Seiten driften auseinander. Der Generator muss zum HTTP-Stack (ADR 0002:
`net/http.ServeMux`) passen.

Stand Okt. 2026: `oapi-codegen` v2.8.0 (Apache-2.0) unterstützt OpenAPI 3.1 nur teilweise (Mehrfachtypen werden zu
`any`), hat einen `std-http-server`-Generator und `strict-server`. `ogen` (Apache-2.0) bringt eigenen Router mit.
Frontend-Generatoren: `orval` 8.x (MIT, Angular `HttpClient`/`httpResource`), `ng-openapi-gen` 1.x (MIT, Angular ≥ 16),
`@hey-api/openapi-ts` (MIT, Angular-Plugin unklar).

## Optionen

### Server

- **S1: `oapi-codegen` (`std-http-server` + `strict-server`)** – passt zu `net/http`, typisierte
  Request/Response-Objekte, große Nutzerbasis.
- **S2: `ogen`** – sehr schneller generierter Code inkl. Validierung, aber eigener Router und eigene Konventionen.
- **S3: Handgeschrieben + Spezifikation manuell pflegen** – Drift-Risiko.

### Client

- **C1: `orval`** – aktiv entwickelt, unterstützt `httpResource` (Signals), Mock-Generierung (MSW) für Tests.
- **C2: `ng-openapi-gen`** – schlank, Angular-nah, weniger Funktionen.
- **C3: Handgeschriebene Services** – Drift-Risiko.

## Entscheidung

- **Spezifikation:** OpenAPI **3.0.3** (statt 3.1), um die bekannten 3.1-Lücken der Generatoren zu vermeiden.
  Spec-first: Änderungen beginnen in `api/openapi.yaml`.
- **Server: S1** `oapi-codegen` v2 (Version in `tools`-Abhängigkeit gepinnt) mit `std-http-server` und `strict-server`;
  generierter Code unter `internal/api/gen`, eingecheckt.
- **Client: C1** `orval` mit `HttpClient`-Services; generierter Code unter `web/src/app/api/gen`, eingecheckt. Basis-URL
  kommt aus dem `ConfigService` (Laufzeit-`config.json`), nicht aus der Spezifikation.
- **JSON-Schemas der Monitor-Typen** (Checker-Plugins) sind **nicht** Teil der OpenAPI-Spezifikation; sie werden zur
  Laufzeit über `GET /api/v1/monitor-types` geliefert (`config` ist im Vertrag ein freies Objekt).
- **CI:** `make generate` und anschließender `git diff --exit-code` stellt sicher, dass generierter Code aktuell ist;
  Spezifikation wird mit einem Linter (z.B. Redocly CLI oder Spectral) geprüft.
- **Fehler:** RFC 9457 Problem Details als gemeinsames Schema.

## Konsequenzen

- Positiv: Ein Vertrag, typsichere Server- und Clientseite, Kunden erhalten eine verlässliche API-Doku.
- Negativ: Zwei Codegeneratoren in der Toolchain; 3.0.3 statt 3.1 (kein `type: [x, "null"]`, stattdessen `nullable`).
- Folgearbeiten: OpenAPI-Entwurf für alle P1-Endpunkte (AP-0.12); Contract-Tests.

## Approval

| Datum      | Status   | Durch    | Anmerkung                                 |
|------------|----------|----------|-------------------------------------------|
| 2026-10-19 | Proposed | Claude   |                                           |
| 2026-10-19 | Accepted | Projektinhaber | im Chat für alle ADRs 0001–0011 bestätigt |
