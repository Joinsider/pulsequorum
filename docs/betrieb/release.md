# Release: Ablauf, Prüfung und Fehlerfälle

Dieses Runbook beschreibt, wie ein Release von Pulsequorum entsteht (ADR 0040, Änderung vom 2026-11-20). Es richtet sich an den Projektinhaber, der Releases auslöst und veröffentlicht.

## Überblick

1. Der Projektinhaber startet `make release` auf einem sauberen, aktuellen `master`.
2. Das Werkzeug prüft den Stand, berechnet die Version und zeigt den Changelog. Der Changelog öffnet sich im Editor und lässt sich überarbeiten.
3. Das Werkzeug legt einen **annotierten, signierten Tag** an, prüft die Signatur lokal und pusht den Tag nach Rückfrage.
4. Der Tag startet den Workflow `Release` (`.github/workflows/release.yml`). Er prüft Tag und CI erneut, führt die Sicherheitsprüfungen aus und baut einen **Draft-Release** mit Archiven, Checksummen, SBOMs und Images auf GHCR.
5. Der Projektinhaber prüft den Draft und **veröffentlicht ihn manuell**.

Die Tag-Nachricht ist der Changelog. Sie ist mitsigniert und wird zum Text des Releases.

**Warum kein Schlüssel in CI?** Der Signierschlüssel liegt nur auf dem Rechner des Projektinhabers. Es gibt kein Bot-Konto, keinen Deploy-Key und keine Signatur-Secrets. Ein Angreifer, der die CI kompromittiert, kann deshalb keinen gültigen Tag erzeugen. Der Workflow vertraut nur dem Schlüssel aus `.github/allowed_signers` auf `master`. Der manuelle Publish-Klick ist das zweite menschliche Gate.

## Voraussetzungen

Vor dem ersten Release prüfen:

- **Lokale SSH-Signatur ist in git konfiguriert.** Der Signierschlüssel steht in `user.signingkey`, das Format ist SSH:
  ```bash
  git config --get gpg.format        # muss "ssh" sein
  git config --get user.signingkey   # Pfad zum öffentlichen Schlüssel
  git config --get user.email        # Noreply-Adresse aus .github/allowed_signers
  ```
  Die Adresse in `user.email` muss zum Prinzipal in `.github/allowed_signers` passen. Sonst scheitert `git verify-tag`.
- **`gh` ist angemeldet** und darf das Repository lesen (Check-Runs werden abgefragt):
  ```bash
  gh auth status
  ```
- **Der Schlüssel steht in `.github/allowed_signers`.** Die Datei ist versioniert und auf `master` gültig.
- **`master` ist sauber und aktuell.** Der Arbeitsbaum hat keine Änderungen, und `master` entspricht `origin/master`:
  ```bash
  git switch master
  git status
  git fetch origin master
  ```

## Release erstellen

### Vorschau: `make release-dry-run`

```bash
make release-dry-run                  # nächste Patch-Version
make release-dry-run BUMP=minor       # nächste Minor-Version
make release-dry-run RELEASE_VERSION=0.3.0-rc.1
```

Der Befehl zeigt die nächste Version, die Zahl der Commits seit dem letzten Release und den Changelog. Er legt nichts an, pusht nichts und prüft die CI nicht. Er prüft aber Branch, Arbeitsbaum und Abgleich mit `origin/master`.

### Release anlegen: `make release`

```bash
make release BUMP=patch               # Standard: patch
make release BUMP=minor
make release BUMP=major               # in 0.x nicht erlaubt, siehe Versionsregeln
make release RELEASE_VERSION=0.3.0-rc.1       # explizite Version, hat Vorrang vor BUMP
```

Das Werkzeug prüft in dieser Reihenfolge:

1. Der aktuelle Branch ist `master`.
2. Der Arbeitsbaum ist sauber.
3. Nach `git fetch` ist der lokale `master` identisch mit `origin/master`.
4. Die Version ist gültig. Eine explizite Version muss größer sein als die letzte.
5. Alle Pflicht-Checks sind auf dem Commit grün: `go`, `golangci`, `web`, `licenses`, `generate`, `workflow-lint`, `secrets`, `build`. Das Werkzeug fragt sie über `gh api` ab.

Danach öffnet sich der Editor mit dem Changelog. Ein bereits vorhandener Tag führt zum Abbruch, weil git ihn ablehnt.

**Changelog bearbeiten.** Der Editor zeigt eine Kopfzeile mit Hinweisen. Beim Speichern gilt:

- Zeilen, die mit `# ` beginnen (und Zeilen, die nur `#` sind), entfallen. Die Kopfzeilen des Werkzeugs verschwinden so automatisch.
- Überschriften wie `### Features` bleiben erhalten.
- Ein leerer Text bricht den Vorgang ab.
- Die erste Zeile ist der Betreff des Tags. Sie wird nicht als Release-Text verwendet. Der Titel des Releases lautet immer `Pulsequorum vX.Y.Z`.

Der Editor wird über `GIT_EDITOR`, `VISUAL` oder `EDITOR` gewählt, sonst `vi`.

**Tag und Push.** Nach dem Speichern legt das Werkzeug den Tag an (`git tag --sign --cleanup=verbatim`) und prüft ihn mit `git verify-tag` gegen `.github/allowed_signers`. Schlägt die Prüfung fehl, löscht das Werkzeug den lokalen Tag und bricht ab.

**Vor dem Push fragt das Werkzeug nach:** „Push signed tag … This starts the release workflow.“ Nur `y` pusht. Jede andere Antwort lässt den Tag lokal. Später reicht `git push origin refs/tags/vX.Y.Z`.

## Versionsregeln

- SemVer im Bereich `0.x`. Solange die Major-Version 0 ist, kann alles brechen.
- Breaking Changes erhöhen in `0.x` die **Minor**-Stelle. Alle anderen Änderungen erhöhen die Patch-Stelle.
- `BUMP=major` ist in `0.x` abgelehnt. `1.0.0` entsteht nur mit ausdrücklicher Angabe `RELEASE_VERSION=1.0.0`.
- Erste Version ist `v0.0.1`. Sie dient dem technischen Durchstich der Pipeline.
- Vorabversionen tragen ein Suffix: `-rc.N`, `-alpha.N` oder `-beta.N`. Sie werden auf GitHub als Pre-Release markiert.
- Image-Tags: `X.Y.Z` immer. `X.Y` nur ohne Suffix. `latest` erst ab `1.0.0` und nur ohne Suffix. Ein `latest` auf einer `0.x`-Version würde Nutzer unbemerkt auf Breaking Changes bringen.

## Was der Workflow tut

Der Workflow `Release` startet bei einem Tag `v*` oder manuell per `workflow_dispatch`. Rechte gibt es nur je Job. Ein Lauf nach dem anderen (`concurrency: release`, ohne Abbruch).

| Job | Auslöser | Aufgabe |
|-----|----------|---------|
| `verify-tag` | Tag-Push | Prüft, dass der Tag SemVer ist und annotiert. Prüft die Signatur gegen `.github/allowed_signers` von `origin/master`. Prüft, dass der getaggte Commit auf `master` liegt. Prüft, dass alle Pflicht-Checks auf dem Commit grün sind. Gibt Version, SHA und Notizen aus. |
| `gates` | Tag-Push | `govulncheck` (gepinnt über `tools/go.mod`) und `npm audit` mit geprüfter Ausnahmeliste. Ein Befund bricht den Release ab (NFA-SEC-06). |
| `goreleaser` | Tag-Push, nach `verify-tag` und `gates` | Läuft im Environment `release`. Checkt exakt den geprüften Commit aus. Baut mit GoReleaser einen **Draft** (Archive, SBOMs, Multi-Arch-Images auf GHCR). Signiert `checksums.txt` und die Images mit cosign keyless. Erzeugt Build-Provenance. Prüft Checksummen, Signaturen und Attestation und schreibt einen Link auf den Draft in die Step Summary. |
| `snapshot` | nur `workflow_dispatch` | Probelauf: `goreleaser release --snapshot --skip=publish,sign`. Veröffentlicht nichts. Die Artefakte liegen 7 Tage als Workflow-Artefakt `snapshot-dist`. |

Der Probelauf eignet sich, um Build und Konfiguration ohne Tag zu testen. Er verändert keinen Release.

## Draft prüfen und veröffentlichen

1. Im Step Summary des Laufs den Link auf den Draft öffnen, oder unter **Releases** den Draft suchen.
2. Prüfen:
   - Titel `Pulsequorum vX.Y.Z` und Notizen. Die Notizen stammen aus der signierten Tag-Nachricht.
   - Archive für Linux und macOS (amd64, arm64), `checksums.txt`, `checksums.txt.sigstore.json` und die SBOMs.
   - Die Kennzeichnung als Pre-Release passt zum Suffix.
   - Im Step Summary steht der Image-Digest.
3. Den Draft mit **Publish release** veröffentlichen.

Mit dem Veröffentlichen werden Tag und Assets **unveränderlich** (Immutable Releases). GitHub erzeugt die Release-Attestation. Danach ist ein Fehler im Tag oder in einem Asset nicht mehr korrigierbar. Siehe „Fehlerfälle“.

### Release-Notizen nachträglich ändern

Titel und Notizen lassen sich auch nach dem Veröffentlichen über **Edit** am Release ändern. Tag und Assets bleiben unverändert. Die signierte Tag-Nachricht bleibt dagegen so, wie sie beim Anlegen war. Ein Release-Text, der von der Tag-Nachricht abweicht, ist also zulässig. Die Tag-Nachricht ist der maßgebliche, signierte Text.

## Artefakte prüfen

Alle Befehle ersetzen `X.Y.Z` durch die Version ohne `v` (Images) und `vX.Y.Z` durch den Tag. Das Repository ist `Joinsider/pulsequorum`.

**1. Tag-Signatur** (im Repository-Verzeichnis):

```bash
git -c gpg.format=ssh -c gpg.ssh.allowedSignersFile=.github/allowed_signers verify-tag vX.Y.Z
```

**2. Checksummen** (Archive, `checksums.txt` und `checksums.txt.sigstore.json` in ein leeres Verzeichnis laden, z.B. mit `gh release download vX.Y.Z --repo Joinsider/pulsequorum`):

```bash
sha256sum -c checksums.txt
```

Liegen nur einzelne Archive vor, `sha256sum --ignore-missing -c checksums.txt` verwenden. Auf macOS ohne GNU-Tools: `shasum -a 256 -c checksums.txt`.

**3. Signatur der Checksummen:**

```bash
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity https://github.com/Joinsider/pulsequorum/.github/workflows/release.yml@refs/tags/vX.Y.Z \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
```

**4. Signatur des Images:**

```bash
cosign verify \
  --certificate-identity https://github.com/Joinsider/pulsequorum/.github/workflows/release.yml@refs/tags/vX.Y.Z \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/joinsider/pulsequorum:X.Y.Z
```

**5. Herkunftsnachweis des Images:**

```bash
gh attestation verify oci://ghcr.io/joinsider/pulsequorum:X.Y.Z --repo Joinsider/pulsequorum
```

**6. Release-Attestation von GitHub** (nur für veröffentlichte Releases):

```bash
gh release verify vX.Y.Z
```

Schlagen die Prüfungen 3 bis 5 fehl, den Release nicht weitergeben. Ursache klären, bevor ein Nutzer das Image einsetzt.

## Fehlerfälle

**Tag wird abgelehnt (lokal oder beim Push).**
- Schlägt `verify-tag` lokal fehl, entfernt das Werkzeug den Tag selbst. Ist der Tag noch lokal vorhanden, löschen: `git tag -d vX.Y.Z`.
- Lehnt der Server den Push ab (Ruleset), bleibt der Tag lokal. Ursache klären und den Tag danach löschen: `git tag -d vX.Y.Z`. Der Tag war dann nie auf dem Server.

**Tag ist auf dem Server, Workflow scheitert vor dem Publish.**
- Remote-Tags lassen sich nicht löschen. Das Ruleset sperrt Löschungen, und Immutable Releases sperren den Tag-Namen.
- Ursache beheben, auf `master` committen (über PR) und mit der **nächsten Patch-Version** neu auslösen. Beispiel: `v0.0.1` schlägt fehl, dann `v0.0.2`.

**Fehler nach dem Publish.**
- Tag und Assets sind dauerhaft fest. Den Fehler heilt nur eine neue Patch-Version. Notizen lassen sich trotzdem korrigieren.

**Fehlerhafte Version zurückziehen.**

Ein veröffentlichter Release wird nicht gelöscht, sondern ersetzt und sichtbar markiert. Wer die Version schon gezogen hat, behält sie ohnehin, und Mirrors sowie der Go-Modul-Proxy halten eine Kopie. Ein gelöschter Immutable Release sperrt außerdem den Tag-Namen dauerhaft.

1. **Nachfolger veröffentlichen:** Fehler über PR auf `master` beheben und mit `make release` die nächste Patch-Version erzeugen.
2. **Fehlerhafte Version markieren:** Am alten Release über **Edit** ganz oben einfügen: `> ⚠️ Nicht verwenden: <kurzer Grund>. Bitte vX.Y.Z+1 verwenden.` Titel und Notizen bleiben auch nach dem Veröffentlichen editierbar.
3. **Image entfernen, falls nötig:** Im GHCR-Paket `pulsequorum` die Version `X.Y.Z` löschen, damit sie niemand neu zieht. Bei schweren Fehlern auch die Tags `X.Y` prüfen, die nach dem Nachfolger wieder auf eine gute Version zeigen. `latest` gibt es in `0.x` nicht.
4. **Go-Modul zurückziehen:** In `go.mod` des Nachfolgers eine `retract`-Direktive aufnehmen, dann warnt `go get` vor der Version:

   ```
   retract v0.0.3 // Kurzer Grund, siehe Release-Notizen
   ```

5. **Sicherheitslücke:** Zusätzlich ein GitHub Security Advisory anlegen (**Security → Advisories**) und die betroffenen sowie die behobene Version nennen.

**Draft wird verworfen.**
- Die Images liegen bereits in GHCR, sind aber nicht freigegeben. Sie bleiben stehen und müssen von Hand gelöscht werden: GHCR-Paket `pulsequorum` öffnen und die Version entfernen.
- Der Tag bleibt bestehen. Der saubere Weg ist eine neue Patch-Version. `latest` ist davon in `0.x` nicht betroffen.

**CI-Checks sind nicht grün.**
- `make release` bricht ab und nennt die fehlenden Checks. Erst die CI reparieren, dann erneut starten.
- Das Flag `-skip-ci-check` des Werkzeugs umgeht die Prüfung. Es ist nicht mit einem Make-Ziel verknüpft und nur bei sicher grüner CI zu verwenden. `verify-tag` prüft die CI im Workflow trotzdem. Ein Release mit roter CI scheitert dort nach dem Push.

## Schlüsselwechsel

Ein neuer Signierschlüssel kommt **vor dem nächsten Release** in `.github/allowed_signers`. Ablauf:

1. Neuen Ed25519-Schlüssel erzeugen und lokal als `user.signingkey` eintragen.
2. Einen PR mit der neuen Zeile in `.github/allowed_signers` öffnen. Der Prinzipal bleibt die Noreply-Adresse. Die Commits des PRs sind signiert.
3. Den PR über die CI mergen. Erst danach zählt der Schlüssel auf `master`, denn Workflow und Werkzeug lesen die Datei von dort.
4. Den nächsten Release mit dem neuen Schlüssel auslösen.

Der alte Schlüssel bleibt in der Datei, solange er nicht kompromittiert ist, damit ältere Tags prüfbar bleiben. Älteres Prüfen gelingt auch über die Datei aus dem Commit des Tags:
`git show <commit>:.github/allowed_signers`.
Bei Verdacht auf Kompromittierung sofort per PR entfernen und die betroffenen Tags prüfen.

## Falsch-positive Secret-Funde

Der Job `secrets` (gitleaks) meldet manchmal Treffer, die keine Geheimnisse sind. Die Ausnahmen stehen in `.gitleaksignore`, eine Zeile je Fund:

```text
# Begründung, warum es kein Geheimnis ist.
<commit>:<datei>:<regel>:<zeile>
```

Vorgehen:

1. Den Fund prüfen. Ein echtes Geheimnis hat Vorrang: zuerst widerrufen und rotieren, dann die Ausnahme (wenn überhaupt) setzen.
2. Die Begründung als Kommentar über die Zeile schreiben.
3. Die Ausnahme per PR eintragen und den PR prüfen. Eine Ausnahme ohne Begründung ist nicht zulässig.

Der Eintrag bindet an Commit, Datei, Regel und Zeile. Ändert sich die Zeile, greift die Ausnahme nicht mehr.

Bestehende Einträge: Die eingebettete OpenAPI-Datei in `internal/api/gen/api.gen.go` und das feste Passwort des Acceptance-Test-Clusters AS-01 in `test/acceptance/as01/main_test.go`.
