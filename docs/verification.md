# Verification record

## Trader desk clarity refinement — 30 October 2026

The scoped UI refinement keeps the existing paper runtime and API semantics,
replaces small display-heavy operator text with readable task labels, and
separates Trade from Session history. Native disclosures hold execution settings,
balance details and technical activity. Cancellation warnings, free/local-reserved
amounts, exact decimals and the original account/action for unknown retries remain
available. Sending and unknown outcomes now have distinct UI messages.

- `npm run build` passes TypeScript and the Vite production build.
- All 48 existing frontend checks pass. Copy assertions were updated to the
  new labels; exact request/retry identity, account scope, authority binding,
  cancellation and history revision checks remain intact.
- The Impeccable mechanical detector returned no findings on the changed
  application and history surfaces. Static token checks give secondary text
  contrast of at least 6.94:1 on the primary dark surfaces.
- The incumbent interface was inspected in the browser using an isolated local
  synthetic paper account. Subsequent browser access to the updated localhost
  preview was rejected by browser security policy. Updated desktop/mobile render
  and interactive browser sign-off are therefore **not verified**. A user
  screenshot was requested; build/component checks do not substitute for it.

The earlier replay UI draft remains a separate uncommitted workstream. Its type
predicate was corrected locally so that the existing draft does not block the
whole frontend build; it is not wired into this interface or counted as delivered.

## Cooperative account scheduling and fresh bridge/public-data gates — 29 October 2026

Runtime `069609b`, process fixture `0c00e73` and manifest/docs `6635f63` implement
bounded resumable source reduction and local control receipts. Each turn yields
after four transitions or 5 ms between atomic steps by default. An individual
SQL call can exceed that cooperative budget. Ordinary requests retain FIFO order;
separate bounded controls/views and maintenance run between reduction turns.

- Real PostgreSQL regressions reduce a 512-fill transport prefix by count/time,
  discard the in-memory prefix, and reconstruct it under a fresh owner with
  exactly 1.0256 BTC / 997.4630656 USD. A control/read at the two-fill checkpoint
  sees cancellation requested, a retained child/hold and readiness false.
  Later market calls remain ordered and cannot acknowledge early. These are
  deterministic in-process scheduling/fresh-owner checks, not a capacity result.
- Independent Sol review found three bugs before commit: resolved malformed
  observations lacked a continuation cursor; implicit plan obsolescence could
  release a queued hold during debt; later stale rejections could clear an
  outstanding uncertain-commit reload. All three are fixed with regressions.
  Queued children retain their submitting state and cannot acquire simulated
  fills merely because cancellation was requested. Caught-up disposal is atomic;
  attempted children still accept racing fills until known finality.
- The first full run found one obsolete process-test expectation: it demanded
  that pause wait for the cancellation fault checkpoint. The updated test
  requires both a successful local receipt and the durable cancellation gate
  before SIGKILL, preserving all recovery/economic assertions. Its focused race
  run passed in 4.257 s; accepted cancellation debt drained from one to zero.
- The subsequent full uncached `go test -race -count=1 ./...` passed with
  development PostgreSQL, the actual Python interpreter and a fresh ordinary
  replay binary. Suite durations: workload 7.440 s, daemon 50.675 s, runtime
  76.664 s, store 4.608 s, inference 7.389 s. This run preceded the component
  commits; its Go sources are those committed through `75d71fd`. Durations are
  test-suite times, not product latency or admission limits.
- The [fresh combined conformance report](../conformance/results/20261029-phase3-4.json)
  records clean source `75d71fd`, all five steps passed, and 216 Python tests
  with zero skips/failures/errors (13.06 s). It rebuilds its owned ordinary daemon
  and hashes four shared fixtures. The original three fixture hashes are
  unchanged; the added CRC32 fixture is checked in both languages. The existing
  Gymnasium v0 deprecation warning remains.
- Public-data regressions cover malformed CRC widths/forms, verified quoted
  CRC propagation, disconnect/checksum-triggered resnapshot before trade delivery,
  and explicit depth-truncation deletions without resurrecting trimmed levels.
  The retained real capture still matches Go/Python normalization. The local
  [Phase 3](phase3-public-data-audit.md) and [Phase 4](phase4-bridge-audit.md)
  requirement audits close those gates. The slower synthetic bridge remains
  optional; Python is still the training default.
- Frontend commit `62bfbee` adds cancellation-receipt/finality and paused-child
  resume regressions. All 48 tests across six files and the TypeScript/Vite
  production build passed; no UI production code changed.
- Four subsequent clean-build [scheduling workloads](../benchmarks/results/runtime-quantum-20261029/README.md)
  retain three complete ordinary 50 Hz source / 20 Hz HTTP trials (1,000/400
  completions each) and an invalid 250 Hz ramp (2,232/2,500 source completions,
  268 runner rejections). All retain exact final finances and zero debt.
  Original/compressed bytes and hashes, all samples/eight input manifests,
  route counts, exact cashflows, finality/gauges, empirical p99s and scoped
  observer denominators were independently checked. The current host reports
  16 logical CPUs versus the older cohort's 24; no causal before/after
  performance claim is made. Steady completion does not prove recovery headroom.

Process admission, shared feeds/accounts, recording pressure, policy evaluation,
historical market replay, private transport, production soak and restore gates
remain open. No private exchange order was submitted.

## Core acceptance and scoped PostgreSQL observation — 29 October 2026

Checkpoints `bf3d4d5`, `f9a6342`, `35fc7b7` and `79062b0` add rational
ledger/compiler/replay properties, replacement-finality conformance, bounded
PostgreSQL activity diagnostics and the [Phase 2 acceptance audit](phase2-core-audit.md).

- At clean `35fc7b7`, the full uncached Go race suite passed with development
  PostgreSQL, the real Python interpreter and newly built replay binary.
  Workload 7.137 s, daemon 52.093 s, runtime 12.660 s, store 4.114 s and
  inference 7.133 s are suite durations, not product latency.
- The [combined conformance run](../conformance/results/20261029-phase2.json)
  passed generated-binding/reference drift, four Go race packages, an owned
  daemon build and 198 Python checks with zero skips/failures/errors. Five
  approved economic/observation goldens now include the replacement lifecycle;
  the previous four cases were unchanged. Existing float tolerances remain.
- Optional activity diagnostics passed three focused race repetitions before
  the full suite. Checks cover exact application scoping, PID reuse, observed
  lock release, missing metadata, query deadlines, every tick through Stop,
  caps, explicit truncation and observer connection reuse. Independent Sol
  review found no important issues in sampler/caller integration or Phase 2
  requirement coverage.
- Seven [PostgreSQL activity workload reports](../benchmarks/results/runtime-pg-activity-20261029/README.md)
  retain exact original bytes from the same clean executable. Three observer-off
  and three observer-on ordinary runs each complete 1,000 source updates and
  400 HTTP reads. The enabled diagnostics each retain 1,001 ticks and 2,002
  backend observations. Periodic 20 ms sampling can alias with 50 Hz input;
  counts are snapshots, not elapsed service or CPU time.
- The separate 250 Hz ramp loses 25 source offers, completing 2,475/2,500, and
  remains invalid. Its complete sampler has 513 backend observations, including
  248 WAL IO, 47 active/unclassified and 136 idle-in-transaction classifications.
  During its 10.232 s measurement/drain, 7,902 commits accumulate 5.769 s of
  client COMMIT round trips, versus 7.027 ms of pool acquisition and 174.366 ms
  of account FOR UPDATE round trips. These overlapping values locate commit
  cost but do not establish pure storage/CPU attribution or supported load.
- Root independently verified original/compressed hashes, all fourteen source
  manifests, offer/admission/outcome/route counts, empirical p99s, exact cashflows,
  residuals, unlocked balances, cursor/finality/gauge invariants, sampler lifetime
  denominators and selected store counter/count/sum/bucket deltas. The invalid
  ramp is retained despite its valid financial conservation and zero final debt.

Phases 1–2 are locally accepted; the Go training default remains Python.
Stage 2 service-demand/admission/recovery-headroom and the broader product,
policy, deployment and live gates remain open. No private orders were sent.

## Shared contracts and recovery-under-demand — 29 October 2026

Contract checkpoints `15bdb5c`, `3bcb69f` and `0287a0f` add shared canonical
decimal/time/action/observation vectors, a combined conformance command, pinned
reference precision and explicit legacy float-timer compatibility examples.
The [Phase 1 audit](phase1-contract-audit.md) maps the local exit criteria to
implementation and the [original conformance report](../conformance/results/20261029-phase1.json).

- The five-step conformance command passed: byte-for-byte generated bindings,
  non-mutating reference golden regeneration, Go domain/execution/replay/RPC
  race checks, an owned ordinary daemon build, and the full Python suite.
  Python passed **198** checks, with zero skips, failures or errors; the existing
  Gymnasium v0 deprecation warning remains. The report records source `0287a0f`
  with `dirty=true` for pending test/docs work and hashes all three fixtures.
- `npm ci`, `npm test` and `npm run build` passed in `apps/web`: **46** checks
  across six files, plus TypeScript/Vite production output. No UI code changed.
- Ruff passed across `src tests scripts conformance`.
- `2c080ec` adds an ordinary-process SIGKILL/resume check under independent
  continuous HTTP demand from two accounts/processes. It uses 2 Hz synthetic
  sources and 20 HTTP reads/s/account; it does not exercise shared actors/pools
  or establish an admission limit. Recovery stays paused with original pins,
  balances and fill identities. The bystander continues advancing its source.
- The first full Go race run passed all other packages, but the new check
  rejected a financially final shutdown because its stopped source retained
  invalid readiness. `fe2cc31` distinguishes recovered-ready-paused from reaped,
  known-final shutdown. Only the exact stopped-source flag is allowed at the
  latter boundary; financial debt, holds, unresolved facts, pending policy and
  clock discontinuity still fail. Fifteen deterministic mutations protect it.
  The corrected focused PostgreSQL race run passed in 13.938 s, including 400
  scheduled HTTP offers, exact finances and all child processes reaped.
  The subsequent full uncached Go race suite passed at clean `bf3d4d5`, with
  PostgreSQL and actual Python/replay paths configured: daemon 52.114 s,
  runtime 13.345 s, store 4.508 s, inference 6.364 s and workload 6.406 s.
  These are package suite durations, not product latency.
- Three fresh [ordinary recovery trials](../benchmarks/results/paper-recovery-load-20261029/README.md)
  then passed from the same clean build. Each preserves all 400 scheduled reads:
  bystander 200 completed; victim 195 completed and five endpoint-unbound offers.
  Kill-to-first-ready-paused observations were 291.273, 296.290 and 298.288 ms,
  including the deliberate unbound interval; cold launches were 33.006, 36.272
  and 38.881 ms. Exact final cashflows, residuals, pins, cursor drain and process
  finality were independently recomputed/checked against the original reports.
  Crash debt happened to be zero in all three; this is no backlog/headroom claim.
- `b74c62c` adds a pull-request validation workflow with real PostgreSQL,
  Python/Go integration, conformance and frontend jobs. Its constituent commands
  are exercised locally; no GitHub execution or remote push is claimed.

Phase 1 is complete locally. Phase 2 generated-sequence acceptance, broader
recorder recovery, scale/admission, historical replay, policy quality, private
venue integration, deployment, soak and restore gates remain open.

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
- The clean `fc525b1` executable was then used for three
  [audited workloads](../benchmarks/results/runtime-audited-20261028/README.md).
  Baseline and real-Python HTTP runs each completed 1,000 market updates and
  400 reads with explicit response-identity and terminal-conservation checks.
  The worker completed 100 measured RPCs; its 107 lifecycle fills conserve
  BTC 10.997 and USD 99899.101606 from the stated initial balances.
  Baseline market/read p99s were 11.540/10.971 ms; worker-run p99s were
  32.506/35.112 ms. These are single trials, not overhead or capacity estimates.
  The separate 250-source-events/s block-profile trial rejected 290 of 2,500
  measured offers and is invalid despite exact terminal accounting and zero
  debt. Its 20.77 aggregate seconds of completed Go blocking are largely
  runner/actor channel waits; concurrent waits exceed elapsed wall time and
  do not isolate a database server or storage bottleneck. The worker was
  stopped and reaped after verification.
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


## Archive import and inference duplicate checkpoint — 29 October 2026

- `79a90dd` adds the bounded immutable recording archive and import/inspect CLI.
  A real ordinary-CLI import, duplicate import and inspect agreed on dataset
  `2a409892cbf06e56141fd23bc5bbf33bba4920400f753740e0b0020e2cb7e80a`.
  Independent hashes/byte lengths match the retained raw capture, original
  manifest and all 45 normalized events. The focused archive/CLI race checks
  pass (8.088 s and 1.399 s). See [the operator checkpoint](market-archive.md#executed-local-checkpoint--29-october-2026).
- `e8de84b` adds direct accepted-proposal result replay: one original financial
  effect, three separately durable late audits, 100 newer logical book updates,
  and compilation against the latest valid book. This is handler-level evidence.
- The [fresh combined conformance report](../conformance/results/20261029-contracts-19a35c9.json)
  records clean source `19a35c90030803d2a2c3afcdd320189e04ce62e4`, all five steps
  passed, 216 Python checks, zero skips/failures/errors, and unchanged hashes for
  all four fixtures. Its Go scope is contracts/replay/RPC, not the full product.
- The separate full uncached Go race attempt against the same source, development
  PostgreSQL and actual Python/replay paths **failed** the new accepted-backlog
  process audit: `fixture crash durable execution prefix differs from snapshot`.
  The daemon package failed at 54.205 s; the runtime package passed at 80.120 s,
  and the archive, CLI, inference, store and other listed packages passed. This
  run is not a full-suite pass. The initial report used temporary output and is
  not part of a retained clean process cohort. Investigation found that the
  account snapshot and execution rows were collected in separate transactions
  after OS process reap, allowing a previously received server-side COMMIT to
  become visible between reads. A bounded coherent post-reap observation is
  required before publishing accepted-backlog recovery evidence.

Neither the archive nor this contract result passes product capacity, historical
session binding, policy evaluation, private transport, soak or restore gates.

## Clean archive replay and accepted-backlog closeout — 29 October 2026

- `87a0ca3` corrects the previously failed crash observer: a bounded account-row
  lock waits for any server-side commit after client reap, then remains held
  across account/execution collection. A deterministic pending-writer regression
  verifies the wait and newest matching prefix. Strict prefix, held obligation
  and exact accounting assertions were retained.
- `1d1026e` adds explicit sealed quiet-tail tape coverage without fabricated
  market events or changed legacy tape hashes. `883704c` adds the bounded offline
  archive comparison CLI, strict explicit specs, exact baseline cashflows,
  source/provenance checks, common deadline valuation, comparability reasons,
  cancellation, resource bounds and exclusive complete report publication.
  [The replay runbook](historical-replay.md) records the actual ordinary review
  invocation and independent rational-arithmetic check. It remains a simulated
  reference, with no inferred paper-session binding.
- Clean `883704c02d925d7585440e67e845751e39e38850` passes the full uncached Go
  race suite with development PostgreSQL and actual Python/replay paths:
  1,078 test/subtest pass events, no skipped test cases, daemon 66.036 s,
  runtime 82.910 s. Three packages contain no tests. The [retained bundle](../benchmarks/results/paper-accepted-backlog-20261029/README.md)
  includes exact compressed Go JSON output, raw-output SHA-256 and source state
  before/after the run. Every Go regression named in the Phase 5/6 audits is
  present among its passing test events.
- The [fresh combined conformance run](../conformance/results/20261029-contracts-883704c.json)
  passes all five steps and 216 Python checks, with zero skips/failures/errors
  and unchanged hashes for all four fixtures. Its ordinary replay binary hash
  is retained in that report. This is distinct from full product Go coverage.
- Three ordinary untagged/non-race daemons from the same clean source have
  SHA-256 `5046cdcf4aba516241b7c6d59db14d527fbaca1f988607e99516d5707532d92d`.
  Each retained process trial is complete/valid with no issues: 2/128, 1/128 and
  2/128 post-reap snapshot/execution prefixes, debt 6−5=1, positive child holds,
  then all 128 accepted fills recovered under the next epoch while paused.
  The 0.003 BTC batch has exact 0.00029997 USD fees and leaves BTC 0.003 /
  USD 19.69973003 balances with 0.047 BTC residual. The source is explicitly
  harness-authored; ordinary 2 Hz trades did not generate this batch.
- Every process trial retains 400 scheduled HTTP offers: victim 176 completed
  and 24 endpoint unbound, bystander 200 completed. Bystander source sequence
  advances 5→25. Both final parents are cancelled, all holds/intents are final,
  cursors drain, original pins remain, both graceful shutdowns are known final,
  and all process lifetimes are reaped.
- Root independently checks raw/gzip hashes and lengths, all HTTP indices,
  routes/outcomes and timing boundaries, exact rational cashflows/fees/residuals,
  and read-only retained database observations, batch JSONB payload hash,
  executions, final snapshots, balances, fee assumptions, owner epochs,
  pins/cursors and reservation/outbox finality. The bundle preserves the
  independent audit and environment metadata. The environment was not declared
  exclusive; no source-rate capacity or service attribution claim is made.

The scoped requirement maps now close local [Phase 5](phase5-paper-audit.md)
and [Phase 6](phase6-inference-audit.md). The earlier failed full-suite attempt
remains above. Stage 2 admission/headroom, shared multi-account execution,
paper-session source binding/replay jobs/UI, design-user acceptance, immutable
policy evaluation/promotion, private adapter/reconciliation, deployment,
soak/restore and authorized live evidence remain incomplete. The full goal is
active.


## RL paper beta execution checkpoint — 30–31 October 2026

The [handoff suite record](../conformance/results/20261030-paper-beta-handoff.json)
retains the full uncached Go race run (1,186 passing test/subtest events, no failing
or skipped test cases), 216 Python checks without skips and 78 UI checks across
seven files plus the production build. Go used actual PostgreSQL and Python/replay
paths. Three Go packages have no test files. The record distinguishes runtime
source `b795231`, UI source `611003f` and the pre-commit replay binary's modified
build metadata; it must not be described as a clean-source release build.

The [packaged execution evidence](../deployment/evidence/2026-10-30-handoff/README.md)
records a real Go-backed PPO model completing a `0.1 BTC` plan through 50 simulated
fills, followed by one maker fill in a second, later cancelled plan. No operator
resume was needed to finish the first plan. Exact parsed revision-pinned reports,
all 51 execution identities, balances and zero remaining reservations survived
stop/resume. The second plan's expiry occurred before the worker's SIGTERM, so
that step does not establish worker-loss causality. Earlier worker-unavailable
fallback evidence remains separately scoped.

An [explicit image refresh](../deployment/evidence/2026-10-31-refresh/README.md)
on 31 October observed HTTP availability and account readiness while retaining
the same model, account and reports. These are bounded local observations, not a
public deployment, uptime guarantee, strategy-quality benchmark or capacity claim.
Browser/mobile review remains blocked by the browser access restriction. The
historical native feed interruption still has no established cause; supported
feed reliability, hosted access, backup/restore, retention and soak remain beta
gates in the [milestone checklist](hosted-paper-beta.md).


### Source diagnostics follow-up — 31 October 2026

The retained local container later lost source readiness while HTTP remained
healthy. This observation is later than the ready snapshot above, so that snapshot
must not be read as continuous feed evidence. The old binary logged only a generic
failure; the exact ingress error and historical cause remain unknown. Its failure
preceded the fresh Go suite, so this suite cannot explain that occurrence.

Commit `9788269` preserves internal error identity while retaining safe outward
messages. Unexpected source return logs the UTC observation time, an allowlisted
reason and whether invalidation was confirmed. Controlled tests first failed on
the missing classification, then passed with actual randomized PostgreSQL fixtures,
including an injected ownership takeover. This establishes diagnostic behavior,
not the historical failure's cause or its resolution. No timeout, ordering,
financial guard or automatic recovery behavior was changed.

The [fresh full Go race record](../conformance/results/20261031-source-diagnostics.json)
retains the compressed raw event log and hashes: 1,190 passing test/subtest events,
no failures or skipped test cases, three packages without tests, and a passing real
Python PPO boundary. The Python suite also reran against a fresh ordinary Go binary: 216 checks pass
without skips. All 78 UI checks and the production build reran successfully. The
record identifies both replay binaries and modified build metadata. Feed
reliability remains an open release gate.


The [curated source diagnostic record](../deployment/evidence/2026-10-31-feed-diagnostics/README.md)
retains the previous failure boundary and the controlled red/green checks. The
diagnostic build subsequently stayed ready at every probe through 480 seconds
from startup, reaching synthetic receipt 959 with an already cancelled parent.
No new trades or operator plan commands were issued. This bounded observation
does not resolve the earlier interruption or satisfy the 24-hour soak gate.


### Offline backup and isolated restore — 31 October 2026

Component `679c442` adds a quiescent, whole-schema backup plus immutable bundle
and recordings archives, private output permissions and an atomic completion
manifest. Restore refuses existing resources, verifies every payload, uses fresh
volumes/credentials and starts only a network-isolated PostgreSQL database.
All table rows and sequence values are compared using sorted JSON-row SHA-256;
restored file contents, ownership and modes are independently reread.

The [actual drill record](../deployment/evidence/2026-10-31-backup-restore/README.md)
retains the source cutoff and verification inventory: 13 tables plus one sequence,
23,922 journal entries, 23,863 command rows and 51 execution identities. Both fresh
offline restores passed. An explicit, separately configured clone inspection with
Go/PPO and fresh auth returned the exact completed revision-3569 and cancelled
revision-3736 reports, all 51 fills and exact balances. No ports were published,
no trading command was sent, and inspection/target databases were subsequently
stopped with volumes retained. The original source resumed ready with reports
unchanged. The restore script never starts those inspection services itself.

Sixteen focused archive/integrity/refusal regressions pass. The full Python suite
also passes 232 checks without failures or skips using actual Python/replay
integration paths; its existing Gymnasium v0 warning remains. Ruff check and
format verification pass for the two new Python files. No Go or UI code changed;
the prior full Go race and UI evidence remains separately scoped.

This terminal-account drill does not establish active-parent/held-obligation
recovery, nonempty recording restoration, fresh-host image transfer, power-loss
behavior or hosted reliability. Those broader operations gates remain open.


### Active queued-child restore and HTTPS preparation — 31 October 2026

The [active restore record](../deployment/evidence/2026-10-31-active-restore/README.md)
uses two further isolated projects. After source-container death, an account-row
lock waits for any server-side commit before the durable cutoff is collected.
The ACTIVE parent at revision 24115 has a queued passive .002 BTC child and a
.20037996 USD hold. Offline restore matches the full cutoff; ordinary Go/PPO
resume advances epoch 9→10, preserves deadline/policy/spec and leaves the parent
ready PAUSED, with that child obsolete and no hold or new dispatch attempt.
All 51 executions, 23,492 previous observation rows, 24,055 command rows/results
and five pinned reports survive. Independent root comparison verifies every
retained row hash against the private full SQL snapshots and derives balances
with rational arithmetic. Three nonempty copied Kraken recording fixtures also
match. All test containers were stopped with volumes retained. This boundary
had no dispatched/uncertain child or nonzero accepted-fill debt.

Component `959994f` adds an optional certificate-based HTTPS overlay using runtime
certificate files and replacing the base HTTP publication. The
[local TLS record](../deployment/evidence/2026-10-31-tls/README.md) verifies trusted
self-signed test-CA hostname validation, HTTP 200 health/authenticated reads,
HTTP 401 unauthenticated reads, HTTP 413 ingress body rejection and exact retained
reports. TLS 1.2/1.3 succeed; TLS 1.1, untrusted/wrong-host certificates and missing
or mismatched certificate configuration are rejected. An initial isolated test
bridge failed to publish the port; normal loopback-published bridge verification
was completed before claiming successful ingress. No production certificate,
public hostname or browser/mobile review was supplied. Hosted transport,
renewal/expiry alerts and broader operations gates remain open.

### Account credential rotation and bounded Compose logs — 31 October 2026

The [credential rotation record](../deployment/evidence/2026-10-31-credentials/README.md)
verifies one account-scoped token replacement over the local TLS profile. The old
token first returned HTTP 200; after the old daemon stopped with exit code zero
and logged `known_final`, the old token returned 401, the new token 200, an
unauthenticated request 401, and a valid token for a wrong account 404. Health
returned 200 and the account was ready without recovery required. The terminal
cancelled account's balances, reservations, plan, child, fills and policy state
were unchanged, and both pinned reports matched their original canonical hashes.
No valid trading command was issued. The record covers disruptive rotation for
one daemon, not hosted delivery, browser behavior, a cohort, public hosting or
database-password rotation.

Commit `a4c8625` configures Docker's `json-file` driver for all five Compose
services, with 10 MB files and three files per container. The focused
`tests/test_deployment_logging.py` check passed against resolved Compose
configuration; the whole Python suite was not rerun for this change. This size
limit is not a 30-day retention guarantee. Existing containers were not
recreated, so they have not adopted the new logging settings. Database, bundle
and recording volume growth is not bounded by this setting. Recording aggregate
work in Go remains in progress.
