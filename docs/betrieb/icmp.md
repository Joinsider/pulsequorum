# ICMP-Monitore im Container

ICMP-Checks (Ping) brauchen ein Socket, das normale Prozesse nicht ohne Weiteres öffnen dürfen. Pulsequorum unterstützt zwei Modi (Pflichtenheft FA-CHK-31):

| Modus | Socket | Voraussetzung |
|-------|--------|---------------|
| `unprivileged` (empfohlen) | UDP-ICMP-Socket | Die Gruppe des Prozesses liegt in `net.ipv4.ping_group_range` |
| `privileged` | Raw-Socket | Effektive Capability `CAP_NET_RAW` |

Einstellung über `ICMP_MODE` (`auto` | `unprivileged` | `privileged` | `off`, Standard `auto`). Beim Start pingt der Node `127.0.0.1` und protokolliert den nutzbaren Modus (`ICMP mode`). Bei `auto` wird zuerst `unprivileged`, dann `privileged` versucht. Klappt keiner, meldet der Node eine Warnung; ICMP-Monitore werden ihm dann nicht zugewiesen (Capability `icmp`, ab Phase 2), auf einem Single-Node melden sie DOWN mit Hinweis auf dieses Dokument.

Das Image läuft als `nonroot` (UID/GID 65532). Deshalb ist **`unprivileged` der empfohlene Weg**: `NET_RAW` hilft einem Nicht-Root-Prozess nur, wenn die Capability auch effektiv ist, was im distroless-Image ohne Datei-Capabilities nicht der Fall ist.

## Docker / Docker Compose

```yaml
services:
  uptime:
    image: ghcr.io/joinsider/pulsequorum:latest
    sysctls:
      net.ipv4.ping_group_range: "65532 65532"
```

`ping_group_range` ist ein Namespace-Sysctl und lässt sich pro Container ohne zusätzliche Rechte setzen. Aktuelle Docker-Versionen setzen für Container bereits einen offenen Bereich (`0 2147483647`); die explizite Angabe macht die Konfiguration unabhängig davon.

## Podman (auch rootless)

```bash
podman run --sysctl net.ipv4.ping_group_range="65532 65532" ghcr.io/joinsider/pulsequorum:latest
```

Podman setzt standardmäßig `net.ipv4.ping_group_range=0 0` (nur Gruppe 0); ohne die Option funktioniert `unprivileged` mit dem `nonroot`-Image daher nicht. Dauerhaft lässt sich der Wert in `containers.conf` unter `default_sysctls` hinterlegen.

## Kubernetes

```yaml
spec:
  securityContext:
    sysctls:
      - name: net.ipv4.ping_group_range
        value: "65532 65532"
```

`net.ipv4.ping_group_range` gehört zu den „sicheren“ Sysctls und braucht keine Freigabe am Kubelet.

## Prüfen

Im Log nach dem Start:

```
level=INFO msg="ICMP mode" mode=unprivileged
```

oder bei Problemen eine Warnung `ICMP checks are unavailable on this node` mit der Ursache je Modus.

> Hinweis: Die Angaben zu Docker-, Podman- und Kubernetes-Standardwerten stammen aus der Recherche (Pflichtenheft PF-17) und sind im Projekt noch nicht in allen drei Umgebungen praktisch verifiziert (AP-1.5).
