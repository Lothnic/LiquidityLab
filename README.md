# LiquidityLab

LiquidityLab is a paper spot-execution system and research environment. It includes a deterministic Go execution runtime with durable PostgreSQL account state, an authenticated HTTP API and local paper server, a React operator desk, and Python replay, Gymnasium, training, and inference tools.

The paper runtime simulates balances, orders, and fills. It does not send private exchange orders. Public market data, when available, changes the market input only; it does not turn paper execution into live execution. Synthetic feed events and default rules or fees are assumptions, not exchange observations.

## Run the local paper desk

The supported local operator flow uses the bounded Go daemon and PostgreSQL. Install the web dependencies, build the UI, and create a paper account with an isolated development database and a fresh bearer token. Use `paper resume` to reopen that account with its retained balances and paused plan. Follow [paper-server.md](docs/paper-server.md) and [paper recovery](docs/paper-recovery.md) for commands, source options and recovery limits. The daemon serves the built UI and authenticated API on the same loopback origin.

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

The Go paper runtime, durable store and actor, authenticated API, bounded local server, Python inference worker/client, and React desk are implemented. The default execution mode is the deterministic baseline. Explicit paper/shadow opt-in connects asynchronous Python proposals with durable slot resolution and configured baseline/pause fallback; see [policy runtime](docs/policy-runtime.md). Revision-pinned [session history](docs/session-history.md) preserves exact accounting and stored event inspection. A real 50-frame Kraken recording with 45-event Go/Python normalization conformance is retained; this short capture verifies that slice, not sustained feed operations. Synthetic source events are generated demo input.

The Go CLI can [archive verified public recordings](docs/market-archive.md) and [compare bounded historical baseline simulations](docs/historical-replay.md). Recorded paper sessions can prospectively [pin a sealed archive and decision cursors](docs/source-provenance.md). The daemon optionally runs [durable replay jobs](docs/replay-jobs.md) with authenticated history, cancellation and exact report downloads. Every comparison scenario is an estimate with explicit assumptions. Actual-session comparison, unsealed live-source binding and trader replay UI remain outstanding.

Live private exchange access and order transport are unavailable. Local paper recovery has process-kill and suspended-sender checks; its database effect fence provides no external exchange fencing guarantee. Design-user acceptance, deployment hardening, policy evaluation/promotion, shared multi-account operation and measured product capacity remain outstanding. Stored event inspection does not replay market data. Replay throughput benchmarks do not establish supported account capacity. See the [implementation status](docs/implementation-progress.md), [runtime plan](docs/go-runtime-plan.md), and [verification evidence](docs/verification.md) for progress and limitations.

The JSONL fixture is intentionally small and deterministic. It covers relative receipt times from 0 through 64 seconds, including a snapshot, absolute quantity book updates, trades, and a feed gap. Its generated demo outcomes, balances, fee settings, and instrument assumptions do not establish execution quality or profitability. See [simulation assumptions](docs/simulation.md) before interpreting reports.
