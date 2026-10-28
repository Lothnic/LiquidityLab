# Verification record

## Public capture and durable workload evidence — 28 October 2026

Commits `2917994`, `929522c`, `8c7e329`, `5d0113a`, `81ea498` and `4e4838c` add native
receive timing, HTTP/inference/profiling workloads, a real capture fixture,
separate bounded Go blocking profiles and retained workload results.

- The [Kraken BTC/USD fixture](../examples/recordings/kraken-btcusd-20261028/README.md)
  contains 50 actual public frames, one valid snapshot, 45 normalized events and
  no gaps. Go and Python agree on exact normalized prices, quantities, levels,
  trade identity and ordering. The short capture's receive-to-book p99 is a
  histogram upper bound of 200 ms; recording persistence reached 180.386 ms.
  These timings combine several operations and do not isolate disk fsync.
- The [original durable baseline](../benchmarks/results/durable-runtime-20261028/README.md)
  retains 11 schema-v1 reports, including unsuccessful load trials. Three
  metrics-enabled/disabled pairs are descriptive comparisons; differing
  wall-clock decisions prevent a causal instrumentation-overhead claim.
- The [v2 envelope bundle](../benchmarks/results/runtime-envelope-20261028/README.md)
  retains seven exact reports plus CPU/allocation profiles from the clean
  `929522c` binary. Six workloads completed; the allocation-profile trial
  rejected 375 of 2,500 source offers and remains invalid. Final financial
  conservation and a drained cursor do not repair those missing offers.
- HTTP baseline measurement completed 1,000 market updates and 400 reads.
  The real unvalidated Python worker completed 100 measured RPCs alongside
  1,000 market updates and 400 reads; its 107 fills cover the full lifecycle.
  The controlled timeout/baseline trial had 50 measured expiries. The
  timeout/pause trial paused during warmup and had zero measured policy calls.
  The short timeout/baseline and CPU/allocation measurements contain no later
  scheduled fill; their fills occurred in warmup.
- An independent inspection verified all seven compressed/uncompressed report
  hashes against the originals, all 14 input-manifest hashes, both profile
  hashes, exact signed fill/fee cash flows and final unlocked balances.
  Shutdown gauges, policy calls in flight and durable source debt were zero.
  The original HTTP checker consumed bounded JSON but did not assert response
  identity; the archived reports retain that limitation.
- Separate blocking-profile checks cover fresh-file refusal before database
  use, mutual exclusion with CPU/allocation capture, the 32 MiB cap, parseable
  nonempty samples, cleanup on write failure and real PostgreSQL execution.
  Sampling is enabled only around measurement/drain; output remains cumulative
  Go synchronization history and does not measure database server fsync.
- Python passed 88 checks without skips with the replay binary configured.
  Ruff passed and generated protocol bindings matched. Existing frontend
  evidence remains 46 checks and a production build; no UI code changed here.
- The first fresh full Go race run passed all product packages but exposed a
  workload-test fixture with only two read slots and an all-success assertion.
  Its fixture now admits all ten offered reads through 16 bounded slots; the
  unchanged success/route assertions passed 25 real-PostgreSQL race repetitions.
  The subsequent full uncached Go race suite passed with development PostgreSQL
  and the actual Python/replay paths configured: workload package 6.026 s,
  daemon 39.535 s, runtime 13.561 s, store 4.352 s and inference 6.246 s.
  These package times are suite durations, not execution latency.

The evidence review also identified two runner audit gaps: HTTP response
identity was not validated and a falsely completed parent with residual quantity
could pass conservation. Commit `4e4838c` corrects both with dedicated
regressions and records explicit validation-contract versions. Tests also cover
valid derived decimals with repeating 72-place average prices, preserving
precision without changing public input parsing. The old reports remain immutable. Shared multi-account execution, load-plus-fault
headroom, measured restart/restore under demand, policy quality, private venue
integration, design-user acceptance, deployment and operational gates remain
open. The [shared runtime design](shared-runtime-design.md) is preparation, not
an implemented or admitted multi-account product.

## Owned paper recovery and process boundaries — 28 October 2026

Checkpoints: `50f7294`, `e0d68ba`, `59e9ff7`, `d710fce`, `c27bbec` and
`2d54659`. The daemon can now explicitly create or resume one retained paper
account. Initial balances are prohibited on resume; recovered active plans
remain paused with their original start, deadline, benchmark and policy pins.
See [paper-recovery.md](paper-recovery.md) for commands and compatibility limits.

Executed checks:

- The full uncached `go test -race -count=1 ./...` suite passed with development
  PostgreSQL, real Python worker and replay binary paths configured. This includes
  store, runtime, API, both paper CLIs, ordinary-build inert checkpoint behavior,
  and actual child-process recovery. The daemon package took 37.805 seconds in
  this run; that is suite elapsed time, not execution latency.
- The ten-scenario process matrix passed twice with `-race -count=2`, then again
  within the full suite and the saved verbose process run. It covers committed
  intent, authorization before send, accepted submit before reduction, full-child
  fill before source-cursor advance, genuine partial passive child fill, resting
  acknowledgement, accepted cancellation, ordered trade fact, pending policy,
  and an old sender kept alive through takeover.
- The ordinary production binary was SIGKILLed and reaped during a partially
  completed parent. Resume reopened the same schema/account, reconstructed accepted
  source effects, preserved exact balances/fees/deadline, and stayed paused.
  Retrying the original start command returned its duplicate receipt without
  restarting execution. Graceful shutdown then reported `known_final=true`.
- Store checks prove the insert guard rejects old/missing/malformed ownership
  tokens even with an altered SQL search path, while preserving external fact
  ingress independently of execution leases. They cover 20 takeover/append races,
  retained attempted holds, queued obsolescence, canonical retries, transaction-
  local token cleanup and a crash before the first runtime snapshot.
- A deterministic SQL lock-queue test commits a genuine old reducer fill between
  bootstrap's first read and ownership claim. The fresh post-claim snapshot
  preserves its exact economics. All rules/fees/execution configuration fields
  are checked before ownership changes; active baseline parents require explicit
  resume after readiness returns.
- Focused create/resume option, missing-account and incompatible-source checks
  passed with `-race -count=3`. A rejected incompatible resume preserves the
  existing owner's revision, readiness and balances.
- Python: 87 checks passed, no skips, with the rebuilt replay binary; the existing
  Gymnasium v0 deprecation warning remains. Ruff passed and generated bindings
  matched. Latest web evidence remains 46 passing component/API checks and a
  production build; this backend change introduced no frontend contract change.

Each process scenario independently reconstructs expected fills from the durable
simulated venue observations, including accepted effects not yet reduced at
crash. It compares fill identities, quantity, quote fees, balances, original
benchmark/start/deadline, ownership epoch, outstanding holds/intents and drained
source cursors. The suspended sender is released only after the next owner has
recovered; a controller event confirms its handoff returned, and SQL confirms it
could not append a submission. This is a concrete PostgreSQL paper-effect fence.
No external exchange connection exists in these runs.

The saved verbose run is the ignored local artifact
`artifacts/paper-recovery-process-20261028.log`. Its ten unloaded local process
recoveries ranged from 61.9 to 101.2 ms from launching the resumed child to its
ready paused account. Source debt was zero or one fact before recovery and zero
afterward. These single-host scenarios are not a latency SLO, percentile,
capacity result or recovery estimate for large journals. The source remains
synthetic and the separate policy test server returns a bounded test action.

The controlled matrix compiles a separate binary with the
`liquiditylab_faults` build tag and private controller pipes. Ordinary builds
contain inert functions and cannot enable those faults through environment
settings. All child processes were reaped; test schemas alone were removed.
Existing manual verification accounts were retained.

Remaining work at that earlier checkpoint included persistence metrics/load
tooling and real public capture, now covered in the newer bounded section above.
Shared multi-account feeds and supervision, historical market counterfactual replay,
evaluation and promotion, private transport/readiness, design-user evidence,
CI/deployment, load-plus-fault capacity, 24-hour soak and backup/restore. Local
paper recovery does not establish private venue failover or completion of the
full implementation goal.

## Asynchronous policy and retained history — 28 October 2026

Component checkpoints: `f9125d7` (store), `ec293eb` (policy runtime),
`18eeea0` (API reports) and `6433711` (history UI). Host tools remain
Go 1.26.8, PostgreSQL 18.6, Python 3.12.11, Node 26.5.0 and npm 11.17.0.
The local PostgreSQL tests use isolated randomized schemas.

Executed checks:

- `go test -race -count=1 ./...`, with development PostgreSQL DSN, absolute
  `LIQUIDITYLAB_PYTHON` and `LIQUIDITYLAB_REPLAY_BINARY` configured: all packages
  passed. The inference suite included the actual Python worker boundary.
- Focused pending-policy recovery checks passed under PostgreSQL with
  `-race -count=3`: a stopped worker returning after restart and an old actor
  still running after a mocked ownership transfer cannot change recovered
  financial state. Mocked isolation assertions prove database fencing behavior;
  these checks are not OS process-kill or actual sender-isolation evidence.
- `LIQUIDITYLAB_REPLAY_BINARY=<built daemon> .venv/bin/python -m pytest -q`:
  87 passed, no skips; one existing Gymnasium v0 deprecation warning.
- `npm test -- --run` in `apps/web`: six files, 46 checks passed. New coverage
  includes history scope/cursor validation, deferred responses across plan
  selection/refresh/return-to-latest, timeout labels, not-started records,
  and a 450-fill fixture that never renders more than 200 rows per page.
- `npm run build`: TypeScript and Vite production build passed.
- `.venv/bin/ruff check src tests scripts`: passed.
- `scripts/generate_protocol.py --check`, with the pinned Go plugin paths:
  generated bindings matched.
- Real gRPC status regression checks preserve stable busy/deadline/cancel/
  invalid causes for audit classification and release the client's admission
  slot after failure.

### Real worker, actor, browser and durable result

An eight-step PPO smoke bundle trained through the Go replay backend was loaded
by the actual Python inference process. Bundle SHA-256:
`0e73f473892a2939910ea17420eb36d22b8f38c8e479b1b95e0a79d47fad7e6b`.
It remains unvalidated and live-ineligible. Eight training steps establish an
integration artifact, not a useful learned policy.

The built Go daemon and React desk used a new isolated paper account
`synthetic-paper-daemon/model_preview_01`, schema
`ll_model_preview_20261028_01`, generated market input and explicit initial
balances of 0 BTC and 20 USD. The browser previewed and created a paused 0.05 BTC
buy with a 102 USD ceiling, 0.01 BTC maximum child and a two-minute deadline.
Starting later preserved that original deadline.

The worker returned accepted semantic proposals, and four 0.005 BTC simulated
fills occurred before its deliberate termination. The actor then used the
configured baseline fallback; three 0.01 BTC fills completed the parent. Its
last policy projection reported `fallback_baseline` / `worker_unavailable`.
A cancellation attempted during the final transition was rejected as stale
and the refreshed desk correctly displayed completed state.

Authoritative final values:

- Seven unique simulated fills, 0.05 BTC total, average 100.01 USD.
- Frozen arrival midpoint 100 USD; quote fees 0.010001 USD.
- Executed cost 0.010501 USD; zero remaining quantity/residual estimate.
- Final balances 0.05 BTC and 14.989499 USD; zero held local reservations.
- All seven outbox intents final; observed/processed inbox cursors equal.
- Seventeen durable policy requests and seventeen decision records.
- First terminal report reference at revision 295; later source receipts did
  not move that reference.

The History panel loaded a known-final report, inspected creation revision 84
showing not-started/zero fills, returned to the final report and downloaded JSON.
The export retained all seven fills and explicitly identified its partial
100-event timeline. Browser error logs were empty. API assertions compared the
exact report totals, finality, creation revision and reservations. Subsequent
SQL inspection confirmed final intents, drained inbox and zero held reservations.
The daemon shut down with `known_final=true`; both daemon and worker stopped,
retaining their database state.

Local ignored evidence files under `artifacts/` include
`policy-history-20261028-before-stop.json`,
`policy-history-20261028-final.json`, `policy-history-20261028-export.json`,
and `policy-history-final-20261028.png`. They are implementation-host artifacts,
not checked-in benchmark or deployment evidence. Browser verification used
seven fills; the 450-fill bound is exercised by the component test.

### Remaining recovery and release gates

The new history views inspect stored revisions; they do not implement historical
market counterfactual replay. At this history checkpoint the daemon created one
new account and existing-account resume was unfinished. Stage 1 review found
unowned simulated observations and pre-claim bootstrap state. Those defects are
addressed by the subsequent owned paper recovery checkpoint above. Database fencing for this simulated venue must not be
represented as external exchange fencing.

Real public capture, private order transport, design-user validation, immutable
policy evaluation/promotion, multi-account capacity, process-kill recovery,
load/fault/24-hour soak and backup/restore gates were open at that checkpoint.
The local paper process-recovery portion is delivered above; the full objective
has not passed its completion audit.

## Paper desk checkpoint — 23 October 2026

Implementation checkpoint `dc521e5`, with authority revision correction
`a102a5e`. Host tools: Go 1.26.8, PostgreSQL 18.6, Python 3.12.11,
Node 26.5.0 and npm 11.17.0. PostgreSQL is a local development instance;
its tests create isolated schemas, not a production database.

Executed checks:

- `go test -race -count=1 ./...` with `LIQUIDITYLAB_TEST_POSTGRES_DSN` set:
  all packages passed, including runtime, store, API and paper CLI/server.
- `npm test` in `apps/web`: 35 tests across four files passed. Component tests
  cover exact decimal plan submission, preview authority binding, immutable
  unknown-outcome retry, late account responses, ready waiting previews,
  readiness rejection and form-edit invalidation. API tests cover nested
  payloads, malformed successful receipts and journal cursor validation.
- `npm run build` in `apps/web`: TypeScript and Vite production build passed.
- `LIQUIDITYLAB_REPLAY_BINARY=<built liquiditylabd> .venv/bin/python -m pytest -q`:
  87 passed, no skips. One existing Gymnasium v0 deprecation warning remains.
  A preliminary run without the binary passed 80 and skipped seven integration
  checks; the configured rerun above exercised them.
- `.venv/bin/ruff check .`: passed.
- `scripts/generate_protocol.py --check`, using the pinned Go plugins: generated
  Go/Python bindings matched.
- `go test -race -count=1 ./internal/inference -run TestPythonPPOWorkerBoundary -v`
  with the real Python/replay paths: passed; a tiny CPU PPO run supplied 20 Go
  features and a semantic action to Go validation/compilation. This does not
  prove actor scheduling, fallback integration or model quality.

### Browser and durable outcome

The in-app browser exercised the actual built React assets, authenticated API,
Go actor and PostgreSQL store against explicitly **synthetic** feed events and
balances. Cohort `ui_preview_03` followed preview → create paused → start →
partial fills → pause → resume → cancel. The retained database outcome was:

- Target 0.05 BTC; seven simulated fills totaling 0.042 BTC, average 100.01 USD.
- 0.008 BTC residual; 0.00840084 USD simulated fees.
- Frozen arrival mid 100 USD; executed cost 0.00882084 USD, excluding residual.
- Final BTC balance 0.042; USD 15.79117916; locked balances zero.
- Parent cancelled; zero held local reservations and zero unresolved intents.
- Graceful server shutdown reported `known_final=true` and retained its schema.

A separate final-build cohort `ui_preview_05` verified a ready preview with no
immediately placeable first child: the desk explained the waiting estimate,
created a paused parent, then cancelled it without starting. The browser error
log was empty during that check. Cohorts used distinct new accounts/schemas;
existing account state was never reset to repeat a run. Every verification
server stopped with a known-final result.

The workflow exposed and corrected perpetual preview expiry from no-effect
paper receipts, authority churn from HOLD bookkeeping, misleading final-state
copy and fee currency labels, and long-plan-ID/header mobile overflow. At the
final narrow layouts, document width was 320 at viewport 320 and 375 at viewport
390 (the desktop browser reserves scrollbar width). The temporary viewport was
reset. Financial tables scroll locally where needed; the document fits.

Local ignored evidence:

- `artifacts/paper-desk-cancelled.jpg`: completed browser workflow with retained fills.
- `artifacts/paper-desk-mobile.jpg`: 320 px long-ID plan layout.
- `artifacts/paper-desk-workflow.json`: scoped durable synthetic account summary.

These files are local run artifacts, not committed real-market datasets. The
browser workflow is development evidence, not design-user acceptance, production
capacity, OS process-kill recovery, private venue integration, or a live pilot.
Full history/session replay, asynchronous actor/model integration, immutable
holdout/promotion, deployment, load/soak/restore and live authorization gates
remain open. Real public capture has still not been demonstrated on this host.

## Historical Python reference slice

The following records the original implementation and was not the suite size
at the current checkpoint.

## Executed checks (original slice)

- `uv run --extra dev --extra rl pytest`: 40 checks passed. The optional integration test trains an 8-step PPO policy on synthetic data, saves it, reloads it on CPU, and performs inference.
- `ruff check .`: passed.
- A separate CPU PPO smoke run completed 128 requested/actual steps with seed 7. Its bundle is unvalidated and is not eligible for live execution.
- Both checked-in baseline reports were generated from the checked-in synthetic tape and plan files. The buy completes 0.060 base units; the sell completes 0.030 of 0.060 and leaves the remainder at its deadline.
- `uv build --out-dir dist`: built the source distribution and wheel.
- The wheel was installed in an isolated `uv run --no-project` environment. The registered Gymnasium environment reset and stepped with a 20-feature observation and six discrete actions. The recorder and training guard imported from that installed wheel.

The full verification environment includes Gymnasium 1.4.0, NumPy 2.5.4, Stable-Baselines3 2.9.0, and CPU PyTorch 2.14.1. The lockfile records the resolved dependencies. Default installation omits the optional RL extra.

## External connection outcome

A 15-second public Kraken WebSocket capture attempt received no usable book messages because the opening handshake timed out. The recorder wrote gap records and a failure manifest; no real-data replay result is claimed. Protocol tests use Kraken's published checksum example and local fixtures.

At that original slice, private account access, live orders, trader interface, durable live sessions, holdout evaluation and promotion were absent. The current paper implementation is recorded above; private live execution, holdout evidence and promotion remain incomplete.
