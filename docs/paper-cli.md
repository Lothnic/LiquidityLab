# Bounded recorded-data paper CLI

`liquiditylab paper` runs recorded public events through the durable PostgreSQL
paper account actor and its automatic baseline scheduler. Every balance, fee,
order and fill is simulated. It makes no exchange orders and does not establish
that a recording is an authentic or continuous real market capture. The current
CLI supports only `BTC/USD`, with the runtime's explicitly synthetic rules
(`0.01` price tick, `0.001` quantity lot) and synthetic quote fees (`0.001` maker,
`0.002` taker). These are assumptions, not current exchange metadata.

Build the command and use a dedicated development database:

```sh
go build -o work/liquiditylab ./cmd/liquiditylab
# Set LIQUIDITYLAB_DATABASE_URL in your environment to your development DSN.
work/liquiditylab paper \
  --events work/normalized.jsonl \
  --plan examples/paper-buy-plan.json \
  --schema ll_paper_example_01 \
  --account synthetic_example_01 \
  --initial-base 0 --initial-quote 20 \
  --policy scheduled --duration 30s > work/paper-summary.json
```

The DSN comes only from `LIQUIDITYLAB_DATABASE_URL`. Connection error text is
withheld to avoid printing credentials. `--schema` must explicitly identify an
isolated lowercase PostgreSQL schema; `public` and system schemas are refused.
The CLI creates/migrates that schema but never drops it. Both initial synthetic
balances are required and accept exact decimal text. A plan's `base_balance`
and `quote_budget` cannot exceed those balances.

Each run inserts a **new** synthetic account under tenant `synthetic-paper-cli`.
An existing account is refused before ownership is claimed or balances are
changed. `--account` defaults to `recorded-` plus the first 24 hexadecimal
characters of the recording's SHA-256, so retrying the same file in the same
schema does not silently create a replacement. Choose an explicit new account
or a new isolated schema for a separate experiment. Existing accounts require
an operator to inspect durable outcomes, isolate the previous sender and
reconcile through the runtime API; this command supplies no automatic takeover
or invented recovery proof. An interrupted or failed setup may already have
created an account, which remains available for that inspection.

## Input contract

`--events` consumes the Go command's normalized JSONL output, for example from:

```sh
work/liquiditylab normalize --input work/raw.jsonl --output work/normalized.jsonl
```

Every line is one object with integer `time_ns`, integer `receipt_index`, a
boolean `valid`, `kind` and `symbol: "BTC/USD"`. A present `type` must match
`kind`. Snapshot and book levels use pairs of exact decimal **strings**, as
emitted by `normalize`. Supported kinds are `snapshot`, `book`, `trade`, `gap`.
Normalized metadata such as source receipt index, exchange timestamp and
checksum is accepted; the CLI does not independently reverify it.

This intentionally synthetic two-event fixture runs for 1.5 seconds. It is a
format example, not evidence of a valid real capture:

```jsonl
{"kind":"snapshot","type":"snapshot","time_ns":0,"receipt_index":0,"symbol":"BTC/USD","valid":true,"bids":[["99.99","2"]],"asks":[["100.01","2"]]}
{"kind":"book","type":"book","time_ns":1500000000,"receipt_index":1,"symbol":"BTC/USD","valid":true}
```

Times must be nonnegative, nondecreasing and no greater than 24 hours.
Receipt indices must be contiguous from zero, matching the Go normalizer;
missing/repeated indices fail before account creation. Equal-time events retain line order.
A receipt-only trade uses the stable file-digest session identity plus its
receipt index. Venue trade identities retain the runtime's symbol scope. Book
revision identities combine that same session identity with receipt index.
All input is validated before account creation. Unsupported symbols, unknown
fields, missing required timing/identity fields, negative quantities, invalid
trade payloads, crossed/empty books falsely marked valid, oversized data and
empty recordings are refused. A feed gap or explicitly invalid event is
submitted to durably invalidate readiness, then terminates this session.

`--plan` is the Go `domain.Plan` JSON contract. Money is encoded as exact
strings and the deadline is integer `deadline_ns`. Use
[`examples/paper-buy-plan.json`](../examples/paper-buy-plan.json). The older
Python replay examples using `deadline` in seconds are not this contract.
Plan deadlines must be positive and at most 24 hours. A plan's limit can leave
quantity unfilled; there is no forced completion.

## Timing and stopping

Events arrive at their recorded receipt offsets from the first event, using
real elapsed wall time with Go's monotonic timer. The CLI does not accelerate
recordings. The actor's actual scheduler uses its default one-second decision
cadence; `scheduled` and `liquidity-aware` select the same shared baselines as
Go replay. Plan creation pins that baseline for the session. A short recording
ending before the first scheduler tick may legitimately have no fills. Equal
time events are supplied in order, while database processing takes real time.

Only after the first valid snapshot does the CLI explicitly reconcile the NEW
synthetic balances with evidence labeled `SIMULATED`, create a paused plan and
start it. That assertion is not a private exchange balance verification.
The first of end of recording, `--duration`, SIGINT or SIGTERM stops ingress.
A gap, lease loss, quarantined authority, history exhaustion or other failed
operation stops the session with an error. Shutdown requests cancellation of
an active/paused parent, drains accepted actor work and closes the actor.
Unknown outcomes retain their durable identities and reservations and require
operator recovery. Cancellation does not imply a previously attempted order
had no fill.

Defaults are `--duration 30s`, `--max-input-bytes 16777216`, `--max-events 10000`
and `--max-line-bytes 262144`. Hard maxima are 24 hours of running time, 64 MiB
of input, 10,000 events and 1 MiB per JSONL line. Plans are limited to 64 KiB;
events to 512 levels each and accumulated books to 2,048 levels. Empty input
returns immediately. Runtime/store history bounds also apply. Database setup
and each operation have separate deadlines (normally seven seconds), and
shutdown allows ten seconds for actor close; these bounded operations can add
time beyond the ingress duration.

## Durable result

Standard error states `PAPER / RECORDED DATA`. Standard output contains one JSON
summary after account creation, including when a running session fails:

- `profile`, `source`, simulation assumptions, stable recording/session identity
  and explicit synthetic reconciliation evidence;
- exact initial/final account balances, durable parent/child state, simulated
  fills and outstanding intents/reservations/facts;
- create/start/cancel command IDs, expected revisions, committed receipts or
  an explicit uncertain/error outcome;
- delivered/available event counts, stop reason, actor close result and
  `known_final`;
- exact journal entry counts and first/last revision grouped by kind, scanned
  in bounded pages; full snapshots remain in PostgreSQL.

A clean bounded stop returns exit status zero only when shutdown is known
final. Errors return nonzero; a summary with `known_final: false` must not be
interpreted as a resolved account. A stopped plan can be cancelled, expired or
completed, and partial/no fills are valid simulated outcomes. The durable
snapshot may differ from the last public book because ordinary book updates
are held in memory until a durable transition. PostgreSQL retains the account,
journal, paper transport outcomes and execution facts after the CLI exits.

## Checks

Parser and flag tests need no database. PostgreSQL checks create randomized
`ll_cli_test_*` schemas and drop only the schemas owned by those tests:

```sh
go test ./cmd/liquiditylab
LIQUIDITYLAB_TEST_POSTGRES_DSN='your isolated development DSN' \
  go test -race -count=1 ./cmd/liquiditylab
```

The 1.5-second synthetic fixture checks actual scheduler decisions, durable
paper fills, exact accounting, real-time pacing, cancellation/drain and refusal
to reset an existing account. Additional checks cover feed gaps, duration and
context interruption. These do not establish live execution correctness,
policy performance, process-kill recovery or production capacity.
