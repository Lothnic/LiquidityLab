# LiquidityLab: Go runtime and Python RL migration

Decision plan and implementation checkpoint updated 29 October 2026 through clean `883704c`, with retained evidence in `bdd762b`. The Go core, replay bridge, public-data pipeline, PostgreSQL paper runtime, authenticated API, React desk and Python inference integration are committed. Local migration gates 1–6 are closed by requirement/evidence audits. Verified recording archives and an offline baseline comparison CLI are available; durable paper-session/source binding, persisted replay jobs and trader replay UI remain unfinished. Three clean accepted-backlog process trials retain strict partial crash prefixes and exact paused recovery. [Section 16](#16-current-go-delivery-and-scale-plan) remains the implementation sequence. See [verification.md](verification.md) for executed checks; product admission/headroom, shared multi-account operation, policy promotion, private execution, deployment, soak/restore and live authorization remain open.

## Decision in brief

Use Go for the product runtime, financial state, venue connections, and replay engine; retain Python for Gymnasium, training, evaluation, and initially inference. Use React/TypeScript for the trader interface. Start with a modular service, PostgreSQL, and a separately supervised policy worker, then partition execution by account as measured demand grows.

Go is the engineering choice for the product runtime. Calling it the most optimal implementation at scale would require measurements we do not have. The current Go/gRPC training bridge is slower than the Python reference on the checked-in workload. Account capacity remains unmeasured. Local paper recovery has process evidence; external exchange failover remains unimplemented. Optimize each workload against its own evidence.

The next delivery is a complete durable **paper execution assistant**: create a bounded trade, preview baseline execution, start, observe partial fills, pause/cancel, restart safely, and inspect the execution journal. Ship that workflow with a Go baseline before requiring an RL policy. RL remains the same Go engine behind a Python Gymnasium interface, and a proposal source in the product.

## Implementation order at a glance

1. Freeze shared contracts and prove Go accounting/replay conformance against Python.
2. Connect Python Gymnasium to batched Go replay; retain Python training and inference.
3. Deliver a durable paper assistant with PostgreSQL, account ownership, and bounded queues.
4. Add the trader interface, evaluated policy proposals, and private venue recovery.
5. Demonstrate recovery and bounded capacity before a supervised live pilot; increase admission and add account shards only after further measured validation.

The first usable product is a trader-supervised execution assistant. It accepts a trader's intended trade and helps execute within its quantity, price, and time limits. RL is one replaceable proposal source; the product can operate with its Go baseline while policy evaluation continues.

## 1. Decision and scope

Make **Go the authoritative product and execution runtime**, with **Python owning the RL interface, training, evaluation, and initially model inference**. Keep the trader interface in TypeScript/React.

Go is the selected engineering direction for the persistent service, concurrent venue connections, account ownership, recovery, and deployment. Whether it is the best-performing implementation at the intended scale must be established by measurement. Network behavior, database durability, exchange limits, allocation rates, and model inference can dominate language-level differences.

This decision supersedes the earlier plan's Python/FastAPI product backend, SQLite production ledger, and proposed NautilusTrader execution-engine dependency. Existing Python code remains the reference implementation during migration. No private exchange integration or production database exists to migrate today.

The first release continues to support crypto spot, one venue, and one active parent order per connected account. The architecture supports many accounts without permitting concurrent writers to any one account. More instruments and accounts come before multiple venues or several simultaneous plans against one balance.

The product remains an execution assistant: a trader chooses the trade and its size, price boundary, and deadline. A policy proposes execution actions. The Go runtime checks and carries them out.

## 2. Current implementation and what moves

The original starting point was Python v0.1.0 with 40 checks. The repository has since advanced; use the following checkpoint when scheduling remaining work:

- **Committed:** versioned/generated Go and Python protocol bindings; exact amounts; shared ledger/compiler, fees, reservations, observations, passive matching and baselines; deterministic replay/conformance; public recorder and incremental normalizer; bounded replay RPC and Python Gym/vector/PPO bridge; PostgreSQL ownership, inbox, journal and outbox; durable paper actor/scheduler/transport; recorded-data CLI; authenticated API; bounded paper server; Python inference worker and Go policy client; durable runner v2 with optional HTTP polling, controlled inference, and profiling. One real Kraken BTC/USD capture is committed with passing Go/Python normalization conformance; broader pipeline and runtime gates remain open.
- **Paper desk committed:** React supervision interface in `apps/web/` and `docs/trader-ui.md`, exact preview/command binding, retained uncertain retries, API payload validation and responsive layouts. Synthetic browser verification covers create/start/fills/pause/resume/cancel; it is not design-user or live venue evidence. Account authority revisions now distinguish receipt/HOLD bookkeeping from financial/control changes; persistence instrumentation and seven single-trial v2 workload reports are committed; repeated comparisons and load-plus-recovery measurements remain outstanding.
- **Policy/history committed:** `f9125d7` adds revision-pinned store reads, `ec293eb` integrates asynchronous proposals/fallback, `18eeea0` adds scoped reports/provenance, and `6433711` adds bounded history UI. The real Python worker → actor browser run exercised accepted proposals, worker-loss fallback and exact retained reports on synthetic input. This is software integration evidence, not model quality or capacity evidence.
- **Remaining:** historical market counterfactual replay; multi-account supervision/routing; scalable shared feed distribution; immutable evaluation/promotion; private venue integration; CI/deployment and measured capacity. HTTP updates poll; SSE remains optional future work.
- **Measured constraint:** the optimized replay bridge delivers about 23k transitions/second at batch 32 versus about 37–46k for Python in the checked-in synthetic comparison. Keep Python as the default training backend until the bridge gate passes. See [benchmark evidence](../benchmarks/README.md).
- **External gates:** the single committed public capture is a bounded integration fixture, not broad venue validation. Private-account access, an authorized live pilot, and design-user evidence are still outstanding.

The existing code and checks establish implementation progress, not execution quality or account capacity at scale.

The responsibility mapping for the rewrite is:

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

Keep the current Python CLI working through the migration. Complete the Go product CLI/server separately. Remove reference execution code from the default training path only after Go conformance and bridge performance pass. Existing training artifacts are unvalidated and must not be silently promoted to a different engine.

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

The current methods are unary RPCs. Enforce application-level admission, count/byte limits, and deadline checks explicitly; streaming transport flow control does not provide unary work admission. [gRPC flow control](https://grpc.io/docs/guides/flow-control/)

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

The local paper venue now uses the `postgres-paper-v1` insert contract: takeover atomically fences all simulated transport/trade effects and returns their recovery watermark. Process tests exercise an old sender returning after that takeover. The external sender isolation requirements below still apply when implementing private exchange transport. See [paper recovery](paper-recovery.md).

Use durable ownership epochs and leases to reject stale database writes. The sender verifies current ownership before dispatch. However, **a database fencing token does not fence the exchange**: an old process may retain a live connection or already have a request in flight.

Initial failover is controlled: pause new sends, isolate/terminate the previous sender, reconcile its known and unknown intents, then enable the new owner. An expired lease alone is insufficient to authorize a second sender. If old-owner isolation cannot be established, stay paused and require operator recovery. Name the operator/supervisor responsible, record process/connection isolation evidence durably, and link it to the ownership-transfer decision; trusted recovery booleans alone do not demonstrate isolation. Exercise a suspended old owner that resumes after attempted transfer. Measure the maximum interval from a stop request to the last possible outbound handoff, including synchronous transport delays. Automated high availability requires a separately verified outbound-fencing design and venue capability analysis.

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
  internal/execution/features.go # Shared versioned observation construction
  internal/replay/              # Simulation clock, fills, settlement
  internal/replayrpc/           # Bounded replay service and wire mapping
  internal/marketdata/           # Books, recording, fanout and validity
  internal/venue/kraken/         # Protocol parsing and authenticated transport
  internal/runtime/             # Account actors, ownership and recovery
  internal/store/postgres/       # Transactional state and outbox
  internal/inference/            # Python policy client and eligibility
  internal/api/                  # HTTP/operator commands and updates
  proto/liquiditylab/v1/         # Language-independent contracts
  gen/go/                       # Generated Go protocol types
  src/liquiditylab/              # Existing Python package, retained
    v1/                         # Generated Python protocol types
    go_backend.py               # Gym/vector environment adapter
    go_vector.py                # Batched SB3 adapter
    inference/                  # Python model worker
  apps/web/                     # Trader interface
  migrations/                   # Ordered PostgreSQL migrations
  conformance/                  # Versioned common input/output vectors
  benchmarks/                   # Workload manifests and measured results
  deploy/                       # Local compose and process configuration
```

Keep the Go release, Protobuf compiler/plugins, database driver, and RPC packages pinned. Generated code must be reproducible and checked for schema drift. Credentials and market recordings stay outside version control. Most runtime directories above now exist. `apps/web/` is committed; production deployment packaging and any new shared ingest/account-routing modules remain planned. Inspect Git history before scheduling a component as new work.

## 12. Migration phases and gates

The estimates below describe the original full effort, not a new estimate of work remaining. Phases 1–4 and the persistence part of phase 5 already have substantial implementation. Complete the remaining exit criteria without rewriting those components again. Development load measurements begin alongside paper execution; the final production-capacity claim follows the complete adapter and workload validation.

### Phase 1 — specification and fixtures: 2–3 focused days

Completed local checkpoint: schemas, generated bindings, exact wire mapping, shared boundary/economic fixtures, explicit reference precision and documented v0/v1 timer differences are committed. The [Phase 1 requirement audit](phase1-contract-audit.md) maps each requirement to implementation and the retained five-step conformance run: generated bindings and fixtures unchanged, four Go race packages passed, and 198 Python checks passed without skips. Later economic, performance and release gates remain separate.

Freeze v1 amount/time/action contracts, extract common golden cases from the existing tests, and add malformed/overflow/rounding/cancellation fixtures. Record all intentional differences from the Python implementation.

Exit: generated Go/Python messages, an executable schema-conformance harness, and a clear mapping from current policy/observation versions. The Python suite remains green.

### Phase 2 — Go domain core and replay: 4–6 days

Completed local checkpoint: the core, baselines and replay match five approved Python economic/observation tapes. Generated rational ledger/compiler/replay scenarios, short bounded fuzz campaigns, and a shared replacement-finality case close the remaining semantic evidence. The [Phase 2 audit](phase2-core-audit.md) records the requirement map, clean combined conformance run and configured full Go race pass. Go training-default, performance, policy and release gates remain separate.

Implement exact amounts, ledger, compiler, order lifecycle, deterministic clock, book/depletion model, features, and settlement. Add scheduled and liquidity-aware baselines.

Exit: Go replays the approved tapes and matches the reference economics/state outcomes; new v1 time behavior is documented. Test generated event sequences, duplicate executions, and cancellation races. Go race tests cover runtime-facing mutable state.

### Phase 3 — Go public-data pipeline: 2–4 days

Completed local checkpoint: parser, recorder and normalizer preserve source precision, validate shared checksum cases and pass deterministic corruption/reconnect/depth-deletion checks. Commit `8c7e329` retains a verified real 50-frame Kraken BTC/USD capture with 45-event Go/Python conformance. The [Phase 3 audit](phase3-public-data-audit.md) and clean `75d71fd` combined run close the literal local exit criteria. Recording-pressure monitoring, sustained load and wider operational gates remain open.

Port raw decoding, CRC validation, capture manifests, normalization, and book validity. Run both normalizers against the same raw recordings. Do not replace a failed real capture with invented live data.

Exit: checksum/source precision cases match; corruption is detected; live capture is verified when network access permits. An earlier failed handshake attempt is retained as historical evidence; the later verified 28 October 2026 public capture demonstrates successful network capture.

### Phase 4 — Go-backed Gym bridge: 4–6 days

Completed local checkpoint: the bounded service, Gym/vector adapter, retry/worker-loss behavior and PPO train/save/load/infer roundtrip pass the clean `75d71fd` combined run. The [Phase 4 audit](phase4-bridge-audit.md) maps each requirement. The measured synthetic bridge is slower, so Python remains the default and the separate default-cutover gate stays open.

Implement batch RPC and isolated environment handles, then wrap them in Python Gym/vector interfaces. Verify retry/step identities and training failure propagation.

Exit: PPO can train, save, load, and infer against the Go-backed v1 environment. A worker crash cannot silently continue a different episode or reuse an action. Measure batched transport overhead against the Python reference before defaulting to Go.

### Phase 5 — durable paper execution: 4–6 days

Completed local checkpoint: the [Phase 5 audit](phase5-paper-audit.md) maps all literal durable-paper requirements to passing implementation/tests. Clean `883704c` full Go race evidence and three ordinary accepted-backlog recovery trials retain exact finances, strict partial crash prefixes and held obligations before paused recovery. The controlled 128-fill batch is harness-authored simulated transport data. Private exchange failover, supported admission/headroom and operational restore remain separate open gates.

Implement PostgreSQL migrations, account actor, reservation/outbox transactions, command deduplication, recovery, and current-market paper fills. Keep live transport unavailable in this profile.

Exit: restarting at each intent/ack/cancel boundary preserves the account and parent obligation; unknown outcomes remain reserved. Competing owners cannot make new application state authoritative. Test pause/cancel, deadline expiry, stale revisions, and invalid data against queued-but-unsent outbox orders: obsolete intents must not be newly handed to transport. Test a concurrent fill where transport already began as an in-flight reconciliation case.

### Phase 6 — Python inference service: 3–4 days

Completed local checkpoint: the [Phase 6 audit](phase6-inference-audit.md) maps bundle/request verification, bounded scheduling, fallback, stale/malformed/late/duplicate rejection and acceptance across newer book/source updates to passing clean `883704c` Go/Python evidence. A repeated accepted handler result has one financial effect, with separately durable late audits; 100 logical book updates do not invalidate unchanged authority. Immutable evaluation/promotion and operational capacity remain outstanding.

Add bundle/schema verification, bounded worker scheduling, inference deadlines, response binding, and Go fallback/pause behavior.

Exit: stale, incompatible, delayed, duplicate, and malformed responses cannot produce an unauthorized order. Fast market updates do not cause perpetual rejection of otherwise valid semantic actions.

### Phase 7 — trader API and interface: 5–8 days

Current checkpoint: scoped authenticated API, exact preview, curated execution projections, React interface and revision-pinned reports/history are committed with synthetic browser and component evidence. Verified-archive offline baseline comparison is implemented; paper-session/source binding, persisted replay jobs, trader replay UI and design-user validation remain outstanding. Revision inspection does not satisfy counterfactual replay.

Build the plan form, active execution screen, pause/cancel, history, and session replay. Expose actual versus simulated results clearly. Add command authentication for remote deployment.

Exit: design users can supervise a paper session and understand partial completion and pending cancellation without reading model internals.

### Phase 8 — live adapter and controlled recovery: 4–8 days

Integrate account/private streams, submission, cancellation/expiry, fee reconciliation, and venue budgets. Exercise failure boundaries with a simulator before an explicitly authorized bounded live pilot.

Exit: exchange-confirmed fills reconcile; restart/failover procedures and the private snapshot/stream handoff are demonstrated; no ambiguity is resolved by a blind retry. Pre-existing external orders block pilot readiness, unexpected external activity pauses execution, and free/locked/local-reserved balances do not double-count holds. Exercise deadline/pause races with the real adapter's outbound queue as well as the mock transport. Live credentials and pilot authorization are external dependencies.

### Phase 9 — workload and operational validation: 3–5 days, partly parallel

Measure ingress, actor delay, DB commit latency, inference, queue bounds, memory/GC, and replay throughput. Repeat fault cases under load. Include a soak test and database restore drill.

Exit: publish the hardware/workload manifest, measured bottlenecks, and the capacity actually supported. Reduce claimed limits when targets fail rather than weakening correctness.

The original full-scope estimate was roughly 31–50 engineering days for an experienced builder, or **8–12 weeks for the migration and narrow supervised beta** with integration buffer. It is not a new remaining-work estimate: substantial components now exist, while runtime integration, usability and external venue gates still carry uncertainty. Re-estimate the remaining delivery after the paper actor and policy-client reviews. A hosted multi-user service or broad multi-venue product requires another scope and estimate.

## 13. Scale targets and benchmark method

Replay benchmarks and allocation profiles have been recorded; no product account-capacity benchmark or full operational soak has passed. The following are initial engineering test targets, not supported production claims. Replay transitions/second, normalized feed events/second, actor commands/second, venue submissions/second, and active accounts are separate quantities.

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

Use a provisional 100 ms inference allocation inside the 150 ms decision target, leaving 50 ms for feature preparation, validation and persistence. The 200 ms validity budget is a hard stale-response cutoff, not a second latency target or permission to queue work for 200 ms. Measure the end-to-end percentile directly; summing individual component p99 values does not establish an end-to-end p99.

Then test 250 and 1,000 accounts to discover the first limiting component. An unmet target is a measured limit, not a reason to send stale actions or relax account ownership. Tune or add a shard only after identifying whether the constraint is the DB, account actor, model workers, memory, or venue budget.

### Staged deployment and the reason to split each component

**Stage A — supervised beta.** Deploy one Go service, PostgreSQL, and a separate Python inference process. Keep public books shared by instrument, account mutation serialized, and training/replay on separate resources. Set a supported account limit from the measured workload. Admission refuses new sessions when the service cannot preserve its latency, recovery, or venue-budget headroom.

**Stage B — partition account execution.** Add executor processes when account work breaches the latency objective or sustained CPU/queue pressure leaves insufficient failover headroom after profiling and tuning. Persist an explicit account-to-owner assignment and ownership epoch. A routing hash may suggest assignments; changing worker count must not silently change ownership. Drain an account, settle or classify outstanding sends, isolate the old sender, reconcile, and then activate the new owner. A single hot account remains serialized and may need a lower supported workload.

**Stage C — isolate shared ingest and inference.** Extract ingest if parsing, fanout, recording, or GC materially interferes with account latency. Fan out versioned snapshots with receipt-session identity, snapshot revision, and book validity. Receivers detect loss/restart and stop price-sensitive actions until resynchronized. Replicate Python inference when measured queue age consumes its deadline budget; route by bundle compatibility and reject excess work. Preserve end-to-end expiry after retries and load balancing.

**Stage D — scale storage and history.** Separate large public recordings and historical analytics from transactional account state. Use manifest-addressed object storage for immutable recording chunks. Add database capacity or partitioning only when measured commit latency, lock waits, storage I/O, or recovery time requires it. If account storage is eventually sharded, colocate each account's commands, fills, reservations, intents, and outbox in one transactional shard. Design identity/uniqueness scope explicitly before migration. Read replicas may serve delayed history; execution decisions use authoritative state.

**Stage E — independent API capacity.** Scale authenticated HTTP/UI delivery separately if subscriber or read traffic consumes execution resources. Commands remain revision-checked durable inputs routed to the account owner. Bound subscriptions and coalesce UI snapshots. A history query or slow browser must not block private-event processing.

Stage order after B follows the observed bottleneck. Adding a broker, orchestrator, or service boundary is a separate decision with an owner, failure model, and measured benefit. These are LiquidityLab design choices, not language guarantees.

### Capacity model and database write budget

Use measured service demand to choose admission, rather than deriving capacity from goroutine counts. For A active accounts, cadence D seconds, E durable private/paper facts per account per second, and K durable transitions per decision, estimate decision demand as A/D and DB transition demand as A × (E + K/D), then include command, lease, recovery and retry overhead. Measure E and K from traces; paper passive matching can make E track trade rate even when no fill occurs.

Publish serialized state bytes/transition, journal growth, WAL bytes/second, commit latency and lock waits alongside throughput. Current snapshots include book/state and accumulated fills; bound their growth and measure the cost of rewriting them. Before increasing admission, consider incremental fill/history tables and less frequent bounded checkpoints while retaining atomic balances, reservations, deduplication and outbox transitions. Verify replay from each checkpoint and migration compatibility. Batch ordered source receipts only if every fact remains recoverable and a crash cannot apply a fill twice. Do not replace durable financial facts with coalesced UI state.

A model worker's single-flight client is a concurrency bound, not a 100-decisions/second guarantee. If measured safe worker service time is S seconds, provision from utilization × worker_count / S, with margin below saturation and a measured burst budget. Stagger periodic decisions; a synchronized burst of 100 accounts can overload a worker even if its average rate appears sufficient. Keep latency targets unchanged when scaling worker count.

Acceptance: compare 100 accounts on shared instruments with staggered and synchronized decisions, no-fill trade bursts, one hot account, slow DB, inference loss and private-event recovery bursts. Report total DB connection caps across processes, fairness, oldest event age and rejected admission. Require recovery and control headroom under the advertised limit. Exact numeric capacity remains unknown until those runs pass.

### Resource budgets and operational measurements

Publish a capacity manifest for each release: maximum active accounts, instruments, environments, connections, queue counts/bytes/age, DB connections, inference concurrency, tape memory, disk retention, and in-flight venue requests. Initial values are configuration subject to benchmark acceptance. Limit both item counts and bytes: one oversized event/report must not bypass a queue's memory budget.

Admission checks must reserve headroom for recovery bursts and cancellations. Put a deadline and cancellation path on external work and database calls; do not hold an account database transaction open during Python inference or venue network I/O. Read an observation snapshot, perform inference outside the transaction, then revalidate revisions within the durable transition. Bound concurrent DB access across actors instead of allocating one permanent connection per account.

Apply per-account command, mailbox, subscription, and inference quotas as well as process totals. One active parent per account remains the initial limit. Schedule bounded work fairly across runnable actors, with dedicated capacity for control and recovery. A hot account cannot monopolize the worker pool. Private-event overflow invalidates that account's readiness and starts reconciliation; admission pressure rejects new starts before existing accounts lose their operating headroom.

Avoid multiplying feed volume by account count. Apply each public delta once per instrument book. Account decisions consume versioned snapshots at their configured cadence. Deliver ordered trade/order-relevant events only to affected paper actors; those events cannot be replaced by occasional snapshots when simulating passive fills. At 100 accounts and one decision/second, the initial model workload is roughly 100 decisions/second, independently of the 50,000-event/second ingest target. A single Python worker must demonstrate that demand or admission must be lower.

Measure service demand per account and instrument before choosing shard sizes: CPU milliseconds per second, resident bytes, DB transactions/second, journal bytes/day, and inference time/decision. Record fixed ingest cost separately. Budget aggregate database connections across every process; the existing store's bounded pool is per store instance and does not establish a deployment-wide cap. Add a shared concurrency budget or proxy when multiplying instances. Preserve measured headroom for a recovery burst and the loss/draining of a worker before admitting more sessions.

Separate order-create, cancel, account-recovery, and REST-history budgets according to venue rules. Preserve capacity for cancellation and reconciliation during submission pressure. Backoff with jitter on throttling and apply account/global limits at their documented scope. More Go workers or API keys do not imply more venue trading capacity. Kraken documents separate REST counters and trading-engine limits across protocols. [Kraken rate limits](https://docs.kraken.com/exchange/guides/rest/ratelimits)

Instrument public-book lag, oldest private event, actor mailbox age, durable command age, outbox unknown count/age, ownership changes, reservation discrepancies, inference expiry, venue throttle counts, and recording write lag. Keep account/tenant IDs in authorized logs/traces rather than unbounded metric labels. Record p99 latency together with rejection, pause, truncation, and disconnect rates so load shedding cannot conceal overload.

Set retention and deletion rules for raw recordings, execution audit history, reports, and backups before hosted deployment. Recordings and bundles carry checksums and schema/engine revisions. Disk-pressure handling must preserve durable account writes and mark incomplete captures. Define and measure backup recovery point and recovery time objectives before making an availability commitment.

Provisional paper-beta retention: keep seven days of hot raw public capture and thirty days total; immutable datasets referenced by bundles or evaluation evidence are pinned and exempt from expiry. Keep thirty days of diagnostic logs with sensitive fields redacted. Preserve account journals, fills, reservations, commands, and policy provenance for the entire beta; their eventual retention and deletion policy is a separate hosted-release decision. Prune complete chunks only after verifying manifests and active references. Estimate bytes/day from measured wire volume and compression before selecting storage capacity.

Provisional disaster-recovery objectives are at most five minutes of database recovery-point loss and sixty minutes to restore a paused service on the reference deployment. Use base backups plus continuous WAL archiving, monitor archive lag, and verify access to encrypted backups and recovery keys. PostgreSQL documents the base-backup/WAL recovery mechanism and its archive-continuity requirements. [PostgreSQL continuous archiving](https://www.postgresql.org/docs/current/continuous-archiving.html) Drill restoration into an isolated database, verify journal/projection consistency, then reconcile against venue history before enabling execution. Track time to reconciled readiness separately; the sixty-minute target does not promise trading resumes while exchange outcomes remain unknown. These are proposed acceptance targets until a restore drill demonstrates them.

### Capacity promotion and rollback criteria

Use a mock venue and paper accounts for cohorts of 1, 10, 25, and 100 active accounts. Hold each for at least thirty minutes with the defined ingress, private-event bursts, inference delays, and UI traffic; hold the final supported workload for the full 24-hour soak. Capture the manifest and percentile/error/resource measurements for every cohort.

Promote only when the latency/resource objectives above pass, queues drain after the injected burst, no unexplained reservation/accounting differences remain, and fault-induced unknown intents resolve within the scenario's specified recovery budget. Each scenario declares that budget before running. A growing unknown/recovery backlog, persistent queue growth, or any accounting/ownership violation stops the ramp. Return admission to the last demonstrated capacity and investigate; do not force ownership transfer or discard queued accounting facts to reduce the displayed load. Production admission limits come from the last passed workload, with documented recovery headroom.

## 14. Verification and release requirements

Retain the original Python reference checks and the expanded Go/Python/UI coverage recorded in [verification.md](verification.md). Complete the following cross-language and runtime requirements:

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

## 16. Current Go delivery and scale plan

### Architecture decision

Complete the Go product runtime incrementally. Retain Python for Gymnasium, PPO training, evaluation and initially model inference. Retain React/TypeScript for the browser. The existing Go monetary and execution packages remain authoritative; adding a second execution engine would increase reconciliation work.

Initial deployable unit: one modular Go process, one shared PostgreSQL pool, separately supervised Python policy workers when enabled, and static browser assets. Training runs in a separate resource pool. A Go baseline must operate without Python. Keep one active parent per account and one writer per account. Start with Kraken Spot and BTC/USD; verify venue/account availability before private integration.

The existing architecture diagram shows the target responsibilities, not completed multi-account deployment. The current paper daemon is loopback-only and manages one created or explicitly resumed synthetic account. Product hosting requires a supervisor, account registry and routing in addition to the existing actor.

### Runtime interfaces and responsibilities

Keep these interfaces narrow; implementation details remain inside their modules:

- **Execution core:** explicit state plus event produces a validated transition and effects. Exact balances, reservations, fees, action compilation and features stay in Go. No network, database or implicit wall-clock reads inside deterministic calculations.
- **Account supervisor:** admits accounts, loads compatible state, acquires ownership, routes commands/facts, supervises actors and drains them. Tenant/account identity comes from authenticated scope and durable assignments. It does not create money or infer successful venue outcomes.
- **Market hub:** one ordered reducer per venue/instrument, immutable versioned book views, source validity and recorder health. Use separate delivery contracts for replaceable views and ordered paper trades. Keep account matching debt/order state private to its actor.
- **Transition store:** atomically persists authority revision, command identity, financial effects, source cursor and outbox. A shared process pool and deployment-wide connection budget bound access. History uses bounded projections and query budgets.
- **Policy gateway:** accepts a frozen observation and bundle identity; returns a semantic proposal or an explicit unavailable/expired result. It owns bounded worker admission and compatibility routing. The actor revalidates and compiles the proposal before any order intent.
- **Venue adapter:** translates authorized intents, emits stable private observations and supplies reconciliation queries. Credential access and outbound trading capability remain exclusively in the Go runtime. Transport acceptance, fills and cancel finality are distinct facts.

### Stage 0 — land the existing integration work

**Completed checkpoint:** component commits `f9125d7`, `ec293eb`, `18eeea0` and `6433711`, with the executed checks and synthetic browser outcome in [verification.md](verification.md). The sequence below records the delivered scope. Local paper Stage 1 delivery is recorded below; later scale and release gates remain open.

**Scope:** review the current dirty files, correct documentation against behavior, and commit by part. Preserve unrelated work. Do not recreate modules that already exist.

Planned commit sequence:

1. `feat(store): add revision-pinned session history` — session queries, migration 0003 and store checks.
2. `feat(runtime): resolve asynchronous policy slots durably` — actor integration, bounded deadlines, inference compatibility, daemon options and runtime checks.
3. `feat(api): expose scoped execution reports and policy status` — history routes, curated projections and daemon integration checks.
4. `feat(web): add execution history and policy status` — bounded history views, stale-response guards, exports and browser integration.

Land dependencies in this order or split further to keep each commit coherent. No staging of the whole working tree. Each commit identifies its own evidence and limitations.

**Exit gate:** a baseline and an opt-in Python policy can each be supervised through preview, start, fills or HOLD, pause and cancel. A stalled/late worker cannot delay control or act after fallback. Reports preserve exact fills/fees and pin their revision. Unvalidated bundles remain paper/shadow only. Executed acceptance evidence is in the verification record; these checks do not establish model quality or supported capacity.

### Stage 1 — make an account operationally restartable

**Local paper checkpoint:** `50f7294` reconstructs fresh state and pauses recovered parents; `e0d68ba` adds the owned database effect contract; `59e9ff7` wires simulated transport/recovery; `d710fce` adds create/resume; `c27bbec` and `2d54659` exercise real process death, precise durable checkpoints and a still-running old sender. This delivers the local paper scope. It provides no physical exchange connection fencing or hosted availability claim. See [paper-recovery.md](paper-recovery.md) and [verification.md](verification.md).

**Scope:** introduce explicit `create` and `resume` startup paths. Resume opens the same tenant/account/schema without changing initial balances, silently creating a replacement account, replaying old trader commands or clearing holds. Expose startup states: recovering, paused, ready and recovery-required. A supervisor launches actors only after compatible snapshot/config and ownership checks.

Add an OS process-kill harness with a persistent mock venue. Cover intent commit, attempted handoff, venue acceptance before local acknowledgement, partial fill, cancel/fill race, source-inbox reduction and pending inference. Restart the actual binary against the same database. Include a suspended old sender that resumes after a proposed takeover.

**Commits:** existing-account lifecycle and compatibility checks; process harness and crash scenarios; controlled ownership-transfer runbook and isolation evidence.

**Exit gate:** balances/fill identities/reservations agree after recovery; an attempted unknown send is never blindly retried; unknown holds survive; the service stays paused if reconciliation or sender isolation is incomplete. Measure recovery duration, replay debt and the interval from stop request to last possible transport handoff. Database epochs alone cannot prove exchange fencing.

### Stage 2 — establish the performance baseline

Instrument before optimizing the durable path:

- receipt-to-book, private-event age, actor wait/service time and local control acknowledgement;
- ready-observation-to-durable-intent including inference, expiry/fallback rates and rejected work;
- transactions per source fact/decision/fill, pool wait, commit latency and lock wait;
- serialized checkpoint bytes, journal growth, WAL bytes/second and restore/replay time;
- CPU, allocation rate, GC CPU, RSS, queue count/bytes/age and recording write lag.

Record hardware, software revisions, workload seed, instrument distribution, decision cadence, policy/bundle, payload sizes, storage and process limits in an immutable benchmark manifest. Low-cardinality metrics plus scoped logs/traces provide evidence; tenant/account IDs must not become unbounded metric labels.

**Commits:** runtime/store instrumentation; persistent mock venue and workload runner; benchmark manifests and baseline report.

**Current checkpoint (28 October 2026):** bounded telemetry, store and actor/inference instrumentation, the durable runner, and schema-v2 support for optional HTTP polling, controlled inference, and profiling are committed. The archived `durable-runtime-20261028` bundle contains 11 schema-v1 reports from three metrics-off/on baseline trials and five ramps; the high-rate invalid source-delivery trials remain included and are not capacity evidence. The [v2 envelope bundle](../benchmarks/results/runtime-envelope-20261028/README.md) adds seven reports: six complete and one invalid allocation-profile trial with 375 rejected source offers. HTTP polling and real/controlled inference are now measured in those bounded trials; repeated cohorts, server-side attribution, recovery headroom and supported admission remain unproven.

**Exit gate:** reproducible one-account measurements distinguish ingest, actor, persistence, inference and UI costs. Report both successful work and overload outcomes. Identify the first limiting resource before changing storage or extracting deployments. The existing v1 results are descriptive and do not close this gate; review the v2 workload limits and extend recovery/load and bottleneck evidence before passing the gate. HTTP polling does not measure React rendering; the short pause-fallback trial timed out during warmup and performed no measured inference calls.

Follow-up [audited workloads](../benchmarks/results/runtime-audited-20261028/README.md)
use explicit response/terminal-validation contracts from a clean `fc525b1`
build. Two HTTP trials completed; the separate blocking-profile trial lost 290
source offers and remains invalid. The serial source delivery path accumulates
backlog before exhausting the configured pool, but current CPU/Go blocking
profiles do not identify pure database/storage wait. Measure recovery under
demand and server-side service cost before assigning an admission limit.

The [29 October scoped PostgreSQL cohort](../benchmarks/results/runtime-pg-activity-20261029/README.md)
adds three ordinary observer-off/on pairs and a separate 250 Hz ramp, from clean
`35fc7b7`. All six ordinary runs complete 1,000 source offers and 400 HTTP reads.
The ramp rejects 25 of 2,500 source offers and remains invalid. It records 7,902
commits, 5.769 s aggregate client COMMIT round trips and 248 WAL-IO classifications
among 513 backend snapshots. These observations locate substantial cost on the
durable commit path; sampled snapshots do not isolate pure fsync/server CPU,
and periodic sampling can alias with source cadence. No admission/headroom gate
is passed by these results. Next, bound the current actor's fact-reduction work
per turn and verify controls and recovery against an accepted backlog.

#### Stage 2 commit interfaces

The following five-part commit sequence records how Stage 2 was implemented. All five components are now committed, but implementation alone does not satisfy the measurement gate.

1. **`feat(telemetry): add bounded runtime measurements`** — `internal/telemetry/`. Define fixed metric identities, units and bounded distributions with optional recording. Freeze timestamp boundaries, outcome denominators and percentile resolution before integration. Use process-local monotonic elapsed time for durations and UTC for artifact correlation; never subtract monotonic timestamps from different processes. Include bounds at the proposed 25/100/150/200 ms objectives so bucket estimates can evaluate those targets. Keep raw SQL, credentials, account IDs and error strings out of metric labels. Report histogram quantiles as bucket upper bounds; empty or overflow estimates are unavailable. Saturating counts disclose overflow. Recording performs no file or network I/O on the actor path.
2. **`feat(store): measure pool and durable transition cost`** — `internal/store/postgres/`. Preserve `Open` compatibility while allowing an explicit shared pool cap and optional recorder. Measure acquisition, transaction lifetime, commit/rollback and query outcomes. Call the `SELECT ... FOR UPDATE` measurement account-lock roundtrip: it includes query and decode work and cannot identify pure lock wait. Report attempted serialized bytes separately from writes whose commit was acknowledged. A failed commit can have an unknown outcome; it is not evidence of a successful rollback. Deferred cleanup must release a connection exactly once.
3. **`feat(runtime): measure actor and policy latency`** — `internal/runtime/`, `internal/inference/`, daemon wiring. Record offered, admitted, completed, failed and unresolved work separately, plus queue age, handler duration, local control acknowledgement, decision-to-durable-intent and inference/fallback outcomes. Start each interval at its documented event and preserve its origin through asynchronous work. Count a pending policy slot once when it resolves. Measure local command acknowledgement separately from venue cancellation finality. Controls, private facts and recovery retain priority when optional inference is saturated.
4. **`feat(bench): add durable paper workload runner`** — `benchmarks/runtime/`. Run the real actor and PostgreSQL store with a persistent simulated venue. Require an explicit development database/schema and fresh report paths; never reset an existing account to repeat a run. Support baseline-only, controlled inference, delayed inference, history/UI polling and recovery scenarios independently. Use the same immutable input and actions when comparing revisions.
5. **`docs(bench): record the durable runtime baseline`** — workload manifests, commands and bounded reports. Record successful and rejected demand, exact final economics, CPU/allocation/GC/RSS, pool pressure, transaction and byte rates, recovery debt and end-to-end latencies. Include at least three repeat runs after warmup and an observer-enabled/disabled comparison before using the recorder to justify an optimization. List incomplete measurements explicitly.

The workload runner must schedule offered load independently of response time. Retain intended dispatch, actual admission and completion times; report generator lag and rejected work. A generator that waits for each response can hide overload by reducing the offered rate. A separate closed-loop run can measure maximum service throughput, but cannot establish latency under the declared arrival rate.

Each report contains a version, Git revision and dirty-state marker, engine/schema versions, seed, hardware/OS/runtime versions, database/storage configuration, source event mix and bytes, account/instrument counts, cadence, bundle identity, pool/queue caps, warmup and measured interval. Store manifests and hashes with the results. Cluster WAL deltas include other database activity; use an isolated cluster for attribution or label them cluster-wide. Capture CPU, allocation and blocking profiles in separate comparable runs when their collection interferes with timing. [Go diagnostics](https://go.dev/doc/diagnostics)

Before starting Stage 3, the report must identify measured service demand per account/decision and per shared instrument, the first saturated resource, and an initial admission limit with recovery headroom. A failed target is still useful evidence; it does not pass the capacity gate.

### Stage 3 — shared feeds and bounded multi-account execution

Build an account registry/supervisor around the existing actors. Persist explicit account-to-owner assignments. A hash can suggest an assignment; adding a process cannot silently move financial ownership. Start with many actors in one process and one shared store pool. Bound active accounts, runnable work, total database concurrency and inference work; reserve capacity for controls and reconciliation. Stagger decision cadence and isolate a hot account.

Build one market hub per venue/instrument:

- Apply each public delta once. Publish immutable book views with source session, sequence/version, metadata revision and validity. Coalesce a view only where intermediate states are not financially relevant.
- Preserve ordered trades and any order-relevant market facts for paper matching. Bind the first-observed fact to the appropriate child/order state; delayed delivery must not match a historical trade against a newly created order.
- Bound consumer backlog by count, bytes and age. A missed required event invalidates the affected consumer until replay/resnapshot succeeds; a slow consumer cannot stall every account.
- Retain a durable source segment until all required cursors/checkpoints and recovery retention permit deletion. Specify atomicity between recording availability and committing a cursor that references it. No account may reference an unavailable source fact after a crash.

Measure shared-book work separately from paper matching. Book application can approach O(public events); passive paper matching still requires work for each affected account/trade. Under the current per-account durable receipt design, 5,000 relevant trades/second delivered to 100 accounts can demand up to 500,000 receipt writes/second before projection work. This is an illustrative demand calculation, not a measured workload or supported limit.

**Commits:** bounded account supervisor/routing and shared pool; shared versioned books; ordered paper-trade delivery and recovery cursors; multi-account fairness/admission scenarios.

**Exit gate:** 1, 10, 25 and then 100 mock accounts share books without changing per-account economics. One account, slow browser or failed worker cannot monopolize control/recovery work. Private facts remain tenant-isolated. Publish both paper-trade fanout cost and live-style private-event cost; one workload cannot stand in for the other.

#### Multi-account delivery contracts

Implement Stage 3 as four dependent parts:

1. **Supervisor and command routing.** Add a durable account registry and explicit assignment/lifecycle states around `internal/runtime.Actor`. Reuse one process store pool. Replace the API's construction-time authority map with a bounded resolver while retaining tenant/account authorization before access. Preserve command identity across ownership changes. Distinguish unavailable ownership from unknown command outcome; retrying a command never invents a new identity. Shutdown stops new admission, drains accepted work and releases only authority it owns. Define canonical venue-account identity separately from logical tenant/account IDs and API-key IDs. Before private activation, verify that mapping and enforce one financial owner/reservation domain per managed venue account; credential rotation retains the mapping. Ambiguous credential aliases cannot establish independent readiness.
2. **Shared public books.** Extract source/book ownership from the single-account daemon. Key each hub by venue/instrument and metadata revision; publish immutable source-session/version/validity views. Bound reference lifetimes so a slow reader cannot retain unlimited old books. An invalid shared book blocks new price-sensitive actions for its consumers. Public book versions never change another account's fills, reservations or matching debt.
3. **Ordered paper facts.** Define durable source availability, subscription start position, first-observation binding and account cursor advancement before implementing fanout. The shared fact must be recoverable before an account commits a reference to it. On restart, recover the same ordered range; deduplicate economic application. Decide explicitly how paper liquidity is simulated across accounts: retain independent per-account simulations initially and label that assumption, rather than accidentally claiming a shared real venue queue.
4. **Fair admission and fault evidence.** Configure process and per-account count/byte/age budgets, inference capacity, database concurrency and reserved control/recovery capacity. Bound fact reduction per scheduling turn by count and measured time; the current fixed-watermark drain can span many transactions, so mailbox priority alone does not guarantee prompt controls. Yield only between atomic transitions, preserve source order and block new creates while required facts remain unapplied. Distinguish receipt of a stop request from its durable effect and completed cancellation/reconciliation. Stagger normal decisions and also measure synchronized bursts. Exercise a sustained fact backlog, hot account, blocked history read, slow database, stopped worker, source gap, full consumer queue and actor restart. Increase the cohort only after the preceding one passes.

Overflow behavior is part of each interface. Replaceable UI/book views may coalesce only after preserving required order-relevant facts. An unaccepted command gets an explicit rejection. An accepted command remains discoverable by identity. Required private/paper facts remain durable or trigger an explicit readiness failure and recoverable source gap; silently dropping them is never load shedding. Record every rejection, invalidation and paused account alongside latency.

### Stage 4 — optimize measured storage and inference constraints

**Persistence:** the current actor serializes complete state, journals it and reloads an account; snapshots include books and accumulated fills. First measure this cost. If it dominates, introduce append-only fill/decision rows, compact active-state projections and bounded checkpoints with ordered deltas between them. Keep balances, reservations, fill deduplication, outbox, command result and consumed cursor atomic. History must not repeatedly scan growing full snapshots for every report.

A shared durable market log plus per-account cursors may remove duplicated paper payload storage, but only after Stage 3 defines ordering, retention, first-observation binding and crash recovery. Batching may reduce transactions while preserving every required fact. UI coalescing rules cannot be used to discard accounting facts. Set maximum checkpoint size and recovery debt in the capacity manifest and prove reconstruction from each retained checkpoint.

**Inference:** implement a bounded pool of compatible workers/clients, preserving one pending slot per plan. A single-flight client is a concurrency limit; its throughput depends on service time. Size admission using measured service time, utilization margin and bursts. Route by bundle/schema/profile, reject work that cannot finish inside its budget, and resolve each slot once. Keep cancel/recovery independent of the pool. Replicate workers before considering ONNX/native inference; require action parity for any new inference implementation.

**Commits:** one independently measured optimization per commit; forward/backward-compatible migrations; reconstruction evidence; inference pool and saturation scenarios.

**Exit gate:** repeat the same workload and crash cases. Publish before/after CPU, WAL, bytes/account, p99 control/decision latency and recovery duration. Reject an optimization that improves throughput by weakening source ordering, exact arithmetic, recovery or auditability.

### Stage 5 — complete the useful trading workflow and policy eligibility

Complete historical market replay separately from the new revision-pinned reports. Store immutable source manifests, engine/rules/fee/simulator versions and policy hashes. Let the trader inspect actual or paper fills alongside a clearly labelled counterfactual baseline/model replay. Freeze terminal benchmark references. Never present simulated savings as realized profit or evidence that a different real order would have filled.

Build immutable evaluation runs using chronological training/validation/holdout periods, multiple seeds, baseline comparisons, realistic fees/latency/queue assumptions and all invalid/failure counts. Promotion is an auditable decision naming the bundle, supported regime, evidence and rollback choice. A mutable manifest flag grants no trading authority. The baseline can proceed toward a supervised pilot independently of learned-policy promotion.

**Commits:** recorded-session replay/reporting; evaluation dataset/report tooling; immutable promotion/eligibility records; trader workflow refinements from design-user evidence.

**Exit gate:** a trader can inspect completion, fees, residual quantity, failure reasons and provenance. Evaluation is reproducible; unsupported/unvalidated models cannot become live by configuration alone.

### Stage 6 — private venue integration and deployment

Implement Kraken private connectivity against the persistent mock first: instrument metadata, credential loading, submit/cancel, private events, balance/open-order/execution reconciliation and snapshot/stream handoff. Respect account and wider venue budgets; reserve cancellation/recovery capacity independently of creates. Pre-existing external orders and unexplained balances block readiness under the initial exclusive-management assumption.

Package profiles with explicit process configuration, migrations, dependency locking, generated-code CI, health/readiness checks and durable volumes. Keep secrets out of browser/Python/logs. Hosted access requires tenant-scoped authentication, TLS, credential storage/rotation and bounded API/history work. Deployment rollback uses expand/migrate/contract schemas and cannot reverse venue fills.

**Commits:** private transport; reconciliation and rate budgets; process/deployment packaging and CI; backup/restore and operator runbooks.

**Exit gate:** successful real public capture, authorized read-only private reconciliation and mock order-lifecycle/fault evidence are required before a separately authorized first live order. Collect real submission/fill/cancel evidence within that bounded pilot. Perform backup/WAL restore into an isolated paused deployment, verify state, then reconcile. No private credentials or pilot authorization is assumed by this plan.

### Stage 7 — advertise capacity, then add executor shards

Run the section 13 load-plus-fault workload and 24-hour soak. Ramp public input from a small baseline toward the candidate 50,000 events/second; declare book/trade mix, payloads and affected accounts. Measure the end-to-end percentiles directly. Candidate 100-account/ten-book capacity remains an acceptance workload until demonstrated.

Publish a release capacity manifest: active accounts, instruments, cadence, source/private-event mix, inference workers, database connection total, queue limits, recovery debt, retained storage, cost per active account and recovery headroom. Derive admission from the last passing cohort, including the declared failure/drain scenario. The previous five-minute RPO and sixty-minute paused-restore targets remain provisional until demonstrated; any database loss requires venue reconciliation before sending.

Extract deployments when measurements justify the change:

- **Executor shards:** account CPU/mailbox pressure remains limiting after profiling. Persist assignments; transfer by drain, old-sender isolation, reconciliation and explicit activation. A hot single account stays serialized.
- **Independent ingest:** parsing/recording/fanout materially interferes with execution latency. Preserve source identities and loss detection across transport.
- **Inference replicas:** measured queue age or worker saturation consumes the deadline budget. Admission must include model compatibility and burst limits.
- **API/history replicas:** reads/subscriptions interfere with execution. Commands route to the owner; delayed historical reads carry an as-of revision. Execution reads authoritative state.
- **Storage partitioning/sharding:** measured commit/I/O/retention/recovery constraints require it. Keep one account's financial transition within one transactional shard. Define identity/uniqueness and resumable migration first.

No Kafka, Redis or Kubernetes dependency is required for the first delivery. Introduce each only for a measured need with an explicit failure/recovery contract. If a shard is lost and capacity is insufficient, preserve safety and reduce admission rather than forcing unsafe failover.

**Exit gate:** measured limits and overload behavior are documented; queues recover after bursts; no accounting/ownership violations occur; restore and sender-isolation drills pass. Repeat these gates after any deployment split. Only the independently authorized pilot can establish initial real-trading operation.

### Planning estimate and delegation

Provisional effort for one experienced implementer with reviewed subagents: Stages 2–4 **10–18 engineering days**, Stages 5–6 **10–20**, and final Stage 7 evidence **3–6**. The remaining allowance is **23–44 engineering days**, plus elapsed soak, integration buffer and external access/design-user gates. Stages 0–1 have already been delivered locally and are excluded. These are planning allowances, not forecasts derived from measured delivery velocity; re-estimate after Stage 2 exposes the actual persistence and inference costs. Executor/storage sharding beyond demonstrated beta capacity is conditional and excluded from that estimate.

Use GPT-6.1-sol for financial semantics, account lifecycle, ordering/cursor design, recovery, integration review and release decisions. Use cheaper subagents for disjoint fixtures, straightforward projection/UI wiring, workload manifests and documentation against approved interfaces. Do not let multiple agents independently alter financial contracts. Every part receives its own commit and evidence record; planning commits do not imply implementation checks were rerun.

### Independent training and cutover gates

Keep Python training runnable and the current Python replay backend as its default. The recorded bridge result is about 23k transitions/second versus Python's 37–46k on the synthetic comparison; native Go throughput is a different measurement. Profile batch size, serialization, allocations, environment parallelism, reset/report cost and complete PPO wall time. Consider packed buffers or another transport only after identifying the actual cost; preserve operation identity, environment isolation and crash behavior.

Switch the default only after exact economic conformance, approved feature tolerance, invalid-trajectory handling, policy reevaluation and section 13 throughput/overhead gates pass on representative tapes/actions. Do not retire Python reference execution until its replacement and rollback window pass. Python Gym/training/evaluation remain supported regardless of the execution-backend cutover.

Maintain three separate release records: **product admission**, **Go training default**, and **live authorization**. Each names immutable evidence, approved limits, decision owner and rollback procedure. To roll back execution: stop admission/new sends, isolate the outgoing sender, drain/reconcile outcomes, verify schema/bundle compatibility, establish the new owner, then resume only when ready. A binary rollback or database restore cannot undo fills already accepted by a venue.

## 17. Evidence boundaries

The module mapping is based on the current repository. The architectural choices, timelines, numeric targets, and deployment topology are proposed decisions. Documentation links support specific platform capabilities, not a claim that LiquidityLab already implements them. Current behavior and known limits remain recorded in [verification.md](verification.md) and [simulation.md](simulation.md).
