# Full implementation progress

Scope: `docs/go-runtime-plan.md` plus the trader workflow, policy evaluation/promotion, and operational gates in `docs/implementation-plan.md`. The active goal stays open until all required implementation and verification is complete. A working replay slice is not the completion condition.

## Verified starting state

- Python reference environment, accounting, recorder, baseline CLI, PPO integration, and 40 checks were implemented before this migration.
- Migration plan is committed as `9851b87`.
- Go 1.26.8 and Protobuf compiler are available on the implementation host.
- Docker's configured daemon is unavailable; a local PostgreSQL 18.6 development instance was provisioned instead. Earlier integration work reports passing real-PostgreSQL race checks for account transitions, ownership, reservations, dispatch and the observation inbox. This is development evidence, not a production restore or failover drill.
- Public Kraken WebSocket attempts obtained no valid recording. Live capture must be demonstrated before that integration gate can be passed.
- No private exchange credentials, live pilot authorization, or real-money verification has been obtained.

Checkpoint for this planning update: commit `7a0488d`, plus the uncommitted React interface and its documentation. No implementation checks were rerun for this documentation change. The original 40-check record is historical; it is not the size of the expanded suite.

## Committed migration components

- Exact amounts, domain contracts, accounting/compiler, deterministic Go replay and common Python economic fixtures.
- Shared Go fee, reservation and observation construction; feature vectors compared within two float32 ULPs on approved fixtures.
- Generated Go/Python protobuf contracts with reproducible generation; bounded replay RPC with isolated environment handles and retry identity.
- Python Go-backed Gymnasium/vector/PPO integration, terminal reports, invalid-trajectory handling and configurable operation budgets.
- Public Kraken parser, recorder, reconnect/resnapshot logic, normalization and recording bounds. Real capture remains unverified.
- PostgreSQL migrations and bounded connection pool; transactional account/journal/command/reservation/outbox state; ownership and dispatch fencing; durable ordered observation inbox.
- Durable paper account actors, trade-driven passive fills, shared baseline scheduler, persisted source provenance and arrival benchmark.
- Python policy worker and Go client, with bounded local inference, bundle/schema/request checks and a real PPO proposal-boundary integration. Actor-driven asynchronous inference/fallback remains unfinished.
- Account-scoped journal metadata, authenticated HTTP commands, exact previews, curated execution projections, recorded-data paper CLI and bounded paper web daemon. Full historical reports/session replay remain unfinished.
- Shared incremental normalization and durable public receipt callbacks; real public capture remains unverified.
- Reproducible native/bridge benchmark tooling and recorded profiles. The measured Go bridge remains slower than Python for the checked-in synthetic workload, so Python stays the default training backend.

Representative commits: `d953990` (replay), `7f214d6` (bindings), `6ddb984` (RPC), `d5c1306` (Gym), `c9f221f` (PPO), `3797cda` (benchmarks), `530cb21` (store), `f64a120` (inbox), `ca07992` (shared execution rules), `0dc5cc3` (operation budgets), `a5933c1` (paper actors), `9996cd5` (Python worker), `4b2eea6` (Go policy boundary), `e6810af` (baseline scheduling), `49da8a4` (paper CLI), `a030615` (API), `352e29b` (incremental public stream), and `7a0488d` (paper server). See the Git history for their component boundaries; code presence does not pass every phase's exit gate.

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

The latest user request is to plan the Go rewrite for scale. The updated plan retains committed components and defines the remaining delivery sequence; it does not restart the rewrite.

The paper runtime, Python worker, Go client, API and paper CLI/server are committed. React code in `apps/web/` and `docs/trader-ui.md` is present but uncommitted; complete workflow review and evidence are still required. Current HTTP updates poll, journal reads expose metadata, model proposals are not integrated into actor scheduling, and no product capacity benchmark has passed.

Next milestones, each split into component commits (details and gates in the Go runtime plan, section 16):

1. Complete/review the paper desk and add full session history/reports/replay.
2. Define financial/control revisions versus market/source versions; instrument transaction/snapshot/WAL cost before reducing writes.
3. Integrate asynchronous Python proposals with actor revalidation, admission, cadence staggering and one deterministic fallback per decision slot.
4. Demonstrate OS process-kill recovery and controlled sender isolation/ownership transfer.
5. Implement measured shared-ingest/multi-account operation with separate book-view and ordered paper-trade delivery contracts.
6. Complete immutable evaluation/promotion, packaging/CI, workload tooling, metrics and operator/restore runbooks.
7. Integrate the private adapter and obtain real venue evidence before an explicitly authorized bounded pilot.
8. Publish load/fault/24-hour soak/restore evidence, set supported admission, then add account shards only when measured demand requires them.

Product admission, the Go training-default switch, and live authorization each require their own evidence and rollback decision. A successful paper session or replay benchmark cannot pass the other gates. No tests or runtime operations are required by this documentation-only planning update.

Account shards, separate ingest/inference deployments and storage partitioning are triggered by measured bottlenecks. Public events are applied once per instrument; account decisions consume shared versioned books. Capacity is admitted from demonstrated latency, recovery and resource headroom. The measured replay-bridge gap has its own optimization workstream and does not block baseline paper-product delivery.
