# Durable paper runtime workload

This command runs the real account actor and PostgreSQL store with one synthetic
BTC/USD paper account and a Go baseline. It measures the durable product path;
the replay throughput benchmark measures a different workload. It does not
establish supported account capacity, exchange behavior, model quality, or UI
performance.

The runtime manifest records effective `view_capacity`, `fact_quantum_steps`
and `fact_quantum_time_ns` alongside ordinary/control/fact queue capacities.
The reduction defaults are four transitions or 5 ms between atomic transitions;
the View lane defaults to 64 slots. An individual SQL transition can exceed the
cooperative budget. Market call timing includes waits between resumed turns;
`public_market_apply_ns` sums only active turns. Compare matching revisions and
configuration when evaluating scheduling changes.

## Optional scoped PostgreSQL activity

Add `--db-activity-interval 20ms` to sample only this runner's store connections.
The default `0` disables the diagnostic and preserves the DSN's application name.
An enabled run gives the store a fresh random diagnostic tag; the existing direct
observer is excluded and exclusively used for samples during measurement/drain.
There is still one observer connection in addition to the configured pool.
No query text, query ID, addresses, DSN, credentials or arbitrary error strings
are selected or retained. Application names are diagnostic metadata, not an
authentication or financial-ownership mechanism.

`postgres_activity` records its own UTC origin and local monotonic schedule,
intended/attempt/end offsets, query outcomes, PID-plus-backend-start lifetimes,
connection disappearances and fixed backend observation categories. Sample
offsets do not share the offered input phase's origin. First/last backend offsets
are client query bounds, not exact server transition times. A query has a 200 ms
deadline. Overlapping/missed ticks are retained as `query_busy` or
`generator_late`; the sampler does not issue a catch-up burst. Stop joins its
accepted query before ordinary database/audit queries reuse the connection.

Bounds: interval 10 ms–1 s, at most 10,000 ticks, 50,000 backend observations,
128 lifetime identities and 8 MiB serialized diagnostic. Configuration validates
the full-pool schedule plus one second for drain before database changes. A
longer drain may reach runtime caps; it then remains explicitly incomplete.
Missing metadata/backends, missed ticks, errors, restarts and caps make the
diagnostic incomplete and the requested overall run invalid, while its source
delivery and financial checks remain separately visible. An oversized diagnostic
discloses raw-sample truncation and retains aggregate counters. A query timeout
can close the observer connection, in which case later endpoint/audit failure
also remains visible; no replacement connection is silently introduced.

Counts use **backend observations** as their denominator. They are not request
counts or exact wait durations. `active_unclassified` is not CPU usage; idle
`ClientRead` is not database contention. `active_wal_io` identifies only IO waits
named `WalSync`/`WalWrite`; `LWLock/WALWrite` stays `active_lwlock`. Connections
that exist between samples and brief waits can be missed. Sampling perturbs
the measured workload, so compare repeated observer-off/on runs before making
performance decisions. Server CPU and cumulative backend IO/WAL service costs
are not measured by this addition. PostgreSQL documents activity snapshots,
visibility and wait identities in its [version 18 monitoring reference](https://www.postgresql.org/docs/18/monitoring-stats.html).

Focused race checks cover fixed categories, missing identities/PID reuse,
deadlines, missed ticks, caps, error redaction, disabled behavior and observer
reuse. A real PostgreSQL advisory-lock waiter verifies that the sampler sees
the owned wait, excludes unrelated/observer tags, and sees its disappearance
after release. A separate ordinary workload check exercises opt-in wiring and
the unchanged final conservation audit.

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
records internal admission, wait, and service work. A successful actor return counts as completed. In HTTP mode, completion requires
a successful response, full bounded body consumption, and valid JSON. A timeout/unknown outcome never counts as completed.

Every input retains its intended offset, generator attempt, runner admission,
service call start, response time, and outcome where available. Offsets are local
monotonic nanoseconds from the phase start. `generator_lag` is attempt minus
intended; `runner_queue_wait` is call start minus runner admission;
`offered_to_success` includes generation, queue, and the selected service delay. Exact nearest
rank percentiles are calculated from the retained bounded samples, separately
for market and view calls. Rejected and uncertain work are counted alongside
latencies. `service_call_all_returns` includes error responses; success-only
latencies never imply that rejected work completed.

A full source queue, actor market error, cancellation, or dispatch later than
`--max-generator-lag` (default 250ms) **invalidates the entire workload**. Inputs
attempted after their phase window are rejected. The report still retains all
offered slots and outcomes. Subsequent successful calls cannot repair a missing
source fact or make that run valid. The final parent is cancelled, and the
command exits nonzero. This runner is unsuitable for treating source loss as
acceptable load shedding. View admission rejections remain visible without
invalidating source delivery; failed or uncertain reads invalidate the final run.

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
write lag, actual restart/replay/restore duration, React rendering, and pure
lock-only time. An explicit separate Go block profile can attribute completed Go
synchronization waits, subject to the cumulative/window limits below. Inference
RPC timing is present only when a policy worker is selected; external worker CPU,
memory, and server-side idleness are unmeasured. Final cursor debt and local
cancellation acknowledgement do not substitute for those measurements. Derive candidate admission only after
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

## Optional HTTP and policy workloads (report v2)

Current binaries write `durable-runtime-benchmark-v2`. Stored v1 reports remain
immutable evidence for their original binary and workload. V2 uses
`service_call_started_ns`, `service_call_returned_ns`, `service_call_success`,
and `service_call_all_returns` in place of v1's `actor_call_*` names. Every sample
states its `service_boundary`; market calls remain `actor.MarketFromSession`,
while reads are `actor.View` or an HTTP round trip including body/JSON work.
The baseline defaults remain direct actor views and Go execution without Python.
`--view-rate` sets the offered rate of the selected read operation in either mode.

New reports identify their checks under `validation_contracts`:
`financial: exact-cashflow-terminal-v2` requires terminal status to agree with
the exact remaining quantity, and `http: scoped-account-plan-v1` requires the
returned account/session projections to match the frozen workload identity and
parent specification. Actor-read runs use `http: not-requested`. Older archived
v2 reports lack these fields and used JSON syntax checks for HTTP responses;
their recorded validity does not imply the stronger response checks ran.

`--read-mode http` starts the real scoped trader API on a random literal loopback
port. It generates a private random bearer credential scoped to this run's
account. The credential and listener address never enter the report. HTTP reads
use the same bounded runner slots as actor reads, an explicit client/API deadline
(`--http-timeout`, default 1s, range 10ms–2s), no proxies/redirects/compression, and
a maximum 1 MiB response. The client consumes the body and validates JSON before
recording completion; response status and consumed bytes remain in each sample.
The server and idle connections are shut down before final actor quiescence.
This measures **HTTP polling, not React rendering**.

The explicit HTTP route mix defaults to 60% account state, 20% session list
(`limit=1`), and 20% current session report. Set `--http-state-percent`,
`--http-sessions-percent`, and `--http-report-percent` to integers totaling 100.
A deterministic coprime stride spreads each 100-read cycle across that mix;
short runs report their actual route counts and can differ from the percentages.
Routes are scoped to the new account and its `benchmark-plan`. Samples include
route, status, and bytes; `http_route_counts` and `read_response_bytes` summarize
actual demand. Rejection and error results remain visible alongside successful
latencies. HTTP timeouts are unresolved responses and never count as completed.

The controlled gRPC worker is enabled only by an explicit
`--mock-policy-delay 0` (immediate) through `--mock-policy-delay 5s`. It exposes
real loopback gRPC, a pinned synthetic policy hash, the real observation schema,
`unvalidated` mode, and semantic HOLD proposals that echo the frozen request
binding. It admits one Predict at a time, rejects excess requests, and honors
context cancellation. It exercises serialization/RPC/deadline machinery, not
Python or model quality. A healthy HOLD worker normally produces no fills;
a timeout with baseline fallback exercises actual baseline order execution.

For example, run a controlled worker plus the default HTTP polling mix with:

```sh
../../work/runtime/durable-runtime \
  --development --schema ll_runtime_bench \
  --storage-description 'Local development PostgreSQL; shared host; WAL cluster-wide' \
  --seed 42 --warmup 2s --duration 20s \
  --market-rate 10 --view-rate 2 --trade-percent 50 \
  --baseline scheduled --cadence 100ms --pool 4 \
  --source-queue 64 --view-in-flight 4 --read-mode http \
  --mock-policy-delay 150ms --policy-timeout 100ms \
  --policy-profile paper --policy-fallback baseline \
  --out ../../work/runtime/controlled-http-01.json
```

`--policy-timeout` is pinned at 1–200ms, `--policy-fallback` is `baseline` or
`pause`, and `--policy-profile` is `paper` or `shadow`. Paper applies eligible
proposals; shadow records them and executes the pinned Go baseline. No option
permits live operation or validates/promotes a model. The `pause` fallback stops
further decision demand after its first failure, so it is a different offered
policy workload from continuing baseline fallback.

For a real Python target, first start a compatible worker with a preexisting
Go replay policy bundle as described in [inference.md](../../docs/inference.md):

```sh
uv run python -m liquiditylab.inference /absolute/path/to/policy-bundle \
  --listen 127.0.0.1:50052
```

In the benchmark invocation, replace `--mock-policy-delay ...` with
`--policy-target 127.0.0.1:50052 --policy-sha256 <exact-lowercase-policy-sha256>`.
A local Unix target (`unix:/absolute/socket`) also works. Real and controlled
selectors are mutually exclusive. `inference.New` verifies bundle identity,
ordered schema, promotion mode, and non-live eligibility **before database
opening, migration, account creation, or ownership acquisition**. Verification
failure writes a sanitized setup failure report and creates no account. Raw
worker/network errors, target addresses, and DSNs are not retained. The external
worker is managed separately; the runner closes only its client.

`policy_worker` records verified hash/mode/profile/fallback/deadline, controlled
fixture delay/action where applicable, and before/after client request counters.
These counters count calls reaching the client wrapper; they do not count every
scheduler slot. Failures include the `rejected`/`expired` subsets. Snapshot
maximum in-flight values are cumulative. Runtime/inference telemetry additionally
records decision demand, fallback, rejection, and RPC latency when `--metrics` is
enabled. Metrics-disabled runs still track client completion for cleanup. After
actor shutdown, the wrapper closes admission under a mutex, closes the client,
and waits up to 2s for registered calls to finish, including a late actor
Propose goroutine. `client_calls_drained` proves only this process's client
quiescence, not external worker idleness. Remaining calls or policy slots make
the run invalid.

## Separate profiling runs

CPU, allocation, and blocking capture perturb measurements. Use fresh
accounts/reports and separate comparable runs; select at most one profiler. All
three output flags reserve
new files with `O_EXCL`, mode `0600`, before database mutation. Existing profile
paths fail before account creation. Files are capped at 32 MiB and always closed;
setup/interrupted capture is marked incomplete, and write failures fail the run.
Only the profile basename, kind, boundary, byte length and SHA-256 enter the
report. Profiles contain stack/sample metadata, never heap object contents.

Add exactly one flag to the same explicit workload invocation, using a new report
pathname for each run:

```sh
--cpu-profile ../../work/runtime/baseline-cpu-01.pprof
--alloc-profile ../../work/runtime/baseline-allocs-01.pprof
--block-profile ../../work/runtime/baseline-block-01.pprof
```

The first captures CPU only from immediately before measured schedule construction
through measurement and accepted-call drain; CPU profiler stop/flush completes
before the resource endpoint read and can add overhead to its elapsed window.
The second writes Go's sampled cumulative `allocs` profile after measured resource
endpoints. Allocation history includes process startup, warmup, runner, HTTP/mock
work and measurement; it is **not an interval allocation delta**. No forced GC is
inserted into measurement. Use the report's allocation deltas for the measured
resource interval. Profile capture time and boundaries are recorded explicitly.
Neither profile includes CPU/allocations in an external Python process.

The third enables Go's process-wide `runtime.SetBlockProfileRate(1)` immediately
before measured schedule construction and disables it with rate 0 immediately
after measurement and accepted-call drain, before resource endpoint reads. Rate
1 records all completed blocking events, adding potentially substantial overhead.
Normal runs leave block sampling disabled. The runtime's cumulative `block`
profile is written after resource endpoints, bounded by the same 32 MiB limit.
The reported start/finish timestamps mark the sampling window; serialization
occurs afterward. Every exit closes the file and disables any active sampler.

The profile attributes cumulative contention counts and blocked nanoseconds to
Go channel and synchronization stacks (including mutexes and wait groups).
Go records completed blocking events; waits can cross the enable/disable boundary,
and in-progress waits are not a complete inventory. The runtime provides no reset:
any earlier blocking samples in this process remain in the cumulative output.
A fresh runner process ordinarily has sampling disabled during setup/warmup;
the profile still must not be interpreted as a strict interval delta, elapsed
wall time, or lock-only time. Concurrent waits can sum above wall time. It covers
this Go process, including the runner and HTTP/client work, and does not measure
actual PostgreSQL fsync, database server waits, or external Python worker blocking.
Profiles contain stack/sample metadata and no object contents.

Inspect files against the exact checksummed executable used for that report:

```sh
go tool pprof -top ../../work/runtime/durable-runtime ../../work/runtime/baseline-cpu-01.pprof
go tool pprof -top -alloc_space ../../work/runtime/durable-runtime ../../work/runtime/baseline-allocs-01.pprof
go tool pprof -top -sample_index=delay ../../work/runtime/durable-runtime ../../work/runtime/baseline-block-01.pprof
go tool pprof -top -sample_index=contentions ../../work/runtime/durable-runtime ../../work/runtime/baseline-block-01.pprof
```
