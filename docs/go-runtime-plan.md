# LiquidityLab: Go runtime and Python RL migration

Decision plan prepared 23 October 2026. This document plans the migration; it does not claim that a Go implementation, distributed execution system, or production benchmark exists.

## 1. Decision and scope

Make **Go the authoritative product and execution runtime**, with **Python owning the RL interface, training, evaluation, and initially model inference**. Keep the trader interface in TypeScript/React.

Go is the selected engineering direction for the persistent service, concurrent venue connections, account ownership, recovery, and deployment. Whether it is the best-performing implementation at the intended scale must be established by measurement. Network behavior, database durability, exchange limits, allocation rates, and model inference can dominate language-level differences.

This decision supersedes the earlier plan's Python/FastAPI product backend, SQLite production ledger, and proposed NautilusTrader execution-engine dependency. Existing Python code remains the reference implementation during migration. No private exchange integration or production database exists to migrate today.

The first release continues to support crypto spot, one venue, and one active parent order per connected account. The architecture supports many accounts without permitting concurrent writers to any one account. More instruments and accounts come before multiple venues or several simultaneous plans against one balance.

The product remains an execution assistant: a trader chooses the trade and its size, price boundary, and deadline. A policy proposes execution actions. The Go runtime checks and carries them out.

## 2. Current implementation and what moves

The starting point is the Python v0.1.0 implementation and its 40 passing checks, including a CPU PPO save/load roundtrip. Those results establish a working foundation, not execution quality or capacity at scale.

Move authoritative behavior into Go:

- `contracts.py`: instrument rules, precise amounts, plan constraints, action meanings, and validation.
- `accounting.py`: balance changes, fees, fill idempotency, remaining quantity, and cost accounting.
- `compiler.py`: policy-action conversion into bounded child-order intents.
- `book.py`: book validity and simulated consumption debt.
- `replay.py`: event ordering, simulated fills, order arrival/cancellation/settlement, features, and execution metrics.
- `data/kraken.py`: public stream decoding, checksum validation, capture, gap handling, and normalization.
- `policies.py`: scheduled and liquidity-aware baseline execution behavior.

Keep in Python:

- `env.py`: Gymnasium API, action and observation spaces; its eventual backend calls Go.
- `training.py`: PPO/SB3 integration, invalid-trajectory guard, and policy bundles.
- Model loading/inference, evaluation tooling, analysis, and experiment configuration.
- The present Python replay implementation as a temporary conformance reference.

Keep the current Python CLI working through the migration. Introduce a Go product CLI/server separately. Remove reference execution code from the default training path only after Go conformance and bridge performance pass. Existing training artifacts are unvalidated and must not be silently promoted to a different engine.

## 3. Target architecture

![Go runtime and Python RL architecture](go-runtime-architecture.svg)

Start with a **modular Go service**, a **Python policy worker**, and **PostgreSQL**. An offline Go replay worker is a separate mode of the same execution code. Serve the built browser interface from the Go service. Split deployments later when measured workloads require independent scaling.

Go owns:

- Trader API and authenticated commands.
- Plan revisions, account reservations, and durable execution history.
- Canonical market state and feature construction.
- Hard boundaries, action compilation, and child-order state.
- Venue credentials, order transport, reconciliation, and stop/pause behavior.
- The deterministic core used by both replay and live operation.

Python owns:

- Gymnasium wrapping and vectorized training orchestration.
- Policy optimization, evaluation, and bundle publication.
- Inference on a versioned feature vector; it returns a semantic action.

The inference worker receives neither exchange credentials nor a direct order-submission capability. A model response is a proposal. Go validates it against current account state and recompiles it against the current book before creating any order intent.

PostgreSQL is the durable source for application state. The exchange remains authoritative for actual acceptance, fills, and balances. A database record cannot make an unknown exchange outcome known.

## 4. Shared contracts first

Create versioned Protocol Buffer schemas under `proto/liquiditylab/v1/` and generate Go/Python bindings. Use them for inference and the offline replay bridge. Keep the browser API as versioned HTTP JSON with a documented mapping to the domain contracts. Protobuf supports generated contracts and has specific evolution rules; reserve removed field numbers and do not reuse enum meanings. [Protocol Buffers guide](https://protobuf.dev/programming-guides/proto3/)

Core contracts include:

- Instrument identity and immutable metadata revision.
- Exact decimal amount, currency, price, quantity, fee rate, and fee amount.
- Plan ID/revision, account ID, hard boundaries, start, deadline, and fallback choice.
- Market event, receipt-session identity, local receipt index, and source validity.
- Observation schema, ordered feature names, and preprocessing fingerprint.
- Decision request/response, semantic action, and decision expiry.
- Child intent, command ID, venue ID, execution identity, and reconciliation outcome.
- Episode specification, step result, settlement status, and economic-score validity.

### Amounts and arithmetic

Never transport money as binary floating point. Use a canonical decimal representation with a signed integer coefficient encoded as a string and an explicit scale. Currency is mandatory for monetary amounts. Validate digit/scale limits before allocating or calculating.

The initial Go decimal type should wrap checked, exact arithmetic using private `math/big` state, with immutable value semantics and explicit rounding operations. Preserve raw source strings separately for venue checksum calculation. Profile this representation before substituting narrower integer fast paths.

Define and test operations for addition, multiplication, comparison, floor/ceil to a tick/lot, and conservative fee reservation. Specify division rounding and the supported precision envelope rather than inheriting a language's default context. Use higher internal precision for products; reject unsupported amounts instead of wrapping, clipping money, or silently rounding through float64. PostgreSQL numeric columns must use the same approved bounds.

Python's default Decimal precision is not the financial specification. The conformance harness must use sufficient explicit precision. If an extreme-value fixture exposes an existing Python rounding defect, correct and version the specification rather than reproducing a known defect in Go.

### Time and action versions

Use integer nanoseconds for relative replay time. Store live absolute deadlines in UTC and use a local monotonic clock for waiting while the process is alive. A monotonic timestamp is not transferable across restarts or hosts. Clock discontinuities trigger revalidation before new execution.

Legacy float-second tapes need an explicit import rule: parse the written decimal representation, quantize to nanoseconds with a documented rule, and preserve receipt order. Characterize differences caused by old float arithmetic at ties. Introduce `LiquidityLab-SpotExecution-v1` for the Go-backed environment rather than declaring this migration invisible to existing bundles.

The wire enum reserves zero for `UNSPECIFIED`; assign explicit values to HOLD, CANCEL_CHILD, PASSIVE_SMALL, PASSIVE_NORMAL, AGGRESSIVE_SMALL, and AGGRESSIVE_NORMAL. The current Python action integers are 0–5, so generated bindings require an explicit mapping. Unknown values are rejected. Cancelling a child does not cancel the parent obligation.

## 5. One execution core for replay and production

Implement a deterministic Go reducer that accepts explicit market, account, control, and timer events and produces state changes plus effects. It must not read wall time, perform network I/O, or access a database internally. Supply clocks, persistence, and venue adapters at the runtime boundary.

Use the same amount logic, plan rules, compiler, reservation accounting, feature builder, and baseline policies in live and replay modes. Fill sources differ: replay uses a model, while live uses confirmed venue events. Identical code does not imply identical market outcomes.

Preserve these current semantics through explicit conformance cases:

- Receipt events win equal-time ties before simulated arrival, then cancellation.
- A cancellation request retains reservations and can race with a fill.
- Replacements wait for the prior child to become known-final.
- A passive quote touch alone does not imply a fill.
- Simulated liquidity debt survives unchanged book updates and decision intervals.
- A feed gap invalidates an active replay episode.
- Settlement freezes the residual price reference at the decision cutoff.
- Known partial completion has a valid terminal cost; unknown settlement does not.
- Once settlement is complete, later unrelated tape gaps do not invalidate that result.
- Terminal cost and incomplete-quantity penalty are applied once.

Before making Go the default, compare both implementations on the same approved vectors. Monetary balances, fees, quantities, order outcomes, and cost components must match exactly. Boolean/schema/action meanings must match exactly. Define float32 feature tolerance in ULPs and a reward tolerance in the contract; start with two float32 ULPs for derived features and investigate discrepancies rather than widening tolerances automatically.

Record intended time-format or arithmetic corrections as v1 behavior changes. Re-evaluate policies trained against v0 after those changes.

## 6. Python Gym interface to Go

Run a local Go replay process for each training job, managing a bounded set of independent simulated environments. Python's Gym wrapper owns opaque environment handles and calls the replay process through a loopback or Unix-socket gRPC connection.

Proposed offline methods:

- `CreateBatch`: load a dataset manifest once, create N isolated environments, and return handles plus engine/schema versions.
- `ResetBatch`: select validated episode specifications and seeds, return observations and reset metadata.
- `StepBatch`: submit one semantic action per handle and return observations, rewards, termination/truncation, and score validity.
- `GetReports`: retrieve final detailed reports outside the hot training step.
- `CloseBatch`: release handles and mapped dataset resources.

Batch across parallel environments. Do not make one cross-language RPC for every market tick, and do not resubmit an entire recording with every step. The Go process advances the tape internally to the next decision or settlement boundary. One environment's state must never share liquidity debt, balances, random state, or order IDs with another.

A replay RPC must carry a request sequence and stable operation identity. A timed-out step may already have advanced the environment, so the server caches the last result or exposes status by request identity; a client cannot blindly retry and advance the same episode twice. Limit environment count, payload size, outstanding work, and report size.

The Python training guard rejects invalid/unresolved outcomes before they enter PPO rollouts. Policy-independent invalid intervals are excluded identically before evaluation. Policy-dependent failures stay in failure statistics and block promotion.

Keep an explicit `backend="python-reference"` during transition. Make `backend="go"` the new v1 environment only after parity, isolation, cancellation, and throughput gates pass. Avoid cgo/CPython bindings initially; consider a native binding only if profiling shows batched local RPC remains a meaningful bottleneck.

## 7. Live inference boundary

A decision request includes tenant/account/session identity, session revision, account/order-state revision, observation schema/hash, policy bundle checksum, market snapshot identity, request ID, observation vector, and validity budget. The Python worker verifies compatibility before inference and echoes the binding metadata with its action.

Set explicit RPC deadlines; gRPC does not provide a useful application deadline automatically. Cancellation must also be handled in the server's work scheduling. [gRPC deadlines](https://grpc.io/docs/guides/deadlines/)

Go allows at most one outstanding inference request per active plan initially. It rejects a response if the plan/account/order state changed, the plan stopped, the bundle/schema differs, or the request expired.

Do **not** require that no newer market tick arrived. That would reject almost every response on an active book. Permit newer market data when the request remains within its freshness budget, then compile the semantic action against the latest valid book and current hard boundaries. Log both observation and execution snapshot identities. A model does not submit a stale raw price directly.

If inference misses its budget or the queue is full, apply the plan's preselected pause or Go baseline behavior. Operator cancellation, account events, and reconciliation do not wait for Python. Bound Python queues and CPU thread counts. Pin a policy bundle per active session; updates affect new sessions unless an explicit controlled switch is implemented later.

Use one Python worker initially. Later replicate inference workers by compatible bundle/model demand, with bounded batching that fits the live latency budget. Do not combine offline training and production inference in the same resource pool.

An ONNX deployment is a later optimization requiring export and action-parity checks. A small Python inference worker is the first implementation; the plan does not assume an official Go inference binding or a lossless export for every future model.

## 8. Durable runtime and account ownership

Use one serialized account actor per connected exchange account. It owns reservations for all currencies, known/unknown orders, and the account-state revision. Parent plans execute under that authority. Independent account actors may run concurrently in a bounded worker process.

API replicas validate access and append revision-checked, idempotent commands. They never submit orders directly. Account routing is derived from durable ownership, not whichever HTTP process received the request.

PostgreSQL stores plan revisions, command deduplication, intents, outbox entries, fills, reservations, account projections, ownership, policy eligibility, and reconciliation checkpoints. Apply the account transition, reservation, and outbox insert in one transaction. Use explicit row locking/constraints where required; PostgreSQL documents both row-level locks and advisory locks. [Locking reference](https://www.postgresql.org/docs/current/explicit-locking.html)

Do not write every public market tick into the transactional database. Record public streams in append-only chunks with manifests; build shared books in memory. Add object storage and analytical formats when measured retention/query requirements justify them.

The outbox dispatcher persists attempt state before transport. A crash after exchange acceptance but before local acknowledgement creates an unknown outcome, not permission to resend. Client IDs and local idempotency do not establish universal exactly-once exchange execution. Reconcile open orders and executions before resolving unknown intents.

An outbox intent is not permanent permission to trade. Immediately before outbound transport handoff, route order creation back through the account authority and revalidate ownership epoch, plan revision/status, deadline, reservations, instrument rules, and current market/account validity. Serialize that authorization with pause/cancel processing. Transactionally obsolete unsent intents when their plan stops, their revision changes, or validity expires. A request already handed to transport is treated as potentially in flight and must be cancelled/reconciled; a pause cannot retract bytes already sent. Record the dispatch authorization revision and snapshot identity.

### Account readiness and external activity

The initial live pilot requires an account under exclusive application management while an execution session is active. This is an explicit operating constraint, not a property conferred by an API key or account actor. It does not require a venue subaccount feature. Pre-existing external open orders block readiness; the application does not cancel someone else's orders to clear that condition. Broader support for concurrently managed/manual account activity is a later capability.

Before the first send and after reconnect, reconcile balances, open orders, recent executions, and local unknown intents using a venue-specific snapshot/stream handoff. Buffer and deduplicate private events against documented source identities/checkpoints and repeat queries where required. The integration milestone must demonstrate the handoff with concurrent events; do not assume REST queries form one atomic snapshot. If a consistent account state cannot be established, remain paused.

Track total/free/locked balances and which local reservations are already reflected in venue holds. Subtract unreflected pending local reservations from venue-free funds; do not subtract known venue holds twice. Recompute revisions and reservations after reconciliation. Unexpected manual trades, deposits/withdrawals, unknown orders, or balance differences invalidate readiness and pause affected execution until explained. If exclusive management cannot be observed for a pilot account, use paper mode until shared-account reservation support is implemented.

### Ownership and failover

Use durable ownership epochs and leases to reject stale database writes. The sender verifies current ownership before dispatch. However, **a database fencing token does not fence the exchange**: an old process may retain a live connection or already have a request in flight.

Initial failover is controlled: pause new sends, isolate/terminate the previous sender, reconcile its known and unknown intents, then enable the new owner. An expired lease alone is insufficient to authorize a second sender. If old-owner isolation cannot be established, stay paused and require operator recovery. Automated high availability requires a separately verified outbound-fencing design and venue capability analysis.

Within a healthy deployment, shard accounts deterministically across executor processes. Rebalance only through draining, ownership transfer, and reconciliation. Keep per-account venue message limits and any wider venue limits authoritative across all workers.

## 9. Market data, backpressure, and ordering

Maintain one shared public feed/book per venue/instrument per ingest shard, then distribute immutable snapshots or versioned references to interested account actors. Account-private events remain isolated by tenant and account.

Preserve exact wire values for Kraken checksums. Retain subscription-depth truncation, explicit deletions, historical trade-snapshot filtering, and resnapshot behavior from the Python recorder. A missed delta invalidates the book until reconstruction succeeds.

Use bounded queues and distinguish their semantics:

- Public book deltas: preserve required order; overflow invalidates/resnapshots the book.
- Account/fill events: stop new execution on a gap or overflow and reconcile from venue history; never silently drop accounting facts.
- Inference work: reject or expire excess requests rather than allowing stale backlog.
- UI snapshots: coalesce replaceable display state, while keeping a durable audit history.
- Recording: monitor write lag and disk capacity; stop presenting an incomplete recording as continuous valid history.

Avoid giving a goroutine to every public market message. Use long-lived bounded workers and serialized reducers where state order matters. Operator stop requests get prompt handling while preserving already observed account-event causality.

## 10. Product API and initial deployment

Use Go's HTTP server for the product API and WebSocket/SSE updates. Use explicit command IDs and expected revisions for plan creation, start, pause, resume, and cancel. Status updates distinguish requested cancellation, confirmed cancellation, and unresolved state.

The initial self-hosted deployment contains:

- One Go process running API, market ingest, account executor, and reconciliation modules.
- One separately supervised Python inference process.
- PostgreSQL with durable storage and tested backups.
- Static React assets served by Go.
- Offline replay/training processes launched separately from the live service.

Maintain explicit replay/paper/live profiles. Paper profiles cannot construct the live order transport or access production credentials. Browser sessions and Python workers never receive exchange secrets. Production remote access requires authenticated commands and transport, even before a hosted multi-user service exists.

Scale the API, ingest, execution, and inference deployments independently only as their measured bottlenecks emerge. A message broker, Kubernetes, and a distributed cache are optional later decisions. The first scale step is account partitioning and shared market data, not service proliferation.

## 11. Proposed repository layout

Keep existing Python paths working while adding:

```text
liquiditylab/
  go.mod / go.sum
  cmd/liquiditylabd/             # API/ingest/executor modes
  cmd/liquiditylab/              # Product and offline Go CLI
  internal/domain/              # Exact amounts, contracts, immutable values
  internal/execution/           # Reducer, compiler, reservations, baselines
  internal/features/            # Versioned observation construction
  internal/replay/              # Simulation clock, fills, settlement, batch RPC
  internal/marketdata/           # Books, recording, fanout and validity
  internal/venue/kraken/         # Protocol parsing and authenticated transport
  internal/runtime/             # Account actors, ownership and recovery
  internal/store/postgres/       # Transactional state and outbox
  internal/inference/            # Python policy client and eligibility
  internal/api/                  # HTTP/operator commands and updates
  proto/liquiditylab/v1/         # Language-independent contracts
  gen/go/                       # Generated Go protocol types
  src/liquiditylab/              # Existing Python package, retained
    proto/                      # Generated Python protocol types
    go_backend.py               # Gym/vector environment adapter
    inference/                  # Python model worker
  apps/web/                     # Trader interface
  migrations/                   # Ordered PostgreSQL migrations
  conformance/                  # Versioned common input/output vectors
  benchmarks/                   # Workload manifests and measured results
  deploy/                       # Local compose and process configuration
```

Choose and pin a supported Go release, Protobuf compiler/plugins, Go database driver, and RPC packages during the first implementation milestone. Generated code is reproducible and checked for schema drift. Credentials and market recordings stay outside version control. Repository paths are proposed; no Go module has been added by this planning task.

## 12. Migration phases and gates

### Phase 1 — specification and fixtures: 2–3 focused days

Freeze v1 amount/time/action contracts, extract common golden cases from the existing tests, and add malformed/overflow/rounding/cancellation fixtures. Record all intentional differences from the Python implementation.

Exit: generated Go/Python messages, an executable schema-conformance harness, and a clear mapping from current policy/observation versions. The Python suite remains green.

### Phase 2 — Go domain core and replay: 4–6 days

Implement exact amounts, ledger, compiler, order lifecycle, deterministic clock, book/depletion model, features, and settlement. Add scheduled and liquidity-aware baselines.

Exit: Go replays the approved tapes and matches the reference economics/state outcomes; new v1 time behavior is documented. Test generated event sequences, duplicate executions, and cancellation races. Go race tests cover runtime-facing mutable state.

### Phase 3 — Go public-data pipeline: 2–4 days

Port raw decoding, CRC validation, capture manifests, normalization, and book validity. Run both normalizers against the same raw recordings. Do not replace a failed real capture with invented live data.

Exit: checksum/source precision cases match; corruption is detected; live capture is verified when network access permits. The previously observed handshake failure remains an integration issue until demonstrated otherwise.

### Phase 4 — Go-backed Gym bridge: 4–6 days

Implement batch RPC and isolated environment handles, then wrap them in Python Gym/vector interfaces. Verify retry/step identities and training failure propagation.

Exit: PPO can train, save, load, and infer against the Go-backed v1 environment. A worker crash cannot silently continue a different episode or reuse an action. Measure batched transport overhead against the Python reference before defaulting to Go.

### Phase 5 — durable paper execution: 4–6 days

Implement PostgreSQL migrations, account actor, reservation/outbox transactions, command deduplication, recovery, and current-market paper fills. Keep live transport unavailable in this profile.

Exit: restarting at each intent/ack/cancel boundary preserves the account and parent obligation; unknown outcomes remain reserved. Competing owners cannot make new application state authoritative. Test pause/cancel, deadline expiry, stale revisions, and invalid data against queued-but-unsent outbox orders: obsolete intents must not be newly handed to transport. Test a concurrent fill where transport already began as an in-flight reconciliation case.

### Phase 6 — Python inference service: 3–4 days

Add bundle/schema verification, bounded worker scheduling, inference deadlines, response binding, and Go fallback/pause behavior.

Exit: stale, incompatible, delayed, duplicate, and malformed responses cannot produce an unauthorized order. Fast market updates do not cause perpetual rejection of otherwise valid semantic actions.

### Phase 7 — trader API and interface: 5–8 days

Build the plan form, active execution screen, pause/cancel, history, and session replay. Expose actual versus simulated results clearly. Add command authentication for remote deployment.

Exit: design users can supervise a paper session and understand partial completion and pending cancellation without reading model internals.

### Phase 8 — live adapter and controlled recovery: 4–8 days

Integrate account/private streams, submission, cancellation/expiry, fee reconciliation, and venue budgets. Exercise failure boundaries with a simulator before an explicitly authorized bounded live pilot.

Exit: exchange-confirmed fills reconcile; restart/failover procedures and the private snapshot/stream handoff are demonstrated; no ambiguity is resolved by a blind retry. Pre-existing external orders block pilot readiness, unexpected external activity pauses execution, and free/locked/local-reserved balances do not double-count holds. Exercise deadline/pause races with the real adapter's outbound queue as well as the mock transport. Live credentials and pilot authorization are external dependencies.

### Phase 9 — workload and operational validation: 3–5 days, partly parallel

Measure ingress, actor delay, DB commit latency, inference, queue bounds, memory/GC, and replay throughput. Repeat fault cases under load. Include a soak test and database restore drill.

Exit: publish the hardware/workload manifest, measured bottlenecks, and the capacity actually supported. Reduce claimed limits when targets fail rather than weakening correctness.

These phases total roughly 31–50 engineering days for an experienced builder before wider product feedback and integration buffer. Budget approximately **8–12 weeks for the migration and narrow supervised beta**, contingent on venue access and the measured bridge/runtime work. A hosted multi-user service or broad multi-venue product requires another scope and estimate.

## 13. Scale targets and benchmark method

No scale benchmark has been run yet. The following are initial engineering test targets, not supported production claims.

Use a pinned reference host with 8 vCPUs and 16 GiB RAM for Go, PostgreSQL, and CPU inference, with the exact CPU, storage, OS, process limits, and model recorded. Use a mock venue for load and fault injection; do not generate load through real trading endpoints.

Initial workload:

- 50,000 normalized public market events/second across ten instrument books.
- 100 connected mock accounts, each with one active parent and a one-second policy cadence.
- 500 private/account events/second in aggregate, including duplicates and recovery bursts.
- A separately bounded UI subscriber load; start at 100 connected screens.
- Venue limits enforced by the mock adapter; logical policy rate is not a promise of order submissions per second.

Proposed service objectives under that workload:

- p99 receipt-to-applied-public-book delay below 25 ms with bounded memory.
- p99 ready-observation-to-durable-decision-intent below 150 ms, including inference; explicit inference expiry within 200 ms.
- p99 local acknowledgement of a pause/cancel command below 100 ms. Exchange cancellation completion is measured separately.
- No silent account-event loss, lost reservations, duplicate execution accounting, or automatic resolution of unknown orders.
- Go RSS below 4 GiB and bounded queue occupancy during a 24-hour soak on the reference workload; record whole-host resource headroom too.
- Replay at least matches Python reference throughput on identical hardware and semantics. Batched bridge overhead should consume at most 10% of training wall time for the defined reference vector workload.

Measure baseline Python throughput before claiming a Go speedup. Report p50/p95/p99, throughput, allocation rate, GC CPU, max queue age, DB latency, and error counts together. Use profiles to identify bottlenecks. Go's GC has CPU/memory tradeoffs; tune it from observed workload behavior, not an assumption that Go eliminates pauses. [GC guide](https://go.dev/doc/gc-guide) · [Profiling guide](https://go.dev/blog/pprof)

Then test 250 and 1,000 accounts to discover the first limiting component. An unmet target is a measured limit, not a reason to send stale actions or relax account ownership. Tune or add a shard only after identifying whether the constraint is the DB, account actor, model workers, memory, or venue budget.

## 14. Verification and release requirements

Retain the current 40 checks as a reference and expand them with cross-language and runtime checks:

- Exact amount parsing, tick/lot rounding, fee currencies, overflow bounds, and duplicate fill identity.
- Golden full and partial execution reports and independent reward reconciliation.
- Receipt/arrival/cancel ties, persistent liquidity debt, invalid data, and settlement finality.
- Go/Python schema compatibility, explicit enum mapping, feature order and numeric tolerance.
- Replay environment isolation, timed-out step retries, seed determinism, and reset identity.
- Stale inference, policy mismatch, late replies after cancel, and fallback under load.
- Transaction rollback, outbox acceptance ambiguity, account ownership loss, and failover isolation.
- Pause/deadline races with queued outbox orders, account-readiness handoff, external activity, and venue-hold reservation reconciliation.
- Market queue overflow, resnapshot, private-stream recovery, rate limits, and disk exhaustion.
- API authorization, tenant/account isolation, log redaction, and restore/reconciliation.

Use Go fuzzing for parsers, amounts, and event sequences, and the race detector for concurrency checks. These tools find different classes of defect; neither proves exchange correctness. [Go fuzzing](https://go.dev/doc/security/fuzz/)

Run conformance and isolated mock-venue tests in CI. Real-money checks remain an explicitly authorized operational stage. Policies need the existing holdout/promotion process in addition to runtime correctness; an engine port does not validate a trading policy.

## 15. Commit and orchestration plan

Keep each part in a separate reviewable commit, continuing the user's existing preference:

1. Versioned schemas and conformance fixtures.
2. Go module, exact amounts, and domain invariants.
3. Go execution reducer/compiler and accounting parity.
4. Go replay, fills, features, and settlement parity.
5. Go public recorder and normalization parity.
6. Batched replay service and Python Gym adapter.
7. PostgreSQL migrations, account ownership, reservations, and outbox.
8. Python inference worker and Go policy client.
9. Trader API and authenticated command handling.
10. Trader interface and execution journal.
11. Private venue integration and recovery.
12. Load/fault benchmarks, deployment, and runbooks.

Commit boundaries may split further when a part is large; each commit must identify its own verification evidence and remaining limitations. Preserve the current working Python package until the replacement path passes its gates.

Use GPT-6.1-sol for integration decisions, ownership/recovery review, arithmetic semantics, and cross-language parity. Use cheaper subagents for bounded generated-schema wiring, fixtures, straightforward tests, documentation, and UI forms once contracts are frozen. Assign disjoint files and reconcile through the integration lead. Do not delegate independent incompatible designs for the financial core.

## 16. First implementation step after this plan

Start with contracts and conformance, then Go amount/accounting/compiler code. The first Go deliverable should replay the existing buy and partial-sell cases with matching balances and fees, while the existing Python trainer remains runnable.

Do not begin by rewriting every file or by deploying many services. The first milestone establishes shared semantics; the next proves that Go can execute them correctly. The product runtime and scale work build on that result.

## 17. Evidence boundaries

The module mapping is based on the current repository. The architectural choices, timelines, numeric targets, and deployment topology are proposed decisions. Documentation links support specific platform capabilities, not a claim that LiquidityLab already implements them. Current behavior and known limits remain recorded in [verification.md](verification.md) and [simulation.md](simulation.md).
