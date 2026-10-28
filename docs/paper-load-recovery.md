# Ordinary paper restart under concurrent read demand

`TestPaperProcessLoadRecoveryWithBystanderDemand` kills and reaps an ordinary
paper daemon, cold starts it against the retained account, and checks recovery
while a second ordinary daemon continues using the same PostgreSQL instance.
Both accounts receive independent HTTP read schedules at 20 offers per second.
Each ordinary synthetic source emits one trade every 500 ms and an initial
snapshot. This is a bounded local recovery check with a bystander process.

## Run

Use an explicitly selected development PostgreSQL instance. The test creates a
randomized `ll_load_recovery_*` schema and retains its account state for inspection.
It never resets or drops an existing account or schema. Set a fresh output path
whose parent directory already exists:

```sh
LIQUIDITYLAB_TEST_POSTGRES_DSN='<development-dsn>' \
LIQUIDITYLAB_RECOVERY_LOAD_OUTPUT='/absolute/path/fresh-recovery.json' \
  go test -race -count=1 ./cmd/liquiditylabd \
  -run '^TestPaperProcessLoadRecoveryWithBystanderDemand$' -v
```

The evidence file is created with exclusive creation and mode `0600` before any
database changes. An existing path, including a symlink, fails the check. Without
`LIQUIDITYLAB_RECOVERY_LOAD_OUTPUT`, evidence uses `t.TempDir()` and normally
disappears after the test. Preserve an explicit file for a retained result.
Missing `LIQUIDITYLAB_TEST_POSTGRES_DSN` skips this development integration check.

The harness builds one ordinary executable before the measured schedule, clears
inherited `GOFLAGS`, and explicitly disables build tags and race instrumentation.
It hashes that exact executable and reads its Go build identity, rejecting a
wrong package, nonempty tags or race instrumentation. The test harness itself can
run under `-race`. The daemon receives its database URL and API token through its
environment; neither enters the evidence or test output. Read failures are fixed
classifications without raw network error text, URLs or response bodies.

## Workload and recovery contract

Each account starts with 0 BTC and 20 USD and a started 0.05 BTC buy parent,
0.01 BTC maximum child, and original 120-second deadline. Setup waits for a
partially filled active parent before beginning the ten-second read schedules.
Each schedule cycles through three state reads, one session-list read and one
session-report read per five offers. A schedule has four nonblocking client slots,
a 750 ms request timeout and a 1 MiB response limit. It retains all 200 intended
offers per account, their intended and actual monotonic timestamps, admission,
call and end times, bounded outcome, HTTP status, response byte count and phase.
State and report replies must identify the original scoped account and parent,
retain its plan pins and represent a started active or paused residual parent.

At about three seconds the victim receives SIGKILL and is reaped. Its read
schedule continues through an explicit 250 ms unbound interval, cold launch,
listener discovery and readiness. In-flight errors, endpoint-unbound offers,
runner rejections and unresolved calls remain visible; they never become
successful completions. Phases use intended offer times on the original
monotonic clock. Failure cleanup cancels and drains both pollers, retaining every
scheduled slot, including slots explicitly marked not attempted after cancellation.
Such an incomplete run remains invalid.

The checker reads accepted paper observations directly from PostgreSQL and
independently reconstructs unique execution identities, quantities, quote fees,
BTC cash flows and USD cash flows. It compares the recovered snapshot, execution
rows, exact unlocked balances, residual quantity and projected API finances.
Recovery must retain the original start, deadline, arrival midpoint, specification,
rules, fees, simulation configuration, currencies, instrument revision, source and
policy pins. The ownership epoch must advance exactly once; observed and processed
source cursors must agree, with no child, holds, outstanding intents, pending facts,
unresolved facts or pending policy request.

The recovered parent must stay paused for at least another 1.1 seconds with no
fill or residual change. An exact retry of its original start request must return
a duplicate receipt. Accepted observations already present after the kill must
retain their sequence, identity, kind and payload hash after recovery. The
bystander must retain ownership and advance its durable source sequence. Both
read schedules drain before graceful SIGTERM. Each remaining daemon must exit
successfully with `known_final=true`, be reaped, and leave a cancelled residual
parent with independently reconciled finances. Failure cleanup also kills and
reaps any surviving child.

Recovered readiness and terminal financial finality are checked at explicit,
separate boundaries. The recovered paused account must be ready with a valid
market and book, and `recovery_required=false`. After a process has been reaped
and its graceful log confirms known finality, a cancelled parent may retain the
precise stopped-source state: `reason=market_invalid`,
`market_id=web-source-ended`, invalid market/book and account not ready. Source
termination can persist that readiness invalidation before final cancellation.
The final audit records the actual flags and reason. Every financial obligation,
source cursor, fill, fee and unlocked balance must still reconcile; any other
recovery reason, clock discontinuity or outstanding obligation fails the audit.

## Evidence and interpretation

The JSON records binary hash and build identity, account and schema identities,
original plans, workload bounds, before/after source sequences and ownership
epoch, crash observation hashes, recovered/final financial audits, every read
offer and outcome, process reaping and graceful finality. The configured daemon
pool cap is 16 connections per process: 32 across the two concurrently running
daemons. The audit store adds a cap of two, and at most one direct SQL audit
connection is opened at once, for a configured aggregate cap of 35. These are
configuration bounds, not measured connection occupancy.

`valid=true` requires the complete recovery, accounting, schedule and cleanup
contract. A successful process exit or zero observed debt alone is insufficient.
The report begins incomplete and is marked failed if any assertion fails. Focused
unit guards check absent endpoints, retained rejected/unresolved offers, response
identity, falsely completed residual parents and missing report evidence:

```sh
go test -race -count=1 ./cmd/liquiditylabd -run '^TestPaperLoadRecovery'
```

Kill-to-first-ready-paused and cold-launch-to-first-ready-paused are single-trial
wall-clock observations. Listener discovery polls at 10 ms; readiness and pause
checks wait 20 ms between bounded control requests. Discovery and request time are
included, so the first ready observation is an upper bound. HTTP demand follows
an independent monotonic schedule; synthetic market demand is the daemon's own
fixed 2 Hz source. Restart creates a new synthetic session and snapshot, and
synthetic events during downtime were never accepted or replayed.

The trial does not establish a shared process or pool, 50 Hz source demand,
capacity, headroom, SLOs, private venue failover, database restore or a 24-hour
soak. Ordinary kill timing can leave zero accepted source debt: record zero
honestly; it provides no proof of backlog recovery. The controlled crash-boundary
matrix in [paper-recovery.md](paper-recovery.md) remains separate evidence.
