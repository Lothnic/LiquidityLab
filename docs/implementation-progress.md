# Full implementation progress

Scope: `docs/go-runtime-plan.md` plus the trader workflow, policy evaluation/promotion, and operational gates in `docs/implementation-plan.md`. The active goal stays open until all required implementation and verification is complete. A working replay slice is not the completion condition.

## Verified starting state

- Python reference environment, accounting, recorder, baseline CLI, PPO integration, and 40 checks were implemented before this migration.
- Migration plan is committed as `9851b87`.
- Go 1.26.8 and Protobuf compiler are available on the implementation host.
- Docker's configured daemon is unavailable; a local PostgreSQL 18.6 development instance was provisioned instead. Earlier integration work reports passing real-PostgreSQL race checks for account transitions, ownership, reservations, dispatch and the observation inbox. This is development evidence, not a production restore or failover drill.
- Public Kraken WebSocket attempts obtained no valid recording. Live capture must be demonstrated before that integration gate can be passed.
- No private exchange credentials, live pilot authorization, or real-money verification has been obtained.

Checkpoint: 28 October 2026 through `2d54659`. The full uncached Go race suite passed with development PostgreSQL and the real Python/replay paths configured; Python passed 87 checks, UI passed 46 checks and a production build, Ruff and generated binding drift checks passed. A real Python worker drove the Go paper actor; worker loss triggered its configured baseline and the session completed. Session history/creation-revision inspection and JSON export were exercised in the browser. Local paper create/resume and OS crash-boundary recovery are now committed, including exact accepted-effect reconstruction and suspended-old-sender fencing. These are synthetic software checks, not policy quality, real venue or capacity evidence. See the verification record for scope and remaining gates.

## Committed migration components

- Exact amounts, domain contracts, accounting/compiler, deterministic Go replay and common Python economic fixtures.
- Shared Go fee, reservation and observation construction; feature vectors compared within two float32 ULPs on approved fixtures.
- Generated Go/Python protobuf contracts with reproducible generation; bounded replay RPC with isolated environment handles and retry identity.
- Python Go-backed Gymnasium/vector/PPO integration, terminal reports, invalid-trajectory handling and configurable operation budgets.
- Public Kraken parser, recorder, reconnect/resnapshot logic, normalization and recording bounds. Real capture remains unverified.
- PostgreSQL migrations and bounded connection pool; transactional account/journal/command/reservation/outbox state; ownership and dispatch fencing; durable ordered observation inbox.
- Durable paper account actors, trade-driven passive fills, shared baseline scheduler, persisted source provenance and arrival benchmark.
- Owned PostgreSQL paper effects, fresh post-takeover reconstruction, exact configuration compatibility, create/resume CLI and OS process crash/suspended-sender evidence. Private exchange fencing remains separate.
- Python policy worker and Go client, with bounded local inference, bundle/schema/request checks and a real PPO proposal-boundary integration. Actor-driven asynchronous inference/fallback, pinned parent bundles and durable request/outcome audit are committed.
- Account-scoped journal metadata, authenticated HTTP commands, exact previews, curated execution projections, recorded-data paper CLI and bounded paper web daemon. Revision-pinned session reports and scoped timeline reads are committed; historical market counterfactual replay remains unfinished.
- Shared incremental normalization and durable public receipt callbacks; real public capture remains unverified.
- Financial/control authority revisions separate from receipt/HOLD journal bookkeeping, preserving exact command retries and legacy revision checks.
- Committed React paper desk, validated account/preview/command/journal payloads, scope/request isolation, exact plan bindings, unknown-outcome retry, stale-view gating and responsive layouts. Synthetic create/start/fills/pause/resume/cancel workflow and a waiting first-slice plan were exercised in a real browser; design-user evidence remains missing.
- Reproducible native/bridge benchmark tooling and recorded profiles. The measured Go bridge remains slower than Python for the checked-in synthetic workload, so Python stays the default training backend.

Representative commits: `d953990` (replay), `7f214d6` (bindings), `6ddb984` (RPC), `d5c1306` (Gym), `c9f221f` (PPO), `3797cda` (benchmarks), `530cb21` (store), `f64a120` (inbox), `ca07992` (shared execution rules), `0dc5cc3` (operation budgets), `a5933c1` (paper actors), `9996cd5` (Python worker), `4b2eea6` (Go policy boundary), `e6810af` (baseline scheduling), `49da8a4` (paper CLI), `a030615` (API), `352e29b` (incremental public stream), `7a0488d` (paper server), `a102a5e` (authority revisions), and `dc521e5` (paper desk). See the Git history for their component boundaries; code presence does not pass every phase's exit gate.

## Requirements ledger

Checkboxes represent full exit criteria, including required integration evidence and the final audit. An unchecked phase can contain substantial committed implementation; it is not an instruction to rebuild those components. Per-phase checkpoints appear in section 12 of the Go runtime plan.

- [ ] Phase 1: versioned shared schemas, generated bindings, contract validation, common conformance vectors, preserved Python checks.
- [ ] Phase 2: Go exact amounts, domain/ledger/compiler, event ordering, book/depletion, features, settlement, baselines, cross-language economic parity.
- [ ] Phase 3: Go public recorder/normalizer, checksum/depth/precision parity, corruption/recovery, verified real capture.
- [ ] Phase 4: bounded batched replay RPC, isolated handles, request identity and retry semantics, Python Gym/vector adapter, PPO roundtrip, transport benchmarks.
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

Next work follows section 16 of the Go runtime plan. The Stage 2 backlog now specifies five separate commits: bounded telemetry, store instrumentation, actor/inference instrumentation, a durable paper workload runner, and reproducible baseline evidence. Stage 3 follows with supervisor/routing, shared books, ordered paper facts, and bounded scheduling/admission. These are planned changes, not passed capacity gates.

1. Instrument transaction/snapshot/WAL cost, recovery debt, control latency and inference before reducing durable writes.
2. Add shared books, ordered paper-trade delivery, account supervision/routing, worker-pool admission and staggered multi-account schedules.
3. Optimize measured persistence/inference constraints while preserving exact accounting and recoverable source ordering.
4. Complete historical market replay, immutable evaluation/promotion and design-user validation.
5. Complete private adapter/reconciliation, authorized read-only venue evidence, packaging/CI, metrics and backup/restore runbooks. Real order lifecycle evidence belongs inside a separately authorized bounded pilot.
6. Publish load/fault/24-hour soak/restore evidence and supported admission before conditional executor/storage sharding.

Product admission, the Go training-default switch, and live authorization require separate evidence and rollback decisions. The latest browser cohort completed its paper parent and the daemon shut down with `known_final=true`; all seven intents are final, no local held reservations remain, and the source inbox is drained. The verification worker and daemon have stopped. No exchange orders were sent.

Account shards, separate ingest/inference deployments and storage partitioning follow measured bottlenecks. Shared public feeds and multiple accounts remain planned; the current daemon still manages one created or resumed paper account. The slower measured replay bridge has its own optimization workstream and does not block baseline paper-product delivery.
