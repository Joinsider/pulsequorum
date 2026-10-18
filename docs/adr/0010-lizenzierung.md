# ADR 0010: Lizenzierung, CLA und Abhängigkeitslizenzen

| | |
|---|---|
| **Status** | Proposed |
| **Datum** | 2026-10-19 |
| **Entscheider** | Projektinhaber |
| **Bezug** | RB-LIC-01..05, PF-41, PF-42, O-06, E17, E18, Konzept 19 |

## Kontext

E18 legt AGPLv3 + kommerzielle Lizenz fest. Offen sind die genaue SPDX-Variante, das Verfahren für Beiträge und die erlaubten Lizenzen von Abhängigkeiten. Recherche: Ein DCO genügt für Relizenzierung nicht; erforderlich ist eine CLA mit ausreichender Rechteeinräumung.

## Optionen (SPDX-Variante)

### Option A: `AGPL-3.0-only`
- Vorteile: Volle Kontrolle; künftige FSF-Versionen gelten nicht automatisch. Üblich bei Dual-Licensing-Projekten.
- Nachteile: Ein späterer Wechsel auf eine neue AGPL-Version erfordert eine bewusste Relizenzierung (mit CLA möglich).

### Option B: `AGPL-3.0-or-later`
- Vorteile: Automatische Kompatibilität mit künftigen Versionen.
- Nachteile: Lizenzbedingungen könnten sich ohne eigenes Zutun ändern.

## Entscheidung

- **SPDX:** `AGPL-3.0-only` (Option A). Jede Quelldatei beginnt mit `// SPDX-License-Identifier: AGPL-3.0-only` und einer Copyright-Zeile.
- **Dateien:** `LICENSE` (AGPLv3-Volltext), `LICENSING.md` (Dual-Licensing erklärt, Kontakt für kommerzielle Lizenz), `CONTRIBUTING.md` (CLA-Pflicht).
- **CLA:** Individual- und Corporate-CLA mit umfassender Lizenzeinräumung (keine Copyright-Übertragung), technisch über CLA Assistant (GitHub App). **Text wird anwaltlich geprüft, bevor der erste externe Beitrag angenommen wird.** Bis dahin keine externen PRs mergen.
- **Erlaubte Abhängigkeitslizenzen (Allow-List):** MIT, BSD-2-Clause, BSD-3-Clause, Apache-2.0, ISC, MPL-2.0 (nur unverändert genutzt), 0BSD, Unlicense. Alles andere nur per ADR. Prüfung in CI für Go und npm (ADR 0009).
- **AGPL §13:** Die UI zeigt in „Über“ und im Footer einen Link auf den Quellcode der laufenden Version (Repository-URL + Commit).
- **Third-Party-Notices:** Beim Release-Build generiert und im Image unter `/licenses` sowie in der UI verlinkt.

## Konsequenzen

- Positiv: Rechtlich saubere Basis für das kommerzielle Modell.
- Negativ: CLA senkt die Beitragsbereitschaft etwas; Kosten für anwaltliche Prüfung.
- Hinweis: Dies ist keine Rechtsberatung.

## Approval

| Datum | Status | Durch | Anmerkung |
|-------|--------|-------|-----------|
| 2026-10-19 | Proposed | Claude | |
