# Durable paper runtime workload

This command runs the real account actor and PostgreSQL store with one synthetic
BTC/USD paper account and a Go baseline. It measures the durable product path;
the replay throughput benchmark measures a different workload. It does not
establish supported account capacity, exchange behavior, model quality, or UI
performance.

Build the binary outside the repository and explicitly select a development
database, dedicated schema, and **new** output file. Reserved schemas (`public`,
`information_schema`, and `pg_*`) are refused. No account or schema is reset, reused,
or dropped. Every invocation creates a random account with exactly BTC 10 and
USD 100000, with both balances initially free. A report is reserved with
`O_EXCL` and mode `0600` before database mutation. An existing output path fails
before migrations or account creation. Parent directories must already exist.

From the repository root, with `work/` in the parent task directory:

```sh
mkdir -p ../../work/runtime
go build -buildvcs=true -o ../../work/runtime/durable-runtime ./benchmarks/runtime
export LIQUIDITYLAB_BENCH_POSTGRES_DSN='postgres://liquiditylab@/postgres?host=/tmp/liquiditylab-pg-01a1655e&port=55432'
../../work/runtime/durable-runtime \
  --development --schema ll_runtime_bench \
  --storage-description 'Local development PostgreSQL; describe actual storage and concurrent activity here' \
  --seed 42 --warmup 2s --duration 20s \
  --market-rate 10 --view-rate 2 --trade-percent 50 \
  --baseline scheduled --cadence 100ms --pool 4 \
  --source-queue 64 --view-in-flight 4 \
  --out ../../work/runtime/baseline-01.json
```

Repeat the same invocation with fresh names `baseline-02.json` and
`baseline-03.json`. Use another fresh output and `--metrics=false` for an
observer comparison. Compare repeated runs and resource/latency distributions;
one enabled/disabled pair cannot isolate overhead from host variation. All
accounts remain in the selected schema. Their unique identities are in reports.
The DSN is read only from the named environment variable (`--dsn-env` selects
another name); it is omitted from reports and database errors are sanitized.
Keep secrets out of the free-form storage description.

The scheduled baseline proposes an aggressive slice every **15 seconds** and
HOLD at intervening decisions. The first slice normally runs during warmup.
The 20-second example therefore includes a later slice in measurement. A shorter
run is useful for checking the runner but can contain no measured fills. The
alternative `--baseline liquidity-aware` exercises the existing baseline's
passive/urgency behavior; report it as a different workload.

## Arrival and loss semantics

The offered market and view schedules are computed from index/rate offsets,
independently of completion time. Inputs use a local SplitMix64 seed; book
levels, trade quantities, IDs, mix, and intended offsets are reproducible. Books
update a stable 99/101 spread with quantity 1; seeded trades sell at 99 with
quantities 0.01–0.04. This is a synthetic input model, not captured venue data.
The full canonical input manifests and their SHA-256 hashes are included in
each phase. The hash is over Go's compact JSON encoding of `input_manifest`,
before the report's pretty printing. View manifest entries include an unused
zero-value event.

One worker applies market inputs in source order. Its source queue is bounded;
views use separate bounded concurrent slots. Runner admission timestamps refer
to those queue/slot boundaries. Actor admission is only observable between call
start and return through the public actor API. Actor telemetry separately
records internal admission, wait, and service work. A successful actor return
counts as completed. A timeout/unknown outcome never counts as completed.

Every input retains its intended offset, generator attempt, runner admission,
actor call start, response time, and outcome where available. Offsets are local
monotonic nanoseconds from the phase start. `generator_lag` is attempt minus
intended; `runner_queue_wait` is call start minus runner admission;
`offered_to_success` includes generation, queue, and actor delay. Exact nearest
rank percentiles are calculated from the retained bounded samples, separately
for market and view calls. Rejected and uncertain work are counted alongside
latencies. `actor_call_all_returns` includes error responses; success-only
latencies never imply that rejected work completed.

A full source queue, actor market error, cancellation, or dispatch later than
`--max-generator-lag` (default 250ms) **invalidates the entire workload**. Inputs
attempted after their phase window are rejected. The report still retains all
offered slots and outcomes. Subsequent successful calls cannot repair a missing
source fact or make that run valid. The final parent is cancelled, and the
command exits nonzero. This runner is unsuitable for treating source loss as
acceptable load shedding. View admission rejections remain visible without
invalidating source delivery; uncertain view outcomes invalidate the final run.

The ordered source worker serializes its calls, but does not throttle the
offered schedule. Actor call durations describe a service component, not a
closed-loop maximum-throughput experiment. There is no maximum-throughput mode.

Bounds are enforced before database use: at most 10000 combined offered inputs,
rates of 0–500/s each, at most five minutes total configured intervals, 4096
source slots, 64 concurrent views, 64 pool connections, and a 32 MiB report.
Duration/drain failures are explicit; a database operation can be uncertain
after its caller stops waiting. The command has a total context budget of the
configured intervals plus two minutes; actor shutdown receives another 15s.

## Reading reports

Warmup drains before measurement starts a fresh arrival clock. The actor stays
active between phases. The report contains before/after cumulative telemetry,
pool, resource, storage, and WAL snapshots around measurement and its drain.
Counters, histogram bucket counts and sums can be subtracted. **Cumulative
histogram extrema and quantiles cannot be subtracted** to obtain measured
extrema or quantiles. Use the phase samples for exact input latencies. Snapshot
boundaries are sequential reads rather than a single global atomic instant.

The initial fresh snapshot is followed by the product `RecoverPaper` readiness
path. The exact final actor view is reloaded durably after cancellation and actor
shutdown and includes balances, fills,
reservations, intents, readiness, source cursors, and unresolved facts. A valid
run requires a known terminal parent, no remaining holds/intents, no recovery
required, zero durable source cursor debt, and zero in-flight telemetry gauges
after shutdown. A separate exact conservation audit computes signed cash flows
and both fee currencies from the initial balances, rejects duplicate executions
and parent overfill, and compares snapshot fills with persisted execution rows.
`financial_conservation` records that result and total filled/remaining quantity.
`telemetry_after_actor_shutdown` is a distinct quiescent snapshot; it includes
cleanup and the final durable reload, while measured snapshots retain their
phase boundaries. Final accounting is an observed
paper result; real-time scheduling can change order/trade binding and economics
even with the same input seed. The initial plan buys up to BTC 10, caps each
child at 0.01, and has limit 102 and budget 100000. It is cancelled after the
interval rather than forced to fill.

Build the benchmark with `-buildvcs=true` **after committing the intended source**.
`host.binary_build` records the executable's embedded Go build metadata, including
its main module, dependency versions/checksums, and the fixed permitted VCS
settings (`vcs.revision`, `vcs.time`, `vcs.modified`, and VCS type). When VCS
stamping is absent, `vcs_status` explicitly says `unavailable`; the runner does
not infer a build revision from the current checkout. Arbitrary linker flags
and environment settings are omitted, and local replacement paths are redacted.
`host.executable` records the running executable's SHA-256 and byte length,
computed by streaming at most 512 MiB from the local binary. An unavailable or
oversized executable checksum is explicitly marked unavailable.

`host.launch_worktree_revision` and `launch_worktree_dirty` describe Git in the
**launch directory**, which may differ from the source used to build that
binary. They are context, not binary software identity. Launch Git metadata is
explicitly unavailable when no checkout can be inspected. Rebuild after source
changes; copying a previous executable into a newer checkout does not update its
embedded build identity or checksum.

Other host information includes engine/state/migration
versions and migration checksums, Go/runtime/OS, CPU, host memory and available
cgroup caps. Process CPU, allocation, GC, RSS/heap endpoints and pool snapshots
include runner overhead. Peak RSS is the process lifetime high-water mark.
`gc_cpu_fraction_cumulative` is not an interval ratio. Resource endpoint reads
come from Linux `/proc` and `getrusage`; the runner targets this Linux
development environment.

Database settings include durability/WAL and connection/cache settings. The
observer uses one additional connection outside the declared actor/store pool.
Schema bytes include retained earlier runs, so compare endpoints. **WAL deltas
are cluster-wide** and include other sessions, databases, checkpoints, and
benchmark observation activity. Storage description is supplied by the operator;
fsync enabled does not prove device durability. No cluster isolation is assumed.

The report explicitly lists missing measurements: private-event age, recording
write lag, actual restart/replay/restore duration, inference, UI, and pure blocked
lock time. Final cursor debt and local cancellation acknowledgement do not
substitute for those measurements. Derive candidate admission only after
separate fault/drain evidence and a saturation analysis.

## Focused checks

```sh
go test ./benchmarks/runtime
LIQUIDITYLAB_TEST_POSTGRES_DSN="$LIQUIDITYLAB_BENCH_POSTGRES_DSN" \
  go test -race ./benchmarks/runtime -count=1
```

The optional PostgreSQL test runs two short workloads, verifies exact balances
against recorded fills, checks fresh accounts and metrics on/off, and retains
its unique development schema/accounts for audit. Unit checks cover bounded
configuration, deterministic manifests, independent offered counts under
overload, ordered market calls, uncertain completion handling, and refusal to
overwrite reports.
