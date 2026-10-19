# ADR 0007: Frontend-Stack

| | |
|---|---|
| **Status** | Accepted |
| **Datum** | 2026-10-19 |
| **Entscheider** | Projektinhaber |
| **Bezug** | FA-UI-01..09, NFA-USE-02, PF-34, PF-40, E15, E21, Konzept 13 |

## Kontext

Das Konzept legt Angular und ECharts fest. Aktuelle Major-Version ist Angular 22 (Active bis 2027-06, LTS bis 2028-06). Seit Angular 21 sind zoneless Change Detection und Vitest Standard für neue Projekte; Karma ist abgekündigt. Die SPA wird eingebettet und extern gehostet; Live-Daten kommen per WebSocket. Zu entscheiden sind UI-Komponenten, Charts, Tests, OIDC-Bibliothek und i18n.

## Optionen (UI-Komponenten)

- **U1: Angular Material + CDK 22** (MIT) – offiziell, mit Angular synchron versioniert, gute Barrierefreiheit, Material-3-Theming.
- **U2: PrimeNG** – sehr viele Komponenten (u.a. Tabellen), aber eigener Release-Takt und wechselnde Lizenzmodelle bei Themes.
- **U3: Tailwind + eigene Komponenten** – maximale Gestaltungsfreiheit, deutlich mehr Eigenaufwand und A11y-Verantwortung.

## Entscheidung

- **Angular 22**, Standalone-Komponenten, **zoneless**, Signals für Zustand, neue Control-Flow-Syntax, funktionale Guards, Lazy-Loading je Feature (`/admin` separat).
- **Build:** `@angular/build:application` (esbuild), Ausgabe nach `web/dist`, Einbettung gemäß ADR 0001.
- **UI: U1** Angular Material + CDK.
- **Charts:** ECharts 6 (Apache-2.0) über `ngx-echarts` 22 (MIT), mit Tree-Shaking (nur benötigte Chart-Typen importieren).
- **Tests:** Vitest (Unit/Komponenten, Standard der CLI), Playwright (E2E, P1).
- **Lint/Format:** `angular-eslint`, Prettier (vom CLI angelegt).
- **Live-Daten:** WebSocket-Nachrichten werden ausschließlich in Signals geschrieben (zoneless, PF-40); Reconnect mit Backoff + Jitter.
- **Laufzeit-Konfiguration:** `config.json` wird vor dem Bootstrap über `provideAppInitializer` geladen; keine Build-Zeit-Umgebungsdateien für URLs.
- **Auth:** eigener `AuthService` (ADR 0008) mit austauschbarer Strategie; für Zitadel (P4) `angular-auth-oidc-client` (MIT, OpenID-zertifiziert, Angular ≥ 20).
- **i18n:** Laufzeit-Übersetzung, damit ein einziges Build-Artefakt alle Sprachen enthält (die eingebaute Angular-i18n erzeugt ein Build je Sprache). Bibliothek: `@jsverse/transloco` (MIT); Deutsch und Englisch.
- **API-Client:** generiert (ADR 0003).

## Konsequenzen

- Positiv: Zukunftssicherer Standard-Stack, wenige, gut gepflegte Abhängigkeiten, ein Artefakt.
- Negativ: Material-Optik muss für einen eigenständigen Auftritt angepasst werden (Theming).
- Folgearbeiten: Design-Grundlagen (Farben/Status-Symbole, Dark Mode) zu Beginn von AP-1.14.

## Approval

| Datum | Status | Durch | Anmerkung |
|-------|--------|-------|-----------|
| 2026-10-19 | Proposed | Claude | |
| 2026-10-19 | Accepted | Projektinhaber | im Chat für alle ADRs 0001–0011 bestätigt |
