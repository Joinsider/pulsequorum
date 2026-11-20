# ADR 0038: Backup, Export und Restore

|                 |                                                                                                                                                                                                  |
|-----------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **Status**      | Proposed                                                                                                                                                                                         |
| **Datum**       | 2026-11-19                                                                                                                                                                                       |
| **Entscheider** | Projektinhaber                                                                                                                                                                                         |
| **Bezug**       | FA-OBS-03, Z-S-04, FA-CLU-02, FA-CLU-09, FA-SEC-SEC, FA-TEN-01, FA-DAT-01, NFA-MNT-04, PF-01, PF-08, PF-12; ADR 0004, 0005, 0011, 0012, 0013, 0015, 0016, 0018, 0019, 0029, 0031; AP-5.4, AP-5.5 |

## Kontext

FA-OBS-03 verlangt einen **konsistenten Export** (Konfiguration und optional Messdaten) über API und CLI und einen
**Restore nur in einen frischen Cluster**. Z-S-04 nennt Export und Import aller Daten auch für einen Wechsel des
Backends (PostgreSQL, AP-5.4).

Was es schon gibt:

- **Zwei Dateien je Voter** (ADR 0004): `state.db` (Datenklasse A, nur die FSM schreibt) und `metrics.db` (Datenklasse
  B, lokal je Node, Rollups lokal berechnet, Heartbeats mit Schlüssel `(node_id, incarnation, seq)`).
- **Raft-Snapshots** sind `VACUUM INTO` von `state.db` (`StateStore.WriteSnapshot`), Restore per Dateitausch nach
  `PRAGMA quick_check` (`RestoreSnapshot`). Der angewandte Index steht in `fsm_meta` in derselben Datei. `Fingerprint`
  hasht alle replizierten Tabellen.
- **Geheimnisse** (ADR 0011, 0015): AES-256-GCM mit DEKs je Epoche (`secret_keys`, gewrappt mit dem KEK,
  `kek_fingerprint`). Format `v1|key_id|nonce|ct`, AAD enthält Tabelle, Objekt-ID und Feld
  (`notification_channels.url_enc`, Webhook-Header, `monitors.secrets_json`, `signing_keys.private_key_enc`,
  `cluster_ca.key_enc`). Der KEK liegt nie in der Datenbank: entweder extern (`MASTER_KEY_FILE`/`MASTER_KEY`) oder
  generiert in `DATA_DIR/master.key`.
- **Hashes statt Geheimnisse**: Passwörter (Argon2id), Refresh-Sitzungen, API-Tokens (SHA-256), Join-Tokens.
- **Cluster-Identität** im Raft-Zustand: CA (`cluster_ca`), Node-Register, Inkarnationen, gesperrte Nodes und
  Zertifikate, Join-Tokens.
- **Recovery-CLI** (ADR 0019): `uptimecluster admin …`, Online-Befehle über den Cluster-Port mit dem Voter-Zertifikat,
  Offline-Befehle bei gestopptem Node, Übergabedateien im `DATA_DIR` (`recovery-revoke.json`).
- **`GetStatus`** liefert `applied_index` und `commit_index` jedes Nodes.
- Eine Lesesperre über `metrics.db` darf nicht lange offen bleiben (ADR 0013 C4, WAL-Wachstum).
- Befehlsstufen und Expand/Contract (ADR 0018): Während eines Rolling Updates laufen zwei Binary-Versionen
  nebeneinander.

Was andere Systeme tun (Recherche):

- **Consul/Vault** (hashicorp/raft wie wir): `snapshot save` liefert einen Point-in-Time-Snapshot des Raft-Zustands.
  Restore läuft auf dem Leader über `raft.Restore`. Laut Doku-Kommentar übernimmt `Restore` die **aktuelle**
  Raft-Konfiguration statt der aus dem Snapshot, damit ein Restore in einen neuen Cluster möglich ist. Es lässt eine
  Lücke im Log, damit Follower und neue Nodes den Snapshot per InstallSnapshot erhalten. Gedacht ist das für Disaster
  Recovery in einen frischen Cluster. Vault verlangt `-force`, wenn der Snapshot mit anderen Schlüsseln versiegelt ist.
- **etcd**: `etcdctl snapshot save` von einem Member mit Integritäts-Hash. `etcdutl snapshot restore` schreibt in ein
  **neues, leeres** Datenverzeichnis und vergibt **neue Member- und Cluster-IDs**, damit alte Member nicht versehentlich
  beitreten. Ein Restore startet immer einen neuen logischen Cluster.
- **age** (`filippo.io/age`, BSD-3-Clause, Format-Spezifikation age-encryption.org/v1, aktuelle Linie v1.3.x):
  Datei-Verschlüsselung mit X25519-Empfängern (auch hybrid Post-Quantum) oder Passphrase (scrypt, Work-Factor beim
  Entschlüsseln begrenzbar). Gestreamt in Blöcken mit STREAM-AEAD, Kürzung und Manipulation werden erkannt. Die Doku rät
  von Passphrasen für automatisierte Abläufe ab.

## Optionen

### Frage 1: Was ist ein Backup?

**1A Physischer Raft-Snapshot (Consul-Stil).** Das Backup ist die `VACUUM INTO`-Kopie von `state.db` mit Metadaten. Der
Restore spielt sie per `raft.Restore` ein.

- Vorteile: Code existiert fast vollständig. Exakt der replizierte Zustand, Fingerprint vergleichbar.
- Nachteile:
    - Die Kopie enthält die **Cluster-Identität**: den CA-Schlüssel, das Node-Register und die Join-Tokens. Ein
      wiederhergestellter Cluster vertraut damit denselben Node-Zertifikaten wie der alte. Laufen beide, können alte
      Nodes mit dem neuen sprechen (PF-01, PF-06).
    - Die DEKs sind mit dem alten KEK gewrappt. Ohne den alten `master.key` sind alle Geheimnisse verloren, mit ihm
      steht das Backup und der Schlüssel oft auf demselben Volume.
    - Das Format ist das SQLite-Schema einer bestimmten Migrationsversion. Es taugt nicht für den Wechsel zu PostgreSQL
      (Z-S-04) und nicht für den Export eines einzelnen Mandanten.
    - Messdaten (`metrics.db`) sind nicht enthalten.

**1B Physische Kopie beider Dateien, Restore offline (etcd-Stil).** `VACUUM INTO` von `state.db` und `metrics.db`,
Restore schreibt ein neues `DATA_DIR` und tauscht die Identität aus.

- Vorteile: Messdaten vollständig. Neue Identität wie bei etcd.
- Nachteile: Die Probleme von 1A beim Format bleiben. `metrics.db` ist bis zu einige GB groß und braucht beim Export
  doppelten Platz. Rollups sind lokal je Node; eine Kopie auf alle Voter verteilt Daten, die dort nie berechnet wurden.
  Ein offline geschriebenes `state.db` passt nicht zu einem leeren Raft-Log: Follower, die aus dem Log aufholen statt
  per Snapshot, sähen die Daten nicht.

**1C Logischer, versionierter Export, Restore über eine gebaute Snapshot-Datei und `raft.Restore`.** ← vorgeschlagen

- Export: ein NDJSON-Strom mit einem Datensatz je Objekt, eigene Datentypen im neuen Paket `internal/backup`, unabhängig
  vom SQLite-Schema. Die Quelle ist eine `VACUUM INTO`-Kopie von `state.db` (Konsistenzpunkt, siehe Frage 3).
- Restore: Der Leader des frischen Clusters baut eine neue `state.db`. Er nimmt seine **eigene** Identität (CA, Nodes,
  KEK-gewrappte DEKs, Befehlsstufe) und fügt die Fachdaten aus dem Backup ein. Die Datei spielt er per `raft.Restore`
  ein. Follower und neue Nodes erhalten sie wegen der Lücke im Log per InstallSnapshot.
- Vorteile: Identität und Fachdaten sind getrennt. Ein Format für Backup, Backend-Wechsel und später Mandanten-Export.
  Ältere Formatversionen bleiben lesbar, unabhängig von Migrationen. Für PostgreSQL schreibt derselbe Importer direkt in
  eine Transaktion.
- Nachteile: Ein eigener Export- und Importcode je Tabelle, der bei jeder Schemaänderung mitwachsen muss. Dagegen hilft
  ein Test, der jede Tabelle aus `sqlite_master` einer Regel zuordnet: exportiert oder bewusst ausgeschlossen.

**1D Logischer Export, Import über normale Raft-Befehle** (`PutEntity`, `Batch`).

- Vorteile: Keine Sonderwege in der FSM, jeder Datensatz durchläuft die Validierung.
- Nachteile: Es gibt keine Befehle für Incidents, Statushistorie, Audit-Log, Passwort-Hashes oder Grabsteine. Die FSM
  setzt Zeitstempel und Revisionen neu, Audit-Einträge würden zu Einträgen des Imports. Zehntausende Log-Einträge,
  langsam, und ein Abbruch hinterlässt einen halb importierten Cluster.

### Frage 2: Umfang

**2A Nur Konfiguration** (Mandanten, Pläne, Nutzer, Monitore, Kanäle, Wartungsfenster, API-Tokens).

**2B Konfiguration und Verlauf aus Klasse A immer, Messdaten optional.** ← vorgeschlagen

- Immer dabei: der gesamte fachliche Zustand aus Klasse A, auch Incidents, Statushistorie und Audit-Log.
- Mit `--metrics`: verdichtete Messdaten aus `metrics.db` (`uptime_rollups`, `latency_minutes`, `series`), wahlweise ab
  einem Zeitpunkt.

**2C Zusätzlich Roh-Heartbeats.** Sie gelten nur 48 h, sind an Node-ID und Inkarnation gebunden und machen den Großteil
der Datenmenge aus.

### Frage 3: Konsistenter Zeitpunkt

**3A Export nur auf dem Leader** nach `raft.Barrier`.

- Nachteile: Die API müsste an den Leader weiterleiten, und große Downloads belasten den Leader.

**3B Export auf jedem Voter mit Read-Index.** ← vorgeschlagen

1. Der Node liest den `commit_index` des Leaders, auf dem Leader selbst nach `Barrier`, auf einem Follower per
   `GetStatus` über mTLS.
2. Er wartet, bis sein `applied_index` diesen Wert erreicht hat (Zeitlimit, dann 503).
3. Er erzeugt per `VACUUM INTO` eine Kopie von `state.db` im `DATA_DIR`. Den Export liest er nur aus dieser Kopie.
4. Das Manifest enthält den angewandten Index **aus der Kopie**, die Befehlsstufe und den Fingerprint der Kopie. Der
   Fingerprint lässt sich gegen die Prüfpunkte des Clusters halten (ADR 0025).

- Messdaten sind Klasse B und nie exakt konsistent. Der Export liest sie seitenweise (Keyset, kurze Lesetransaktionen,
  ADR 0013 C4) bis zu einem Stichtag `cutoff_ms` = `last_issued_at_ms` der Kopie. Die Retention kann während des Exports
  die ältesten Zeilen entfernen; das ist dokumentiert.

**3C Eine lange Lesetransaktion über beide Dateien.** Verletzt ADR 0013 C4, weil sie Checkpoints blockiert.

### Frage 4: Geheimnisse und Master-Key

**4A Chiffrate und gewrappte DEKs unverändert exportieren.** Der Betreiber sichert den KEK getrennt.

- Nachteile: Mit generiertem KEK (Standard, ADR 0011 Option B) liegt der Schlüssel im `DATA_DIR`. Geht der Cluster
  verloren, ist er meist mit verloren. Der Restore bräuchte zusätzlich einen Weg, einen fremden KEK zu laden.

**4B DEKs mit einem passphrasenbasierten Schlüssel neu wrappen, eigenes Verfahren** (scrypt oder Argon2id aus `x/crypto`
plus AES-GCM). Der Rest des Exports bleibt unverschlüsselt.

- Nachteile: eigenes Kryptoformat. Passwort-Hashes, Token-Hashes, E-Mail-Adressen, interne Monitor-Ziele und das
  Audit-Log lägen im Klartext in der Datei.

**4C Gesamten Export mit age verschlüsseln, DEKs innen ungewrappt, Chiffrate unverändert.** ← vorgeschlagen

- Das age-Format ist die Hülle. Die DEKs werden im Export **mit age neu verpackt** statt mit dem KEK. Jeder Wert bleibt
  das Chiffrat aus `state.db`, mit unveränderter `key_id` und AAD. Beim Export entsteht kein Klartext eines
  Geheimnisses, nur die DEKs liegen kurz im Speicher.
- Empfänger:
    - **X25519-Empfänger** (`age1…`, empfohlen, auch hybrid PQ). Der Cluster kann Backups erzeugen, aber nicht lesen.
      Das passt zu automatisierten Backups.
    - **Passphrase** (scrypt, nur CLI, mindestens 16 Zeichen, Eingabe wie bei `reset-password`). Beim Entschlüsseln wird
      der scrypt-Work-Factor begrenzt.
- Restore: Die DEKs werden mit dem KEK **des neuen Clusters** gewrappt und mit denselben `key_id`s gespeichert. Der alte
  KEK wird nie gebraucht.
- Verschlüsselung ist **Pflicht**, ein unverschlüsselter Export ist nicht vorgesehen.

**4D Geheimnisse im Klartext exportieren.** Ausgeschlossen (FA-SEC-SEC, ADR 0015).

### Frage 5: Restore nur in einen frischen Cluster

**5A Nur dokumentieren.**

**5B Im Server erzwingen, neue Cluster-Identität.** ← vorgeschlagen

Der Leader nimmt einen Restore nur an, wenn alle folgenden Bedingungen gelten:

- Die Raft-Konfiguration hat **genau einen Voter**, nämlich ihn selbst, und im Register steht kein weiterer Node.
- Es gibt **keine Fachdaten**: nur die beim Bootstrap angelegten Mandanten `default` und `_system`, nur die
  Standardpläne, keine Nutzer, Monitore, Kanäle, Wartungsfenster, API-Tokens und Incidents. Im lokalen Modus muss der
  Restore deshalb vor dem Setup laufen.
- In `fsm_meta` steht kein Restore-Marker (`restored_from`).
- Der Aufruf enthält die Bestätigung `--yes`. Ohne sie zeigt die CLI nur einen Dry-Run: Manifest, Mengen je Typ,
  Quellcluster, Zeitpunkt.

Die Identität des frischen Clusters bleibt: CA, Node-ID, KEK und das eigene Bootstrap-DEK. Wie bei etcd können Nodes des
alten Clusters deshalb weder beitreten noch per mTLS sprechen. Nicht übernommen werden: CA, Node-Register,
Inkarnationen, gesperrte Nodes und Zertifikate, Join-Tokens, Refresh-Sitzungen (alle melden sich neu an),
Signaturschlüssel, Request-Dedup, Alert-Outbox, unbestätigte Alerts und aktive Systemalerts. Die Begründung steht in der
Tabelle unter „Entscheidung“.

**5C Restore auch in laufende Cluster (Consul-Stil).** Damit könnte man einen Fehler zurückrollen. `raft.Restore` lässt
den Leader aber vor seinen Followern committen. Bestehende Sitzungen, Outbox und Node-Register müssten zusammengeführt
werden. Das ist für den Rollback nach einem Update (ADR 0018) verlockend, aber nicht gefordert.

### Frage 6: Wer darf exportieren?

**6A Nur `platform-admin`, immer der ganze Cluster.** ← vorgeschlagen für AP-5.5

**6B Zusätzlich `tenant-admin` für den eigenen Mandanten.** Ohne Geheimnisse, weil die DEKs clusterweit gelten und ein
Mandant sie nie erhalten darf (Tenant-Grenze). Geheime Felder kämen als `***`. Kanäle wären damit ohne URL, Import nur
in einen leeren Mandanten. Sinnvoll für Mandantenumzüge, aber eigener Import-Pfad und eigene Tests der Tenant-Grenze. →
offene Frage, nicht in AP-5.5.

### Frage 7: API oder CLI

**7A Nur CLI.** **7B Nur API.** **7C Export über API und CLI, Restore nur über CLI.** ← vorgeschlagen

- Restore setzt Zugriff auf den Host voraus. Wer den hat, kontrolliert den Node ohnehin (wie bei ADR 0019). Ein
  Restore-Endpunkt in der öffentlichen API wäre eine neue Angriffsfläche für eine einmalige Aktion.

## Entscheidung

**1C, 2B, 3B, 4C, 5B, 6A, 7C.** Format und Verschlüsselung mit age.

**1. Format `uptimecluster-backup`, Version 1**

- Datei `*.ucbak` = age (gzip (NDJSON ) ). Nur `compress/gzip` und `encoding/json` aus der Standardbibliothek, dazu
  `filippo.io/age`.
- Zeile 1 ist der **Kopf**: `format`, `format_version` (ganzzahlig), Programmversion, `backup_id` (UUIDv7),
  `created_ms`, Quell-Node, `applied_index`, `command_level`, Fingerprint, Auth-Modus, Umfang (`metrics`,
  `metrics_from_ms`, `cutoff_ms`).
- Danach folgen die Datensätze `{"t":"<typ>","v":<typversion>,"d":{…}}` in fester Reihenfolge: `secret_key` (DEK roh,
  `key_id`, `created_ms`, `retired_ms`), dann `plan`, `tenant`, `deleted_tenant`, `user`, `notification_channel`,
  `monitor` (mit Tags und `secrets` als Chiffrat), `monitor_channel`, `maintenance_window`, `api_token`, `incident`,
  `status_history`, `audit`. Mit `--metrics` folgen `series`, `uptime_rollup` und `latency_minute`.
- Die letzte Zeile ist der **Abschluss**: Anzahl je Typ und SHA-256 über alle vorherigen Zeilen. Fehlt der Abschluss
  oder stimmt er nicht, scheitert der Restore.
- Die age-Hülle erkennt Manipulation und Kürzung (authentifizierte Blöcke). Der Abschluss erkennt einen abgebrochenen
  Export auf Erzeugerseite. Eine Signatur über den Ursprung gibt es in Version 1 nicht (offene Frage 4).
- **Versionierung:** Ein Binary schreibt immer seine `format_version` und liest alle Versionen ab 1 bis zu seiner
  eigenen. Eine neuere Version lehnt es mit der Meldung „mindestens Version … nötig“ ab. Unbekannte Felder oder Typen
  sind ein Fehler, damit nichts still verloren geht. Neue Felder erhöhen die Typversion, neue Typen die
  `format_version`. Während eines Rolling Updates schreibt jeder Voter seine eigene Version. Der Restore läuft deshalb
  mit einem Binary, das mindestens so neu ist wie der Exporteur, typischerweise dem neuesten.

**2. Was wird übernommen?** Ein Test ordnet jede Tabelle aus `sqlite_master` beider Dateien genau einer Zeile zu. Eine
neue Tabelle ohne Eintrag lässt den Test fehlschlagen.

| Tabelle                                                                                                                                                                                                                              | Behandlung                                                               |
|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------|
| `tenants`, `plans`, `deleted_tenants`, `users`, `monitors`, `monitor_tags`, `notification_channels` (auch Systemkanäle), `monitor_channels`, `maintenance_windows`, `api_tokens`, `incidents`, `monitor_status_history`, `audit_log` | exportiert, IDs und Revisionen bleiben (AAD der Chiffrate bleibt gültig) |
| `secret_keys`                                                                                                                                                                                                                        | DEKs roh in der age-Hülle, beim Restore mit dem neuen KEK gewrappt       |
| `monitor_states`                                                                                                                                                                                                                     | nicht exportiert, Monitore starten nach dem Restore in `pending`         |
| offene Incidents                                                                                                                                                                                                                     | beim Restore zum `cutoff_ms` geschlossen, Kennzeichnung `restored`       |
| `cluster_ca`, `nodes`, `node_incarnations`, `revoked_nodes`, `revoked_certs`, `join_tokens`                                                                                                                                          | nie, neue Identität                                                      |
| `signing_keys`, `refresh_sessions`                                                                                                                                                                                                   | nie, neue Anmeldung nötig                                                |
| `request_dedup`, `alert_outbox`, `unconfirmed_alerts`, `system_alerts`, `fsm_meta`                                                                                                                                                   | nie, flüchtig oder clusterbezogen                                        |
| `metrics.db`: `series`, `uptime_rollups`, `latency_minutes`                                                                                                                                                                          | optional (`--metrics`)                                                   |
| `metrics.db`: `heartbeats`, `minutes`, `rollups` (alt), `stream_positions`, `degraded_alerts`, `purged_tenants`, `meta`                                                                                                              | nie                                                                      |

**3. Export**

- **CLI:**
  `uptimecluster admin backup --out <datei> (--recipient age1… | --recipients-file <f> | --passphrase-file <f> | Passphrase von stdin) [--metrics [--metrics-since <dauer>]]`.
  Online wie `reset-password`: liest das lokale `DATA_DIR`, holt den `commit_index` über den Cluster-Port (Voter-mTLS)
  und schreibt die Datei mit 0600. `--out -` schreibt auf stdout.
- **API:** `POST /api/v1/admin/backups`, nur `platform-admin`, nicht für API-Tokens. Im Body stehen `recipients` (nur
  X25519, keine Passphrase über HTTP), `includeMetrics` und `metricsSince`. Die Antwort ist ein gestreamter
  `application/octet-stream` mit `Content-Disposition`. Höchstens ein Export gleichzeitig je Node (sonst 429). Die SPA
  bekommt einen Knopf in der Cluster-Ansicht.
- Beide Wege nutzen dieselbe Funktion `backup.Export(ctx, src, w, opts)`. Die `VACUUM INTO`-Kopie liegt als
  `.backup-*.db` im `DATA_DIR` und wird danach gelöscht, auch beim Abbruch.
- Jeder Export erzeugt einen Audit-Eintrag `backup.export` (Akteur, Empfänger-Fingerprints, Umfang, `backup_id`). Dafür
  gibt es den neuen Befehl `RecordBackup` mit Befehlsstufe **12** (ADR 0018; Vergabe der P5-Stufen siehe Roadmap P5).
  Vor dem Anheben der Stufe wird nur per `slog` protokolliert.

**4. Restore**

- **CLI:** `uptimecluster admin restore --in <datei> (--identity-file <f> | --passphrase-file <f> | stdin) [--yes]`,
  online gegen den lokalen Node.
- Ablauf:
    1. Die CLI entschlüsselt und prüft die Datei vollständig: Kopf, Formatversion, Typen, Abschluss. Erst danach berührt
       sie den Cluster. Ohne `--yes` endet sie hier mit dem Dry-Run.
    2. Mit `--yes` streamt sie die **verschlüsselte** Datei und die Identität bzw. Passphrase über den Cluster-Port
       (neuer RPC `RestoreBackup`, nur Voter-Zertifikat, nur der lokale Node) an den Server. Die Identität wird nie als
       Argument übergeben und nie protokolliert.
    3. Der Server prüft die Bedingungen aus 5B.
    4. Er baut eine neue `state.db` als temporäre Datei (0600) im `DATA_DIR`: `VACUUM INTO` der eigenen `state.db`
       (Identität, Befehlsstufe), Fachdaten aus dem Backup in einer Transaktion, DEKs mit dem eigenen KEK gewrappt,
       Marker `restored_from = <backup_id>` in `fsm_meta`. Danach folgen `quick_check` und eine Prüfung der Mengen gegen
       den Abschluss.
    5. Er spielt die Datei per `raft.Restore` ein, schlägt anschließend sofort `RecordBackup{restored}` vor (Audit, und
       der angewandte Index rückt hinter den Snapshot) und löscht die temporäre Datei.
    6. Mit `--metrics` importiert er die Messdaten in die eigene `metrics.db` (siehe 5).
- Danach treten weitere Voter normal mit neuem Join-Token bei und erhalten den Zustand per InstallSnapshot.
- Der Importer arbeitet gegen eine Schnittstelle `backup.Target`, nicht direkt gegen SQLite. Für PostgreSQL (AP-5.4)
  schreibt dieselbe Implementierung in eine Transaktion.

**5. Messdaten nach dem Restore**

- `metrics.db` ist lokal je Node, Rollups werden nicht repliziert. Jeder Voter importiert die Messdaten deshalb selbst:
  auf dem Leader beim Restore, auf jedem weiteren Voter nach dem Beitritt mit
  `uptimecluster admin restore-metrics --in <datei> …`. Das ist rein lokal, ohne Raft.
- Der Befehl prüft, dass `backup_id` dem Marker `restored_from` entspricht, und ist idempotent (Marker in `metrics.db`).
- Serien alter Node-IDs bleiben als historische Serien erhalten. Die Uptime-Berechnung über alle Melder ist davon nicht
  betroffen.
- Ein Voter ohne Import zeigt für die Zeit vor dem Restore keinen Verlauf. `GetStatus` meldet, ob der Import erfolgt
  ist, und die Cluster-Ansicht zeigt einen Hinweis.

**6. Abhängigkeit**

- `filippo.io/age` (BSD-3-Clause, auf der Allow-List von ADR 0010). Gepinnt wird die zum Umsetzungszeitpunkt aktuelle
  v1.3.x-Version. Die transitiven Abhängigkeiten (u. a. `golang.org/x/crypto`, schon vorhanden) prüft `make licenses`.
- Ein eigenes Kryptoformat wird nicht gebaut.

**7. Betrieb**

- Ein Runbook `docs/betrieb/backup.md` beschreibt Export, Ablage, Restore Schritt für Schritt, Messdaten je Voter,
  Auth-Modus und häufige Fehler.
- Es empfiehlt X25519-Empfänger, deren privaten Schlüssel offline liegt, und regelmäßige Restore-Proben.
- Wiederkehrende Backups laufen in Version 1 extern (Cron oder systemd-Timer mit `admin backup`), siehe offene Frage 5.
- Der Restore übernimmt Mandanten-Bindungen an Organisationen (ADR 0027–0029). Der neue Cluster muss deshalb mit
  demselben `AUTH_MODE` und Issuer laufen. Die CLI warnt bei Abweichung zum Kopf.

## Konsequenzen

- **Positiv:**
    - Ein Format für Backup, Backend-Wechsel und später Mandantenumzüge. Unabhängig vom SQLite-Schema und lesbar über
      mehrere Versionen.
    - Kein Klartext-Geheimnis entsteht beim Export. Der alte KEK wird für den Restore nicht gebraucht, damit fällt die
      häufigste Falle weg (Backup ohne Schlüssel).
    - Mit X25519-Empfängern kann ein kompromittierter Cluster seine eigenen Backups nicht lesen.
    - Der frische Cluster behält seine eigene Identität. Alte Nodes können nicht stören (wie etcd, PF-01).
    - Der Restore erreicht alle Voter über den bewährten InstallSnapshot-Pfad, ohne halb importierten Zustand.
- **Negativ / Risiken:**
    - Für jede neue Tabelle braucht es Export- und Importcode oder einen bewussten Ausschluss. Der Zuordnungstest
      erzwingt die Entscheidung, nicht ihre Richtigkeit.
    - Wer die Passphrase oder den age-Schlüssel verliert, verliert das Backup. Das ist Absicht.
    - Ein Backup enthält Passwort- und Token-Hashes. API-Tokens bleiben nach einem Restore gültig.
    - Messdaten müssen je Voter importiert werden. Ein vergessener Import zeigt sich nur als Hinweis.
    - Ein Restore mit einem älteren Binary als dem Exporteur ist nicht möglich.
    - Laufen alter und neuer Cluster parallel, prüfen beide dieselben Ziele und alarmieren doppelt (dokumentiert).
    - `raft.Restore` wird zum ersten Mal genutzt. Die Tests müssen Neustart, Follower-Beitritt und Fingerprints nach dem
      Restore abdecken.
- **Folgearbeiten:**
    - Paket `internal/backup` (Format, Export, Import, Zuordnungstest).
    - CLI `admin backup`, `admin restore`, `admin restore-metrics`.
    - RPC `RestoreBackup`, Befehl `RecordBackup` mit Befehlsstufe.
    - API `POST /admin/backups` und SPA-Knopf.
    - Runbook, Restore-Test (Abnahme FA-OBS-03), STRIDE-Ergänzung.
    - Pflichtenheft FA-OBS-03 präzisieren (Format, Verschlüsselungspflicht, Messdaten je Voter).
    - Nicht Teil dieses ADR: CA-Rotation (FA-PKI-05, ebenfalls AP-5.5), sie bekommt einen eigenen ADR.

### Offene Fragen an Projektinhaber

1. **Mandanten-Export (6B):** Soll ein `tenant-admin` seinen Mandanten exportieren und in einen leeren Mandanten
   importieren können, ohne Geheimnisse? Vorschlag: nicht in AP-5.5, später mit eigenem ADR.
2. **Roh-Heartbeats (2C):** Reichen verdichtete Messdaten, oder sollen die letzten 48 h Roh-Heartbeats optional mit?
   Vorschlag: nicht in Version 1.
3. **Passphrase:** Nur in der CLI zulassen (Vorschlag) oder auch in der API im Body?
4. **Signatur:** Reicht die Integrität der age-Hülle, oder soll ein Backup zusätzlich signiert werden (z. B. Ed25519 mit
   einem Schlüssel des Betreibers)? Vorschlag: nicht in Version 1.
5. **Geplante Backups im Server** (`BACKUP_SCHEDULE`, `BACKUP_DIR`, `BACKUP_RECIPIENTS` auf dem Leader, Rotation der
   Dateien): jetzt, später oder nie? Vorschlag: später. Version 1 läuft extern per Cron.
6. **Restore in einen laufenden Cluster (5C)** als Rollback nach einem missglückten Update: bewusst ausgeschlossen
   lassen?
7. **API-Tokens nach dem Restore:** gültig lassen (Vorschlag, Automatisierung läuft weiter) oder alle widerrufen?

## Approval

| Datum      | Status   | Durch  | Anmerkung |
|------------|----------|--------|-----------|
| 2026-11-19 | Proposed | Claude |           |
