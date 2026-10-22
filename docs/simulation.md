# Simulation contract and limitations

LiquidityLab's replay is a deterministic execution simulator. It gives the policy only events available by the current relative receipt time and preserves file receipt order, requiring nondecreasing receipt times. `receipt_index` is preserved as identity metadata; equal-time events keep their input order. Event time is a float in seconds from the beginning of the fixture; The combination of file order and receipt time defines causal order. A late-arriving event must not be moved earlier merely because its venue timestamp is earlier. The included compact fixtures use `time` as the receipt-time coordinate.

## Market events

Each JSONL record has `kind`, `time`, `receipt_index`, and `valid`. A `snapshot` establishes the bid and ask book as price/quantity string pairs. A `book` record supplies **absolute quantities** for the listed levels; it is not a signed delta. A zero quantity removes a level. A `trade` has a price, quantity, `side` (`buy` or `sell`, meaning aggressor direction), and stable `trade_id`. A `gap` invalidates the book until a valid replacement snapshot. Invalid intervals must not be treated as healthy observations or as tradable liquidity.

The fixture includes a short gap. That gap intentionally makes data health observable and is not an exchange recording. Do not infer missing levels or trades during it.

## Exact accounting

Plans, prices, quantities, fees, and balances use `Decimal`; JSON monetary values are strings to avoid binary-float rounding. For a fill, quote notional is `quantity × price`. A buy subtracts quote notional and quote-denominated fees from quote balance, adds gross base quantity, then subtracts any base-denominated fee. A sell subtracts gross base quantity and any base fee, and adds quote notional less quote fees. Negative balances, duplicate conflicting execution IDs, fills beyond the parent quantity, and fills across the hard limit are rejected. Identical duplicate execution IDs are idempotent.

Execution cost is measured against the initial midpoint with a positive sign for adverse cost on either side. Fees are added, converting base fees at their own fill price. Remaining quantity is marked against the frozen settlement midpoint, so the terminal objective includes an explicit residual opportunity-cost term. This is a modeled comparison, not a realized accounting gain.

## Order and fill assumptions

Decisions use a fixed one-second interval and the six semantic actions defined in `contracts.Action`: hold, cancel, small/normal passive, and small/normal aggressive. The action compiler clips order size to the remaining obligation, balances, configured child limit, and participation cap, then rounds down to the instrument lot. It never increases size to satisfy minimum venue rules. A passive order is a limit order and only trades when the modeled queue ahead has been consumed by eligible opposite-side trade flow; a quote touching the order price is not sufficient. An aggressive order is a boundary-capped IOC-style limit and can remain unfilled.

Simulated fills use only eligible recorded trade flow and available book state. An IOC fill consumes simulated depth persistently, so the same unchanged historical quantity cannot be reused at each decision. Absolute L2 updates retain that consumption debt. A zero-quantity removal clears debt at that level, allowing a later explicit reappearance to supply fresh liquidity. Passive fills advance solely through qualifying trade prints at the exact child price; L2 reductions never generate passive fills. This conservative approximation cannot reconstruct individual queue position from aggregated depth. L2 reductions and trades may describe overlapping activity; fixtures are authored to avoid intentionally counting one reduction as a second independent execution signal.

Submission and cancellation have modeled latency. Until submission acknowledgement, or until cancellation reaches a known final state, the child remains unresolved and its quantity is reserved. A replacement waits for cancellation settlement; it cannot assume a cancel request instantly removed the order. These timing values are simulator configuration, not an exchange latency promise.

## Deadline and settlement

At the plan deadline, the simulator stops new decisions/submissions, requests cancellation of outstanding orders, and allows a 30-second settlement window by default. Events received during that window can still settle fills, including cancellation-race fills. The residual reference midpoint is frozen at settlement start. If order state remains unresolved at the end of the allowance, the episode must be truncated rather than scored as a clean completed execution. The simulator does not force-fill the remaining parent quantity: a partial result is a valid outcome, especially when the limit prevents completion.

## Warmup, fees, and venue rules

The current environment uses compact features available from the current book and completed decision intervals. Price return and log-return standard deviation use up to the last ten completed intervals, growing from the episode's initial midpoint until enough history exists. The signed trade-flow feature accumulates from episode start. These fields are named in the `compact-v1` schema and policy manifest. The environment does not implement the plan's proposed 60-second feature warmup before an episode. A fixture beginning without a valid snapshot is not decision-eligible.

Default fee rates and fee currency are synthetic assumptions from `FeeConfig`, not account-specific rates. `InstrumentRules` defaults are also illustrative. They do not replace current venue metadata, minimum notional, precision, account fee tier, or product status. Reports and examples should retain their synthetic/simulation label.

The defaults are simulation controls only. They establish repeatability for small scenarios; they are not a calibrated model of any exchange and do not establish expected real-world fill rates, costs, or profitability.

## Reward reconciliation

With side sign `s` (+1 buy, -1 sell), initial midpoint `m0`, and unfilled quantity `U`, marked cost is `s × (filled notional − filled quantity × m0) + quote-valued fees + s × U × (current midpoint − m0)`. At cutoff the residual midpoint freezes. Each step returns the negative marked-cost change in basis points of target notional; settlement adds the completion penalty once. The sum of floating-point rewards agrees with the independent terminal objective to numerical tolerance. A truncated report leaves the economic score invalid and its terminal objective unset.
