# Stage 3 shared market runtime design

Status: design preparation, 28 October 2026. This document specifies proposed work; it does not implement or activate Stage 3. Repository seams were inspected at `8c7e329`. Stage 2 service-demand, saturation, recovery-headroom and initial-admission gates remain prerequisites. No supported account count, throughput, latency or live readiness is claimed.

This design expands Stage 3 of [go-runtime-plan.md](go-runtime-plan.md). Preserve all ordered-fact, durable-source and ownership gates below when dividing implementation into commits. Evidence for one synthetic paper account does not establish shared runtime capacity.

## Existing implementation and seams

- `cmd/liquiditylabd/paper.go`: `paperDaemon` and `newPaperDaemon` own one actor, source, inference client and store. `Ingest` waits for that actor under `ingress`; a synchronous loop over actors would let the slowest consumer stall the source. `Close` contains single-account cancellation and lifecycle logic.
- `internal/api/handler.go`: `api.Authority` already presents `View` and `Do`; `Config.Accounts` supports multiple exact tenant/account pairs. Admission is process-wide; one caller/history workload can consume all HTTP slots. `list` queries authorities serially.
- `internal/runtime/actor.go`: `submit`, `loop`, `handle`, `maintenance` and `advance` serialize one account and separate control, ordinary work and fact wakeups. `market` applies public levels and calls the paper matcher. `bootstrapState` and `persistRevision` bind immutable configuration and durable snapshots.
- `internal/runtime/facts.go`: `drainFacts` captures a fixed observed watermark, then drains it across potentially many transactions. Its 16-row read batch is not a scheduling bound. `applyFill` preserves financial changes, execution deduplication and reservation changes through `Store.Apply`.
- `internal/runtime/paper.go`: `ObserveTrade` freezes the first matching outcome, including ignored trades, before changing account state. `paperTradeIdentity` distinguishes venue trade identity from receipt-session identity. Retry must never rematch against a later child.
- `internal/execution/book.go`: `Book` combines public levels with account-specific simulated consumption `Debt`. Sharing this mutable pointer would incorrectly share simulated liquidity consumption.
- `internal/store/postgres/paper.go`: `ClaimPaperOwnership` fences database-backed paper effects and can advance an existing paper ownership epoch. A second process must verify durable assignment before calling `runtime.New`; construction must not silently transfer assignment.
- `internal/store/postgres/inbox.go`: `RecordObservation` persists before notification, deduplicates exact identities and bounds pending count/bytes. Rejection before persistence still requires upstream retention/retry.
- `internal/store/postgres/dispatch.go`: `AuthorizeDispatch` checks account readiness, market validity, revisions and unprocessed observations atomically before handoff. Extend this gate for shared-source debt.
- `internal/marketdata/stream.go`: `Normalizer.Accept` owns receipt order, checksum validation and source/session discontinuities. `captureSink.persist` in `capture.go` syncs raw receipts, but raw files do not yet define a shared source catalog, subscriptions, retention or account recovery cursors. Synthetic input has no equivalent durable source.

## Small public interfaces

Keep the existing actor as the sole financial writer. Introduce three deep modules rather than a general executor framework.

### Account supervisor

Proposed `internal/runtime/supervisor.go` public operations:

```go
NewSupervisor(ctx, Config) (*Supervisor, error)
Supervisor.Authority(key pg.Key) (AccountAuthority, error)
Supervisor.Close(ctx) error
```

`Config` contains an explicit bounded startup account manifest, process identity, one shared store, actor construction inputs and resource limits. `AccountAuthority` retains existing `View`/`Do` behavior. Routing, lifecycle, assignment verification and outstanding-request accounting stay inside the module. Start with a fixed account set; dynamic account creation and automatic reassignment are separate work.

Persist account assignment independently of its current lease: logical account key, assigned process, assignment generation and lifecycle state. Actor construction verifies the assignment transactionally. Transfer requires explicit drain and activation. Hash placement may suggest an assignment; adding a process cannot move financial ownership. Shutdown closes admission, drains accepted requests and releases only authority owned by this supervisor.

Fixed actors with bounded queues and an explicit account cap are sufficient initially. Do not replace account serialization with a global worker pool unless measurements require it. Any runnable-work scheduler must retain per-account ordering and bounded turns.

### Shared market hub

Proposed `internal/marketdata/hub.go` operations:

```go
NewHub(config HubConfig, source DurableSource) (*Hub, error)
Hub.Publish(ctx, MarketInput) (SourcePosition, error)
Hub.Subscribe(ctx, Subscription) (*SubscriptionHandle, error)
Hub.Close(ctx) error
```

Key a hub by venue, instrument and immutable metadata revision. Each view/event carries source session, contiguous position, version and validity. Apply a public delta once; install an immutable view. Send bounded wakeups/source positions to consumers instead of unbounded channels of book copies.

View construction and version retention are internal. Do not expose mutable maps or arbitrary long-lived historical references. Bound current/checkpoint versions and reader lifetimes. Replaceable display views may coalesce only after preserving every required order-relevant event.

### Durable source delivery

Use PostgreSQL first: immutable normalized source events, stream heads/checkpoints, durable subscription start and account source cursor. Proposed store operations:

```go
AppendMarketSource(ctx, stream, event) (SourcePosition, error)
SubscribeMarket(ctx, key, stream, start) (Subscription, error)
PendingMarketSource(ctx, key, budget) (SourceBatch, error)
```

Extend transactional `Change` with consumed source position/reference. Do not expose a separate cursor-advance mutation: economic changes, frozen matching identity and cursor advancement must remain atomic. Durable source availability must precede any committed account reference. Redundant in-memory wakeups may coalesce because actors can recover work from durable positions.

## Ordering, source availability and recovery

1. Publish a normalized event durably before an account can reference it. A successful raw file write alone is insufficient unless its manifest, stable location, committed length and recovery availability participate in the contract.
2. Subscription activation names a valid checkpoint and exact start position. Earlier historical events cannot apply to a newly subscribed account. Retain stream/session identity across retries; reconnect starts a new source session.
3. Reduce account-required source events in order. At a trade's ordered position, freeze its outcome against the eligible child. Preserve ignored trades as deduplication facts. Retry/restart uses that result rather than recomputing against a newer child.
4. New child creation/authorization cannot overtake an already available required source range. Separate source and command channels do not establish this rule.
5. Enforce the source-debt gate durably in `Apply` and `AuthorizeDispatch`, checking the subscription cursor against the required source head/generation under transactional synchronization with publication. An in-memory comparison leaves a publication/commit race. Define lock order before implementation.
6. Account state, fill identity, reservations, command result, frozen matching outcome and consumed cursor commit atomically where required. A crash after a fill commit and before completing its source outcome must replay without duplicating the fill.
7. On restart recover the same ordered range and retain original first-observation binding. A source reference must never resolve to an unavailable event after a crash. Recovery stays paused until source consistency and existing paper reconciliation requirements pass.
8. Preserve `paperTradeIdentity` semantics: repeated venue trade IDs have one economic identity scoped by instrument; events without venue IDs require stable session and receipt position. Conflicting payloads under an identity quarantine the account.

## Shared levels and independent paper economics

Separate immutable public levels/validity from account-owned consumption debt and matching state. Reservations, fills, passive queue position and simulated liquidity consumption remain private. An account must never mutate another account's public view or simulation overlay.

Preserve existing debt semantics: a deleted public price clears debt at that price; an unchanged level does not replenish consumed liquidity. Intermediate updates can be financially relevant even when their final display view is identical. Begin with ordered delivery of required events and optimize coalescing only with equivalence evidence.

Initially retain independent simulations per account. Reports must identify that assumption; shared source data does not imply shared real venue queue competition. Measure source normalization/book application separately from per-account trade matching and receipt persistence.

## Bounded admission and scheduling

Bound active accounts, request counts, retained bytes and oldest-work age per account and process. One hot account receives only its bounded lane and cannot consume all runnable/admission capacity. Publication cannot synchronously wait for every account.

Refactor `drainFacts` into resumable turns bounded by count and measured elapsed time. Yield only between atomic transitions and retain its target/continuation. Pending required source/private facts block new creates, actions and dispatch; yielding must not restore readiness.

Reserve capacity for controls and reconciliation through actual database admission, in addition to mailbox channels. One shared pool needs an explicit process connection budget and a mechanism that keeps optional reads/decisions from consuming every available slot. History/list reads need bounded per-account admission. Reserve command capacity through the HTTP handler so blocked readers cannot monopolize it.

Keep inference outside account serialization, bounded globally, with one pending slot per active plan and pinned fallback. A full or stopped worker cannot consume control/recovery capacity. Stagger normal decision cadence while also measuring synchronized bursts.

Distinguish stop-request receipt, durable control effect and completed cancellation/reconciliation. Preserve cancellation through quarantine and retain reservations until known-final evidence. `TestActorCancelControlPersistsThroughQuarantinedInbox` is an existing invariant to preserve. Any deferred control continuation must retain command identity and prerequisite source position; accepted work remains discoverable after caller timeout.

## Loss, retention and rejection

Public gaps, bad checksums, source/session changes and missed required consumer events invalidate affected price-sensitive execution until replay/resnapshot and recovery pass. Installing the latest display view is insufficient evidence of ordered recovery.

Private facts and paper matching outcomes cannot disappear or expire as load shedding. Rejection before durable acceptance is explicit and leaves the upstream source responsible for retaining and retrying the exact identity. A durable insertion failure cannot be described as accepted work. Retain required durable facts or declare explicit readiness failure with a recoverable source gap.

Bound source retention. A stalled account cannot pin unlimited history. Before deleting required events, require their economic application/checkpoint or durable archival availability. If the hard retention budget cannot be satisfied, stop admission/publication and record a gap. Never delete required recovery facts and claim readiness. Record every rejection, invalidation, paused account and unavailable recovery interval alongside latency.

## Canonical venue account identity

Keep `pg.Key{TenantID, AccountID}` for application routing. Add a durable identity consisting of venue and canonical venue account/subaccount ID, bound to exactly one managed financial account/reservation domain across logical aliases. Credential IDs map to that identity; rotation preserves it. Ambiguous or unverified credential aliases cannot establish readiness.

Paper accounts use an explicitly synthetic identity namespace. Private identity verification is required before live activation. Database ownership epochs fence local paper effects only and do not establish exchange sender isolation.

## Integration and commit decomposition

1. Durable assignment and canonical identity migrations after `0004_paper_effect_fencing.sql`; enforce assignment before actor construction and prevent aliases from creating independent ownership.
2. Bounded supervisor, shared store/pool, authority handles and owned shutdown. Replace single-account construction/lifecycle in `newPaperDaemon` and `Close`; retain exact API scopes.
3. Immutable versioned public levels and account simulation overlay. Extract hub publication from `Ingest`/`invalidateLocked`; retain Normalizer source identity rules and ordered relevant updates.
4. Durable source events/subscriptions, first-observation binding and atomic cursors. Extend `State`, `bootstrapState`, `persistRevision`, `recoverPaper`, `Change` and `AuthorizeDispatch`. This is required Stage 3 delivery, not an optional later optimization.
5. Bounded fact turns, control/recovery reservation, per-account/process admission, history isolation and inference bounds. Preserve quarantined cancellation semantics.
6. Daemon integration and fault workloads with measured cohorts. Publish before/after service demand and recovery evidence; update admission only after a passing gate.

## Required acceptance evidence

- Assignment survives restart; a second process cannot steal assignment; aliases/credential rotation cannot create two financial owners; partial startup unwinds only owned actors.
- Tenant/account routing isolation, per-account/process explicit rejection, accepted-command lookup after timeout, bounded shutdown and no cross-account state mutation.
- One public application per delta; immutable view mutation isolation; metadata/session/version validation; source-gap propagation; bounded retained versions; preserved account debt behavior.
- Crash after source commit, after first binding, after fill commit and before cursor completion. Recovery reproduces balances, fees, fills, passive queue and debt exactly without duplication.
- Queued historical trade followed by new child creation; duplicate/conflicting trades; exact subscription start; unavailable source; source-head/command commit race; retention refusal.
- Sustained fact backlog, one hot account, synchronized decisions, blocked history/database, stopped policy worker, full consumer queue and quarantined private fact. Controls progress with separate receipt/effect/finality observations while required facts and holds remain intact.
- Compare independent-account economics against the current single-account workload. Measure shared book work, paper trade fanout and live-style private facts separately.
- Run 1, 10, 25 and then 100 mock accounts only after each preceding cohort passes. These are proposed workloads, not supported capacity. Stage 2 evidence must first identify per-account/per-instrument service demand, saturation and recovery headroom; Stage 3 remains inactive until its prerequisites pass.
