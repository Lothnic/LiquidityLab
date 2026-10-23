# Full implementation progress

Scope: `docs/go-runtime-plan.md` plus the trader workflow, policy evaluation/promotion, and operational gates in `docs/implementation-plan.md`. The active goal stays open until all required implementation and verification is complete. A working replay slice is not the completion condition.

## Verified starting state

- Python reference environment, accounting, recorder, baseline CLI, PPO integration, and 40 checks were implemented before this migration.
- Migration plan is committed as `9851b87`.
- Go 1.26.8 and Protobuf compiler are available on the implementation host.
- Docker's configured daemon is unavailable; a local PostgreSQL 18.6 development instance was provisioned instead. Earlier integration work reports passing real-PostgreSQL race checks for account transitions, ownership, reservations, dispatch and the observation inbox. This is development evidence, not a production restore or failover drill.
- Public Kraken WebSocket attempts obtained no valid recording. Live capture must be demonstrated before that integration gate can be passed.
- No private exchange credentials, live pilot authorization, or real-money verification has been obtained.

Checkpoint for this planning update: commit `0dc5cc3`, plus uncommitted runtime and inference drafts. No implementation checks were rerun for this documentation change. The original 40-check record is historical; it is not the size of the expanded suite.

## Committed migration components

- Exact amounts, domain contracts, accounting/compiler, deterministic Go replay and common Python economic fixtures.
- Shared Go fee, reservation and observation construction; feature vectors compared within two float32 ULPs on approved fixtures.
- Generated Go/Python protobuf contracts with reproducible generation; bounded replay RPC with isolated environment handles and retry identity.
- Python Go-backed Gymnasium/vector/PPO integration, terminal reports, invalid-trajectory handling and configurable operation budgets.
- Public Kraken parser, recorder, reconnect/resnapshot logic, normalization and recording bounds. Real capture remains unverified.
- PostgreSQL migrations and bounded connection pool; transactional account/journal/command/reservation/outbox state; ownership and dispatch fencing; durable ordered observation inbox.
- Reproducible native/bridge benchmark tooling and recorded profiles. The measured Go bridge remains slower than Python for the checked-in synthetic workload, so Python stays the default training backend.

Representative commits: `d953990` (replay), `7f214d6` (bindings), `6ddb984` (RPC), `d5c1306` (Gym), `c9f221f` (PPO), `3797cda` (benchmarks), `530cb21` (store), `f64a120` (inbox), `ca07992` (shared execution rules), and `0dc5cc3` (operation budgets). See the Git history for their component boundaries; code presence does not pass every phase's exit gate.

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

Uncommitted drafts are preserved in `internal/runtime/`, `src/liquiditylab/inference/`, and `tests/test_inference.py`. The paper actor draft has durable submission/fill facts, account serialization, reservations and recovery paths; automatic passive fills, feed/scheduler wiring, control fairness and process-level restart evidence still need completion. The Python worker draft has bundle/schema/request validation and bounded local inference. Its worker reports 21 focused checks passing, but it still needs integration review, the Go client, eligibility/fallback handling and a separate commit.

Next milestones, each split into component commits:

1. Finish the actor, shared passive matcher, baseline scheduler and end-to-end paper CLI; demonstrate recovery at process boundaries.
2. Integrate Python inference with the Go client and stale-response/fallback behavior.
3. Deliver authenticated trader commands, React supervision and the execution journal; obtain usability evidence.
4. Complete immutable evaluation/promotion, reproducible packaging/CI, workload tooling and operational runbooks.
5. Integrate the private adapter, demonstrate load/fault handling and restore/reconciliation at pilot capacity, then obtain authorization for the bounded live pilot. Complete the final capacity/24-hour soak gates before increasing supported admission.

Account shards, separate ingest/inference deployments and storage partitioning are triggered by measured bottlenecks. Public events are applied once per instrument; account decisions consume shared versioned books. Capacity is admitted from demonstrated latency, recovery and resource headroom. The measured replay-bridge gap has its own optimization workstream and does not block baseline paper-product delivery.
