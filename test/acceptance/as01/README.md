# Acceptance scenario AS-01 (automated driver)

Automates scenario AS-01 from `docs/pflichtenheft.md`, section 13.2:

> Single-Node per Compose starten, Admin einrichten, HTTP-Monitor anlegen, Ziel abschalten
> → DOWN-Alert über ntfy innerhalb `(retries+1) × Intervall + 30 s`, Incident angelegt, Uptime < 100 %

The node is started separately (Compose or the binary). The driver `as01`
does the rest against the node's base URL.

## Usage

```bash
go run ./test/acceptance/as01 \
  -base http://127.0.0.1:8080 \
  -setup-token TOKEN \
  -callback-host host.containers.internal \
  -interval 20 -retries 1
```

| Flag | Default | Meaning |
|------|---------|---------|
| `-base` | `http://127.0.0.1:8080` | Base URL of the node (the `/api/v1` prefix is added) |
| `-setup-token` | empty | One-time setup token from the node log (`setup_token=`) |
| `-user` | `admin` | Admin username |
| `-password` | empty | Admin password. If empty on a fresh node, a random one is generated and not printed |
| `-listen` | `:18950` | Listen address of the fake ntfy server |
| `-target-listen` | `:18951` | Listen address of the HTTP target |
| `-callback-host` | `host.containers.internal` | Host name under which the node reaches this driver |
| `-interval` | `20` | Monitor interval in seconds (API range 20 to 86400) |
| `-retries` | `1` | Monitor retries (0 to 10) |

Either `-setup-token` (fresh node) or `-password` (existing admin) is required.
If the node already has an admin, the driver logs in with `-password`.

The result is a Markdown table on stdout (step, expected, measured, result)
and an overall verdict. The exit status is 1 if any step fails. The
informational UP notification never affects the result. The access token and
passwords are never printed.

## What the driver does

1. Starts the fake ntfy server (`POST /<topic>`, recording arrival time, path,
   `Title`, `Priority`, `Tags` and body) and the HTTP target. The target answers
   200, or 503 while the failure is switched on.
2. Checks `GET /health`, then runs `POST /auth/setup` with the setup token, or
   logs in if the admin already exists.
3. Creates a `shoutrrr` channel with URL `ntfy://<callback-host>:<ntfy-port>/<topic>?scheme=http`
   (events `down`, `up`, default for new monitors) and sends one test message.
4. Creates an `http` monitor for `http://<callback-host>:<target-port>/`.
5. Waits until the monitor is `UP` (timeout 3 × interval + 30 s).
6. Switches the target to 503 (t0) and waits for the ntfy `DOWN` message for
   this monitor. The limit is `(retries+1) × interval + 30 s`. Checks the monitor
   status `DOWN`, an open incident, and that the 24 h uptime is below 100 %.
7. Switches the target back to 200 and waits for the `UP` message
   (informational). Checks that the incident is resolved.

## Notes

- Shoutrrr's ntfy service posts to `/<topic>` with `Title`, `Priority` and
  `Tags` headers, and with the message as plain text. The driver matches the
  Shoutrrr body text `DOWN: <monitor name>` / `UP: <monitor name>`.
- The node's SSRF protection is off in the default local auth mode. With
  Zitadel and `SSRF_PROTECTION=auto`, the callback host and target are blocked
  and the run is expected to fail; this was not verified.
- The driver leaves its channel and monitor (`as01-…` names) on the node for
  inspection. Delete them manually before the next run if a clean node is needed.
- Unit tests: `go test ./test/acceptance/as01/` covers the fake ntfy (including a
  real Shoutrrr send), the timing limits, the report table and flag validation.
  The scenario itself is run against a live node, not in unit tests.
