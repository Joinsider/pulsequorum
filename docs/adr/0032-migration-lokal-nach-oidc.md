# ADR 0032: Migration von `local` nach `oidc`

|                 |                                                                           |
|-----------------|---------------------------------------------------------------------------|
| **Status**      | Accepted                                                                  |
| **Datum**       | 2026-11-18                                                                |
| **Entscheider** | Projektinhaber                                                                  |
| **Bezug**       | FA-AUTH-08, FA-AUTH-10, FA-DAT-01, ADR 0019, ADR 0027, ADR 0029, ADR 0031 |

## Kontext

FA-AUTH-08 verlangt: Beim Wechsel von `AUTH_MODE=local` zu `oidc` bleiben die Daten des Mandanten `default` erhalten. Im
Profil `zitadel` wird `default` der Plattform-Organisation zugeordnet. ADR 0027 hat die Übernahme lokaler Konten auf
AP-4.5 verschoben.

**Bestand (Sichtung vom 2026-11-18):**

- **Daten:** Monitore, Kanäle, Wartungsfenster, Incidents und Heartbeats hängen nur am Mandanten, nicht an einem Nutzer.
  Es gibt keine Spalte `created_by` an Mandantendaten. Ein Wechsel des Auth-Modus ändert sie nicht. Der Modus selbst
  steht nirgends im State, nur in der Konfiguration.
- **Bindung von `default`:** Im Profil `zitadel` bindet der Leader `default` beim Start an `ZITADEL_PLATFORM_ORG_ID`
  (`bindPlatformOrg` in `cmd/uptimecluster/oidc.go`). Mit `OIDC_TENANCY=single` landet die Plattform-Organisation
  ohnehin in `default`. Im Profil `generic` landet jeder Nutzer in `default`. Der Kern von FA-AUTH-08 ist damit gebaut,
  aber nicht getestet: Es gibt keinen Test für einen Wechsel auf einer bestehenden Datenbank und keinen für
  `tryBindPlatformOrg`.
- **Lokale Konten (`users`):** bleiben im Modus `oidc` liegen und werden ignoriert. Anmeldung, Refresh und
  Nutzerverwaltung antworten 409. Die Konten dienen als Notzugang über die Recovery-CLI (ADR 0019,
  `docs/betrieb/recovery.md`).
- **Refresh-Sitzungen (`refresh_sessions`):** werden beim Wechsel nicht widerrufen. Sie laufen gleitend bis zu 14 Tage.
  Wird später für einen Notfall auf `local` zurückgeschaltet, gelten Refresh-Cookies aus der Zeit vor der Migration
  wieder. Ein Mensch, dem man im IdP den Zugang entzogen hat, käme so über ein altes Cookie zurück.
- **Identität:** Lokal ist der Akteur die UUID aus `users.id`, im Modus `oidc` das `sub`. Audit-Log
  (`audit_log.actor_*`) und API-Tokens (`api_tokens.created_by_*`) speichern Art, ID und Namen. Derselbe Mensch hat nach
  dem Wechsel eine andere ID.
- **API-Tokens (`uct_…`):** gehören dem Mandanten (ADR 0031) und gelten in jedem Auth-Modus. Ob es den anlegenden Nutzer
  noch gibt, wird nicht geprüft.
- **Weitere Mandanten aus dem lokalen Modus** (über `/admin/tenants` angelegt): Mit `zitadel`/`single` und mit `generic`
  sind sie nicht mehr erreichbar. Mit `zitadel`/`multi` kann ein `platform-admin` sie über `PATCH /admin/tenants/{id}`
  mit `orgId` an eine Organisation binden.
- **Falsch gebundenes `default`:** Ist `default` schon an eine andere Organisation gebunden, protokolliert der Leader
  eine Warnung und gibt auf. FSM und Admin-API lehnen ein Umbinden von `default` ab („set by the server configuration“).
  Der einzige Weg zurück ist die alte `ZITADEL_PLATFORM_ORG_ID`.

Zu entscheiden ist, wie mit Konten, Sitzungen, Tokens, Audit-Identitäten und Bindungsfehlern umgegangen wird und was
AP-4.5 liefert.

## Optionen

### Frage 1: Lokale Konten und Sitzungen

**1A: Status quo.** Konten und Sitzungen bleiben unverändert.

- Vorteile: kein Code.
- Nachteile: Nach einem Rückwechsel auf `local` gelten alte Refresh-Cookies bis zu 14 Tage weiter. Der Rückwechsel ist
  der dokumentierte Notzugang, also kein exotischer Fall.

**1B: Konten behalten, Sitzungen widerrufen (Empfehlung).** Läuft der Leader im Modus `oidc`, widerruft er alle noch
aktiven Refresh-Sitzungen. Er prüft das beim Start und danach alle 30 s, damit auch Sitzungen erfasst werden, die
während eines gemischten Rolling-Restarts auf Votern im Modus `local` entstehen. Dafür nutzt er den bestehenden
FSM-Befehl `RevokeSessions` mit `user_id` (`NewRevokeUserSessions`), einmal je betroffenem Nutzer, als System-Akteur.

- Vorteile: Kein neuer FSM-Befehl, keine neue Befehlsstufe. Nach einem Rückwechsel braucht jeder lokale Nutzer ein
  Passwort, ein altes Cookie reicht nicht. Der Notzugang über `reset-password` bleibt.
- Nachteile: Ein Rückwechsel meldet alle lokalen Nutzer ab (gewollt). Während des Rolling-Restarts gibt es ein kurzes
  Fenster mit gemischten Modi.

**1C: Konten löschen.**

- Vorteile: kein Altbestand.
- Nachteile: Der Notzugang über die Recovery-CLI entfällt. Ein Rückweg wäre nur noch über ein Backup möglich.
  Unumkehrbar.

### Frage 2: Identität im Audit-Log

**2A: Keine Verknüpfung (Empfehlung).** Alte Einträge behalten UUID und Nutzernamen, neue tragen das `sub`. Die
Betriebsdoku nennt das.

- Vorteile: Das Audit-Log bleibt unverändert und nur anhängend.
- Nachteile: Für eine Person gibt es zwei Akteur-IDs. Suchen über die Zeit des Wechsels hinweg gehen über den Namen.

**2B: Zuordnungstabelle lokale ID → `sub`,** die ein `platform-admin` pflegt.

- Vorteile: durchgehende Identität.
- Nachteile: neuer FSM-Befehl, neue Stufe, eine Pflegeaufgabe für Betreiber. Eine falsche Zuordnung verfälscht die
  Nachvollziehbarkeit. Der Nutzen ist klein.

### Frage 3: API-Tokens lokaler Nutzer

**3A: Gültig lassen (Empfehlung).** Tokens gehören dem Mandanten (ADR 0031), nicht der Person. Beim Start im Modus
`oidc` protokolliert der Leader einmal die Zahl aktiver Tokens, die ein lokaler Nutzer angelegt hat. Ein `tenant-admin`
kann sie über `/api-tokens` widerrufen.

- Vorteile: Automatisierungen laufen während der Migration weiter. Es passt zum Modell aus ADR 0031.
- Nachteile: Ein Token einer Person, die es im IdP nicht gibt, bleibt bis zum Ablauf gültig (höchstens 366 Tage).

**3B: Beim Wechsel widerrufen.**

- Vorteile: Nach dem Wechsel gibt es nur noch Zugänge aus dem IdP.
- Nachteile: Alle Automatisierungen brechen gleichzeitig ab. Der Widerruf ist unumkehrbar. Wer das will, kann es mit 3A
  gezielt selbst tun.

### Frage 4: `default` ist an eine andere Organisation gebunden

**4A: Status quo, klarer melden (Empfehlung für AP-4.5).** Der Leader protokolliert den Fehler als `error` statt `warn`
und nennt die Abhilfe (alte `ZITADEL_PLATFORM_ORG_ID`). `/health/ready` bleibt unberührt. Die Betriebsdoku beschreibt
den Fall. Das Umbinden von `default` auf eine neue Plattform-Organisation bleibt außerhalb von FA-AUTH-08.

- Vorteile: keine Änderung an einer Sicherheitsgrenze.
- Nachteile: Ein Betreiber, der die Plattform-Organisation wechseln will, kann es nicht.

**4B: Opt-in zum Umbinden.** Mit `ZITADEL_REBIND_DEFAULT=true` bindet der Leader `default` einmal auf die konfigurierte
Organisation um, als System-Akteur und mit Audit-Eintrag. Das verlangt eine neue FSM-Regel und damit Stufe 10.

- Vorteile: Ein Wechsel der Plattform-Organisation wird möglich.
- Nachteile: Größere Angriffsfläche. Wer die Konfiguration eines Voters ändern kann, kann die Plattform-Rechte einer
  anderen Organisation geben (das kann er heute über `ZITADEL_PLATFORM_ORG_ID` bei ungebundenem `default` allerdings
  auch). Haben Voter verschiedene Werte, kann die Bindung mit dem Leader wechseln. Das gehört in einen eigenen ADR, wenn
  der Bedarf konkret wird.

### Frage 5: Weitere Mandanten aus dem lokalen Modus

**5A: Dokumentieren und warnen (Empfehlung).** Beim Start im Modus `oidc` protokolliert der Leader einmal die Mandanten
ohne Organisation, außer `default` und `_system`. Bei `zitadel`/`multi` nennt die Meldung `PATCH /admin/tenants/{id}`
mit `orgId`; bei `single` und `generic` sagt sie, dass diese Mandanten nicht erreichbar sind. Die Daten bleiben
unverändert.

- Vorteile: kein neuer Pfad, nutzt die Admin-API aus ADR 0029.
- Nachteile: Lokale Nutzer dieser Mandanten werden nicht übernommen. Ihre Konten in Zitadel legt der Betreiber selbst
  an.

**5B: Assistent in der SPA,** der lokale Nutzer pro Mandant auflistet und Organisationen in Zitadel anlegt.

- Vorteile: bequemer.
- Nachteile: Er bräuchte Schreibrechte in der Management-API von Zitadel. Das ist eine neue Integration mit eigenem
  Sicherheitsmodell und übersteigt AP-4.5 deutlich.

## Entscheidung

Vorgeschlagen: **1B, 2A, 3A, 4A, 5A.**

Begründung: FA-AUTH-08 ist im Kern gebaut und braucht vor allem Tests und eine Anleitung. Die einzige Änderung mit
Sicherheitswirkung ist 1B. Sie schließt den Weg, über den alte Refresh-Cookies nach einem Rückwechsel wieder gelten, und
braucht dafür weder einen neuen Befehl noch eine neue Stufe. Die anderen Punkte bleiben beim bestehenden Modell (Tokens
gehören dem Mandanten, das Audit-Log ist nur anhängend, die Bindung von `default` kommt aus der Konfiguration) und
werden sichtbar gemacht statt geändert.

**Umfang von AP-4.5:**

1. **Sitzungswiderruf (1B):** Schleife auf dem Leader im Modus `oidc`, Start und alle 30 s. Eine Store-Abfrage liefert
   die Paare (Mandant, Nutzer) mit aktiver Sitzung. Für jedes Paar geht `NewRevokeUserSessions` mit stabiler Request-ID
   durch das FSM. Schreibt der Hauptagent (Sitzungen sind Auth-Code).
2. **Hinweise beim Start (3A, 4A, 5A):** Leader protokolliert einmal die Zahl aktiver Tokens lokaler Nutzer und die
   ungebundenen Mandanten. Bei falsch gebundenem `default` eine Fehlermeldung mit Abhilfe.
3. **Tests:**
    - Unit-Tests für `tryBindPlatformOrg` (ungebunden, gleich, fremd gebunden, Org schon vergeben, Befehlsstufe zu
      klein, kein Leader). Dafür wandert die Funktion aus `cmd/` in ein testbares Paket.
    - Integrationstest: Store mit Daten in `default`, lokalen Nutzern, Sitzungen und einem Token. Ein Node startet im
      Modus `oidc` mit dem Fake-IdP (`internal/auth/oidctest`). Geprüft wird: Daten unverändert sichtbar, `default`
      gebunden, Sitzungen widerrufen, Token gültig, lokaler Access-Token und Refresh-Cookie abgewiesen. Dann zurück auf
      `local`: altes Cookie abgewiesen, Anmeldung mit Passwort möglich.
    - Ende zu Ende gegen `deploy/oidc-dev/zitadel` (`single` und `multi`) und `deploy/oidc-dev/keycloak` (`generic`),
      Protokoll in `docs/betrieb/oidc.md`.
4. **Doku:** neuer Abschnitt „Wechsel von `local` zu `oidc`“ in `docs/betrieb/oidc.md` mit Ablauf (Backup, alle Voter
   umstellen, Rolling-Restart, Prüfen), den Folgen für Konten, Sitzungen, Tokens und Audit-Log, dem Rückweg und dem
   Fall „`default` fremd gebunden“. Verweis aus `recovery.md`.

## Konsequenzen

- **Positiv:**
    - FA-AUTH-08 ist durch Tests belegt, auch der Rückweg.
    - Ein Rückwechsel auf `local` öffnet keine alten Sitzungen mehr.
    - Kein neuer FSM-Befehl, `CommandVersion` bleibt 9.
- **Negativ / Risiken:**
    - Nach einem Rückwechsel sind alle lokalen Nutzer abgemeldet.
    - Während eines Rolling-Restarts kann ein Voter im Modus `local` noch kurz Sitzungen ausstellen. Sie werden
      spätestens 30 s nachdem ein Leader im Modus `oidc` läuft widerrufen. Ist der Leader noch im Modus `local`, wartet
      der Widerruf, bis der Leader wechselt.
    - API-Tokens ehemaliger lokaler Nutzer bleiben gültig, bis ein `tenant-admin` sie widerruft oder sie ablaufen.
    - Die Audit-Identität einer Person bricht am Wechsel.
    - Die Plattform-Organisation lässt sich weiterhin nicht wechseln (4B bleibt offen).
- **Folgearbeiten:**
    - Oberfläche für ungebundene Mandanten und Tokens lokaler Nutzer in AP-4.6.
    - Eigener ADR für 4B, falls der Wechsel der Plattform-Organisation gebraucht wird.

## Approval

| Datum      | Status   | Durch    | Anmerkung                                                                            |
|------------|----------|----------|--------------------------------------------------------------------------------------|
| 2026-11-18 | Proposed | Claude   |                                                                                      |
| 2026-11-18 | Accepted | Projektinhaber | im Chat angenommen („Ja, so annehmen“: 1B, 2A, 3A, 4A, 5A), eingetragen durch Claude |
