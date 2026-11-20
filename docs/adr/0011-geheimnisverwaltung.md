# ADR 0011: Verschlüsselung von Geheimnissen (Envelope Encryption)

|                 |                                                                |
|-----------------|----------------------------------------------------------------|
| **Status**      | Accepted                                                       |
| **Datum**       | 2026-10-19                                                     |
| **Entscheider** | Projektinhaber                                                       |
| **Bezug**       | FA-SEC-SEC, FA-ALR-09, FA-AUTH-02, FA-PKI-01, O-05, Konzept 15 |

## Kontext

Kanal-URLs, Webhook-Header, JWT-Signaturschlüssel und der CA-Schlüssel liegen im Raft-Zustand und damit in jeder
SQLite-Datei, jedem Snapshot und jedem Export. Sie sollen verschlüsselt sein; der Schlüssel muss auf allen Votern
verfügbar sein und rotiert werden können. AES-GCM mit zufälligen Nonces ist bis ~2³² Verschlüsselungen pro Schlüssel
sicher – für unser Volumen unkritisch, aber pro Schlüssel zu begrenzen.

## Optionen (Herkunft des Master-Keys / KEK)

### Option A: Extern bereitgestellt (`MASTER_KEY_FILE`/`MASTER_KEY`/später KMS), identisch auf allen Votern

- Vorteile: Datenbank-Diebstahl (Volume, Backup) allein gibt keine Geheimnisse preis.
- Nachteile: Betreiber muss den Schlüssel sicher auf alle Voter verteilen; Verlust = Geheimnisse verloren.

### Option B: Beim Bootstrap erzeugt, beim Join über mTLS an neue Voter übertragen, als Datei (0600) im Datenverzeichnis

- Vorteile: Null Aufwand für Betreiber.
- Nachteile: Schützt nur Exporte/Snapshots außerhalb des Datenverzeichnisses, nicht ein gestohlenes Volume.

## Entscheidung

**Beides, mit Option B als Standard und Option A als empfohlener Härtung:**

- Ist `MASTER_KEY_FILE` oder `MASTER_KEY` gesetzt, wird dieser KEK verwendet (Option A). Für den kommerziellen Betrieb
  ist das Pflicht (Startwarnung im Zitadel-Modus, wenn nicht gesetzt).
- Sonst erzeugt der erste Voter einen KEK in `DATA_DIR/master.key` (0600) und überträgt ihn beim Join verschlüsselt über
  mTLS (Option B).
- Der KEK-Fingerprint liegt im Raft-Zustand; ein Voter mit abweichendem KEK verweigert den Start mit klarer Meldung.

Verfahren:

- **DEK:** 256-Bit-Datenschlüssel je Epoche, mit dem KEK per AES-256-GCM gewrappt und im Raft-Zustand gespeichert
  (`secret_keys(key_id, wrapped_dek, created_ms, retired_ms)`).
- **Chiffrat:** `v1 | key_id | nonce(12) | ciphertext+tag`; Additional Data = `<tabelle>/<objekt-id>/<feld>` (verhindert
  Umkopieren zwischen Datensätzen).
- **Rotation:** neuer DEK per Raft-Befehl (automatisch jährlich oder manuell); Hintergrundjob verschlüsselt Bestände
  neu; alter DEK wird nach vollständiger Umschlüsselung stillgelegt. KEK-Rotation: alle DEKs neu wrappen (Admin-CLI).
- **Nur Standardbibliothek** (`crypto/aes`, `crypto/cipher`, `crypto/rand`).
- Entschlüsselte Werte nie in Logs (`slog.LogValuer`-Maskierung) und nie in API-Antworten (nur maskiert, z.B.
  `telegram://***@telegram?chats=…`).

## Konsequenzen

- Positiv: Geheimnisse sind in Exporten und Snapshots geschützt; mit externem KEK auch bei Volume-Diebstahl.
- Negativ: Ohne externen KEK nur begrenzter Schutz (dokumentiert); KEK-Verlust bei Option A ist nicht behebbar.
- Folgearbeiten: Admin-CLI für KEK-Rotation (P2), KMS-Anbindung (später).

## Approval

| Datum      | Status   | Durch    | Anmerkung                                 |
|------------|----------|----------|-------------------------------------------|
| 2026-10-19 | Proposed | Claude   |                                           |
| 2026-10-19 | Accepted | Projektinhaber | im Chat für alle ADRs 0001–0011 bestätigt |
