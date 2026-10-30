# Bounded paper web server

`liquiditylabd paper` serves the built React operator UI and the authenticated
trader API from one loopback HTTP origin. It owns one new durable PostgreSQL
paper account (or resumes an existing one) and one serialized account actor. The daemon sends no exchange
orders: balances, rules, fees, decisions, orders and fills are simulated. A
Kraken public-market-data source does not enable private exchange access or a
live order adapter.

The scheduled baseline is the default. A local Python policy can be explicitly
enabled in paper/shadow mode with a pinned bundle and baseline/pause fallback;
see [policy-runtime.md](policy-runtime.md). The desk also serves retained
[session history](session-history.md). Neither feature enables live transport
or makes an unvalidated policy eligible for live use.

For an optional bounded local process-metrics export, add a new output path with
`--metrics-output`; see [runtime-metrics.md](runtime-metrics.md) for the JSONL
format, metric definitions, and limits.

## Start a synthetic session

Use a dedicated development PostgreSQL database. Build the UI and daemon, then
set a database URL and generate a fresh bearer token in the shell environment.
Do not commit the token, put it in a command-line argument, or reuse the
example token from tests.

```sh
cd apps/web
npm ci
npm run build
cd ../..

go build -o build/liquiditylabd ./cmd/liquiditylabd

export LIQUIDITYLAB_DATABASE_URL='postgres://liquiditylab@localhost/liquiditylab'
export LIQUIDITYLAB_API_TOKEN="$(openssl rand -hex 32)"

build/liquiditylabd paper create \
  --listen 127.0.0.1:8080 \
  --schema ll_paper_server_20261023_a \
  --account synthetic_20261023_a \
  --source synthetic \
  --web-dir apps/web/dist \
  --initial-base 0 \
  --initial-quote 100 \
  --duration 30m
```

Creation requires a new isolated account and explicit balances; it refuses to
reset an existing identity. `paper resume` reopens the same schema/account and
rejects initial-balance flags. See [paper-recovery.md](paper-recovery.md) for
the owned simulated recovery flow. Keep the database dedicated to development. Bind only to a
literal loopback IP with a numeric port; DNS names and non-loopback listeners
are refused. The default lifetime is 30 minutes and the maximum is 24 hours.
The UI and `/v1` API share the daemon origin, so open `http://127.0.0.1:8080/`
and enter the bearer token in the UI. The UI keeps that token in component
memory only; it does not save it in browser storage or a URL.

The synthetic source emits a generated BTC/USD book snapshot (bid
99.99 × 2, ask 100.01 × 2), then a sell trade at 99.99 for 0.4 BTC every
500 ms. These values are generated demo events, not exchange observations. The
new account starts only with the exact balances provided by the two required
flags. The server uses synthetic BTC/USD rules: 0.01 price tick, 0.001 lot,
0.001 minimum quantity and 0.01 minimum notional. Fees are 0.001 maker and
0.002 taker in quote currency, labeled `synthetic-assumption`. API responses
and the UI label the profile, source, fills and fees as paper or synthetic;
review those labels before acting on an account view.

## Kraken public data source

The default source is `kraken`, so select it only when you intend to capture
public market data. It requires `--capture-output` to name a path that does not
already exist, including its manifest paths. For example, add
`--source kraken --capture-output kraken-btc-usd.jsonl` to the command above.
Capture is bounded by frame and byte limits and is written as raw JSONL;
defaults are 100,000 frames and 64 MiB, with maxima of 1,000,000 frames and
1 GiB. It uses public market data only; no private exchange credentials or
order transport are configured. A recording path or Kraken source label does
not by itself establish that a real capture was completed, authentic,
continuous, or representative.

## Operation and recovery

Create starts with explicit synthetic balances. Resume obtains an atomic paper
ownership view, drains accepted outcomes, cancels remaining simulated children
without resubmitting attempted orders, and waits for a fresh valid source book.
The recovered plan remains paused until a new explicit trader resume command;
its original deadline and exact fills/fees remain unchanged. Contradictory facts
or unexplained holds keep recovery required. No replacement balance or external
sender-isolation assertion is supplied by the daemon.

A source gap, invalid event, source failure or natural end invalidates market
readiness. A later snapshot in that running source cannot clear the gap. Inspect
the retained account before explicit recovery with a fresh receipt session.
On unexpected source return, the daemon logs a UTC `observed_at` timestamp,
an allowlisted failure category and `invalidation=confirmed` or `unconfirmed`.
An unconfirmed invalidation has a separate sanitized diagnostic. The timestamp
records source return, not the last durable receipt or necessarily the start of
the incident. Raw connection errors, credentials and worker details are withheld.
A normal source cancellation during shutdown does not emit this failure message.
Preserve these logs and the account revision before an explicit resume; an HTTP
health check can pass while source readiness requires recovery. The daemon does
not automatically retry or resume trading after a source failure.

The fence applies only to the PostgreSQL-backed simulated venue; private exchange
balance verification, transport and exchange failover remain unavailable.

Shutdown on SIGINT, SIGTERM or lifetime expiry first refuses new HTTP writes,
then cancels source ingress and closes the actor before allowing up to five
seconds for HTTP clients to drain. The actor cancels an active plan, drains
accepted work and checks that the durable account is known final. A clean stop
reports `known_final=true` and retains the schema, account and journal. If the
source, actor, persistence or HTTP shutdown misses its bound, the command
returns an error; inspect the durable account and preserve command identities
before deciding on recovery. Do not infer that a timed-out request or
cancellation had no committed effect.

The daemon has bounded request, header, lifetime, capture, database-operation
and shutdown limits. It is a local paper tool, not a capacity-tested service.
This implementation does not establish model-policy performance, market-data
quality from an actual recording, live-adapter correctness, or readiness for
live execution. Live execution remains unavailable.

## Checks

### Optional historical comparison jobs

Add `--replay-archive /absolute/verified/archive/root` to create or resume to
enable [durable replay jobs](replay-jobs.md). The root must already exist as an
ordinary directory. Requests select only content IDs; the worker reverifies
each archive. It uses its own two-connection database pool and one calculation
slot. Replay HTTP requests have four separate slots, so slow report responses
do not consume the financial-control handler slots. Without the flag the replay
routes are disabled. No replay job sends orders or changes account revisions.

The local daemon integration check imports the retained real archive and
exercises scoped enqueue, exact retry, worker completion, metadata listing,
byte-verified export, shutdown join and a fresh-pool result read. These checks
do not establish supported service capacity or actual-session reconstruction.

### Existing lifecycle checks

The existing option tests cover nonnumeric and out-of-range listener ports.
PostgreSQL tests use randomized schemas and remove only the schema created by
each test. Configure `LIQUIDITYLAB_TEST_POSTGRES_DSN` for the database test
suite:

```sh
LIQUIDITYLAB_TEST_POSTGRES_DSN='postgres://liquiditylab@localhost/postgres' \
  go test -race -count=1 ./cmd/liquiditylabd
```

These checks exercise bounded lifecycle behavior with synthetic inputs; they
do not verify an exchange capture or live execution.
