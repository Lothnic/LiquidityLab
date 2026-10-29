# Full implementation progress

Scope: `docs/go-runtime-plan.md` plus the trader workflow, policy evaluation/promotion, and operational gates in `docs/implementation-plan.md`. The active goal stays open until all required implementation and verification is complete. A working replay slice is not the completion condition.

## Verified starting state

- Python reference environment, accounting, recorder, baseline CLI, PPO integration, and 40 checks were implemented before this migration.
- Migration plan is committed as `9851b87`.
- Go 1.26.8 and Protobuf compiler are available on the implementation host.
- Docker's configured daemon is unavailable; a local PostgreSQL 18.6 development instance was provisioned instead. Earlier integration work reports passing real-PostgreSQL race checks for account transitions, ownership, reservations, dispatch and the observation inbox. This is development evidence, not a production restore or failover drill.
- A bounded real public Kraken BTC/USD capture is committed in `8c7e329`, with Go/Python normalization conformance. It verifies this capture slice only; broader corruption/recovery coverage and operational gates remain.
- No private exchange credentials, live pilot authorization, or real-money verification has been obtained.

Checkpoint: 29 October 2026, implementation through `75d71fd`. The local [Phase 1](phase1-contract-audit.md), [Phase 2](phase2-core-audit.md), [Phase 3](phase3-public-data-audit.md) and [Phase 4](phase4-bridge-audit.md) gates are complete. The fresh clean-source [combined conformance run](../conformance/results/20261029-phase3-4.json) passes all five steps and 216 Python checks with zero skips, failures or errors. All four fixture hashes are verified; the previous three are unchanged. The full uncached Go race suite passes for these Go sources with development PostgreSQL and actual Python/replay paths. Frontend evidence is 48 checks and a production build. Runtime `069609b` now yields between bounded fact-reduction turns, preserves queued holds through debt, separates local control receipts from finality, and passes 512-fill fresh-owner recovery plus ordinary process crash/recovery checks. Supported admission and recovery headroom remain unproved.

Recent delivery includes the real 50-frame Kraken capture with 45-event Go/Python conformance, bounded runtime instrumentation, seven retained HTTP/inference/profiling workload reports, a separate Go blocking profiler, and stronger response/terminal-state audit checks. Six workload reports completed; the allocation-profile trial rejected 375 source offers and remains invalid. See the [envelope evidence](../benchmarks/results/runtime-envelope-20261028/README.md) and [verification record](verification.md) for scope, checker versions and limits.

Three [follow-up audited workloads](../benchmarks/results/runtime-audited-20261028/README.md) exercise the stronger checks from a clean build. Baseline and real Python HTTP runs each completed 1,000 source updates and 400 reads. The separate blocking-profile trial rejected 290 source offers and remains invalid. Across both v2 bundles there are eight complete and two invalid trials; neither bundle establishes supported admission or load-plus-recovery headroom.

Earlier browser evidence exercised the real Python worker, worker-loss fallback, session history and JSON export on synthetic input. Paper create/resume and OS crash recovery preserve accepted effects and fence suspended old senders. These observations do not establish policy quality, venue execution or supported capacity.

## Committed migration components

- Exact amounts, domain contracts, accounting/compiler, deterministic Go replay and common Python economic fixtures.
- Shared Go fee, reservation and observation construction; feature vectors compared within two float32 ULPs on approved fixtures.
- Generated Go/Python protobuf contracts with reproducible generation; bounded replay RPC with isolated environment handles and retry identity.
- Python Go-backed Gymnasium/vector/PPO integration, terminal reports, invalid-trajectory handling and configurable operation budgets.
- Public Kraken parser, recorder, reconnect/resnapshot logic, normalization and recording bounds. Commit `8c7e329` preserves one real 50-frame BTC/USD capture (3.922 s, 44 book messages, one valid snapshot, zero gaps) and a passing 45-event Go/Python conformance record. This verifies normalization on that recording; it is not an SLO, capacity, private-stream, or live-order result.
- PostgreSQL migrations and bounded connection pool; transactional account/journal/command/reservation/outbox state; ownership and dispatch fencing; durable ordered observation inbox.
- Durable paper account actors, trade-driven passive fills, shared baseline scheduler, persisted source provenance and arrival benchmark.
- Owned PostgreSQL paper effects, fresh post-takeover reconstruction, exact configuration compatibility, create/resume CLI and OS process crash/suspended-sender evidence. Private exchange fencing remains separate.
- Python policy worker and Go client, with bounded local inference, bundle/schema/request checks and a real PPO proposal-boundary integration. Actor-driven asynchronous inference/fallback, pinned parent bundles and durable request/outcome audit are committed.
- Account-scoped journal metadata, authenticated HTTP commands, exact previews, curated execution projections, recorded-data paper CLI and bounded paper web daemon. Revision-pinned session reports and scoped timeline reads are committed; historical market counterfactual replay remains unfinished.
- Shared incremental normalization and durable public receipt callbacks; the committed real capture exercises this path with native receive timing and a local paper daemon, without a plan or private-order stream.
- Immutable recording archives (`79a90dd`) and bounded offline baseline comparison now verify source bytes, preserve sealed quiet-tail coverage, and report exact simulated fills/fees/cashflows with explicit assumptions. The reference is also simulated; paper-session/source binding, persisted replay jobs and trader replay UI remain unfinished.
- Financial/control authority revisions separate from receipt/HOLD journal bookkeeping, preserving exact command retries and legacy revision checks.
- Committed React paper desk, validated account/preview/command/journal payloads, scope/request isolation, exact plan bindings, unknown-outcome retry, stale-view gating and responsive layouts. Synthetic create/start/fills/pause/resume/cancel workflow and a waiting first-slice plan were exercised in a real browser; design-user evidence remains missing.
- Reproducible native/bridge benchmark tooling and recorded profiles. The measured Go bridge remains slower than Python for the checked-in synthetic workload, so Python stays the default training backend.

Representative commits: `d953990` (replay), `7f214d6` (bindings), `6ddb984` (RPC), `d5c1306` (Gym), `c9f221f` (PPO), `3797cda` (benchmarks), `530cb21` (store), `f64a120` (inbox), `ca07992` (shared execution rules), `0dc5cc3` (operation budgets), `a5933c1` (paper actors), `9996cd5` (Python worker), `4b2eea6` (Go policy boundary), `e6810af` (baseline scheduling), `49da8a4` (paper CLI), `a030615` (API), `352e29b` (incremental public stream), `7a0488d` (paper server), `a102a5e` (authority revisions), and `dc521e5` (paper desk). See the Git history for their component boundaries; code presence does not pass every phase's exit gate.

## Requirements ledger

Checkboxes represent full exit criteria, including required integration evidence and the final audit. An unchecked phase can contain substantial committed implementation; it is not an instruction to rebuild those components. Per-phase checkpoints appear in section 12 of the Go runtime plan.

- [x] Phase 1: versioned shared schemas, generated bindings, contract validation, common conformance vectors, preserved Python checks. See the [requirement/evidence audit](phase1-contract-audit.md) and retained 198-check conformance run; this is a local contract gate.
- [x] Phase 2: Go exact amounts, domain/ledger/compiler, event ordering, book/depletion, features, settlement, baselines, approved cross-language economic parity. See the [local core/replay acceptance audit](phase2-core-audit.md); this does not switch the training default or pass performance/live gates.
- [x] Phase 3: Go public recorder/normalizer, checksum/depth/precision parity, deterministic corruption/recovery, verified real capture. See the [local public-data audit](phase3-public-data-audit.md); recording pressure and sustained operational validation remain wider gates.
- [x] Phase 4: bounded batched replay RPC, isolated handles, request identity/retry and failure semantics, Python Gym/vector adapter, PPO roundtrip and measured bridge comparison. See the [bridge audit](phase4-bridge-audit.md); Python remains the training default and performance/policy gates remain open.
- [ ] Phase 5: PostgreSQL schema/migrations, serialized account authority, reservations/outbox, dispatch revalidation, command identity, restart/recovery/failover checks, durable paper sessions.
- [ ] Phase 6: Python inference worker, bundle/schema verification, response binding, deadlines/backpressure, Go policy client, deterministic fallback/pause behavior.
- [ ] Phase 7: authenticated trader API, React plan/supervision/history/replay interface, cancellation-state clarity, design-user usability evidence.
- [ ] Phase 8: private exchange transport/streams, readiness handoff, fees/rate budgets, reconciliation, ownership transfer, authorized bounded pilot and actual fill evidence.
- [ ] Phase 9: reproducible workload/hardware manifest, throughput/latency/resource results, failure/load checks, 24-hour soak, backup/restore drill, operational runbooks.
- [ ] Policy evaluation: chronological holdouts, immutable evaluation specification, multiple seeds and fill assumptions, complete failure coverage, promotion gating.
- [ ] Deployment: separated replay/paper/live profiles, secret isolation, reproducible generated code/builds, CI, tested packaging and operator documentation.
- [ ] Final audit: requirement-by-requirement evidence against both plans; unresolved external gates remain explicitly incomplete.

## Current work

The active goal continues full implementation of both plans with component commits and reviewed subagents. Stage 0 of the current Go delivery plan is committed:

- `f9125d7`: revision-pinned PostgreSQL session reads and migration.
- `ec293eb`: asynchronous actor policy proposals, bounded inference/fallback, restart/late-result checks and paper daemon opt-in.
- `18eeea0`: scoped reports, frozen terminal valuation, policy provenance and historical finality checks.
- `6433711`: history UI, exact report validation, stale-response guards, bounded fill pages and exports.

Local paper recovery now adds `50f7294` (fresh post-claim bootstrap), `e0d68ba` (paper effect store fence), `59e9ff7` (owned runtime/recovery), `d710fce` (create/resume CLI), `c27bbec` (ordinary-binary SIGKILL/reap/resume), and `2d54659` (ten controlled crash/suspended-sender scenarios). Recovered parents stay paused; accepted facts determine exact fills/fees/balances, attempted orders are not resubmitted, and no initial balance flag is accepted on resume. Private exchange fencing remains unimplemented.

Next work follows section 16 of the Go runtime plan. Stage 2 instrumentation and the v2 durable runner are committed, including optional HTTP polling, controlled inference, and profiling. The checked-in 11-report baseline remains schema v1; it includes three metrics-off/on baseline trials and five ramp trials, with invalid source-delivery outcomes retained. Seven v2 reports now exercise HTTP reads, controlled healthy/expired inference, the real unvalidated Python worker, and separate CPU/allocation profiles. Six completed; the allocation-profile trial rejected 375 of 2,500 offered source events and is invalid. These single trials leave the Stage 2 recovery/headroom and attribution exit gate open. Stage 3 still requires supervisor/routing, shared books, ordered paper facts, and bounded scheduling/admission. Committed implementation and narrow capture evidence do not pass the capacity gates.

1. Extend ordinary-process recovery under demand to accepted backlog with the bounded actor. Count/time quanta, controls/reads during a 512-fill prefix, exact fresh-owner reconstruction, queued-hold protection and worker expiry are implemented and race checked. Three new [steady 50 Hz source / 20 Hz HTTP trials](../benchmarks/results/runtime-quantum-20261029/README.md) complete, while a 250 Hz ramp rejects 268/2,500 and remains invalid. Blocking profiles, scoped PostgreSQL activity and modest load-plus-recovery evidence exist; service demand by operation/instrument and supported admission/headroom remain open before reducing durable writes or activating shared delivery.
2. Add shared books, ordered paper-trade delivery, account supervision/routing, worker-pool admission and staggered multi-account schedules.
3. Optimize measured persistence/inference constraints while preserving exact accounting and recoverable source ordering.
4. Bind verified recordings to paper sessions, add persisted authorized replay jobs and trader replay UI around the offline comparison, and complete immutable evaluation/promotion and design-user validation.
5. Complete private adapter/reconciliation, authorized read-only venue evidence, packaging/CI, metrics and backup/restore runbooks. Real order lifecycle evidence belongs inside a separately authorized bounded pilot.
6. Publish load/fault/24-hour soak/restore evidence and supported admission before conditional executor/storage sharding.

Product admission, the Go training-default switch, and live authorization require separate evidence and rollback decisions. The latest browser cohort completed its paper parent and the daemon shut down with `known_final=true`; all seven intents are final, no local held reservations remain, and the source inbox is drained. The verification worker and daemon have stopped. No exchange orders were sent.

Account shards, separate ingest/inference deployments and storage partitioning follow measured bottlenecks. Shared public feeds and multiple accounts remain planned; the current daemon still manages one created or resumed paper account. The slower measured replay bridge has its own optimization workstream and does not block baseline paper-product delivery.
