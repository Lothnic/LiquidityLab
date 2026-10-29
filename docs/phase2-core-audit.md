# Phase 2 core and replay acceptance — 29 October 2026

Scope: Phase 2 in [the Go runtime plan §12](go-runtime-plan.md#12-migration-phases-and-gates)
and the execution semantics in [§5](go-runtime-plan.md#5-one-execution-core-for-replay-and-production).
The inspected implementation and executed evidence satisfy this local core/replay
gate. Go training-default cutover, policy reevaluation, public/private transport,
supported capacity and release acceptance remain separate gates.

## Implementation requirements

- **Exact amounts, ledger and compiler:** `internal/domain/decimal.go` and
  `contracts.go` define the bounded amount/time/plan contracts. `internal/execution/`
  contains ledger, fee/reservation, grid/limit/affordability compilation, book
  depletion, passive matching, features and baseline functions. These calculations
  receive explicit inputs rather than database/network dependencies or wall time.
  Decimal bounds/malformed/rounding tests and the Phase 1 shared corpus remain
  in the combined conformance gate.
- **One shared financial implementation:** replay builds `x.NewLedger` and calls
  `x.Compile`, `x.FeeFor`, `x.BuildObservation` and shared baseline functions.
  Runtime `Actor.ledger` reconstructs that ledger; confirmed paper fills pass
  through its `Apply` and resulting deltas are persisted atomically. Runtime
  command/fact paths use `x.Compile`, `x.ReservationFor`, `x.BuildObservation`
  and `x.BaselineAction`. Persistence/transport/clocks remain runtime responsibilities;
  sharing calculations does not equate paper matching with real venue fills.
- **Deterministic replay/order lifecycle:** `internal/replay/engine.go` advances
  explicit integer nanoseconds and orders receipts/timers. Tape construction
  owns caller input and permits immutable sharing across isolated engines.
  Generated scenarios verify repeated trajectories, reset and input immutability.
- **Scheduled and liquidity-aware baselines:** `internal/execution/policy.go`,
  `policy_test.go` and replay policy wrappers cover explicit scheduler state,
  eligible-slice advancement and liquidity/urgency thresholds. The approved
  full-buy case exercises a baseline through the reference comparison.

## Required semantic evidence

1. **Receipt → arrival → cancel ties:** `TestReceiptArrivalAndCancellationTies`,
   `TestArrivalReceiptOrdering` and generated before/at/after scenarios check
   actual `New`/`Step` behavior, including a simultaneous three-way tie.
2. **Cancel retains obligations and races with fills:** passive matching tests
   accept eligible trades during `cancel_requested`; generated cases and the
   common cancellation golden preserve partial fills and reservations.
3. **Replacement waits for known finality:** the fifth Python-reference golden
   and `TestReplacementWaitsForKnownFinalWithRacingFill` check the intermediate
   timeline. Minimum resting age refuses a cancel; cancellation remains pending
   across a 0.14 fill; the next proposal cannot replace it until acknowledgement.
   The new child is then capped to the 0.06 remaining parent quantity. Exact
   quote balance is 86.12614, fee 0.01386, marked cost -0.15614 and final objective
   48.07. The original four golden cases were unchanged.
4. **Quote touch is insufficient for a fill:** `TestMatchPassiveTrade` ignores
   non-trade/invalid events and checks queue exhaustion. The replacement timeline
   remains unfilled across quote changes until its eligible trade arrives.
5. **Liquidity debt persists:** `TestBookDebtPersistsUntilExplicitDeletion`,
   `TestConsumedDepthPersistsAndIsolation` and generated repeated-IOC scenarios
   require explicit level deletion/refresh before consumed depth becomes available.
6. **Feed gap invalidates an active episode:** the common gap golden and generated
   gap/arrival tie retain an invalid, unscored trajectory.
7. **Residual reference freezes at cutoff:** the common cancellation golden and
   generated settlement price moves verify the decision-cutoff midpoint survives
   subsequent tape changes.
8. **Known partial versus unknown settlement:** common partial sell/cancel cases
   remain scored; `TestUnknownSettlementHasNoScore` retains the unresolved child
   and no terminal economic score.
9. **Late unrelated gaps cannot invalidate completed settlement:**
   `TestCompletionSkipsLaterGapAndResetsExactly` and the shared cancellation case
   terminate before later tape gaps.
10. **Terminal objective and residual penalty apply once:** shared goldens compare
    reward sums/objectives, generated cases compare accumulated rewards with an
    independent rational objective, and the replacement timeline checks its
    hand-calculated reward sequence. Post-terminal stepping must fail without
    mutating the report.

The [generated-check record](generated-core-checks.md) describes bounded seed
coverage, rational oracles, duplicate-identity atomicity, compiler thresholds,
three short fuzz campaigns and their limits. These are not exhaustive arbitrary
event-stream proofs. The Go/Python approved tapes compare exact monetary/outcome
fields, with existing two-float32-ULP feature and `1e-10` reward tolerances.
No tolerance was widened for this acceptance.

## Executed acceptance

At clean source `35fc7b7a65cb0cc251c618a9e99d70409a893cf0`:

- The [retained combined report](../conformance/results/20261029-phase2.json)
  passes all five steps: generated bindings, regenerated reference fixtures,
  four Go domain/execution/replay/RPC race packages, ordinary replay binary build
  and **198 Python checks with zero skips/errors/failures**. It binds all three
  fixture hashes and the owned executable identity. Five economic/observation
  cases are consumed directly by the Go golden comparisons.
- The complete uncached `go test -race -count=1 ./...` passed with the local
  development PostgreSQL DSN, actual Python interpreter and newly built replay
  binary configured. Daemon 52.093 s, runtime 12.660 s, store 4.114 s and inference
  7.133 s are package suite durations, not product latency. This includes
  runtime-facing mutable state and process recovery beyond the four-package gate.
- The focused replacement/common-golden Go race checks and hostile Decimal-context
  Python reference checks passed before the final full runs.

V1 amount/time differences and legacy float-timer ties are documented and tested
in `conformance/go-runtime.md` and `time-compatibility.json`. Existing trained
policies still require reevaluation before cutover/promotion. The measured Go
training bridge remains slower for the recorded workload, so Python remains the
default training backend. This Phase 2 acceptance changes no trading authority.
