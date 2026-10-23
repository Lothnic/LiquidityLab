# Local policy inference worker

The worker serves the versioned `liquiditylab.v1.PolicyService` contract for
offline replay and paper/shadow integration. It loads one immutable policy
bundle at startup and returns a proposed action. It has no exchange credentials
or order-submission capability. Go must still validate the echoed request
binding and current account state before using a proposal. The Go client is in
`internal/inference`; actor integration and fallback remain separate work. This worker alone does
not establish a safe live decision path.

## Install and run

Install the optional CPU inference and gRPC dependencies in addition to the
project:

```sh
uv sync --extra rl --extra go
uv run python -m liquiditylab.inference /path/to/policy-bundle \
  --listen unix:/run/user/1000/liquiditylab-inference.sock
```

The default listener is `unix:/tmp/liquiditylab-inference.sock`. Unix sockets
are created with owner-only permissions (`0600`); an existing path is never
replaced. TCP is accepted only on a numeric loopback IP, for example
`127.0.0.1:50051` or `[::1]:50051`. TCP loopback has no authentication and is
intended only for local processes. The process handles `SIGINT` and `SIGTERM`
with a bounded graceful shutdown and removes only the same socket inode it
created.

The worker requires a Go replay v1 bundle with `backend: "go"`, environment
`LiquidityLab-SpotExecution-v1`, an owned-local-process-binary provenance
record, a valid policy ZIP SHA-256, the exact observation schema and action
mapping, and `live_eligible: false`. It rejects symlinked bundle/policy files,
malformed or oversized manifests, incompatible versions, and policy digest
mismatches. Optional PyTorch and Stable-Baselines3 imports happen only when a
real model is loaded; the default loader selects CPU and limits Torch intra-op
and inter-op threads to one where Torch permits it.

## RPC contract

`Predict` accepts a `DecisionRequest` containing both a `DecisionBinding` and
an `Observation`. The worker requires nonempty request, tenant, account,
session, policy, observation-version, schema-hash, and market-snapshot
identifiers; positive session/account revisions; the pinned policy digest;
the current observation version and schema digest; and an unexpired request
within a maximum 200 ms validity window by default. The observation must carry
exactly 20 finite features in the declared field order. The request is rejected
if its serialized protobuf size exceeds 8 KiB by default. The deadline must be
explicit and active before model execution and remains active after inference.

The schema digest is SHA-256 over UTF-8 canonical JSON for
`{"version":"compact-v1","fields":[...]}`: keys sorted, separators
`(',', ':')`, Unicode unescaped, and field order preserved. The current digest
is:

```text
96e4088967693e4b7e7e5653c13e8128f0507a09d6255acf88eea3e05107441a
```

Use `liquiditylab.inference.observation_schema_sha256()` to obtain the digest
from the installed schema. `Predict` echoes the full binding unchanged. Model
actions are semantic values `0..5`; the worker maps them to Go wire enum values
`1..6`. It rejects malformed or out-of-range model output. `GetPolicyInfo`
requires the loaded policy SHA and returns the observation version, ordered
field names, raw bundle status, and `live_eligible: false`.

The worker admits one inference at a time. A concurrent call fails fast with
gRPC `RESOURCE_EXHAUSTED`; invalid, stale, oversized, or mismatched requests
fail with `INVALID_ARGUMENT`. A model error returns a generic `INTERNAL`
status. Callers must treat every non-success as no action and apply the Go
runtime's configured pause or deterministic fallback. This worker does not
choose that fallback. Cancellation cannot interrupt a Torch call already in
progress, so the post-call deadline check suppresses a late result; the caller
still needs its own deadline and cancellation handling.

## Readiness and limits

Bundles produced by training remain `unvalidated` and live-ineligible. The
worker reports the bundle's status for inspection, but never promotes it, and
rejects a manifest that sets `live_eligible` to true. No live promotion path is
implemented. Loading a model and passing these interface checks are not
evaluation, strategy validation, or production-readiness evidence.

This is a local single-process worker with one model and one in-flight request.
It does not implement batching, remote authentication, worker replication,
model hot reload, or Go-side stale-response/fallback handling. The 200 ms
expiry is a stale-request cutoff, not a latency guarantee. Capacity and
end-to-end latency must be measured with the Go client and target deployment
before any paper or live admission decision.

## Go policy client

`inference.New(ctx, Config{...})` verifies the worker's pinned policy digest,
observation version/fields and reported eligibility. Set `Profile` to `paper`
or `shadow` and explicitly set `AllowUnvalidated: true`; live construction is
rejected regardless of manifest status. Only absolute Unix paths or literal
loopback TCP endpoints are accepted. The client uses one fail-fast proposal
slot, 8 KiB request and 4 KiB response limits, a default 100 ms RPC budget, and
a maximum 200 ms request validity window. It does not retry or queue excess
inference work.

`Propose` verifies the complete echoed binding, action enum, mode, field order,
shape and finite features. `Proposal.ValidateCurrent` must then run under the
account authority before action compilation: plan/account identity/revisions,
policy/schema, active status, account readiness, market validity and expiry must
still match. A newer market snapshot is permitted; the action must be compiled
against that latest valid book. These methods provide the boundary checks;
they do not themselves enqueue account commands, dispatch orders or choose a
fallback.

The optional real boundary check trains an eight-step PPO bundle, starts the
Python worker, sends real Go replay features through the Go client and compiles
the returned action in Go. Both absolute executable environment variables are
required; otherwise this check skips explicitly:

```sh
LIQUIDITYLAB_PYTHON="$PWD/.venv/bin/python" \
LIQUIDITYLAB_REPLAY_BINARY="$PWD/build/liquiditylabd" \
  go test -race -count=1 ./internal/inference -run TestPythonPPOWorkerBoundary -v
```

The subprocesses use bounded startup/execution/cleanup, private Unix socket
directories, and process-group cleanup. Passing this integration does not
provide holdout evaluation or live eligibility.
