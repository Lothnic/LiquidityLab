# LiquidityLab

LiquidityLab is a paper spot-execution system and research environment. It includes a deterministic Go execution runtime with durable PostgreSQL account state, an authenticated HTTP API and local paper server, a React operator desk, and Python replay, Gymnasium, training, and inference tools.

The paper runtime simulates balances, orders, and fills. It does not send private exchange orders. Public market data, when available, changes the market input only; it does not turn paper execution into live execution. Synthetic feed events and default rules or fees are assumptions, not exchange observations.

## Run the local paper desk

The supported local operator flow uses the bounded Go daemon and a new PostgreSQL schema/account. Install the web dependencies, build the UI, and start the daemon with an isolated development database and a fresh bearer token. Follow [paper-server.md](docs/paper-server.md) for the complete command, database requirements, source options, shutdown behavior, and recovery limits. The daemon serves the built UI and authenticated API on the same loopback origin.

The [trader UI guide](docs/trader-ui.md) covers the Vite development server, operator flow, exact amount handling, preview and command semantics, freshness, and presentation limits. The UI labels the execution profile as Paper and identifies the data source separately.

## Python replay and training

Use Python 3.12 and `uv` for the Python environment:

```sh
uv sync --extra dev --python 3.12
```

Run a deterministic generated demo or replay the checked-in fixtures:

```sh
uv run liquiditylab demo --policy scheduled --side buy
uv run liquiditylab demo --policy liquidity-aware --side sell
uv run liquiditylab replay examples/synthetic.jsonl --plan examples/buy-plan.json --policy scheduled --output artifacts/buy-report.json
uv run liquiditylab replay examples/synthetic.jsonl --plan examples/sell-plan.json --policy scheduled --output artifacts/sell-report.json
```

Optional PPO training uses the separate RL extra because PyTorch is large:

```sh
uv run --extra rl python -m liquiditylab.training examples/training-tape.jsonl --plan examples/buy-plan.json --output artifacts/policy-001 --timesteps 2048 --seed 0
```

Bundles start as unvalidated and are not live eligible. Training and replay checks establish software behavior, not policy quality or execution performance. The Go execution core is the durable runtime and also supports replay and Python Gymnasium/SB3 integration; see [Go replay setup](docs/go-replay.md) and the [inference worker and Go client](docs/inference.md).

## Current scope and limits

The Go paper runtime, durable store and actor, authenticated API, bounded local server, Python inference worker/client, and React desk are implemented. The default execution mode is the deterministic baseline. Explicit paper/shadow opt-in connects asynchronous Python proposals with durable slot resolution and configured baseline/pause fallback; see [policy runtime](docs/policy-runtime.md). Revision-pinned [session history](docs/session-history.md) preserves exact accounting and stored event inspection. Public capture and normalization code exists, but a usable real Kraken recording has not been verified on the implementation host. Synthetic source events are generated demo input.

Live private exchange access and order transport are unavailable. Historical market counterfactual replay, existing-account daemon recovery and process-kill evidence, design-user acceptance, deployment hardening, policy evaluation/promotion, and measured product capacity remain outstanding. Stored event inspection does not replay market data. Replay throughput benchmarks do not establish supported account capacity. See the [implementation status](docs/implementation-progress.md), [runtime plan](docs/go-runtime-plan.md), and [verification evidence](docs/verification.md) for current progress and limitations.

The JSONL fixture is intentionally small and deterministic. It covers relative receipt times from 0 through 64 seconds, including a snapshot, absolute quantity book updates, trades, and a feed gap. Its generated demo outcomes, balances, fee settings, and instrument assumptions do not establish execution quality or profitability. See [simulation assumptions](docs/simulation.md) before interpreting reports.
