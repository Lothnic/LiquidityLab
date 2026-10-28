# Recover an existing paper account

The local daemon supports `paper create` and `paper resume`. Both use synthetic
balances and simulated orders. This recovery mechanism does not isolate an
external exchange connection or authorize private trading.

## Create and resume

Creation requires explicit initial balances and refuses an existing account:

```sh
build/liquiditylabd paper create \
  --schema ll_paper_account_a --account paper_account_a \
  --source synthetic --web-dir apps/web/dist \
  --initial-base 0 --initial-quote 100 --duration 30m
```

After a stop, use the same database URL, schema and account identity:

```sh
build/liquiditylabd paper resume \
  --schema ll_paper_account_a --account paper_account_a \
  --source synthetic --web-dir apps/web/dist --duration 30m
```

Both commands require `LIQUIDITYLAB_DATABASE_URL` and a configured
`LIQUIDITYLAB_API_TOKEN`. Keep the token out of URLs, command arguments and Git.
The existing `paper --flags` syntax remains an alias for creation. Resume rejects
either initial-balance flag, even an empty one, and never creates a missing
account. The default source is Kraken; select synthetic explicitly for this
example. Kraken resumes require a new capture output path for their new receipt
session.

Rules, fee assumptions, simulation settings, currencies, source, instrument
revision and any active policy bundle must match retained state. A policy-enabled
account requires its compatible worker and the same explicit policy options at
startup; an unavailable or incompatible worker currently fails startup before
takeover. Restoring that worker is required to resume that configuration. Changing
an account default bundle is permitted only after its previous obligations have
settled; the historical parent's bundle remains pinned.

## What recovery does

1. Validate retained configuration before changing ownership.
2. Atomically establish a new `postgres-paper-v1` epoch and obtain the complete
   current account, including its accepted-observation watermark. New simulator
   effects require that epoch at the database insertion boundary.
3. Reconstruct the post-claim snapshot. Pause recovered active parents, fence any
   pending policy request as `fallback_pause` / `owner_restart`, and invalidate
   market/account readiness. Preserve original start/deadline and fill identities.
4. Drain committed source facts. Obsolete unsent intents, cancel retained paper
   children through durable owned observations and settle their outcomes.
   An attempted order is never submitted again to discover what happened.
5. Require a fresh valid source snapshot. Derive simulated reconciliation from
   the drained paper venue, empty pending obligations and retained balances.
   The daemon supplies no replacement balances or invented external isolation
   proof.
6. Restore account readiness while the recovered parent stays paused. Starting
   or resuming execution requires a new explicit trader command. Retrying the
   original start command returns its old receipt and cannot restart the plan.

The process logs startup recovery/readiness. The API exposes account readiness,
recovery flags, parent status, fills and reservations. `/healthz` remains liveness
only. A quarantined/contradictory fact, unexplained hold, invalid clock, persistence
failure or missing valid book prevents readiness. Preserve the account for
inspection; do not replace its identity or clear reservations to bypass recovery.
An expired original deadline remains expired. Graceful shutdown still cancels a
nonterminal parent and reports whether finality is known.

## Scope of the fence

The paper venue's only persistent effect is a PostgreSQL observation. Migration
0004 guards simulated submit/cancel/trade inserts, including older binaries that
call the generic observation function. An old effect either commits before
takeover and belongs to its recovery watermark, or its delayed insert fails.
Current ownership is also required to return cached simulator outcomes.

Actual external financial facts are a different input: they remain admissible
through trusted ingress independently of the execution lease. The paper fence
must never be reused as proof that a remote exchange sender is isolated. Private
venue failover remains a separate unimplemented capability.

## Reproduce process checks

Use an isolated development PostgreSQL DSN:

```sh
LIQUIDITYLAB_TEST_POSTGRES_DSN='<development-dsn>' \
  go test -race -count=1 ./cmd/liquiditylabd \
  -run 'TestPaperProcess' -v
```

The harness creates randomized schemas, compiles actual daemon processes and
cleans up only its own schemas/processes. One check uses the ordinary binary and
SIGKILL/reap/resume during a partially completed parent. The boundary matrix
compiles a separate binary with `-tags liquiditylab_faults` and controls private
pipes around intent persistence, dispatch authorization, durable submit/cancel,
resting acknowledgement, fill/cursor reduction, an ordered paper trade and a
pending policy request. It also resumes a still-running old sender after takeover.
Ordinary builds have inert checkpoints and ignore the test environment variable.

These checks establish local paper recovery behavior. They are not throughput,
24-hour soak, database restore, model-quality or real venue execution evidence.
