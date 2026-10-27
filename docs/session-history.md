# Retained execution history

The authenticated paper API exposes revision-pinned session records. These are
stored accounting reports and journal inspection; they do not rerun market data
or estimate how a different execution policy would have traded.

## Routes and cursors

All routes use the existing exact bearer-token tenant/account scope:

- `GET /v1/accounts/{tenant}/{account}/sessions?before_revision=0&as_of_revision=0&limit=20`
  lists sessions newest creation first. Limits are 1–50. A zero as-of revision
  pins the read to the current account revision; reuse the returned
  `as_of_revision` and exclusive `next_before_revision` for subsequent pages.
- `GET /v1/accounts/{tenant}/{account}/sessions/{plan}?as_of_revision=0`
  returns a `session-report-v1` projection. `as_of_revision` is the requested
  historical account cutoff; `revision` is the last retained snapshot for that
  parent at or before the cutoff. They may differ after another parent starts.
- `GET /v1/accounts/{tenant}/{account}/sessions/{plan}/timeline?after_revision=0&as_of_revision=0&limit=100`
  returns ascending metadata with an exclusive `next_after_revision`. Limits
  are 1–200. Pin the timeline to the report's `as_of_revision` before paging.

Stop paging when a page is shorter than its limit. Empty pages retain their
cursor. Unknown/repeated keys, negative/fractional/overflow cursors, future
revisions and invalid limits are rejected. Reads use repeatable-read database
transactions and never mutate an account. Missing or unauthorized accounts
remain indistinguishable. Raw snapshots and journal detail are not exposed.

The handler accepts an optional `SessionReader`; a supplied PostgreSQL journal
store implements it automatically. History returns 503 when unavailable.

## Financial meaning

Reports preserve exact decimal quantities, prices, fees, completion, frozen
arrival benchmark, child state, instrument rules, fee assumptions, simulation
configuration and policy provenance. The accounting helpers use all retained
parent fills. Known finality requires a terminal parent, no child and no known
pending/unresolved financial state or recovery/clock flag. An ended market feed
does not invalidate already settled fills.

`executed_cost_quote` is filled slippage against arrival plus quote-valued
fees. It excludes remaining quantity. `residual_opportunity_quote` marks the
remaining quantity against arrival: buy uses remaining × (reference − arrival),
sell uses the opposite sign. `marked_cost_estimate_quote` adds those two values.
These are cost measures/estimates, not P&L or demonstrated execution savings.

The residual reference uses the first terminal snapshot when available, even if
later source receipts change the book. Before terminal state it uses the observed
as-of book. Missing arrival or valid reference makes estimates unavailable.
Paper fills and fee settings remain simulated assumptions.

## Browser behavior and bounds

The History panel lists up to 200 sessions, loads up to 500 timeline events, and
renders no more than 200 fill rows on a page. Older/newer fill controls retain
the complete report in memory for export. Selecting an event requests the stored
report at that revision; the previous report keeps its retained-accounting label
while that request loads. Return-to-latest restores the selected retained report.
Refresh obtains a new list cutoff and clears selection.

History requests have a five-second timeout. Account/token generations and
request identities discard late results after refresh, selection changes or
return-to-latest. Financial fields and report scope/revisions are validated
before rendering. JSON export includes the complete retained fill report and
explicit timeline loaded-count, as-of, completeness and 500-event bound.

The store currently derives reports from bounded journal snapshots (1 MiB each,
16 KiB parent metadata). This is suitable for the current local paper workflow;
database query-plan, journal growth, retention and multi-account capacity remain
measurement/optimization work in the Go runtime plan.
