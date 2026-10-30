# Local PPO paper demo

This guide runs the current end-to-end local path: train PPO against the Go v1
replay process, load that bundle in the Python inference worker, and let the Go
paper daemon validate and apply proposals (or its configured fallback). It uses
synthetic market data and synthetic balances. It does not submit exchange
orders.

The example is a wiring demo. The small training run and example tape do not
measure strategy quality. Every trained bundle remains `unvalidated` and
`live_eligible: false`; the runtime requires an explicit paper/shadow opt-in and
does not promote it.

## Build and train

Run from the `outputs/liquiditylab` project directory with Go and Python 3.12
installed. This creates an owned Go replay binary, which is required for bundle
provenance. The output directory must be new.

```sh
mkdir -p build artifacts
go build -o build/liquiditylabd ./cmd/liquiditylabd
go build -o build/liquiditylab ./cmd/liquiditylab
uv sync --locked --extra go --extra rl --extra dev

uv run --locked --extra go --extra rl python -m liquiditylab.training \
  examples/training-tape.jsonl \
  --plan examples/buy-plan.json \
  --backend go \
  --replay-binary "$PWD/build/liquiditylabd" \
  --num-envs 1 \
  --timesteps 2048 \
  --seed 7 \
  --output "$PWD/artifacts/ppo-demo-001"
```

The 2,048 step run is still a wiring demo, not a quality benchmark. Eight steps
is the minimum accepted run and is useful only as a boundary smoke check. Use a
fresh output directory for another run. Inspect
`artifacts/ppo-demo-001/manifest.json`; it records the dataset, policy, code,
dependency lock, and Go executable hashes. The inference worker verifies the
policy ZIP hash when it starts.

## Start the worker and paper daemon

The paper daemon requires a PostgreSQL DSN and an API token of 32–4096 bytes
without whitespace. Provide an isolated database schema and a unique account ID
for `create`. The same schema/account is supplied to `resume`; restart recovers
the retained account, pauses unfinished plans for reconciliation, and preserves
the pinned bundle. Never use `create` against an existing account.

Build the current web assets first if they are not already present:

```sh
cd apps/web
npm ci
npm run build
cd ../..
```

In terminal 1, make a private temporary directory and launch the worker on a
Unix socket inside it. `mktemp -d` creates a directory private to your user;
the socket itself is also created with mode `0600`.

```sh
POLICY_SOCKET_DIR=$(mktemp -d "${TMPDIR:-/tmp}/liquiditylab-policy.XXXXXX")
chmod 700 "$POLICY_SOCKET_DIR"
printf 'Use this socket directory in terminal 2: %s\n' "$POLICY_SOCKET_DIR"
uv run --locked --extra rl python -m liquiditylab.inference \
  "$PWD/artifacts/ppo-demo-001" \
  --listen "unix:$POLICY_SOCKET_DIR/worker.sock"
```

In terminal 2, set `LIQUIDITYLAB_DATABASE_URL` and `LIQUIDITYLAB_API_TOKEN`
from your local PostgreSQL setup, then create a synthetic paper account. The
initial balances below are invented demo funds, not exchange balances.

```sh
export LIQUIDITYLAB_DATABASE_URL='postgres://USER:PASSWORD@127.0.0.1:5432/liquiditylab?sslmode=disable'
export LIQUIDITYLAB_API_TOKEN='replace-with-a-local-random-token-of-at-least-32-bytes'
POLICY_SHA=$(sha256sum artifacts/ppo-demo-001/policy.zip | cut -d ' ' -f 1)
# Replace this with the exact directory printed by terminal 1.
POLICY_SOCKET_DIR=/tmp/liquiditylab-policy.REPLACE_WITH_PRINTED_SUFFIX

build/liquiditylabd paper create \
  --schema rl_demo_001 \
  --account rl-demo-001 \
  --source synthetic \
  --initial-base 0.01 \
  --initial-quote 1000 \
  --policy-target "unix:$POLICY_SOCKET_DIR/worker.sock" \
  --policy-sha256 "$POLICY_SHA" \
  --policy-allow-unvalidated \
  --policy-profile paper \
  --policy-fallback baseline \
  --policy-timeout 100ms \
  --listen 127.0.0.1:8080 \
  --web-dir apps/web/dist \
  --duration 30m
```

Open `http://127.0.0.1:8080`. The synthetic market source emits a deterministic
demo stream near 100 USD/BTC. For a concrete buy walkthrough, enter amount
`0.01 BTC`, maximum buy price `102 USD`, time to complete `1` minute, maximum
per order `0.002 BTC`, cash budget `1000 USD`, and asset budget `0 BTC`. Choose
Liquidity-aware under execution settings, review the first order, create the
paused plan, then start it. The preview is the deterministic baseline preview;
it is not a PPO prediction. The running plan exercises the Go account,
reservation, execution-guard, policy, and audit path. To stop and
recover the same account, stop the daemon and run `build/liquiditylabd paper
resume` with the same `--schema`, `--account`, policy target/hash/opt-in/profile/
fallback/timeout and other settings. Do not pass initial balances to `resume`:
the retained balances are authoritative. Preserve the worker socket and bundle
path for the resume invocation. For this command the daemon serves the built
frontend from `apps/web/dist` (the default `--web-dir`).

## Containerized alternative

The container path was run on 2026-10-30. From the repository root,
`deployment/demo.sh init`, `build`, `prepare`, then `create` initialized local
secrets, built the images, trained a 2,048 step Go-backed CPU PPO bundle, and
started paper mode on host loopback port 18882. PostgreSQL and the worker were
healthy, and create/start/cancel/stop/resume commands ran. That initial run did
not produce an accepted policy action or a fill: its first decision paused with
`expired`, and operator resumes paused with `source_facts_pending`. In a
separate worker-loss step, stopping the worker followed by an authorized
operator resume recorded `fallback_pause` / `worker_unavailable`. A complete
Compose stop/resume retained the cancelled session and its empty fill list; it
does not demonstrate active-session recovery or fill retention. See the
[deployment run notes](../deployment/README.md) and
[container lifecycle evidence](../deployment/evidence/2026-10-30/lifecycle-summary.json)
for the tested scope, revisions, and limitations. The container defaults to a synthetic feed
and `pause` fallback, uses a private Unix socket, and binds the web endpoint to
host loopback.

After the bounded result handoff in `b795231`, the same immutable model completed
an actual `0.1 BTC` paper plan through 50 simulated fills in about 126 seconds,
without operator resumes or changes to the 100 ms proposal budget or 500 ms feed.
A second plan produced one maker fill and subsequently paused on expiry before
the worker was stopped. Stop/resume retained both revision-pinned reports, all
51 execution identities and exact balances. See the
[post-handoff evidence](../deployment/evidence/2026-10-30-handoff/README.md).
The [31 October refresh](../deployment/evidence/2026-10-31-refresh/README.md)
also retained these records and recovered a ready account with the rebuilt UI.
These bounded observations establish the integration and simulated execution;
they do not establish policy quality, reliable latency or continuous feed service.

The Python worker owns one immutable PPO model and handles one inference at a
time. The Go actor freezes a bound request, calls the worker asynchronously,
rechecks current authority and market validity, and compiles any accepted action
through Go guards. A market snapshot can advance while inference is running;
the resulting action is compiled against the latest valid book. The worker has
no credentials or order-submission path. `paper` executes accepted proposals;
`shadow` records proposals but executes the pinned deterministic baseline.

## Exercise worker failure and fallback

With `--policy-fallback baseline`, stop terminal 1 while terminal 2 remains
running. Worker calls fail or time out, and Go uses the parent's already pinned
deterministic baseline, recording `fallback_baseline`. Restarting the worker
with the same bundle and socket lets later decisions request proposals again.
The daemon allows at most one pending policy request per actor; a stalled call
does not block controls, fills, market ingress, or lease renewal, and does not
cause unbounded inference goroutines.

To see pause behavior, create a second fresh account/schema with
`--policy-fallback pause` from its first start. That fallback is pinned for the
account's retained obligations, so it cannot be changed on a resume. Stop the
worker during an active plan: the runtime records `fallback_pause`, pauses the
parent, and cancels its child through the durable control path. Restart and
recover with `paper resume`, reconcile, then explicitly resume the plan when
ready. Use distinct new schema/account IDs for this separate scenario.

## Decision visibility in the trader desk

The trader desk now has an RL policy panel. Before a parent is created it shows
the configured bundle, profile, fallback, and promotion status. During and
after a plan, it shows the current saved decision's proposal, Go-applied action,
result, and reason. Session History shows the saved model decision and
provenance for the report revision selected. This is the latest decision in
that saved report, not a continuous decision stream; choose a policy-decision
or paper-fill event under **Technical activity** to inspect another saved
revision. In shadow mode, the proposal is informational and the deterministic
baseline executes. The first-order review remains a baseline preview, not an AI
prediction. The UI labels the model unvalidated and paper-only; none of these
panels establish savings, win rate, or production readiness.

For one recorded native example, connect to synthetic account `rl-beta-demo`,
open **Session history**, select the session `rl-demo-model-and-fallback-20261030`,
then expand **Technical activity** and select **execution** at revision 302
(load more events if necessary). The saved report shows `policy_action`, reason `accepted`, policy
action 6 both proposed and applied, and one simulated 0.01 BTC fill at 100.01
USD. That particular report was still in progress with 0.09 BTC remaining and
an attempted child whose finality was unknown. The byte-for-byte saved report,
retention comparison, model provenance, and later source-gap/recovery snapshots
are in [native evidence](../deployment/evidence/2026-10-30/native/README.md).

## Limits

The example tape is synthetic training input and the paper market is a separate
synthetic stream; this is not a holdout evaluation. A later native paper run
also encountered a source gap after roughly 1,797 synthetic receipts; readiness
became false and recovery was required. Its cause was not diagnosed. After Go
shutdown and operator resume with the same bundle, readiness returned, but the
original second plan's deadline had elapsed and it was expired. This observation
does not establish a feed availability target or healthy-feed soak. There is no
validated strategy-quality result, demonstrated savings, win-rate claim, live
policy promotion path, or live order transport. The worker is local, single-model,
single-flight, and unauthenticated on loopback TCP; prefer the Unix socket for
this local demo. Its 200 ms expiry is a stale-request limit, not a latency
promise. Evaluate on held-out, independently sourced data and measure the full
Go-to-Python path before making claims about quality or deployment.

See [inference worker contract](inference.md), [paper policy runtime](policy-runtime.md),
and [Go replay and training](go-replay.md) for the detailed request binding,
fallback, provenance, and resource limits.
