# ADR 0002: Go-HTTP-Stack, Konfiguration und Logging

| | |
|---|---|
| **Status** | Proposed |
| **Datum** | 2026-10-19 |
| **Entscheider** | Projektinhaber |
| **Bezug** | FA-NODE-02, FA-NODE-05, FA-NODE-07, FA-API-01..05, Konzept 18.1 (chi, coder/websocket, slog) |

## Kontext

Das Konzept sieht `net/http` + `chi` als Router vor. Seit Go 1.22 unterstützt `http.ServeMux` Methoden und Pfadparameter (`GET /api/v1/monitors/{id}`). Jede zusätzliche Abhängigkeit muss lizenzrechtlich (Dual Licensing) und sicherheitstechnisch gepflegt werden. Die REST-Schicht wird aus der OpenAPI-Spezifikation generiert (ADR 0003); der Generator muss den Router unterstützen.

## Optionen

### Option A: Standardbibliothek `net/http.ServeMux` + eigene Middleware
- Vorteile: Keine Abhängigkeit; Pfadparameter und Methoden-Routing nativ; von `oapi-codegen` als `std-http-server` unterstützt.
- Nachteile: Middleware-Gruppen/Sub-Router muss man selbst zusammenstecken (wenige Zeilen).

### Option B: `go-chi/chi/v5` (wie im Konzept)
- Vorteile: Bewährt, komfortable Middleware-Gruppen, viele Beispiele.
- Nachteile: Zusätzliche Abhängigkeit ohne funktionalen Mehrwert gegenüber Go ≥ 1.22.

### Option C: Framework (Echo, Gin, Fiber)
- Nachteile: Eigene Handler-Signaturen, schlechtere Kompatibilität mit `net/http`-Ökosystem; Fiber nicht `net/http`-kompatibel.

## Entscheidung

**Option A (Abweichung vom Konzept: kein chi).** Wechsel zu chi bleibt jederzeit mit geringem Aufwand möglich, da beide `http.Handler` nutzen.

Weitere Festlegungen:
- **Konfiguration:** Umgebungsvariablen, eingelesen in ein typisiertes `config.Config` mit eigener, kleiner Parser-Funktion (Standardbibliothek), Validierung beim Start, dokumentierte Defaults. Keine Konfigurationsbibliothek.
- **Logging:** `log/slog`, Format `json` (Standard) oder `text`, Level per `LOG_LEVEL`. Geheimnisse werden über einen `slog.LogValuer`-Typ maskiert.
- **WebSocket:** `github.com/coder/websocket` (ISC), wie im Konzept.
- **Fehlerformat:** RFC 9457 Problem Details (`application/problem+json`).
- **Server:** getrennte Timeouts (`ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout` – außer für Streams, `IdleTimeout`), Graceful Shutdown über `signal.NotifyContext` mit 10 s Frist.
- **Endpunkte ohne Auth:** `/healthz` (Liveness), `/readyz` (Readiness), `/api/v1/health` (öffentliche Kurzinfo), `/config.json`.

## Konsequenzen

- Positiv: Minimale Abhängigkeiten, idiomatisches Go, leichtes Erlernen (Konzept 18.3).
- Negativ: Etwas eigener Code für Middleware-Ketten.
- Folgearbeiten: Middleware für Request-ID, Recovery, Logging, CORS, Security-Header, Rate-Limit (P1).

## Approval

| Datum | Status | Durch | Anmerkung |
|-------|--------|-------|-----------|
| 2026-10-19 | Proposed | Claude | |
