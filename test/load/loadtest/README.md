# loadtest

Load and measurement CLI for Pulsequorum voters (NFA-PERF-01 to NFA-PERF-04,
roadmap AP-1.15). It runs against a voter started locally or as a container.

    go build -o loadtest ./test/load/loadtest

## Subcommands

### targets: HTTP target farm

    loadtest targets -listen :18900 -count 1000 [-latency 0s]

- `GET /t/{i}` answers `200 ok` (`503 down` while target `i` is switched down).
- `POST /control/{i}/down` and `POST /control/{i}/up` switch target `i` (204).
- `-latency` adds a fixed delay to every answer. The default is no delay.
- Target state is one atomic flag per target, so the farm is concurrency-safe.

### setup: create the monitors

    loadtest setup -base http://127.0.0.1:8080 -setup-token TOKEN \
      -user admin -password 'long-password' \
      -count 1000 -interval 60 -target-base http://host.containers.internal:18900 \
      -prefix load-

- With `-setup-token` the admin is created (`POST /auth/setup`). If the node is
  already set up (409), the tool logs in instead. Without a token it logs in.
- Creates `count` HTTP monitors `{prefix}NNNN` with config `{"url": target-base/t/i}`,
  `intervalSeconds`, `retries: 0`, using 16 parallel workers (`-workers`).
- Every request carries an `Idempotency-Key`. The key is a name-based UUID
  (version 5 layout, SHA-1 of prefix and index) so reruns do not create duplicates.
  Random UUIDs would defeat that.
- Exits non-zero if any monitor fails. Progress and total time go to stderr.

### measure: API, status latency and resources

    loadtest measure -base http://127.0.0.1:8080 -user admin -password '...' \
      -duration 10m -targets-control http://127.0.0.1:18900 \
      (-container NAME | -pid PID) -out report.json > report.md

- API: every 5 s the full monitor listing (`limit=500`, all pages) is timed,
  and one uptime request (`range` random of 24h/7d/30d/365d) for a random monitor.
- Status latency: every 60 s a random monitor is switched down, the tool waits
  for the `DOWN` state event on the WebSocket stream, switches it up and waits for `UP`.
  Cycles of different monitors may overlap. `latency = received - event.ts`,
  where `event.ts` is the commit time set by the voter. `flip to event` is
  informational. Timeouts (`-status-timeout`, default 150 s) are counted.
  One reader goroutine stamps `received` as soon as a message arrives and appends
  every state event to a log. A waiter is registered before the target is flipped
  and scans the log from that point on, so a late waiter cannot delay the reader.
- Stream reconnects: when the stream closes (for example with 4001 "session expired"
  after the 15-minute token expiry), the tool logs in again, requests a new ticket and
  reconnects. The reconnects are counted. A wait during which the stream was down is
  reported as `lost during reconnect`, not as a timeout.
- Unexpected status changes: every `DOWN` and `PENDING` state event of a monitor that
  the tool did not flip during the run is counted as a false alarm. The report lists
  the first five with `event.ts` and receive time. `PENDING` is counted and listed,
  but only `DOWN` decides the verdict `no unexpected DOWN`.
- Resources: every 5 s `docker stats --no-stream` (`-container`, the podman shim
  works too) or `ps -o %cpu=,rss= -p PID` (`-pid`). Three output shapes are accepted:
  Docker (`"CPUPerc":"12.34%"`, `"MemUsage":"85.2MB / 1.074GiB"`), podman object
  (numbers, `CPUNano` and `SystemNano`) and podman array (`--format json`, `cpu_time`
  such as `"1m53.007223s"`). With a cumulative CPU counter the CPU percentage is
  computed per interval: counter delta divided by wall time between two readings,
  times 100. The printed percentage is only used without a counter, because podman's
  `cpu_percent` is an average since container start. CPU is in per-core units, so
  100 % means one full core. The first reading has no interval and gives no CPU value.
- The Markdown report goes to stdout and the JSON report (samples included) to `-out`.
  The report ends with PASS, FAIL or NO DATA verdicts against the thresholds:
  CPU mean < 100 % (50 % of 2 vCPUs), memory < 512 MiB, API p95 < 200 ms,
  status latency <= 2 s, and `no unexpected DOWN`.

Because the status latency uses the voter's `event.ts` and the local receive
time, the clocks must be synchronized. Run the tool on the voter host or use NTP.
The number of heartbeats is not counted. The report gives the expected rate
(monitors x 60 / interval) instead.

## Target farm in the container network

Run the target farm inside the container network of the voter, not on the macOS
host. With targets on the host, Podman's gvproxy dropped SYNs at about 17 new
connections per second (66 sockets in SYN-SENT and about 420 false DOWNs in 20
minutes). That measures the VM networking, not Pulsequorum. Inside the network
the monitors reach the farm directly.

Build a static Linux binary for the Podman VM (same architecture as the host):

    GOOS=linux GOARCH=$(go env GOARCH) CGO_ENABLED=0 go build -o loadtest-linux ./test/load/loadtest

Create the network and start the farm. The control endpoint stays reachable from
the host through the published port:

    docker network create ucload-net
    docker run -d --name uctargets --network ucload-net -p 18900:18900 \
      -v $PWD/loadtest-linux:/loadtest:ro gcr.io/distroless/static-debian12 \
      /loadtest targets -listen :18900 -count 1000

Start the voter with `--network ucload-net`. Create the monitors with the farm's
container name as target base, and drive the control endpoint from the host:

    loadtest setup -base http://127.0.0.1:8080 -setup-token "$TOKEN" \
      -user admin -password "$PW" -count 1000 -interval 60 \
      -target-base http://uctargets:18900
    loadtest measure -base http://127.0.0.1:8080 -user admin -password "$PW" \
      -duration 15m -targets-control http://127.0.0.1:18900 \
      -container pulsequorum -out "$S/report.json"

## Example run

    S=/tmp/lt
    loadtest targets -count 1000 &
    loadtest setup -setup-token "$TOKEN" -user admin -password "$PW" \
      -count 1000 -interval 60 -target-base http://host.containers.internal:18900
    loadtest measure -user admin -password "$PW" -duration 10m \
      -targets-control http://127.0.0.1:18900 -container pulsequorum -out "$S/report.json"

## Tests

    go test ./test/load/loadtest/

Tests cover the percentile function, the docker (Docker, podman object and podman
array) and ps parsers, the CPU meter, the idempotency key, the target farm handler,
the target index parsing, the status event log (waiters, lost during reconnect,
re-login on reconnect), the unexpected-change count and the report verdicts.
