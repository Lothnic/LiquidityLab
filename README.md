# LiquidityLab

A crypto spot execution assistant for planning orders, supervising paper trades, and comparing execution strategies on recorded markets.

**Go owns execution and accounting. Python provides the RL environment, training, and policy inference. React provides the trader desk.**

**Status:** local paper trading is implemented. Balances, orders, fees, and fills are simulated; live exchange execution is not enabled. The current desk supports one BTC/USD paper account per daemon.

**Current milestone:** [hosted paper-trading beta](docs/hosted-paper-beta.md).
The beta includes a real RL-assisted paper demo, with visible model proposals,
Go execution checks and fallback behavior; the demo policy remains unvalidated.
Deployment preparation comes first; the hosting provider and domain will be
selected afterward.

Try the [RL paper demo](docs/rl-paper-demo.md) or the
[portable container setup](deployment/README.md). A trained policy completed a
synthetic `0.1 BTC` plan through 50 simulated fills in the local container stack.
Restart retained its report, fill identities and balances. The model remains
unvalidated; hosted access, reliability and operational release gates remain open.

## The trading workflow

1. **Set limits:** choose buy or sell, an amount, a hard price boundary, a deadline, and a maximum size per order.
2. **Review the plan:** inspect the baseline order preview and required funds before creating a paused plan. With RL enabled, this preview is not a model prediction.
3. **Start and supervise:** track fills, fees, remaining quantity, balances, and local reservations. Pause, resume, or cancel as needed.
4. **Inspect the record:** open session history, examine saved reports, and export results.

The deadline starts when a plan is created and continues while paused. Cancellation stays pending until the current order's final state is confirmed.

The default execution method is a deterministic baseline: `scheduled` or `liquidity-aware`. A Python policy can propose execution actions through an optional paper/shadow integration; Go validates proposals against the current execution constraints and applies the configured baseline or pause fallback.

## What is implemented

- **Durable paper execution:** PostgreSQL account state, exact decimal accounting, reservations, ordered observations, command retries, and ownership fencing.
- **Trader desk:** authenticated account access, plan review, execution controls, current fills, session history, and report exports.
- **Market data:** synthetic demo input, public Kraken recording and normalization, checksum validation, and verified recording archives.
- **Historical comparisons:** CLI and durable API jobs compare baseline simulations on the same verified recording, with explicit rules, fees, latency, and fill assumptions.
- **RL integration:** Python Gymnasium environments, PPO training, a Go replay bridge, bounded policy inference with fallback, and visible proposed/applied actions in the desk and saved reports.
- **Recovery evidence:** local process-kill, restart, accepted-fill recovery, and stale-owner checks against development PostgreSQL.

Historical comparison reports contain simulated references and alternatives. Their estimated cost differences are not realized savings. The replay comparison UI and reconstruction of a trader's actual execution remain unfinished.

## Run the paper desk

### Prerequisites

The verified development setup uses Linux, Go **1.26.8**, Node.js **26** / npm **11**, and PostgreSQL **18.6**. Python **3.12** and [`uv`](https://docs.astral.sh/uv/) are needed for the Python workflows below. Archive filesystem operations currently require Linux.

Start with an existing, dedicated development PostgreSQL database and a database role that can create the configured schema. The database and role in the example URL must already exist.

### Build and start

From the repository root:

```sh
npm --prefix apps/web ci
npm --prefix apps/web run build

mkdir -p build
go build -o build/liquiditylabd ./cmd/liquiditylabd

export LIQUIDITYLAB_DATABASE_URL='postgres://liquiditylab@localhost/liquiditylab'
export LIQUIDITYLAB_API_TOKEN="$(openssl rand -hex 32)"

build/liquiditylabd paper create \
  --listen 127.0.0.1:8080 \
  --schema ll_paper_demo \
  --account demo \
  --source synthetic \
  --web-dir apps/web/dist \
  --initial-base 0 \
  --initial-quote 1000 \
  --duration 30m
```

Open **[localhost:8080](http://127.0.0.1:8080/)** and paste the value of `LIQUIDITYLAB_API_TOKEN` into the access-token field. The token stays in tab memory and is cleared on disconnect. Use a fresh token for your server; never commit it.

This example starts with **0 synthetic BTC and 1,000 synthetic USD**. Prices come from a generated demo feed. The daemon serves the built UI and API from the same loopback address and applies database migrations automatically.

### Resume an existing account

Creation refuses to reset an existing account. To reopen it, use the same database URL, schema, and account with `resume`:

```sh
build/liquiditylabd paper resume \
  --listen 127.0.0.1:8080 \
  --schema ll_paper_demo \
  --account demo \
  --source synthetic \
  --web-dir apps/web/dist \
  --duration 30m
```

Resume preserves retained balances and deadlines, recovers accepted paper effects, and leaves recovered plans paused. Initial-balance flags are rejected in resume mode.

See the [paper server guide](docs/paper-server.md), [recovery guide](docs/paper-recovery.md), and [trader UI guide](docs/trader-ui.md) for source options, development setup, and lifecycle details.

## Replay and RL

### Run a baseline without PostgreSQL

```sh
uv sync --locked --extra dev --python 3.12
uv run --locked liquiditylab demo --policy scheduled --side buy
uv run --locked liquiditylab demo --policy liquidity-aware --side sell
```

Replay a checked-in synthetic tape with an explicit plan:

```sh
mkdir -p artifacts
uv run --locked liquiditylab replay examples/synthetic.jsonl \
  --plan examples/buy-plan.json \
  --policy scheduled \
  --output artifacts/buy-report.json
```

### Train a policy

The optional RL dependencies include PyTorch and Stable-Baselines3:

```sh
uv run --locked --extra rl python -m liquiditylab.training \
  examples/training-tape.jsonl \
  --plan examples/buy-plan.json \
  --output artifacts/policy-001 \
  --timesteps 2048 \
  --seed 7
```

Policy bundles start **unvalidated** and are not live eligible. Training on the example tape demonstrates the pipeline; policy quality still requires holdout evaluation and promotion gates. Python remains the default training backend; the measured synthetic Go bridge has not justified switching that default.

For Go-backed Gymnasium/PPO integration, see [Go replay and training](docs/go-replay.md). The current inference worker requires a bundle trained through the owned Go backend; the default Python bundle above is not compatible with that worker. Follow the [RL paper demo](docs/rl-paper-demo.md) to train and use a compatible bundle in the trader desk. For the request and fallback contract, see [policy runtime](docs/policy-runtime.md).

### Compare strategies on verified recordings

The Go CLI imports recordings into content-addressed archives and compares baseline scenarios with exact fills, fees, cashflows, and residual quantities. A retained public Kraken capture is included in [`examples/recordings/`](examples/recordings/).

Recorded paper sessions can [pin a verified sealed archive and decision cursors](docs/source-provenance.md). Legacy sessions and unsealed live inputs remain unbound; matching a symbol or timestamp does not establish source lineage.

Follow the [archive guide](docs/market-archive.md) and [historical comparison guide](docs/historical-replay.md). To run comparisons as persistent authenticated API jobs, start the paper daemon with `--replay-archive /absolute/archive/root`; see [replay jobs](docs/replay-jobs.md).

## Code layout

| Location | Responsibility |
| --- | --- |
| [`cmd/`](cmd/) | Go CLI and daemon entry points |
| [`internal/`](internal/) | Execution core, replay engine, account actors, market data, API, inference client, and persistence |
| [`migrations/`](migrations/) | Versioned PostgreSQL schema |
| [`apps/web/`](apps/web/) | React and TypeScript trader desk |
| [`src/liquiditylab/`](src/liquiditylab/) | Python reference engine, Gymnasium integration, training, and inference |
| [`deployment/`](deployment/) | Portable paper/CPU policy images, local Compose lifecycle, resource limits and retained demo evidence |
| [`proto/`](proto/), [`gen/`](gen/), [`conformance/`](conformance/) | Shared contracts, generated bindings, and cross-language fixtures |
| [`examples/`](examples/), [`benchmarks/`](benchmarks/) | Example plans/tapes, retained recordings, and scoped benchmark evidence |

Go is the authority for paper execution. Inference and analytical replay jobs run outside the account actor; neither can bypass its financial controls. Replay workers use a separate bounded database pool, and replay HTTP requests have separate admission slots from trading controls.

## Checks and evidence

Common development commands:

```sh
go test -race ./...
uv run --locked --extra dev pytest -q
npm --prefix apps/web test
npm --prefix apps/web run build
```

Integration coverage requires additional configuration. PostgreSQL checks use `LIQUIDITYLAB_TEST_POSTGRES_DSN`; Go/Python process checks use the interpreter and replay-binary settings documented in the [verification record](docs/verification.md) and [Go replay guide](docs/go-replay.md). Cases without their required configuration may be skipped. Shared contract verification is documented in [`conformance/`](conformance/README.md).

Recorded checks demonstrate software behavior within their stated scope. They do not establish production capacity, policy quality, or live-trading readiness.

## Remaining work

- Finish the trader replay UI, source reconstruction, and actual-session comparisons.
- Add shared feeds, account supervision, and bounded multi-account scheduling.
- Complete immutable policy evaluation and promotion gates.
- Implement private exchange transport, reconciliation, and a separately authorized bounded pilot.
- Complete deployment hardening, supported-capacity measurements, a 24-hour soak, backup/restore drills, and trader usability validation.

The [progress ledger](docs/implementation-progress.md) tracks delivered components and open acceptance gates. The [Go runtime plan](docs/go-runtime-plan.md) describes the architecture and delivery sequence; [simulation assumptions](docs/simulation.md) explains how to interpret replay results.
