# Generated execution and replay checks

These bounded checks extend Phase 2's existing shared Python goldens and
focused lifecycle cases. They exercise the Go core without a database or venue.
They do not establish live fill realism, model quality or runtime capacity.

## Independent expectations

`internal/execution/property_test.go` covers:

- Buy/sell cashflows with base/quote fees using integer units and `math/big.Rat`,
  independent of production decimal arithmetic and fee/ledger helpers.
- Eight deterministic 32–60-fill sequences; fuzz inputs are capped at 64 bytes.
  Every fill checks balances, gross notional, fees, filled and remaining quantity.
- Exact duplicate no-ops and eight conflicting identity fields. Snapshots include
  the private execution identity map, and the original duplicate must still be
  recognized after rejecting a conflict.
- All six actions across 64 compiler configurations, including consumed depth,
  unavailable books, deadlines and pending children. Rational expectations cover
  price/lot grids, hard limits, participation, child caps and full-fee affordability.
  Compiler calls must leave ledger, book and child inputs unchanged.
- Exact affordability/minimum-notional thresholds ±0.000001 and lot dust cases.

`internal/replay/generated_test.go` crosses five event templates, before/at/after
timer ties, both sides and fee currencies in 60 deterministic scenarios:

- Receipt versus arrival, receipt versus cancellation, and a three-way tie.
- Duplicate trade receipts, consumed IOC depth, explicit deletion and refresh.
- Feed gaps, frozen residual valuation, deterministic repeated/reset trajectories,
  caller fixture immutability and rejected post-terminal steps.
- Rational balance/fee/marked-cost conservation after every step. Maker status
  and fee currency come from the fixture rather than the returned fill labels.
  Terminal objective and accumulated rewards match a separate rational objective
  calculation within the existing `1e-10` float tolerance. Invalid trajectories
  must retain an absent terminal score.

The templates vary amounts and prices using a seeded generator. They are not
arbitrary unrestricted event-stream generation, and their small numeric envelope
does not replace decimal overflow/bounds tests or the shared economic goldens.

## Executed checks — 29 October 2026

Go 1.26.8 on the development host; no production code changed for these checks.

```sh
go test -race -count=1 ./internal/execution ./internal/replay
GOMAXPROCS=2 go test ./internal/execution -run '^$' -fuzz '^FuzzLedgerConservationAndDuplicateAtomicity$' -fuzztime=15s -parallel=2
GOMAXPROCS=2 go test ./internal/execution -run '^$' -fuzz '^FuzzCompilerConstraints$' -fuzztime=15s -parallel=2
GOMAXPROCS=2 go test ./internal/replay -run '^$' -fuzz '^FuzzGeneratedReplaySequences$' -fuzztime=15s -parallel=2
```

The full two-package race run passed (execution 7.008 s, replay 1.298 s).
All three bounded fuzz campaigns passed: ledger 186 executions in 16.077 s,
compiler 80,117 in 15.102 s, replay 34,557 in 15.080 s. Fuzz execution counts
and cached interesting inputs vary across runs; these are short exploratory
campaigns, not exhaustive evidence. The ledger campaign spent most of its
budget after its first reported executions and provides much less exploration
than the other campaigns. No failing corpus entry was produced.

Independent review corrected two initially weak replay oracles before the
recorded replay campaign: trusting fill fee labels and checking only the
presence of terminal objectives. Neither correction changed product semantics.

The complete PostgreSQL/Python-integrated Go race suite and the Phase 2
requirement audit remain separate acceptance evidence.
