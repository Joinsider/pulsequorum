## Was und warum

<!-- Kurz: was ändert sich, warum, welche Anforderungs-IDs (FA-/NFA-) und ADRs -->

## Prüfpunkte

- [ ] `make lint` und `make test` grün
- [ ] Große Änderung? Dann gibt es einen akzeptierten ADR (CLAUDE.md, Regel 2)
- [ ] Ändert sich, was `Apply` mit einem Raft-Befehl macht (neues Feld mit Wirkung, andere Validierung, anderes Ergebnis)? Dann hat die Payload eine neue Mindestversion in `internal/cluster/versions.go` und `CommandVersion` ist angehoben (ADR 0018)
- [ ] Schemaänderungen nur als Expand (neue Tabellen, nullable Spalten); Entfernen/Umbenennen frühestens ein Release später (NFA-MNT-04, ADR 0018)
- [ ] Roadmap aktualisiert
