# Durable paper account runtime

`internal/runtime` provides one serialized paper account authority backed by
PostgreSQL. It uses the same exact ledger, action compiler, reservation/fee
rules and passive trade matcher as Go replay. It cannot construct a live
transport. A trader API and product CLI are subsequent
integration components; this package alone is not a complete trading product.

## Lifecycle and commands

Create/migrate a store, create an account with synthetic balances, then call
`runtime.New(ctx, runtime.DefaultConfig(store, accountKey))`. The actor claims a
fresh ownership epoch and starts with invalid market data and readiness false.
Supply a fresh valid snapshot and an explicit paper `Reconcile` assertion with
balances before starting a plan. Reconciliation evidence is a caller assertion
for this simulated profile; it is not a verified private exchange snapshot.

`Do` accepts `create`, `start`, `pause`, `resume`, `cancel`, `deadline`, `action`
and `dispatch`, plus `decision` for a pinned baseline. Every command has a stable ID and expected account revision.
Identical retries return the original committed receipt; changed payloads under
the same ID conflict. An accepted request continues with an internal deadline
if its caller disappears. A caller timeout therefore means outcome unknown;
retain and retry the same ID and payload.

Creating a parent starts it paused. Starting pins its UTC deadline and relative
execution horizon. There is one active parent per account, including paused and
cancel-requested parents. A queued child and its reservation commit atomically.
Dispatch recompiles against the current book and validates readiness, lease,
revisions, metadata, balances and deadline before physical transport handoff.
Obsolete unsent children release their reservation. Attempted or unknown
children remain reserved until known final. A cancel request is distinguishable
from a final cancellation; a fill may still race with cancellation.

Public command/reconciliation IDs cannot use internal receipt prefixes:
`boot-`, `deadline-`, `dispatch-market-`, `market-invalid-`, `clock-invalid-`,
`observation-`, `paper-`, `transport-`, or `runtime-`. Fill ingress is trusted
runtime input and also excludes these event-ID prefixes.

## Baseline scheduling

`Config.AutoExecute` enables automatic decisions and dispatch; its default is
false for explicit command orchestration. `Config.Baseline` defaults to
`scheduled`. A create command can choose `scheduled` or `liquidity-aware` and
pins that choice to its parent. A `decision` command must use the pinned name.
The account transaction stores the schedule cursor, next decision time, count,
causal midpoint window, and the exact 20-feature observation used for that
decision. Identical command retries do not advance the schedule again.

Scheduling runs while the parent is active and the account/book are ready.
It dispatches a queued intent only after the usual final checks. Pause, expired
deadlines and recovery prevent new decisions. A restart does not run a burst of
every missed cadence; the next actual decision establishes its next due time.
The scheduled baseline still advances its explicit 15-second slice cursor once
per eligible decision, preserving replay semantics. Model inference is not yet
wired into this scheduler; its worker/client are separate components.

## Market data and paper effects

Use `Market` for snapshots, book updates, gaps and trades with a venue trade ID.
Use `MarketFromSession` with a stable receipt-session ID for trades lacking a
venue ID; those trades require a receipt index. A book snapshot revision is not
a receipt-session identity. Ingest must preserve event ordering and declare any
lost deltas as a gap. The actor does not independently prove upstream feed
continuity from arbitrary caller-provided snapshots.

Aggressive children consume available simulated book liquidity and book exact
taker fees. Passive children rest only when the quote does not cross. An
opposite-side public trade exactly at the passive price depletes queue ahead,
then can fill the child with exact maker fees. Touching a quote, or a trade
through the price, does not itself prove a fill under this conservative model.
These are simulated fills and fees, not exchange executions.

The paper transport persists the complete outcome before the account reducer
changes balances. Submission, cancellation and public-trade facts have stable
identities. Venue trade IDs are scoped by symbol; receipt-only identities are
scoped by session, symbol and index. Receipt time is not part of the economic
identity, so a reconnect/retry can discover the first immutable outcome.
Changed economic payloads conflict. Zero-fill trade outcomes also persist,
preventing a retry from filling a later child.

Each fill commits once with its reservation/balance change. The observation
cursor advances only after the containing outcome is fully reduced. This
permits recovery after the financial transaction but before cursor commit.
Contradictory or unexpected fills remain in the inbox and quarantine the
account for explicit resolution. Reconciled balances cannot resume a parent
whose rejected fill history is unresolved; such recovery requires aborting the
parent. Public-data invalidity is persisted with readiness false.

## Recovery and bounds

`Close` stops admission and drains accepted requests; it does not assert that
external outcomes are resolved or transfer ownership. Restart requires an
explicit previous-sender-isolated/reconciled proof, a new owner identity and a
fresh book. The database epoch rejects stale writes but is not an exchange
fence. Current restart tests establish durable checkpoints in one process;
process-kill and operational failover drills remain required.

Elapsed execution time uses a monotonic process anchor after restart's initial
UTC reconciliation. Backward clock movement, or a wall/monotonic divergence
above one second, durably invalidates market readiness. A fresh snapshot plus
explicit reconciliation is required; a still-active parent must be aborted
after such a discontinuity.

Defaults bound ordinary/control/fact notifications to 64/16/64 items, fill
history to 2,048, and unresolved fact history to 256. Individual public events
accept at most 512 levels; accumulated book state is capped at 2,048 levels.
The store bounds pending observations to 4,096 facts / 16 MiB and each snapshot
or observation payload to 1 MiB. Each drain takes a fixed initial inbox
watermark, so continuous ingress cannot move its finish line. Maintenance and
control remain runnable ahead of ordinary work, but a reducer/transport call
already executing is bounded rather than preempted. The proposed 100 ms control
latency and production account capacity have not been demonstrated.

This initial paper implementation writes every accepted trade outcome to
PostgreSQL, including zero-fill outcomes. Its write cost and history growth
must be measured before claiming feed/account capacity. Public book deltas are
kept in memory rather than written individually to account history.

## Verification

Use an isolated PostgreSQL development database; tests create randomized schemas
and drop only their own schemas:

```sh
LIQUIDITYLAB_TEST_POSTGRES_DSN='postgres://user@localhost/liquiditylab_test' \
  go test -race -count=1 ./internal/runtime ./internal/store/postgres
```

Coverage includes IOC accounting, manual and trade-driven passive fills,
deduplication/conflicts, queued pause/cancel behavior, lease loss, startup
readiness, ownership isolation, deep-copy views, invalid books, clock
quarantine, durable source outcomes and pre-cursor recovery. These checks do
not establish live correctness, policy quality, capacity or restore readiness.
