# Mitwirken an UptimeCluster

Vielen Dank für dein Interesse an UptimeCluster.

## Wichtig: Externe Beiträge werden derzeit noch nicht angenommen

Externe Pull Requests werden **noch nicht akzeptiert**. Die Contributor License
Agreement (CLA) muss zuerst von einer Anwältin bzw. einem Anwalt geprüft werden.
Erst danach werden externe Beiträge angenommen. Bis dahin bitte keine PRs von
außerhalb des Projektteams einreichen.

## Contributor License Agreement (CLA)

Jeder Beitrag erfordert die Unterzeichnung einer CLA, bevor er gemergt wird.
Die Unterzeichnung läuft technisch über [CLA Assistant](https://cla-assistant.io/)
als GitHub-App; der Bot kommentiert den Pull Request und fragt die Zustimmung ab.

- **Individual CLA** für Beiträge als Einzelperson.
- **Corporate CLA** für Beiträge im Auftrag eines Arbeitgebers oder einer Organisation.

Die CLA räumt dem Projekt eine umfassende Lizenz an den Beiträgen ein, damit
UptimeCluster sowohl unter der AGPL-3.0-only als auch unter der kommerziellen
Lizenz vertrieben werden kann (siehe [LICENSING.md](LICENSING.md)). **Du behältst
das Urheberrecht an deinem Beitrag.** Es wird nicht übertragen.

Eine reine Developer Certificate of Origin (DCO) genügt **nicht**, weil sie keine
ausreichende Rechteeinräumung für die Relizenzierung umfasst.

## Konventionen

Bitte halte dich an die Projektkonventionen aus [CLAUDE.md](CLAUDE.md):

- **Commits** folgen [Conventional Commits](https://www.conventionalcommits.org/) und sind auf Englisch verfasst (z.B. `feat(checker): add TCP checker`). Der Commit-Body erklärt das *Warum*.
- **Tests und Build** müssen für die betroffenen Teile grün sein (`make test`, `make lint`).
- **Architekturentscheidungen:** Große Änderungen (siehe CLAUDE.md, Regel 2) brauchen vorab einen ADR in `docs/adr/`.
- **Sprache:** Dokumentation auf Deutsch, Code, Bezeichner, Kommentare und Log-Meldungen auf Englisch.
- **SPDX-Header:** Neue Quelldateien erhalten den Header `SPDX-License-Identifier: AGPL-3.0-only` mit Copyright-Zeile.
