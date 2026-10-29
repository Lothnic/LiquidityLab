# Go replay and Python training

Go owns the versioned v1 replay engine; Python provides Gymnasium and PPO. The
Python v0 engine remains the reference/default backend while migration performance
and integration gates are evaluated. These workers never load exchange secrets or
submit real orders.

## Build and run

Use the Go version in `go.mod` and Python 3.12:

```sh
mkdir -p build
go build -o build/liquiditylabd ./cmd/liquiditylabd
go build -o build/liquiditylab ./cmd/liquiditylab
uv sync --locked --extra go --extra rl --extra dev
```

Pass an absolute executable path, or set `LIQUIDITYLAB_REPLAY_BINARY` to it:

```python
import gymnasium as gym
import liquiditylab

with gym.make("LiquidityLab-SpotExecution-v1", binary="/absolute/path/build/liquiditylabd") as env:
    observation, info = env.reset(seed=7)
    for _ in range(40):
        observation, reward, terminated, truncated, info = env.step(5)
        if terminated or truncated:
            report = env.unwrapped.report()
            break
```

The default episode is synthetic. `GoLiquidityEnv` accepts `events`, `plan`,
`rules`, `fees`, and `config`, matching the reference constructor. Monetary values
must be Decimal, integer, or decimal strings. Normalized Go tapes use `time_ns`;
legacy reference tapes use decimal-written seconds. Do not pass JSON float money.
Reports contain integer nanosecond times and exact monetary strings.

An owned process uses a private Unix socket and terminates on environment close.
Alternatively run `build/liquiditylabd replay --listen 127.0.0.1:50051` and pass
`endpoint="127.0.0.1:50051"`. External endpoints are local-only and idle batches
expire after fifteen minutes. Closing a client does not terminate an externally
managed server. Creating a new server never resumes old handles.

## Batched PPO

```sh
uv run --locked --extra go --extra rl python -m liquiditylab.training \
  examples/training-tape.jsonl --plan examples/buy-plan.json \
  --backend go --replay-binary "$PWD/build/liquiditylabd" --num-envs 8 \
  --timesteps 2048 --seed 7 --output artifacts/go-policy-001
```

`make_go_vec_env` uses one Go batch with an immutable tape and isolated state per
environment. Each vector step sends one RPC; a separate batched reset handles
finished environments. SB3 receives terminal observations plus new reset
observations. Invalid/truncated episodes and unresolved RPC outcomes halt training
before the invalid transition reaches PPO. No Python execution fallback occurs.

Bundles include the exact owned Go binary SHA-256, Python implementation and
dependency lock hashes, dataset hash, v1 environment, and explicit policy-to-wire
action mapping. Changing a binary during training prevents bundle publication.
Training through an externally managed endpoint cannot currently publish a bundle
because the worker cannot establish binary provenance. A saved policy starts
`unvalidated`, `live_eligible: false`; holdout evaluation/promotion is still required.

The owned worker permits 100,000 sequenced mutations per batch and is bounded to a
maximum of one million through `max_operations` on the batch/environment APIs or
`--max-batch-operations` on the training CLI. The server CLI
server default is 10,000; `--max-operations-per-batch` exposes the retained-identity
memory tradeoff. Exceeding a limit fails the job explicitly. Long training jobs
need a deliberately selected budget; automatic batch rollover is not implemented.
Seeds currently label resets: the conservative simulator is deterministic and has
no randomized fills. Arbitrary per-environment Python attribute/method mutation
is unsupported. See [RPC resource/retry contract](../internal/replayrpc/NOTES.md).

## Verification

```sh
go test -race ./...
LIQUIDITYLAB_REPLAY_BINARY="$PWD/build/liquiditylabd" \
  uv run --locked --extra dev --extra rl --extra go pytest -q
```

Without the binary environment variable, real-process Python integration cases
are explicitly skipped. Verification includes real Gym checks, a dropped step
response safely retried with identical identity, worker death, malformed response
invalidation, independent batch state, and PPO save/load/inference. Generated-code
drift has its own command in [protocol.md](protocol.md).

## Public Go capture

```sh
build/liquiditylab capture --symbol BTC/USD --depth 10 --duration 30s \
  --max-frames 10000 --max-bytes 67108864 --output data/kraken-go.jsonl
build/liquiditylab normalize --input data/kraken-go.jsonl --output data/kraken-go-events.jsonl
```

Capture keeps raw decimals, validates checksums, writes explicit gaps, reconnects
for resnapshot, and emits a SHA-256 manifest. The default aggregate byte cap is
64MiB; normalization defaults to the same input-size limit and materializes the
result. Receipt metadata discontinuities invalidate continuity. An earlier
twelve-second host attempt received no frames and exited with `no_valid_book`;
that failed attempt remains historical evidence. A later verified public capture
is retained in [the 2026-10-28 Kraken BTC/USD recording](../examples/recordings/kraken-btcusd-20261028/)
with raw frames, manifest, normalized events, and Go/Python conformance evidence.
No private trading is claimed.
