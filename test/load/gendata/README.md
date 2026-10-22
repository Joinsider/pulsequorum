# gendata: synthetic history for AS-10

`gendata` is an offline load-test tool. It fills the data directory of a
**stopped** voter with N days of heartbeat history for the monitors that already
exist in that voter's `state.db`. It supports acceptance scenario AS-10
(`docs/pflichtenheft.md`): 30 days of data at 1,000 monitors, a retention run,
then a storage limit of 4 GB per voter and an API p95 below 200 ms.

It is test tooling, not product code. It only uses the existing store and
uptime APIs and does not change `internal/`.

## Usage

```bash
go build -o gendata ./test/load/gendata/
./gendata -data /path/to/voter-data -days 30 [-tenant default] [-down-rate 0.002] [-seed 1] [-cpuprofile cpu.prof]
```

| Flag | Default | Meaning |
|------|---------|---------|
| `-data` | (required) | Data directory of the voter. It must contain `state.db` and `node_id`. |
| `-days` | 30 | Days of history, ending at the time of the run. |
| `-tenant` | `default` | Tenant whose monitors get history. |
| `-down-rate` | 0.002 | Probability per UP check that a DOWN streak starts. A streak lasts 1 to 10 checks. |
| `-seed` | 1 | Seed of the generator. The same seed gives the same data. |
| `-cpuprofile` | | Writes a CPU profile of the run. |

### Procedure for AS-10

1. Stop the voter. The generator writes `metrics.db` and does not coordinate
   with a running process.
2. Create the monitors through the API (or the UI), as the scenario requires.
   Paused monitors are skipped.
3. Run `gendata -data DIR -days 30`. Keep a copy of `DIR` first, because a
   failed run leaves partial history behind.
4. Start the voter on `DIR` and measure: `metrics.db` size, retention run,
   API latency.

The generator refuses to run when `metrics.db` already holds heartbeats of the
node. Delete `metrics.db*` to start from a clean history, or use a data
directory without history. A fresh `metrics.db` gets a new incarnation.

## What it does

- Reads the monitors of the tenant from `state.db`, all pages. It does not
  write `state.db`.
- Reads the node ID from `DIR/node_id` and opens `metrics.db` with
  `sqlite.OpenMetrics`. The heartbeat stream uses the node ID and the
  incarnation of `metrics.db`. The seq is strictly increasing and continues
  after the stored seq.
- Generates one check per `IntervalSeconds` for every monitor, starting at a
  random offset. Checks are emitted in time order. Latency is log-normal, median
  45 ms, p95 about 80 ms, clamped below the monitor's timeout. A DOWN streak
  fails either as `connect` (connection refused) or as `timeout`.
- Stores the heartbeats in batches of 5,000 with `InsertHeartbeats`.
- Simulates the live job in hourly steps: after each simulated hour it calls
  `uptime.Job.RunOnce` with a fake clock. Rollups, compaction into latency
  minutes after 48 h, and the default retention (raw 30 d, hourly 365 d) run
  as they would live. Hourly steps are needed because the job rolls up at
  most 24 hours per run.
- Prints a progress line per simulated day (rows, elapsed time, rows/s, sizes
  of `state.db`, `metrics.db` and their WAL files). At the end it checkpoints
  both databases and prints the final sizes and the total.

## Limitations

- **Creation time.** The uptime job and the API skip buckets that end before a
  monitor's `CreatedMs`. The real creation time in `state.db` is the time of
  the API call, so the backfilled history lies before it. To make the job roll
  it up, `gendata` gives the job a view in which every monitor was created at
  the start of the history (`backfillState`). The API still reads the real
  `CreatedMs`, so **API uptime queries over the backfilled period report no
  rollup data** for those hours. That makes the API measurement not
  representative. Fixing this needs a way to set the creation time of a
  monitor (for example through the state store, or a backdated import). The
  generator does not write `state.db`.
- **Consensus.** The consensus status comes from the status history in
  `state.db`, which the generator does not write. Consensus rollups therefore
  show the time as unknown. Node rollups (one stream) are computed from the
  heartbeats and are real.
- **Single node.** The history is a single stream of one node, the voter's own.
  Probes and other voters are not simulated.
- **Incidents and status history** are not written; they live in `state.db` and
  go through Raft.
- **Monitors are not in the Raft log.** Monitors seeded directly with SQL, for
  benchmarks, are not in the Raft log. A later restart of the voter can replace
  them. Use the API for the real scenario.

## Testing

```bash
go vet ./test/load/gendata/
go test ./test/load/gendata/
```

The unit tests cover latency determinism and distribution, DOWN streak
statistics, window bounds, and chronological order across monitors with
different intervals. The integration test runs three days for two monitors in
a temporary data directory. It checks the seq, hourly rollups, compaction of
heartbeats older than 48 h, that the last 47 h are kept in full, and that the
monitors in `state.db` are unchanged.
